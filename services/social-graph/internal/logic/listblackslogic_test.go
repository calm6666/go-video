package logic

// listblackslogic_test.go 覆盖 ListBlacks：守卫、投影取的是 black_mid 列（不是发起方 mid）、
// ctime DESC 排序、分页钳制与无重叠、软删/他人行不可见、黑名单与关注表互不串台、失败传播。

import (
	"context"
	"slices"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// 布景：2001 拉黑了 3002/3003/3004（ctime 互不相同），另有一条已取消拉黑的 3005（ctime 最大）、
// 一条「4321 拉黑了 2001」的反向行，以及一条 2001 的关注行（用来证明两张表不串台）。
const (
	blOwner = 2001
	blA     = 3002
	blB     = 3003
	blC     = 3004
	blGone  = 3005
	blOther = 4321
)

const (
	blActime    = 1_654_000_100
	blBctime    = 1_654_000_200
	blCctime    = 1_654_000_300
	blGoneCtime = 1_654_000_400 // 已取消的那行 ctime 最大：没被 state 过滤就会挤进第一页
	blRevCtime  = 1_654_000_900 // 反向行「blOther 拉黑 blOwner」
	blFollowCtm = 1_654_000_050 // 挂在关注表里的行
)

func listBlacksSeed(st *store) {
	seedBlack(st, distinctBlack(0, blOwner, blA, blActime))
	seedBlack(st, distinctBlack(0, blOwner, blB, blBctime))
	seedBlack(st, distinctBlack(0, blOwner, blC, blCctime))
	gone := distinctBlack(0, blOwner, blGone, blGoneCtime)
	gone.State = blackGone
	seedBlack(st, gone)
	seedBlack(st, distinctBlack(0, blOther, blOwner, blRevCtime))
	seedFollow(st, distinctFollow(0, blOwner, blA, blFollowCtm))
}

func TestListBlacksGuards(t *testing.T) {
	cases := []struct {
		name   string
		mid    int64
		pn, ps int32
		want   error
	}{
		{"mid 为 0", 0, 1, 20, model.ErrInvalidMid},
		{"mid 为负", -1, 1, 20, model.ErrInvalidMid},
		{"ps 为 51 超上界", blOwner, 1, 51, model.ErrPsTooLarge},
		{"ps 为 9999 超上界", blOwner, 1, 9999, model.ErrPsTooLarge},
		{"ps 为负数不超上界（放行）", blOwner, 1, -1, nil},
		{"pn 为负数不设守卫（放行）", blOwner, -3, 20, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			listBlacksSeed(st)
			l := NewListBlacksLogic(context.Background(), e.svcCtx)
			if c.want != nil {
				wantGuardRejected(t, st, c.name, c.want, func() error {
					_, err := l.ListBlacks(&rpc.ListReq{Mid: c.mid, Pn: c.pn, Ps: c.ps})
					return err
				})
				return
			}
			// 放行的一档证明守卫只挡上界（与 ListFollowing/ListFollower 同一边倒口径）。
			before := st.log.snapshot()
			got, err := l.ListBlacks(&rpc.ListReq{Mid: c.mid, Pn: c.pn, Ps: c.ps})
			wantNoErr(t, c.name, err)
			if len(st.log.opsFrom(before)) == 0 {
				t.Fatalf("%s：放行后一次依赖调用都没发生，说明守卫位置不对", c.name)
			}
			wantEQ(t, c.name, "total", got.GetTotal(), int32(3))
		})
	}
}

