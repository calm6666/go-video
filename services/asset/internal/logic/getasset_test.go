package logic

// getasset_test.go 覆盖读侧单条查询 GetAsset：入参守卫、详情缓存读穿/回填、
// 三类下游故障的不同口径（原样上抛 / 包一层上抛 / 就地吞掉）、状态枚举透传，
// 以及应答面（只透出 bucket+object_key 引用，不透出任何 URL/凭据）。
//
// 被测路径是 logic → **真实 Repository** → 内存缓存/内存 model，所以「命中就不查库」
// 「不缓存空结果」「回填在读库之后」这些结论是从调用序列里读出来的，不是替身自述。

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

// fullAsset 是一行字段全非零、且彼此不同的假媒资：投影漏一个字段就会在
// assetLine/replyAssetLine 的比对里暴露出来。
func fullAsset(id int64) *model.AssetMeta {
	return &model.AssetMeta{
		AssetID:   id,
		UploadID:  fakeUploadID,
		Mid:       fakeMid,
		Bucket:    fakeBucket,
		ObjectKey: fakeOriginKey,
		Size:      1099511627776,
		Md5:       fakeMd5,
		Duration:  42000,
		Width:     1920,
		Height:    1080,
		Codec:     "h264",
		State:     model.StateTranscoded,
		Ctime:     fakeCtime,
		Mtime:     fakeCtime + 60,
	}
}

func TestGetAssetRejectsNonPositiveAssetID(t *testing.T) {
	for _, id := range []int64{0, -1, -9007199254740993} {
		t.Run(fmt.Sprintf("asset_id=%d", id), func(t *testing.T) {
			st := newStore()
			// 库里有同一条能被读到的行，确保「失败」来自守卫而不是查不到。
			seedAsset(t, st, fullAsset(1))
			l := NewGetAssetLogic(context.Background(), newTestSvc(st))

			got, err := l.GetAsset(&rpc.AssetReq{AssetId: id})

			wantErrIdentity(t, "GetAsset", err, model.ErrInvalidAssetID)
			if got != nil {
				t.Errorf("GetAsset(%d) 应答 = %v, want nil", id, got)
			}
			wantNoCall(t, "GetAsset 守卫", st, 0)
		})
	}

	// 正向对照：同一个库位、asset_id 从 0 变 1，守卫必须放行（否则上面的失败没有判别力）。
	st := newStore()
	row := seedAsset(t, st, fullAsset(1))
	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 1})
	wantNoErr(t, "GetAsset(1)", err)
	wantEQ(t, "GetAsset(1)", "应答", replyAssetLine(got), assetLine(row))
}

// TestGetAssetCacheHitServesStaleProbeResult 钉住探测结果的读侧口径：
// 缓存命中即返回，不查库、不比对版本，所以 transcode 回写后的最长 pinnedCacheTTL 秒内
// 调用方看到的仍是旧状态/旧时长。失效只发生在 UpdateAssetMeta/TransitionState 的 DelAsset。
func TestGetAssetCacheHitServesStaleProbeResult(t *testing.T) {
	st := newStore()
	stale := fullAsset(123)
	stale.State = model.StateUploaded
	stale.Duration, stale.Width, stale.Height, stale.Codec = 0, 0, 0, ""
	st.cache.warmAsset(stale)

	fresh := fullAsset(123) // 库里已是转码完成态
	seedAsset(t, st, fresh)

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	wantNoErr(t, "GetAsset", err)

	wantOps(t, "GetAsset 缓存命中", st.log.opsFrom(0), []string{"cache.Get:" + keyAsset(123)})
	wantEQ(t, "命中缓存", "state", int32(got.GetState()), int32(rpc.AssetState_STATE_UPLOADED))
	wantEQ(t, "命中缓存", "duration", got.GetDuration(), int64(0))
	wantEQ(t, "命中缓存", "codec", got.GetCodec(), "")
	// 库里的新值没被读到：三处对照，任意一处读到都说明命中判定不成立。
	wantEQ(t, "命中缓存不查库", "库存 state", st.meta.row(123).State, int32(model.StateTranscoded))
	wantEQ(t, "命中缓存不查库", "FindOne 次数", st.log.countPrefix("meta.FindOne"), 0)
}

