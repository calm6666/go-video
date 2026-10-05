package logic

// registerasset_test.go 覆盖写侧登记 RegisterAsset：四条入参守卫的**先后与边界**、
// 登记行的初始形态（state=UPLOADED、探测字段留空、ctime==mtime）、登记后**预热**详情缓存
// 这个容易被忽略的副作用、没有幂等键导致的重复行，以及 INSERT 失败时的残留形态。
//
// 被测路径仍是 logic → **真实 Repository** → 内存 meta/内存缓存，
// 所以「先写库还是先写缓存」「缓存写失败有没有被吞」「重复登记有没有先查再插」
// 都是从有序调用轨迹里读出来的，不是替身自述。

import (
	"context"
	"testing"
	"time"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

// regSize 是一个假的 1 TiB 文件大小：只要求非零、能和其它字段区分。
const regSize int64 = 1099511627776

func validRegisterReq() *rpc.RegisterAssetReq {
	return &rpc.RegisterAssetReq{
		UploadId:  fakeUploadID,
		Mid:       fakeMid,
		Bucket:    fakeBucket,
		ObjectKey: fakeOriginKey,
		Size:      regSize,
		Md5:       fakeMd5,
	}
}

// insertOpOf / setOpOf 拼本次调用应当落在轨迹里的两条依赖调用。
func insertOpOf(uploadID, mid int64) string {
	return "meta.Insert:upload=" + itoa(uploadID) + "/mid=" + itoa(mid)
}
func setOpOf(assetID int64) string { return "cache.Set:" + keyAsset(assetID) }

// TestRegisterAssetGuardsRunInSourceOrderAndTouchNothing 钉守卫的**顺序**，不只是「会被拒」：
// 四条同时坏时必须报 upload_id，然后按 registerassetlogic.go:32-43 的顺序逐条往后移。
// 每个被拒的请求都必须一次依赖都不碰（守卫在 INSERT 与缓存之前）。
func TestRegisterAssetGuardsRunInSourceOrderAndTouchNothing(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(in *rpc.RegisterAssetReq)
		wantErr error
	}{
		{
			"四条全坏：upload_id 先判",
			func(in *rpc.RegisterAssetReq) { in.UploadId, in.Mid, in.Bucket, in.ObjectKey = 0, 0, "", "" },
			model.ErrInvalidUploadID,
		},
		{"upload_id=0", func(in *rpc.RegisterAssetReq) { in.UploadId = 0 }, model.ErrInvalidUploadID},
		{"upload_id 负数", func(in *rpc.RegisterAssetReq) { in.UploadId = -1 }, model.ErrInvalidUploadID},
		{"upload_id 合规后 mid=0", func(in *rpc.RegisterAssetReq) { in.Mid = 0 }, model.ErrInvalidMid},
		{"mid 负数", func(in *rpc.RegisterAssetReq) { in.Mid = -fakeMid }, model.ErrInvalidMid},
		{
			"bucket 与 object_key 同坏：bucket 先判",
			func(in *rpc.RegisterAssetReq) { in.Bucket, in.ObjectKey = "", "" },
			model.ErrInvalidBucket,
		},
		{"只剩 object_key 空", func(in *rpc.RegisterAssetReq) { in.ObjectKey = "" }, model.ErrInvalidObjectKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := validRegisterReq()
			c.mutate(req)
			st := newStore()
			l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))

			got, err := l.RegisterAsset(req)

			wantErrIdentity(t, "RegisterAsset", err, c.wantErr)
			if got != nil {
				t.Errorf("RegisterAsset(%s) 应答 = %v, want nil", c.name, got)
			}
			wantNoCall(t, "RegisterAsset 守卫", st, 0)
			if len(st.meta.rows) != 0 {
				t.Errorf("守卫拒绝后库里仍留下 %d 行媒资", len(st.meta.rows))
			}
		})
	}

	// 正向对照：同一套布景下入参合规必须放行，并真的落到 INSERT。
	st := newStore()
	l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.RegisterAsset(validRegisterReq())
	wantNoErr(t, "RegisterAsset 合规入参", err)
	wantOps(t, "合规入参只写库+写缓存", st.log.opsFrom(0), []string{insertOpOf(fakeUploadID, fakeMid), setOpOf(got.GetAssetId())})
}