// TestListBlacksProjectsBlackMidNotOwnerMid 钉住最容易写反的一列：
// 列表项的 mid 必须是**被拉黑者**（r.BlackMid），不是发起方；若误取 r.Mid，三条会全变成 2001。
func TestListBlacksProjectsBlackMidNotOwnerMid(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listBlacksSeed(st)
	l := NewListBlacksLogic(context.Background(), e.svcCtx)

	got, err := l.ListBlacks(&rpc.ListReq{Mid: blOwner, Pn: 1, Ps: 10})
	wantNoErr(t, "黑名单列表", err)
	wantEQ(t, "黑名单列表", "total", got.GetTotal(), int32(3))
	items := got.GetItems()
	wantEQ(t, "黑名单列表", "条目数", len(items), 3)
	// ctime DESC：blC(300) > blB(200) > blA(100)；blGone 的 ctime 最大但已取消，不出现。
	wantInt64sEQ(t, "黑名单列表", "black_mid 列", itemMids(items), []int64{blC, blB, blA})
	wantInt64sEQ(t, "黑名单列表", "ctime 列", itemCtimes(items), []int64{blCctime, blBctime, blActime})
	wantInt32sEQ(t, "黑名单列表", "attr 列", itemAttrs(items),
		[]int32{int32(rpc.RelationAttr_RELATION_ATTR_BLACKED), int32(rpc.RelationAttr_RELATION_ATTR_BLACKED), int32(rpc.RelationAttr_RELATION_ATTR_BLACKED)})
	for i, wantBlack := range []int64{blC, blB, blA} {
		row := st.blacks.row(blOwner, wantBlack)
		wantEQ(t, "黑名单列表逐条", "black_mid", items[i].GetMid(), row.BlackMid)
		wantEQ(t, "黑名单列表逐条", "ctime", items[i].GetCtime(), row.Ctime)
		if items[i].GetMid() == blOwner {
			t.Fatalf("黑名单列表逐条：第 %d 项把发起方 mid 当成了列表项", i+1)
		}
	}
	wantOps(t, "黑名单列表链路", st.log.ops, []string{
		"black.ListByMid:count:2001/1/10",
		"black.ListByMid:rows:2001/1/10",
	})
	// 读列表不刷计数、不碰缓存（AGENTS.md §5），也不串到关注/特别关注表上。
	wantCount(t, "黑名单列表", st.log, "cache.", 0)
	wantCount(t, "黑名单列表", st.log, "stat.", 0)
	wantCount(t, "黑名单列表", st.log, "follow.", 0)
	wantCount(t, "黑名单列表", st.log, "special.", 0)
}

func TestListBlacksHidesSoftDeletedAndReverseRows(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listBlacksSeed(st)
	l := NewListBlacksLogic(context.Background(), e.svcCtx)

	got, err := l.ListBlacks(&rpc.ListReq{Mid: blOwner, Pn: 1, Ps: 50})
	wantNoErr(t, "黑名单过滤", err)
	mids := itemMids(got.GetItems())
	if slices.Contains(mids, blGone) {
		t.Errorf("黑名单过滤：已取消拉黑的 %d 仍在列表里 %v", blGone, mids)
	}
	if slices.Contains(mids, blOwner) {
		t.Errorf("黑名单过滤：列表里出现了发起方自己 %v", mids)
	}
	// 反向行「4321 拉黑 2001」不是 2001 的黑名单；查 4321 才看得到。
	rev, err := l.ListBlacks(&rpc.ListReq{Mid: blOther, Pn: 1, Ps: 50})
	wantNoErr(t, "反向黑名单", err)
	wantInt64sEQ(t, "反向黑名单", "black_mid 列", itemMids(rev.GetItems()), []int64{blOwner})
	wantEQ(t, "反向黑名单", "total", rev.GetTotal(), int32(1))
	// 库存了 5 行（3 条本人拉黑别人 + 1 条已取消 + 1 条别人拉黑本人），列表只给 3 条 ⇒ 过滤在 SQL 里做。
	wantEQ(t, "黑名单过滤", "库存行数", st.blacks.countRows(), 5)
	wantEQ(t, "黑名单过滤", "返回条数", len(got.GetItems()), 3)
}