// TestGetAssetCacheHitDoesNotCheckPayloadAssetID 钉现状：缓存载荷的主键与请求不一致时，
// 读侧照原样返回，不做一致性校验（asset_id 引用一致性的唯一防线只剩「写侧 key 是自己拼的」）。
func TestGetAssetCacheHitDoesNotCheckPayloadAssetID(t *testing.T) {
	st := newStore()
	other := fullAsset(999)
	other.Mid = fakeMidOther
	st.cache.warm(123, mustMarshalAsset(other)) // key 是 123，载荷里的主键是 999

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	wantNoErr(t, "GetAsset", err)

	wantEQ(t, "缓存载荷主键", "asset_id", got.GetAssetId(), int64(999))
	wantEQ(t, "缓存载荷主键", "mid", got.GetMid(), int64(fakeMidOther))
	wantOps(t, "GetAsset", st.log.opsFrom(0), []string{"cache.Get:" + keyAsset(123)})
}

// TestGetAssetCorruptCachePayloadFailsClosed 钉两条：脏 JSON 直接失败（不回落查库），
// 且读路径不会顺手清掉脏 key —— 于是同一 asset_id 在 TTL 内持续失败。
func TestGetAssetCorruptCachePayloadFailsClosed(t *testing.T) {
	st := newStore()
	st.cache.warm(123, "{不是 JSON")
	seedAsset(t, st, fullAsset(123)) // 库里明明有可读的行

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrContains(t, "GetAsset 脏缓存", err, "GetAsset unmarshal cache")
	wantOps(t, "不回落查库、不清 key", st.log.opsFrom(0), []string{"cache.Get:" + keyAsset(123)})
	if payload, ok := st.cache.stored(123); !ok || payload != "{不是 JSON" {
		t.Errorf("脏缓存键未被保留（stored=%q,%v）：读路径若开始自愈，本用例与 README 缺口都要改", payload, ok)
	}
}

// TestGetAssetEmptyPayloadIsTreatedAsMiss 钉 hit 判定：值为空串算 miss，继续读库并回填。
func TestGetAssetEmptyPayloadIsTreatedAsMiss(t *testing.T) {
	st := newStore()
	row := seedAsset(t, st, fullAsset(123))
	st.cache.warm(123, "") // Setex 写空串（jsonMustMarshal 失败时的产物）留下的状态

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	wantNoErr(t, "GetAsset", err)
	wantEQ(t, "空串按 miss", "应答", replyAssetLine(got), assetLine(row))
	wantOps(t, "空串按 miss", st.log.opsFrom(0), []string{
		"cache.Get:" + keyAsset(123),
		"meta.FindOne:123",
		"cache.Set:" + keyAsset(123),
	})
}

// TestGetAssetCacheMissReadsThroughAndBackfillsFullRow 钉读穿回填的完整口径：
// 顺序＝读缓存→查库→回填，回填的是整行（字段一个都不能少），第二次读只碰缓存。
func TestGetAssetCacheMissReadsThroughAndBackfillsFullRow(t *testing.T) {
	st := newStore()
	row := seedAsset(t, st, fullAsset(123))

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	wantNoErr(t, "GetAsset", err)
	wantEQ(t, "读穿应答", "应答", replyAssetLine(got), assetLine(row))
	wantEQ(t, "读穿应答", "ctime", got.GetCtime(), row.Ctime)
	wantEQ(t, "读穿应答", "mtime", got.GetMtime(), row.Mtime)
	wantOps(t, "读穿顺序", st.log.opsFrom(0), []string{
		"cache.Get:" + keyAsset(123),
		"meta.FindOne:123",
		"cache.Set:" + keyAsset(123),
	})

	// 回填内容单独解码一次：证明存的是整行而不是裁剪过的 DTO。
	backfilled := st.cache.storedAsset(123)
	if backfilled == nil {
		payload, _ := st.cache.stored(123)
		t.Fatalf("回填后解码失败：缓存里存的是 %q", payload)
	}
	wantEQ(t, "回填内容", "整行", assetLine(backfilled), assetLine(row))

	before := st.log.snapshot()
	if _, err := l.GetAsset(&rpc.AssetReq{AssetId: 123}); err != nil {
		t.Fatalf("第二次 GetAsset：%v", err)
	}
	wantOps(t, "第二次只碰缓存", st.log.opsFrom(before), []string{"cache.Get:" + keyAsset(123)})
	wantCount(t, "第二次不查库", st.log, "meta.FindOne", 1)
}

