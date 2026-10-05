package logic

// updateassetmeta_test.go 覆盖写侧探测回写 UpdateAssetMeta：主键守卫、
// 「只改 duration/width/height/codec + mtime」的列面、失效缓存的时机（**不读**缓存，
// 直接 UPDATE→DEL→回读三步），以及三条现状风险：
// 空请求把已探测结果抹成零值、负值与终态一律放行、写失败/失效失败都会留下陈旧缓存。
//
// 与 TransitionState 的区别要留神：这里没有状态机判定，`state` 只是被顺带读回来的字段。

import (
	"context"
	"strings"
	"testing"
	"time"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

// probed 布一行「已探测完」的媒资：四个探测列都是可辨识的非零值。
func probed(t *testing.T, st *store, assetID int64, state int32) *model.AssetMeta {
	t.Helper()
	m := fullAsset(assetID)
	m.State = state
	m.Duration, m.Width, m.Height, m.Codec = 42000, 1920, 1080, "h264"
	m.Ctime, m.Mtime = fakeCtime, fakeCtime+60
	return seedAsset(t, st, m)
}

func updateOps(assetID int64) []string {
	return []string{opMetaUpdateMeta(assetID), opCacheDel(assetID), opMetaFindOne(assetID)}
}

func TestUpdateAssetMetaRejectsNonPositiveAssetID(t *testing.T) {
	for _, id := range []int64{0, -1, -910001} {
		t.Run("asset_id="+itoa(id), func(t *testing.T) {
			st := newStore()
			probed(t, st, fakeAssetID, model.StateTranscoded)
			l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

			got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{AssetId: id, Duration: 1, Codec: "av1"})

			wantErrIdentity(t, "UpdateAssetMeta", err, model.ErrInvalidAssetID)
			if got != nil {
				t.Errorf("UpdateAssetMeta(%d) 应答 = %v, want nil", id, got)
			}
			wantNoCall(t, "UpdateAssetMeta 守卫", st, 0)
		})
	}

	// 正向对照 + 一条独有结论：这条路径**不读**详情缓存，第一步就是 UPDATE。
	st := newStore()
	probed(t, st, fakeAssetID, model.StateTranscoded)
	l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))
	got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{AssetId: fakeAssetID, Duration: 86400, Width: 1280, Height: 720, Codec: "av1"})
	wantNoErr(t, "UpdateAssetMeta 合规入参", err)
	wantEQ(t, "合规入参", "应答 duration", got.GetDuration(), int64(86400))
	wantOps(t, "回写不读缓存", st.log.opsFrom(0), updateOps(fakeAssetID))
	wantCount(t, "回写不读缓存", st.log, "cache.Get", 0)
}

// TestUpdateAssetMetaWritesProbeColumnsAndInvalidatesCache 钉可改字段集合与副作用顺序：
// 只有 duration/width/height/codec 被改写（外加 model 层刷新的 mtime），
// 归属与引用列（upload_id/mid/bucket/object_key/size/md5）、状态、ctime 一律不动；
// 缓存是**失效**而不是回填，应答来自失效后的那次回读。
func TestUpdateAssetMetaWritesProbeColumnsAndInvalidatesCache(t *testing.T) {
	st := newStore()
	before := probed(t, st, fakeAssetID, model.StateTranscoded)
	l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

	at := time.Now().Unix()
	got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{
		AssetId: fakeAssetID, Duration: 86400, Width: 1280, Height: 720, Codec: "av1",
	})
	after := time.Now().Unix()

	wantNoErr(t, "UpdateAssetMeta", err)
	row := st.meta.row(fakeAssetID)
	wantEQ(t, "探测列已改写", "duration", row.Duration, int64(86400))
	wantEQ(t, "探测列已改写", "width", row.Width, int32(1280))
	wantEQ(t, "探测列已改写", "height", row.Height, int32(720))
	wantEQ(t, "探测列已改写", "codec", row.Codec, "av1")
	// 其余列原样（逐列对照布景值，而不是笼统比整行）。
	wantEQ(t, "不改归属", "upload_id", row.UploadID, before.UploadID)
	wantEQ(t, "不改归属", "mid", row.Mid, before.Mid)
	wantEQ(t, "不改引用", "bucket", row.Bucket, before.Bucket)
	wantEQ(t, "不改引用", "object_key", row.ObjectKey, before.ObjectKey)
	wantEQ(t, "不改引用", "size", row.Size, before.Size)
	wantEQ(t, "不改引用", "md5", row.Md5, before.Md5)
	wantEQ(t, "回写不推进状态机", "state", row.State, int32(model.StateTranscoded))
	wantEQ(t, "回写不改创建时间", "ctime", row.Ctime, before.Ctime)
	// mtime 由 model 层的 nowUnix() 刷新（不是入参也不是布景值）。
	wantUnixWindow(t, "mtime 刷新", "mtime", row.Mtime, at, after)
	// 应答就是回读出来的那一行，不是入参拼的。
	wantEQ(t, "应答 == 库存行", "整行", replyAssetLine(got), assetLine(row))
	wantEQ(t, "应答", "state", got.GetState(), rpc.AssetState_STATE_TRANSCODED)
	wantOps(t, "三步副作用", st.log.opsFrom(0), updateOps(fakeAssetID))
	if _, ok := st.cache.stored(fakeAssetID); ok {
		t.Errorf("回写后详情缓存未失效：%v", st.cache.storedAsset(fakeAssetID))
	}
}

