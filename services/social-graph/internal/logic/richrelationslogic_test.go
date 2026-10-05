package logic

// richrelationslogic_test.go 覆盖 RichRelations（2026-10-04 新增的批量富关系读口）：
// 守卫（含批量上限与空列表的先后次序）、四位掩码的逐键组合、各位的独立性（不做优先级压制）、
// state=0 过滤、键去重，以及「四条查询任一失败即整体报错、后续查询不再发」的口径。
//
// 用例里的期望 attr 一律写成**字面量数字**而不是 rpc.RelationAttr_* 常量：
// 这样契约的位分配（1/2/4/8）一旦改动就会红，而不是跟着常量一起漂。

import (
	"context"
	"maps"
	"slices"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

func TestRichRelationsGuards(t *testing.T) {
	cases := []struct {
		name  string
		owner int64
		mids  []int64
		want  error
	}{
		{"owner 为 0", 0, []int64{3002}, model.ErrInvalidOwnerMid},
		{"owner 为负", -7, []int64{3002}, model.ErrInvalidOwnerMid},
		{"mids 101 个超限", 2001, ownersSized(101), model.ErrTooManyMids},
		{"mids 200 个超限", 2001, ownersSized(200), model.ErrTooManyMids},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := NewRichRelationsLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				_, err := l.RichRelations(&rpc.RichRelationsReq{Owner: c.owner, Mids: c.mids})
				return err
			})
		})
	}
}

// TestRichRelationsEmptyMidsShortCircuitsBeforeLimit 空列表在「超限」判定之前：
// 返回空 map 且四条查询一条都不发，不是错误。同时钉住上限是 >100 而不是 >=100。
func TestRichRelationsEmptyMidsShortCircuitsBeforeLimit(t *testing.T) {
	cases := []struct {
		name string
		mids []int64
	}{
		{"mids 为 nil", nil},
		{"mids 为空切片", []int64{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			l := NewRichRelationsLogic(context.Background(), e.svcCtx)

			got, err := l.RichRelations(&rpc.RichRelationsReq{Owner: 2001, Mids: c.mids})
			wantNoErr(t, c.name, err)
			if got == nil {
				t.Fatalf("%s：响应 = nil, want 空 map 响应", c.name)
			}
			wantEQ(t, c.name, "map 长度", len(got.GetAttrs()), 0)
			wantNoCall(t, c.name, st, 0)
		})
	}

	t.Run("恰好 100 个放行", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		l := NewRichRelationsLogic(context.Background(), e.svcCtx)
		got, err := l.RichRelations(&rpc.RichRelationsReq{Owner: 2001, Mids: ownersSized(100)})
		wantNoErr(t, "恰好 100 个", err)
		wantEQ(t, "恰好 100 个", "map 长度", len(got.GetAttrs()), 100)
		// 100 个 mid 是 4 条 IN 查询，不是 400 条单行查询，也不碰缓存。
		wantEQ(t, "恰好 100 个", "查询条数", len(st.log.ops), 4)
		wantCount(t, "恰好 100 个", st.log, "cache.", 0)
	})
}

