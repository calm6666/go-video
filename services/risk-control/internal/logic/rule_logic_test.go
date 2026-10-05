package logic

// rule_logic_test.go 覆盖规则域两个入口：UpsertRule / ListRules。
//
// risk_rule 是「可解释裁决」的唯一规则源：决策日志只存 rule_id@version，
// 规则表就是回放当时裁决的唯一依据，所以断言集中在四条：
//  1. 版本语义必须与注释一致（repository.go:394 ruleSemanticsChanged 只看
//     动作/指标/比较符/阈值/窗口/裁决/优先级）：启停与换操作人不刷版本，
//     于是「运营重试同一份 payload」必须是幂等的（不产生第三个版本）。
//  2. 唯一键与列宽：uniq_name(name)（迁移 SQL :61）、name VARCHAR(128)（同文件 :47）。
//     本轮为此改生产代码一处：原实现把名字交给 sanitizeShortString **裁断**到 128 字节，
//     而 MySQL 的 VARCHAR 长度按字符计、Go 的 len 按字节计 ⇒ 中文长名会被切成非法 UTF-8，
//     两条只有第 43 个汉字之后才不同的规则还会折叠成同一个 name。现改为拒绝
//     （upsertrulelogic.go 的 maxRuleNameLen），用例见守卫表。
//  3. 写后读回真值：新建必须由 Insert 回填自增 rule_id（响应里的 ID 不是 0，且与库内行一致）；
//     更新路径以库内值覆盖 name（规则名不可变更），响应 version 取自写后的 FindOne。
//  4. 缓存失效范围：invalidateRuleCache 只删 rc:rl:<action>，动作 0（全域）要删全部 7 个动作键
//     （repository/facts.go:183-195），漏删就是「改了规则但裁决照旧」。
//
// state 语义说明：risk_rule 的 INSERT 显式绑定 state 列（model/rule.go:106-109），
// 所以 DDL 的 `state DEFAULT 1` 永不生效；新建 state=0 即建为停用，
// 这是 upsertrulelogic.go 注释里写明的刻意选择（宁可不生效也不静默上线），故只钉不修。
// 但**更新**路径同样透传 state：调用方不回传 state 就会顺手停用一条在跑的规则，
// 本轮只钉住现状并记候选缺口 #22。
//
// 替身与真实 SQL 的一处差异要如实说明：fakeRuleModel.List 把「先 COUNT 再 SELECT」合并成
// 一条 `rule.List:*` 轨迹，所以这里的「查询条数」断言只能钉住「logic 只发一次仓库调用」；
// total=0 时省掉第二条 SELECT 是真实实现的行为，见 model/rule.go:203-205。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

// ruleReq 是一条合法的评论频率规则（窗口 60 秒内 >=20 次即 BLOCK，优先级 100，已启用）。
func ruleReq(name string) *rpc.UpsertRuleReq {
	return &rpc.UpsertRuleReq{
		Name:          name,
		ActionType:    rpc.GuardedAction_ACTION_COMMENT,
		Metric:        rpc.Metric_METRIC_ACTION_COUNT,
		Op:            rpc.CompareOp_OP_GTE,
		Threshold:     20,
		WindowSeconds: 60,
		Decision:      rpc.Decision_DECISION_BLOCK,
		Priority:      100,
		State:         model.StateEnabled,
		Operator:      testOperator,
	}
}

func upsertRule(t *testing.T, st *store, in *rpc.UpsertRuleReq) (*rpc.UpsertRuleReply, error) {
	t.Helper()
	return NewUpsertRuleLogic(context.Background(), st.svcCtx).UpsertRule(in)
}

func listRules(t *testing.T, st *store, in *rpc.ListRulesReq) (*rpc.ListRulesReply, error) {
	t.Helper()
	return NewListRulesLogic(context.Background(), st.svcCtx).ListRules(in)
}

// ruleListOp 复现 fakeRuleModel.List 的调用轨迹（model/rule.go:180 的签名顺序）：
// rule.List:<动作>/<指标>/<状态>/<offset>/<limit>。
func ruleListOp(action int32, metric string, state int32, offset, limit int) string {
	return fmt.Sprintf("rule.List:%d/%s/%d/%d/%d", action, metric, state, offset, limit)
}

// delRulesOp 是规则缓存失效动作：rc:rl:<action>（repository/window.go:151）。
func delRulesOp(action int32) string {
	return fmt.Sprintf("redis.Del:rc:rl:%d", action)
}

// updateSeq 是更新路径的完整副作用序列：
// 前置读 old → UPDATE → 失效 old 动作缓存 → 失效 new 动作缓存 → 读回 fresh。
// 同一动作时两次失效落在同一个键上（冗余但幂等，见 repository.go:402-403）。
func updateSeq(ruleID int64, oldAction, newAction int32) []string {
	return []string{
		fmt.Sprintf("rule.FindOne:%d", ruleID),
		fmt.Sprintf("rule.Update:%d", ruleID),
		delRulesOp(oldAction),
		delRulesOp(newAction),
		fmt.Sprintf("rule.FindOne:%d", ruleID),
	}
}

func idsOfRules(reply *rpc.ListRulesReply) []int64 {
	out := make([]int64, 0, len(reply.Rules))
	for _, r := range reply.Rules {
		out = append(out, r.GetRuleId())
	}
	return out
}

