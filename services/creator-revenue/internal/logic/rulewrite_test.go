package logic

// 本文件锁规则写侧口径（rulereplay.go + UpsertRevenueRule/SetRevenueRuleState 入口）：
// 只写 DRAFT、状态机三条合法边、同来源唯一 ACTIVE 的自动归档，
// 以及「主表 + cr_rule_change_log 同事务」与 request_id 幂等回放。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ruleSeed 造一条规则行（单价 1000 分/千单位，version 由 addRule 兜成 1）。
func ruleSeed(code string, sourceType int32, state int32) *model.RevenueRule {
	return &model.RevenueRule{
		RuleCode: code, SourceType: sourceType, Name: code + " 规则",
		UnitPricePer1000: 1000, Currency: "CNY", Unit: "minute",
		State: state, CreatedBy: "ops-01", UpdatedBy: "ops-01",
	}
}

func ruleInput(mut func(*rpc.UpsertRevenueRuleReq)) *rpc.UpsertRevenueRuleReq {
	in := &rpc.UpsertRevenueRuleReq{
		RuleCode: "R_VIP", SourceType: rpc.RevenueSourceType(model.SourceTypeVipWatch),
		Name: "会员有效观看", UnitPricePer_1000Minor: 1000, Currency: "CNY", Unit: "minute",
		MinQuantity: 10, MonthlyCapMinor: 150000, Operator: "ops-77",
		RequestId: "req-rule-1", Reason: "新增单价口径",
	}
	if mut != nil {
		mut(in)
	}
	return in
}

// runRuleDraftTx / runStateChangeTx 从真实事务边界进入被测函数，
// 失败即整体回滚，与 UpsertRevenueRule / SetRevenueRuleState 的调用方式一致。
func runRuleDraftTx(db *fakeDB, d *ruleDraft) (int64, bool, error) {
	var (
		id      int64
		created bool
	)
	err := fakeConn{db: db}.TransactCtx(context.Background(),
		func(ctx context.Context, tx sqlx.Session) error {
			var err error
			id, created, err = applyRuleDraftInTx(ctx, tx, d)
			return err
		})
	return id, created, err
}

func runStateChangeTx(db *fakeDB, pre *model.RevenueRule, target int32, version int64, reqID string) error {
	return fakeConn{db: db}.TransactCtx(context.Background(),
		func(ctx context.Context, tx sqlx.Session) error {
			return applyRuleStateChange(ctx, tx, pre, target, version, "ops-77", reqID, "生效新单价")
		})
}

func mustRuleLog(t *testing.T, logs []*model.RuleChangeLog, action string) *model.RuleChangeLog {
	t.Helper()
	for _, l := range logs {
		if l.Action == action {
			return l
		}
	}
	t.Fatalf("缺少 action=%s 的规则台账，实际 %+v", action, logs)
	return nil
}

// ---------------------------------------------------------------- 入参护栏

func TestValidateRuleUpsertGates(t *testing.T) {
	ctx, _ := newTestSvc(t)

	cases := []struct {
		name   string
		in     *rpc.UpsertRevenueRuleReq
		target error
	}{
		{"rule_code 必填", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.RuleCode = "  " }), model.ErrRuleCodeRequired},
		{"name 必填", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.Name = "" }), model.ErrTextTooLong},
		{"name 超列宽", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.Name = strings.Repeat("n", model.MaxRuleNameBytes+1)
		}), model.ErrTextTooLong},
		{"description 超列宽", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.Description = strings.Repeat("d", model.MaxDescriptionBytes+1)
		}), model.ErrTextTooLong},
		{"来源必须是真实枚举", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.SourceType = rpc.RevenueSourceType(model.SourceTypeUnspecified)
		}), model.ErrInvalidSourceType},
		{"unit 必填", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.Unit = "" }), model.ErrInvalidRuleParams},
		{"currency 超列宽拒绝（不静默截断）", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.Currency = strings.Repeat("C", model.MaxCurrencyBytes+1)
		}), model.ErrTextTooLong},
		{"负单价拒绝", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.UnitPricePer_1000Minor = -1
		}), model.ErrNegativeUnitPrice},
		// 单价护栏是防手滑放大成资金事故的唯一拦点，上限取自配置而非写死。
		{"单价超配置上限", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.UnitPricePer_1000Minor = 1_000_001
		}), model.ErrRuleUnitPriceTooLarge},
		{"负门槛拒绝", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.MinQuantity = -1 }), model.ErrInvalidRuleParams},
		{"负封顶拒绝", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.MonthlyCapMinor = -1 }), model.ErrInvalidRuleParams},
		{"封顶超配置上限", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.MonthlyCapMinor = 50_000_001
		}), model.ErrRuleCapTooLarge},
		{"effective_from 负数拒绝", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.EffectiveFrom = -1
		}), model.ErrInvalidRuleParams},
		{"operator 必填", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.Operator = "" }), model.ErrOperatorRequired},
		{"reason 必填", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.Reason = " " }), model.ErrReasonRequired},
		{"reason 超列宽", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.Reason = strings.Repeat("理", model.MaxReasonBytes+1)
		}), model.ErrTextTooLong},
		{"request_id 必填", ruleInput(func(in *rpc.UpsertRevenueRuleReq) { in.RequestId = "" }), model.ErrRequestIDRequired},
		{"request_id 超列宽", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.RequestId = strings.Repeat("r", model.MaxRequestIDBytes+1)
		}), model.ErrTextTooLong},
		{"更新必须带 expected_version", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.RuleId, in.ExpectedVersion = 7, 0
		}), model.ErrVersionConflict},
		{"新建不得指定 expected_version", ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
			in.ExpectedVersion = 3
		}), model.ErrVersionConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := validateRuleUpsert(ctx.Config, c.in); !errors.Is(err, c.target) {
				t.Fatalf("期望 %v，实际 %v", c.target, err)
			}
		})
	}

	// 封顶 0 表示「不限额」，是合法值；护栏只拦越界的大数。
	if _, err := validateRuleUpsert(ctx.Config, ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.MonthlyCapMinor = 0
	})); err != nil {
		t.Fatalf("monthly_cap_minor=0（不限额）应合法：%v", err)
	}
	// 单价刚好等于上限：护栏是「超过」才拒，边界值必须放行。
	if _, err := validateRuleUpsert(ctx.Config, ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.UnitPricePer_1000Minor = 1_000_000
	})); err != nil {
		t.Fatalf("上限边界值被误拒：%v", err)
	}
}

