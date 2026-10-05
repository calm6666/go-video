package logic

// listfollowerlogic_test.go 覆盖 ListFollower：守卫、粉丝行投影（取的是 mid 列＝关注发起方）、
// 排序与分页、软删不可见、失败传播，外加一条「following / follower 方向没写反」的交叉对账。

import (
	"context"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// 粉丝侧布景：2001、3003、3004 都关注了 3002（fanCtime 互不相同），3005 已取关。
const (
	fanA = 2001
	fanB = 3003
	fanC = 3004
	// 被关注的一方
	owned = 3002
)

const (
	fanActime   = 1_652_000_100
	fanBctime   = 1_652_000_200
	fanCctime   = 1_652_000_300
	goneCtime   = 1_652_000_400
	ownFollowed = 1_652_000_500
)

func listFollowerSeed(st *store) {
	seedFollow(st, distinctFollow(0, fanA, owned, fanActime))
	seedFollow(st, distinctFollow(0, fanB, owned, fanBctime))
	seedFollow(st, distinctFollow(0, fanC, owned, fanCctime))
	gone := distinctFollow(0, 3005, owned, goneCtime)
	gone.State = followGone
	seedFollow(st, gone)
	// 3002 自己关注别人，不得混进它的粉丝列表。
	seedFollow(st, distinctFollow(0, owned, 4321, ownFollowed))
}

func TestListFollowerGuards(t *testing.T) {
	cases := []struct {
		name   string
		mid    int64
		pn, ps int32
		want   error
	}{
		{"mid 为 0", 0, 1, 20, model.ErrInvalidMid},
		{"mid 为负", -1, 1, 20, model.ErrInvalidMid},
		{"ps 为 51", 3002, 1, 51, model.ErrPsTooLarge},
		{"ps 为 9999", 3002, 1, 9999, model.ErrPsTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			listFollowerSeed(st)
			l := NewListFollowerLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, st, c.name, c.want, func() error {
				_, err := l.ListFollower(&rpc.ListReq{Mid: c.mid, Pn: c.pn, Ps: c.ps})
				return err
			})
		})
	}
}

func TestListFollowerProjectsFanMidNotFollowedMid(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listFollowerSeed(st)
	l := NewListFollowerLogic(context.Background(), e.svcCtx)

	got, err := l.ListFollower(&rpc.ListReq{Mid: owned, Pn: 1, Ps: 10})
	wantNoErr(t, "粉丝列表", err)
	wantEQ(t, "粉丝列表", "total", got.GetTotal(), int32(3))
	items := got.GetItems()
	// 顺序 ctime DESC：fanC(300) > fanB(200) > fanA(100)；3005 已取关（虽然 ctime 最大）不出现。
	wantInt64sEQ(t, "粉丝列表", "mid 列（关注发起方＝粉丝）", itemMids(items), []int64{fanC, fanB, fanA})
	wantInt64sEQ(t, "粉丝列表", "ctime 列", itemCtimes(items), []int64{fanCctime, fanBctime, fanActime})
	wantInt32sEQ(t, "粉丝列表", "attr 列", itemAttrs(items), []int32{2, 2, 2})
	for i, wantFan := range []int64{fanC, fanB, fanA} {
		row := st.follows.row(wantFan, owned)
		wantEQ(t, "粉丝列表逐条", "mid 取的是 row.mid", items[i].GetMid(), row.Mid)
		wantEQ(t, "粉丝列表逐条", "ctime", items[i].GetCtime(), row.Ctime)
		wantEQ(t, "粉丝列表逐条", "attr＝FOLLOWER", items[i].GetAttr(), int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWER))
	}
	wantOps(t, "粉丝列表链路", st.log.ops, []string{
		"follow.ListFollowers:count:3002/1/10",
		"follow.ListFollowers:rows:3002/1/10",
	})
	wantCount(t, "粉丝列表", st.log, "cache.", 0)
	wantCount(t, "粉丝列表", st.log, "stat.", 0)
}

