package logic

// 本文件锁两个「分成规则」读入口（GetRevenueRule / ListRevenueRules）的判定口径：
//   - 定位方式的优先级（rule_id > rule_code > 报错）必须可判别：用「两个都给但指向不同行」
//     来钉住，只断言「不报错」是永真断言；
//   - 历史版本回放的可得性是资金复核的根：版本大于当前版本 → found=false（该版还不存在），
//     变更台账缺失 → found=false 且**绝不回现单价**（回现值就是拿今天的口径回答昨天的争议）；
//   - 「首条 from_version >= ?」的排序事实源是 model/cr_rule_change_log.go:115 的
//     `ORDER BY from_version ASC`，所以种子按插入序与版本序**相反**放，插入序泄漏即红；
//   - 读故障与「查不到」必须分开（三类故障之一：原始错误上抛）；
//   - 现状缺陷原样钉住 + README 登记：ListRevenueRules 只校验 source_type、不校验 state
//     （listrevenueruleslogic.go:46），与本服务另外两个列表入口的口径不一致。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"
)

func runGetRevenueRule(t *testing.T, ctx *svc.ServiceContext, in *rpc.GetRevenueRuleReq) (*rpc.GetRevenueRuleReply, error) {
	t.Helper()
	return NewGetRevenueRuleLogic(context.Background(), ctx).GetRevenueRule(in)
}

func runListRevenueRules(t *testing.T, ctx *svc.ServiceContext, in *rpc.ListRevenueRulesReq) (*rpc.ListRevenueRulesReply, error) {
	t.Helper()
	return NewListRevenueRulesLogic(context.Background(), ctx).ListRevenueRules(in)
}

// ruleRowSeed 造一行规则（单价/封顶/时间戳均为明显假值）。
func ruleRowSeed(id int64, code string, sourceType, state int32, price, version int64) *model.RevenueRule {
	return &model.RevenueRule{
		RuleId: id, RuleCode: code, SourceType: sourceType, Name: code + " 规则",
		Description: "口径说明", UnitPricePer1000: price, Currency: "CNY", Unit: "minute",
		MinQuantity: 10, MonthlyCapMinor: 1500, State: state, EffectiveFrom: 1_700_000_000,
		Version: version, CreatedBy: txOperator, UpdatedBy: "ops-02",
		Ctime: 1_700_000_000, Mtime: 1_710_000_000,
	}
}

// appendRuleLog 放一条规则变更台账。LogId 手工给定：FirstChangeFrom 的裁决只认
// from_version，插入序与版本序故意打乱（见 TestGetRevenueRuleHistoryReplay）。
func appendRuleLog(db *fakeDB, logID int64, code string, fromVersion, fromPrice int64, fromState int32) {
	db.ruleLogs = append(db.ruleLogs, &model.RuleChangeLog{
		LogId: logID, RuleCode: code, SourceType: model.SourceTypeVipWatch,
		Action: model.RuleActionUpdate, FromState: fromState, ToState: model.RuleStateActive,
		FromUnitPrice: fromPrice, ToUnitPrice: fromPrice + 100,
		FromVersion: fromVersion, ToVersion: fromVersion + 1,
		Operator: txOperator, Reason: "调价", RequestId: "req-log-" + code, Ctime: 1_700_000_100,
	})
}

func mustRuleSeq(t *testing.T, rows []*rpc.RevenueRuleInfo, want ...int64) {
	t.Helper()
	got := make([]int64, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.RuleId)
	}
	if len(got) != len(want) {
		t.Fatalf("rule_id 序列不符：期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个 rule_id 不符：期望 %v，实际 %v", i, want, got)
		}
	}
}

// ---------------------------------------------------------------- GetRevenueRule

func TestGetRevenueRuleTargetRequiredBeforeAnyRead(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.GetRevenueRuleReq
	}{
		{"两个定位位都不给", &rpc.GetRevenueRuleReq{}},
		{"rule_code 只有空白", &rpc.GetRevenueRuleReq{RuleCode: "   "}},
		{"负 rule_id 不算定位", &rpc.GetRevenueRuleReq{RuleId: -1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			db.addRule(ruleRowSeed(7, "R_SEVEN", model.SourceTypeVipWatch, model.RuleStateActive, 700, 5))
			reply, err := runGetRevenueRule(t, ctx, c.in)
			mustErrIs(t, err, model.ErrRuleTargetRequired)
			if reply != nil {
				t.Fatalf("闸门未过不得回结论：%+v", reply)
			}
			if len(db.reads) != 0 || len(db.calls) != 0 {
				t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
			}
		})
	}
}

