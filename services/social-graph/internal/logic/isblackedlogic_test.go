package logic

// isblackedlogic_test.go 覆盖 IsBlacked 的 4 类断言：守卫、结果落在哪个字段、
// 方向只查一边（不对称）、软删行不可见、无缓存（每次打库）、失败传播。

import (
	"context"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

func TestIsBlackedGuards(t *testing.T) {
	cases := []struct {
		name       string
		mid, owner int64
		want       error
	}{
		{"mid 为 0", 0, 3002, model.ErrInvalidMid},
		{"mid 为负", -1, 3002, model.ErrInvalidMid},
		{"owner 为 0", 2001, 0, model.ErrInvalidOwnerMid},
		{"owner 为负", 2001, -7, model.ErrInvalidOwnerMid},
		{"两侧同为负数先报 mid", -5, -5, model.ErrInvalidMid},
		{"两侧同为 0 先报 mid", 0, 0, model.ErrInvalidMid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			seedBlack(e.st, distinctBlack(0, 2001, 3002, 1_656_000_100))
			l := NewIsBlackedLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				_, err := l.IsBlacked(&rpc.RelationReq{Mid: c.mid, Owner: c.owner})
				return err
			})
		})
	}
}

// TestIsBlackedHasNoSelfGuard 与 DelBlack 同口径：读侧也没有自发保护，
// 「2001 是否拉黑 2001」会真的打一条 SELECT。
func TestIsBlackedHasNoSelfGuard(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedBlack(st, distinctBlack(0, 2001, 2001, 1_656_000_150))
	l := NewIsBlackedLogic(context.Background(), e.svcCtx)

	got, err := l.IsBlacked(&rpc.RelationReq{Mid: 2001, Owner: 2001})
	wantNoErr(t, "自发拉黑查询", err)
	wantEQ(t, "自发拉黑查询", "following", got.GetFollowing(), true)
	wantOps(t, "自发拉黑查询链路", st.log.ops, []string{"black.FindOne:2001>2001"})
}

// TestIsBlackedAnswersAboutMidBlockingOwner 钉两件事：
// 参数顺序（mid 是拉黑发起方、owner 是被问的人）与结果字段（答复塞进 RelationReply.following）。
func TestIsBlackedAnswersAboutMidBlockingOwner(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedBlack(st, distinctBlack(0, 2001, 3002, 1_656_000_100)) // 2001 拉黑了 3002
	l := NewIsBlackedLogic(context.Background(), e.svcCtx)

	yes, err := l.IsBlacked(&rpc.RelationReq{Mid: 2001, Owner: 3002, RealIp: "10.1.2.3"})
	wantNoErr(t, "已拉黑", err)
	wantEQ(t, "已拉黑", "following 字段承载黑名单答案", yes.GetFollowing(), true)

	// 反过来问「3002 是否拉黑了 2001」必须是 false：本方法只查单边，不做双向对查。
	no, err := l.IsBlacked(&rpc.RelationReq{Mid: 3002, Owner: 2001})
	wantNoErr(t, "未拉黑", err)
	wantEQ(t, "未拉黑", "following", no.GetFollowing(), false)

	// 两次调用各自一条 SELECT，参数按 (mid, owner) 顺序传入，没有任何缓存/计数/关注动作。
	wantOps(t, "黑名单查询链路", st.log.ops, []string{
		"black.FindOne:2001>3002",
		"black.FindOne:3002>2001",
	})
	wantCount(t, "黑名单查询", st.log, "cache.", 0)
	wantCount(t, "黑名单查询", st.log, "follow.", 0)
	wantCount(t, "黑名单查询", st.log, "stat.", 0)
	wantCount(t, "黑名单查询", st.log, "black.Upsert", 0)
	wantCount(t, "黑名单查询", st.log, "black.Delete", 0)
}

// TestIsBlackedIsNotCachedAndRepeatsQuery 说明本方法**每次**都打库：
// 与 IsFollowing（先查 Redis）不同，黑名单没有缓存层，所以也没有脏读窗口。
func TestIsBlackedIsNotCachedAndRepeatsQuery(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedBlack(st, distinctBlack(0, 2001, 3002, 1_656_000_100))
	l := NewIsBlackedLogic(context.Background(), e.svcCtx)

	for i := 1; i <= 3; i++ {
		got, err := l.IsBlacked(&rpc.RelationReq{Mid: 2001, Owner: 3002})
		wantNoErr(t, "重复查询", err)
		wantEQ(t, "重复查询", "following", got.GetFollowing(), true)
	}
	wantCount(t, "重复查询", st.log, "black.FindOne:2001>3002", 3)
	wantOps(t, "重复查询链路", st.log.ops, []string{
		"black.FindOne:2001>3002",
		"black.FindOne:2001>3002",
		"black.FindOne:2001>3002",
	})
}

// TestIsBlackedHidesSoftDeletedRow：取消拉黑后同一对参数立刻翻成 false，
// 且这个翻转与 Follow 守卫读到的是同一份 state 过滤结果。
func TestIsBlackedHidesSoftDeletedRow(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedBlack(st, &model.RelationBlack{
		ID: 555, Mid: 2001, BlackMid: 3002, State: blackGone, Ctime: 1_656_000_100, Mtime: 1_656_000_200,
	})
	l := NewIsBlackedLogic(context.Background(), e.svcCtx)

	got, err := l.IsBlacked(&rpc.RelationReq{Mid: 2001, Owner: 3002})
	wantNoErr(t, "已取消的拉黑", err)
	wantEQ(t, "已取消的拉黑", "following", got.GetFollowing(), false)
	// 行还在库里（软删），只是查询带了 state=0。
	wantEQ(t, "已取消的拉黑", "库存行数", st.blacks.countRows(), 1)

	// 无行同口径：查一个从没拉黑过的人。
	none, err := l.IsBlacked(&rpc.RelationReq{Mid: 2001, Owner: 4321})
	wantNoErr(t, "从未拉黑", err)
	wantEQ(t, "从未拉黑", "following", none.GetFollowing(), false)
	wantOps(t, "黑名单读侧链路", st.log.ops, []string{
		"black.FindOne:2001>3002",
		"black.FindOne:2001>4321",
	})
}

func TestPropagatesIsBlackedFailures(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedBlack(st, distinctBlack(0, 2001, 3002, 1_656_000_100))
	st.blacks.failWith("FindOne", errStore)
	l := NewIsBlackedLogic(context.Background(), e.svcCtx)

	got, err := l.IsBlacked(&rpc.RelationReq{Mid: 2001, Owner: 3002})
	wantErrIs(t, "黑名单查询失败", err, errStore)
	wantErrMessage(t, "黑名单查询失败", err, "relation_black FindOne: social-graph-test: store unavailable")
	wantOps(t, "黑名单查询失败链路", st.log.ops, []string{"black.FindOne:2001>3002"})
	if got != nil {
		t.Fatalf("黑名单查询失败：响应 = %+v, want nil（不得降级成「没拉黑」的假成功）", got)
	}
}