// TestFollowingAndFollowerDirectionsAreNotSwapped 是本服务最容易写反的一处：
// relation_follow 里 mid 是**关注发起方**、follower_mid 是**被关注者**（见迁移 SQL 注释），
// 所以「我关注谁」查 mid 列、「谁关注我」查 follower_mid 列。布一行 2001→3002 做四向对账。
func TestFollowingAndFollowerDirectionsAreNotSwapped(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedFollow(st, distinctFollow(0, 2001, 3002, 1_653_000_000))
	followingL := NewListFollowingLogic(context.Background(), e.svcCtx)
	followerL := NewListFollowerLogic(context.Background(), e.svcCtx)
	req := &rpc.ListReq{Mid: 2001, Pn: 1, Ps: 20}

	// 2001 的关注列表 = {3002}；2001 的粉丝列表 = 空。
	gf, err := followingL.ListFollowing(req)
	wantNoErr(t, "2001 关注列表", err)
	wantInt64sEQ(t, "2001 关注列表", "mid 列", itemMids(gf.GetItems()), []int64{3002})
	sf, err := followerL.ListFollower(req)
	wantNoErr(t, "2001 粉丝列表", err)
	wantEQ(t, "2001 粉丝列表", "total", sf.GetTotal(), int32(0))
	wantEQ(t, "2001 粉丝列表", "条目数", len(sf.GetItems()), 0)

	// 3002 一侧正好相反：关注列表空、粉丝列表 = {2001}。
	req3002 := &rpc.ListReq{Mid: 3002, Pn: 1, Ps: 20}
	g3002, err := followingL.ListFollowing(req3002)
	wantNoErr(t, "3002 关注列表", err)
	wantEQ(t, "3002 关注列表", "total", g3002.GetTotal(), int32(0))
	s3002, err := followerL.ListFollower(req3002)
	wantNoErr(t, "3002 粉丝列表", err)
	wantInt64sEQ(t, "3002 粉丝列表", "mid 列", itemMids(s3002.GetItems()), []int64{2001})

	// 两条读径走的索引不同（idx_mid_state_ctime / idx_follower_state_ctime），轨迹可区分。
	// total==0 时 model 不再发第二条 SELECT（fakes_test.go 的 pageRelations 复刻了这个短路），
	// 所以只有「命中那一侧」才留下 rows 轨迹：ListFollowings 只查 2001、ListFollowers 只查 3002。
	wantStringsEQ(t, "方向对账", "两个方法各查了哪张切片",
		append(st.log.opsWith("ListFollowings"), st.log.opsWith("ListFollowers")...), []string{
			"follow.ListFollowings:count:2001/1/20",
			"follow.ListFollowings:rows:2001/1/20",
			"follow.ListFollowings:count:3002/1/20",
			"follow.ListFollowers:count:2001/1/20",
			"follow.ListFollowers:count:3002/1/20",
			"follow.ListFollowers:rows:3002/1/20",
		})
}

func TestListFollowerClampsAndPages(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listFollowerSeed(st)
	l := NewListFollowerLogic(context.Background(), e.svcCtx)

	got, err := l.ListFollower(&rpc.ListReq{Mid: owned, Pn: 0, Ps: 0})
	wantNoErr(t, "粉丝列表钳制", err)
	wantEQ(t, "粉丝列表钳制", "条目数", len(got.GetItems()), 3)
	wantOps(t, "粉丝列表钳制链路", st.log.ops, []string{
		"follow.ListFollowers:count:3002/0/0",
		"follow.ListFollowers:rows:3002/1/20",
	})

	before := st.log.snapshot()
	p2, err := l.ListFollower(&rpc.ListReq{Mid: owned, Pn: 2, Ps: 2})
	wantNoErr(t, "粉丝列表第二页", err)
	wantInt64sEQ(t, "粉丝列表第二页", "mid 列", itemMids(p2.GetItems()), []int64{fanA})
	wantEQ(t, "粉丝列表第二页", "total", p2.GetTotal(), int32(3))
	wantOps(t, "粉丝列表第二页链路", st.log.opsFrom(before), []string{
		"follow.ListFollowers:count:3002/2/2",
		"follow.ListFollowers:rows:3002/2/2",
	})
}

func TestPropagatesListFollowerFailures(t *testing.T) {
	t.Run("COUNT 失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		listFollowerSeed(st)
		st.follows.failWith("ListFollowersCount", errStore)
		l := NewListFollowerLogic(context.Background(), e.svcCtx)

		got, err := l.ListFollower(&rpc.ListReq{Mid: owned, Pn: 1, Ps: 20})
		wantErrIs(t, "粉丝 COUNT 失败", err, errStore)
		wantErrMessage(t, "粉丝 COUNT 失败", err, "relation_follow ListFollowers count: social-graph-test: store unavailable")
		wantOps(t, "粉丝 COUNT 失败链路", st.log.ops, []string{"follow.ListFollowers:count:3002/1/20"})
		if got != nil {
			t.Fatalf("粉丝 COUNT 失败：响应 = %+v, want nil", got)
		}
	})
	t.Run("列表失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		listFollowerSeed(st)
		st.follows.failWith("ListFollowersRows", errStore)
		l := NewListFollowerLogic(context.Background(), e.svcCtx)

		_, err := l.ListFollower(&rpc.ListReq{Mid: owned, Pn: 1, Ps: 20})
		wantErrIs(t, "粉丝列表失败", err, errStore)
		wantErrMessage(t, "粉丝列表失败", err, "relation_follow ListFollowers list: social-graph-test: store unavailable")
		wantCount(t, "粉丝列表失败", st.log, "cache.", 0)
	})
}