// 归一化与默认值：写侧只产出 DRAFT 意图，币种缺失回落配置。
func TestValidateRuleUpsertNormalizes(t *testing.T) {
	ctx, _ := newTestSvc(t)
	d, err := validateRuleUpsert(ctx.Config, ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RuleCode = "  R_VIP  "
		in.Name = "  会员有效观看  "
		in.Currency = "  "
	}))
	mustNoErr(t, err)
	if d.RuleCode != "R_VIP" || d.Name != "会员有效观看" {
		t.Fatalf("入参未做首尾空白归一：%q / %q", d.RuleCode, d.Name)
	}
	if d.Currency != "CNY" {
		t.Fatalf("缺币种未回落到配置默认值：%q", d.Currency)
	}
	if d.UnitPricePer1000 != 1000 || d.MinQuantity != 10 || d.MonthlyCapMinor != 150000 {
		t.Fatalf("折算参数未按入参落草稿：%+v", d)
	}
	if d.Reason != "新增单价口径" || d.RequestId != "req-rule-1" || d.Operator != "ops-77" {
		t.Fatalf("复核要素丢失：%+v", d)
	}
}

// ---------------------------------------------------------------- 新建 / 修改草稿

func TestApplyRuleDraftCreatesDraftWithCreateLog(t *testing.T) {
	ctx, db := newTestSvc(t)
	d, err := validateRuleUpsert(ctx.Config, ruleInput(nil))
	mustNoErr(t, err)

	id, created, err := runRuleDraftTx(db, d)
	mustNoErr(t, err)
	if !created || id == 0 {
		t.Fatalf("首建应回 created 且带主键：id=%d created=%v", id, created)
	}
	row := db.ruleByCode("R_VIP")
	if row == nil {
		t.Fatal("规则未落库")
	}
	// 写侧只产生 DRAFT：ACTIVE 单价直接决定别人的钱，不能凭空出现。
	if row.State != model.RuleStateDraft {
		t.Fatalf("新建规则必须是 DRAFT，实际 state=%d", row.State)
	}
	if row.Version != 1 || row.CreatedBy != "ops-77" || row.UpdatedBy != "ops-77" {
		t.Fatalf("首版与责任人记录缺失：%+v", row)
	}
	if len(db.ruleLogs) != 1 {
		t.Fatalf("首建必须留一条台账：%+v", db.ruleLogs)
	}
	l := db.ruleLogs[0]
	if l.Action != model.RuleActionCreate || l.FromState != model.RuleStateUnspecified ||
		l.ToState != model.RuleStateDraft || l.FromVersion != 0 || l.ToVersion != 1 {
		t.Fatalf("CREATE 台账口径错误：%+v", l)
	}
	if l.FromUnitPrice != 0 || l.ToUnitPrice != 1000 || l.Reason != "新增单价口径" ||
		l.RequestId != "req-rule-1" || l.Operator != "ops-77" {
		t.Fatalf("CREATE 台账缺单价历史或责任人：%+v", l)
	}
}