// ruleRow 读回库内规则行（版本/名字/ctime 的「落库真值」断言用）。
func (st *store) ruleRow(ruleID int64) (model.RiskRule, bool) {
	row, ok := st.rule.rows[ruleID]
	if !ok {
		return model.RiskRule{}, false
	}
	return *row, true
}

// newRule 是布数据用的规则行（评估字段取一组固定值，只改列出的三个维度）。
func newRule(name string, action int32, metric string, state int32) model.RiskRule {
	return model.RiskRule{Name: name, ActionType: action, Metric: metric,
		Op: model.OpGTE, Threshold: 10, WindowSeconds: 60, Decision: model.DecisionBlock,
		Priority: 50, State: state, Version: 1, Operator: testOperator}
}

// --- UpsertRule ---

func TestUpsertRuleCreateBackfillsPrimaryKeyAndInvalidatesCache(t *testing.T) {
	st := newStore(t)
	before := time.Now().Unix()

	reply, err := upsertRule(t, st, ruleReq("rule-comment-flood"))
	wantNoErr(t, "新建规则", err)
	// 新建只发一条唯一性预检 + 一条 INSERT + 一次缓存失效：多一条说明有人写后又偷偷重读，少一条就是漏失效。
	wantOps(t, "新建序列", st.log.all(), []string{
		"rule.FindByName:rule-comment-flood",
		"rule.Insert:rule-comment-flood",
		delRulesOp(model.ActionComment),
	})
	wantEQ(t, "新建", "created", reply.Created, true)
	wantEQ(t, "新建", "version 固定为 1", reply.Rule.GetVersion(), int32(1))
	wantEQ(t, "新建", "state 透传", reply.Rule.GetState(), model.StateEnabled)
	wantEQ(t, "新建", "operator", reply.Rule.GetOperator(), testOperator)
	wantEQ(t, "新建", "threshold", reply.Rule.GetThreshold(), int64(20))
	wantEQ(t, "新建", "priority", reply.Rule.GetPriority(), int32(100))
	wantEQ(t, "新建", "metric 往返", reply.Rule.GetMetric(), rpc.Metric_METRIC_ACTION_COUNT)
	wantEQ(t, "新建", "action 往返", reply.Rule.GetActionType(), rpc.GuardedAction_ACTION_COMMENT)
	// 主键必须由 Insert 的 LastInsertId 回填（model/rule.go:120）：响应带 0 等于让调用方瞎猜。
	if reply.Rule.GetRuleId() == 0 {
		t.Fatalf("新建规则未回填自增主键：%+v", reply.Rule)
	}
	after := time.Now().Unix()
	wantRangeInt64(t, "新建", "ctime", reply.Rule.GetCtime(), before, after)
	wantEQ(t, "新建", "mtime==ctime", reply.Rule.GetMtime(), reply.Rule.GetCtime())

	row, ok := st.ruleRow(reply.Rule.GetRuleId())
	if !ok {
		t.Fatalf("库内没有新建的规则行，rows=%d", len(st.rule.rows))
	}
	wantEQ(t, "库内行", "主键与响应一致", row.RuleID, reply.Rule.GetRuleId())
	wantEQ(t, "库内行", "version", row.Version, int32(1))
	wantEQ(t, "库内行", "name", row.Name, "rule-comment-flood")
	wantEQ(t, "库内行", "rows 总数", len(st.rule.rows), 1)

	// 名字规范化：去掉首尾空白与控制符，但**不**裁断长度（超长由守卫拒绝，见守卫表）。
	got, err := upsertRule(t, st, ruleReq("  rule-padded\t"))
	wantNoErr(t, "带空白的规则名", err)
	wantOps(t, "带空白的规则名", st.log.opsFrom(3), []string{
		"rule.FindByName:rule-padded", "rule.Insert:rule-padded", delRulesOp(model.ActionComment),
	})
	wantEQ(t, "带空白的规则名", "name 去掉空白后入库", got.Rule.GetName(), "rule-padded")

	// 唯一键冲突由预检拦下：只发预检，绝不发 INSERT（1062 只是并发竞态下的兜底）。
	dup, err := upsertRule(t, st, ruleReq("rule-padded"))
	wantErrIs(t, "重名", err, model.ErrRuleNameDuplicated)
	if !strings.Contains(err.Error(), "rule-padded") {
		t.Errorf("重名错误未带规则名：%v", err)
	}
	wantOps(t, "重名序列", st.log.opsFrom(6), []string{"rule.FindByName:rule-padded"})
	wantEQ(t, "重名爆炸半径", "rows 总数", len(st.rule.rows), 2)
	if dup != nil {
		t.Errorf("重名请求仍返回了响应体：%+v", dup)
	}

	// 全域规则（action 0）会影响所有动作的缓存：必须逐个失效 7 个键，
	// 漏一个就是「改了全域规则但该动作照旧用旧规则集裁决」。
	st2 := newStore(t)
	st2.redis.warm("rc:rl:3", "stale-rule-set")
	all := ruleReq("rule-global")
	all.ActionType = rpc.GuardedAction_ACTION_UNSPECIFIED
	created, err := upsertRule(t, st2, all)
	wantNoErr(t, "全域规则", err)
	wantOps(t, "全域规则序列", st2.log.all(), []string{
		"rule.FindByName:rule-global", "rule.Insert:rule-global",
		delRulesOp(1), delRulesOp(2), delRulesOp(3), delRulesOp(4),
		delRulesOp(5), delRulesOp(6), delRulesOp(7),
	})
	wantEQ(t, "全域规则", "action_type 落 0", created.Rule.GetActionType(), rpc.GuardedAction_ACTION_UNSPECIFIED)
	if _, ok := st2.redis.kv["rc:rl:3"]; ok {
		t.Errorf("全域规则未失效动作 3 的规则缓存，旧规则集仍会命中")
	}
}