// TestGetAssetMissingRowReturnsRawSentinel 钉两条：未找到返回**未包装**的
// model.ErrAssetNotFound（logic 用的是 == 比较），且不写空值占位 ⇒ 不存在的 ID 每次都打库。
func TestGetAssetMissingRowReturnsRawSentinel(t *testing.T) {
	st := newStore()
	seedAsset(t, st, fullAsset(124)) // 库里只有邻居，排除「查错行」

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIdentity(t, "GetAsset 未找到", err, model.ErrAssetNotFound)
	wantOps(t, "不缓存空结果", st.log.opsFrom(0), []string{
		"cache.Get:" + keyAsset(123),
		"meta.FindOne:123",
	})
	if _, ok := st.cache.stored(123); ok {
		t.Errorf("未找到却写了缓存：负缓存一旦出现，本用例与 README 的「无空值保护」结论都要改")
	}
}

// TestGetAssetCacheReadErrorPropagatesRaw 钉故障类别①：缓存读失败**原样上抛、不包装**，
// 并且不降级查库——Redis 不可用时详情读整体失败。
func TestGetAssetCacheReadErrorPropagatesRaw(t *testing.T) {
	st := newStore()
	seedAsset(t, st, fullAsset(123))
	st.cache.failWith("GetAsset", errBoom)

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIdentity(t, "缓存读失败上抛", err, errBoom) // Repository 不包 %w
	wantOps(t, "缓存故障不降级", st.log.opsFrom(0), []string{"cache.Get:" + keyAsset(123)})
	wantCount(t, "缓存故障不查库", st.log, "meta.FindOne", 0)
}

// TestGetAssetDBErrorPropagatesWithoutBackfill 钉故障类别②：model 层用 %w 包一层再上抛，
// 没被换成不透明哨兵；失败时也不回填缓存。
func TestGetAssetDBErrorPropagatesWithoutBackfill(t *testing.T) {
	st := newStore()
	st.meta.failWith("FindOne", errBoom)

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "查库失败上抛", err, errBoom)
	// 注意：不断言包装文本——那层 %w 是替身照抄 model/assetmodel.go:77 写的，
	// 断言它只能证明替身自己，证明不了生产 SQL。
	wantOps(t, "查库失败顺序", st.log.opsFrom(0), []string{
		"cache.Get:" + keyAsset(123),
		"meta.FindOne:123",
	})
	wantCount(t, "失败不回填", st.log, "cache.Set", 0)
}

// TestGetAssetBackfillFailureIsSwallowed 钉故障类别③：读穿后的回填失败被就地吞掉
// （`_ = r.cache.SetAsset(...)`，连日志都没有），请求仍然成功，代价是下次继续打库。
func TestGetAssetBackfillFailureIsSwallowed(t *testing.T) {
	st := newStore()
	row := seedAsset(t, st, fullAsset(123))
	st.cache.failWith("SetAsset", errBoom)

	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	wantNoErr(t, "回填失败不该让读请求失败", err)
	wantEQ(t, "回填失败仍返回整行", "应答", replyAssetLine(got), assetLine(row))
	wantOps(t, "回填尝试过", st.log.opsFrom(0), []string{
		"cache.Get:" + keyAsset(123),
		"meta.FindOne:123",
		"cache.Set:" + keyAsset(123),
	})
	if _, ok := st.cache.stored(123); ok {
		t.Errorf("SetAsset 已注入失败却仍写入缓存：替身漏实现，本用例失去判别力")
	}
}

// TestGetAssetNotFoundTextIsTheWireContract 钉跨服务耦合：GetAsset 用 error 而不是
// found=false 字段表达「媒资不存在」，过 gRPC 后只剩 Unknown + 文案，
// services/catalog/internal/repository/asset_client.go:19 就是按
// `asset: not found` 这个字符串识别的。文案一改，catalog 会把「不存在」误判成「下游不可用」。
func TestGetAssetNotFoundTextIsTheWireContract(t *testing.T) {
	st := newStore()
	l := NewGetAssetLogic(context.Background(), newTestSvc(st))

	_, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	wantErrIdentity(t, "GetAsset 未找到", err, model.ErrAssetNotFound)
	wantEQ(t, "跨服务识别文案", "error.Error()", err.Error(), "asset: not found")
}