func TestApplyRuleDraftUpdatesDraftAndLogsOldPrice(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	d, err := validateRuleUpsert(ctx.Config, ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RuleId, in.ExpectedVersion = cur.RuleId, cur.Version
		in.UnitPricePer_1000Minor = 2500
		in.Reason = "单价复核后上调"
	}))
	mustNoErr(t, err)

	id, created, err := runRuleDraftTx(db, d)
	mustNoErr(t, err)
	if !created || id != cur.RuleId {
		t.Fatalf("更新应复用既有主键：id=%d created=%v", id, created)
	}
	row := db.ruleByCode("R_VIP")
	if row.UnitPricePer1000 != 2500 || row.Version != 2 || row.State != model.RuleStateDraft {
		t.Fatalf("草稿更新未生效或未递增版本：%+v", row)
	}
	l := mustRuleLog(t, db.ruleLogs, model.RuleActionUpdate)
	// 「原来算多少」只有这张台账记得，主表已被覆盖。
	if l.FromUnitPrice != 1000 || l.ToUnitPrice != 2500 || l.FromVersion != 1 || l.ToVersion != 2 {
		t.Fatalf("UPDATE 台账未记旧值：%+v", l)
	}
	if l.FromState != model.RuleStateDraft || l.ToState != model.RuleStateDraft {
		t.Fatalf("改草稿不该动状态：%+v", l)
	}
}

func TestApplyRuleDraftGuards(t *testing.T) {
	cases := []struct {
		name   string
		state  int32
		revise func(*rpc.UpsertRevenueRuleReq, int64)
		target error
	}{
		{
			name:   "ACTIVE 规则不可就地改价",
			state:  model.RuleStateActive,
			target: model.ErrRuleNotDraft,
		},
		{
			name:   "ARCHIVED 规则不可复活式编辑",
			state:  model.RuleStateArchived,
			target: model.ErrRuleNotDraft,
		},
		{
			name:  "expected_version 与服务端不符",
			state: model.RuleStateDraft,
			revise: func(in *rpc.UpsertRevenueRuleReq, id int64) {
				in.RuleId, in.ExpectedVersion = id, 99
			},
			target: model.ErrVersionConflict,
		},
		{
			// rule_code 与 rule_id 指向不同行：不能凭调用方给的主键改到别人的规则。
			name:  "rule_id 与 rule_code 不同行",
			state: model.RuleStateDraft,
			revise: func(in *rpc.UpsertRevenueRuleReq, id int64) {
				in.RuleId, in.ExpectedVersion, in.RuleCode = id+1000, 1, "R_VIP"
			},
			target: model.ErrRuleNotFound,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, db := newTestSvc(t)
			seed := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, c.state))
			in := ruleInput(nil)
			if c.revise == nil {
				in.RuleId, in.ExpectedVersion = seed.RuleId, seed.Version
			} else {
				c.revise(in, seed.RuleId)
			}
			d, err := validateRuleUpsert(ctx.Config, in)
			mustNoErr(t, err)
			before := len(db.calls)
			_, _, err = runRuleDraftTx(db, d)
			mustErrIs(t, err, c.target)
			if db.countCallsAfter(before, "ins:cr_rule_change_log") != 0 || len(db.ruleLogs) != 0 {
				t.Fatalf("主表判定未过却写了台账：%+v", db.ruleLogs)
			}
			after := db.ruleByCode("R_VIP")
			if after.UnitPricePer1000 != 1000 || after.Version != seed.Version || after.State != c.state {
				t.Fatalf("被拒的更新改动了主表：%+v", after)
			}
		})
	}
}

func TestApplyRuleDraftCasMissIsVersionConflict(t *testing.T) {
	ctx, db := newTestSvc(t)
	seed := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	d, err := validateRuleUpsert(ctx.Config, ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RuleId, in.ExpectedVersion = seed.RuleId, seed.Version
		in.UnitPricePer_1000Minor = 3000
	}))
	mustNoErr(t, err)
	db.noRowsFor["upd:cr_revenue_rule.draft"] = true // 读完锁行后条件已不再命中（被人抢先）

	_, _, err = runRuleDraftTx(db, d)
	mustErrIs(t, err, model.ErrVersionConflict)
	if len(db.ruleLogs) != 0 {
		t.Fatalf("CAS 未命中时台账要一起回滚：%+v", db.ruleLogs)
	}
	if got := db.ruleByCode("R_VIP"); got.UnitPricePer1000 != 1000 || got.Version != 1 {
		t.Fatalf("CAS 未命中却改了单价或版本：%+v", got)
	}
}

func TestApplyRuleDraftCodeTakenIsFlaggedForReplayLookup(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	// 不带 rule_id 却复用已有编码：INSERT 撞 uniq_rule_code。
	d, err := validateRuleUpsert(ctx.Config, ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RequestId = "req-other"
	}))
	mustNoErr(t, err)
	_, _, err = runRuleDraftTx(db, d)
	if !errors.Is(err, errRuleCodeTaken) {
		t.Fatalf("编码被占用必须回 errRuleCodeTaken 交给外层判重放，实际 %v", err)
	}
	if len(db.rules) != 1 {
		t.Fatalf("被拒的新建多出一行：%d", len(db.rules))
	}
}

