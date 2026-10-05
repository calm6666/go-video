package logic

// favtargetcheck_test.go 钉住收藏**写侧**的归属与存在性口径（本包唯一的跨方法行为哨兵文件）。
//
// 为什么单独成文件而不塞进 addfavlogic_test.go：这里锁的是「AddFav 在落库之前到底读没读
// 别的行」这一条跨 AddFav/DelFav/UserFolders 的结论，且三条都是**已知缺陷的现状哨兵**
// （见每个用例的 TODO(缺陷) 注释与 README 的已知缺口编号），修好任何一条都会让对应用例转红，
// 届时按注释改成断言「被拒绝」。
//
// 对照事实（写断言前逐个核对过，不是照 README 猜的）：
//   - deploy/migrations/engagement/000002_create_favorite.sql：favorite_item **没有** fid 的
//     外键，只有 `KEY idx_mid_fid (mid, fid)`；favorite_folder 的主键是 fid。所以数据库层面
//     也不会替你挡住「fid 指向别人的夹子 / 指向已删的夹子」。
//   - model/favorite_item.go：FavoriteItemModel 只有 Add/Del/IsFavored/IsFavoreds 四个方法，
//     **没有任何按 fid 列举收藏项的查询** ⇒ 收藏项一旦落到一个不属于自己（或已删）的 fid 上，
//     就再没有任何接口能把它读出来（本服务连 UserFolders 也只列 favorite_folder）。
//   - internal/repository/repository.go 的 AddFav/DelFav 全程只碰 favItemMd + 两个缓存 key，
//     从不读 favFolder。

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// favOps 是一次「AddFav 落库」的完整调用序列（顺序即结论）：
// 写关系行 → 更新收藏标记 → 失效本人收藏夹列表缓存。**没有第四条**。
func favOps(mid, oid int64, tp int32, fid int64) []string {
	return []string{
		"favItem.Add:" + itoa(mid) + ":" + itoa(oid) + ":" + itoa(int64(tp)) + ":" + itoa(fid),
		"cache.SetIsFavored:" + itoa(mid) + ":" + itoa(oid) + ":" + itoa(int64(tp)) + ":1",
		"cache.DelFolders:" + itoa(mid),
	}
}

// TestAddFavAcceptsNonexistentTarget_KnownGap TODO(缺陷)：AddFav 不校验被收藏的作品是否存在。
//
// 这里用「整个请求的调用轨迹只有三条、一次读都没有」把现状钉死：本服务不依赖任何其他服务的
// RPC，也不读 catalog/video 的投影，所以一个不存在、已删除或版权过期的 oid 照样能收藏成功并
// 计入 favorite_item。若以后接入存在性校验（新增注入点或订阅下架事件），本用例会因轨迹里
// 多出读操作而转红 —— 那时应改成断言新增的「目标不存在/已下架」错误。
// 注意：`model/errors.go` 里目前**没有**任何这类哨兵（既无 ErrVideoGone 也无 ErrVideoNotFound），
// 本服务连「想报这个错」都还没有可用的错误值，这一条要随修复一起补。
//
// 与 AddShare 的对照：分享同样不校验目标存在，所以「互动指向已消失的对象」是本服务的
// 一致取舍，不是收藏独有的漏项；这条取舍必须由 catalog 侧的下架事件或离线清理作业兜住。
func TestAddFavAcceptsNonexistentTarget_KnownGap(t *testing.T) {
	st := newStore()

	got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).
		AddFav(&rpc.AddFavReq{Mid: 7, Oid: 999_999_999, Fid: 33, Tp: 2, Otype: 11})
	wantNoErr(t, "收藏不存在的作品", err)
	if got == nil {
		t.Fatalf("AddFav() 响应 = nil, want 非空 EmptyReply")
	}
	// 关键断言：序列里**只有**写和缓存失效，没有任何前置读（存在性校验必然要读点什么）。
	wantOps(t, "已知缺口 14", st.log.opsFrom(0), favOps(7, 999_999_999, 2, 33))
	wantCount(t, "已知缺口 14", st.log, "favItem.Add:", 1)
	// 收藏项确实落库了，且 state=0（有效收藏）——不是「静默丢弃」。
	row := st.favItem.get(7, 999_999_999, 2)
	if row == nil {
		t.Fatalf("favorite_item 未落库，说明现状不是「无校验直接写入」")
	}
	wantEQ(t, "已知缺口 14", "State（不存在的对象也被记成有效收藏）", row.State, int32(0))
	wantEQ(t, "已知缺口 14", "Oid", row.Oid, int64(999_999_999))
	// IsFavored 随后也会对这个不存在的对象报「已收藏」（读侧同样无存在性口径）。
	faved, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 999_999_999, Tp: 2})
	wantNoErr(t, "回读收藏状态", err)
	wantEQ(t, "已知缺口 14", "已消失对象仍报已收藏", faved.Faved, true)
}

