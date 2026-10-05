package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestDelFolderRejectsGuardsBeforeTouchingDeps 注意顺序：DelFolder 先校验 fid 再校验 mid，
// 与 AddFav/DelFav 相反。用例锁住这个顺序，避免「顺手统一」时改了错误语义。
func TestDelFolderRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.DelFolderReq
		want error
	}{
		{"fid 为 0", &rpc.DelFolderReq{Fid: 0, Mid: 7}, model.ErrInvalidFid},
		{"fid 为负", &rpc.DelFolderReq{Fid: -3, Mid: 7}, model.ErrInvalidFid},
		{"mid 为 0（fid 合法）", &rpc.DelFolderReq{Fid: 33, Mid: 0}, model.ErrInvalidMid},
		{"mid 为负", &rpc.DelFolderReq{Fid: 33, Mid: -7}, model.ErrInvalidMid},
		{"两者都非法时 fid 优先", &rpc.DelFolderReq{Fid: 0, Mid: 0}, model.ErrInvalidFid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedFolder(st, 33, 7, "只看番", 1, model.FolderStateNormal, 0)
			before := st.log.snapshot()
			got, err := NewDelFolderLogic(context.Background(), newTestSvc(st)).DelFolder(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestDelFolderSoftDeletesOwnFolderAndInvalidatesCache 正常路径逐字段 + 顺序。
func TestDelFolderSoftDeletesOwnFolderAndInvalidatesCache(t *testing.T) {
	st := newStore()
	seedFolder(st, 33, 7, "只看番", 1, model.FolderStateNormal, 9)

	got, err := NewDelFolderLogic(context.Background(), newTestSvc(st)).
		DelFolder(&rpc.DelFolderReq{Fid: 33, Mid: 7})
	wantNoErr(t, "DelFolder", err)
	if got == nil {
		t.Fatalf("DelFolder() 响应 = nil")
	}
	wantOps(t, "DelFolder", st.log.opsFrom(0), []string{
		"folder.Del:33:7",
		"cache.DelFolders:7",
	})
	row := st.folder.get(33)
	if row == nil {
		t.Fatalf("favorite_folder 行丢了（软删不该删行）")
	}
	wantEQ(t, "删夹", "State 软删", row.State, int32(model.FolderStateDeleted))
	wantEQ(t, "删夹", "Mid 保留", row.Mid, int64(7))
	wantEQ(t, "删夹", "Name 保留（审计证据）", row.Name, "只看番")
	wantEQ(t, "删夹", "Public 保留", row.Public, int32(1))
	wantEQ(t, "删夹", "Count 快照未被顺手改", row.Count, int32(9))
	isRecentUnix(t, "删夹", "Mtime 已刷新", row.Mtime)
}

// TestDelFolderRejectsOtherUsersFolder 越权删别人的夹必须报 ErrFolderNotFoundOrForbidden，
// 且**不能**失效调用方的列表缓存（否则会误伤缓存）、不能改目标行。
// WHERE fid=? AND mid=? 的「不区分不存在与不属于本人」也是有意的：不泄露别人夹子是否存在。
func TestDelFolderRejectsOtherUsersFolder(t *testing.T) {
	st := newStore()
	seedFolder(st, 33, 88, "别人的夹", 1, model.FolderStateNormal, 3)
	st.cache.warmFolders(7, `[{"fid":1}]`)

	got, err := NewDelFolderLogic(context.Background(), newTestSvc(st)).
		DelFolder(&rpc.DelFolderReq{Fid: 33, Mid: 7})
	wantFail(t, "删别人的夹", got, err, model.ErrFolderNotFoundOrForbidden)
	wantOps(t, "删别人的夹", st.log.opsFrom(0), []string{"folder.Del:33:7"})
	wantEQ(t, "删别人的夹", "目标行状态未变", st.folder.get(33).State, int32(model.FolderStateNormal))
	wantEQ(t, "删别人的夹", "本人缓存未失效", st.cache.foldersPayload(7), `[{"fid":1}]`)
}

// TestDelFolderRejectsMissingFolder fid 不存在同样是 ErrFolderNotFoundOrForbidden
// （model 只在 RowsAffected==0 时报错），不返回成功。
func TestDelFolderRejectsMissingFolder(t *testing.T) {
	st := newStore()

	got, err := NewDelFolderLogic(context.Background(), newTestSvc(st)).
		DelFolder(&rpc.DelFolderReq{Fid: 404, Mid: 7})
	wantFail(t, "删不存在的夹", got, err, model.ErrFolderNotFoundOrForbidden)
	wantOps(t, "删不存在的夹", st.log.opsFrom(0), []string{"folder.Del:404:7"})
	wantEQ(t, "删不存在的夹", "行数", len(st.folder.rows), 0)
	wantCount(t, "删不存在的夹", st.log, "cache.DelFolders", 0)
}

// TestDelFolderRepeatedDeleteStillSucceeds 软删 WHERE 里**没有** state=0 门槛，
// 所以第二次删除仍算命中、仍返回成功。这是现状（幂等对客户端友好），
// 但也意味着「删已删的夹」和「删不存在的夹」在错误码上不一致 —— 见 README 已知缺口。
func TestDelFolderRepeatedDeleteStillSucceeds(t *testing.T) {
	st := newStore()
	seedFolder(st, 33, 7, "只看番", 0, model.FolderStateNormal, 0)
	l := NewDelFolderLogic(context.Background(), newTestSvc(st))

	if _, err := l.DelFolder(&rpc.DelFolderReq{Fid: 33, Mid: 7}); err != nil {
		t.Fatalf("第一次删夹：%v", err)
	}
	got, err := l.DelFolder(&rpc.DelFolderReq{Fid: 33, Mid: 7})
	wantNoErr(t, "第二次删夹", err)
	if got == nil {
		t.Fatalf("DelFolder() 响应 = nil")
	}
	wantOps(t, "重复删夹", st.log.opsFrom(0), []string{
		"folder.Del:33:7", "cache.DelFolders:7",
		"folder.Del:33:7", "cache.DelFolders:7",
	})
	wantEQ(t, "重复删夹", "State 仍是删除态", st.folder.get(33).State, int32(model.FolderStateDeleted))
	wantEQ(t, "重复删夹", "行数没变", len(st.folder.rows), 1)
}

// TestDelFolderDoesNotCascadeItems_KnownGap 删夹不级联处理夹内收藏项（logic 注释里
// 写明「由调用方保证为空」，服务端不校验）。后果：夹已删，但夹里那条 favorite_item
// 仍然 state=0，IsFavored 继续报「已收藏」，而 UserFolders 已经看不到那个夹。
// 本用例锁现象，不做修复（缺陷 #6）。
func TestDelFolderDoesNotCascadeItems_KnownGap(t *testing.T) {
	st := newStore()
	seedFolder(st, 33, 7, "只看番", 1, model.FolderStateNormal, 1)
	seedFavItem(st, 7, 10001, 33, 2, 11, 0)

	_, err := NewDelFolderLogic(context.Background(), newTestSvc(st)).
		DelFolder(&rpc.DelFolderReq{Fid: 33, Mid: 7})
	wantNoErr(t, "删夹", err)
	wantEQ(t, "删夹不级联", "favorite_item 未触碰", st.favItem.rowCount(), 1)
	wantEQ(t, "删夹不级联", "夹内收藏仍有效", st.favItem.get(7, 10001, 2).State, int32(0))
	wantOps(t, "删夹的完整副作用", st.log.opsFrom(0), []string{
		"folder.Del:33:7", "cache.DelFolders:7",
	})
}

// TestDelFolderPropagatesModelFailure folder.Del 失败时不失效缓存。
func TestDelFolderPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedFolder(st, 33, 7, "只看番", 1, model.FolderStateNormal, 0)
	st.cache.warmFolders(7, `[{"fid":33}]`)
	st.folder.failWith("Del", errBoom)

	got, err := NewDelFolderLogic(context.Background(), newTestSvc(st)).
		DelFolder(&rpc.DelFolderReq{Fid: 33, Mid: 7})
	wantFail(t, "Del 失败", got, err, errBoom)
	wantOps(t, "Del 失败后的调用", st.log.opsFrom(0), []string{"folder.Del:33:7"})
	wantEQ(t, "Del 失败", "状态未变", st.folder.get(33).State, int32(model.FolderStateNormal))
	wantEQ(t, "Del 失败", "缓存未失效", st.cache.foldersPayload(7), `[{"fid":33}]`)
}

// TestDelFolderSwallowsCacheFailure 缓存失败静默：软删已落库仍返回成功。
func TestDelFolderSwallowsCacheFailure(t *testing.T) {
	st := newStore()
	seedFolder(st, 33, 7, "只看番", 1, model.FolderStateNormal, 0)
	st.cache.warmFolders(7, `[{"fid":33}]`)
	st.cache.failWith("DelFolders", errBoom)

	got, err := NewDelFolderLogic(context.Background(), newTestSvc(st)).
		DelFolder(&rpc.DelFolderReq{Fid: 33, Mid: 7})
	wantNoErr(t, "缓存失败不影响删夹", err)
	if got == nil {
		t.Fatalf("DelFolder() 响应 = nil")
	}
	wantEQ(t, "缓存失败", "已软删", st.folder.get(33).State, int32(model.FolderStateDeleted))
	wantEQ(t, "缓存失败", "脏缓存仍在", st.cache.foldersPayload(7), `[{"fid":33}]`)
}