// TestUpsertRuleVersionBumpsOnlyOnEvaluatedFields 钉住版本语义这条可解释性的命脉：
// 决策日志里的 rule_id@version 必须能唯一定位一份评估条件。
func TestUpsertRuleVersionBumpsOnlyOnEvaluatedFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(in *rpc.UpsertRuleReq)
		wantVer int32
	}{
		{"原样重试不刷版本", func(in *rpc.UpsertRuleReq) {}, 1},
		{"只改启停", func(in *rpc.UpsertRuleReq) { in.State = model.StateDisabled }, 1},
		{"只换操作人", func(in *rpc.UpsertRuleReq) { in.Operator = 8802 }, 1},
		{"改阈值", func(in *rpc.UpsertRuleReq) { in.Threshold = 21 }, 2},
		{"改比较符", func(in *rpc.UpsertRuleReq) { in.Op = rpc.CompareOp_OP_GT }, 2},
		{"改窗口", func(in *rpc.UpsertRuleReq) { in.WindowSeconds = 300 }, 2},
		{"改指标", func(in *rpc.UpsertRuleReq) { in.Metric = rpc.Metric_METRIC_IP_ACTION_COUNT }, 2},
		{"改裁决", func(in *rpc.UpsertRuleReq) { in.Decision = rpc.Decision_DECISION_REVIEW }, 2},
		// 优先级决定 hit_rule_ids 的展示顺序，属于「影响评估结果」的字段。
		{"改优先级", func(in *rpc.UpsertRuleReq) { in.Priority = 99 }, 2},
		{"改适用动作", func(in *rpc.UpsertRuleReq) { in.ActionType = rpc.GuardedAction_ACTION_LOGIN }, 2},
	}
	for _, tc := range cases {
		st := newStore(t)
		first, err := upsertRule(t, st, ruleReq("rule-version"))
		wantNoErr(t, tc.name+"：新建", err)
		id := first.Rule.GetRuleId()

		upd := ruleReq("rule-version")
		upd.RuleId = id
		tc.mutate(upd)
		before := st.log.snapshot()
		second, err := upsertRule(t, st, upd)
		wantNoErr(t, tc.name+"：更新", err)
		wantEQ(t, tc.name, "created（更新不是新建）", second.Created, false)
		wantEQ(t, tc.name, "version", second.Rule.GetVersion(), tc.wantVer)
		wantEQ(t, tc.name, "主键不变", second.Rule.GetRuleId(), id)
		wantEQ(t, tc.name, "rows 总数（不新建行）", len(st.rule.rows), 1)
		// 响应必须来自写后读回（repository.go:404），不能是调用方自报的值。
		row, ok := st.ruleRow(id)
		if !ok {
			t.Fatalf("%s：更新后库内规则行消失", tc.name)
		}
		wantEQ(t, tc.name, "库内 version 与响应一致", row.Version, second.Rule.GetVersion())
		wantEQ(t, tc.name, "库内 threshold", row.Threshold, upd.Threshold)
		wantEQ(t, tc.name, "库内 state", row.State, upd.State)
		wantOps(t, tc.name+"：更新序列", st.log.opsFrom(before), updateSeq(id, model.ActionComment, int32(upd.ActionType)))
	}

	// 动作迁移必须同时失效 old 与 new 两个键，否则旧动作留下脏缓存。
	mig := newStore(t)
	m, err := upsertRule(t, mig, ruleReq("rule-migrate"))
	wantNoErr(t, "迁移前新建", err)
	mig.redis.warm("rc:rl:2", "stale-comment-rules")
	mig.redis.warm("rc:rl:5", "stale-login-rules")
	up := ruleReq("rule-migrate")
	up.RuleId = m.Rule.GetRuleId()
	up.ActionType = rpc.GuardedAction_ACTION_LOGIN
	if _, err := upsertRule(t, mig, up); err != nil {
		t.Fatalf("动作迁移失败：%v", err)
	}
	for _, k := range []string{"rc:rl:2", "rc:rl:5"} {
		if _, ok := mig.redis.kv[k]; ok {
			t.Errorf("动作迁移后 %s 仍在缓存里，旧规则集还会被命中", k)
		}
	}
}