// 台账写入失败时主表必须一起回滚：只改主表等于「改了价没人知道」，
// 只写台账等于「有人声称改了价但金额没变」，两种都比不改更糟。
func TestApplyRuleDraftRollbackLeavesNoOrphanRow(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.failOn["ins:cr_rule_change_log"] = errors.New("deadline exceeded")
	d, err := validateRuleUpsert(ctx.Config, ruleInput(nil))
	mustNoErr(t, err)

	_, _, err = runRuleDraftTx(db, d)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("故障未上抛：%v", err)
	}
	if len(db.rules) != 0 || len(db.ruleLogs) != 0 {
		t.Fatalf("事务回滚不彻底：rules=%d logs=%d", len(db.rules), len(db.ruleLogs))
	}
}

// ---------------------------------------------------------------- 状态机

func TestCheckRuleTransitionEdges(t *testing.T) {
	mk := func(state int32) *model.RevenueRule {
		return &model.RevenueRule{RuleCode: "R_VIP", State: state, Version: 2}
	}
	cases := []struct {
		name    string
		from    int32
		to      int32
		wantErr bool
	}{
		{"DRAFT→ACTIVE 合法", model.RuleStateDraft, model.RuleStateActive, false},
		{"ACTIVE→ARCHIVED 合法", model.RuleStateActive, model.RuleStateArchived, false},
		{"DRAFT→ARCHIVED 合法（草稿作废）", model.RuleStateDraft, model.RuleStateArchived, false},
		{"同状态重复切换拒绝", model.RuleStateActive, model.RuleStateActive, true},
		{"ARCHIVED 是终态不可复活", model.RuleStateArchived, model.RuleStateActive, true},
		{"ARCHIVED 不可再归档", model.RuleStateArchived, model.RuleStateArchived, true},
		{"不能退回 DRAFT", model.RuleStateActive, model.RuleStateDraft, true},
		{"目标不能是枚举外的值", model.RuleStateDraft, model.RuleStateUnspecified, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkRuleTransition(mk(c.from), c.to)
			if c.wantErr {
				mustErrIs(t, err, model.ErrRuleStateTransition)
				return
			}
			mustNoErr(t, err)
		})
	}
}

func TestApplyRuleStateChangeAutoArchivesPreviousActive(t *testing.T) {
	_, db := newTestSvc(t)
	old := db.addRule(ruleSeed("R_VIP_OLD", model.SourceTypeVipWatch, model.RuleStateActive))
	old.Version = 5
	fresh := db.addRule(ruleSeed("R_VIP_NEW", model.SourceTypeVipWatch, model.RuleStateDraft))
	fresh.UnitPricePer1000 = 1200
	pre := *fresh // 调用方手里的是事务外读到的快照

	mustNoErr(t, runStateChangeTx(db, &pre, model.RuleStateActive, pre.Version, "req-act"))

	if pre.State != model.RuleStateDraft || pre.Version != 1 {
		t.Fatalf("被测函数就地改了调用方的快照：%+v", pre)
	}
	newRow := db.ruleByCode("R_VIP_NEW")
	if newRow.State != model.RuleStateActive || newRow.Version != 2 || newRow.UpdatedBy != "ops-77" {
		t.Fatalf("生效未落库：%+v", newRow)
	}
	oldRow := db.ruleByCode("R_VIP_OLD")
	if oldRow.State != model.RuleStateArchived || oldRow.Version != 6 {
		t.Fatalf("旧 ACTIVE 未被自动归档：%+v", oldRow)
	}
	if len(db.ruleLogs) != 2 {
		t.Fatalf("生效要留两笔台账（归档旧 + 生效新）：%+v", db.ruleLogs)
	}

	// 自动归档那笔必须能被反查：子键 = 父 request_id + "#a<rule_id>"，原因带上本次原因。
	auto := mustRuleLog(t, db.ruleLogs, model.RuleActionAutoArchive)
	if auto.RuleCode != "R_VIP_OLD" || auto.FromState != model.RuleStateActive ||
		auto.ToState != model.RuleStateArchived || auto.FromVersion != 5 || auto.ToVersion != 6 {
		t.Fatalf("AUTO_ARCHIVE 台账口径错误：%+v", auto)
	}
	if auto.RequestId != autoArchiveKey("req-act", old.RuleId) {
		t.Fatalf("AUTO_ARCHIVE 子键不可预测：%q", auto.RequestId)
	}
	if !strings.HasPrefix(auto.Reason, "因生效规则切换被自动归档；本次原因：生效新单价") {
		t.Fatalf("自动归档未带上本次原因，事后无法解释：%q", auto.Reason)
	}
	if auto.Operator != "ops-77" {
		t.Fatalf("自动归档丢了负责人：%+v", auto)
	}
	if auto.FromUnitPrice != auto.ToUnitPrice || auto.ToUnitPrice != 1000 {
		t.Fatalf("归档不改单价，台账两端必须一致：%+v", auto)
	}
	act := mustRuleLog(t, db.ruleLogs, model.RuleActionActivate)
	if act.RuleCode != "R_VIP_NEW" || act.RequestId != "req-act" ||
		act.FromVersion != 1 || act.ToVersion != 2 || act.FromUnitPrice != 1200 {
		t.Fatalf("ACTIVATE 台账口径错误：%+v", act)
	}
}

