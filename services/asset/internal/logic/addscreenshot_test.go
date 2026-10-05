package logic

// addscreenshot_test.go 覆盖写侧截图登记 AddScreenshot：四条守卫的先后
// （timestamp 判在最后、且在 bucket/object_key 之后）、归属键、重复插入的行为，
// 以及一条契约层面的事实：截图**只有写侧**，本服务没有任何读它的 rpc。
//
// asset_screenshot 上的 idx_asset_timestamp 是非唯一 KEY
// （deploy/migrations/asset/000002_create_asset_attachment_tables.sql:43），
// 所以同 (asset_id, timestamp) 重放就是追加新行；加上没有读接口，
// 这张表实际是只进不出（README 缺口 9）。

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"go-video/services/asset/model"
	"go-video/services/asset/rpc"
)

func validShotReq(assetID int64, ts int64) *rpc.AddScreenshotReq {
	return &rpc.AddScreenshotReq{
		AssetId:   assetID,
		Bucket:    fakeBucket,
		ObjectKey: "ugc-ut/fake/shot-01.jpg",
		Timestamp: ts,
	}
}

// TestAddScreenshotGuardsTimestampLast 钉守卫顺序（addscreenshotlogic.go:30-41）：
// asset_id → bucket → object_key → timestamp。
// 最要紧的是最后一条：timestamp 为负时，只要引用列还坏着，报出来的仍是引用列的错。
func TestAddScreenshotGuardsTimestampLast(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(in *rpc.AddScreenshotReq)
		wantErr error
	}{
		{"四条全坏：先判 asset_id", func(in *rpc.AddScreenshotReq) {
			in.AssetId, in.Bucket, in.ObjectKey, in.Timestamp = 0, "", "", -1
		}, model.ErrInvalidAssetID},
		{"asset_id 负数", func(in *rpc.AddScreenshotReq) { in.AssetId = -fakeAssetID }, model.ErrInvalidAssetID},
		{"负时间戳 + 空 bucket：先判 bucket", func(in *rpc.AddScreenshotReq) { in.Bucket, in.Timestamp = "", -1 }, model.ErrInvalidBucket},
		{"负时间戳 + 空 object_key：先判 object_key", func(in *rpc.AddScreenshotReq) { in.ObjectKey, in.Timestamp = "", -1 }, model.ErrInvalidObjectKey},
		{"引用列合规后只剩负时间戳", func(in *rpc.AddScreenshotReq) { in.Timestamp = -1 }, model.ErrInvalidTimestamp},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newStore()
			seedAssetInState(t, st, fakeAssetID, model.StateTranscoded)
			l := NewAddScreenshotLogic(context.Background(), newTestSvc(st))

			req := validShotReq(fakeAssetID, 1500)
			c.mutate(req)
			got, err := l.AddScreenshot(req)

			wantErrIdentity(t, "AddScreenshot", err, c.wantErr)
			if got != nil {
				t.Errorf("AddScreenshot(%s) 应答 = %v, want nil", c.name, got)
			}
			wantNoCall(t, "AddScreenshot 守卫", st, 0)
			wantEQ(t, "守卫拒绝", "库存截图行数", len(st.shot.rows), 0)
		})
	}

	// 边界正向对照：timestamp=0 合法（判的是 <0），且必须真的落库。
	st := newStore()
	l := NewAddScreenshotLogic(context.Background(), newTestSvc(st))
	got, err := l.AddScreenshot(validShotReq(fakeAssetID, 0))
	wantNoErr(t, "AddScreenshot timestamp=0", err)
	wantEQ(t, "零时间戳", "库存 ts", st.shot.row(got.GetShotId()).Timestamp, int64(0))
	wantOps(t, "合规入参只发一条 INSERT", st.log.opsFrom(0), []string{opShotInsert(fakeAssetID)})
}

// TestAddScreenshotWritesRowOwnedByAssetID 钉归属键与前置检查位置：
// 落库列是入参 asset_id + timestamp，shot_id 由 INSERT 分配；
// 整条序列只有这一条 INSERT，没有任何存在性 SELECT，也不碰详情缓存。
func TestAddScreenshotWritesRowOwnedByAssetID(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateTranscoded)
	before := time.Now().Unix()
	l := NewAddScreenshotLogic(context.Background(), newTestSvc(st))

	got, err := l.AddScreenshot(validShotReq(fakeAssetID, 1500))
	after := time.Now().Unix()
	wantNoErr(t, "AddScreenshot", err)

	row := st.shot.row(got.GetShotId())
	if row == nil {
		t.Fatalf("应答 shot_id=%d 在库里不存在", got.GetShotId())
	}
	wantEQ(t, "应答 == 库存行", "整行", replyShotLine(got), shotLine(row))
	wantEQ(t, "归属键", "asset_id", row.AssetID, fakeAssetID)
	wantEQ(t, "时间点", "timestamp", row.Timestamp, int64(1500))
	wantEQ(t, "入参透传", "bucket", row.Bucket, fakeBucket)
	wantEQ(t, "入参透传", "object_key", row.ObjectKey, "ugc-ut/fake/shot-01.jpg")
	wantUnixWindow(t, "登记时间", "ctime", row.Ctime, before, after)
	wantOps(t, "只发一条 INSERT", st.log.opsFrom(0), []string{opShotInsert(fakeAssetID)})
	wantCount(t, "不校验媒资是否存在", st.log, "meta.", 0)
	wantCount(t, "附件写不动详情缓存", st.log, "cache.", 0)
}