func TestUpsertRuleUpdateKeepsNameAndRetryStaysIdempotent(t *testing.T) {
	st := newStore(t)
	first, err := upsertRule(t, st, ruleReq("rule-login-abuse"))
	wantNoErr(t, "新建", err)
	id := first.Rule.GetRuleId()
	ctime := first.Rule.GetCtime()

	// 更新时改名：库里必须保持原名，且更新路径不参与唯一性检查（不发 FindByName）。
	upd := ruleReq("rule-名字改了吧")
	upd.RuleId = id
	upd.Threshold = 30
	upd.IdempotencyKey = "batch-1"
	before := st.log.snapshot()
	second, err := upsertRule(t, st, upd)
	wantNoErr(t, "改名尝试", err)
	wantEQ(t, "改名", "name 不可变更", second.Rule.GetName(), "rule-login-abuse")
	wantEQ(t, "改名", "ctime 保留", second.Rule.GetCtime(), ctime)
	wantEQ(t, "改名", "version 因阈值 +1", second.Rule.GetVersion(), int32(2))
	wantOps(t, "改名序列", st.log.opsFrom(before), updateSeq(id, model.ActionComment, model.ActionComment))
	row, _ := st.ruleRow(id)
	wantEQ(t, "改名落库", "name", row.Name, "rule-login-abuse")
	wantEQ(t, "改名落库", "uniq_name 索引键未漂移", st.rule.names["rule-login-abuse"], id)

	// 运营重试同一份 payload（幂等键只用于日志关联，不参与版本判定）：语义已与库内一致 ⇒ 不再刷版本。
	retry := upd
	retry.IdempotencyKey = "batch-2"
	third, err := upsertRule(t, st, retry)
	wantNoErr(t, "重试", err)
	wantEQ(t, "重试", "version 停在 2（幂等）", third.Rule.GetVersion(), int32(2))
	// 爆炸半径：整张表只有一条规则、两次 UPDATE，没有第三个版本行。
	wantEQ(t, "重试爆炸半径", "rows 总数", len(st.rule.rows), 1)
	wantEQ(t, "重试爆炸半径", "自增序号", st.rule.next, int64(1))
	wantEQ(t, "重试爆炸半径", "UPDATE 次数", st.log.countPrefix("rule.Update"), 2)
	wantEQ(t, "重试爆炸半径", "INSERT 次数", st.log.countPrefix("rule.Insert"), 1)
	// 只出现在新建路径：更新不查名字，改名也不占用唯一键。
	wantEQ(t, "重试爆炸半径", "FindByName 次数", st.log.countPrefix("rule.FindByName"), 1)
	if _, ok := st.rule.names["rule-名字改了吧"]; ok {
		t.Errorf("改名尝试占住了唯一键：%v", st.rule.names)
	}
}

func TestUpsertRuleUnknownIDAndDuplicatedNameLeaveNoTrace(t *testing.T) {
	// 不存在的 rule_id：只有一条前置读，UPDATE 与缓存失效都不能发生。
	st := newStore(t)
	upd := ruleReq("rule-ghost")
	upd.RuleId = 999
	_, err := upsertRule(t, st, upd)
	wantErrIs(t, "更新不存在的规则", err, model.ErrRuleNotFound)
	wantOps(t, "幽灵 ID 序列", st.log.all(), []string{"rule.FindOne:999"})
	wantEQ(t, "幽灵 ID 爆炸半径", "rows 总数", len(st.rule.rows), 0)
	if n := st.log.countPrefix("redis."); n != 0 {
		t.Errorf("失败更新触碰 Redis %d 次：%v", n, st.log.all())
	}

	// 新建撞名：预检拦下，DB 侧 1062 不该成为常规路径。
	if _, err := upsertRule(t, st, ruleReq("rule-x")); err != nil {
		t.Fatalf("首次新建失败：%v", err)
	}
	before := st.log.snapshot()
	if _, err := upsertRule(t, st, ruleReq("rule-x")); err == nil {
		t.Fatalf("重名新建未被拒绝")
	} else {
		wantErrIs(t, "重名新建", err, model.ErrRuleNameDuplicated)
	}
	wantOps(t, "重名新建序列", st.log.opsFrom(before), []string{"rule.FindByName:rule-x"})
	wantEQ(t, "重名爆炸半径", "rows 总数", len(st.rule.rows), 1)
	wantEQ(t, "重名爆炸半径", "自增序号（没有白烧一个 ID）", st.rule.next, int64(1))
	// 说明：预检与 INSERT 之间竞态窗口的兜底是 DB 的 1062 → ErrRuleNameDuplicated
	// （model/rule.go:111-112 的 isDuplicateEntry）。它需要真实唯一索引才能触发，
	// 本包替身里 names 映射本身就是那个索引，无法表达「预检漏看而索引拦住」的状态 ⇒ 记入未覆盖清单。
}

