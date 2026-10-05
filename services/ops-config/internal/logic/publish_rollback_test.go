// 本文件覆盖「发布 / 回滚」这一组写接口：它们共用同一套写入骨架
// （追加不可变版本 + CAS 推进指针 + 同事务挂规则），风险点也因此同源：
// 乐观锁、幂等回放、跨表原子性、审计存证缺口的可见性。
//
// 断言取向：只钉「契约与状态迁移」，不复述实现步骤；
// 每一次被拒的请求都要求「行 / 审计 / 缓存」三者零副作用。

package logic

import (
	"errors"
	"strings"
	"testing"

	auditrpc "go-video/services/audit/rpc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

// 枚举一律从 model 常量换算而来：logic 与 model 若出现口径漂移，
// 本文件的断言立刻失效（而不是各自测一套数字）。
const (
	vtString = rpc.ConfigValueType(model.ValueTypeString)
	vtInt    = rpc.ConfigValueType(model.ValueTypeInt)
	vtBool   = rpc.ConfigValueType(model.ValueTypeBool)
	vtJSON   = rpc.ConfigValueType(model.ValueTypeJSON)
	vtNone   = rpc.ConfigValueType(model.ValueTypeUnspecified)

	modePercentage = rpc.RolloutMode(model.ModePercentage)
	modeWhitelist  = rpc.RolloutMode(model.ModeWhitelist)
	modePlatform   = rpc.RolloutMode(model.ModePlatform)
	modeMidSuffix  = rpc.RolloutMode(model.ModeMidSuffix)

	platAndroid = rpc.ClientPlatform(model.PlatformAndroid)
)

func TestPublishConfigRejectsInvalidIdentityAndLockInputs(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-seed")
	before := e.effects()
	longName := strings.Repeat("名", 65)
	longReason := strings.Repeat("由", 501)

	cases := []struct {
		name string
		req  *rpc.PublishConfigReq
		want error
	}{
		{"缺 ctx", &rpc.PublishConfigReq{CfgKey: "home.title", Value: "v2", Reason: "r"}, model.ErrRequestIDRequired},
		{"空 request_id", &rpc.PublishConfigReq{Ctx: callCtx("  "), CfgKey: "home.title", Value: "v2", Reason: "r"}, model.ErrRequestIDRequired},
		{"request_id 超列宽", &rpc.PublishConfigReq{Ctx: callCtx(strings.Repeat("r", 65)), CfgKey: "home.title", Value: "v2", Reason: "r"}, model.ErrRequestIDTooLong},
		{"operator_id 缺失", &rpc.PublishConfigReq{Ctx: &rpc.CallContext{RequestId: "req-a", OperatorName: "甲"}, CfgKey: "home.title", Value: "v2", Reason: "r"}, model.ErrOperatorRequired},
		{"operator_name 超列宽", &rpc.PublishConfigReq{Ctx: &rpc.CallContext{RequestId: "req-a", OperatorId: 1, OperatorName: longName}, CfgKey: "home.title", Value: "v2", Reason: "r"}, model.ErrOperatorNameTooLong},
		{"cfg_key 为空", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), Value: "v2", Reason: "r"}, model.ErrConfigKeyRequired},
		{"cfg_key 含大写与空格", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), CfgKey: "Home Title", Value: "v2", Reason: "r"}, model.ErrConfigKeyInvalid},
		{"cfg_key 含斜杠", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), CfgKey: "home/title", Value: "v2", Reason: "r"}, model.ErrConfigKeyInvalid},
		{"scope 是小程序（本项目没有这一端）", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), CfgKey: "home.title", Scope: "mini_program", Value: "v2", Reason: "r"}, model.ErrScopeUnknown},
		{"原因为空", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), CfgKey: "home.title", Value: "v2", ExpectVersion: 1}, model.ErrReasonRequired},
		{"原因只有空白", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), CfgKey: "home.title", Value: "v2", ExpectVersion: 1, Reason: " \t "}, model.ErrReasonRequired},
		{"原因超长（拒绝而非截断）", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), CfgKey: "home.title", Value: "v2", ExpectVersion: 1, Reason: longReason}, model.ErrReasonTooLong},
		{"expect_version 为负", &rpc.PublishConfigReq{Ctx: callCtx("req-a"), CfgKey: "home.title", Value: "v2", ExpectVersion: -1, Reason: "r"}, model.ErrVersionConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(tc.req)
			wantFail(t, err, tc.want, tc.name)
			e.requireSameEffects(t, before, tc.name)
		})
	}
}