// TestAddFavAcceptsForeignFolder_KnownGap 写侧归属校验缺失：`fid` 只校验 >0，
// 从不比对它属于 `mid`，也不查 favorite_folder 是否存在。
//
// 因此 mid=7 可以把收藏塞进 mid=999 的收藏夹 id 下面。数据库没有外键会拦（见文件头），
// 而 favorite_item 又没有任何「按 fid 列举」的查询，所以这条收藏项从落库那一刻起就是**孤儿**：
// 999 的主页看不到它（列表按 mid 过滤），7 的收藏夹列表里也没有对应夹子可点。
//
// TODO(缺陷)：修复方向是在 AddFav/DelFav 之前校验「fid 属于 mid 且 state=0」（fid=0 的
// 默认夹除外），本服务自己就能做（favorite_folder 就在本域），不需要跨服务 RPC。
func TestAddFavAcceptsForeignFolder_KnownGap(t *testing.T) {
	st := newStore()
	victim := seedFolder(st, 33, 999, "999 的私人夹", 0, model.FolderStateNormal, 4)

	got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).
		AddFav(&rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2, Otype: 11})
	wantNoErr(t, "塞进他人收藏夹", err)
	if got == nil {
		t.Fatalf("AddFav() 响应 = nil")
	}
	wantOps(t, "已知缺口 16", st.log.opsFrom(0), favOps(7, 10001, 2, 33))
	// 归属校验必然要读 favorite_folder：一次都没读 ⇒ 现状确实没校验。
	wantCount(t, "已知缺口 16", st.log, "folder.", 0)

	row := st.favItem.get(7, 10001, 2)
	wantEQ(t, "已知缺口 16", "Fid 原样写入他人的夹子", row.Fid, int64(33))
	wantEQ(t, "已知缺口 16", "Mid 仍是操作者本人", row.Mid, int64(7))
	wantEQ(t, "已知缺口 16", "State", row.State, int32(0))

	// 被塞的人不受影响：夹子的归属、状态与 count 快照都没动（写侧越权只污染自己的行）。
	after := st.folder.get(33)
	wantEQ(t, "已知缺口 16", "他人夹子的 Mid", after.Mid, victim.Mid)
	wantEQ(t, "已知缺口 16", "他人夹子的 State", after.State, victim.State)
	wantEQ(t, "已知缺口 16", "他人夹子的 Count（既没 +1 也没被读）", after.Count, victim.Count)

	// 7 自己的收藏夹列表里没有任何夹子可承接这条收藏 ⇒ 它在所有收藏夹视图里都不可见。
	list, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: 7, Vmid: 7})
	wantNoErr(t, "7 的收藏夹列表", err)
	wantEQ(t, "已知缺口 16", "7 名下收藏夹条数", len(list.Folders), 0)
	wantCount(t, "已知缺口 16", st.log, "favItem.", 1) // 没有任何接口能按 fid 反查收藏项
}