// TestApplyRuleStateChangeAutoArchiveReasonFitsColumn 锁住「自动归档台账的原因」这条窄路径：
// 前缀本身占掉几十字节，本次原因又允许给到列宽上限，拼接结果必然超长，
// 只能靠 trunc 兜住——而且兜尾必须落在汉字边界上，否则 utf8mb4 列直接写入失败。
func TestApplyRuleStateChangeAutoArchiveReasonFitsColumn(t *testing.T) {
	_, db := newTestSvc(t)
	db.addRule(ruleSeed("R_VIP_OLD", model.SourceTypeVipWatch, model.RuleStateActive))
	fresh := db.addRule(ruleSeed("R_VIP_NEW", model.SourceTypeVipWatch, model.RuleStateDraft))
	longReason := strings.Repeat("理", model.MaxReasonBytes/3) // 170 个汉字 = 510 字节

	err := fakeConn{db: db}.TransactCtx(context.Background(),
		func(ctx context.Context, tx sqlx.Session) error {
			return applyRuleStateChange(ctx, tx, fresh, model.RuleStateActive, fresh.Version,
				"ops-77", "req-wide", longReason)
		})
	mustNoErr(t, err)

	auto := mustRuleLog(t, db.ruleLogs, model.RuleActionAutoArchive)
	if len(auto.Reason) > model.MaxReasonBytes {
		t.Fatalf("原因超出 VARCHAR(%d)：%d 字节", model.MaxReasonBytes, len(auto.Reason))
	}
	if !utf8.ValidString(auto.Reason) {
		t.Fatalf("裁尾裁进了汉字内部，utf8mb4 列写入必失败：%q", auto.Reason)
	}
	if !strings.HasPrefix(auto.Reason, "因生效规则切换被自动归档；本次原因：理理理") {
		t.Fatalf("裁尾把本次原因裁没了：%q", auto.Reason)
	}
}

func TestApplyRuleStateChangeKeepsDifferentSourceActive(t *testing.T) {
	_, db := newTestSvc(t)
	coin := db.addRule(ruleSeed("R_COIN", model.SourceTypeCoin, model.RuleStateActive))
	fresh := db.addRule(ruleSeed("R_VIP_NEW", model.SourceTypeVipWatch, model.RuleStateDraft))

	mustNoErr(t, runStateChangeTx(db, fresh, model.RuleStateActive, fresh.Version, "req-act"))

	// 「同一来源至多一条 ACTIVE」不跨来源：投币那套单价不该被顺手归档。
	if got := db.ruleByCode("R_COIN"); got.State != model.RuleStateActive || got.Version != coin.Version {
		t.Fatalf("不同来源被误归档：%+v", got)
	}
	if len(db.ruleLogs) != 1 || db.ruleLogs[0].Action != model.RuleActionActivate {
		t.Fatalf("不该产生 AUTO_ARCHIVE 台账：%+v", db.ruleLogs)
	}
}

