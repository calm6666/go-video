// 本文件覆盖写路径的「失败面」：可选依赖缺席时怎么降级、身份与幂等键缺失时怎么拒、
// 状态机不允许的跳转怎么挡。三组风险的共同点是——**做错了不会立刻有人发现**：
//
//  1. 审计客户端没配 / 调不通：业务写入仍然成功（这是契约），但缺口必须**带着
//     event_id 出现在 Error 日志里**，否则补偿任务无从下手，「审计缺口」就变成了
//     一句口头承诺。回参 audit_entry_id=0 只证明「没写成」，证明不了「找得回来」。
//     契约原文（rpc/opsconfig.proto 文件头）：「审计写失败**不回滚**业务写入（配置
//     发布已成功），但必须打 Error 级日志并把 audit_entry_id 留 0，由后续补偿任务重投；
//     这样审计缺口可见而不是被静默吞掉。」
//  2. Redis 投影不可用：缓存是「可整域重建的只读投影」（internal/svc 的 CacheKV 注释），
//     所以写路径不能被它挡住，也不能因为它在而少写库。
//  3. 身份/幂等键/原因缺失、乐观锁基线过期、非法状态跳转：必须在**碰到存储之前**拒掉，
//     并且零副作用。半拒半写是最难复现的一类现场。
//
// 因此这里的每条断言都落在探针上：写了哪些行、开了几个事务、动了哪些缓存键、
// 审计收到几个 event_id、日志里有没有那句可补偿的 event_id。
//
// 契约出处：
//   - rpc/opsconfig.proto:270「写接口必须带 request_id（幂等键）与 operator_id」；
//   - rpc/opsconfig.proto:39-42 审计分工与「留 0 + Error 日志 + 补偿重投」；
//   - rpc/opsconfig.proto:327 SetRolloutRuleState「不物理删除，保留放量决策证据」；
//   - model/errors.go 每条哨兵的语义注释（幂等/乐观锁/列宽/状态机）。

package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

// --- 写接口探针：一条 = 一个 RPC，setup 造出「这次调用本可以成功」的现场 ---

type writeProbe struct {
	// id 进 request_id，必须短且唯一：event_id 断言靠它对齐。
	id     string
	name   string
	action string // appendAudit 的动作名，决定 event_id 的第三段
	setup  func(t *testing.T, e *testEnv)
	// call 发这次写请求，回「响应里的 audit_entry_id」。
	call func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error)
	// landed 证明业务写入真的落库了（降级不等于没做）。
	landed func(t *testing.T, e *testEnv, before effectCounts)
}