func TestPublishConfigRejectsValuesThatDoNotMatchDeclaredType(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()
	cases := []struct {
		name      string
		key       string
		valueType rpc.ConfigValueType
		value     string
		want      error
	}{
		{"未声明类型", "home.a1", vtNone, "x", model.ErrValueTypeUnsupported},
		{"未知类型", "home.a2", rpc.ConfigValueType(9), "x", model.ErrValueTypeUnsupported},
		{"int 键写非数字", "home.a3", vtInt, "abc", model.ErrValueInvalid},
		{"int 键写浮点", "home.a4", vtInt, "1.5", model.ErrValueInvalid},
		{"bool 键写 1", "home.a5", vtBool, "1", model.ErrValueInvalid},
		{"json 键写残缺对象", "home.a6", vtJSON, `{"a":`, model.ErrValueInvalid},
		{"值超过 OpsValue.MaxBytes", "home.a7", vtString, strings.Repeat("a", 8193), model.ErrValueTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
				Ctx: callCtx("req-" + tc.key), CfgKey: tc.key,
				ValueType: tc.valueType, Value: tc.value, Reason: "r",
			})
			wantFail(t, err, tc.want, tc.name)
			e.requireSameEffects(t, before, tc.name)
		})
	}
	// 边界：正好等于上限的值必须通过（限的是「超过」，不是误杀）。
	if _, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-edge"), CfgKey: "home.edge", ValueType: vtString,
		Value: strings.Repeat("a", 8192), Reason: "边界值",
	}); err != nil {
		t.Fatalf("MaxBytes 边界的值被误拒：%v", err)
	}
}

func TestPublishConfigFirstPublishAppendsVersionAndAdvancesPointer(t *testing.T) {
	e := newTestEnv(t)
	reply, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-first"), CfgKey: "home.title", Scope: " android ",
		ValueType: vtString, Value: "v1", Reason: "首次上线",
	})
	reply = wantOK(t, reply, err, "首次发布")

	configID := reply.GetVersion().GetConfigId()
	item := e.itemRow(configID)
	if item == nil {
		t.Fatal("配置项主记录未落库")
	}
	if item.Scope != model.ScopeAndroid {
		t.Fatalf("scope 未归一（trim 后入库）：%q", item.Scope)
	}
	if item.LatestVersion != 1 || item.State != model.StateOn || item.Epoch != 1 {
		t.Fatalf("首发后指针/状态/代次应为 1/ON/1，实得 %+v", item)
	}
	ver := e.versionRow(configID, 1)
	if ver.ChangeType != model.ChangeTypeCreate || ver.RequestID != "req-first" ||
		ver.Reason != "首次上线" || ver.OperatorName != "运营甲" || ver.Value != "v1" {
		t.Fatalf("版本快照内容不完整：%+v", ver)
	}
	if ver.AuditEntryID != 777 {
		t.Fatalf("审计 entry_id 未回填（存证与发布必须对得上）：%d", ver.AuditEntryID)
	}
	if reply.GetReused() || reply.GetAuditEntryId() != 777 {
		t.Fatalf("首发不该 reused，且要回 entry_id：%+v", reply)
	}
	if reply.GetItem().GetLatestVersion() != 1 || reply.GetItem().GetEpoch() != 1 {
		t.Fatalf("回参里的指针是旧的：%+v", reply.GetItem())
	}
	// 投影失效：只删 :item: 一个键，且必须落在自己的前缀下。
	requireOwnKeys(t, e.cache.dels, e.limits.keyPrefix, "发布删键")
	requireDeletedKeys(t, e.cache.dels, []string{e.limits.itemKey(model.ScopeAndroid, "home.title")}, "发布删键")
	if got := e.call("ConfigItem.AdvanceRelease"); got != 1 {
		t.Fatalf("指针推进次数应为 1，实得 %d", got)
	}
	if got := e.call("ConfigItem.AdvanceRelease.autocommit"); got != 0 {
		t.Fatalf("发布必须走事务版 AdvanceReleaseTx，不能用会自动提交的非事务版：%d", got)
	}
}

func TestPublishConfigOnExistingKeyGuardsExpectationAndType(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	before := e.effects()

	call := func(requestID string, valueType rpc.ConfigValueType, expect int64) error {
		_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
			Ctx: callCtx(requestID), CfgKey: "home.title", ValueType: valueType,
			Value: "v2", ExpectVersion: expect, Reason: "r",
		})
		return err
	}

	// 期望「新建」却发现键已存在：调用方状态过期，不是可以顺手覆盖的事。
	wantFail(t, call("req-2", vtString, 0), model.ErrConfigExists, "expect=0 而键已存在")

	// expect 与库里指针不一致：报冲突并带上双方版本号（运维要能看出落后多少）。
	err := call("req-3", vtString, 7)
	wantErr(t, err, model.ErrVersionConflict, "expect 落后")
	if !strings.Contains(err.Error(), "expect=7") || !strings.Contains(err.Error(), "latest=1") {
		t.Fatalf("冲突信息必须给出双方版本号，否则调用方不知道该重读到哪：%v", err)
	}

	// 值类型不可变：改类型等于把所有读取方按错的方式解析同一把键。
	wantFail(t, call("req-4", vtInt, 1), model.ErrValueTypeImmutable, "改值类型")
	// int 类型即便「数字值」也不放过：判定依据是声明类型而不是值的形状。
	wantFail(t, call("req-5", vtBool, 1), model.ErrValueTypeImmutable, "改成 bool")

	e.requireSameEffects(t, before, "被拒的发布必须零副作用")
	if n := e.versionCount(configID); n != 1 {
		t.Fatalf("被拒的发布改变了版本行数：%d", n)
	}
}