func TestApplyRuleStateChangeGuards(t *testing.T) {
	t.Run("并发改了来源则段锁失效", func(t *testing.T) {
		_, db := newTestSvc(t)
		cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeCoin, model.RuleStateDraft))
		// pre 是事务外读到的快照：期间 source_type 被别的请求改成了会员观看。
		pre := *cur
		pre.SourceType = model.SourceTypeVipWatch
		mustErrIs(t, runStateChangeTx(db, &pre, model.RuleStateActive, pre.Version, "req-x"),
			model.ErrConcurrentUpdate)
		if len(db.ruleLogs) != 0 || db.ruleByCode("R_VIP").State != model.RuleStateDraft {
			t.Fatal("并发判定失败却留下了副作用")
		}
	})
	t.Run("版本冲突", func(t *testing.T) {
		_, db := newTestSvc(t)
		cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
		mustErrIs(t, runStateChangeTx(db, cur, model.RuleStateActive, cur.Version+1, "req-x"),
			model.ErrVersionConflict)
		if len(db.ruleLogs) != 0 {
			t.Fatalf("版本冲突不该留台账：%+v", db.ruleLogs)
		}
	})
	t.Run("非法迁移", func(t *testing.T) {
		_, db := newTestSvc(t)
		cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateArchived))
		mustErrIs(t, runStateChangeTx(db, cur, model.RuleStateActive, cur.Version, "req-x"),
			model.ErrRuleStateTransition)
	})
	t.Run("规则不存在", func(t *testing.T) {
		_, db := newTestSvc(t)
		cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
		orphan := *cur
		orphan.RuleId = 999
		mustErrIs(t, runStateChangeTx(db, &orphan, model.RuleStateActive, orphan.Version, "req-x"),
			model.ErrRuleNotFound)
	})
	t.Run("自身 CAS 未命中", func(t *testing.T) {
		_, db := newTestSvc(t)
		cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
		db.noRowsFor["upd:cr_revenue_rule.state"] = true
		mustErrIs(t, runStateChangeTx(db, cur, model.RuleStateActive, cur.Version, "req-x"),
			model.ErrVersionConflict)
		if got := db.ruleByCode("R_VIP"); got.State != model.RuleStateDraft || len(db.ruleLogs) != 0 {
			t.Fatalf("CAS 未命中仍留下了状态或台账：%+v %+v", got, db.ruleLogs)
		}
	})
	t.Run("归档旧 ACTIVE 的 CAS 未命中回可重试错误", func(t *testing.T) {
		_, db := newTestSvc(t)
		old := db.addRule(ruleSeed("R_VIP_OLD", model.SourceTypeVipWatch, model.RuleStateActive))
		old.Version = 5
		fresh := db.addRule(ruleSeed("R_VIP_NEW", model.SourceTypeVipWatch, model.RuleStateDraft))
		db.noRowsFor["upd:cr_revenue_rule.state"] = true
		// 同一条 UPDATE 语句：先执行的是「归档旧 ACTIVE」那笔，所以先在那儿 miss。
		mustErrIs(t, runStateChangeTx(db, fresh, model.RuleStateActive, fresh.Version, "req-x"),
			model.ErrConcurrentUpdate)
		if got := db.ruleByCode("R_VIP_OLD"); got.State != model.RuleStateActive || got.Version != 5 {
			t.Fatalf("并发失败却改动了旧规则：%+v", got)
		}
	})
	t.Run("台账写不进去则两行状态都回滚", func(t *testing.T) {
		_, db := newTestSvc(t)
		db.addRule(ruleSeed("R_VIP_OLD", model.SourceTypeVipWatch, model.RuleStateActive))
		fresh := db.addRule(ruleSeed("R_VIP_NEW", model.SourceTypeVipWatch, model.RuleStateDraft))
		db.failOn["ins:cr_rule_change_log"] = errors.New("lock wait timeout")
		err := runStateChangeTx(db, fresh, model.RuleStateActive, fresh.Version, "req-x")
		if err == nil || !strings.Contains(err.Error(), "lock wait timeout") {
			t.Fatalf("台账故障必须上抛而不是吞掉：%v", err)
		}
		if db.ruleByCode("R_VIP_OLD").State != model.RuleStateActive ||
			db.ruleByCode("R_VIP_NEW").State != model.RuleStateDraft {
			t.Fatal("台账写不进去时，生效与自动归档都必须保持原样（否则同来源出现空档）")
		}
	})
}

func TestAutoArchiveKeyWidthBudget(t *testing.T) {
	if got := autoArchiveKey("req-act", 42); got != "req-act#a42" {
		t.Fatalf("子键格式变了会破坏幂等反查：%q", got)
	}
	// 后缀预算必须容得下 int64 最坏宽度，否则 requireScopedRequestID 的留白算错，
	// 落库时幂等键会被截断成两条不同键。
	if n := len(autoArchiveKey("", int64(^uint64(0)>>1))); n != autoArchiveSuffixBytes {
		t.Fatalf("autoArchiveSuffixBytes=%d 与实际最坏宽度 %d 不符", autoArchiveSuffixBytes, n)
	}
}

// ---------------------------------------------------------------- request_id 幂等回放

func TestResolveRuleReplayBranches(t *testing.T) {
	_, db := newTestSvc(t)
	rules := fakeRules{db: db}
	logs := fakeRuleLogs{db: db}

	// 1. 唯一键冲突却查不到台账行：只能是并发同键，让调用方重试。
	_, err := resolveRuleReplay(context.Background(), rules, logs, "R_VIP", "req-none")
	mustErrIs(t, err, model.ErrConcurrentUpdate)

	cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	db.ruleLogs = append(db.ruleLogs, &model.RuleChangeLog{
		RuleCode: "R_OTHER", Action: model.RuleActionCreate, RequestId: "req-shared", ToVersion: 1,
	})
	// 2. 同一个 request_id 用在了别的 rule_code 上：调用方复用错了键。
	_, err = resolveRuleReplay(context.Background(), rules, logs, "R_VIP", "req-shared")
	mustErrIs(t, err, model.ErrRequestIDConflict)

	db.ruleLogs = append(db.ruleLogs, &model.RuleChangeLog{
		RuleCode: "R_VIP", Action: model.RuleActionCreate, RequestId: "req-1", ToVersion: 1,
	})
	// 3. 真重放：台账 ToVersion 就是当前版本 → 回首次结果。
	info, err := resolveRuleReplay(context.Background(), rules, logs, "R_VIP", "req-1")
	mustNoErr(t, err)
	if info == nil || info.RuleId != cur.RuleId || info.Version != 1 {
		t.Fatalf("重放没回首次结论：%+v", info)
	}
	// 4. 其后又有新变更：首次结果已被覆盖，不能回一个「看起来对但其实是今天的值」。
	cur.Version = 7
	_, err = resolveRuleReplay(context.Background(), rules, logs, "R_VIP", "req-1")
	mustErrIs(t, err, model.ErrRequestIDConflict)

	// 5. 规则行读不到：宁可回冲突，也不能回 (nil, nil) 冒充成功。
	db.rules = nil
	info, err = resolveRuleReplay(context.Background(), rules, logs, "R_VIP", "req-1")
	mustErrIs(t, err, model.ErrRequestIDConflict)
	if info != nil {
		t.Fatalf("冲突时不得带回应答：%+v", info)
	}
}