// TestGetAssetStateIsRawNumericCast 钉 helpers.go:26 的强转口径：
// model.state 与 rpc.AssetState 数值一一对应，读侧不做值域校验，脏值原样透出。
func TestGetAssetStateIsRawNumericCast(t *testing.T) {
	pairs := []struct {
		name  string
		state int32
		want  rpc.AssetState
	}{
		{"UNSPECIFIED", model.StateUnspecified, rpc.AssetState_STATE_UNSPECIFIED},
		{"UPLOADED", model.StateUploaded, rpc.AssetState_STATE_UPLOADED},
		{"SCANNED", model.StateScanned, rpc.AssetState_STATE_SCANNED},
		{"TRANSCODED", model.StateTranscoded, rpc.AssetState_STATE_TRANSCODED},
		{"FAILED", model.StateFailed, rpc.AssetState_STATE_FAILED},
		// 库里出现枚举外的脏值（TINYINT 没有 CHECK 约束）时不报错、不改写。
		{"脏值 7", 7, rpc.AssetState(7)},
		{"脏值 -1", -1, rpc.AssetState(-1)},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			st := newStore()
			target := fullAsset(456)
			target.State = p.state
			seedAsset(t, st, target)
			// 同库再放一条已知状态的行做对照，排除「不管查哪条都返回同一个状态」。
			contrast := fullAsset(123)
			contrast.State = model.StateScanned
			contrast.Codec = "vp9"
			seedAsset(t, st, contrast)

			l := NewGetAssetLogic(context.Background(), newTestSvc(st))
			got, err := l.GetAsset(&rpc.AssetReq{AssetId: 456})
			wantNoErr(t, "GetAsset", err)
			wantEQ(t, "状态透传", "state", int32(got.GetState()), p.state)
			wantEQ(t, "状态枚举", "state", got.GetState(), p.want)

			other, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
			wantNoErr(t, "GetAsset 对照行", err)
			wantEQ(t, "对照行状态", "state", int32(other.GetState()), int32(model.StateScanned))
			wantEQ(t, "对照行编码", "codec", other.GetCodec(), "vp9")
		})
	}
}

// TestAssetRepliesExposeOSSReferenceButNoSignedURL 钉应答面：本服务只透出
// bucket + object_key 这一对**引用**，4 种应答里没有任何 URL/签名/过期/凭据字段，
// 也没有把桶拼成可访问地址的路径（预签名归 playback，见 AGENTS.md §6）。
func TestAssetRepliesExposeOSSReferenceButNoSignedURL(t *testing.T) {
	banned := []string{"url", "uri", "sign", "secret", "token", "expire", "accesskey", "cdn", "endpoint", "host"}

	messages := map[string]any{
		"AssetReply":      &rpc.AssetReply{},
		"CoverReply":      &rpc.CoverReply{},
		"SubtitleReply":   &rpc.SubtitleReply{},
		"ScreenshotReply": &rpc.ScreenshotReply{},
		"AssetsReply":     &rpc.AssetsReply{},
		"CoversReply":     &rpc.CoversReply{},
		"SubtitlesReply":  &rpc.SubtitlesReply{},
	}
	for name, msg := range messages {
		typ := reflect.TypeOf(msg).Elem()
		var fields []string
		for i := 0; i < typ.NumField(); i++ {
			sf := typ.Field(i)
			if !sf.IsExported() {
				continue // protoimpl 内部字段（state/sizeCache/unknownFields）不参与判定
			}
			fields = append(fields, sf.Name)
		}
		joined := strings.ToLower(strings.Join(fields, " "))
		for _, bad := range banned {
			if strings.Contains(joined, bad) {
				t.Errorf("%s 出现了含 %q 的字段 %v：媒资应答只能透出 bucket/object_key 引用", name, bad, fields)
			}
		}
	}

	// 引用本身确实透出（网关/playback 靠它定位对象），但拼不出任何带签名的地址。
	st := newStore()
	seedAsset(t, st, fullAsset(123))
	l := NewGetAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.GetAsset(&rpc.AssetReq{AssetId: 123})
	wantNoErr(t, "GetAsset", err)
	wantEQ(t, "引用透出", "bucket", got.GetBucket(), fakeBucket)
	wantEQ(t, "引用透出", "object_key", got.GetObjectKey(), fakeOriginKey)
	text := fmt.Sprintf("%+v", got)
	for _, bad := range []string{"://", "Signature", "x-oss", "OSSAccessKeyId", "Expires="} {
		if strings.Contains(text, bad) {
			t.Errorf("GetAsset 应答文本含 %q，疑似把可访问地址或凭据带了出来：%s", bad, text)
		}
	}
}