func TestGetRevenueRuleReadyGate(t *testing.T) {
	ctx, db := newTestSvc(t)
	ctx.Rules = nil
	reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleCode: "R_X"})
	mustErrIs(t, err, model.ErrDBNotConfigured)
	if reply != nil {
		t.Fatalf("未配置库不得回结论：%+v", reply)
	}
	if len(db.reads) != 0 {
		t.Fatalf("Ready 闸门不该访问数据：%v", db.reads)
	}
}

// 定位优先级：rule_id 赢，且**只**走 rule_id 那条读。
// 两条读都发就判不出优先级，所以断言的是读轨迹逐项等长。
func TestGetRevenueRulePrefersRuleIdOverRuleCode(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(7, "R_SEVEN", model.SourceTypeVipWatch, model.RuleStateActive, 700, 1))
	db.addRule(ruleRowSeed(9, "R_NINE", model.SourceTypeVipWatch, model.RuleStateActive, 900, 1))

	reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, RuleCode: "R_NINE"})
	mustNoErr(t, err)
	if !reply.Found || reply.Rule.RuleCode != "R_SEVEN" || reply.Rule.UnitPricePer_1000Minor != 700 {
		t.Fatalf("rule_id 必须压过 rule_code：%+v", reply.Rule)
	}
	db.assertReads(t, 0, "fake:rules.FindOne")

	// 只给 rule_code 时按码定位，并且码值要先裁空白（网关常带 padding）。
	byCode, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleCode: "  R_NINE  "})
	mustNoErr(t, err)
	if !byCode.Found || byCode.Rule.RuleId != 9 {
		t.Fatalf("rule_code 定位/去空白错误：%+v", byCode.Rule)
	}
	db.assertReads(t, 1, "fake:rules.FindByCode")

	// 现状哨兵：负 rule_id 不进 rule_id 分支（判据是 `> 0`），
	// 于是「rule_id=-1 + rule_code 给了」会被静默降级成按码查。
	// 收严位置：定位分支前对 rule_id<0 直接报 ErrInvalidRuleId 类错误（本服务暂无该哨兵）。
	neg, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: -1, RuleCode: "R_SEVEN"})
	mustNoErr(t, err)
	if !neg.Found || neg.Rule.RuleId != 7 {
		t.Fatalf("现状：负 rule_id 被降级成按码查（若改为报错请同步撤销本断言）：%+v", neg.Rule)
	}
}

func TestGetRevenueRuleVersionEquivalence(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := db.addRule(ruleRowSeed(7, "R_SEVEN", model.SourceTypeVipWatch, model.RuleStateActive, 700, 5))

	// version=0 与 version==当前版本都读当前行，且**不碰变更台账**。
	for _, v := range []int64{0, 5} {
		reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, Version: v})
		mustNoErr(t, err)
		if !reply.Found || reply.Rule.Version != cur.Version || reply.Rule.UnitPricePer_1000Minor != 700 {
			t.Fatalf("version=%d 应读当前行：%+v", v, reply.Rule)
		}
	}
	db.assertReads(t, 0, "fake:rules.FindOne", "fake:rules.FindOne")

	// 未来版本 / 负版本：found=false，既不报错也不拿现值冒充。
	for _, v := range []int64{6, 99, -1} {
		before := len(db.reads)
		reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, Version: v})
		mustNoErr(t, err)
		if reply.Found || reply.Rule != nil {
			t.Fatalf("version=%d 还不存在，不得回值：%+v", v, reply)
		}
		// 只读主表一次：found=false 的结论不该再去翻变更台账。
		db.assertReads(t, before, "fake:rules.FindOne")
	}
}