func TestPublishConfigInheritsValueTypeWhenOmittedAndAppendsNextVersion(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	reply, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-2"), CfgKey: "home.title", ValueType: vtNone,
		Value: "v2", ExpectVersion: 1, Reason: "第二次发布",
	})
	reply = wantOK(t, reply, err, "第二次发布")

	if got := reply.GetVersion(); got.GetVersion() != 2 ||
		got.GetChangeType() != model.ChangeTypePublish || got.GetValueType() != vtString {
		t.Fatalf("第二次发布应是 version=2 / publish / 沿用 string：%+v", got)
	}
	item := e.itemRow(configID)
	if item.LatestVersion != 2 || item.Epoch != 2 {
		t.Fatalf("指针与代次应各自 +1：%+v", item)
	}
	// 历史不可改写：v1 的原值必须还在。
	if v1 := e.versionRow(configID, 1); v1.Value != "v1" || v1.Version != 1 {
		t.Fatalf("v1 快照被改写：%+v", v1)
	}
}

func TestPublishConfigWithRolloutHoldsThePointerBack(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	reply := e.publishWithRules(t, "home.title", "v2", 1, "req-2", []*rpc.RolloutRuleSpec{{
		Name: "pct30", Mode: modePercentage, Percentage: 30, Remark: "小流量验证",
	}})

	item := e.itemRow(configID)
	if item.LatestVersion != 1 {
		t.Fatalf("挂了灰度规则的发布绝不能推进指针（端上此刻仍应读 v1）：%+v", item)
	}
	if item.Epoch != 1 {
		t.Fatalf("未推进指针就不该换代：%+v", item)
	}
	if reply.GetVersion().GetVersion() != 2 {
		t.Fatalf("版本行仍要追加（放量中的新值）：%+v", reply.GetVersion())
	}
	if len(reply.GetRules()) != 1 {
		t.Fatalf("回参要带上本次挂的规则：%+v", reply.GetRules())
	}
	rule := e.ruleRow(reply.GetRules()[0].GetRuleId())
	if rule.State != model.StateOff {
		t.Fatalf("新规则必须默认停用（保存不等于开闸）：%+v", rule)
	}
	if rule.Version != 2 || rule.ConfigID != configID {
		t.Fatalf("规则没挂到新版本上：%+v", rule)
	}
	if rule.Priority == 0 {
		t.Fatalf("缺省优先级的规则在「首个命中」判定里次序不定：%+v", rule)
	}
	if !strings.Contains(e.audit.reqs[1].GetEntry().GetAfterDigest(), "pointer_advanced=false") {
		t.Fatalf("审计摘要要如实记录「指针未推进」：%s", e.audit.reqs[1].GetEntry().GetAfterDigest())
	}
	// 灰度规则一律不缓存：带 start_at/end_at 的投影会让定时开闸延后一个 TTL 生效。
	for _, k := range e.cache.sets {
		if strings.Contains(k, ":rule:") || strings.Contains(k, ":rollout:") {
			t.Fatalf("灰度规则被写进了投影：%s", k)
		}
	}
}

func TestPublishConfigRuleShapeErrorPointsToTheOffendingIndex(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	before := e.effects()

	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-2"), CfgKey: "home.title", ValueType: vtString,
		Value: "v2", ExpectVersion: 1, Reason: "r",
		Rollout: []*rpc.RolloutRuleSpec{
			{Name: "ok", Mode: modeWhitelist, WhitelistMids: []int64{7}},
			{Name: "bad", Mode: modePercentage, Percentage: 0},
		},
	})
	wantErr(t, err, model.ErrRuleModeMismatch, "第二条规则形态不自洽")
	if !strings.Contains(err.Error(), "rollout[1]") {
		t.Fatalf("错误必须指到第几条规则，否则运营无法定位：%v", err)
	}
	e.requireSameEffects(t, before, "规则预检失败")
}

func TestPublishConfigUnknownPlatformInRuleIsRejectedNotIgnored(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()
	// 本项目只有四端（AGENTS.md §6）：第五个取值绝不能被当成「不限端」放过。
	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-p"), CfgKey: "home.p", ValueType: vtString, Value: "v", Reason: "r",
		Rollout: []*rpc.RolloutRuleSpec{{
			Name: "p", Mode: modePlatform,
			Platforms: []rpc.ClientPlatform{platAndroid, rpc.ClientPlatform(5)},
		}},
	})
	wantErr(t, err, model.ErrPlatformUnknown, "规则里混入未知端")
	if errors.Is(err, model.ErrPlatformRequired) {
		t.Fatalf("未知端被降级成了「未填端」：%v", err)
	}
	e.requireSameEffects(t, before, "规则端非法")

	// 数组里出现 0 是漏填，不是「不限端」：口径同样必须是拒绝。
	_, err = NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-p2"), CfgKey: "home.p", ValueType: vtString, Value: "v", Reason: "r",
		Rollout: []*rpc.RolloutRuleSpec{{
			Name: "p", Mode: modePlatform, Platforms: []rpc.ClientPlatform{rpc.ClientPlatform(0)},
		}},
	})
	wantErr(t, err, model.ErrPlatformUnknown, "显式给了 0 值端")
	e.requireSameEffects(t, before, "0 值端")
}

