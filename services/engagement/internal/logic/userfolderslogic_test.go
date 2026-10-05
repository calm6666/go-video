package logic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestUserFoldersRejectsGuardBeforeTouchingDeps 只有一条守卫：vmid<=0。
// 注意 mid 不校验（0/负数都能进 Repository），所以表里单独放一条「mid 非法但 vmid 合法」
// 的用例，锁住它走的是「查他人收藏夹」分支而不是报错。
func TestUserFoldersRejectsGuardBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.UserFoldersReq
		want error
	}{
		{"vmid 为 0", &rpc.UserFoldersReq{Mid: 7, Vmid: 0}, model.ErrInvalidMid},
		{"vmid 为负", &rpc.UserFoldersReq{Mid: 7, Vmid: -7}, model.ErrInvalidMid},
		{"vmid 为 0 且夹子存在", &rpc.UserFoldersReq{Mid: 0, Vmid: 0}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedFolder(st, 1, 7, "日常收藏", 1, model.FolderStateNormal, 3)
			before := st.log.snapshot()
			got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).UserFolders(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestUserFoldersSelfMissReadsDbAndBackfills 本人查询 + 缓存 miss：
// 顺序必须是「读缓存 → 查库 → 回填」，投影逐字段核对，排序按 fid ASC。
func TestUserFoldersSelfMissReadsDbAndBackfills(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	seedFolder(st, 22, likeMid, "私人清单", 0, model.FolderStateNormal, 0)
	// 干扰行：已删除的夹、别人的夹都不许出现。
	seedFolder(st, 33, likeMid, "已删除", 1, model.FolderStateDeleted, 9)
	seedFolder(st, 44, 999, "他人的夹", 1, model.FolderStateNormal, 9)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantNoErr(t, "本人收藏夹", err)
	wantOps(t, "本人收藏夹", st.log.opsFrom(0), []string{
		"cache.GetFolders:7",
		"folder.ListByUser:7:7",
		"cache.SetFolders:7",
	})
	wantEQ(t, "本人收藏夹", "条数（软删与他人被过滤）", len(got.Folders), 2)

	first := got.Folders[0]
	wantEQ(t, "本人收藏夹 11", "Fid", first.Fid, int64(11))
	wantEQ(t, "本人收藏夹 11", "Mid", first.Mid, likeMid)
	wantEQ(t, "本人收藏夹 11", "Name", first.Name, "只看番")
	wantEQ(t, "本人收藏夹 11", "Description", first.Description, "只看番-desc")
	wantEQ(t, "本人收藏夹 11", "Cover", first.Cover, "https://cover/11")
	wantEQ(t, "本人收藏夹 11", "Public", first.Public, int32(1))
	wantEQ(t, "本人收藏夹 11", "State", first.State, int32(model.FolderStateNormal))
	wantEQ(t, "本人收藏夹 11", "Ctime", first.Ctime, int64(1_700_000_200))
	wantEQ(t, "本人收藏夹 11", "Mtime", first.Mtime, int64(1_700_000_200))
	wantEQ(t, "本人收藏夹 11", "Count", first.Count, int32(5))

	second := got.Folders[1]
	wantEQ(t, "本人收藏夹 22", "Fid（按 fid 升序）", second.Fid, int64(22))
	wantEQ(t, "本人收藏夹 22", "Name", second.Name, "私人清单")
	wantEQ(t, "本人收藏夹 22", "Public（私密夹本人可见）", second.Public, int32(0))
	wantEQ(t, "本人收藏夹 22", "Count", second.Count, int32(0))

	// 回填的载荷必须真是刚查出来的那两行，而不是空列表——否则下次命中缓存就永远空。
	payload := st.cache.foldersPayload(likeMid)
	wantEQ(t, "回填载荷", "非空", payload != "", true)
	for _, want := range []string{`"Name":"只看番"`, `"Name":"私人清单"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("回填载荷缺少 %s，实际 %s", want, payload)
		}
	}
	if strings.Contains(payload, "已删除") {
		t.Errorf("回填载荷把软删夹也缓存了：%s", payload)
	}
}

// TestUserFoldersSelfHitSkipsDb 本人查询命中缓存时**一次库都不查**：
// 这是收藏夹列表接口的全部性能意义所在。用「缓存里放一份与库里不同的数据」
// 来证明响应真的来自缓存而不是恰好相同。
func TestUserFoldersSelfHitSkipsDb(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "库里的名字", 1, model.FolderStateNormal, 5)
	st.cache.warmFolders(likeMid, `[{"Fid":11,"Mid":7,"Name":"缓存里的名字","Public":1,"State":0,"Count":2}]`)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantNoErr(t, "命中缓存", err)
	wantOps(t, "命中缓存", st.log.opsFrom(0), []string{"cache.GetFolders:7"})
	wantEQ(t, "命中缓存", "条数", len(got.Folders), 1)
	wantEQ(t, "命中缓存", "Name 来自缓存", got.Folders[0].Name, "缓存里的名字")
	wantEQ(t, "命中缓存", "Count 来自缓存", got.Folders[0].Count, int32(2))
	wantCount(t, "命中缓存", st.log, "folder.ListByUser", 0)
	wantCount(t, "命中缓存", st.log, "cache.SetFolders", 0)
}

// TestUserFoldersEmptySelfListIsNegativeCached 本人一个夹都没有时，回填的是 "null"
// 而不是「不回填」——所以第二次请求仍然不回源。这条锁定负缓存，
// 顺带记下后果：新建夹若未成功失效缓存（缺陷 #5），本人会在 TTL(60s) 内看不到新夹。
func TestUserFoldersEmptySelfListIsNegativeCached(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, 999, "别人的夹", 1, model.FolderStateNormal, 0)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantNoErr(t, "空列表", err)
	wantOps(t, "空列表首次", st.log.opsFrom(0), []string{
		"cache.GetFolders:7",
		"folder.ListByUser:7:7",
		"cache.SetFolders:7",
	})
	wantEQ(t, "空列表", "返回空而不是 nil 响应", got != nil, true)
	wantEQ(t, "空列表", "条数", len(got.Folders), 0)
	wantEQ(t, "空列表", "载荷", st.cache.foldersPayload(likeMid), "null")

	again, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantNoErr(t, "空列表第二次", err)
	wantOps(t, "空列表第二次", st.log.opsFrom(3), []string{"cache.GetFolders:7"})
	wantEQ(t, "空列表第二次", "条数", len(again.Folders), 0)
}

// TestUserFoldersOtherUserReadsDbAndHidesPrivate 查他人收藏夹：
//  1. 完全不走缓存（既读也不写）——否则会把 A 的列表投喂给 B；
//  2. 只返回 public=1 的夹；
//  3. ListByUser 的过滤键是 vmid，mid 只用来决定要不要加公开条件。
func TestUserFoldersOtherUserReadsDbAndHidesPrivate(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, 88, "公开夹", 1, model.FolderStateNormal, 3)
	seedFolder(st, 22, 88, "私密夹", 0, model.FolderStateNormal, 7)
	// 访客自己的夹不得混进别人的主页。
	seedFolder(st, 33, likeMid, "访客自己的夹", 1, model.FolderStateNormal, 1)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: 88})
	wantNoErr(t, "查他人", err)
	wantOps(t, "查他人", st.log.opsFrom(0), []string{"folder.ListByUser:7:88"})
	wantEQ(t, "查他人", "只返回公开夹", len(got.Folders), 1)
	wantEQ(t, "查他人", "Name", got.Folders[0].Name, "公开夹")
	wantEQ(t, "查他人", "Fid", got.Folders[0].Fid, int64(11))
	wantCount(t, "查他人", st.log, "cache.", 0)
}

// TestUserFoldersUnvalidatedMidFallsIntoOtherBranch mid 为 0（未登录/客户端漏传）时
// 不报错，而是按「查他人」口径只返回公开夹。这条锁现状：
// 本人主页在 mid 缺失时会退化成「只剩公开夹」，而不是报参数错。
func TestUserFoldersUnvalidatedMidFallsIntoOtherBranch(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "公开夹", 1, model.FolderStateNormal, 3)
	seedFolder(st, 22, likeMid, "私密夹", 0, model.FolderStateNormal, 7)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: 0, Vmid: likeMid})
	wantNoErr(t, "mid 缺失", err)
	wantOps(t, "mid 缺失", st.log.opsFrom(0), []string{"folder.ListByUser:0:7"})
	wantEQ(t, "mid 缺失", "只剩公开夹", len(got.Folders), 1)
	wantEQ(t, "mid 缺失", "Name", got.Folders[0].Name, "公开夹")
	wantCount(t, "mid 缺失", st.log, "cache.", 0)
}

// TestUserFoldersCorruptedCacheFailsWithoutFallback 缺陷 #14 的现象固化：
// 缓存载荷不是合法 JSON 时直接报错，**不回源查库**。
// 一次写坏（或版本切换后结构不兼容）会让本人的收藏夹接口在 TTL(60s) 内持续 500。
func TestUserFoldersCorruptedCacheFailsWithoutFallback(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	st.cache.warmFolders(likeMid, `{"不是数组"`)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	if err == nil {
		t.Fatalf("脏缓存必须报错，实际返回 %#v", got)
	}
	if got != nil {
		t.Errorf("脏缓存出错时响应体 = %#v, want nil", got)
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Errorf("脏缓存错误 = %v, want 包住的 *json.SyntaxError（说明错误来自反序列化而不是别的环节）", err)
	}
	if !strings.Contains(err.Error(), "UserFolders unmarshal cache") {
		t.Errorf("错误文本 = %q, want 带 UserFolders unmarshal cache 定位前缀", err.Error())
	}
	wantOps(t, "脏缓存", st.log.opsFrom(0), []string{"cache.GetFolders:7"})
	wantCount(t, "脏缓存", st.log, "folder.ListByUser", 0)
}

// TestUserFoldersPropagatesCacheReadFailure GetFolders 的错误是**透出**的（与 SetFolders 相反）：
// 读穿失败直接判定整个请求失败，不降级查库。Redis 抖动 ⇒ 收藏夹接口不可用。
func TestUserFoldersPropagatesCacheReadFailure(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	st.cache.failWith("GetFolders", errBoom)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantFail(t, "GetFolders 失败", got, err, errBoom)
	wantOps(t, "GetFolders 失败后的调用", st.log.opsFrom(0), []string{"cache.GetFolders:7"})
	wantCount(t, "GetFolders 失败", st.log, "folder.ListByUser", 0)
}

// TestUserFoldersPropagatesModelFailure 查库失败必须透出，且不得回填缓存
// （否则一个瞬时 DB 抖动会被缓存成 60s 的空列表）。
func TestUserFoldersPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	st.folder.failWith("ListByUser", errBoom)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantFail(t, "ListByUser 失败", got, err, errBoom)
	wantOps(t, "ListByUser 失败后的调用", st.log.opsFrom(0), []string{
		"cache.GetFolders:7",
		"folder.ListByUser:7:7",
	})
	wantCount(t, "ListByUser 失败", st.log, "cache.SetFolders", 0)
	wantEQ(t, "ListByUser 失败", "缓存仍是空的", st.cache.foldersPayload(likeMid), "")
}

// TestUserFoldersSwallowsBackfillFailure 回填失败被丢弃（`_ =`）：
// 列表仍能返回，只是下一次请求要再查一遍库（缺陷 #5 同源，方向相反）。
func TestUserFoldersSwallowsBackfillFailure(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	st.cache.failWith("SetFolders", errBoom)

	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantNoErr(t, "SetFolders 失败不应影响读", err)
	wantOps(t, "SetFolders 失败", st.log.opsFrom(0), []string{
		"cache.GetFolders:7",
		"folder.ListByUser:7:7",
		"cache.SetFolders:7",
	})
	wantEQ(t, "SetFolders 失败", "条数仍来自库里", len(got.Folders), 1)
	wantEQ(t, "缺陷 #5", "缓存未写入", st.cache.foldersPayload(likeMid), "")
}

// TestUserFoldersOnlyReadsOwnDomain 收藏夹列表只读 favorite_folder + 自己的列表缓存，
// 不得触碰收藏项、点赞、分享，也不得读写 is_favored 标记（AGENTS.md §5 的域内归属）。
func TestUserFoldersOnlyReadsOwnDomain(t *testing.T) {
	st := newStore()
	seedFolder(st, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	seedFavItem(st, likeMid, likeMessage, 11, 2, 11, 0)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
	st.cache.warmIsFavored(likeMid, likeMessage, 2, true)
	favedBefore, hitBefore := st.cache.isFavoredCached(likeMid, likeMessage, 2)

	_, err := NewUserFoldersLogic(context.Background(), newTestSvc(st)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid, Oid: likeMessage, Tp: 2, AllCount: true})
	wantNoErr(t, "UserFolders", err)

	wantCount(t, "越域检查", st.log, "favItem.", 0)
	wantCount(t, "越域检查", st.log, "like.", 0)
	wantCount(t, "越域检查", st.log, "stat.", 0)
	wantCount(t, "越域检查", st.log, "share.", 0)
	wantCount(t, "越域检查", st.log, "cache.GetIsFavored", 0)
	wantCount(t, "越域检查", st.log, "cache.SetIsFavored", 0)
	// 预热值必须原样还在：既没被读（上面已断言）也没被改/删。
	wantEQ(t, "越域检查", "is_favored 标记未被改动（值）", st.cache.isFav[addrIsFavored(likeMid, likeMessage, 2)] == "1", favedBefore)
	wantEQ(t, "越域检查", "is_favored 标记未被改动（存在）", hitBefore, true)
}

// TestUserFoldersIgnoresTpOidAllCount 缺陷 #12：请求里的 tp / oid / all_count
// 三个字段从头到尾没被读过（注释承诺的「收藏到哪个夹」提示与全部分类计数无实现），
// 所以返回的收藏夹项与不带这些字段时完全一致。
func TestUserFoldersIgnoresTpOidAllCount(t *testing.T) {
	baseline := newStore()
	seedFolder(baseline, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	seedFavItem(baseline, likeMid, likeMessage, 11, 2, 11, 0)
	base, err := NewUserFoldersLogic(context.Background(), newTestSvc(baseline)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid})
	wantNoErr(t, "基线", err)

	withFields := newStore()
	seedFolder(withFields, 11, likeMid, "只看番", 1, model.FolderStateNormal, 5)
	seedFavItem(withFields, likeMid, likeMessage, 11, 2, 11, 0)
	got, err := NewUserFoldersLogic(context.Background(), newTestSvc(withFields)).
		UserFolders(&rpc.UserFoldersReq{Mid: likeMid, Vmid: likeMid, Tp: 2, Oid: likeMessage, AllCount: true, Otype: 11})
	wantNoErr(t, "带 tp/oid/all_count", err)

	wantOps(t, "缺陷 #12（调用序列与基线一致）", withFields.log.opsFrom(0), baseline.log.opsFrom(0))
	if !equalFolderReplies(got, base) {
		t.Errorf("带 tp/oid/all_count 的响应与基线不同：\n got=%v\nbase=%v", got.Folders, base.Folders)
	}
	// 基线本身必须有内容，否则上面那句「与基线一致」会因为两边都空而恒真。
	wantEQ(t, "缺陷 #12", "基线条数", len(base.Folders), 1)
	wantEQ(t, "缺陷 #12", "Count 只是库里的快照位（与分类无关）", got.Folders[0].Count, int32(5))
}

// equalFolderReplies 比较两个收藏夹列表响应的投影字段。
func equalFolderReplies(a, b *rpc.UserFoldersReply) bool {
	if len(a.GetFolders()) != len(b.GetFolders()) {
		return false
	}
	for i := range a.GetFolders() {
		x, y := a.GetFolders()[i], b.GetFolders()[i]
		if x.Fid != y.Fid || x.Mid != y.Mid || x.Name != y.Name || x.Description != y.Description ||
			x.Cover != y.Cover || x.Public != y.Public || x.State != y.State ||
			x.Ctime != y.Ctime || x.Mtime != y.Mtime || x.Count != y.Count {
			return false
		}
	}
	return true
}
