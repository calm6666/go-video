// 本文件只放用例之间共用的脚手架：种子数据、探针与断言助手。
// 测试替身（内存库/缓存/审计）在 fakes_test.go。
//
// 为什么种子数据一律走 model 接口而不是直接写 map：
// 只有经过真写路径，「库里有这一行」才等价于「生产库里可能出现这一行」——
// model 的校验与默认值（唯一键、形态检查、State/Priority 兜底）会先把非法种子挡掉。
// 唯一例外是 putRawVersion：它专门造「库被手工改坏」的行，
// 而那正是 fail-closed 分支（ErrValueTypeUnsupported / ErrVersionNotFound）的入口条件。

package logic

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

func bg() context.Context { return context.Background() }

// wantErr 断言错误链里含目标哨兵。契约用 errors.Is 表达，不用文案，
// 否则改一句提示语就会「测试失败但其实没错」。
func wantErr(t *testing.T, err error, target error, label string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s：期望错误 %v，实得 %v", label, target, err)
	}
}

// wantFail 除了哨兵之外还要求错误非 nil：
// 一个 nil 错误 + 零值响应就是「伪造成功」，是最需要被测试挡住的形态。
func wantFail(t *testing.T, err error, target error, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：期望失败 %v，实得成功（伪造成功是最坏的一类通过）", label, target)
	}
	wantErr(t, err, target, label)
}

func wantOK[T any](t *testing.T, got T, err error, label string) T {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：意外失败：%v", label, err)
	}
	return got
}

// --- 副作用探针 ---

type effectCounts struct {
	rows       [8]int
	audits     int
	dels, sets int
}

func (e *testEnv) effects() effectCounts {
	return effectCounts{
		rows: [8]int{
			len(e.db.items), len(e.db.versions), len(e.db.rules), len(e.db.topics),
			len(e.db.topicItems), len(e.db.slots), len(e.db.slotItems), len(e.db.switches),
		},
		audits: len(e.audit.reqs),
		dels:   len(e.cache.dels),
		sets:   len(e.cache.sets),
	}
}

func (c effectCounts) String() string {
	return fmt.Sprintf("item=%d version=%d rule=%d topic=%d topicItem=%d slot=%d slotItem=%d switch=%d audit=%d del=%d set=%d",
		c.rows[0], c.rows[1], c.rows[2], c.rows[3], c.rows[4], c.rows[5], c.rows[6], c.rows[7],
		c.audits, c.dels, c.sets)
}

// requireSameEffects 断言一次请求没留下任何痕迹：入参被拒、乐观锁输了、事务回滚了，
// 三种情形都要求「行 / 审计 / 缓存」与调用前完全一致。
func (e *testEnv) requireSameEffects(t *testing.T, before effectCounts, label string) {
	t.Helper()
	if got := e.effects(); got != before {
		t.Fatalf("%s：产生了副作用\n  前：%s\n  后：%s", label, before, got)
	}
}

func (e *testEnv) call(name string) int { return e.db.calls[name] }

// captureLogs 挂一份内存日志接收器，用来证明「只能从日志看到」的契约。
// 用 AddWriter 而不是 SetWriter：后者会摘掉默认输出，断言写错时连日志都看不见了。
// 链上的 writer 没有注销接口，因此测试进程会攒下若干个接收器：无副作用，
// 每个用例只读自己那一份，日志写入本身是同步的（Errorf 返回时已进入 text）。
func captureLogs(t *testing.T) *fakeLogWriter {
	t.Helper()
	w := &fakeLogWriter{}
	logx.AddWriter(w)
	return w
}

// txMark / txCallsSince 把「这次调用开了几个事务」的判定窗口收在被测调用上：
// db.txCalls 是整个用例的累计量，种子发布自己就开过事务，
// 拿绝对值断言等于把脚手架的写入算到被测代码头上。
func (e *testEnv) txMark() int { return e.db.txCalls }

func (e *testEnv) txCallsSince(mark int) int { return e.db.txCalls - mark }

// deletedKeysSince 同理只回 baseline 之后新增的投影删除记录。
func (e *testEnv) deletedKeysSince(baseline effectCounts) []string {
	if baseline.dels > len(e.cache.dels) {
		return e.cache.dels
	}
	return e.cache.dels[baseline.dels:]
}