// writeProbes 每次回一份新表：探针闭包不持有跨用例状态，
// 子测试并行与否都不会互相污染。
func writeProbes() []writeProbe {
	return []writeProbe{
		{
			id: "pub", name: "PublishConfig", action: "publish",
			setup: func(t *testing.T, e *testEnv) {},
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				reply, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
					Ctx: ctx, CfgKey: "fc.pub", ValueType: vtString, Value: "v1", Reason: "降级演练",
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				item := e.itemByName("fc.pub")
				if got := e.versionCount(item.ConfigID); got != before.rows[1]+1 {
					t.Fatalf("版本快照没落库：rows=%d want=%d", got, before.rows[1]+1)
				}
				if item.LatestVersion != 1 || item.Epoch == 0 {
					t.Fatalf("指针未推进：%+v", item)
				}
			},
		},
		{
			id: "rb", name: "RollbackConfig", action: "rollback",
			setup: func(t *testing.T, e *testEnv) {
				e.publish(t, "fc.rb", "v1", 0, "fc-rb-s1")
				e.publish(t, "fc.rb", "v2", 1, "fc-rb-s2")
			},
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				reply, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
					Ctx: ctx, CfgKey: "fc.rb", ToVersion: 1, Reason: "降级演练",
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				item := e.itemByName("fc.rb")
				if item.LatestVersion != 3 {
					t.Fatalf("回滚必须追加新版本号，实得 latest=%d", item.LatestVersion)
				}
				if got := e.versionCount(item.ConfigID); got != before.rows[1]+1 {
					t.Fatalf("回滚版本行没落库：rows=%d want=%d", got, before.rows[1]+1)
				}
				if row := e.versionRow(item.ConfigID, 3); row == nil || row.ChangeType != model.ChangeTypeRollback {
					t.Fatalf("回滚行的 change_type 不对：%+v", row)
				}
			},
		},
		{
			id: "rule", name: "SaveRolloutRule", action: "create_rule",
			setup: func(t *testing.T, e *testEnv) { e.publish(t, "fc.rule", "v1", 0, "fc-rule-s") },
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				reply, err := NewSaveRolloutRuleLogic(bg(), e.svc).SaveRolloutRule(&rpc.SaveRolloutRuleReq{
					Ctx: ctx, CfgKey: "fc.rule", Version: 1,
					Rule: &rpc.RolloutRuleSpec{Name: "wl", Mode: modeWhitelist, WhitelistMids: []int64{7}, Remark: "降级演练"},
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				if got := len(e.db.rules); got != before.rows[2]+1 {
					t.Fatalf("规则没落库：%d want=%d", got, before.rows[2]+1)
				}
				// 新建一律 OFF：保存动作本身不得影响线上（saverolloutrulelogic.go 的方法注释）。
				if ids := e.ruleIDsFor(e.itemByName("fc.rule").ConfigID, 1, model.StateOn); len(ids) != 0 {
					t.Fatalf("新建规则必须是停用态，实得开了 %d 条：%v", len(ids), ids)
				}
			},
		},
		{
			id: "state", name: "SetRolloutRuleState", action: "set_rollout_state",
			setup: func(t *testing.T, e *testEnv) {
				// 规则挂在 version=2 上，所以先发一版 v1 再带规则发 v2。
				e.publish(t, "fc.state", "v1", 0, "fc-state-s1")
				e.publishWithRules(t, "fc.state", "v2", 1, "fc-state-s", []*rpc.RolloutRuleSpec{{
					Name: "wl", Mode: modeWhitelist, WhitelistMids: []int64{7}, Remark: "种子",
				}})
			},
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				ruleID := onlyRuleID(t, e, "fc.state", 2, model.StateOff)
				reply, err := NewSetRolloutRuleStateLogic(bg(), e.svc).SetRolloutRuleState(&rpc.SetRolloutRuleStateReq{
					Ctx: ctx, RuleId: ruleID, State: model.StateOn, Reason: "降级演练",
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				if got := len(e.db.rules); got != before.rows[2] {
					t.Fatalf("开闸不该新增规则行：%d want=%d", got, before.rows[2])
				}
				ids := e.ruleIDsFor(e.itemByName("fc.state").ConfigID, 2, model.StateOn)
				if len(ids) != 1 {
					t.Fatalf("规则没开闸：%v", ids)
				}
			},
		},
		{
			id: "topic", name: "SaveTopic", action: "create_topic",
			setup: func(t *testing.T, e *testEnv) {},
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				reply, err := NewSaveTopicLogic(bg(), e.svc).SaveTopic(&rpc.SaveTopicReq{
					Ctx: ctx, Slug: "fc-topic", Title: "降级演练专题", Reason: "降级演练",
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				if got := len(e.db.topics); got != before.rows[3]+1 {
					t.Fatalf("专题没落库：%d want=%d", got, before.rows[3]+1)
				}
			},
		},
		{
			id: "titems", name: "SaveTopicItems", action: "save_topic_items",
			setup: func(t *testing.T, e *testEnv) {
				e.seedTopic(t, &model.Topic{Slug: "fc-titems", Title: "条目演练", State: model.StateOn})
			},
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				topicID := e.topicBySlug("fc-titems").TopicID
				reply, err := NewSaveTopicItemsLogic(bg(), e.svc).SaveTopicItems(&rpc.SaveTopicItemsReq{
					Ctx: ctx, TopicId: topicID, Reason: "降级演练",
					Items: []*rpc.TopicItem{{ItemType: model.ItemTypeUGCVideo, ItemId: "9001", Position: 1}},
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				if got := len(e.db.topicItems); got != before.rows[4]+1 {
					t.Fatalf("专题条目没落库：%d want=%d", got, before.rows[4]+1)
				}
			},
		},
		{
			id: "slot", name: "SaveSlot", action: "create_slot",
			setup: func(t *testing.T, e *testEnv) {},
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				reply, err := NewSaveSlotLogic(bg(), e.svc).SaveSlot(&rpc.SaveSlotReq{
					Ctx: ctx, Code: "fc.slot", Page: "home", Title: "降级演练坑位", Capacity: 20, Reason: "降级演练",
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				if got := len(e.db.slots); got != before.rows[5]+1 {
					t.Fatalf("坑位没落库：%d want=%d", got, before.rows[5]+1)
				}
			},
		},
		{
			id: "sitems", name: "SaveSlotItems", action: "save_slot_items",
			setup: func(t *testing.T, e *testEnv) {
				e.seedSlot(t, &model.RecommendSlot{Code: "fc.sitems", Page: "home", Title: "条目演练", Capacity: 20, State: model.StateOn})
			},
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				slotID := e.slotByCode("fc.sitems").SlotID
				reply, err := NewSaveSlotItemsLogic(bg(), e.svc).SaveSlotItems(&rpc.SaveSlotItemsReq{
					Ctx: ctx, SlotId: slotID, Reason: "降级演练",
					Items: []*rpc.SlotItem{{ItemType: model.ItemTypeUGCVideo, ItemId: "9002", Position: 1}},
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				if got := len(e.db.slotItems); got != before.rows[6]+1 {
					t.Fatalf("坑位条目没落库：%d want=%d", got, before.rows[6]+1)
				}
			},
		},
		{
			id: "sw", name: "SaveClientSwitch", action: "create_client_switch",
			// 绑一个真实配置项：开关自己端上没有投影键（invalidateBoundConfig 注释），
			// 换代与删键都发生在它引用的配置项上——不绑定就测不到「投影删不动」这条路。
			setup: func(t *testing.T, e *testEnv) { e.publish(t, "fc.sw.cfg", "v1", 0, "fc-sw-cfg") },
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				reply, err := NewSaveClientSwitchLogic(bg(), e.svc).SaveClientSwitch(&rpc.SaveClientSwitchReq{
					Ctx: ctx, SwitchKey: "fc_switch", Platform: platAndroid,
					ConfigId: e.itemByName("fc.sw.cfg").ConfigID, Reason: "降级演练",
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				if got := len(e.db.switches); got != before.rows[7]+1 {
					t.Fatalf("开关没落库：%d want=%d", got, before.rows[7]+1)
				}
			},
		},
		{
			id: "refresh", name: "RefreshCache", action: "refresh_cache",
			setup: func(t *testing.T, e *testEnv) { e.publish(t, "fc.refresh", "v1", 0, "fc-refresh-s") },
			call: func(t *testing.T, e *testEnv, ctx *rpc.CallContext) (int64, error) {
				reply, err := NewRefreshCacheLogic(bg(), e.svc).RefreshCache(&rpc.RefreshCacheReq{
					Ctx: ctx, Target: model.RefreshTargetConfig, CfgKey: "fc.refresh", Reason: "降级演练",
				})
				return reply.GetAuditEntryId(), err
			},
			landed: func(t *testing.T, e *testEnv, before effectCounts) {
				t.Helper()
				// 刷新不新增行，唯一的权威副作用是换代（DEL 只是加速收敛）。
				if got := len(e.db.items); got != before.rows[0] {
					t.Fatalf("刷新不该新增配置项行：%d want=%d", got, before.rows[0])
				}
				// setup 里那次发布已经把 epoch 推到 1，这里必须再往上走一代。
				if got := e.itemByName("fc.refresh").Epoch; got < 2 {
					t.Fatalf("刷新必须让 epoch 换代，实得 %d（前置发布后至少 2）", got)
				}
			},
		},
	}
}

// onlyRuleID 取「某配置项某版本某状态」下唯一那条规则：数量不为 1 就是前置没造对，
// 必须直接失败，否则后面的断言会变成「拿 nil 断 nil」。
func onlyRuleID(t *testing.T, e *testEnv, cfgKey string, version int64, state int32) int64 {
	t.Helper()
	ids := e.ruleIDsFor(e.itemByName(cfgKey).ConfigID, version, state)
	if len(ids) != 1 {
		t.Fatalf("前置不成立：%s version=%d state=%d 的规则数 = %d，期望恰好 1", cfgKey, version, state, len(ids))
	}
	return ids[0]
}

// --- 1. 审计缺口：每个写 RPC × 三种「存证进不去」的形态 ---

func TestEveryWriteDegradesVisiblyWhenAuditCannotBeStored(t *testing.T) {
	shapes := []struct {
		id      string
		name    string
		disable func(e *testEnv)
	}{
		{"nil", "未配置 AuditRPC", func(e *testEnv) { e.svc.Audit = nil }},
		{"rpcerr", "audit 调用失败", func(e *testEnv) { e.audit.err = errors.New("audit rpc unavailable") }},
		{"noid", "audit 回包里没有 entry_id", func(e *testEnv) { e.audit.entryID = 0 }},
	}
	for _, tc := range writeProbes() {
		for _, shape := range shapes {
			t.Run(tc.name+"/"+shape.name, func(t *testing.T) {
				e := newTestEnv(t)
				tc.setup(t, e)
				// 日志接收器挂在 setup 之后：只读被测这一段，不把脚手架的日志算进来。
				logs := captureLogs(t)
				shape.disable(e)

				requestID := "fc-" + tc.id + "-" + shape.id
				setEntryBefore := e.call("ConfigVersion.SetAuditEntry")
				entryID, err := tc.call(t, e, callCtx(requestID))
				if err != nil {
					t.Fatalf("审计缺口不该阻断业务写入（proto 文件头）：%v", err)
				}
				if entryID != 0 {
					t.Fatalf("存证没写成却回了 entry_id=%d：伪造引用比没有引用更糟", entryID)
				}
				// 留 0 的版本行才是补偿任务的活：写了 0 反而会把有效引用覆盖掉。
				if got := e.call("ConfigVersion.SetAuditEntry"); got != setEntryBefore {
					t.Fatalf("entry_id=0 时不该回写审计引用：调用数 %d -> %d", setEntryBefore, got)
				}
				// 补偿任务唯一的入口就是日志里那句 event_id。
				want := "event_id=ops-config:" + requestID + ":" + tc.action
				if joined := logs.joined(); !strings.Contains(joined, want) {
					t.Fatalf("Error 日志里没有可补偿的 event_id %q\n实际 Error 日志：\n%s", want, joined)
				}
			})
		}
	}
}

// TestEveryWriteStoresAuditUnderItsOwnEventID 是上一组的对照组：
// 只断言「日志里有 event_id」是不够的——如果实现无条件打日志，缺口测试就会假通过。
// 这里把 event_id 的形态（"ops-config:<request_id>:<action>"，proto 文件头写死的三段）
// 与「成功时不回写 0」一起钉住。
func TestEveryWriteStoresAuditUnderItsOwnEventID(t *testing.T) {
	for _, tc := range writeProbes() {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			tc.setup(t, e)
			logs := captureLogs(t)
			// setup 自己就走真实写接口（发布、挂规则），审计请求数是累计量：
			// 只按增量判定，才不会被脚手架的存证算到被测调用头上。
			auditBefore := len(e.audit.reqs)
			requestID := "fc-ok-" + tc.id
			entryID, err := tc.call(t, e, callCtx(requestID))
			if err != nil {
				t.Fatalf("%v", err)
			}
			if entryID != 777 {
				t.Fatalf("audit_entry_id = %d，want 777（替身回的那条）", entryID)
			}
			if got := len(e.audit.reqs) - auditBefore; got != 1 {
				t.Fatalf("本次调用的审计请求数 %d want=1", got)
			}
			entry := e.audit.reqs[auditBefore].GetEntry()
			if want := "ops-config:" + requestID + ":" + tc.action; entry.GetEventId() != want {
				t.Fatalf("event_id = %q want %q（形态错了会让重放多出一条存证）", entry.GetEventId(), want)
			}
			if entry.GetAction() != "ops_config."+tc.action || entry.GetActionDomain() != "ops_config" {
				t.Fatalf("动作口径不符：%q/%q", entry.GetAction(), entry.GetActionDomain())
			}
			if entry.GetActorId() != 42 || entry.GetActorName() != "运营甲" {
				t.Fatalf("审计没带上操作人：%+v", entry)
			}
			if msg := logs.joined(); strings.Contains(msg, "ops-config/audit") {
				t.Fatalf("存证成功却打了缺口日志：\n%s", msg)
			}
		})
	}
}

// --- 2. 身份与幂等键：所有写 RPC 的同一套前置门 ---

func TestEveryWriteRequiresRequestIdAndOperator(t *testing.T) {
	var (
		longASCII  = "r" + strings.Repeat("k", 64) // 65 字节
		longName   = strings.Repeat("a", 65)
		edgeName64 = strings.Repeat("a", 64)
	)
	cases := []struct {
		name string
		ctx  func(requestID string) *rpc.CallContext
		want error
	}{
		// 缺 ctx 单独看像「调用方忘了」，但它同时是「无法归因」：契约要求写接口必带身份。
		{"ctx 为 nil", func(string) *rpc.CallContext { return nil }, model.ErrRequestIDRequired},
		{"request_id 为空", func(string) *rpc.CallContext {
			return &rpc.CallContext{OperatorId: 42, OperatorName: "运营甲"}
		}, model.ErrRequestIDRequired},
		{"request_id 只有空白", func(string) *rpc.CallContext {
			return &rpc.CallContext{RequestId: " \t ", OperatorId: 42, OperatorName: "运营甲"}
		}, model.ErrRequestIDRequired},
		// request_id 是 uniq_request_id 的列：交给 MySQL 只会得到 1406，回放还会静默失效。
		{"request_id 超列宽", func(string) *rpc.CallContext {
			return &rpc.CallContext{RequestId: longASCII, OperatorId: 42, OperatorName: "运营甲"}
		}, model.ErrRequestIDTooLong},
		{"operator_id 为 0", func(rid string) *rpc.CallContext {
			return &rpc.CallContext{RequestId: rid, OperatorName: "运营甲"}
		}, model.ErrOperatorRequired},
		{"operator_id 为负", func(rid string) *rpc.CallContext {
			return &rpc.CallContext{RequestId: rid, OperatorId: -1, OperatorName: "运营甲"}
		}, model.ErrOperatorRequired},
		{"operator_name 超列宽", func(rid string) *rpc.CallContext {
			return &rpc.CallContext{RequestId: rid, OperatorId: 42, OperatorName: longName}
		}, model.ErrOperatorNameTooLong},
	}
	for _, tc := range writeProbes() {
		for _, c := range cases {
			t.Run(tc.name+"/"+c.name, func(t *testing.T) {
				e := newTestEnv(t)
				tc.setup(t, e)
				before := e.effects()
				_, err := tc.call(t, e, c.ctx("fc-id-"+tc.id))
				wantFail(t, err, c.want, tc.name+"/"+c.name)
				// 「先验后写」的证据就是零副作用：入参被拒时不该留下任何行、键或审计。
				e.requireSameEffects(t, before, tc.name+"/"+c.name)
			})
		}
		// 边界另一侧：恰好等于列宽必须放行，否则这些用例就成了「把限制写紧」的护身符。
		t.Run(tc.name+"/operator_name 正好 64 字节要放行", func(t *testing.T) {
			e := newTestEnv(t)
			tc.setup(t, e)
			ctx := &rpc.CallContext{RequestId: "fc-edge-" + tc.id, OperatorId: 1, OperatorName: edgeName64}
			if _, err := tc.call(t, e, ctx); err != nil {
				t.Fatalf("64 字节的展示名被拒：%v", err)
			}
		})
	}
}

// TestRequestIdColumnLimitIsSixtyFourChars 把「上限数字本身」钉住一次：
// 上面每组都复用同一个常量表达式，改常量就会在这里露出来。
// 注意口径：实现按**字节**量 operator_name（比 VARCHAR(64) 的字符口径更严，宁紧不松），
// 而 request_id / reason 按 rune 量 —— 这里只钉「超 64 必拒、64 必过」这一条共识。
func TestRequestIdColumnLimitIsSixtyFourChars(t *testing.T) {
	if model.MaxRequestIDChars != 64 {
		t.Fatalf("request_id 上限应与 ops_config_version.uniq_request_id 的 VARCHAR(64) 一致，实得 %d", model.MaxRequestIDChars)
	}
	e := newTestEnv(t)
	before := e.effects()
	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx:    &rpc.CallContext{RequestId: strings.Repeat("r", 64), OperatorId: 42, OperatorName: "运营甲"},
		CfgKey: "fc.len", ValueType: vtString, Value: "v1", Reason: "边界",
	})
	if err != nil {
		t.Fatalf("恰好 64 字符的幂等键必须能用：%v", err)
	}
	if e.effects() == before {
		t.Fatal("这次调用本该真的发布一次，却没留下任何痕迹")
	}
}

// --- 3. 可选依赖缺席 / 抖动：投影挡不住写 ---

func TestEveryWriteSucceedsWhenTheCacheProjectionIsGone(t *testing.T) {
	shapes := []struct {
		id   string
		name string
		brk  func(e *testEnv)
	}{
		{"nilcache", "Cache 未配置", func(e *testEnv) { e.svc.Cache = nil }},
		{"delfail", "DEL 失败", func(e *testEnv) { e.cache.delErr = errors.New("redis down") }},
	}
	for _, tc := range writeProbes() {
		for _, shape := range shapes {
			t.Run(tc.name+"/"+shape.name, func(t *testing.T) {
				e := newTestEnv(t)
				tc.setup(t, e)
				before := e.effects()
				logs := captureLogs(t)
				shape.brk(e)

				entryID, err := tc.call(t, e, callCtx("fc-nocache-"+tc.id+"-"+shape.id))
				if err != nil {
					t.Fatalf("缓存投影不可用不该让写入失败（CacheKV 注释：Redis 是可整域重建的投影）：%v", err)
				}
				if entryID != 777 {
					t.Fatalf("业务写成功就该照常存证，audit_entry_id=%d", entryID)
				}
				tc.landed(t, e, before)
				if shape.id == "delfail" {
					// 删不动投影必须说清楚「epoch 已换代」，否则运维会以为已经即时收敛了。
					if msg := logs.joined(); !strings.Contains(msg, "epoch 已换代") {
						t.Fatalf("DEL 失败未按「最多影响一个 TTL」记账：%s", msg)
					}
				}
			})
		}
	}
}

// TestSwitchInvalidationFollowsItsConfigBinding 钉住 invalidateBoundConfig 的两个分支：
// 绑定配置项＝给那一项换代并删掉它的指针投影；未绑定＝既不 bump 也不删
// （本服务没有开关自己的键空间，见该方法注释与 README 契约缺口）。
// 上面那组降级用例只覆盖到「删不动时要记账」，这里补齐「该删的时候真删了」。
func TestSwitchInvalidationFollowsItsConfigBinding(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "gr.bind", "v1", 0, "gb-1")
	item := e.itemByName("gr.bind")
	e.resolveCached(t, "gb-read", "gr.bind", nil) // 先让投影进缓存，才看得出有没有被删
	epochBefore := item.Epoch
	before := e.effects()

	if _, err := NewSaveClientSwitchLogic(bg(), e.svc).SaveClientSwitch(&rpc.SaveClientSwitchReq{
		Ctx: callCtx("gb-bound"), SwitchKey: "gr_bind", Platform: platAndroid,
		ConfigId: item.ConfigID, Reason: "端能力上线",
	}); err != nil {
		t.Fatalf("绑定配置项的开关写入：%v", err)
	}
	if got := e.itemByName("gr.bind").Epoch; got != epochBefore+1 {
		t.Fatalf("开关改了绑定的配置项却不换代：got=%d want=%d", got, epochBefore+1)
	}
	requireDeletedKeys(t, e.deletedKeysSince(before),
		[]string{e.limits.itemKey(model.ScopeGlobal, "gr.bind")}, "绑定的配置项要删自己的指针投影")

	// 另一分支：不绑配置项的开关没有任何属于它的键，也就不许动别人的代次。
	freeBefore := e.effects()
	if _, err := NewSaveClientSwitchLogic(bg(), e.svc).SaveClientSwitch(&rpc.SaveClientSwitchReq{
		Ctx: callCtx("gb-free"), SwitchKey: "gr_free", Platform: platAndroid, Reason: "纯开关",
	}); err != nil {
		t.Fatalf("未绑定配置项的开关写入：%v", err)
	}
	if got := e.itemByName("gr.bind").Epoch; got != epochBefore+1 {
		t.Fatalf("未绑定却顺带换代了别的配置项：got=%d", got)
	}
	if keys := e.deletedKeysSince(freeBefore); len(keys) != 0 {
		t.Fatalf("未绑定配置项时不该删任何投影键：%v", keys)
	}
}