func TestPublishConfigWhitelistOverflowReportsItsOwnCode(t *testing.T) {
	e := newTestEnv(t)
	mids := make([]int64, 0, model.MaxWhitelistMids+1)
	for i := int64(1); i <= int64(model.MaxWhitelistMids)+1; i++ {
		mids = append(mids, i*7+1)
	}
	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-wl"), CfgKey: "home.wl", ValueType: vtString, Value: "v", Reason: "r",
		Rollout: []*rpc.RolloutRuleSpec{{
			Name: "wl", Mode: modeWhitelist, WhitelistMids: mids,
		}},
	})
	// 白名单是排障工具不是放量手段：超限要报自己的错误码，而不是笼统的「批量太大」。
	wantErr(t, err, model.ErrWhitelistTooLarge, "白名单超限")
	if errors.Is(err, model.ErrBatchTooLarge) {
		t.Fatalf("白名单超限被泛化成了批量错误，调用方看不出该改哪里：%v", err)
	}
}

func TestPublishConfigCasLossLeavesNoGhostVersion(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	before := e.effects()

	// 注入「条件 UPDATE 未命中」：等价于别人在这条事务推指针之前先发布了。
	e.db.failAdvanceRelease = true
	txMark := e.txMark()
	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-2"), CfgKey: "home.title", ValueType: vtString,
		Value: "v2", ExpectVersion: 1, Reason: "r",
	})
	wantErr(t, err, model.ErrVersionConflict, "CAS 输了")
	if got := e.txCallsSince(txMark); got != 1 {
		t.Fatalf("推进指针必须在事务里：本次调用开了 %d 个事务（应为 1，多出来的说明版本行与指针推进被拆成两条事务）", got)
	}
	// 关键断言：版本行也不能留下，否则库里多出一条「存在但从未生效」的幽灵版本。
	e.requireSameEffects(t, before, "CAS 输了")
	if item := e.itemRow(configID); item.LatestVersion != 1 || item.Epoch != 1 {
		t.Fatalf("指针被误动：%+v", item)
	}
}

func TestPublishConfigVersionRaceIsReportedAsConflict(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	before := e.effects()
	// 并发下 MAX(version)+1 可能撞 uniq_config_version：必须转成可重试的 ErrVersionConflict，
	// 而不是把 driver 专有的错误码原样抛给调用方。
	e.db.conflictOnInsert = model.ErrVersionExists
	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-2"), CfgKey: "home.title", ValueType: vtString,
		Value: "v2", ExpectVersion: 1, Reason: "r",
	})
	wantErr(t, err, model.ErrVersionConflict, "版本号竞争")
	if !errors.Is(err, model.ErrVersionExists) {
		t.Fatalf("转换时丢了原始原因（排障要能看到是撞了唯一键）：%v", err)
	}
	e.requireSameEffects(t, before, "版本号竞争")
}

func TestPublishConfigTransactionFailureLeavesNothingBehind(t *testing.T) {
	for _, step := range []string{"insert_version", "upsert_rule"} {
		t.Run(step, func(t *testing.T) {
			e := newTestEnv(t)
			e.publish(t, "home.title", "v1", 0, "req-1")
			before := e.effects()
			e.db.failInTx = step
			_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
				Ctx: callCtx("req-2"), CfgKey: "home.title", ValueType: vtString,
				Value: "v2", ExpectVersion: 1, Reason: "r",
				Rollout: []*rpc.RolloutRuleSpec{{
					Name: "w", Mode: modeWhitelist, WhitelistMids: []int64{9},
				}},
			})
			if err == nil {
				t.Fatal("事务内注入失败却回了成功")
			}
			if errors.Is(err, model.ErrVersionConflict) {
				t.Fatalf("注入错误被误判成乐观锁冲突：%v", err)
			}
			e.requireSameEffects(t, before, "事务失败")
			if dels := e.deletedKeysSince(before); len(dels) != 0 {
				t.Fatalf("发布没成功却删了读投影：%v", dels)
			}
			if len(e.audit.reqs) != before.audits {
				t.Fatalf("发布没成功却写了审计：%d 条（调用前 %d 条）", len(e.audit.reqs), before.audits)
			}
		})
	}
}

func TestPublishConfigIsIdempotentByRequestID(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	first := e.publishWithRules(t, "home.title", "v2", 1, "req-2", []*rpc.RolloutRuleSpec{{
		Name: "w", Mode: modeWhitelist, WhitelistMids: []int64{9},
	}})
	before := e.effects()
	inserts := e.call("ConfigVersion.InsertTx")

	for i := 0; i < 2; i++ {
		again, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
			// 入参（值 / expect / reason）与首次完全不同也必须走回放：request_id 才是幂等键。
			Ctx: callCtx("req-2"), CfgKey: "home.title", ValueType: vtString,
			Value: "完全不同的值", ExpectVersion: 99, Reason: "重放时的不同入参",
		})
		wantOK(t, again, err, "回放")
		if !again.GetReused() {
			t.Fatal("回放必须标 reused=true，调用方才分得清这是幂等结果")
		}
		if v := again.GetVersion(); v.GetVersion() != 2 || v.GetValue() != "v2" ||
			v.GetVersionId() != first.GetVersion().GetVersionId() {
			t.Fatalf("回放没回第一次的既成事实：%+v", v)
		}
		if len(again.GetRules()) != 1 {
			t.Fatalf("回放要把该版本已挂的规则一并回出：%+v", again.GetRules())
		}
	}
	if e.call("ConfigVersion.InsertTx") != inserts {
		t.Fatal("幂等回放里仍写了版本行")
	}
	e.requireSameEffects(t, before, "幂等回放零副作用")
}