// --- 行探针（直接读内存库，不再经被测代码） ---

// copyOf 回一份行副本：探针若把 live 指针交出去，用例改指针等于改库，
// 「断言库里某列没变」就会变成自我实现的假命题。
func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

func (e *testEnv) itemRow(configID int64) *model.ConfigItem { return copyOf(e.db.items[configID]) }
func (e *testEnv) ruleRow(ruleID int64) *model.RolloutRule  { return copyOf(e.db.rules[ruleID]) }
func (e *testEnv) topicRow(id int64) *model.Topic           { return copyOf(e.db.topics[id]) }
func (e *testEnv) slotRow(id int64) *model.RecommendSlot    { return copyOf(e.db.slots[id]) }
func (e *testEnv) switchRow(id int64) *model.ClientSwitch   { return copyOf(e.db.switches[id]) }

// itemByName 按 cfg_key 找行：主键由替身自增分配，用例不该猜它从几开始，
// 猜错就会把「断言某行没变」写成「断言 nil 没变」。
func (e *testEnv) itemByName(cfgKey string) *model.ConfigItem {
	for _, row := range e.db.items {
		if row.CfgKey == cfgKey {
			return copyOf(row)
		}
	}
	panic("库里没有配置项 " + cfgKey)
}

// topicBySlug / slotByCode 同 itemByName：probe 的 setup 与 call 是两个闭包，
// 拿不到 seed 的返回值，只能按业务键回查主键。
func (e *testEnv) topicBySlug(slug string) *model.Topic {
	for _, row := range e.db.topics {
		if row.Slug == slug {
			return copyOf(row)
		}
	}
	panic("库里没有专题 " + slug)
}

func (e *testEnv) slotByCode(code string) *model.RecommendSlot {
	for _, row := range e.db.slots {
		if row.Code == code {
			return copyOf(row)
		}
	}
	panic("库里没有坑位 " + code)
}

func (e *testEnv) versionRow(configID, version int64) *model.ConfigVersion {
	for _, row := range e.db.versions {
		if row.ConfigID == configID && row.Version == version {
			return copyOf(row)
		}
	}
	return nil
}

func (e *testEnv) versionCount(configID int64) int {
	var n int
	for _, row := range e.db.versions {
		if row.ConfigID == configID {
			n++
		}
	}
	return n
}

func (e *testEnv) ruleIDsFor(configID, version int64, state int32) []int64 {
	var out []int64
	for _, row := range e.db.rules {
		if row.ConfigID == configID && row.Version == version && (state == 0 || row.State == state) {
			out = append(out, row.RuleID)
		}
	}
	slices.Sort(out)
	return out
}

// --- 缓存键断言 ---

// requireDeletedKeys 精确比对「这次删了哪些键」：多删 = 越界（AGENTS.md §5），
// 少删 = 改了但端上还是旧的。两侧都要钉，所以用集合相等而不是包含。
// 传入的 got 必须是**被测调用自己**那一段（见 deletedKeysSince），
// 否则种子发布删掉的键会被算到被测调用头上。
func requireDeletedKeys(t *testing.T, got, want []string, label string) {
	t.Helper()
	actual := slices.Clone(got)
	slices.Sort(actual)
	expect := slices.Clone(want)
	slices.Sort(expect)
	if !slices.Equal(actual, expect) {
		t.Fatalf("%s：删除键不符\n  got =%v\n want=%v", label, actual, expect)
	}
}

// requireOwnKeys 断言被写/删过的每个键都落在本服务自己的前缀之下。
func requireOwnKeys(t *testing.T, keys []string, prefix, label string) {
	t.Helper()
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix+":") {
			t.Fatalf("%s：键 %q 越出了本服务的键空间 %q", label, k, prefix)
		}
	}
}

// --- 种子数据 ---

func (e *testEnv) seedItem(t *testing.T, cfgKey, scope string, valueType, state int32) *model.ConfigItem {
	t.Helper()
	if scope == "" {
		scope = model.ScopeGlobal
	}
	if valueType == 0 {
		valueType = model.ValueTypeString
	}
	if state == 0 {
		state = model.StateOn
	}
	id, err := e.svc.Models.ConfigItem.Insert(bg(), &model.ConfigItem{
		CfgKey: cfgKey, Scope: scope, ValueType: valueType, State: state, Title: cfgKey + " 标题",
	})
	if err != nil {
		t.Fatalf("seed 配置项 %s：%v", cfgKey, err)
	}
	return e.itemRow(id)
}