// --- 4. 非法状态跳转与越界入参：全部在碰存储之前拒 ---

func TestRolloutStateTransitionRejectsIllegalTargets(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "gr.state", "v1", 0, "gr-s0")
	e.publishWithRules(t, "gr.state", "v2", 1, "gr-s1", []*rpc.RolloutRuleSpec{{
		Name: "wl", Mode: modeWhitelist, WhitelistMids: []int64{7}, Remark: "种子",
	}})
	cfgID := e.itemByName("gr.state").ConfigID
	offRule := onlyRuleID(t, e, "gr.state", 2, model.StateOff)
	onRule := e.seedRule(t, &model.RolloutRule{
		ConfigID: cfgID, Version: 1, Name: "already-on", Mode: model.ModeWhitelist,
		WhitelistMids: model.IDListString([]int64{9}), State: model.StateOn, Priority: 10, OperatorID: 42,
	})
	baseline := e.effects()

	call := func(ruleID int64, state int32, reason string) error {
		_, err := NewSetRolloutRuleStateLogic(bg(), e.svc).SetRolloutRuleState(&rpc.SetRolloutRuleStateReq{
			Ctx: callCtx("gr-ctx"), RuleId: ruleID, State: state, Reason: reason,
		})
		return err
	}
	cases := []struct {
		name   string
		ruleID int64
		state  int32
		reason string
		want   error
	}{
		{"rule_id 为 0", 0, model.StateOn, "改闸", model.ErrRuleNotFound},
		{"rule_id 不存在", 999999, model.StateOn, "改闸", model.ErrRuleNotFound},
		{"state 未指定", offRule, model.StateUnspecified, "改闸", model.ErrRuleStateInvalid},
		{"state 取第四个值", offRule, 3, "改闸", model.ErrRuleStateInvalid},
		{"没写原因", offRule, model.StateOn, "", model.ErrReasonRequired},
		{"原因只有空白", offRule, model.StateOn, "  ", model.ErrReasonRequired},
		// 重复开闸不报错的话，审计里就会出现两条「看起来止了血」的记录（logic 注释）。
		{"目标态与当前一致", onRule.RuleID, model.StateOn, "再开一次", model.ErrRuleStateUnchanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantFail(t, call(tc.ruleID, tc.state, tc.reason), tc.want, tc.name)
			e.requireSameEffects(t, baseline, tc.name)
		})
	}

	// 合法跳转只改 state 一列：priority/percentage 必须不动（那是 SaveRolloutRule 的覆盖语义）。
	beforeRow := *e.ruleRow(offRule)
	if _, err := NewSetRolloutRuleStateLogic(bg(), e.svc).SetRolloutRuleState(&rpc.SetRolloutRuleStateReq{
		Ctx: callCtx("gr-open"), RuleId: offRule, State: model.StateOn, Reason: "开闸演练",
	}); err != nil {
		t.Fatalf("合法开闸：%v", err)
	}
	after := e.ruleRow(offRule)
	if after.State != model.StateOn {
		t.Fatalf("state 没改成 ON：%+v", after)
	}
	if after.Priority != beforeRow.Priority || after.Percentage != beforeRow.Percentage ||
		after.Mode != beforeRow.Mode || after.WhitelistMids != beforeRow.WhitelistMids ||
		after.Version != beforeRow.Version || after.Name != beforeRow.Name {
		t.Fatalf("开闸顺带改了放量条件\n  前：%+v\n  后：%+v", beforeRow, after)
	}
}