func TestPublishConfigReplayRejectsHalfFinishedRelease(t *testing.T) {
	e := newTestEnv(t)
	// 现场：版本行写进去了，指针没动，且该版本一条规则都没挂 ⇒ 上次执行在两步之间被打断。
	item := e.seedItem(t, "home.title", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	e.seedVersion(t, item.ConfigID, 1, "v1", model.ValueTypeString, model.ChangeTypePublish, "req-half", "半条命令")

	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-half"), CfgKey: "home.title", ValueType: vtString,
		Value: "v1", Reason: "r",
	})
	wantErr(t, err, model.ErrReleaseIncomplete, "半发布不得被报成成功")
	if !strings.Contains(err.Error(), "home.title") {
		t.Fatalf("错误要带键名供排障：%v", err)
	}

	// 同一现场，只要该版本确实挂着灰度（放量中的合法状态），回放就该成功。
	e.seedRule(t, &model.RolloutRule{
		ConfigID: item.ConfigID, Version: 1, Name: "w", Mode: model.ModeWhitelist,
		WhitelistMids: model.IDListString([]int64{9}), State: model.StateOff,
	})
	reply, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-half"), CfgKey: "home.title", ValueType: vtString,
		Value: "v1", Reason: "r",
	})
	wantOK(t, reply, err, "带规则的放量中回放")
	if !reply.GetReused() || len(reply.GetRules()) != 1 {
		t.Fatalf("回放结果不完整：%+v", reply)
	}
}

func TestRollbackConfigAppendsNewVersionWithoutRewritingHistory(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	e.publish(t, "home.title", "v2", 1, "req-2")
	e.publish(t, "home.title", "v3", 2, "req-3")
	configID := e.itemByName("home.title").ConfigID

	before := e.effects()
	beforeV1 := *e.versionRow(configID, 1)
	reply, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-4"), CfgKey: "home.title", ToVersion: 1, Reason: "线上报错止血",
	})
	wantOK(t, reply, err, "回滚")

	item := e.itemRow(configID)
	if item.LatestVersion != 4 {
		t.Fatalf("回滚必须前进到新版本号（指针只会变大）：%+v", item)
	}
	ver := reply.GetVersion()
	if ver.GetVersion() != 4 || ver.GetChangeType() != model.ChangeTypeRollback || ver.GetRollbackFrom() != 1 {
		t.Fatalf("回滚行的版本号/change_type/rollback_from 不符：%+v", ver)
	}
	if ver.GetValue() != "v1" || ver.GetValueType() != vtString {
		t.Fatalf("回滚必须复用历史值：%+v", ver)
	}
	if after := *e.versionRow(configID, 1); after != beforeV1 {
		t.Fatalf("历史快照被改写：%+v vs %+v", after, beforeV1)
	}
	if item.Epoch != 4 {
		t.Fatalf("回滚也要换代（端上据此丢掉旧值）：epoch=%d", item.Epoch)
	}
	requireDeletedKeys(t, e.deletedKeysSince(before),
		[]string{e.limits.itemKey(model.ScopeGlobal, "home.title")}, "回滚删键")
}