// seedPublished 造一个「已发布一次」的配置项 + 对应快照行。
// 用于只测读取路径、不想被发布接口牵连的用例。
func (e *testEnv) seedPublished(t *testing.T, cfgKey, value, requestID string) (*model.ConfigItem, *model.ConfigVersion) {
	t.Helper()
	item := e.seedItem(t, cfgKey, model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	ver := e.seedVersion(t, item.ConfigID, 1, value, model.ValueTypeString,
		model.ChangeTypeCreate, requestID, "种子：首次发布")
	// 指针必须与快照同行，否则读路径会走成 ErrUnpublished。
	e.db.items[item.ConfigID].LatestVersion = ver.Version
	e.db.items[item.ConfigID].Epoch++
	return e.itemRow(item.ConfigID), e.versionRow(item.ConfigID, ver.Version)
}

// seedVersion 追加一条不可变版本快照（走 model 写路径，唯一键与形态校验都生效）。
func (e *testEnv) seedVersion(t *testing.T, configID, version int64, value string, valueType int32,
	changeType, requestID, reason string) *model.ConfigVersion {
	t.Helper()
	v := &model.ConfigVersion{
		ConfigID: configID, Version: version, Value: value, ValueType: valueType,
		ChangeType: changeType, RequestID: requestID, Reason: reason,
		OperatorID: 42, OperatorName: "运营甲", PublishedAt: model.NowUnix(),
	}
	if _, err := e.svc.Models.ConfigVersion.Insert(bg(), v); err != nil {
		t.Fatalf("seed 版本 config_id=%d version=%d：%v", configID, version, err)
	}
	return e.versionRow(configID, version)
}

// putRawVersion 绕过 model 校验直接落行：只用于制造「数据被手工改坏」的现场。
func (e *testEnv) putRawVersion(v *model.ConfigVersion) *model.ConfigVersion {
	if v.VersionID == 0 {
		v.VersionID = e.db.id()
	}
	cp := *v
	e.db.versions[cp.VersionID] = &cp
	return &cp
}

// forceItemState 改配置项的启停位。契约里**没有**「停用配置项」的 RPC
// （opsconfig.proto 的 OpsConfig service 只有发布/回滚/规则/专题/坑位/开关/刷新），
// 所以库里出现 state=2 只有「DBA 手工收口」这一条路，测试也只能照此造现场。
func (e *testEnv) forceItemState(configID int64, state int32) *model.ConfigItem {
	row := e.db.items[configID]
	if row == nil {
		panic("库里没有 config_id")
	}
	cp := *row
	cp.State = state
	e.db.items[configID] = &cp
	return copyOf(&cp)
}

func (e *testEnv) seedRule(t *testing.T, rule *model.RolloutRule) *model.RolloutRule {
	t.Helper()
	id, _, err := e.svc.Models.RolloutRule.Upsert(bg(), rule)
	if err != nil {
		t.Fatalf("seed 灰度规则 %s：%v", rule.Name, err)
	}
	return e.ruleRow(id)
}

func (e *testEnv) seedTopic(t *testing.T, topic *model.Topic) *model.Topic {
	t.Helper()
	if topic.State == 0 {
		t.Fatalf("seed 专题必须显式给 state：0 会让「默认下架」的用例失真")
	}
	id, err := e.svc.Models.Topic.Insert(bg(), topic)
	if err != nil {
		t.Fatalf("seed 专题 %s：%v", topic.Slug, err)
	}
	return e.topicRow(id)
}

func (e *testEnv) seedSlot(t *testing.T, slot *model.RecommendSlot) *model.RecommendSlot {
	t.Helper()
	if slot.State == 0 {
		t.Fatalf("seed 坑位必须显式给 state")
	}
	id, err := e.svc.Models.Slot.Insert(bg(), slot)
	if err != nil {
		t.Fatalf("seed 坑位 %s：%v", slot.Code, err)
	}
	return e.slotRow(id)
}

func (e *testEnv) seedSwitch(t *testing.T, sw *model.ClientSwitch) *model.ClientSwitch {
	t.Helper()
	id, err := e.svc.Models.ClientSwitch.Insert(bg(), sw)
	if err != nil {
		t.Fatalf("seed 开关 %s/%d：%v", sw.SwitchKey, sw.Platform, err)
	}
	return e.switchRow(id)
}

func (e *testEnv) seedSlotItems(t *testing.T, slotID int64, capacity int32, items []*model.SlotItem) {
	t.Helper()
	if _, err := e.svc.Models.SlotItem.ReplaceAll(bg(), slotID, capacity, items, 42, model.NowUnix(), 0); err != nil {
		t.Fatalf("seed 坑位条目 slot_id=%d：%v", slotID, err)
	}
}

func (e *testEnv) seedTopicItems(t *testing.T, topicID int64, items []*model.TopicItem) {
	t.Helper()
	if _, err := e.svc.Models.TopicItem.ReplaceAll(bg(), topicID, items, 42, model.NowUnix(), 0); err != nil {
		t.Fatalf("seed 专题条目 topic_id=%d：%v", topicID, err)
	}
}

// --- 走真实接口的快捷调用 ---

func (e *testEnv) publish(t *testing.T, cfgKey, value string, expect int64, requestID string) *rpc.PublishConfigReply {
	t.Helper()
	return e.publishWithRules(t, cfgKey, value, expect, requestID, nil)
}

func (e *testEnv) publishWithRules(t *testing.T, cfgKey, value string, expect int64, requestID string,
	specs []*rpc.RolloutRuleSpec) *rpc.PublishConfigReply {
	t.Helper()
	reply, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx(requestID), CfgKey: cfgKey, ValueType: rpc.ConfigValueType(model.ValueTypeString),
		Value: value, ExpectVersion: expect, Reason: "用例发布", Rollout: specs,
	})
	if err != nil {
		t.Fatalf("PublishConfig(%s, expect=%d)：%v", cfgKey, expect, err)
	}
	return reply
}