func TestUpsertRuleGuardsRunBeforeAnyDependency(t *testing.T) {
	longASCII := strings.Repeat("x", maxRuleNameLen+1)
	longCJK := strings.Repeat("风", maxRuleNameLen/3+1) // 129 字节 = 43 个汉字：裁断会切断多字节序列
	cases := []struct {
		name   string
		mutate func(in *rpc.UpsertRuleReq)
		want   error
		wantIn string
	}{
		{"无操作人", func(in *rpc.UpsertRuleReq) { in.Operator = 0 }, model.ErrOperatorRequired, "operator required"},
		{"操作人为负", func(in *rpc.UpsertRuleReq) { in.Operator = -1 }, model.ErrOperatorRequired, "operator required"},
		{"空名", func(in *rpc.UpsertRuleReq) { in.Name = "" }, model.ErrInvalidRule, "name required"},
		{"纯空白名", func(in *rpc.UpsertRuleReq) { in.Name = "   " }, model.ErrInvalidRule, "name required"},
		{"超长 ASCII 名", func(in *rpc.UpsertRuleReq) { in.Name = longASCII }, model.ErrInvalidRule, "name too long"},
		{"超长中文名（裁断即损坏）", func(in *rpc.UpsertRuleReq) { in.Name = longCJK }, model.ErrInvalidRule, "name too long"},
		{"未实现指标", func(in *rpc.UpsertRuleReq) { in.Metric = rpc.Metric_METRIC_UNSPECIFIED }, model.ErrInvalidRule, "not implemented"},
		{"动作越界", func(in *rpc.UpsertRuleReq) { in.ActionType = rpc.GuardedAction(9) }, model.ErrInvalidTarget, "invalid action or target"},
		{"窗口为 0", func(in *rpc.UpsertRuleReq) { in.WindowSeconds = 0 }, model.ErrInvalidRule, "window_seconds=0"},
		{"窗口超一天", func(in *rpc.UpsertRuleReq) { in.WindowSeconds = 86401 }, model.ErrInvalidRule, "window_seconds=86401"},
		{"负阈值", func(in *rpc.UpsertRuleReq) { in.Threshold = -1 }, model.ErrInvalidRule, "threshold must be >= 0"},
		{"LT 配 0 阈值（恒不命中）", func(in *rpc.UpsertRuleReq) {
			in.Op = rpc.CompareOp_OP_LT
			in.Threshold = 0
		}, model.ErrInvalidRule, "threshold must be > 0"},
		{"比较符越界", func(in *rpc.UpsertRuleReq) { in.Op = rpc.CompareOp(9) }, model.ErrInvalidRule, "op=9"},
		{"裁决写成 ALLOW", func(in *rpc.UpsertRuleReq) { in.Decision = rpc.Decision_DECISION_ALLOW }, model.ErrInvalidRule, "decision=1"},
		{"state 越界", func(in *rpc.UpsertRuleReq) { in.State = 2 }, model.ErrInvalidRule, "state=2"},
	}
	for _, tc := range cases {
		st := newStore(t)
		in := ruleReq("rule-guard")
		tc.mutate(in)
		reply, err := upsertRule(t, st, in)
		wantErrIs(t, tc.name, err, tc.want)
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s：错误文本 %q 未包含 %q", tc.name, err.Error(), tc.wantIn)
		}
		// 守卫必须先于任何依赖调用：规则表是裁决的唯一依据，半成品一行都不能有。
		wantNoCall(t, tc.name, st, 0)
		wantEQ(t, tc.name, "rows 总数", len(st.rule.rows), 0)
		if reply != nil {
			t.Errorf("%s：仍返回响应体 %+v", tc.name, reply)
		}
	}

	// 边界：恰好 128 字节的名字必须放行（拒绝只针对超限）。
	ok := newStore(t)
	got, err := upsertRule(t, ok, ruleReq(strings.Repeat("y", maxRuleNameLen)))
	wantNoErr(t, "列宽边界", err)
	wantEQ(t, "列宽边界", "name 长度", len(got.Rule.GetName()), maxRuleNameLen)

	// 新建不传 state（proto3 零值）⇒ 建为停用：DDL 的 `state DEFAULT 1` 因 INSERT 显式绑定列而失效。
	// 这是 upsertrulelogic.go 注释里写明的取舍（宁可不生效也不静默上线），只钉不改。
	off := newStore(t)
	noState := ruleReq("rule-no-state")
	noState.State = 0
	replied, err := upsertRule(t, off, noState)
	wantNoErr(t, "不传 state 的新建", err)
	wantEQ(t, "新建 state 现状", "落库 state（不是 DDL 的 1）", replied.Rule.GetState(), model.StateDisabled)
	wantEQ(t, "新建 state 现状", "created", replied.Created, true)

	// 同一颗雷在**更新**路径上更疼：只想改阈值、没回传 state 的调用方会顺手停用一条在跑的规则，
	// 而 state 不在 ruleSemanticsChanged 里，事后从 rule_id@version 也回看不出来。
	// 这里钉住现状 = 候选缺口 #22。
	ran := newStore(t)
	one, err := upsertRule(t, ran, ruleReq("rule-running"))
	wantNoErr(t, "在跑的规则", err)
	silent := ruleReq("rule-running")
	silent.RuleId = one.Rule.GetRuleId()
	silent.State = 0 // 调用方只更新了阈值，忘了回传 state
	silent.Threshold = 25
	two, err := upsertRule(t, ran, silent)
	wantNoErr(t, "漏传 state 的更新", err)
	wantEQ(t, "候选缺口 #22 现状", "在跑规则被停用", two.Rule.GetState(), model.StateDisabled)
	// 这个 +1 来自阈值变化；停用本身不贡献版本。
	wantEQ(t, "候选缺口 #22 现状", "停用不产生新版本", two.Rule.GetVersion(), int32(2))
	offRow, _ := ran.ruleRow(one.Rule.GetRuleId())
	// 后果面：停用不刷版本 ⇒ 回放时拿 rule_id@version 无法分辨「当时是启用还是停用」，
	// 只能额外依赖 mtime 猜。库里现在停用的是 version 3？不是 —— 仍是 2（只有阈值 +1）。
	wantEQ(t, "候选缺口 #22 现状", "库内 state", offRow.State, model.StateDisabled)
	wantEQ(t, "候选缺口 #22 现状", "库内 version 只由阈值 +1", offRow.Version, int32(2))
}