// TestAddFavAcceptsDeletedFolder 同上，但 fid 指向**自己**已软删的收藏夹：
// 夹子已经被 DelFolder 软删（state=1），此后仍可持续把新收藏写进这个 fid，
// 而 UserFolders 会过滤掉软删夹 ⇒ 收藏项存在、标记为已收藏，却在任何列表里都读不到。
// 这条与 README 已知缺口 #7（DelFolder 不级联 favorite_item）是同一处缺陷的两个方向：
// 删夹不管存量条目，新藏又能写进已删夹。
func TestAddFavAcceptsDeletedFolder(t *testing.T) {
	st := newStore()
	seedFolder(st, 44, 7, "已删的夹", 1, model.FolderStateDeleted, 0)

	if _, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 404, Fid: 44, Tp: 2}); err != nil {
		t.Fatalf("前置取消（本无收藏行）：%v", err)
	}
	got, err := NewAddFavLogic(context.Background(), newTestSvc(st)).
		AddFav(&rpc.AddFavReq{Mid: 7, Oid: 10001, Fid: 44, Tp: 2, Otype: 11})
	wantNoErr(t, "收藏进已删夹", err)
	if got == nil {
		t.Fatalf("AddFav() 响应 = nil")
	}

	row := st.favItem.get(7, 10001, 2)
	wantEQ(t, "已删夹仍可写", "Fid 仍指向软删夹", row.Fid, int64(44))
	wantEQ(t, "已删夹仍可写", "State（有效收藏）", row.State, int32(0))
	// 本人收藏夹列表按 state=0 过滤，软删夹不出现 ⇒ 上面那条收藏无家可归。
	list, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: 7, Vmid: 7})
	wantNoErr(t, "本人收藏夹列表", err)
	wantEQ(t, "已删夹仍可写", "列表里的夹子数（软删被过滤）", len(list.Folders), 0)
	// 但读侧布尔标记照样说「已收藏」——两处口径不一致就是这条缺陷的表现。
	faved, err := NewIsFavoredLogic(context.Background(), newTestSvc(st)).
		IsFavored(&rpc.IsFavoredReq{Mid: 7, Oid: 10001, Tp: 2})
	wantNoErr(t, "回读收藏状态", err)
	wantEQ(t, "已删夹仍可写", "IsFavored 仍为 true", faved.Faved, true)
}

// TestDelFavIssuesNoExistenceOrOwnershipCheck 取消收藏的同一处缺口：DelFav 只校验
// mid/oid 为正，既不校验对象存在，也不校验 fid 归属（favorite_item 的行本身按 mid 寻址，
// 所以**取消动作不会越权删别人的行**，这里锁的是这一点，别把它当成归属校验已存在）。
//
// 断言顺序：一条 UPDATE 未命中 → 走不带 fid 的兜底 UPDATE → 仍算成功、仍失效缓存。
// 注意 fid=33 属于 999：因为 WHERE 里有 mid=7，别人的收藏项不会被这次取消波及。
func TestDelFavIssuesNoExistenceOrOwnershipCheck(t *testing.T) {
	st := newStore()
	seedFolder(st, 33, 999, "999 的夹", 1, model.FolderStateNormal, 2)
	// 999 在 33 号夹里对同一个 oid 也有一条收藏，绝不能被 7 的取消波及。
	victimRow := seedFavItem(st, 999, 10001, 33, 2, 11, 0)

	got, err := NewDelFavLogic(context.Background(), newTestSvc(st)).
		DelFav(&rpc.DelFavReq{Mid: 7, Oid: 10001, Fid: 33, Tp: 2})
	wantNoErr(t, "取消不存在的收藏", err)
	if got == nil {
		t.Fatalf("DelFav() 响应 = nil, want 非空 EmptyReply")
	}
	wantOps(t, "取消归属", st.log.opsFrom(0), []string{
		"favItem.Del:7:10001:2:33",
		"cache.SetIsFavored:7:10001:2:0",
		"cache.DelFolders:7",
	})
	wantCount(t, "取消归属", st.log, "folder.", 0)
	if row := st.favItem.get(7, 10001, 2); row != nil {
		t.Errorf("取消归属：不存在的收藏被取消后凭空插入了行 %#v, want 无行", *row)
	}
	// 越权没发生：999 那一行仍是有效收藏（state=0），既没被软删也没被改夹子。
	wantEQ(t, "取消归属", "他人收藏行未被波及（state 仍为 0）", st.favItem.get(999, 10001, 2).State, int32(0))
	wantEQ(t, "取消归属", "他人收藏行 Fid 未被波及", st.favItem.get(999, 10001, 2).Fid, victimRow.Fid)
}