// TestRollbackConfigClosesOpenRulesInTheSameTransaction 钉「三表联动」：
// 追加回滚版本 + 关掉这轮放量的规则 + 推进指针必须同生共死。
//
// 场景刻意把回滚目标指向**早于当前生效版本**的那一版（v1，指针在 v2）：
// 库里正在进行的是挂在 v3 上的灰度，止血要一并把它收回来。
// 「回滚到当前生效版本」（to_version == latest_version）不在本用例范围内 ——
// 那是空操作，由 ErrRollbackToLatest 拒掉，见下面的 Noop 用例与
// TestRollbackConfigRejectsRollbackToCurrentEvenWithOpenRules。
func TestRollbackConfigClosesOpenRulesInTheSameTransaction(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	e.publish(t, "home.title", "v2", 1, "req-2")
	e.publishWithRules(t, "home.title", "v3", 2, "req-3", []*rpc.RolloutRuleSpec{
		{Name: "w1", Mode: modeWhitelist, WhitelistMids: []int64{9}},
		{Name: "w2", Mode: modeWhitelist, WhitelistMids: []int64{10}},
	})
	rules := e.ruleIDsFor(configID, 3, 0)
	if len(rules) != 2 {
		t.Fatalf("前置规则没挂上：%v", rules)
	}
	if got := e.itemRow(configID).LatestVersion; got != 2 {
		t.Fatalf("前置指针应停在 v2（挂灰度的发布不推进指针）：%d", got)
	}
	e.openRule(t, rules[0], "req-open-1")
	e.openRule(t, rules[1], "req-open-2")
	e.closeRule(t, rules[1], "req-off-2") // 已停用的规则不该被回滚「重新打开」
	before := e.effects()

	e.db.failInTx = "set_state"
	txMark := e.txMark()
	_, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-4"), CfgKey: "home.title", ToVersion: 1, Reason: "止血",
	})
	if err == nil {
		t.Fatal("事务内关闸失败却回了成功")
	}
	if got := e.txCallsSince(txMark); got != 1 {
		t.Fatalf("插版本 / 关规则 / 推指针必须挤在同一条事务里：本次调用开了 %d 个事务", got)
	}
	// 三表联动（插版本 + 关规则 + 推指针）必须同生共死。
	e.requireSameEffects(t, before, "关闸失败要整事务回滚")
	if got := e.ruleIDsFor(configID, 3, model.StateOn); len(got) != 1 || got[0] != rules[0] {
		t.Fatalf("回滚失败后开着的规则集合被改了：%v", got)
	}
	if item := e.itemRow(configID); item.LatestVersion != 2 {
		t.Fatalf("回滚失败却推进了指针：%+v", item)
	}

	e.db.failInTx = ""
	reply, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-5"), CfgKey: "home.title", ToVersion: 1, Reason: "止血",
	})
	wantOK(t, reply, err, "回滚")
	if got := e.ruleIDsFor(configID, 3, model.StateOn); len(got) != 0 {
		t.Fatalf("回滚必须把这轮放量的规则全部关掉：%v", got)
	}
	if e.itemRow(configID).LatestVersion != 4 {
		t.Fatalf("回滚后指针应指向新追加的 v4：%+v", e.itemRow(configID))
	}
	if v := reply.GetVersion(); v.GetVersion() != 4 || v.GetRollbackFrom() != 1 {
		t.Fatalf("回滚版本行的版本号/来源不符：%+v", v)
	}
	// 留着 ON 规则等于把一部分人继续导回坏版本，所以回滚的 before 必须把它们数进去。
	if !strings.Contains(e.audit.reqs[len(e.audit.reqs)-1].GetEntry().GetBeforeDigest(), "open_rules=1") {
		t.Fatalf("审计 before 摘要要能还原当时开着几条规则：%s",
			e.audit.reqs[len(e.audit.reqs)-1].GetEntry().GetBeforeDigest())
	}
}

// TestRollbackConfigRejectsRollbackToCurrentEvenWithOpenRules 钉住一处容易被「顺手放宽」的判定：
// 即使还挂着在放量中的灰度规则，回滚到当前生效版本依旧是无效操作
// （model/errors.go：ErrRollbackToLatest「回滚目标已经是当前正式版本，属于无效操作」）。
// 把这轮放量收回去是 SetRolloutRuleState 的职责
// （proto PublishConfigReq：带 rollout 时 latest_version 保持不变，
// 由 SetRolloutRuleState 或再次全量发布收口），不能靠回滚顺带做。
func TestRollbackConfigRejectsRollbackToCurrentEvenWithOpenRules(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	e.publishWithRules(t, "home.title", "v2", 1, "req-2", []*rpc.RolloutRuleSpec{
		{Name: "w1", Mode: modeWhitelist, WhitelistMids: []int64{9}},
	})
	rules := e.ruleIDsFor(configID, 2, 0)
	e.openRule(t, rules[0], "req-open-1")
	before := e.effects()

	_, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-3"), CfgKey: "home.title", ToVersion: 1, Reason: "以为回滚能关闸",
	})
	wantErr(t, err, model.ErrRollbackToLatest, "开着灰度也不能回滚到当前版本")
	e.requireSameEffects(t, before, "回滚到当前版本被拒")
	if got := e.ruleIDsFor(configID, 2, model.StateOn); len(got) != 1 {
		t.Fatalf("被拒的回滚动到了规则：%v", got)
	}
}

func TestRollbackConfigRejectsNoopAndMissingTargets(t *testing.T) {
	e := newTestEnv(t)
	e.publish(t, "home.title", "v1", 0, "req-1")
	e.publish(t, "home.title", "v2", 1, "req-2")
	before := e.effects()

	cases := []struct {
		name string
		req  *rpc.RollbackConfigReq
		want error
	}{
		{"to_version 为 0", &rpc.RollbackConfigReq{Ctx: callCtx("r1"), CfgKey: "home.title", Reason: "x"}, model.ErrVersionNotFound},
		{"to_version 为负", &rpc.RollbackConfigReq{Ctx: callCtx("r2"), CfgKey: "home.title", ToVersion: -3, Reason: "x"}, model.ErrVersionNotFound},
		{"键不存在", &rpc.RollbackConfigReq{Ctx: callCtx("r3"), CfgKey: "home.none", ToVersion: 1, Reason: "x"}, model.ErrConfigNotFound},
		{"回滚到当前生效版本（空操作）", &rpc.RollbackConfigReq{Ctx: callCtx("r4"), CfgKey: "home.title", ToVersion: 2, Reason: "x"}, model.ErrRollbackToLatest},
		{"目标版本不存在", &rpc.RollbackConfigReq{Ctx: callCtx("r5"), CfgKey: "home.title", ToVersion: 99, Reason: "x"}, model.ErrVersionNotFound},
		{"原因为空", &rpc.RollbackConfigReq{Ctx: callCtx("r6"), CfgKey: "home.title", ToVersion: 1}, model.ErrReasonRequired},
		{"缺身份", &rpc.RollbackConfigReq{CfgKey: "home.title", ToVersion: 1, Reason: "x"}, model.ErrRequestIDRequired},
		{"scope 未知", &rpc.RollbackConfigReq{Ctx: callCtx("r7"), CfgKey: "home.title", Scope: "mini", ToVersion: 1, Reason: "x"}, model.ErrScopeUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(tc.req)
			wantFail(t, err, tc.want, tc.name)
			e.requireSameEffects(t, before, tc.name)
		})
	}
}