func TestUpsertRuleDependencyFailurePropagates(t *testing.T) {
	// 预检失败：不能伪装成「重名」。
	st := newStore(t)
	boom := errors.New("boom-rule-findbyname")
	st.rule.failWith("FindByName", boom)
	reply, err := upsertRule(t, st, ruleReq("rule-boom"))
	wantErrIs(t, "预检故障", err, boom)
	for _, disguised := range []error{model.ErrRuleNameDuplicated, model.ErrInvalidRule, model.ErrOperatorRequired} {
		if errors.Is(err, disguised) {
			t.Errorf("预检故障被伪装成 %v：%v", disguised, err)
		}
	}
	if reply != nil {
		t.Errorf("预检故障：仍返回响应体 %+v", reply)
	}
	wantOps(t, "预检故障序列", st.log.all(), []string{"rule.FindByName:rule-boom"})
	wantEQ(t, "预检故障爆炸半径", "rows 总数", len(st.rule.rows), 0)

	// INSERT 失败：规则没落库，也不该失效缓存（无变化可广播）。
	st.rule.clear("FindByName")
	insertBoom := errors.New("boom-rule-insert")
	st.rule.failWith("Insert", insertBoom)
	_, err = upsertRule(t, st, ruleReq("rule-boom"))
	wantErrIs(t, "INSERT 故障", err, insertBoom)
	wantOps(t, "INSERT 故障序列", st.log.opsFrom(1), []string{"rule.FindByName:rule-boom", "rule.Insert:rule-boom"})
	wantEQ(t, "INSERT 故障爆炸半径", "rows 总数", len(st.rule.rows), 0)
	if n := st.log.countPrefix("redis."); n != 0 {
		t.Errorf("INSERT 失败却失效了缓存 %d 次：%v", n, st.log.all())
	}

	// 故障恢复后重试：仍然只建一条 v1（幂等靠名字预检，而不是靠重试计数）。
	st.rule.clear("Insert")
	got, err := upsertRule(t, st, ruleReq("rule-boom"))
	wantNoErr(t, "INSERT 故障恢复后重试", err)
	wantEQ(t, "故障恢复后重试", "created", got.Created, true)
	wantEQ(t, "故障恢复后重试", "version", got.Rule.GetVersion(), int32(1))
	wantEQ(t, "故障恢复后重试", "rows 总数", len(st.rule.rows), 1)

	// 更新路径：UPDATE 已落库、写后读回失败 ⇒ 错误原样上抛，但库里确实已经变了。
	readBoom := errors.New("boom-rule-findone")
	st.rule.failFrom("FindOne", 2, readBoom)
	upd := ruleReq("rule-boom")
	upd.RuleId = got.Rule.GetRuleId()
	upd.Threshold = 40
	_, err = upsertRule(t, st, upd)
	wantErrIs(t, "读回故障", err, readBoom)
	row, ok := st.ruleRow(got.Rule.GetRuleId())
	if !ok {
		t.Fatalf("读回故障后规则行消失")
	}
	wantEQ(t, "读回故障爆炸半径", "阈值已落库", row.Threshold, int64(40))
	wantEQ(t, "读回故障爆炸半径", "版本已 +1", row.Version, int32(2))
	wantEQ(t, "读回故障爆炸半径", "UPDATE 次数", st.log.countPrefix("rule.Update"), 1)

	// 清故障后重试同一份 payload：库里已是新语义 ⇒ 版本停在 2，不会长出第三个版本。
	st.rule.clear("FindOne")
	retry, err := upsertRule(t, st, upd)
	wantNoErr(t, "读回故障恢复后重试", err)
	wantEQ(t, "读回故障恢复后重试", "version 不翻倍", retry.Rule.GetVersion(), int32(2))
	wantEQ(t, "读回故障恢复后重试", "rows 总数", len(st.rule.rows), 1)
	wantEQ(t, "读回故障恢复后重试", "UPDATE 总次数", st.log.countPrefix("rule.Update"), 2)
}

// --- ListRules ---