// 历史回放：取 from_version >= 请求版本的**首条**（排序事实源 cr_rule_change_log.go:115
// `ORDER BY from_version ASC`），并只覆盖单价/状态/版本三位，其余字段沿用现值。
func TestGetRevenueRuleHistoryReplayPicksEarliestChangeFrom(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(7, "R_SEVEN", model.SourceTypeVipWatch, model.RuleStateActive, 900, 5))
	// 插入序与 from_version 序相反：按「第一条匹配」实现的错实现能被抓出来。
	appendRuleLog(db, 1, "R_SEVEN", 4, 500, model.RuleStateActive)
	appendRuleLog(db, 2, "R_SEVEN", 2, 300, model.RuleStateDraft)

	reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, Version: 3})
	mustNoErr(t, err)
	if !reply.Found {
		t.Fatal("version=3 落在 from_version=4 的生效区间，应回放成功")
	}
	got := reply.Rule
	if got.UnitPricePer_1000Minor != 500 || got.State != rpc.RuleState(model.RuleStateActive) || got.Version != 3 {
		t.Fatalf("单价/状态/版本回放错误：%+v", got)
	}
	// 现状哨兵（已进函数头说明并登记 README）：名称/说明/门槛/封顶/币种/生效起点
	// 不入变更台账，回放时沿用**现值**。复核面板若把它们当历史值读就会错判。
	if got.Name != "R_SEVEN 规则" || got.MinQuantity != 10 || got.MonthlyCapMinor != 1500 ||
		got.Currency != "CNY" || got.EffectiveFrom != 1_700_000_000 {
		t.Fatalf("现状：非单价字段被回放改写了（口径应是沿用现值）：%+v", got)
	}
	db.assertReads(t, 0, "fake:rules.FindOne", "fake:ruleChanges.FirstChangeFrom")

	// version=2：候选有 from_version 2 与 4，必须取最早那条（300 / DRAFT）。
	earliest, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, Version: 2})
	mustNoErr(t, err)
	if earliest.Rule.UnitPricePer_1000Minor != 300 ||
		earliest.Rule.State != rpc.RuleState(model.RuleStateDraft) {
		t.Fatalf("未取到 from_version 最小的台账行：%+v", earliest.Rule)
	}
	// version=1（比所有 from_version 都早）同样落到最早那条。
	oldest, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, Version: 1})
	mustNoErr(t, err)
	if oldest.Rule.UnitPricePer_1000Minor != 300 {
		t.Fatalf("低于全部 from_version 时应取最早一条：%+v", oldest.Rule)
	}
	// 定位用的 rule_code 来自当前行，调用方给的 rule_id 才是入口。
	db.assertReads(t, 2,
		"fake:rules.FindOne", "fake:ruleChanges.FirstChangeFrom",
		"fake:rules.FindOne", "fake:ruleChanges.FirstChangeFrom")
}

// 台账缺失（例如迁移前的存量规则）：宁可 not found，也不能把现单价当历史单价回。
func TestGetRevenueRuleHistoryWithoutLogIsNotFound(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(7, "R_SEVEN", model.SourceTypeVipWatch, model.RuleStateActive, 900, 5))
	appendRuleLog(db, 1, "R_OTHER", 2, 300, model.RuleStateDraft) // 别的规则的台账

	reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, Version: 2})
	mustNoErr(t, err)
	// found=false 且 rule 必须是 nil：只要不是 nil，就等于把现单价（900）当成历史单价外泄。
	if reply.Found || reply.Rule != nil {
		t.Fatalf("无台账时必须 not found 且不回任何单价，实际 found=%v rule=%+v", reply.Found, reply.Rule)
	}
	db.assertReads(t, 0, "fake:rules.FindOne", "fake:ruleChanges.FirstChangeFrom")
}

func TestGetRevenueRuleMissingRowIsFoundFalse(t *testing.T) {
	ctx, db := newTestSvc(t)
	reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleCode: "R_NONE"})
	mustNoErr(t, err)
	if reply.Found || reply.Rule != nil {
		t.Fatalf("规则不存在应回 found=false：%+v", reply)
	}
	db.assertReads(t, 0, "fake:rules.FindByCode")
}

// 三类故障分开钉之「原始错误上抛」：主表读失败、变更台账读失败都要原样上抛，
// 各自不得折叠成 found=false（那等于对争议复核说「这版规则从来不存在」）。
func TestGetRevenueRuleReadFailuresPropagateRaw(t *testing.T) {
	t.Run("主表读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		boom := errors.New("connection reset")
		db.readFailOn["fake:rules.FindOne"] = boom
		reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7})
		if !errors.Is(err, boom) {
			t.Fatalf("必须原样上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得伪装成 found=false：%+v", reply)
		}
		db.assertReads(t, 0, "fake:rules.FindOne")
	})

	t.Run("变更台账读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addRule(ruleRowSeed(7, "R_SEVEN", model.SourceTypeVipWatch, model.RuleStateActive, 900, 5))
		boom := errors.New("too-many-read-requests")
		db.readFailOn["fake:ruleChanges.FirstChangeFrom"] = boom
		reply, err := runGetRevenueRule(t, ctx, &rpc.GetRevenueRuleReq{RuleId: 7, Version: 2})
		if !errors.Is(err, boom) {
			t.Fatalf("台账读失败必须上抛而不是 not found：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得回半截结论：%+v", reply)
		}
		db.assertReads(t, 0, "fake:rules.FindOne", "fake:ruleChanges.FirstChangeFrom")
	})
}

// ---------------------------------------------------------------- ListRevenueRules