// TestRichRelationsComposesFourIndependentBits 是本方法的判别性主用例：
// 每个 mid 只放一种形态，且形态两两不同，任何一位写反/漏读/被压制都会红。
func TestRichRelationsComposesFourIndependentBits(t *testing.T) {
	e := newEnv(t)
	st := e.st

	// 3002：只有 owner→3002 关注 ⇒ bit0。同时布一条「已取消拉黑」的黑行，证明 state 过滤。
	seedFollow(st, distinctFollow(101, 2001, 3002, 1_650_000_001))
	seedBlack(st, &model.RelationBlack{ID: 201, Mid: 2001, BlackMid: 3002, State: blackGone, Ctime: 1_650_000_002, Mtime: 1_650_000_003})
	// 3003：只有 3003→owner 关注（反方向）⇒ bit1。
	seedFollow(st, distinctFollow(102, 3003, 2001, 1_650_000_004))
	// 3004：双向 ⇒ bit0|bit1 = 3。同时布一条「已取消」的 special 行。
	seedFollow(st, distinctFollow(103, 2001, 3004, 1_650_000_005))
	seedFollow(st, distinctFollow(104, 3004, 2001, 1_650_000_006))
	seedSpecial(st, &model.RelationSpecial{ID: 301, Mid: 2001, SpecialMid: 3004, State: specialGone, Ctime: 1_650_000_007, Mtime: 1_650_000_008})
	// 3005：owner 已取关 3005（软删行仍在表里）⇒ 必须 0，证明读侧按 state=0 过滤。
	seedFollow(st, &model.RelationFollow{ID: 105, Mid: 2001, FollowerMid: 3005, State: followGone, Ctime: 1_650_000_009, Mtime: 1_650_000_010})
	// 3006：3006 关注 owner + owner 拉黑 3006 ⇒ bit1|bit2 = 6，证明拉黑不压制对方的关注位。
	seedFollow(st, distinctFollow(106, 3006, 2001, 1_650_000_011))
	seedBlack(st, distinctBlack(202, 2001, 3006, 1_650_000_012))
	// 3007：只有 special（关注行已软删）⇒ bit3 = 8，证明 special 不蕴含 following。
	seedFollow(st, &model.RelationFollow{ID: 107, Mid: 2001, FollowerMid: 3007, State: followGone, Ctime: 1_650_000_013, Mtime: 1_650_000_014})
	seedSpecial(st, distinctSpecial(302, 2001, 3007, 1_650_000_015))
	// 3008：四位全中 ⇒ 1|2|4|8 = 15。
	seedFollow(st, distinctFollow(108, 2001, 3008, 1_650_000_016))
	seedFollow(st, distinctFollow(109, 3008, 2001, 1_650_000_017))
	seedBlack(st, distinctBlack(203, 2001, 3008, 1_650_000_018))
	seedSpecial(st, distinctSpecial(303, 2001, 3008, 1_650_000_019))
	// 3999→3002 是别人的关系，不得串进来。
	seedFollow(st, distinctFollow(110, 3999, 3002, 1_650_000_020))

	mids := []int64{3002, 3003, 3004, 3005, 3006, 3007, 3008, 3009, 2001}
	l := NewRichRelationsLogic(context.Background(), e.svcCtx)
	got, err := l.RichRelations(&rpc.RichRelationsReq{Owner: 2001, Mids: mids})
	wantNoErr(t, "四位组合", err)
	out := got.GetAttrs()

	wantInt64sEQ(t, "四位组合", "键集合", slices.Sorted(maps.Keys(out)),
		[]int64{2001, 3002, 3003, 3004, 3005, 3006, 3007, 3008, 3009})
	// 逐键断言：只断言「非空」等于没断言。
	wantEQ(t, "四位组合", "3002 仅 owner→他（取消的黑行不计）", out[3002], int32(1))
	wantEQ(t, "四位组合", "3003 仅他→owner（反方向）", out[3003], int32(2))
	wantEQ(t, "四位组合", "3004 互关（取消的 special 行不计）", out[3004], int32(3))
	wantEQ(t, "四位组合", "3005 软删关注 ⇒ 无关系", out[3005], int32(0))
	wantEQ(t, "四位组合", "3006 他关注 owner 且被 owner 拉黑", out[3006], int32(6))
	wantEQ(t, "四位组合", "3007 只剩 special", out[3007], int32(8))
	wantEQ(t, "四位组合", "3008 四位全中", out[3008], int32(15))
	wantEQ(t, "四位组合", "3009 从未布数据", out[3009], int32(0))
	wantEQ(t, "四位组合", "2001 自查询（本服务不写自指行）", out[2001], int32(0))

	// 链路：恰好四条查询，顺序即掩码组合顺序；不查缓存、不动计数快照。
	wantOps(t, "四位组合链路", st.log.ops, []string{
		"follow.FindFollowings:2001/[3002 3003 3004 3005 3006 3007 3008 3009 2001]",
		"follow.FindFollowers:2001/[3002 3003 3004 3005 3006 3007 3008 3009 2001]",
		"black.FindBlacks:2001/[3002 3003 3004 3005 3006 3007 3008 3009 2001]",
		"special.FindSpecials:2001/[3002 3003 3004 3005 3006 3007 3008 3009 2001]",
	})
	wantCount(t, "四位组合", st.log, "cache.", 0)
	wantCount(t, "四位组合", st.log, "stat.", 0)
	// 单条 IsFollowing/IsBlacked 一次都不该发：批量读不该退化成 N 次点查。
	wantCount(t, "四位组合", st.log, "follow.FindOne", 0)
	wantCount(t, "四位组合", st.log, "black.FindOne", 0)
}