// TestRegisterAssetWritesUploadedRowAndReturnsGeneratedID 钉登记后的库存行形态：
// 主键由 INSERT 分配（入参里根本没有 asset_id）、状态固定 UPLOADED、
// 探测四字段（duration/width/height/codec）留空——它们只能由 UpdateAssetMeta 写。
func TestRegisterAssetWritesUploadedRowAndReturnsGeneratedID(t *testing.T) {
	st := newStore()
	before := time.Now().Unix()
	l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.RegisterAsset(validRegisterReq())
	after := time.Now().Unix()
	wantNoErr(t, "RegisterAsset", err)

	id := got.GetAssetId()
	if id <= 0 {
		t.Fatalf("应答 asset_id = %d, want 由 INSERT 分配的正主键", id)
	}
	row := st.meta.row(id)
	if row == nil {
		t.Fatalf("应答给了 asset_id=%d 但库里没有这一行", id)
	}
	// 应答 == 库存行（assetLine 覆盖除 ctime/mtime 外的全部列）。
	wantEQ(t, "登记应答", "整行", replyAssetLine(got), assetLine(row))
	wantEQ(t, "初始状态", "库存 state", row.State, int32(model.StateUploaded))
	wantEQ(t, "初始状态", "应答 state", got.GetState(), rpc.AssetState_STATE_UPLOADED)
	// 归属与引用字段逐项跟着入参，不能被服务端改写。
	wantEQ(t, "入参透传", "upload_id", row.UploadID, fakeUploadID)
	wantEQ(t, "入参透传", "mid", row.Mid, int64(fakeMid))
	wantEQ(t, "入参透传", "bucket", row.Bucket, fakeBucket)
	wantEQ(t, "入参透传", "object_key", row.ObjectKey, fakeOriginKey)
	wantEQ(t, "入参透传", "size", row.Size, regSize)
	wantEQ(t, "入参透传", "md5", row.Md5, fakeMd5)
	// 登记时点没有任何探测结果。
	wantEQ(t, "探测字段留空", "duration", row.Duration, int64(0))
	wantEQ(t, "探测字段留空", "width", row.Width, int32(0))
	wantEQ(t, "探测字段留空", "height", row.Height, int32(0))
	wantEQ(t, "探测字段留空", "codec", row.Codec, "")
	wantEQ(t, "探测字段留空", "应答 duration", got.GetDuration(), int64(0))
	wantEQ(t, "探测字段留空", "应答 codec", got.GetCodec(), "")
	// 时间戳由 logic 现取，且登记时 ctime==mtime（同一行还没被改过）。
	wantUnixWindow(t, "登记时间", "库存 ctime", row.Ctime, before, after)
	wantUnixWindow(t, "登记时间", "库存 mtime", row.Mtime, before, after)
	wantEQ(t, "登记时间", "ctime==mtime", row.Ctime, row.Mtime)
	wantEQ(t, "登记时间", "应答 ctime", got.GetCtime(), row.Ctime)
	wantEQ(t, "登记时间", "应答 mtime", got.GetMtime(), row.Mtime)
	// 顺序：先 INSERT 再写缓存，没有一次读。
	wantOps(t, "登记顺序", st.log.opsFrom(0), []string{insertOpOf(fakeUploadID, fakeMid), setOpOf(id)})
}

// TestRegisterAssetPrewarmsDetailCacheWithStoredRow 钉一个容易漏掉的副作用：
// Repository.RegisterAsset 在 INSERT 之后**写**缓存（repository.go:144），不是失效。
// 于是新登记的媒资立刻可被读穿命中，且缓存里的行主键已经回填
// （`m.AssetID = id` 发生在 jsonMustMarshal 之前，与读侧「缓存载荷不校验主键」不同，
// 这条写入路径自己保证了 key 与 payload 一致）。
func TestRegisterAssetPrewarmsDetailCacheWithStoredRow(t *testing.T) {
	st := newStore()
	l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.RegisterAsset(validRegisterReq())
	wantNoErr(t, "RegisterAsset", err)
	id := got.GetAssetId()

	cached := st.cache.storedAsset(id)
	if cached == nil {
		payload, ok := st.cache.stored(id)
		t.Fatalf("登记后缓存里没有整行（stored=%q,%v）", payload, ok)
	}
	wantEQ(t, "登记预热的缓存内容", "整行", assetLine(cached), assetLine(st.meta.row(id)))
	wantEQ(t, "登记预热的缓存内容", "asset_id", cached.AssetID, id)

	// 判别力对照：紧接着读一次详情，必须只碰缓存、不回源。
	before := st.log.snapshot()
	detail, err := NewGetAssetLogic(context.Background(), newTestSvc(st)).GetAsset(&rpc.AssetReq{AssetId: id})
	wantNoErr(t, "GetAsset 登记后", err)
	wantOps(t, "登记后读详情不回源", st.log.opsFrom(before), []string{"cache.Get:" + keyAsset(id)})
	wantEQ(t, "登记后读详情", "state", detail.GetState(), rpc.AssetState_STATE_UPLOADED)
	wantCount(t, "登记后读详情不查库", st.log, "meta.FindOne", 0)
}