func TestRollbackConfigRefusesToGuessAnUnexplainableHistoricalValueType(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	e.publish(t, "home.title", "v2", 1, "req-2")
	// 现场：一条历史行的 value_type 被手工改成 9 —— 按哪个类型解释这段值都只能是猜。
	e.putRawVersion(&model.ConfigVersion{
		ConfigID: configID, Version: 3, Value: "x", ValueType: 9, ChangeType: model.ChangeTypePublish,
		RequestID: "req-tampered", Reason: "被改坏的历史", PublishedAt: model.NowUnix(),
	})
	before := e.effects()

	_, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-4"), CfgKey: "home.title", ToVersion: 3, Reason: "回滚到旧值",
	})
	wantErr(t, err, model.ErrValueTypeUnsupported, "历史类型不可解释")
	e.requireSameEffects(t, before, "历史类型不可解释")
}

func TestRollbackConfigReplayRules(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.title", "v1", 0, "req-1")
	configID := first.GetVersion().GetConfigId()
	e.publish(t, "home.title", "v2", 1, "req-2")
	reply, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-rb"), CfgKey: "home.title", ToVersion: 1, Reason: "止血",
	})
	wantOK(t, reply, err, "回滚")
	before := e.effects()

	again, err := NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-rb"), CfgKey: "home.title", ToVersion: 1, Reason: "重放",
	})
	wantOK(t, again, err, "回滚重放")
	if again.GetVersion().GetVersionId() != reply.GetVersion().GetVersionId() ||
		again.GetItem().GetLatestVersion() != 3 {
		t.Fatalf("重放产生了新版本：%+v", again.GetVersion())
	}
	if again.GetVersion().GetConfigId() != configID {
		t.Fatalf("重放回了另一个配置项的快照：%d != %d", again.GetVersion().GetConfigId(), configID)
	}
	e.requireSameEffects(t, before, "回滚重放零副作用")

	// 同一个 request_id 被发布用过：回滚不能借用，必须换新键。
	_, err = NewRollbackConfigLogic(bg(), e.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("req-1"), CfgKey: "home.title", ToVersion: 1, Reason: "借用发布的键",
	})
	wantErr(t, err, model.ErrRequestIDRequired, "request_id 不得跨动作复用")

	// 版本行在、指针没推进 ⇒ 上次回滚半途而废，不能报成功。
	e2 := newTestEnv(t)
	item := e2.seedItem(t, "home.title", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	e2.seedVersion(t, item.ConfigID, 1, "v1", model.ValueTypeString, model.ChangeTypeCreate, "s1", "种子")
	e2.seedVersion(t, item.ConfigID, 2, "v1", model.ValueTypeString, model.ChangeTypeRollback, "half-rb", "半条回滚")
	if got := e2.itemRow(item.ConfigID).LatestVersion; got != 0 {
		t.Fatalf("前置条件被破坏：latest=%d", got)
	}
	_, err = NewRollbackConfigLogic(bg(), e2.svc).RollbackConfig(&rpc.RollbackConfigReq{
		Ctx: callCtx("half-rb"), CfgKey: "home.title", ToVersion: 1, Reason: "重放",
	})
	wantErr(t, err, model.ErrReleaseIncomplete, "半条回滚不得报成功")
}

func TestWriteAuditGapsStayVisibleInsteadOfFabricated(t *testing.T) {
	cases := []struct {
		name  string
		setup func(e *testEnv)
	}{
		{"未配置 AuditRPC", func(e *testEnv) { e.svc.Audit = nil }},
		{"audit 调用失败", func(e *testEnv) { e.audit.err = errors.New("audit unavailable") }},
		{"audit 回包里没有 entry_id", func(e *testEnv) { e.audit.entryID = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			tc.setup(e)
			reply, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
				Ctx: callCtx("req-audit"), CfgKey: "home.audit", ValueType: vtString,
				Value: "v1", Reason: "缺存证演练",
			})
			if err != nil {
				t.Fatalf("审计缺口不该阻断发布：%v", err)
			}
			if reply.GetAuditEntryId() != 0 {
				t.Fatalf("绝不能伪造 entry_id：%d", reply.GetAuditEntryId())
			}
			// 发布本身仍然成功，且库里的 audit_entry_id 留在 0（补偿任务据此找活）。
			ver := e.versionRow(reply.GetVersion().GetConfigId(), 1)
			if ver == nil || ver.AuditEntryID != 0 {
				t.Fatalf("版本行的审计引用应为 0：%+v", ver)
			}
			if e.call("ConfigVersion.SetAuditEntry") != 0 {
				t.Fatal("entry_id=0 时不该去写审计引用（会把 0 当成有效值覆盖）")
			}
		})
	}
}