// resolve 用「不带 request_id」的调用做纯回源读取，避免用例被缓存投影干扰。
func (e *testEnv) resolve(t *testing.T, cfgKey string, target *rpc.TargetContext) (*rpc.ResolveConfigReply, error) {
	t.Helper()
	return NewResolveConfigLogic(bg(), e.svc).ResolveConfig(&rpc.ResolveConfigReq{
		Ctx: &rpc.CallContext{}, CfgKey: cfgKey, Target: target,
	})
}

func (e *testEnv) mustResolve(t *testing.T, cfgKey string, target *rpc.TargetContext) *rpc.ConfigView {
	t.Helper()
	reply, err := e.resolve(t, cfgKey, target)
	if err != nil {
		t.Fatalf("ResolveConfig(%s)：%v", cfgKey, err)
	}
	if !reply.GetFound() {
		t.Fatalf("ResolveConfig(%s)：期望命中，实得 found=false", cfgKey)
	}
	return reply.GetConfig()
}

// openRule 把一条规则开闸（走真实接口，顺带钉住「写规则 / 开闸是两步」的语义）。
func (e *testEnv) openRule(t *testing.T, ruleID int64, requestID string) {
	t.Helper()
	e.setRuleState(t, ruleID, model.StateOn, requestID)
}

// closeRule 关闸：回滚只应重新打开「当时还开着」的规则，
// 没有关闸这一步就区分不出「回滚重建了已停用的规则」这类越界行为。
func (e *testEnv) closeRule(t *testing.T, ruleID int64, requestID string) {
	t.Helper()
	e.setRuleState(t, ruleID, model.StateOff, requestID)
}

func (e *testEnv) setRuleState(t *testing.T, ruleID int64, state int32, requestID string) {
	t.Helper()
	_, err := NewSetRolloutRuleStateLogic(bg(), e.svc).SetRolloutRuleState(&rpc.SetRolloutRuleStateReq{
		Ctx: callCtx(requestID), RuleId: ruleID, State: state, Reason: "用例改闸",
	})
	if err != nil {
		t.Fatalf("SetRolloutRuleState(rule_id=%d, state=%d)：%v", ruleID, state, err)
	}
}