func TestSaveRolloutRuleRefusesRulesItCannotAnchor(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "gr.rule", "v1", 0, "gr-rule-s")
	baseline := e.effects()

	call := func(cfgKey string, version int64, spec *rpc.RolloutRuleSpec) error {
		_, err := NewSaveRolloutRuleLogic(bg(), e.svc).SaveRolloutRule(&rpc.SaveRolloutRuleReq{
			Ctx: callCtx("gr-rule-ctx"), CfgKey: cfgKey, Version: version, Rule: spec,
		})
		return err
	}
	good := func(mut func(*rpc.RolloutRuleSpec)) *rpc.RolloutRuleSpec {
		spec := &rpc.RolloutRuleSpec{Name: "wl", Mode: modeWhitelist, WhitelistMids: []int64{7}, Remark: "挂规则"}
		if mut != nil {
			mut(spec)
		}
		return spec
	}
	cases := []struct {
		name    string
		cfgKey  string
		version int64
		spec    *rpc.RolloutRuleSpec
		want    error
	}{
		{"cfg_key 非法", "GR Rule", 1, good(nil), model.ErrConfigKeyInvalid},
		{"cfg_key 不存在", "gr.nope", 1, good(nil), model.ErrConfigNotFound},
		{"version 为 0", "gr.rule", 0, good(nil), model.ErrVersionNotFound},
		{"version 从未发布过", "gr.rule", 7, good(nil), model.ErrVersionNotFound},
		{"rule 为 nil", "gr.rule", 1, nil, model.ErrRuleNameRequired},
		{"规则名为空", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) { s.Name = " " }), model.ErrRuleNameRequired},
		// remark 是本请求唯一的说明位，缺了就没有「为什么挂这条灰度」的证据。
		{"没写 remark", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) { s.Remark = "" }), model.ErrReasonRequired},
		{"mode 未指定", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			s.Mode = rpc.RolloutMode_ROLLOUT_MODE_UNSPECIFIED
		}), model.ErrRuleModeRequired},
		{"PERCENTAGE 但百分比为 0", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			s.Mode, s.WhitelistMids, s.Percentage = modePercentage, nil, 0
		}), model.ErrRuleModeMismatch},
		{"WHITELIST 但白名单为空", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) { s.WhitelistMids = nil }), model.ErrRuleModeMismatch},
		{"FULL 又填了白名单", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			s.Mode, s.WhitelistMids = modeFull, []int64{7}
		}), model.ErrRuleModeMismatch},
		{"百分比越界", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			s.Mode, s.WhitelistMids, s.Percentage = modePercentage, nil, 101
		}), model.ErrPercentageOutOfRange},
		{"端标识未知（第五端＝小程序）", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			s.Mode, s.Platforms = modePlatform, []rpc.ClientPlatform{9}
		}), model.ErrPlatformUnknown},
		{"端数组里出现 UNSPECIFIED", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			s.Mode, s.Platforms = modePlatform, []rpc.ClientPlatform{rpc.ClientPlatform(model.PlatformAndroid), rpc.ClientPlatform_CLIENT_PLATFORM_UNSPECIFIED}
		}), model.ErrPlatformUnknown},
		{"尾号形态非法", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			s.Mode, s.WhitelistMids, s.MidSuffixes = modeMidSuffix, nil, "a,b"
		}), model.ErrMidSuffixInvalid},
		{"白名单超上限", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) {
			for i := int64(1); i <= 201; i++ {
				s.WhitelistMids = append(s.WhitelistMids, i)
			}
		}), model.ErrWhitelistTooLarge},
		{"规则名超列宽", "gr.rule", 1, good(func(s *rpc.RolloutRuleSpec) { s.Name = strings.Repeat("n", 65) }), model.ErrRuleNameTooLong},
	}
	for _, tc := range cases {
		if tc.want == nil {
			continue // 见下面单独一条
		}
		t.Run(tc.name, func(t *testing.T) {
			wantFail(t, call(tc.cfgKey, tc.version, tc.spec), tc.want, tc.name)
			e.requireSameEffects(t, baseline, tc.name)
		})
	}

	// scope 非法：与 cfg_key 分开断言，避免两个原因共用一条用例时认错哨兵。
	cfgID := e.itemByName("gr.rule").ConfigID
	_, err := NewSaveRolloutRuleLogic(bg(), e.svc).SaveRolloutRule(&rpc.SaveRolloutRuleReq{
		Ctx: callCtx("gr-rule-scope"), CfgKey: "gr.rule", Scope: "mini_program", Version: 1, Rule: good(nil),
	})
	wantFail(t, err, model.ErrScopeUnknown, "scope 是小程序（本项目没有这一端）")
	e.requireSameEffects(t, baseline, "scope 非法")

	// 规则只能挂在**已发布**的版本上：指向不存在的版本等于把不存在的值推给线上。
	// 这里再钉一次「不物理删除」：改状态与换规则都不该让历史规则行消失（proto:327）。
	if _, err := NewSaveRolloutRuleLogic(bg(), e.svc).SaveRolloutRule(&rpc.SaveRolloutRuleReq{
		Ctx: callCtx("gr-rule-ok"), CfgKey: "gr.rule", Version: 1, Rule: good(nil),
	}); err != nil {
		t.Fatalf("合法挂规则：%v", err)
	}
	ids := e.ruleIDsFor(cfgID, 1, model.StateOff)
	if len(ids) != 1 {
		t.Fatalf("合法规则没落成停用态：%v", ids)
	}
	if got := e.call("RolloutRule.SetState"); got == 0 {
		t.Log("本次仅新增规则，未改闸：属正常路径")
	}
}