// TestRichRelationsBitNumbersMatchContract 把字面量与契约常量对齐一次：
// 上面的期望用字面量写死，这条负责说明 8 就是 SPECIAL 位（改位数会同时红两条）。
func TestRichRelationsBitNumbersMatchContract(t *testing.T) {
	wantEQ(t, "契约位表", "FOLLOWING", int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING), int32(1))
	wantEQ(t, "契约位表", "FOLLOWER", int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWER), int32(2))
	wantEQ(t, "契约位表", "BLACKED", int32(rpc.RelationAttr_RELATION_ATTR_BLACKED), int32(4))
	wantEQ(t, "契约位表", "SPECIAL", int32(rpc.RelationAttr_RELATION_ATTR_SPECIAL), int32(8))
	wantEQ(t, "契约位表", "MUTUAL 是派生值而非独立位",
		int32(rpc.RelationAttr_RELATION_ATTR_MUTUAL),
		int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING)|int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWER))
}

// TestRichRelationsDeduplicatesMids 重复 mid 合并成一个键（proto 说 mid → attr，是映射不是数组）。
func TestRichRelationsDeduplicatesMids(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedFollow(st, distinctFollow(111, 2001, 3002, 1_650_000_030))
	l := NewRichRelationsLogic(context.Background(), e.svcCtx)

	got, err := l.RichRelations(&rpc.RichRelationsReq{Owner: 2001, Mids: []int64{3002, 3002, 3002}})
	wantNoErr(t, "重复 mid", err)
	wantEQ(t, "重复 mid", "键个数", len(got.GetAttrs()), 1)
	wantEQ(t, "重复 mid", "3002", got.GetAttrs()[3002], int32(1))
}

// TestRichRelationsDoesNotValidateMidValues mids 里的 0/负数原样进 IN 列表：
// 只会得到 attr=0，不报错（钉住当前口径，防止「顺手加校验」无人察觉）。
func TestRichRelationsDoesNotValidateMidValues(t *testing.T) {
	e := newEnv(t)
	st := e.st
	l := NewRichRelationsLogic(context.Background(), e.svcCtx)

	got, err := l.RichRelations(&rpc.RichRelationsReq{Owner: 2001, Mids: []int64{0, -1}})
	wantNoErr(t, "非法 mid 值", err)
	wantEQ(t, "非法 mid 值", "0", got.GetAttrs()[0], int32(0))
	wantEQ(t, "非法 mid 值", "-1", got.GetAttrs()[-1], int32(0))
	wantCount(t, "非法 mid 值", st.log, "follow.FindFollowings:2001", 1)
}

// TestPropagatesRichRelationsFailureEachQuery 四条查询任一失败都：整体报错、响应为 nil、
// 且**失败之后的查询不再发**（半张掩码表会被读成「没关注/没拉黑」，比报错危险）。
func TestPropagatesRichRelationsFailureEachQuery(t *testing.T) {
	cases := []struct {
		name string
		fail func(st *store)
		// ops 断言的是失败那一刻之前（含失败那条）的完整轨迹。
		ops []string
	}{
		{
			name: "正向关注查询失败",
			fail: func(st *store) { st.follows.failWith("FindFollowings", errStore) },
			ops:  []string{"follow.FindFollowings:2001/[3002 3003]"},
		},
		{
			name: "反向关注查询失败",
			fail: func(st *store) { st.follows.failWith("FindFollowers", errStore) },
			ops: []string{
				"follow.FindFollowings:2001/[3002 3003]",
				"follow.FindFollowers:2001/[3002 3003]",
			},
		},
		{
			name: "黑名单查询失败",
			fail: func(st *store) { st.blacks.failWith("FindBlacks", errStore) },
			ops: []string{
				"follow.FindFollowings:2001/[3002 3003]",
				"follow.FindFollowers:2001/[3002 3003]",
				"black.FindBlacks:2001/[3002 3003]",
			},
		},
		{
			name: "特别关注查询失败",
			fail: func(st *store) { st.specials.failWith("FindSpecials", errStore) },
			ops: []string{
				"follow.FindFollowings:2001/[3002 3003]",
				"follow.FindFollowers:2001/[3002 3003]",
				"black.FindBlacks:2001/[3002 3003]",
				"special.FindSpecials:2001/[3002 3003]",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			c.fail(st)
			l := NewRichRelationsLogic(context.Background(), e.svcCtx)

			got, err := l.RichRelations(&rpc.RichRelationsReq{Owner: 2001, Mids: []int64{3002, 3003}})
			wantErrIs(t, c.name, err, errStore)
			if got != nil {
				t.Fatalf("%s：响应 = %+v, want nil（不得回半截掩码伪装全 0）", c.name, got)
			}
			wantOps(t, c.name+"链路", st.log.ops, c.ops)
		})
	}
}