func TestUpsertRevenueRuleCreateReplayReturnsFirstResult(t *testing.T) {
	ctx, db := newTestSvc(t)
	l := NewUpsertRevenueRuleLogic(context.Background(), ctx)

	first, err := l.UpsertRevenueRule(ruleInput(nil))
	mustNoErr(t, err)
	if first == nil || first.State != rpc.RuleState(model.RuleStateDraft) || first.Version != 1 {
		t.Fatalf("首建结论错误：%+v", first)
	}

	// 超时重试：同一 request_id、同一份内容再来一次。
	again, err := l.UpsertRevenueRule(ruleInput(nil))
	mustNoErr(t, err)
	if again.RuleId != first.RuleId || again.Version != first.Version ||
		again.UnitPricePer_1000Minor != first.UnitPricePer_1000Minor {
		t.Fatalf("重放没回首次结果：%+v vs %+v", again, first)
	}
	if len(db.rules) != 1 {
		t.Fatalf("重放多出一行规则：%d", len(db.rules))
	}
	// 台账只有一笔 CREATE：重放绝不产生第二次变更，也不留 old==new 的噪音行。
	if len(db.ruleLogs) != 1 || db.ruleLogs[0].Action != model.RuleActionCreate {
		t.Fatalf("重放留下了多余台账：%+v", db.ruleLogs)
	}
}

func TestUpsertRevenueRuleRequestIdReuseRollsBackRuleRow(t *testing.T) {
	ctx, db := newTestSvc(t)
	l := NewUpsertRevenueRuleLogic(context.Background(), ctx)
	if _, err := l.UpsertRevenueRule(ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RuleCode = "R_COIN"
	})); err != nil {
		t.Fatalf("首建失败：%v", err)
	}

	// 新编码、旧幂等键：主表插入会成功，但台账 INSERT 撞 uniq_request_id。
	_, err := l.UpsertRevenueRule(ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RuleCode = "R_VIP"
	}))
	mustErrIs(t, err, model.ErrRequestIDConflict)
	// 关键断言：绝不能留下「有规则、无台账」的孤儿行。
	if got := db.ruleByCode("R_VIP"); got != nil {
		t.Fatalf("幂等冲突的规则行未回滚：%+v", got)
	}
	if len(db.rules) != 1 || len(db.ruleLogs) != 1 {
		t.Fatalf("数据被部分写入：rules=%d logs=%d", len(db.rules), len(db.ruleLogs))
	}
}

func TestUpsertRevenueRuleCodeTakenByOtherIsConflict(t *testing.T) {
	ctx, db := newTestSvc(t)
	db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	l := NewUpsertRevenueRuleLogic(context.Background(), ctx)

	// 编码已被别人占用、request_id 也是全新的：回编码冲突并给出可操作建议。
	_, err := l.UpsertRevenueRule(ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RequestId = "req-fresh"
	}))
	mustErrIs(t, err, model.ErrRuleCodeConflict)
	if !strings.Contains(err.Error(), "expected_version") {
		t.Fatalf("错误未告知改价的正确路径：%v", err)
	}
	if len(db.rules) != 1 || len(db.ruleLogs) != 0 {
		t.Fatalf("被拒请求写了数据：rules=%d logs=%d", len(db.rules), len(db.ruleLogs))
	}
}

// 锁定当前行为：更新类请求的重试（CAS 先于幂等键判定）回的是版本冲突，
// 而不是首次结果 —— 调用方无法从错误码区分「已生效」与「真冲突」。
// 与 rulereplay.go 函数头「同一 request_id 重放回首次结果」的承诺不一致，已进交付报告。
func TestUpsertRevenueRuleUpdateRetryIsVersionConflict(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	l := NewUpsertRevenueRuleLogic(context.Background(), ctx)
	req := ruleInput(func(in *rpc.UpsertRevenueRuleReq) {
		in.RuleId, in.ExpectedVersion = cur.RuleId, cur.Version
		in.UnitPricePer_1000Minor = 2500
	})
	first, err := l.UpsertRevenueRule(req)
	mustNoErr(t, err)
	if first.Version != 2 {
		t.Fatalf("首调未生效：%+v", first)
	}

	again, err := l.UpsertRevenueRule(req)
	mustErrIs(t, err, model.ErrVersionConflict)
	if again != nil {
		t.Fatalf("当前实现回放不了，应回 nil：%+v", again)
	}
	if got := db.ruleByCode("R_VIP"); got.Version != 2 || got.UnitPricePer1000 != 2500 {
		t.Fatalf("重试造成了二次变更：%+v", got)
	}
	if len(db.ruleLogs) != 1 {
		t.Fatalf("重试多出一笔台账：%+v", db.ruleLogs)
	}
}

