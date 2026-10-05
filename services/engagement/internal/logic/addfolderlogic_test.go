package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestAddFolderRejectsGuardsBeforeTouchingDeps mid 与 name 两条守卫，顺序 mid 先。
// 空名收藏夹会让客户端列表里出现一条无名夹，属于必须拒的脏数据。
func TestAddFolderRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.AddFolderReq
		want error
	}{
		{"mid 为 0", &rpc.AddFolderReq{Mid: 0, Name: "日常收藏"}, model.ErrInvalidMid},
		{"mid 为负", &rpc.AddFolderReq{Mid: -1, Name: "日常收藏"}, model.ErrInvalidMid},
		{"name 空串", &rpc.AddFolderReq{Mid: 7, Name: ""}, model.ErrFolderNameEmpty},
		{"mid 非法优先于 name", &rpc.AddFolderReq{Mid: 0, Name: ""}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			before := st.log.snapshot()
			got, err := NewAddFolderLogic(context.Background(), newTestSvc(st)).AddFolder(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestAddFolderWritesRowAndInvalidatesListCache 逐字段投影 + 顺序。
// 只空白名（" "）不被拒——本服务不裁剪空格，那是客户端/运营的事，这里锁定现状。
func TestAddFolderWritesRowAndInvalidatesListCache(t *testing.T) {
	st := newStore()
	in := &rpc.AddFolderReq{Mid: 7, Name: "只看番", Description: "周末补番", Cover: "https://cdn/cover.png", Public: 1, Tp: 2}

	got, err := NewAddFolderLogic(context.Background(), newTestSvc(st)).AddFolder(in)
	wantNoErr(t, "AddFolder", err)
	if got == nil {
		t.Fatalf("AddFolder() 响应 = nil")
	}
	wantOps(t, "AddFolder", st.log.opsFrom(0), []string{
		"folder.Add:7:只看番",
		"cache.DelFolders:7",
	})
	wantEQ(t, "AddFolder", "返回的 fid = 自增主键", got.Fid, int64(1))

	row := st.folder.get(got.Fid)
	if row == nil {
		t.Fatalf("favorite_folder 未落库")
	}
	wantEQ(t, "落库", "Fid", row.Fid, int64(1))
	wantEQ(t, "落库", "Mid", row.Mid, int64(7))
	wantEQ(t, "落库", "Name", row.Name, "只看番")
	wantEQ(t, "落库", "Description", row.Description, "周末补番")
	wantEQ(t, "落库", "Cover", row.Cover, "https://cdn/cover.png")
	wantEQ(t, "落库", "Public", row.Public, int32(1))
	// INSERT 的 SQL 字面量把 state 钉成 0、count 钉成 0（logic 里显式给了 FolderStateNormal，
	// 但即使 logic 传错值也不会落库）——这条断言能抓住「以后有人把 state 改成透传」。
	wantEQ(t, "落库", "State", row.State, int32(model.FolderStateNormal))
	wantEQ(t, "落库", "Count 快照初值", row.Count, int32(0))
	// ctime/mtime 由 logic 传入（与 favorite_item 不同），断言落在运行窗口内。
	isRecentUnix(t, "落库", "Ctime", row.Ctime)
	isRecentUnix(t, "落库", "Mtime", row.Mtime)
}

// TestAddFolderPrivateKeptAsGiven 私密夹（public=0）必须按入参落库，
// 不能被默认成公开——那是越权暴露用户收藏夹。
func TestAddFolderPrivateKeptAsGiven(t *testing.T) {
	st := newStore()
	got, err := NewAddFolderLogic(context.Background(), newTestSvc(st)).
		AddFolder(&rpc.AddFolderReq{Mid: 7, Name: "私人清单"})
	wantNoErr(t, "建私密夹", err)
	wantEQ(t, "建私密夹", "Public", st.folder.get(got.Fid).Public, int32(0))
}

// TestAddFolderPropagatesModelFailure folder.Add 失败时不能失效列表缓存（否则
// 「缓存没了但也没建上」只会造成无谓回源），也不能返回半截 fid。
func TestAddFolderPropagatesModelFailure(t *testing.T) {
	st := newStore()
	st.folder.failWith("Add", errBoom)

	got, err := NewAddFolderLogic(context.Background(), newTestSvc(st)).
		AddFolder(&rpc.AddFolderReq{Mid: 7, Name: "只看番"})
	wantFail(t, "Add 失败", got, err, errBoom)
	wantOps(t, "Add 失败后的调用", st.log.opsFrom(0), []string{"folder.Add:7:只看番"})
	wantEQ(t, "Add 失败", "未落库", len(st.folder.rows), 0)
}

// TestAddFolderInvalidationOnlyOwnsMid 缓存失效只针对本人 mid：
// 建夹不影响他人列表缓存（AGENTS.md §5 的「不越权写别的投影」在本域的体现）。
func TestAddFolderInvalidationOnlyOwnsMid(t *testing.T) {
	st := newStore()
	st.cache.warmFolders(7, `[{"fid":1}]`)
	st.cache.warmFolders(8, `[{"fid":99}]`)

	_, err := NewAddFolderLogic(context.Background(), newTestSvc(st)).
		AddFolder(&rpc.AddFolderReq{Mid: 7, Name: "只看番"})
	wantNoErr(t, "AddFolder", err)
	wantEQ(t, "本人列表缓存", "已失效", st.cache.foldersPayload(7), "")
	wantEQ(t, "他人列表缓存", "不受影响", st.cache.foldersPayload(8), `[{"fid":99}]`)
	wantCount(t, "AddFolder", st.log, "cache.DelFolders:", 1)
}

// TestAddFolderSwallowsCacheFailure 缓存失效失败被丢弃（`_ =`），建夹仍成功；
// 后果是本人列表缓存在 TTL(60s) 内看不到新夹。登记为缺陷 #5 的一部分。
func TestAddFolderSwallowsCacheFailure(t *testing.T) {
	st := newStore()
	st.cache.warmFolders(7, `[{"fid":1}]`)
	st.cache.failWith("DelFolders", errBoom)

	got, err := NewAddFolderLogic(context.Background(), newTestSvc(st)).
		AddFolder(&rpc.AddFolderReq{Mid: 7, Name: "只看番"})
	wantNoErr(t, "DelFolders 失败不应影响建夹", err)
	wantEQ(t, "DelFolders 失败", "夹子已落库", len(st.folder.rows), 1)
	wantEQ(t, "DelFolders 失败", "脏缓存仍在", st.cache.foldersPayload(7), `[{"fid":1}]`)
	wantEQ(t, "DelFolders 失败", "fid", got.Fid, int64(1))
}

// TestAddFolderQuotaNotEnforced_KnownGap 注释与 errors.go 都写着「每人最多 100 个收藏夹」
// （model.ErrFolderLimitExceeded），但生产路径上没有任何 COUNT 校验：连建 3 个夹
// 也不触发一次计数查询，也不报错。这里锁现状（缺陷 #4），修复时本用例应改红。
func TestAddFolderQuotaNotEnforced_KnownGap(t *testing.T) {
	st := newStore()
	l := NewAddFolderLogic(context.Background(), newTestSvc(st))
	for i := range 3 {
		got, err := l.AddFolder(&rpc.AddFolderReq{Mid: 7, Name: "夹" + itoa(int64(i))})
		wantNoErr(t, "建夹", err)
		wantEQ(t, "建夹", "fid 递增", got.Fid, int64(i+1))
	}
	wantCount(t, "建夹", st.log, "folder.Count", 0)
	wantCount(t, "建夹", st.log, "folder.ListByUser", 0)
	wantEQ(t, "建夹", "落库行数", len(st.folder.rows), 3)
}