// TestRegisterAssetCacheWriteFailureIsSwallowed 钉「写缓存失败被就地吞掉」：
// repository.go:144 是 `_ = r.cache.SetAsset(...)`，连日志都没有。
// 结果是登记成功、库存行完整，但缓存里什么都没有，后续读全部回源（不自愈也无从察觉）。
func TestRegisterAssetCacheWriteFailureIsSwallowed(t *testing.T) {
	st := newStore()
	st.cache.failWith("SetAsset", errBoom)
	l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))

	got, err := l.RegisterAsset(validRegisterReq())
	wantNoErr(t, "RegisterAsset 缓存写失败", err)
	id := got.GetAssetId()
	if id <= 0 {
		t.Fatalf("应答 asset_id = %d, want 正主键", id)
	}
	if st.meta.row(id) == nil {
		t.Fatalf("缓存写失败连带没写库：登记必须只依赖 INSERT")
	}
	wantOps(t, "登记仍尝试写缓存", st.log.opsFrom(0), []string{insertOpOf(fakeUploadID, fakeMid), setOpOf(id)})
	if _, ok := st.cache.stored(id); ok {
		t.Errorf("SetAsset 已注入失败却仍写入缓存：替身漏实现，本用例失去判别力")
	}

	// 后果：下一位读者只能回源（读穿再回填）。
	before := st.log.snapshot()
	if _, err := NewGetAssetLogic(context.Background(), newTestSvc(st)).GetAsset(&rpc.AssetReq{AssetId: id}); err != nil {
		t.Fatalf("GetAsset：%v", err)
	}
	wantOps(t, "登记预热没生效时读侧回源", st.log.opsFrom(before),
		[]string{"cache.Get:" + keyAsset(id), "meta.FindOne:" + itoa(id), "cache.Set:" + keyAsset(id)})
}

// TestRegisterAssetInsertFailureWritesNothingAtAll 钉失败残留形态：
// INSERT 失败 ⇒ 应答 nil、错误原样上抛（logic 只 Errorf 不改写）、
// **一次缓存写都不发生**（repository.go:140-142 在 SetAsset 之前就 return 了）、库里零行。
func TestRegisterAssetInsertFailureWritesNothingAtAll(t *testing.T) {
	st := newStore()
	st.meta.failWith("Insert", errBoom)
	l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))

	got, err := l.RegisterAsset(validRegisterReq())
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "登记 INSERT 失败上抛", err, errBoom)
	wantOps(t, "失败顺序", st.log.opsFrom(0), []string{insertOpOf(fakeUploadID, fakeMid)})
	wantCount(t, "失败不写缓存", st.log, "cache.", 0)
	if len(st.meta.rows) != 0 {
		t.Errorf("INSERT 失败后库里仍有 %d 行：%v", len(st.meta.rows), lines(st.meta.snapshotByPK(), assetLine))
	}

	// 故障只打在注入的那一次上：清掉后同一请求应当能登记成功，且库里恰好一行。
	st.meta.failWith("Insert", nil)
	retry, err := l.RegisterAsset(validRegisterReq())
	wantNoErr(t, "RegisterAsset 重试", err)
	wantEQ(t, "重试后", "库存行数", len(st.meta.rows), 1)
	wantEQ(t, "重试后", "应答行", replyAssetLine(retry), assetLine(st.meta.row(retry.GetAssetId())))
}