func TestListRulesFiltersLandInSQLParameters(t *testing.T) {
	st := newStore(t)
	comment := st.seedRule(newRule("r1", model.ActionComment, model.MetricActionCount, model.StateEnabled))
	global := st.seedRule(newRule("r2", model.ActionAll, model.MetricDeviceActionCount, model.StateEnabled))
	loginOff := st.seedRule(newRule("r3", model.ActionLogin, model.MetricActionCount, model.StateDisabled))
	ip := st.seedRule(newRule("r4", model.ActionComment, model.MetricIpActionCount, model.StateEnabled))
	wantInt64s(t, "布数据", "ids", []int64{comment, global, loginOff, ip}, []int64{1, 2, 3, 4})

	// 不过滤：四条都在（state=-1 不进 WHERE），rule_id DESC。
	all, err := listRules(t, st, &rpc.ListRulesReq{State: -1})
	wantNoErr(t, "不过滤", err)
	wantOps(t, "不过滤序列", st.log.all(), []string{ruleListOp(0, "", -1, 0, 20)})
	wantEQ(t, "不过滤", "total", all.Total, int32(4))
	wantInt64s(t, "不过滤", "ids", idsOfRules(all), []int64{4, 3, 2, 1})

	// 动作过滤必须含全域规则：`action_type IN (0, action)`（model/rule.go:184）。
	// 若实现只写 action_type = ?，全域规则在运营列表里就会凭空消失，而它明明在参与裁决。
	byComment, err := listRules(t, st, &rpc.ListRulesReq{ActionType: rpc.GuardedAction_ACTION_COMMENT, State: -1})
	wantNoErr(t, "按动作", err)
	wantOps(t, "按动作序列", st.log.opsFrom(1), []string{ruleListOp(model.ActionComment, "", -1, 0, 20)})
	wantEQ(t, "按动作", "total（含全域 r2）", byComment.Total, int32(3))
	wantInt64s(t, "按动作", "ids", idsOfRules(byComment), []int64{4, 2, 1})

	// 指标过滤：指标名（不是枚举号）进 SQL 参数。
	onlyIP, err := listRules(t, st, &rpc.ListRulesReq{Metric: rpc.Metric_METRIC_IP_ACTION_COUNT, State: -1})
	wantNoErr(t, "按指标", err)
	wantOps(t, "按指标序列", st.log.opsFrom(2), []string{ruleListOp(0, model.MetricIpActionCount, -1, 0, 20)})
	wantInt64s(t, "按指标", "ids", idsOfRules(onlyIP), []int64{4})

	// state=0/1 是精确匹配，不是「不过滤」；停用规则仍要能查到。
	stopped, err := listRules(t, st, &rpc.ListRulesReq{State: model.StateDisabled})
	wantNoErr(t, "按停用", err)
	wantOps(t, "按停用序列", st.log.opsFrom(3), []string{ruleListOp(0, "", model.StateDisabled, 0, 20)})
	wantEQ(t, "按停用", "total", stopped.Total, int32(1))
	wantInt64s(t, "按停用", "ids", idsOfRules(stopped), []int64{3})

	// 组合过滤：动作 + 指标 + 状态三条件同时进参数，total 与列表同源。
	combo, err := listRules(t, st, &rpc.ListRulesReq{
		ActionType: rpc.GuardedAction_ACTION_COMMENT, Metric: rpc.Metric_METRIC_ACTION_COUNT, State: model.StateEnabled,
	})
	wantNoErr(t, "三条件", err)
	wantOps(t, "三条件序列", st.log.opsFrom(4), []string{ruleListOp(model.ActionComment, model.MetricActionCount, model.StateEnabled, 0, 20)})
	wantEQ(t, "三条件", "total", combo.Total, int32(1))
	wantInt64s(t, "三条件", "ids", idsOfRules(combo), []int64{1})
	// 投影字段逐个核对：运营看到的就是库里的那一版规则。
	if len(combo.Rules) != 1 {
		t.Fatalf("三条件结果数 %d", len(combo.Rules))
	}
	got := combo.Rules[0]
	wantEQ(t, "投影", "name", got.GetName(), "r1")
	wantEQ(t, "投影", "version", got.GetVersion(), int32(1))
	wantEQ(t, "投影", "priority", got.GetPriority(), int32(50))
	wantEQ(t, "投影", "threshold", got.GetThreshold(), int64(10))
	wantEQ(t, "投影", "op", got.GetOp(), rpc.CompareOp_OP_GTE)
	wantEQ(t, "投影", "decision", got.GetDecision(), rpc.Decision_DECISION_BLOCK)
	wantEQ(t, "投影", "window_seconds", got.GetWindowSeconds(), int64(60))
	wantEQ(t, "投影", "operator", got.GetOperator(), testOperator)
}

func TestListRulesSeesWhatUpsertRuleWrote(t *testing.T) {
	// 「写完立刻读」是运营后台的呈现口径：响应里的 id/version/name 必须与列表读到的一致。
	st := newStore(t)
	written, err := upsertRule(t, st, ruleReq("rule-write-then-read"))
	wantNoErr(t, "写入", err)
	listed, err := listRules(t, st, &rpc.ListRulesReq{State: -1})
	wantNoErr(t, "读回", err)
	wantEQ(t, "写后读回", "total", listed.Total, int32(1))
	if len(listed.Rules) != 1 {
		t.Fatalf("写后读回结果数 %d", len(listed.Rules))
	}
	wantEQ(t, "写后读回", "rule_id", listed.Rules[0].GetRuleId(), written.Rule.GetRuleId())
	wantEQ(t, "写后读回", "version", listed.Rules[0].GetVersion(), written.Rule.GetVersion())
	wantEQ(t, "写后读回", "name", listed.Rules[0].GetName(), written.Rule.GetName())
	wantEQ(t, "写后读回", "state", listed.Rules[0].GetState(), written.Rule.GetState())
	wantEQ(t, "写后读回", "mtime", listed.Rules[0].GetMtime(), written.Rule.GetMtime())

	// 改一次（阈值 ⇒ v2）后列表必须立刻看到新版本，不能停在旧行或旧缓存上。
	upd := ruleReq("rule-write-then-read")
	upd.RuleId = written.Rule.GetRuleId()
	upd.Threshold = 99
	second, err := upsertRule(t, st, upd)
	wantNoErr(t, "更新", err)
	wantEQ(t, "写后读回", "新版本", second.Rule.GetVersion(), int32(2))
	again, err := listRules(t, st, &rpc.ListRulesReq{ActionType: rpc.GuardedAction_ACTION_COMMENT, State: -1})
	wantNoErr(t, "更新后读回", err)
	wantEQ(t, "更新后读回", "total（仍是一条）", again.Total, int32(1))
	wantEQ(t, "更新后读回", "version", again.Rules[0].GetVersion(), int32(2))
	wantEQ(t, "更新后读回", "threshold", again.Rules[0].GetThreshold(), int64(99))

	// 停用态也要在列表里可见（停用的规则保留行便于回放，不能从运营视图消失）。
	off := ruleReq("rule-write-then-read")
	off.RuleId = written.Rule.GetRuleId()
	off.State = model.StateDisabled
	if _, err := upsertRule(t, st, off); err != nil {
		t.Fatalf("停用失败：%v", err)
	}
	offSeen, err := listRules(t, st, &rpc.ListRulesReq{State: model.StateDisabled})
	wantNoErr(t, "停用后可见", err)
	wantInt64s(t, "停用后可见", "ids", idsOfRules(offSeen), []int64{second.Rule.GetRuleId()})
	// 停用不刷版本：这一版的 +1 只可能来自阈值 99⇒20。
	offRow, _ := st.ruleRow(written.Rule.GetRuleId())
	wantEQ(t, "停用后可见", "version 由阈值变化决定", offRow.Version, int32(3))
}

