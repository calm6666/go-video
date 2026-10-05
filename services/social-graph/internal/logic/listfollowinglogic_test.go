package logic

// listfollowinglogic_test.go 覆盖 ListFollowing：守卫（含「只挡上界不挡下界」的一边倒钳制）、
// 三列逐字段投影、排序口径（ctime DESC 而非 mtime）、分页无重叠、软删/他人行不可见、
// 失败传播，以及「列表读不刷计数」的 AGENTS.md §5 口径。

import (
	"context"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// 2001 关注了 3002/3003/3004（ctime 互不相同，且 mtime 与 ctime 错开），
// 另有一条已取关的 3005（ctime 最大）与一条别人的 3999→8888。
const (
	f1Ctime = 1_650_000_100 // 2001 → 3002
	f2Ctime = 1_650_000_200 // 2001 → 3003
	f3Ctime = 1_650_000_300 // 2001 → 3004
	f4Ctime = 1_650_000_400 // 2001 → 3005（已取关，ctime 最新）
)

func listFollowingSeed(st *store) {
	seedFollow(st, distinctFollow(0, 2001, 3002, f1Ctime))
	seedFollow(st, distinctFollow(0, 2001, 3003, f2Ctime))
	seedFollow(st, distinctFollow(0, 2001, 3004, f3Ctime))
	gone := distinctFollow(0, 2001, 3005, f4Ctime)
	gone.State = followGone
	seedFollow(st, gone)
	seedFollow(st, distinctFollow(0, 3999, 8888, 1_650_000_900))
}

func TestListFollowingGuards(t *testing.T) {
	cases := []struct {
		name   string
		mid    int64
		pn, ps int32
		want   error
	}{
		{"mid 为 0", 0, 1, 20, model.ErrInvalidMid},
		{"mid 为负", -1, 1, 20, model.ErrInvalidMid},
		{"ps 为 51 超上界", 2001, 1, 51, model.ErrPsTooLarge},
		{"ps 为 100 超上界", 2001, 1, 100, model.ErrPsTooLarge},
		{"ps 为负数不超上界（放行）", 2001, 1, -1, nil},
		{"pn 为负数不设守卫（放行）", 2001, -3, 20, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			listFollowingSeed(st)
			l := NewListFollowingLogic(context.Background(), e.svcCtx)
			if c.want != nil {
				wantGuardRejected(t, st, c.name, c.want, func() error {
					_, err := l.ListFollowing(&rpc.ListReq{Mid: c.mid, Pn: c.pn, Ps: c.ps})
					return err
				})
				return
			}
			// 放行的两个例子证明守卫是**一边倒**的：只挡 ps>50，不管 ps<=0 与 pn<=0，
			// 越界值原样透传给 model 钳制（见 TestListFollowingClampsPaginationInModel）。
			before := st.log.snapshot()
			got, err := l.ListFollowing(&rpc.ListReq{Mid: c.mid, Pn: c.pn, Ps: c.ps})
			wantNoErr(t, c.name, err)
			if len(st.log.opsFrom(before)) == 0 {
				t.Fatalf("%s：放行后一次依赖调用都没发生，说明守卫位置不对", c.name)
			}
			wantEQ(t, c.name, "total", got.GetTotal(), int32(3))
		})
	}
}

func TestListFollowingProjectsEachRowFieldByField(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listFollowingSeed(st)
	l := NewListFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: 1, Ps: 10})
	wantNoErr(t, "关注列表", err)
	wantEQ(t, "关注列表", "total", got.GetTotal(), int32(3))
	items := got.GetItems()
	wantEQ(t, "关注列表", "条目数", len(items), 3)

	// 排序口径 = ctime DESC（不是 mtime）：三条行的 mtime 都是 ctime+11，
	// 若实现按 mtime 排会得到同一条序列，所以这里同时钉住「取的是 ctime 列」。
	wantInt64sEQ(t, "关注列表", "mid 列（被关注者）", itemMids(items), []int64{3004, 3003, 3002})
	wantInt64sEQ(t, "关注列表", "ctime 列", itemCtimes(items), []int64{f3Ctime, f2Ctime, f1Ctime})
	wantInt32sEQ(t, "关注列表", "attr 列", itemAttrs(items), []int32{
		int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING),
		int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING),
		int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING),
	})
	for i, wantMid := range []int64{3004, 3003, 3002} {
		row := st.follows.row(2001, wantMid)
		wantEQ(t, "关注列表逐条", "mid", items[i].GetMid(), row.FollowerMid)
		wantEQ(t, "关注列表逐条", "ctime", items[i].GetCtime(), row.Ctime)
		wantEQ(t, "关注列表逐条", "attr", items[i].GetAttr(), int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING))
	}
	wantOps(t, "关注列表链路", st.log.ops, []string{
		"follow.ListFollowings:count:2001/1/10",
		"follow.ListFollowings:rows:2001/1/10",
	})
	// 读列表不得顺手刷计数/推荐（AGENTS.md §5）。
	wantCount(t, "关注列表", st.log, "cache.", 0)
	wantCount(t, "关注列表", st.log, "stat.", 0)
}