// TestRegisterAssetDoesNotDedupSameUploadID 钉缺口 2 的现状：
// upload_id 上只有非唯一 KEY idx_upload，logic/model 也**没有**先查再插，
// 所以同一个 upload_id 的两次登记产生两个 asset_id、两行 asset_meta，
// 两行各自推进状态机。幂等重放拿到的不是既有 asset_id。
// 轨迹里两次都是「INSERT 打头」，一次读都没有——这就是「没有先查再插」的行为证据。
func TestRegisterAssetDoesNotDedupSameUploadID(t *testing.T) {
	st := newStore()
	l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))

	first, err := l.RegisterAsset(validRegisterReq())
	wantNoErr(t, "第一次登记", err)
	second, err := l.RegisterAsset(validRegisterReq())
	wantNoErr(t, "第二次登记（同 upload_id 同 object_key）", err)

	if first.GetAssetId() == second.GetAssetId() {
		t.Fatalf("重复登记返回了同一个 asset_id=%d：与缺口 2 的现状不符，先确认是否已加唯一键", first.GetAssetId())
	}
	wantOps(t, "两次都是直接 INSERT", st.log.opsFrom(0), []string{
		insertOpOf(fakeUploadID, fakeMid), setOpOf(first.GetAssetId()),
		insertOpOf(fakeUploadID, fakeMid), setOpOf(second.GetAssetId()),
	})
	wantCount(t, "重复登记不先查库", st.log, "meta.FindOne", 0)
	wantCount(t, "重复登记不先查库", st.log, "meta.Count", 0)
	rows := st.meta.snapshotByPK()
	wantEQ(t, "重复登记", "库存行数", len(rows), 2)
	for _, r := range rows {
		wantEQ(t, "两行同 upload_id", "upload_id", r.UploadID, fakeUploadID)
		wantEQ(t, "两行同 object_key", "object_key", r.ObjectKey, fakeOriginKey)
		wantEQ(t, "两行各自的状态机起点", "state", r.State, int32(model.StateUploaded))
	}
	// 两行各自有独立缓存条目：读侧拿到哪一条取决于调用方手里哪个 asset_id。
	firstCached, secondCached := st.cache.storedAsset(first.GetAssetId()), st.cache.storedAsset(second.GetAssetId())
	if firstCached == nil || secondCached == nil {
		t.Fatalf("两行都应各自预热一条缓存：first=%v second=%v", firstCached, secondCached)
	}
	wantEQ(t, "first 的缓存内容", "整行", assetLine(firstCached), assetLine(st.meta.row(first.GetAssetId())))
	wantEQ(t, "second 的缓存内容", "整行", assetLine(secondCached), assetLine(st.meta.row(second.GetAssetId())))
}

// TestRegisterAssetDoesNotValidateSizeMd5OrWhitespace 钉守卫的**覆盖面**：
// 四条守卫只看 upload_id/mid/bucket/object_key，不看 size、md5，也不 trim 空白。
// 于是负数 size、空 md5、纯空格 bucket/object_key 会原样进 INSERT，
// 并出现在应答与预热的缓存里（md5 只有非唯一 KEY idx_md5，本服务也没有按 md5 查询的读路径）。
func TestRegisterAssetDoesNotValidateSizeMd5OrWhitespace(t *testing.T) {
	st := newStore()
	req := validRegisterReq()
	req.Size = -regSize
	req.Md5 = ""
	req.Bucket = " "
	req.ObjectKey = "  "

	l := NewRegisterAssetLogic(context.Background(), newTestSvc(st))
	got, err := l.RegisterAsset(req)
	wantNoErr(t, "RegisterAsset 脏值", err)

	row := st.meta.row(got.GetAssetId())
	if row == nil {
		t.Fatalf("库里没有登记行")
	}
	wantEQ(t, "负数 size 原样入库", "size", row.Size, -regSize)
	wantEQ(t, "空 md5 原样入库", "md5", row.Md5, "")
	wantEQ(t, "空白 bucket 原样入库", "bucket", row.Bucket, " ")
	wantEQ(t, "空白 object_key 原样入库", "object_key", row.ObjectKey, "  ")
	// 应答与缓存都把脏值带了出去：调用方看不出这是被拒过的入参。
	wantEQ(t, "脏值进应答", "应答", replyAssetLine(got), assetLine(row))
	wantEQ(t, "脏值进缓存", "缓存", assetLine(st.cache.storedAsset(got.GetAssetId())), assetLine(row))
}
