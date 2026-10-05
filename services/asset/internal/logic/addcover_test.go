package logic

// addcover_test.go 覆盖写侧附件登记 AddCover：三条守卫的先后、归属键到底是谁、
// 「存在性/权限前置检查有没有发生在 SQL 之前」这个关键问题（答案：完全没有），
// 以及重复登记的行为——asset_cover 上只有非唯一 KEY idx_asset，
// 所以同一 (asset_id, object_key) 重放就是**追加新行**，既不去重也不覆盖。
//
// AddCoverReq 里根本没有 cover_id 字段（rpc/asset.proto:100-106），
// 主键一律由 INSERT 分配；应答里的 cover_id 必须能在库里查到同一行。

import (
	"context"
	"testing"
	"time"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

func validCoverReq(assetID int64) *rpc.AddCoverReq {
	return &rpc.AddCoverReq{
		AssetId:   assetID,
		Bucket:    fakeBucket,
		ObjectKey: fakeCoverKey(1),
		Width:     1920,
		Height:    1080,
	}
}

// TestAddCoverGuardsAssetIDThenBucketThenObjectKey 钉守卫顺序（addcoverlogic.go:31-39）：
// asset_id → bucket → object_key，被拒时一次依赖都不碰，
// 也不产生任何行（附件写没有事务，失败就是彻底的无副作用）。
func TestAddCoverGuardsAssetIDThenBucketThenObjectKey(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(in *rpc.AddCoverReq)
		wantErr error
	}{
		{"三条全坏：先判 asset_id", func(in *rpc.AddCoverReq) { in.AssetId, in.Bucket, in.ObjectKey = 0, "", "" }, model.ErrInvalidAssetID},
		{"asset_id 负数", func(in *rpc.AddCoverReq) { in.AssetId = -fakeAssetID }, model.ErrInvalidAssetID},
		{"bucket 与 object_key 同坏：先判 bucket", func(in *rpc.AddCoverReq) { in.Bucket, in.ObjectKey = "", "" }, model.ErrInvalidBucket},
		{"只剩 object_key 空", func(in *rpc.AddCoverReq) { in.ObjectKey = "" }, model.ErrInvalidObjectKey},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore()
			seedAssetInState(t, st, fakeAssetID, model.StateUploaded) // 媒资真实存在：失败只能来自守卫
			seedCover(t, st, &model.AssetCover{CoverID: 4001, AssetID: fakeAssetID, Bucket: fakeBucket, ObjectKey: fakeCoverKey(1)})
			l := NewAddCoverLogic(context.Background(), newTestSvc(st))

			req := validCoverReq(fakeAssetID)
			c.mutate(req)
			got, err := l.AddCover(req)

			wantErrIdentity(t, "AddCover", err, c.wantErr)
			if got != nil {
				t.Errorf("AddCover(%s) 应答 = %v, want nil", c.name, got)
			}
			wantNoCall(t, "AddCover 守卫", st, 0)
			wantEQ(t, "守卫拒绝", "库存封面行数", len(st.cover.rows), 1)
		})
	}

	st := newStore()
	l := NewAddCoverLogic(context.Background(), newTestSvc(st))
	got, err := l.AddCover(validCoverReq(fakeAssetID))
	wantNoErr(t, "AddCover 合规入参", err)
	wantEQ(t, "合规入参", "库存封面行数", len(st.cover.rows), 1)
	wantEQ(t, "合规入参", "落库归属", st.cover.row(got.GetCoverId()).AssetID, fakeAssetID)
	wantOps(t, "合规入参只发一条 INSERT", st.log.opsFrom(0), []string{opCoverInsert(fakeAssetID)})
}