func TestListFollowingClampsPaginationInModel(t *testing.T) {
	cases := []struct {
		name       string
		pn, ps     int32
		wantCountK string
		wantRowsK  string
	}{
		{"pn=0 ps=0 归一为 1/20", 0, 0, "follow.ListFollowings:count:2001/0/0", "follow.ListFollowings:rows:2001/1/20"},
		{"pn 负数归一为 1", -3, 5, "follow.ListFollowings:count:2001/-3/5", "follow.ListFollowings:rows:2001/1/5"},
		{"ps 负数归一为 20", 1, -1, "follow.ListFollowings:count:2001/1/-1", "follow.ListFollowings:rows:2001/1/20"},
		{"ps=50 正好在上界内", 1, 50, "follow.ListFollowings:count:2001/1/50", "follow.ListFollowings:rows:2001/1/50"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			listFollowingSeed(st)
			l := NewListFollowingLogic(context.Background(), e.svcCtx)

			got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: c.pn, Ps: c.ps})
			wantNoErr(t, c.name, err)
			wantEQ(t, c.name, "条目数", len(got.GetItems()), 3)
			// count 语句带**原始** pn/ps（logic 没钳），rows 带钳制后的值（钳制在 model）。
			wantOps(t, c.name+"链路", st.log.ops, []string{c.wantCountK, c.wantRowsK})
		})
	}
}

func TestListFollowingPagesDoNotOverlapAndUnionEqualsAll(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listFollowingSeed(st)
	l := NewListFollowingLogic(context.Background(), e.svcCtx)

	var all []int64
	for pn := int32(1); pn <= 3; pn++ {
		got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: pn, Ps: 2})
		wantNoErr(t, "翻页", err)
		wantEQ(t, "翻页", "total 恒为全集大小", got.GetTotal(), int32(3))
		all = append(all, itemMids(got.GetItems())...)
	}
	wantInt64sEQ(t, "翻页", "第 1 页 2 条 + 第 2 页 1 条", all, []int64{3004, 3003, 3002})
	wantEQ(t, "翻页", "无重叠", len(all), len(dedup(all)))

	// 越界页：total 仍报 3，items 为空（不是报错，也不是回落到第一页）。
	got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: 4, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "total", got.GetTotal(), int32(3))
	wantEQ(t, "越界页", "条目数", len(got.GetItems()), 0)
	wantOps(t, "越界页链路", st.log.ops[6:], []string{
		"follow.ListFollowings:count:2001/4/2",
		"follow.ListFollowings:rows:2001/4/2",
	})
}

func TestListFollowingEmptyResultSendsOnlyCountQuery(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listFollowingSeed(st)
	l := NewListFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.ListFollowing(&rpc.ListReq{Mid: 7777, Pn: 1, Ps: 20})
	wantNoErr(t, "空列表", err)
	wantEQ(t, "空列表", "total", got.GetTotal(), int32(0))
	wantEQ(t, "空列表", "条目数", len(got.GetItems()), 0)
	wantOps(t, "空列表链路", st.log.ops, []string{"follow.ListFollowings:count:7777/1/20"})
}