func TestListBlacksClampsAndPagesWithoutOverlap(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listBlacksSeed(st)
	l := NewListBlacksLogic(context.Background(), e.svcCtx)

	// count 带原始 pn/ps（logic 不钳制），rows 带钳制后的值（钳制在 model）。
	got, err := l.ListBlacks(&rpc.ListReq{Mid: blOwner, Pn: 0, Ps: 0})
	wantNoErr(t, "黑名单钳制", err)
	wantInt64sEQ(t, "黑名单钳制", "black_mid 列", itemMids(got.GetItems()), []int64{blC, blB, blA})
	wantOps(t, "黑名单钳制链路", st.log.ops, []string{
		"black.ListByMid:count:2001/0/0",
		"black.ListByMid:rows:2001/1/20",
	})

	// 每页 2 条：第一页 {blC,blB}、第二页 {blA}，两页无重叠且并集等于全集。
	before := st.log.snapshot()
	p1, err := l.ListBlacks(&rpc.ListReq{Mid: blOwner, Pn: 1, Ps: 2})
	wantNoErr(t, "黑名单第一页", err)
	wantInt64sEQ(t, "黑名单第一页", "black_mid 列", itemMids(p1.GetItems()), []int64{blC, blB})
	p2, err := l.ListBlacks(&rpc.ListReq{Mid: blOwner, Pn: 2, Ps: 2})
	wantNoErr(t, "黑名单第二页", err)
	wantInt64sEQ(t, "黑名单第二页", "black_mid 列", itemMids(p2.GetItems()), []int64{blA})
	for _, p := range [][]int64{itemMids(p1.GetItems()), itemMids(p2.GetItems())} {
		wantEQ(t, "黑名单分页无重叠", "页内去重后条数", len(dedup(p)), len(p))
	}
	var inter []int64
	for _, m := range itemMids(p2.GetItems()) {
		if slices.Contains(itemMids(p1.GetItems()), m) {
			inter = append(inter, m)
		}
	}
	if len(inter) != 0 {
		t.Errorf("黑名单分页重叠 = %v, want 空", inter)
	}
	union := dedup(append(append([]int64{}, itemMids(p1.GetItems())...), itemMids(p2.GetItems())...))
	slices.Sort(union)
	wantInt64sEQ(t, "黑名单分页并集", "两页合并升序 = 全集", union, []int64{blA, blB, blC})
	wantOps(t, "黑名单翻页链路", st.log.opsFrom(before), []string{
		"black.ListByMid:count:2001/1/2",
		"black.ListByMid:rows:2001/1/2",
		"black.ListByMid:count:2001/2/2",
		"black.ListByMid:rows:2001/2/2",
	})
}

// TestListBlacksEmptyResultSendsOnlyCountQuery 钉住「total==0 时不发第二条 SELECT」，
// 同时证明黑名单为空的人不会被误报成「有黑名单」。
func TestListBlacksEmptyResultSendsOnlyCountQuery(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listBlacksSeed(st)
	l := NewListBlacksLogic(context.Background(), e.svcCtx)

	got, err := l.ListBlacks(&rpc.ListReq{Mid: 5555, Pn: 1, Ps: 20})
	wantNoErr(t, "空黑名单", err)
	wantEQ(t, "空黑名单", "total", got.GetTotal(), int32(0))
	wantEQ(t, "空黑名单", "条目数", len(got.GetItems()), 0)
	wantOps(t, "空黑名单链路", st.log.ops, []string{"black.ListByMid:count:5555/1/20"})
}

func TestPropagatesListBlacksFailures(t *testing.T) {
	t.Run("COUNT 失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		listBlacksSeed(st)
		st.blacks.failWith("ListByMidCount", errStore)
		l := NewListBlacksLogic(context.Background(), e.svcCtx)

		got, err := l.ListBlacks(&rpc.ListReq{Mid: blOwner, Pn: 1, Ps: 20})
		wantErrIs(t, "黑名单 COUNT 失败", err, errStore)
		wantErrMessage(t, "黑名单 COUNT 失败", err, "relation_black ListByMid count: social-graph-test: store unavailable")
		wantOps(t, "黑名单 COUNT 失败链路", st.log.ops, []string{"black.ListByMid:count:2001/1/20"})
		if got != nil {
			t.Fatalf("黑名单 COUNT 失败：响应 = %+v, want nil", got)
		}
	})
	t.Run("列表失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		listBlacksSeed(st)
		st.blacks.failWith("ListByMidRows", errStore)
		l := NewListBlacksLogic(context.Background(), e.svcCtx)

		got, err := l.ListBlacks(&rpc.ListReq{Mid: blOwner, Pn: 1, Ps: 20})
		wantErrIs(t, "黑名单列表失败", err, errStore)
		wantErrMessage(t, "黑名单列表失败", err, "relation_black ListByMid list: social-graph-test: store unavailable")
		wantOps(t, "黑名单列表失败链路", st.log.ops, []string{
			"black.ListByMid:count:2001/1/20",
			"black.ListByMid:rows:2001/1/20",
		})
		if got != nil {
			t.Fatalf("黑名单列表失败：响应 = %+v, want nil（不得吞掉错误后返回半截列表）", got)
		}
	})
}