func TestListRulesPagingContractAndEmptyResult(t *testing.T) {
	st := newStore(t)
	for i := 1; i <= 5; i++ {
		st.seedRule(newRule(fmt.Sprintf("r%d", i), model.ActionComment, model.MetricActionCount, model.StateEnabled))
	}
	cases := []struct {
		name      string
		pn, ps    int32
		metric    rpc.Metric
		wantOff   int
		wantPn    int32
		wantPs    int32
		wantTotal int32
		wantFirst int64
		wantLen   int
	}{
		{"零值走默认", 0, 0, rpc.Metric_METRIC_UNSPECIFIED, 0, 1, 20, 5, 5, 5},
		{"负数走默认", -5, -1, rpc.Metric_METRIC_UNSPECIFIED, 0, 1, 20, 5, 5, 5},
		{"上限 50", 1, 999, rpc.Metric_METRIC_UNSPECIFIED, 0, 1, 50, 5, 5, 5},
		{"恰为上限", 1, int32(maxPageSize), rpc.Metric_METRIC_UNSPECIFIED, 0, 1, int32(maxPageSize), 5, 5, 5},
		{"第二页", 2, 2, rpc.Metric_METRIC_UNSPECIFIED, 2, 2, 2, 5, 3, 2},
		{"越界页", 9, 2, rpc.Metric_METRIC_UNSPECIFIED, 16, 9, 2, 5, 0, 0},
		{"无命中", 1, 20, rpc.Metric_METRIC_DEVICE_MID_COUNT, 0, 1, 20, 0, 0, 0},
	}
	for _, tc := range cases {
		before := st.log.snapshot()
		in := &rpc.ListRulesReq{Pn: tc.pn, Ps: tc.ps, State: -1, Metric: tc.metric}
		reply, err := listRules(t, st, in)
		wantNoErr(t, tc.name, err)
		wantEQ(t, tc.name, "total", reply.Total, tc.wantTotal)
		wantEQ(t, tc.name, "len", len(reply.Rules), tc.wantLen)
		wantEQ(t, tc.name, "pn 回显归一后的页码", reply.Pn, tc.wantPn)
		wantEQ(t, tc.name, "ps 回显归一后的页长", reply.Ps, tc.wantPs)
		if tc.wantLen > 0 {
			wantEQ(t, tc.name, "首条（rule_id DESC）", reply.Rules[0].GetRuleId(), tc.wantFirst)
		}
		// 分页归一发生在 logic 与 repository 两级（logic 上限 50、仓库上限 100），
		// 落到 SQL 的只能是归一后的 offset/limit；这里钉一次仓库调用及其参数。
		ops := st.log.opsFrom(before)
		wantEQ(t, tc.name, "仓库调用次数", len(ops), 1)
		wantEQ(t, tc.name, "分页参数进 SQL", ops[0],
			ruleListOp(0, metricFromProto(tc.metric), -1, tc.wantOff, int(tc.wantPs)))
	}
}

func TestListRulesGuardsAndFailureSurface(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *rpc.ListRulesReq
		want error
	}{
		{"动作越界", &rpc.ListRulesReq{ActionType: rpc.GuardedAction(9), State: -1}, model.ErrInvalidTarget},
		{"未知指标", &rpc.ListRulesReq{Metric: rpc.Metric(99), State: -1}, model.ErrInvalidTarget},
		{"state 越界", &rpc.ListRulesReq{State: 2}, model.ErrInvalidTarget},
		{"state 负值", &rpc.ListRulesReq{State: -2}, model.ErrInvalidTarget},
	} {
		st := newStore(t)
		st.seedRule(newRule("r1", model.ActionComment, model.MetricActionCount, model.StateEnabled))
		_, err := listRules(t, st, tc.in)
		wantErrIs(t, tc.name, err, tc.want)
		wantNoCall(t, tc.name, st, 0)
	}

	// 查询故障原样上抛：返回空列表会被运营误读成「规则没生效」。
	st := newStore(t)
	st.seedRule(newRule("r1", model.ActionComment, model.MetricActionCount, model.StateEnabled))
	boom := errors.New("boom-rule-list")
	st.rule.failWith("List", boom)
	reply, err := listRules(t, st, &rpc.ListRulesReq{State: -1})
	wantErrIs(t, "列表故障", err, boom)
	if reply != nil {
		t.Errorf("列表故障：仍返回响应体 %+v", reply)
	}
	wantOps(t, "列表故障序列", st.log.all(), []string{ruleListOp(0, "", -1, 0, 20)})

	// 列表接口是只读的：不得顺手写库或动缓存（读接口刷缓存会让「看到的」与「库里的」脱节）。
	wantEQ(t, "只读保证", "写操作次数", st.log.countPrefix("rule.Insert")+st.log.countPrefix("rule.Update"), 0)
	wantEQ(t, "只读保证", "Redis 次数", st.log.countPrefix("redis."), 0)
}