// TestUpdateAssetMetaEmptyRequestWipesExistingProbeResults 钉「空更新」的真实语义：
// proto3 分不出「没传」与「传了零值」，logic 也没有任何逐字段判定，
// 所以一次只带 asset_id 的调用会把已写入的时长/宽高/编码**全部抹成零值**，
// 而 state 仍是 TRANSCODED——一台「转码完成但时长为 0」的媒资就这样进了库。
func TestUpdateAssetMetaEmptyRequestWipesExistingProbeResults(t *testing.T) {
	st := newStore()
	before := probed(t, st, fakeAssetID, model.StateTranscoded)
	l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

	got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "空请求被接受（现状）", err)

	row := st.meta.row(fakeAssetID)
	wantEQ(t, "被抹零", "duration", row.Duration, int64(0))
	wantEQ(t, "被抹零", "width", row.Width, int32(0))
	wantEQ(t, "被抹零", "height", row.Height, int32(0))
	wantEQ(t, "被抹零", "codec", row.Codec, "")
	wantEQ(t, "状态没跟着回退", "state", row.State, int32(model.StateTranscoded))
	wantEQ(t, "引用列没被抹", "object_key", row.ObjectKey, before.ObjectKey)
	if row.Mtime == before.Mtime {
		t.Errorf("mtime 仍 = %d：抹零这次写没有留下时间痕迹", row.Mtime)
	}
	wantEQ(t, "应答如实反映抹零", "整行", replyAssetLine(got), assetLine(row))
	wantOps(t, "抹零走的还是三步", st.log.opsFrom(0), updateOps(fakeAssetID))
}

// TestUpdateAssetMetaAcceptsNegativeValuesOnTerminalAsset 钉两处不校验：
//  1. 值域：duration/width 为负、codec 为纯空格都不被拒（守卫只看 asset_id）。
//  2. 状态前置：FAILED 终态照样能被回写，本方法完全不查状态机。
//
// 合起来的后果：一条已判失败的媒资可以带着负时长重新出现在读侧。
func TestUpdateAssetMetaAcceptsNegativeValuesOnTerminalAsset(t *testing.T) {
	st := newStore()
	probed(t, st, fakeAssetID, model.StateFailed)
	l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

	got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{
		AssetId: fakeAssetID, Duration: -42000, Width: -1920, Height: -1080, Codec: "  ",
	})
	wantNoErr(t, "负值 + 终态回写（现状）", err)

	row := st.meta.row(fakeAssetID)
	wantEQ(t, "负值原样入库", "duration", row.Duration, int64(-42000))
	wantEQ(t, "负值原样入库", "width", row.Width, int32(-1920))
	wantEQ(t, "负值原样入库", "height", row.Height, int32(-1080))
	wantEQ(t, "空白 codec 原样入库", "codec", row.Codec, "  ")
	wantEQ(t, "终态未被回退也未被拒绝", "state", row.State, int32(model.StateFailed))
	wantEQ(t, "应答", "state", got.GetState(), rpc.AssetState_STATE_FAILED)
	wantEQ(t, "应答", "duration", got.GetDuration(), int64(-42000))
	// 不查状态机 ⇒ 不会为判定多读一次库，也不会写缓存。
	wantCount(t, "回写不读缓存也不查库判定", st.log, "cache.Get", 0)
	wantCount(t, "回写的读只有一次（写后回读）", st.log, "meta.FindOne", 1)
}