func TestCatalogWritesRejectIllegalShapesWithoutTouchingRows(t *testing.T) {
	e := newTestEnv(t)
	topic := e.seedTopic(t, &model.Topic{Slug: "gr-topic", Title: "已存在专题", State: model.StateOn, Version: 3})
	slot := e.seedSlot(t, &model.RecommendSlot{Code: "gr.slot", Page: "home", Title: "已存在坑位", Capacity: 20, State: model.StateOn, Version: 3})
	sw := e.seedSwitch(t, &model.ClientSwitch{SwitchKey: "gr_switch", Platform: model.PlatformAndroid, Enabled: model.StateOff})
	item := e.seedItem(t, "gr.bound", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	baseline := e.effects()

	t.Run("SaveTopic", func(t *testing.T) {
		cases := []struct {
			name string
			req  *rpc.SaveTopicReq
			want error
		}{
			{"标题为空", &rpc.SaveTopicReq{Ctx: callCtx("gt-1"), Slug: "gr-new"}, model.ErrTopicTitleRequired},
			{"新建不给 slug", &rpc.SaveTopicReq{Ctx: callCtx("gt-2"), Title: "无句柄"}, model.ErrTopicSlugRequired},
			{"slug 形态非法", &rpc.SaveTopicReq{Ctx: callCtx("gt-3"), Slug: "Gr Topic", Title: "x"}, model.ErrTopicSlugInvalid},
			{"slug 撞唯一键", &rpc.SaveTopicReq{Ctx: callCtx("gt-4"), Slug: "gr-topic", Title: "x"}, model.ErrTopicSlugConflict},
			{"窗口倒挂", &rpc.SaveTopicReq{Ctx: callCtx("gt-5"), Slug: "gr-new", Title: "x", StartAt: 200, EndAt: 100}, model.ErrTopicTimeRangeInvalid},
			{"state 第四个值", &rpc.SaveTopicReq{Ctx: callCtx("gt-6"), Slug: "gr-new", Title: "x", State: 4}, model.ErrRuleStateInvalid},
			{"expect_version 为负", &rpc.SaveTopicReq{Ctx: callCtx("gt-7"), TopicId: topic.TopicID, Title: "x", ExpectVersion: -1}, model.ErrVersionConflict},
			{"改一个不存在的专题", &rpc.SaveTopicReq{Ctx: callCtx("gt-8"), TopicId: 999999, Title: "x"}, model.ErrTopicNotFound},
			// 乐观锁基线过期＝手里的清单已经不是当前那份，必须重读而不是覆盖。
			{"expect_version 落后", &rpc.SaveTopicReq{Ctx: callCtx("gt-9"), TopicId: topic.TopicID, Title: "x", ExpectVersion: 2}, model.ErrVersionConflict},
			{"expect_version 超前", &rpc.SaveTopicReq{Ctx: callCtx("gt-10"), TopicId: topic.TopicID, Title: "x", ExpectVersion: 4}, model.ErrVersionConflict},
			{"zone_ids 超上限", &rpc.SaveTopicReq{Ctx: callCtx("gt-11"), TopicId: topic.TopicID, Title: "x",
				ExpectVersion: 3, ZoneIds: idsUpTo(65)}, model.ErrBatchTooLarge},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewSaveTopicLogic(bg(), e.svc).SaveTopic(tc.req)
				wantFail(t, err, tc.want, "SaveTopic/"+tc.name)
				e.requireSameEffects(t, baseline, "SaveTopic/"+tc.name)
			})
		}
	})

	t.Run("SaveTopicItems", func(t *testing.T) {
		cases := []struct {
			name string
			req  *rpc.SaveTopicItemsReq
			want error
		}{
			{"topic_id 为 0", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-1"), Reason: "排期"}, model.ErrTopicNotFound},
			{"topic_id 不存在", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-2"), TopicId: 999999, Reason: "排期",
				Items: []*rpc.TopicItem{{ItemType: model.ItemTypeUGCVideo, ItemId: "1", Position: 1}}}, model.ErrTopicNotFound},
			{"没写原因", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-3"), TopicId: topic.TopicID}, model.ErrReasonRequired},
			{"空数组＝清空，拒", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-4"), TopicId: topic.TopicID, Reason: "排期"}, model.ErrBatchEmpty},
			{"item_type 不认识", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-5"), TopicId: topic.TopicID, Reason: "排期",
				Items: []*rpc.TopicItem{{ItemType: "ad_creative", ItemId: "1", Position: 1}}}, model.ErrItemTypeUnsupported},
			{"专题里挂专题（自引用环）", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-6"), TopicId: topic.TopicID, Reason: "排期",
				Items: []*rpc.TopicItem{{ItemType: model.ItemTypeTopic, ItemId: "1", Position: 1}}}, model.ErrItemTypeUnsupported},
			{"item_id 为空", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-7"), TopicId: topic.TopicID, Reason: "排期",
				Items: []*rpc.TopicItem{{ItemType: model.ItemTypeUGCVideo, ItemId: " ", Position: 1}}}, model.ErrItemRefRequired},
			{"position 不从 1 开始", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-8"), TopicId: topic.TopicID, Reason: "排期",
				Items: []*rpc.TopicItem{{ItemType: model.ItemTypeUGCVideo, ItemId: "1", Position: 2}}}, model.ErrTopicPositionNotSequential},
			{"position 有空洞", &rpc.SaveTopicItemsReq{Ctx: callCtx("ti-9"), TopicId: topic.TopicID, Reason: "排期",
				Items: []*rpc.TopicItem{
					{ItemType: model.ItemTypeUGCVideo, ItemId: "1", Position: 1},
					{ItemType: model.ItemTypeUGCVideo, ItemId: "2", Position: 3},
				}}, model.ErrTopicPositionNotSequential},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewSaveTopicItemsLogic(bg(), e.svc).SaveTopicItems(tc.req)
				wantFail(t, err, tc.want, "SaveTopicItems/"+tc.name)
				e.requireSameEffects(t, baseline, "SaveTopicItems/"+tc.name)
			})
		}
	})

	t.Run("SaveSlot", func(t *testing.T) {
		cases := []struct {
			name string
			req  *rpc.SaveSlotReq
			want error
		}{
			{"code 为空", &rpc.SaveSlotReq{Ctx: callCtx("gs-1"), Capacity: 20}, model.ErrSlotCodeRequired},
			{"code 形态非法（连字符）", &rpc.SaveSlotReq{Ctx: callCtx("gs-2"), Code: "gr-slot", Capacity: 20}, model.ErrSlotCodeInvalid},
			{"code 撞唯一键", &rpc.SaveSlotReq{Ctx: callCtx("gs-3"), Code: "gr.slot", Capacity: 20}, model.ErrSlotCodeConflict},
			{"容量为负", &rpc.SaveSlotReq{Ctx: callCtx("gs-4"), Code: "gr.new", Capacity: -1}, model.ErrSlotCapacityInvalid},
			{"容量超硬上限", &rpc.SaveSlotReq{Ctx: callCtx("gs-5"), Code: "gr.new", Capacity: 201}, model.ErrSlotCapacityInvalid},
			{"端标识未知", &rpc.SaveSlotReq{Ctx: callCtx("gs-6"), Code: "gr.new", Capacity: 20,
				Platforms: []rpc.ClientPlatform{9}}, model.ErrPlatformUnknown},
			{"state 第四个值", &rpc.SaveSlotReq{Ctx: callCtx("gs-7"), Code: "gr.new", Capacity: 20, State: 7}, model.ErrRuleStateInvalid},
			{"坑位不存在", &rpc.SaveSlotReq{Ctx: callCtx("gs-8"), SlotId: 999999, Code: "gr.new", Capacity: 20}, model.ErrSlotNotFound},
			{"expect_version 为负", &rpc.SaveSlotReq{Ctx: callCtx("gs-9"), SlotId: slot.SlotID, Code: "gr.slot", Capacity: 20, ExpectVersion: -1}, model.ErrVersionConflict},
			{"expect_version 落后", &rpc.SaveSlotReq{Ctx: callCtx("gs-10"), SlotId: slot.SlotID, Code: "gr.slot", Capacity: 20, ExpectVersion: 2}, model.ErrVersionConflict},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewSaveSlotLogic(bg(), e.svc).SaveSlot(tc.req)
				wantFail(t, err, tc.want, "SaveSlot/"+tc.name)
				e.requireSameEffects(t, baseline, "SaveSlot/"+tc.name)
			})
		}
		// 缩容必须先看现有生效条目装不装得下：否则已排好的内容会凭空消失而响应里没线索。
		e.seedSlotItems(t, slot.SlotID, 20, []*model.SlotItem{
			{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemID: "a1", State: model.StateOn},
			{Position: 2, ItemType: model.ItemTypeUGCVideo, ItemID: "a2", State: model.StateOn},
		})
		shrunk := e.effects()
		_, err := NewSaveSlotLogic(bg(), e.svc).SaveSlot(&rpc.SaveSlotReq{
			Ctx: callCtx("gs-cap"), SlotId: slot.SlotID, Code: "gr.slot", Capacity: 1, ExpectVersion: 3,
		})
		wantFail(t, err, model.ErrSlotCapacityInvalid, "缩容到装不下现有条目")
		e.requireSameEffects(t, shrunk, "缩容被拒")
	})

	t.Run("SaveSlotItems", func(t *testing.T) {
		// 上一个子用例往坑位里种了 2 条条目，基线必须在它之后重取。
		afterSeed := e.effects()
		ref := func(pos int32, typ, id string) *rpc.SlotItem {
			return &rpc.SlotItem{Position: pos, ItemType: typ, ItemId: id}
		}
		ugc := func(pos int32, id string) *rpc.SlotItem {
			return ref(pos, model.ItemTypeUGCVideo, id)
		}
		// 条目数上限取 min(配置上限, 坑位容量)：容量 20 在这里是硬约束，
		// 越界条目永远展示不出来，不能让半批新半批旧的排期落到线上。
		tooMany := make([]*rpc.SlotItem, 0, 21)
		for i := int32(1); i <= 21; i++ {
			tooMany = append(tooMany, ugc(i, "x"))
		}
		cases := []struct {
			name string
			req  *rpc.SaveSlotItemsReq
			want error
		}{
			{"slot_id 为 0", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-1"), Reason: "排期"}, model.ErrSlotNotFound},
			{"slot_id 不存在", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-2"), SlotId: 999999, Reason: "排期",
				Items: []*rpc.SlotItem{ugc(1, "a1")}}, model.ErrSlotNotFound},
			{"没写原因", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-3"), SlotId: slot.SlotID,
				Items: []*rpc.SlotItem{ugc(1, "a1")}}, model.ErrReasonRequired},
			{"空数组＝清空，拒", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-4"), SlotId: slot.SlotID, Reason: "排期"}, model.ErrBatchEmpty},
			{"item_type 不认识", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-6"), SlotId: slot.SlotID, Reason: "排期",
				Items: []*rpc.SlotItem{ref(1, "ad_creative", "a1")}}, model.ErrItemTypeUnsupported},
			{"position 越界（超出容量）", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-7"), SlotId: slot.SlotID, Reason: "排期",
				Items: []*rpc.SlotItem{ugc(21, "a1")}}, model.ErrSlotPositionOutOfRange},
			{"position 重复", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-8"), SlotId: slot.SlotID, Reason: "排期",
				Items: []*rpc.SlotItem{ugc(1, "a1"), ugc(1, "a2")}}, model.ErrSlotPositionDuplicated},
			{"同一内容挂两次", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-9"), SlotId: slot.SlotID, Reason: "排期",
				Items: []*rpc.SlotItem{ugc(1, "a1"), ugc(2, "a1")}}, model.ErrSlotItemDuplicated},
			{"排期窗口倒挂", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-10"), SlotId: slot.SlotID, Reason: "排期",
				Items: []*rpc.SlotItem{{Position: 1, ItemType: model.ItemTypeUGCVideo, ItemId: "a1", StartAt: 200, EndAt: 100}}},
				model.ErrRuleTimeRangeInvalid},
			{"条目数超过坑位容量", &rpc.SaveSlotItemsReq{Ctx: callCtx("si-11"), SlotId: slot.SlotID, Reason: "排期",
				Items: tooMany}, model.ErrSlotItemLimit},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewSaveSlotItemsLogic(bg(), e.svc).SaveSlotItems(tc.req)
				wantFail(t, err, tc.want, "SaveSlotItems/"+tc.name)
				e.requireSameEffects(t, afterSeed, "SaveSlotItems/"+tc.name)
			})
		}
		// 前置检查的错误要指到第几条，否则运营只能一条条试。
		_, err := NewSaveSlotItemsLogic(bg(), e.svc).SaveSlotItems(&rpc.SaveSlotItemsReq{
			Ctx: callCtx("si-5"), SlotId: slot.SlotID, Reason: "排期",
			Items: []*rpc.SlotItem{ugc(1, "a1"), ref(2, model.ItemTypeUGCVideo, " ")},
		})
		wantFail(t, err, model.ErrItemRefRequired, "SaveSlotItems/item_id 为空")
		if !strings.Contains(err.Error(), "items[1]") {
			t.Errorf("item_id 为空的错误没有指到下标，得到 %v", err)
		}
		e.requireSameEffects(t, afterSeed, "SaveSlotItems/item_id 为空")
	})

	t.Run("SaveClientSwitch", func(t *testing.T) {
		// 同样只跟上一组之后的现场比：SaveSlot 那条子用例种过坑位条目。
		before := e.effects()
		cases := []struct {
			name string
			req  *rpc.SaveClientSwitchReq
			want error
		}{
			{"key 为空", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-1"), Platform: platAndroid, Reason: "开关联调"}, model.ErrSwitchKeyRequired},
			{"key 形态非法（大写）", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-2"), SwitchKey: "GR_switch",
				Platform: platAndroid, Reason: "开关联调"}, model.ErrSwitchKeyRequired},
			{"没写原因", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-3"), SwitchKey: "gr_new", Platform: platAndroid}, model.ErrReasonRequired},
			// 0 不能当「不限端」放过：读取侧就没有优先级可谈了。
			{"端为 UNSPECIFIED", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-4"), SwitchKey: "gr_new", Reason: "开关联调"}, model.ErrPlatformRequired},
			{"端是小程序（第五端）", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-5"), SwitchKey: "gr_new",
				Platform: rpc.ClientPlatform(5), Reason: "开关联调"}, model.ErrPlatformUnknown},
			{"enabled 第三个值", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-6"), SwitchKey: "gr_new",
				Platform: platAndroid, Enabled: 7, Reason: "开关联调"}, model.ErrRuleStateInvalid},
			{"expect_version 为负", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-7"), SwitchId: sw.SwitchID, SwitchKey: "gr_switch",
				Platform: platAndroid, ExpectVersion: -1, Reason: "开关联调"}, model.ErrVersionConflict},
			{"改一个不存在的开关", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-8"), SwitchId: 999999, SwitchKey: "gr_switch",
				Platform: platAndroid, Reason: "开关联调"}, model.ErrSwitchNotFound},
			// 新建时带 expect_version ≠ 0＝调用方以为自己在改一行已有的记录。
			{"新建却给了乐观锁基线", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-9"), SwitchKey: "gr.fresh",
				Platform: platAndroid, ExpectVersion: 3, Reason: "开关联调"}, model.ErrSwitchNotFound},
			{"config_id 指向不存在的配置项", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-10"), SwitchKey: "gr_switch",
				Platform: platAndroid, ConfigId: 999999, Reason: "开关联调"}, model.ErrConfigNotFound},
			// 端校验在引用校验之前：绑了存在的配置项也不能让一个不存在的端溜过去。
			{"config_id 存在但端非法", &rpc.SaveClientSwitchReq{Ctx: callCtx("gc-11"), SwitchKey: "gr_switch",
				Platform: rpc.ClientPlatform(5), ConfigId: item.ConfigID, Reason: "开关联调"}, model.ErrPlatformUnknown},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := NewSaveClientSwitchLogic(bg(), e.svc).SaveClientSwitch(tc.req)
				wantFail(t, err, tc.want, "SaveClientSwitch/"+tc.name)
				e.requireSameEffects(t, before, "SaveClientSwitch/"+tc.name)
			})
		}
	})
}

// idsUpTo 造 n 个正整数 ID，用于踩引用数上限。
func idsUpTo(n int) []int64 {
	out := make([]int64, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, int64(i))
	}
	return out
}

var _ = fmt.Sprintf