func TestListRevenueRulesSourceTypeGate(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(1, "R_ONE", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1))

	for _, st := range []rpc.RevenueSourceType{9, -1, rpc.RevenueSourceType(model.SourceTypeUnspecified + 99)} {
		reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{SourceType: st})
		mustErrIs(t, err, model.ErrInvalidSourceType)
		if reply != nil {
			t.Fatalf("闸门未过不得回结论：%+v", reply)
		}
	}
	if len(db.reads) != 0 || len(db.calls) != 0 {
		t.Fatalf("闸门未过却已访问数据：%v / %v", db.reads, db.calls)
	}

	// source_type=0 是「不过滤」的合法值，不能被枚举校验顺手拒掉。
	reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{SourceType: 0})
	mustNoErr(t, err)
	if reply.Total != 1 {
		t.Fatalf("0 表示不过滤：%+v", reply)
	}
}

// 现状哨兵（缺陷原样钉住，已登记 README 规则域）：
// ListRevenueRules 只校验 source_type，**不校验 state**。
// 越界状态被当成一个真实的过滤值 → 回「空列表 + total=0」，
// 与 ListEnrollments（越界回 ErrEnrollmentStateTransition）、
// ListSettlements（越界回 ErrInvalidRuleState）口径相反：
// 运营面板会把「筛选值写错了」读成「平台还没有这个状态的规则」。
func TestListRevenueRulesStateFilterIsNotValidated(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(1, "R_ONE", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1))

	for _, st := range []rpc.RuleState{4, 99, -1} {
		from := len(db.reads)
		reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{State: st, SourceType: 0})
		mustNoErr(t, err) // 现状：不报错
		if reply.Rules == nil || len(reply.Rules) != 0 || reply.Total != 0 {
			t.Fatalf("现状：越界 state 被当成过滤值回空列表（%d）：%+v", st, reply)
		}
		db.assertReads(t, from, "fake:rules.List", "fake:rules.Count")
	}
}

// 排序事实源：model/cr_revenue_rule.go:196 `ORDER BY rule_id ASC`。
func TestListRevenueRulesOrdersByRuleIdAscAndPages(t *testing.T) {
	ctx, db := newTestSvc(t)
	for _, r := range []*model.RevenueRule{
		ruleRowSeed(30, "R_30", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1),
		ruleRowSeed(10, "R_10", model.SourceTypeVipWatch, model.RuleStateDraft, 100, 1),
		ruleRowSeed(20, "R_20", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1),
	} {
		db.addRule(r)
	}

	full, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{Size: 50})
	mustNoErr(t, err)
	mustRuleSeq(t, full.Rules, 10, 20, 30)
	if full.Total != 3 {
		t.Fatalf("total 错误：%d", full.Total)
	}
	if w := db.window("fake:rules.List"); w.offset != 0 || w.limit != 50 {
		t.Fatalf("分页位没进查询：%+v", w)
	}

	// 逐页拼接必须等于全量读（第 3 页是空页，用来抓「越界页回补」的错实现）。
	var paged []*rpc.RevenueRuleInfo
	for page := int64(1); page <= 3; page++ {
		reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{Page: page, Size: 2})
		mustNoErr(t, err)
		if w := db.window("fake:rules.List"); w.offset != (page-1)*2 || w.limit != 2 {
			t.Fatalf("第 %d 页分页位错误：%+v", page, w)
		}
		if reply.Page != page || reply.Size != 2 || reply.Total != 3 {
			t.Fatalf("第 %d 页回显错误：page=%d size=%d total=%d", page, reply.Page, reply.Size, reply.Total)
		}
		paged = append(paged, reply.Rules...)
	}
	mustRuleSeq(t, paged, 10, 20, 30)
}