// TestAddCoverWritesRowOwnedByRequestAssetIDAndReturnsGeneratedCoverID 钉归属键与主键来源：
// 落库的归属列是**入参 asset_id**（不是任何媒资自身的主键推导），
// cover_id 由 INSERT 分配并回到应答里；ctime 由 logic 现取。
// 同时钉「前置检查的位置」：整条序列只有一条 INSERT，
// 既没有为存在性/归属而发的 SELECT（meta.FindOne / meta.Count 均为 0），也没碰过缓存。
func TestAddCoverWritesRowOwnedByRequestAssetIDAndReturnsGeneratedCoverID(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded) // 媒资确实存在：与「不校验」形成对照
	before := time.Now().Unix()
	l := NewAddCoverLogic(context.Background(), newTestSvc(st))

	got, err := l.AddCover(validCoverReq(fakeAssetID))
	after := time.Now().Unix()
	wantNoErr(t, "AddCover", err)

	if got.GetCoverId() <= 0 {
		t.Fatalf("应答 cover_id = %d, want 由 INSERT 分配的正主键", got.GetCoverId())
	}
	row := st.cover.row(got.GetCoverId())
	if row == nil {
		t.Fatalf("应答 cover_id=%d 在库里不存在", got.GetCoverId())
	}
	wantEQ(t, "归属键", "asset_id", row.AssetID, fakeAssetID)
	wantEQ(t, "归属键", "应答 asset_id", got.GetAssetId(), fakeAssetID)
	wantEQ(t, "应答 == 库存行", "整行", replyCoverLine(got), coverLine(row))
	wantEQ(t, "入参透传", "bucket", row.Bucket, fakeBucket)
	wantEQ(t, "入参透传", "object_key", row.ObjectKey, fakeCoverKey(1))
	wantEQ(t, "入参透传", "width", row.Width, int32(1920))
	wantEQ(t, "入参透传", "height", row.Height, int32(1080))
	wantUnixWindow(t, "登记时间", "ctime", row.Ctime, before, after)
	// 关键前置检查问题：没有任何一次读，也没有任何缓存动作。
	wantOps(t, "只发一条 INSERT", st.log.opsFrom(0), []string{opCoverInsert(fakeAssetID)})
	wantCount(t, "不校验媒资是否存在", st.log, "meta.", 0)
	wantCount(t, "附件写不动详情缓存", st.log, "cache.", 0)
}

// TestAddCoverAcceptsAssetIDThatDoesNotExist 钉缺口 6 的现状：
// asset_meta 里一行都没有，AddCover 仍然成功落库。
// addcoverlogic.go:29 的注释把这件事推给「DB 外键或上层调用顺序」，
// 但 000002 迁移里没有任何 FOREIGN KEY，所以这条孤儿引用无人拦。
func TestAddCoverAcceptsAssetIDThatDoesNotExist(t *testing.T) {
	st := newStore()
	const orphan = int64(888888) // 媒资表里刻意没有这一行
	l := NewAddCoverLogic(context.Background(), newTestSvc(st))

	got, err := l.AddCover(validCoverReq(orphan))
	wantNoErr(t, "AddCover 挂在不存在的媒资上（现状）", err)
	wantEQ(t, "孤儿封面", "归属 asset_id", got.GetAssetId(), orphan)
	if st.meta.row(orphan) != nil {
		t.Fatalf("布景失真：媒资 %d 竟然存在，本用例失去判别力", orphan)
	}
	row := st.cover.row(got.GetCoverId())
	if row == nil {
		t.Fatalf("孤儿封面没落库")
	}
	wantEQ(t, "孤儿封面", "整行", replyCoverLine(got), coverLine(row))
	wantOps(t, "仍然只有一条 INSERT", st.log.opsFrom(0), []string{opCoverInsert(orphan)})
}