// TestUpdateAssetMetaMissingRowReturnsRawSentinel 钉「不存在」的两条路径与残留形态：
// 库里没这行 ⇒ model 按 RowsAffected 判未命中，返回**未包装**的 ErrAssetNotFound
// （logic 用 == 短路），UPDATE 之后一步都不走：既不回读，也**不失效**缓存。
// 于是「幽灵缓存」（key 在、行没了）永远无法被这次写清掉。
func TestUpdateAssetMetaMissingRowReturnsRawSentinel(t *testing.T) {
	t.Run("冷缓存：UPDATE 即止", func(t *testing.T) {
		st := newStore()
		neighbor := probed(t, st, fakeAssetID+1, model.StateTranscoded) // 只有邻居
		l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

		got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{AssetId: fakeAssetID, Duration: 1000, Codec: "vp9"})
		if got != nil {
			t.Errorf("应答 = %v, want nil", got)
		}
		wantErrIdentity(t, "回写不存在的媒资", err, model.ErrAssetNotFound)
		wantOps(t, "失败即止", st.log.opsFrom(0), []string{opMetaUpdateMeta(fakeAssetID)})
		wantCount(t, "失败不回读", st.log, "meta.FindOne", 0)
		wantCount(t, "失败不失效", st.log, "cache.", 0)
		// 失败没有牵连邻居行：它仍是布景时那一版（含探测列与 mtime）。
		wantEQ(t, "邻居行未被牵连", "整行", assetLine(st.meta.row(fakeAssetID+1)), assetLine(neighbor))
		wantEQ(t, "邻居行未被牵连", "mtime", st.meta.row(fakeAssetID+1).Mtime, neighbor.Mtime)
	})

	t.Run("幽灵缓存：写失败后缓存照旧能读到", func(t *testing.T) {
		st := newStore()
		phantom := &model.AssetMeta{
			AssetID: fakeAssetID, UploadID: fakeUploadID, Mid: fakeMid,
			Bucket: fakeBucket, ObjectKey: fakeOriginKey, State: model.StateUploaded,
			Ctime: fakeCtime, Mtime: fakeCtime,
		}
		st.cache.warmAsset(phantom) // key 在、库里没这行
		l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

		_, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{AssetId: fakeAssetID, Duration: 1000})
		wantErrIdentity(t, "幽灵行回写", err, model.ErrAssetNotFound)
		wantOps(t, "失败即止（幽灵留着）", st.log.opsFrom(0), []string{opMetaUpdateMeta(fakeAssetID)})
		if st.cache.storedAsset(fakeAssetID) == nil {
			t.Fatalf("幽灵缓存被清了：本用例与 README 缺口的现状结论要一起改")
		}
		detail, err := NewGetAssetLogic(context.Background(), newTestSvc(st)).GetAsset(&rpc.AssetReq{AssetId: fakeAssetID})
		wantNoErr(t, "幽灵缓存可读", err)
		wantEQ(t, "幽灵缓存", "整行", replyAssetLine(detail), assetLine(phantom))
	})
}

// TestUpdateAssetMetaDBFailureKeepsStaleCacheServing 钉「失败后库里/缓存留下什么形态」：
// UPDATE 失败 ⇒ 库里四个探测列没动、缓存没失效，于是 transcode 这次回写彻底丢失，
// 而读侧仍最长 60 秒拿到**旧**探测结果（这条路径不会因为报错而自动回源）。
func TestUpdateAssetMetaDBFailureKeepsStaleCacheServing(t *testing.T) {
	st := newStore()
	fresh := probed(t, st, fakeAssetID, model.StateTranscoded) // 库里：42000/h264
	stale := *fresh
	stale.Duration, stale.Codec, stale.State = 0, "", model.StateUploaded // 缓存里：还没探测完
	st.cache.warmAsset(&stale)
	st.meta.failWith("UpdateMeta", errBoom)
	l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

	got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{AssetId: fakeAssetID, Duration: 86400, Codec: "av1"})
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "回写失败上抛", err, errBoom)
	wantOps(t, "失败即止", st.log.opsFrom(0), []string{opMetaUpdateMeta(fakeAssetID)})
	wantCount(t, "失败不失效", st.log, "cache.Del", 0)
	// 库里新值一行都没写进去。
	wantEQ(t, "库存未变", "整行", assetLine(st.meta.row(fakeAssetID)), assetLine(fresh))
	// 读侧看到的仍是陈旧那一份。
	detail, err := NewGetAssetLogic(context.Background(), newTestSvc(st)).GetAsset(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "陈旧缓存可读", err)
	wantEQ(t, "陈旧缓存", "duration", detail.GetDuration(), int64(0))
	wantEQ(t, "陈旧缓存", "state", detail.GetState(), rpc.AssetState_STATE_UPLOADED)
}