func TestSetRevenueRuleStateEntryValidation(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	l := NewSetRevenueRuleStateLogic(context.Background(), ctx)

	newReq := func(mut func(*rpc.SetRevenueRuleStateReq)) *rpc.SetRevenueRuleStateReq {
		r := &rpc.SetRevenueRuleStateReq{
			RuleId: cur.RuleId, TargetState: rpc.RuleState(model.RuleStateActive),
			ExpectedVersion: cur.Version, Operator: "ops-77", RequestId: "req-act", Reason: "复核通过",
		}
		if mut != nil {
			mut(r)
		}
		return r
	}

	cases := []struct {
		name   string
		in     *rpc.SetRevenueRuleStateReq
		target error
	}{
		{"必须指定规则", newReq(func(r *rpc.SetRevenueRuleStateReq) { r.RuleId = 0 }), model.ErrRuleTargetRequired},
		{"规则不存在", newReq(func(r *rpc.SetRevenueRuleStateReq) { r.RuleId = 4242 }), model.ErrRuleNotFound},
		{"operator 必填", newReq(func(r *rpc.SetRevenueRuleStateReq) { r.Operator = " " }), model.ErrOperatorRequired},
		{"reason 必填", newReq(func(r *rpc.SetRevenueRuleStateReq) { r.Reason = "" }), model.ErrReasonRequired},
		{"request_id 必填", newReq(func(r *rpc.SetRevenueRuleStateReq) { r.RequestId = "" }), model.ErrRequestIDRequired},
		// 落库子键是 request_id + "#a<rule_id>"：超长父键会在 VARCHAR(64) 上被截断，
		// 幂等键就此判不出来，所以入口按留白先拒。
		{"request_id 必须给子键留空间", newReq(func(r *rpc.SetRevenueRuleStateReq) {
			r.RequestId = strings.Repeat("r", model.MaxRequestIDBytes-autoArchiveSuffixBytes+1)
		}), model.ErrTextTooLong},
		{"expected_version 必填", newReq(func(r *rpc.SetRevenueRuleStateReq) { r.ExpectedVersion = 0 }),
			model.ErrVersionConflict},
		{"非法迁移在进事务前就被拒", newReq(func(r *rpc.SetRevenueRuleStateReq) {
			r.TargetState = rpc.RuleState(model.RuleStateDraft)
		}), model.ErrRuleStateTransition},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := l.SetRevenueRuleState(c.in)
			mustErrIs(t, err, c.target)
		})
	}
	if len(db.ruleLogs) != 0 || db.ruleByCode("R_VIP").State != model.RuleStateDraft {
		t.Fatal("校验类失败写下了数据")
	}

	// 留白上限（43 字节）本身必须放行，否则会把合法幂等键误杀。
	if _, err := l.SetRevenueRuleState(newReq(func(r *rpc.SetRevenueRuleStateReq) {
		r.RequestId = strings.Repeat("r", model.MaxRequestIDBytes-autoArchiveSuffixBytes)
	})); err != nil {
		t.Fatalf("留白上限内的 request_id 被误拒：%v", err)
	}
	if got := db.ruleByCode("R_VIP"); got.State != model.RuleStateActive || got.Version != 2 {
		t.Fatalf("生效未落库：%+v", got)
	}
}

// 锁定当前行为：SetRevenueRuleState 成功后用同一 request_id 重试，
// 会在进事务前被「同状态」判定拦下，回 ErrRuleStateTransition 而不是首次结果，
// 函数头承诺的 uniq_request_id 回放在这条路径上不可达。已进交付报告。
func TestSetRevenueRuleStateRetryAfterSuccessIsNotReplay(t *testing.T) {
	ctx, db := newTestSvc(t)
	cur := db.addRule(ruleSeed("R_VIP", model.SourceTypeVipWatch, model.RuleStateDraft))
	l := NewSetRevenueRuleStateLogic(context.Background(), ctx)
	req := &rpc.SetRevenueRuleStateReq{
		RuleId: cur.RuleId, TargetState: rpc.RuleState(model.RuleStateActive),
		ExpectedVersion: cur.Version, Operator: "ops-77", RequestId: "req-act", Reason: "复核通过",
	}
	first, err := l.SetRevenueRuleState(req)
	mustNoErr(t, err)
	if first.State != rpc.RuleState(model.RuleStateActive) {
		t.Fatalf("首调未生效：%+v", first)
	}

	_, err = l.SetRevenueRuleState(req)
	mustErrIs(t, err, model.ErrRuleStateTransition)
	if !strings.Contains(err.Error(), "已处于目标状态") {
		t.Fatalf("重试被误判成别的错：%v", err)
	}
	got := db.ruleByCode("R_VIP")
	if got.State != model.RuleStateActive || got.Version != 2 {
		t.Fatalf("重试二次改动了数据：%+v", got)
	}
	if len(db.ruleLogs) != 1 {
		t.Fatalf("重试多出一笔台账：%+v", db.ruleLogs)
	}
}