// TestListFollowingHidesSoftDeletedAndForeignRows 已取关的行（哪怕 ctime 最新）与他人行都不得出现。
func TestListFollowingHidesSoftDeletedAndForeignRows(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listFollowingSeed(st)
	l := NewListFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: 1, Ps: 50})
	wantNoErr(t, "可见性", err)
	wantInt64sEQ(t, "可见性", "mid 列", itemMids(got.GetItems()), []int64{3004, 3003, 3002})
	for _, mid := range itemMids(got.GetItems()) {
		wantEQ(t, "可见性", "3005 已取关不出现", mid == int64(3005), false)
		wantEQ(t, "可见性", "8888 属于他人不出现", mid == int64(8888), false)
	}
}

// TODO(缺陷): 列表项的 attr 是按接口硬编码的常量，不是关系事实：
// 特别关注（relation_special）与互关（RELATION_ATTR_MUTUAL）都不会体现在关注列表里，
// relation_follow.attr 这一列也从来没被写过（Follow 传的是零值）。
// 定位：internal/logic/listfollowinglogic.go 的 attr 常量 + Repository.Follow 构造 rec 时漏 Attr。
func TestListFollowingAttrIsHardcodedIgnoresSpecialAndMutual(t *testing.T) {
	e := newEnv(t)
	st := e.st
	listFollowingSeed(st)
	// 3004 既被 2001 特别关注，又反向关注了 2001（事实上的互关）。
	seedSpecial(st, distinctSpecial(0, 2001, 3004, 1_660_000_000))
	mutual := distinctFollow(0, 3004, 2001, 1_650_000_350)
	seedFollow(st, mutual)
	// 库里 attr 列有值也不该被读出来（当前实现根本没这列）。
	st.follows.put(&model.RelationFollow{Mid: 2001, FollowerMid: 3003, Attr: 7, State: followNormal, Ctime: f2Ctime, Mtime: f2Ctime + 11})
	l := NewListFollowingLogic(context.Background(), e.svcCtx)

	got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: 1, Ps: 20})
	wantNoErr(t, "attr 硬编码", err)
	items := got.GetItems()
	wantInt64sEQ(t, "attr 硬编码", "mid 列", itemMids(items), []int64{3004, 3003, 3002})
	wantInt32sEQ(t, "attr 硬编码", "三条全是 FOLLOWING=1", itemAttrs(items), []int32{1, 1, 1})
	wantEQ(t, "attr 硬编码", "库里 attr 未被读出", items[1].GetAttr(), int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING))
	// 全程只读 relation_follow 一张表：不查特别关注、不做双向互关判定、不查计数。
	wantOps(t, "attr 硬编码链路", st.log.ops, []string{
		"follow.ListFollowings:count:2001/1/20",
		"follow.ListFollowings:rows:2001/1/20",
	})
	wantCount(t, "attr 硬编码", st.log, "special.", 0)
	wantCount(t, "attr 硬编码", st.log, "black.", 0)
}

func TestPropagatesListFollowingFailures(t *testing.T) {
	t.Run("COUNT 语句失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		listFollowingSeed(st)
		st.follows.failWith("ListFollowingsCount", errStore)
		l := NewListFollowingLogic(context.Background(), e.svcCtx)

		got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: 1, Ps: 20})
		wantErrIs(t, "COUNT 失败", err, errStore)
		wantErrMessage(t, "COUNT 失败", err, "relation_follow ListFollowings count: social-graph-test: store unavailable")
		wantOps(t, "COUNT 失败链路", st.log.ops, []string{"follow.ListFollowings:count:2001/1/20"})
		if got != nil {
			t.Fatalf("COUNT 失败：响应 = %+v, want nil", got)
		}
	})
	t.Run("列表语句失败", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		listFollowingSeed(st)
		st.follows.failWith("ListFollowingsRows", errStore)
		l := NewListFollowingLogic(context.Background(), e.svcCtx)

		got, err := l.ListFollowing(&rpc.ListReq{Mid: 2001, Pn: 1, Ps: 20})
		wantErrIs(t, "列表失败", err, errStore)
		wantErrMessage(t, "列表失败", err, "relation_follow ListFollowings list: social-graph-test: store unavailable")
		wantOps(t, "列表失败链路", st.log.ops, []string{
			"follow.ListFollowings:count:2001/1/20",
			"follow.ListFollowings:rows:2001/1/20",
		})
		if got != nil {
			t.Fatalf("列表失败：响应 = %+v, want nil（不得只回 total 伪装成功）", got)
		}
	})
}

func dedup(in []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