// TestUpdateAssetMetaDelFailureIsSwallowed 钉写侧另一个被吞掉的错误：
// repository.go:182 的 `_ = r.cache.DelAsset(...)`。
// 结果是「库里已更新、应答是新值、缓存还是旧值」这种三方不一致，且没有任何日志。
func TestUpdateAssetMetaDelFailureIsSwallowed(t *testing.T) {
	st := newStore()
	fresh := probed(t, st, fakeAssetID, model.StateTranscoded)
	stale := *fresh
	stale.Duration, stale.Width, stale.Height, stale.Codec = 0, 0, 0, ""
	st.cache.warmAsset(&stale)
	st.cache.failWith("DelAsset", errBoom)
	l := NewUpdateAssetMetaLogic(context.Background(), newTestSvc(st))

	got, err := l.UpdateAssetMeta(&rpc.UpdateAssetReq{AssetId: fakeAssetID, Duration: 86400, Width: 1280, Height: 720, Codec: "av1"})
	wantNoErr(t, "失效失败不该让回写失败", err)
	wantOps(t, "DEL 确实尝试过", st.log.opsFrom(0), updateOps(fakeAssetID))
	wantEQ(t, "库里已更新", "duration", st.meta.row(fakeAssetID).Duration, int64(86400))
	wantEQ(t, "应答是新值", "整行", replyAssetLine(got), assetLine(st.meta.row(fakeAssetID)))
	// 但缓存还是旧值：调用方成功，下一位读者拿旧的。
	cached := st.cache.storedAsset(fakeAssetID)
	if cached == nil {
		t.Fatalf("DelAsset 注入失败却删掉了缓存：替身漏实现，本用例失去判别力")
	}
	wantEQ(t, "陈旧缓存", "duration", cached.Duration, int64(0))
	wantEQ(t, "陈旧缓存", "codec", cached.Codec, "")
}

// TestUpdateAssetMetaSQLWritesEveryProbeColumnUnconditionally 是源码对账，不是行为验证：
//   - UpdateMeta 的 SET 列面固定是 duration/width/height/codec/mtime 五列，没有 IFNULL、
//     没有按列条件 —— 这就是「空请求抹零」的机制来源。
//   - 两处「未找到」判定都是 `aff == 0`（UPDATE 影响 0 行）。真实驱动默认上报**改变**的行数，
//     所以同一秒内重放同一份探测结果（mtime 也相同）会被判成 ErrAssetNotFound；
//     本包替身按**匹配**行数建模，这条差异在内存用例里看不见（见 README 缺口 17）。
func TestUpdateAssetMetaSQLWritesEveryProbeColumnUnconditionally(t *testing.T) {
	src := readSource(t, modelSourcePath)

	const wantSQL = "UPDATE asset_meta SET duration = ?, width = ?, height = ?, codec = ?, mtime = ? WHERE asset_id = ?"
	if !strings.Contains(src, wantSQL) {
		t.Errorf("model/assetmodel.go 里找不到 UpdateMeta 的 `%s`：可改列面变了，先对齐上面几条用例与 README 缺口 14", wantSQL)
	}
	if n := strings.Count(src, "aff == 0"); n != 2 {
		t.Errorf("model 里 `aff == 0` 判定出现 %d 次, want 2（UpdateMeta/UpdateState 各一次）：未找到判定口径变了，缺口 17 要重新评估", n)
	}
	if strings.Contains(src, "IFNULL") || strings.Contains(src, "COALESCE") {
		t.Errorf("UpdateMeta 开始用 IFNULL/COALESCE 做增量更新：空请求抹零的现状结论（缺口 14）已收严，用例要改")
	}
}