// TestAddScreenshotAcceptsAssetIDThatDoesNotExistAndUngroundedTimestamp 钉两条现状：
// 媒资不存在照样落库（缺口 6）；timestamp 只判 <0，
// 既不判上界、也不与媒资自身 duration 比对（duration 根本没被读出来）。
func TestAddScreenshotAcceptsAssetIDThatDoesNotExistAndUngroundedTimestamp(t *testing.T) {
	st := newStore()
	const orphan = int64(888888)
	l := NewAddScreenshotLogic(context.Background(), newTestSvc(st))

	got, err := l.AddScreenshot(validShotReq(orphan, 1<<62))
	wantNoErr(t, "AddScreenshot 挂在不存在媒资 + 极端时间戳（现状）", err)
	if st.meta.row(orphan) != nil {
		t.Fatalf("布景失真：媒资 %d 竟然存在", orphan)
	}
	row := st.shot.row(got.GetShotId())
	if row == nil {
		t.Fatalf("孤儿截图没落库")
	}
	wantEQ(t, "极端时间戳原样入库", "timestamp", row.Timestamp, int64(1<<62))
	wantEQ(t, "孤儿归属键", "asset_id", row.AssetID, orphan)
	// 判前不读 duration：整条序列里连一次 meta.FindOne 都没有。
	wantOps(t, "仍然只有一条 INSERT", st.log.opsFrom(0), []string{opShotInsert(orphan)})
}

// TestAddScreenshotRepeatedSameTimestampAppendsRows 钉重复插入的行为：
// idx_asset_timestamp 非唯一 ⇒ 同 (asset_id, timestamp) 重放得到第二个 shot_id、第二行。
// 转码任务重试一次，同一画面就会堆积多张截图，而本服务没有任何读接口能发现或清理它们。
func TestAddScreenshotRepeatedSameTimestampAppendsRows(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateTranscoded)
	l := NewAddScreenshotLogic(context.Background(), newTestSvc(st))

	first, err := l.AddScreenshot(validShotReq(fakeAssetID, 1500))
	wantNoErr(t, "第一次登记截图", err)
	second, err := l.AddScreenshot(validShotReq(fakeAssetID, 1500))
	wantNoErr(t, "第二次登记同一时间点", err)

	if first.GetShotId() == second.GetShotId() {
		t.Fatalf("重复登记返回了同一个 shot_id=%d：与「非唯一索引 + 直接追加」的现状不符", first.GetShotId())
	}
	rows := st.shot.snapshotByPK()
	wantEQ(t, "重复登记", "库存行数", len(rows), 2)
	wantSeq(t, "库存的两行就是这两次应答", lines(rows, func(s *model.AssetScreenshot) string {
		return "shot=" + itoa(s.ShotID)
	}), []string{"shot=" + itoa(first.GetShotId()), "shot=" + itoa(second.GetShotId())})
	// 两行除主键外逐字相同：转码重试一次，同一画面就多一张无人可查的截图。
	wantEQ(t, "两行同归属", "asset_id", rows[0].AssetID, rows[1].AssetID)
	wantEQ(t, "两行同时间点", "timestamp", rows[0].Timestamp, rows[1].Timestamp)
	wantEQ(t, "两行同引用", "object_key", rows[0].ObjectKey, rows[1].ObjectKey)
	wantOps(t, "两次都是直接 INSERT", st.log.opsFrom(0), []string{opShotInsert(fakeAssetID), opShotInsert(fakeAssetID)})
	// 没有任何读路径能把这两行查出来：本服务对 asset_screenshot 只有 Insert 一个 model 方法。
	wantCount(t, "截图列表无人可读", st.log, "shot.List", 0)
}

// TestScreenshotIsWriteOnlyInServiceContract 钉契约层面的事实（README 缺口 9 的前提）：
// Asset 服务的对外方法集就是这 10 个，截图只有 AddScreenshot、没有 ListScreenshots。
// 哪天补上读 rpc，本用例先红，逼着同时补读侧用例并改掉缺口 9。
func TestScreenshotIsWriteOnlyInServiceContract(t *testing.T) {
	typ := reflect.TypeOf((*rpc.AssetServer)(nil)).Elem()
	methods := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if !m.IsExported() {
			continue // mustEmbedUnimplementedAssetServer() 是生成物的编译期标记
		}
		methods = append(methods, m.Name)
	}
	slices.Sort(methods)

	wantStringsEQ(t, "Asset 服务契约", "方法集", methods, []string{
		"AddCover", "AddScreenshot", "AddSubtitle", "GetAsset", "ListAssets",
		"ListCovers", "ListSubtitles", "RegisterAsset", "TransitionState", "UpdateAssetMeta",
	})
	// 读侧成对的是封面/字幕，截图不成对：上面这条等式已经把 ListScreenshots 挡在外面，
	// 这里再单独点名，免得只改文档忘了改契约。
	for _, m := range methods {
		if slices.Contains([]string{"ListScreenshots", "GetScreenshot", "DeleteScreenshot"}, m) {
			t.Errorf("契约里出现了 %s：缺口 9 已收严，本用例与 README 要一起更新", m)
		}
	}
}

func TestAddScreenshotInsertFailurePropagatesAndLeavesNoRow(t *testing.T) {
	st := newStore()
	seedAssetInState(t, st, fakeAssetID, model.StateTranscoded)
	st.shot.failWith("Insert", errBoom)
	l := NewAddScreenshotLogic(context.Background(), newTestSvc(st))

	got, err := l.AddScreenshot(validShotReq(fakeAssetID, 1500))
	if got != nil {
		t.Errorf("应答 = %v, want nil", got)
	}
	wantErrIs(t, "截图 INSERT 失败上抛", err, errBoom)
	wantOps(t, "失败顺序", st.log.opsFrom(0), []string{opShotInsert(fakeAssetID)})
	wantEQ(t, "失败残留", "库存截图行数", len(st.shot.rows), 0)
	wantCount(t, "失败不碰缓存", st.log, "cache.", 0)
}