// TestAddCoverRepeatedCallsAppendRowsWithoutDedup 钉重复插入的行为：
// 同 (asset_id, bucket, object_key) 再登记一次 ⇒ 第二个 cover_id、第二行，
// 且新行立刻能被 ListCovers 读出来（前端会看到两张一模一样的封面）。
// 序列里两条 INSERT 之间没有任何读，说明「先查再插」这条路径不存在。
func TestAddCoverRepeatedCallsAppendRowsWithoutDedup(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	l := NewAddCoverLogic(context.Background(), newTestSvc(st))

	first, err := l.AddCover(validCoverReq(fakeAssetID))
	wantNoErr(t, "第一次登记封面", err)
	second, err := l.AddCover(validCoverReq(fakeAssetID))
	wantNoErr(t, "第二次登记同一份封面", err)

	if first.GetCoverId() == second.GetCoverId() {
		t.Fatalf("重复登记返回了同一个 cover_id=%d：与「无唯一键、直接追加」的现状不符", first.GetCoverId())
	}
	rows := st.cover.snapshotByPK()
	wantEQ(t, "重复登记", "库存行数", len(rows), 2)
	for _, r := range rows {
		wantEQ(t, "两行同归属", "asset_id", r.AssetID, fakeAssetID)
		wantEQ(t, "两行同引用", "object_key", r.ObjectKey, fakeCoverKey(1))
	}
	wantOps(t, "两次都是直接 INSERT", st.log.opsFrom(0), []string{opCoverInsert(fakeAssetID), opCoverInsert(fakeAssetID)})

	// 残留形态可被读侧看到：两条都在，按 cover_id 升序。
	list, err := NewListCoversLogic(context.Background(), newTestSvc(st)).ListCovers(&rpc.AssetReq{AssetId: fakeAssetID})
	wantNoErr(t, "ListCovers 复核", err)
	wantEQ(t, "读侧条数", "items", len(list.GetItems()), 2)
	wantSeq(t, "重复封面可被消费方看到", lines(list.GetItems(), replyCoverLine),
		[]string{coverLine(rows[0]), coverLine(rows[1])})
}

// TestAddCoverDoesNotValidateWidthOrHeight 钉守卫的覆盖面：
// width/height 完全不参与判定，0、负数都算合法封面尺寸并原样入库。
func TestAddCoverDoesNotValidateWidthOrHeight(t *testing.T) {
	st := newStore()
	l := NewAddCoverLogic(context.Background(), newTestSvc(st))

	req := validCoverReq(fakeAssetID)
	req.Width, req.Height = -1920, 0
	got, err := l.AddCover(req)
	wantNoErr(t, "AddCover 非法尺寸（现状）", err)

	row := st.cover.row(got.GetCoverId())
	if row == nil {
		t.Fatalf("库里没有这一行封面")
	}
	wantEQ(t, "负宽原样入库", "width", row.Width, int32(-1920))
	wantEQ(t, "零高原样入库", "height", row.Height, int32(0))
	wantEQ(t, "应答如实带出脏值", "整行", replyCoverLine(got), coverLine(row))
}

// TestAddCoverInsertFailurePropagatesAndLeavesNoRow 钉失败残留：
// INSERT 失败 ⇒ 应答 nil、错误原样上抛（logic 只 Errorf）、库里零行、缓存没被碰。
func TestAddCoverInsertFailurePropagatesAndLeavesNoRow(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateUploaded)
	st.cover.failWith("Insert", errBoom)
	l := NewAddCoverLogic(context.Background(), newTestSvc(st))

	got, err := l.AddCover(validCoverReq(fakeAssetID))
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "封面 INSERT 失败上抛", err, errBoom)
	wantOps(t, "失败顺序", st.log.opsFrom(0), []string{opCoverInsert(fakeAssetID)})
	wantEQ(t, "失败残留", "库存封面行数", len(st.cover.rows), 0)
	wantCount(t, "失败不碰缓存", st.log, "cache.", 0)

	// 故障只打在注入那一次：清掉后重试才真正落一行。
	st.cover.failWith("Insert", nil)
	retry, err := l.AddCover(validCoverReq(fakeAssetID))
	wantNoErr(t, "AddCover 重试", err)
	wantEQ(t, "重试后", "库存封面行数", len(st.cover.rows), 1)
	wantEQ(t, "重试后", "整行", replyCoverLine(retry), coverLine(st.cover.row(retry.GetCoverId())))
}