func TestPublishConfigAuditCarriesAttribution(t *testing.T) {
	e := newTestEnv(t)
	ctx := &rpc.CallContext{
		RequestId: "req-attr", OperatorId: 9, OperatorName: "运营乙",
		CallerService: "gateway/admin", TraceId: "trace-1", Ip: "10.0.0.1",
	}
	reply, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: ctx, CfgKey: "home.attr", ValueType: vtString, Value: "v1", Reason: "归因检查",
	})
	wantOK(t, reply, err, "发布")
	if len(e.audit.reqs) != 1 {
		t.Fatalf("审计条数 %d", len(e.audit.reqs))
	}
	req := e.audit.reqs[0]
	entry, actx := req.GetEntry(), req.GetCtx()
	if entry.GetEventId() != "ops-config:req-attr:publish" {
		t.Fatalf("event_id 形态决定了重放会不会多出一条存证：%q", entry.GetEventId())
	}
	if entry.GetAction() != "ops_config.publish" || entry.GetActionDomain() != "ops_config" {
		t.Fatalf("动作口径不符：%q/%q", entry.GetAction(), entry.GetActionDomain())
	}
	if entry.GetTargetType() != "ops_config:release" {
		t.Fatalf("目标类型不符：%q", entry.GetTargetType())
	}
	if !strings.HasSuffix(entry.GetTargetId(), "/1") {
		t.Fatalf("target_id 要指到 config_id/version：%q", entry.GetTargetId())
	}
	if entry.GetReason() != "归因检查" || entry.GetActorName() != "运营乙" || entry.GetActorId() != 9 {
		t.Fatalf("审计没带上「谁、为什么」：%+v", entry)
	}
	if actx.GetCallerService() != "gateway/admin" || actx.GetTraceId() != "trace-1" || actx.GetIp() != "10.0.0.1" {
		t.Fatalf("调用链信息未透传：%+v", actx)
	}
	if entry.GetSourceApp() != auditrpc.SourceApp_SOURCE_APP_ADMIN_WEB {
		t.Fatalf("后台来源应归到 admin_web：%v", entry.GetSourceApp())
	}
	if entry.GetResult() != auditrpc.AuditResult_AUDIT_RESULT_OK || entry.GetSchemaVersion() != 1 {
		t.Fatalf("结果/版本口径不符：%v/%d", entry.GetResult(), entry.GetSchemaVersion())
	}
}

func TestPublishConfigCreateVsExistingKeyExpectationMatrix(t *testing.T) {
	e := newTestEnv(t)
	item := e.seedItem(t, "home.title", model.ScopeGlobal, model.ValueTypeString, model.StateOn)
	// 库里有个从未发布过的键（latest_version=0）：expect=0 就是合法的「首次发布」。
	if _, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-c0"), CfgKey: "home.title", ValueType: vtString,
		Value: "v1", ExpectVersion: 0, Reason: "首发",
	}); err != nil {
		t.Fatalf("对 latest_version=0 的键首发被拒：%v", err)
	}
	// 「我要新建」+ 显式 expect>0 是矛盾入参：只能按「找不到待建对象」处理。
	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-c1"), CfgKey: "home.other", ValueType: vtString,
		Value: "v1", ExpectVersion: 3, Reason: "矛盾入参",
	})
	wantErr(t, err, model.ErrConfigNotFound, "新建却给了 expect")
	if n := e.versionCount(item.ConfigID); n != 1 {
		t.Fatalf("矛盾入参改变了版本行数：%d", n)
	}
}

// TestPublishConfigConcurrentFirstPublishDoesNotAdoptAnExistingKey 钉住最危险的一类并发：
// 两个进程同时首发同一个键。后来者绝不能「接管」别人刚建好的键（那等于覆盖别人的指针）。
func TestPublishConfigConcurrentFirstPublishDoesNotAdoptAnExistingKey(t *testing.T) {
	e := newTestEnv(t)
	first := e.publish(t, "home.race", "a", 0, "req-a")
	// 第二个进程手里还拿着「键不存在」的旧认知：expect=0。
	_, err := NewPublishConfigLogic(bg(), e.svc).PublishConfig(&rpc.PublishConfigReq{
		Ctx: callCtx("req-b"), CfgKey: "home.race", ValueType: vtString, Value: "b", Reason: "并发首发",
	})
	wantErr(t, err, model.ErrConfigExists, "并发首发")
	if got := e.versionCount(first.GetVersion().GetConfigId()); got != 1 {
		t.Fatalf("并发首发把别人的指针覆盖了：版本行数=%d", got)
	}
	if e.itemRow(first.GetVersion().GetConfigId()).LatestVersion != 1 {
		t.Fatal("并发首发的失败方动了指针")
	}
}