// state 与 source_type 两个条件在 List/Count 里必须同源（ruleFilter, cr_revenue_rule.go:250）。
func TestListRevenueRulesFiltersSharePredicateWithCount(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(1, "R_VIP_ON", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1))
	db.addRule(ruleRowSeed(2, "R_COIN_ON", model.SourceTypeCoin, model.RuleStateActive, 200, 1))
	db.addRule(ruleRowSeed(3, "R_COIN_DRAFT", model.SourceTypeCoin, model.RuleStateDraft, 200, 1))
	db.addRule(ruleRowSeed(4, "R_VIP_ARC", model.SourceTypeVipWatch, model.RuleStateArchived, 100, 1))

	cases := []struct {
		name          string
		state, source int32
		wantRuleIDs   []int64
	}{
		{"只看生效规则", model.RuleStateActive, 0, []int64{1, 2}},
		{"只看投币来源", 0, model.SourceTypeCoin, []int64{2, 3}},
		{"生效 + 投币（创作者端面板）", model.RuleStateActive, model.SourceTypeCoin, []int64{2}},
		{"归档 + 会员观看", model.RuleStateArchived, model.SourceTypeVipWatch, []int64{4}},
		{"两个都过滤后为空", model.RuleStateDraft, model.SourceTypeVipWatch, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			from := len(db.reads)
			reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{
				State: rpc.RuleState(c.state), SourceType: rpc.RevenueSourceType(c.source), Size: 10,
			})
			mustNoErr(t, err)
			mustRuleSeq(t, reply.Rules, c.wantRuleIDs...)
			if reply.Total != int64(len(c.wantRuleIDs)) {
				t.Fatalf("total 与过滤结果不同源：total=%d rows=%d", reply.Total, len(reply.Rules))
			}
			if reply.Rules == nil {
				t.Fatal("空结果也要回非 nil 空数组")
			}
			db.assertReads(t, from, "fake:rules.List", "fake:rules.Count")
		})
	}
}

func TestListRevenueRulesPageSizeFoldingReachesQuery(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(1, "R_ONE", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1))

	cases := []struct {
		name               string
		page, size         int64
		wantPage, wantSize int64
		wantOffset         int64
	}{
		{"页宽超上限", 1, 5000, 1, 100, 0},
		{"页宽 0", 1, 0, 1, 100, 0},
		{"页码 0", 0, 10, 1, 10, 0},
		{"深页保留 offset", 3, 10, 3, 10, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{Page: c.page, Size: c.size})
			mustNoErr(t, err)
			if reply.Page != c.wantPage || reply.Size != c.wantSize {
				t.Fatalf("回显分页错误：got page=%d size=%d want page=%d size=%d",
					reply.Page, reply.Size, c.wantPage, c.wantSize)
			}
			if w := db.window("fake:rules.List"); w.offset != c.wantOffset || w.limit != c.wantSize {
				t.Fatalf("实际查询分页位错误：%+v want offset=%d limit=%d", w, c.wantOffset, c.wantSize)
			}
		})
	}
}

func TestListRevenueRulesReadFailuresPropagateRaw(t *testing.T) {
	t.Run("列表读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		boom := errors.New("broken pipe")
		db.readFailOn["fake:rules.List"] = boom
		reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{})
		if !errors.Is(err, boom) {
			t.Fatalf("必须原样上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("故障不得回伪成功的空列表：%+v", reply)
		}
		db.assertReads(t, 0, "fake:rules.List")
	})

	t.Run("计数读失败", func(t *testing.T) {
		ctx, db := newTestSvc(t)
		db.addRule(ruleRowSeed(1, "R_ONE", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1))
		boom := errors.New("deadlock found")
		db.readFailOn["fake:rules.Count"] = boom
		reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{})
		if !errors.Is(err, boom) {
			t.Fatalf("计数失败必须上抛：%v", err)
		}
		if reply != nil {
			t.Fatalf("计数失败不得回带行的半截结论：%+v", reply)
		}
		db.assertReads(t, 0, "fake:rules.List", "fake:rules.Count")
	})
}

func TestListRevenueRulesNilRequestIsTolerated(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(1, "R_ONE", model.SourceTypeVipWatch, model.RuleStateActive, 100, 1))
	reply, err := runListRevenueRules(t, ctx, nil)
	mustNoErr(t, err)
	if reply.Total != 1 || len(reply.Rules) != 1 || reply.Page != 1 || reply.Size != 100 {
		t.Fatalf("nil 请求应折成不过滤的第一页：%+v", reply)
	}
}

// 现状哨兵（规则域，登记 README）：规则是平台级配置，任何入口都不带调用方身份位，
// 所以「运营面可见全部规则（含 DRAFT 单价草案）」这件事完全押在网关权限上。
// 这里钉的是「服务端确实不做任何过滤」，一旦网关漏配权限点，本服务侧没有第二道闸门。
func TestListRevenueRulesReturnsDraftsToAnyone(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleRowSeed(1, "R_DRAFT", model.SourceTypeVipWatch, model.RuleStateDraft, 424200, 1))
	reply, err := runListRevenueRules(t, ctx, &rpc.ListRevenueRulesReq{Size: 10})
	mustNoErr(t, err)
	if len(reply.Rules) != 1 || reply.Rules[0].UnitPricePer_1000Minor != 424200 ||
		reply.Rules[0].State != rpc.RuleState(model.RuleStateDraft) {
		t.Fatalf("现状：未生效草案原样外泄（服务端无第二道闸门）：%+v", reply.Rules)
	}
}
