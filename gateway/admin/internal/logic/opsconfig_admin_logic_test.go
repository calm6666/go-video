package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 ops-config 的口径：主体与会话归因、caller_service 固定值、
// 写接口 request_id 门槛、分页归一、闭集参数（state/target）、proto 枚举互转、
// 投影完整性（含 audit_entry_id 证据指针）与错误原样上抛。
//
// 「expect_version 是否冲突」「position 是否连续/是否超 capacity」「条目条数上限」
// 都是 ops-config 的领域规则，网关不复算（AGENTS.md §5/§9）：这里断言的是
// 「网关把声明原样交给下游」，而不是「网关自己判过」。打桩方式与 audit 测试一致，
// 内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库。

// opsAdminFake 是 ops-config RPC 的假客户端：记录入参、返回预置响应或预置错误。
type opsAdminFake struct {
	opsconfigrpc.OpsConfigClient

	err   error
	calls int

	listConfigsReq    *opsconfigrpc.ListConfigsReq
	listConfigsReply  *opsconfigrpc.ListConfigsReply
	listVersionsReq   *opsconfigrpc.ListConfigVersionsReq
	rolloutListReq    *opsconfigrpc.ListRolloutRulesReq
	getTopicReq       *opsconfigrpc.GetTopicReq
	getTopicReply     *opsconfigrpc.GetTopicReply
	listTopicsReq     *opsconfigrpc.ListTopicsReq
	listSlotsReq      *opsconfigrpc.ListSlotsReq
	listSwitchesReq   *opsconfigrpc.ListClientSwitchesReq
	publishReq        *opsconfigrpc.PublishConfigReq
	publishReply      *opsconfigrpc.PublishConfigReply
	rollbackReq       *opsconfigrpc.RollbackConfigReq
	saveRuleReq       *opsconfigrpc.SaveRolloutRuleReq
	setStateReq       *opsconfigrpc.SetRolloutRuleStateReq
	saveTopicReq      *opsconfigrpc.SaveTopicReq
	saveTopicItemsReq *opsconfigrpc.SaveTopicItemsReq
	saveSlotReq       *opsconfigrpc.SaveSlotReq
	saveSlotItemsReq  *opsconfigrpc.SaveSlotItemsReq
	saveSwitchReq     *opsconfigrpc.SaveClientSwitchReq
	refreshReq        *opsconfigrpc.RefreshCacheReq
	refreshReply      *opsconfigrpc.RefreshCacheReply
}

func (f *opsAdminFake) ListConfigs(_ context.Context, in *opsconfigrpc.ListConfigsReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ListConfigsReply, error) {
	f.calls++
	f.listConfigsReq = in
	if f.listConfigsReply != nil {
		return f.listConfigsReply, f.err
	}
	return &opsconfigrpc.ListConfigsReply{
		Items: []*opsconfigrpc.ConfigItem{{
			ConfigId: 11, CfgKey: "home.feed.page_size", Scope: "global",
			ValueType: opsconfigrpc.ConfigValueType_CONFIG_VALUE_TYPE_INT,
			Title:     "首页每页条数", State: 1, LatestVersion: 4, Epoch: 9, OperatorId: 77,
		}},
		Total: 1,
	}, f.err
}

func (f *opsAdminFake) BatchResolveConfig(_ context.Context, in *opsconfigrpc.BatchResolveConfigReq,
	_ ...grpc.CallOption) (*opsconfigrpc.BatchResolveConfigReply, error) {
	f.calls++
	return nil, f.err
}

func (f *opsAdminFake) ResolveConfig(_ context.Context, in *opsconfigrpc.ResolveConfigReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ResolveConfigReply, error) {
	f.calls++
	return nil, f.err
}

func (f *opsAdminFake) ListConfigVersions(_ context.Context, in *opsconfigrpc.ListConfigVersionsReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ListConfigVersionsReply, error) {
	f.calls++
	f.listVersionsReq = in
	return &opsconfigrpc.ListConfigVersionsReply{
		Items: []*opsconfigrpc.ConfigVersion{{
			VersionId: 41, ConfigId: 11, Version: 4, Value: "20", ChangeType: "rollback",
			RollbackFrom: 2, OperatorId: 77, OperatorName: "运营甲", Reason: "回滚",
			AuditEntryId: 9007199254740993, RequestId: "req-1", PublishedAt: 1700000000,
		}},
		Total: 1, LatestVersion: 4,
	}, f.err
}

func (f *opsAdminFake) ListRolloutRules(_ context.Context, in *opsconfigrpc.ListRolloutRulesReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ListRolloutRulesReply, error) {
	f.calls++
	f.rolloutListReq = in
	return &opsconfigrpc.ListRolloutRulesReply{
		Items: []*opsconfigrpc.RolloutRule{{
			RuleId: 61, ConfigId: 11, Version: 4, Name: "gray-10",
			Mode: opsconfigrpc.RolloutMode_ROLLOUT_MODE_PERCENTAGE, Percentage: 10,
			Platforms: []opsconfigrpc.ClientPlatform{
				opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID,
				opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_HARMONY,
			},
			MidSuffixes: "0,3", WhitelistMids: []int64{101, 102}, Priority: 1, State: 1,
		}},
		Total: 1,
	}, f.err
}

func (f *opsAdminFake) ListTopics(_ context.Context, in *opsconfigrpc.ListTopicsReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ListTopicsReply, error) {
	f.calls++
	f.listTopicsReq = in
	return &opsconfigrpc.ListTopicsReply{
		Items: []*opsconfigrpc.Topic{{TopicId: 21, Slug: "week-hot", State: 1, Version: 2, ZoneIds: []int64{1, 2}}},
		Total: 1,
	}, f.err
}

func (f *opsAdminFake) GetTopic(_ context.Context, in *opsconfigrpc.GetTopicReq,
	_ ...grpc.CallOption) (*opsconfigrpc.GetTopicReply, error) {
	f.calls++
	f.getTopicReq = in
	if f.getTopicReply != nil {
		return f.getTopicReply, f.err
	}
	return &opsconfigrpc.GetTopicReply{
		Topic: &opsconfigrpc.Topic{TopicId: 21, Slug: "week-hot", Title: "本周热播", Version: 2},
		Items: []*opsconfigrpc.TopicItem{{
			Id: 31, TopicId: 21, ItemType: "ugc_video", ItemId: "aid_1", Position: 1, State: 1,
		}},
		Found: true,
		Ttl:   60,
	}, f.err
}

func (f *opsAdminFake) ListSlots(_ context.Context, in *opsconfigrpc.ListSlotsReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ListSlotsReply, error) {
	f.calls++
	f.listSlotsReq = in
	return &opsconfigrpc.ListSlotsReply{
		Items: []*opsconfigrpc.RecommendSlot{{
			SlotId: 51, Code: "home.banner", Page: "home", Title: "首页横幅",
			Platforms: []opsconfigrpc.ClientPlatform{opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_IOS},
			Capacity:  6, State: 1, Version: 3,
		}},
		Total: 1,
	}, f.err
}

func (f *opsAdminFake) ListClientSwitches(_ context.Context, in *opsconfigrpc.ListClientSwitchesReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ListClientSwitchesReply, error) {
	f.calls++
	f.listSwitchesReq = in
	return &opsconfigrpc.ListClientSwitchesReply{
		Items: []*opsconfigrpc.ClientSwitch{{
			SwitchId: 71, SwitchKey: "vertical_feed",
			Platform:   opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID,
			MinVersion: "7.0.0", Enabled: 1, ConfigId: 11, Version: 2,
		}},
		Total: 1,
	}, f.err
}

func (f *opsAdminFake) PublishConfig(_ context.Context, in *opsconfigrpc.PublishConfigReq,
	_ ...grpc.CallOption) (*opsconfigrpc.PublishConfigReply, error) {
	f.calls++
	f.publishReq = in
	if f.publishReply != nil {
		return f.publishReply, f.err
	}
	return &opsconfigrpc.PublishConfigReply{
		Item:    &opsconfigrpc.ConfigItem{ConfigId: 11, CfgKey: "home.feed.page_size", LatestVersion: 5, Epoch: 10},
		Version: &opsconfigrpc.ConfigVersion{VersionId: 42, Version: 5, Value: "30", ChangeType: "publish", RequestId: "req-1"},
		Rules: []*opsconfigrpc.RolloutRule{{
			RuleId: 62, Version: 5, Name: "gray-10",
			Mode: opsconfigrpc.RolloutMode_ROLLOUT_MODE_PERCENTAGE, Percentage: 10,
		}},
		AuditEntryId: 9007199254740993,
		Reused:       true,
	}, f.err
}

func (f *opsAdminFake) RollbackConfig(_ context.Context, in *opsconfigrpc.RollbackConfigReq,
	_ ...grpc.CallOption) (*opsconfigrpc.RollbackConfigReply, error) {
	f.calls++
	f.rollbackReq = in
	return &opsconfigrpc.RollbackConfigReply{
		Item:         &opsconfigrpc.ConfigItem{ConfigId: 11, LatestVersion: 6},
		Version:      &opsconfigrpc.ConfigVersion{Version: 6, ChangeType: "rollback", RollbackFrom: 2},
		AuditEntryId: 0,
	}, f.err
}

func (f *opsAdminFake) SaveRolloutRule(_ context.Context, in *opsconfigrpc.SaveRolloutRuleReq,
	_ ...grpc.CallOption) (*opsconfigrpc.SaveRolloutRuleReply, error) {
	f.calls++
	f.saveRuleReq = in
	return &opsconfigrpc.SaveRolloutRuleReply{
		Rule: &opsconfigrpc.RolloutRule{RuleId: 63, Version: 4, Name: in.GetRule().GetName(),
			Mode: in.GetRule().GetMode(), Percentage: in.GetRule().GetPercentage()},
	}, f.err
}

func (f *opsAdminFake) SetRolloutRuleState(_ context.Context, in *opsconfigrpc.SetRolloutRuleStateReq,
	_ ...grpc.CallOption) (*opsconfigrpc.SetRolloutRuleStateReply, error) {
	f.calls++
	f.setStateReq = in
	return &opsconfigrpc.SetRolloutRuleStateReply{
		Rule: &opsconfigrpc.RolloutRule{RuleId: in.GetRuleId(), State: in.GetState()},
	}, f.err
}

func (f *opsAdminFake) SaveTopic(_ context.Context, in *opsconfigrpc.SaveTopicReq,
	_ ...grpc.CallOption) (*opsconfigrpc.SaveTopicReply, error) {
	f.calls++
	f.saveTopicReq = in
	return &opsconfigrpc.SaveTopicReply{
		Topic: &opsconfigrpc.Topic{TopicId: in.GetTopicId(), Slug: in.GetSlug(),
			Title: in.GetTitle(), State: in.GetState(), Version: 3},
	}, f.err
}

func (f *opsAdminFake) SaveTopicItems(_ context.Context, in *opsconfigrpc.SaveTopicItemsReq,
	_ ...grpc.CallOption) (*opsconfigrpc.SaveTopicItemsReply, error) {
	f.calls++
	f.saveTopicItemsReq = in
	return &opsconfigrpc.SaveTopicItemsReply{
		TopicId: in.GetTopicId(), Total: int32(len(in.GetItems())), AuditEntryId: 4401,
	}, f.err
}

func (f *opsAdminFake) SaveSlot(_ context.Context, in *opsconfigrpc.SaveSlotReq,
	_ ...grpc.CallOption) (*opsconfigrpc.SaveSlotReply, error) {
	f.calls++
	f.saveSlotReq = in
	return &opsconfigrpc.SaveSlotReply{
		Slot: &opsconfigrpc.RecommendSlot{SlotId: in.GetSlotId(), Code: in.GetCode(),
			Capacity: in.GetCapacity(), Platforms: in.GetPlatforms(), State: in.GetState(), Version: 4},
	}, f.err
}

func (f *opsAdminFake) SaveSlotItems(_ context.Context, in *opsconfigrpc.SaveSlotItemsReq,
	_ ...grpc.CallOption) (*opsconfigrpc.SaveSlotItemsReply, error) {
	f.calls++
	f.saveSlotItemsReq = in
	return &opsconfigrpc.SaveSlotItemsReply{
		SlotId: in.GetSlotId(), Total: int32(len(in.GetItems())), AuditEntryId: 4402,
	}, f.err
}

func (f *opsAdminFake) ResolveSlot(_ context.Context, in *opsconfigrpc.ResolveSlotReq,
	_ ...grpc.CallOption) (*opsconfigrpc.ResolveSlotReply, error) {
	f.calls++
	return nil, f.err
}

func (f *opsAdminFake) SaveClientSwitch(_ context.Context, in *opsconfigrpc.SaveClientSwitchReq,
	_ ...grpc.CallOption) (*opsconfigrpc.SaveClientSwitchReply, error) {
	f.calls++
	f.saveSwitchReq = in
	return &opsconfigrpc.SaveClientSwitchReply{
		Switch: &opsconfigrpc.ClientSwitch{SwitchId: in.GetSwitchId(), SwitchKey: in.GetSwitchKey(),
			Platform: in.GetPlatform(), Enabled: in.GetEnabled(), MinVersion: in.GetMinVersion(), Version: 5},
	}, f.err
}

func (f *opsAdminFake) RefreshCache(_ context.Context, in *opsconfigrpc.RefreshCacheReq,
	_ ...grpc.CallOption) (*opsconfigrpc.RefreshCacheReply, error) {
	f.calls++
	f.refreshReq = in
	if f.refreshReply != nil {
		return f.refreshReply, f.err
	}
	return &opsconfigrpc.RefreshCacheReply{Affected: 3, Epoch: 11, AuditEntryId: 4403}, f.err
}

// opsCtx 返回一个「已通过 AdminPermission 中间件」的请求上下文，用于验证会话覆盖。
func opsCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 77})
}

func TestOpsListConfigsRequiresConfiguredService(t *testing.T) {
	l := NewOpsConfigListConfigsLogic(context.Background(), &svc.ServiceContext{})
	if _, err := l.OpsConfigListConfigs(&types.ParamOpsListConfigs{}); err == nil ||
		err.Error() != "ops-config service not configured" {
		t.Fatalf("未配置 ops-config 时应返回明确错误，实际: %v", err)
	}
}

func TestOpsListConfigsMapsContextAndNormalizesPage(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsConfigListConfigsLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})

	resp, err := l.OpsConfigListConfigs(&types.ParamOpsListConfigs{
		Ctx:     types.OpsCallContext{OperatorId: 1, RequestId: "req-read"},
		Scope:   "global",
		Keyword: "feed",
		State:   1,
		Pn:      0,
		Ps:      5000,
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	in := fake.listConfigsReq
	if in.GetCtx().GetCallerService() != "gateway/admin" {
		t.Fatalf("caller_service 应为 gateway/admin，实际 %q", in.GetCtx().GetCallerService())
	}
	if in.GetCtx().GetOperatorId() != 77 {
		t.Fatalf("operator_id 应取会话身份 77，实际 %d", in.GetCtx().GetOperatorId())
	}
	if in.GetCtx().GetOperatorName() != "" {
		t.Fatalf("未声明 operator_name 时不应伪造，实际 %q", in.GetCtx().GetOperatorName())
	}
	if in.Pn != 1 || in.Ps != 100 {
		t.Fatalf("分页未归一: pn=%d ps=%d", in.Pn, in.Ps)
	}
	item := resp.Data.Items[0]
	if item.ConfigId != 11 || item.CfgKey != "home.feed.page_size" || item.LatestVersion != 4 || item.Epoch != 9 {
		t.Fatalf("配置项投影不完整: %+v", item)
	}
	if item.ValueType != int32(opsconfigrpc.ConfigValueType_CONFIG_VALUE_TYPE_INT) {
		t.Fatalf("value_type 枚举未转回 int32: %+v", item)
	}
	if resp.TTL != 0 {
		t.Fatalf("后台读接口的信封 ttl 固定 0，实际 %d", resp.TTL)
	}
}

func TestOpsListVersionsRequiresCfgKey(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsConfigListVersionsLogic(context.Background(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsConfigListVersions(&types.ParamOpsListConfigVersions{
		Ctx: types.OpsCallContext{OperatorId: 77},
	})
	if err == nil {
		t.Fatal("cfg_key 为空时应拒绝：全库版本列表没有可解释次序")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
}

func TestOpsListVersionsProjectsEvidencePointers(t *testing.T) {
	fake := &opsAdminFake{}
	vl := NewOpsConfigListVersionsLogic(context.Background(), &svc.ServiceContext{OpsConfig: fake})
	resp, err := vl.OpsConfigListVersions(&types.ParamOpsListConfigVersions{
		Ctx: types.OpsCallContext{OperatorId: 77}, CfgKey: "home.feed.page_size", Scope: "global",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	v := resp.Data.Items[0]
	// audit_entry_id 与 request_id 是「谁改的 / 哪次请求改的」的唯一线索，裁掉就找不回来了。
	if v.AuditEntryId != 9007199254740993 || v.RequestId != "req-1" {
		t.Fatalf("证据字段未透传: %+v", v)
	}
	if v.ChangeType != "rollback" || v.RollbackFrom != 2 {
		t.Fatalf("回滚来源未透传: %+v", v)
	}
	if resp.Data.LatestVersion != 4 {
		t.Fatalf("latest_version 未透传: %+v", resp.Data)
	}
}

func TestOpsRolloutRulesProjectPlatforms(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsListRolloutRulesLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	resp, err := l.OpsListRolloutRules(&types.ParamOpsListRolloutRules{
		Ctx: types.OpsCallContext{OperatorId: 77}, CfgKey: "home.feed.page_size", Version: 4, State: 1,
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	r := resp.Data.Items[0]
	if r.Mode != int32(opsconfigrpc.RolloutMode_ROLLOUT_MODE_PERCENTAGE) {
		t.Fatalf("mode 枚举未转回 int32: %+v", r)
	}
	want := []int32{
		int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID),
		int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_HARMONY),
	}
	if len(r.Platforms) != len(want) || r.Platforms[0] != want[0] || r.Platforms[1] != want[1] {
		t.Fatalf("platforms 投影不符: %+v", r.Platforms)
	}
	if len(r.WhitelistMids) != 2 || r.MidSuffixes != "0,3" {
		t.Fatalf("放量依据未透传: %+v", r)
	}
}

func TestOpsGetTopicRequiresLocator(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsGetTopicLogic(context.Background(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsGetTopic(&types.ParamOpsGetTopic{Ctx: types.OpsCallContext{OperatorId: 77}})
	if err == nil {
		t.Fatal("topic_id/slug 全空时应拒绝，否则下游无法定位")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
}

func TestOpsGetTopicKeepsFoundFalseAsSuccess(t *testing.T) {
	fake := &opsAdminFake{getTopicReply: &opsconfigrpc.GetTopicReply{Found: false}}
	l := NewOpsGetTopicLogic(context.Background(), &svc.ServiceContext{OpsConfig: fake})

	resp, err := l.OpsGetTopic(&types.ParamOpsGetTopic{
		Ctx: types.OpsCallContext{OperatorId: 77}, Slug: "not-exists", WithItems: true, ItemLimit: 20,
	})
	if err != nil {
		t.Fatalf("专题不存在不是错误: %v", err)
	}
	if resp.Data.Found {
		t.Fatal("found 应原样透传 false")
	}
	if resp.Data.Topic.TopicId != 0 || len(resp.Data.Items) != 0 {
		t.Fatalf("空专题应投影零值: %+v", resp.Data)
	}
	if fake.getTopicReq.GetSlug() != "not-exists" || !fake.getTopicReq.GetWithItems() ||
		fake.getTopicReq.GetItemLimit() != 20 {
		t.Fatalf("定位与条目参数未透传: %+v", fake.getTopicReq)
	}
}

func TestOpsGetTopicProjectsSuggestedTTLSeparately(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsGetTopicLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	resp, err := l.OpsGetTopic(&types.ParamOpsGetTopic{
		Ctx: types.OpsCallContext{OperatorId: 77}, TopicId: 21, WithItems: true,
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	// ops-config 的建议缓存秒数放在 data.ttl：外层信封 ttl 保持 0，避免两种缓存语义混用。
	if resp.Data.CacheTTL != 60 || resp.TTL != 0 {
		t.Fatalf("缓存建议未分离: data.ttl=%d envelope.ttl=%d", resp.Data.CacheTTL, resp.TTL)
	}
	if resp.Data.Items[0].ItemIid != "aid_1" {
		t.Fatalf("条目 item_id 投影键名不符: %+v", resp.Data.Items[0])
	}
}

func TestOpsPublishConfigRequiresRequestIdAndReason(t *testing.T) {
	// base 是「除被测字段外全部合法」的发布请求；每个用例只破坏一个必填项。
	cases := []struct {
		name   string
		ctx    types.OpsCallContext
		mutate func(p *types.ParamOpsPublishConfig)
	}{
		{"缺 request_id", types.OpsCallContext{OperatorId: 77}, nil},
		{"缺 reason", types.OpsCallContext{OperatorId: 77, RequestId: "req-1"},
			func(p *types.ParamOpsPublishConfig) { p.Reason = "  " }},
		{"负版本号", types.OpsCallContext{OperatorId: 77, RequestId: "req-1"},
			func(p *types.ParamOpsPublishConfig) { p.ExpectVersion = -1 }},
		{"缺 cfg_key", types.OpsCallContext{OperatorId: 77, RequestId: "req-1"},
			func(p *types.ParamOpsPublishConfig) { p.CfgKey = "" }},
	}
	for _, tc := range cases {
		fake := &opsAdminFake{}
		l := NewOpsPublishConfigLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
		req := types.ParamOpsPublishConfig{
			Ctx: tc.ctx, CfgKey: "home.feed.page_size", Value: "30", ExpectVersion: 4, Reason: "首页容量调整",
		}
		if tc.mutate != nil {
			tc.mutate(&req)
		}
		if _, err := l.OpsPublishConfig(&req); err == nil {
			t.Fatalf("%s 时应拒绝", tc.name)
		}
		if fake.calls != 0 {
			t.Fatalf("%s 时不应调用 ops-config，实际 %d 次", tc.name, fake.calls)
		}
	}
}

func TestOpsPublishConfigProjectsRolloutAndIdempotency(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsPublishConfigLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})

	resp, err := l.OpsPublishConfig(&types.ParamOpsPublishConfig{
		Ctx:           types.OpsCallContext{OperatorId: 1, RequestId: "req-1", TraceId: "tr-1", Ip: "10.0.0.1"},
		CfgKey:        "home.feed.page_size",
		ValueType:     int32(opsconfigrpc.ConfigValueType_CONFIG_VALUE_TYPE_INT),
		Value:         "30",
		ExpectVersion: 4,
		Reason:        "首页容量调整",
		Rollout: []types.OpsRolloutRuleSpec{{
			Name:       "gray-10",
			Mode:       int32(opsconfigrpc.RolloutMode_ROLLOUT_MODE_PERCENTAGE),
			Percentage: 10,
			Platforms:  []int32{int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID)},
			StartAt:    1700000000,
		}},
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	in := fake.publishReq
	// 会话覆盖冒名的 operator_id，并固定 caller_service。
	if in.GetCtx().GetOperatorId() != 77 || in.GetCtx().GetCallerService() != "gateway/admin" {
		t.Fatalf("上下文未按会话归因: %+v", in.GetCtx())
	}
	if in.GetCtx().GetTraceId() != "tr-1" || in.GetCtx().GetIp() != "10.0.0.1" {
		t.Fatalf("trace_id/ip 未透传给 audit: %+v", in.GetCtx())
	}
	if in.GetValueType() != opsconfigrpc.ConfigValueType_CONFIG_VALUE_TYPE_INT {
		t.Fatalf("value_type 未按 proto 类型转换: %+v", in)
	}
	spec := in.GetRollout()[0]
	if spec.GetMode() != opsconfigrpc.RolloutMode_ROLLOUT_MODE_PERCENTAGE ||
		spec.GetPlatforms()[0] != opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID ||
		spec.GetName() != "gray-10" {
		t.Fatalf("灰度声明未转换: %+v", spec)
	}
	// reused=true 表示 request_id 命中已完成的发布：这是幂等回放的唯一信号，必须透出。
	if !resp.Data.Reused || resp.Data.AuditEntryId != 9007199254740993 {
		t.Fatalf("幂等/审计引用未透出: %+v", resp.Data)
	}
	if resp.Data.Version.Version != 5 || resp.Data.Item.LatestVersion != 5 || len(resp.Data.Rules) != 1 {
		t.Fatalf("发布结果投影不完整: %+v", resp.Data)
	}
}

func TestOpsRollbackRequiresToVersionAndReason(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsRollbackConfigLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsRollbackConfig(&types.ParamOpsRollbackConfig{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-2"}, CfgKey: "k", ToVersion: 0, Reason: "回滚",
	})
	if err == nil {
		t.Fatal("to_version<=0 时应拒绝")
	}
	_, err = l.OpsRollbackConfig(&types.ParamOpsRollbackConfig{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-2"}, CfgKey: "k", ToVersion: 2, Reason: " ",
	})
	if err == nil {
		t.Fatal("reason 空白时应拒绝：回滚必须留下原因")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}

	resp, err := l.OpsRollbackConfig(&types.ParamOpsRollbackConfig{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-2"}, CfgKey: "k", ToVersion: 2, Reason: "线上异常回滚",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	// 回滚生成新版本而不是删历史：新版本号与 rollback_from 同时可见才是可审计的回滚。
	if resp.Data.Version.ChangeType != "rollback" || resp.Data.Version.RollbackFrom != 2 {
		t.Fatalf("回滚证据未透传: %+v", resp.Data.Version)
	}
	if resp.Data.AuditEntryId != 0 {
		t.Fatalf("audit_entry_id=0 应原样可见（表示待补偿），实际 %d", resp.Data.AuditEntryId)
	}
}

func TestOpsSaveRolloutRuleRequiresNameAndVersion(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsSaveRolloutRuleLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsSaveRolloutRule(&types.ParamOpsSaveRolloutRule{
		Ctx:     types.OpsCallContext{OperatorId: 77, RequestId: "req-3"},
		CfgKey:  "k",
		Version: 0,
		Rule:    types.OpsRolloutRuleSpec{Name: "r"},
	})
	if err == nil {
		t.Fatal("version=0 时应拒绝（规则必须挂到已发布版本）")
	}
	_, err = l.OpsSaveRolloutRule(&types.ParamOpsSaveRolloutRule{
		Ctx:     types.OpsCallContext{OperatorId: 77, RequestId: "req-3"},
		CfgKey:  "k",
		Version: 4,
		Rule:    types.OpsRolloutRuleSpec{Percentage: 20},
	})
	if err == nil {
		t.Fatal("rule.name 为空时应拒绝：它是 upsert 的幂等句柄")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	if _, err := l.OpsSaveRolloutRule(&types.ParamOpsSaveRolloutRule{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-3"}, CfgKey: "k", Version: 4,
		Rule: types.OpsRolloutRuleSpec{Name: "gray", Mode: int32(opsconfigrpc.RolloutMode_ROLLOUT_MODE_FULL)},
	}); err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if fake.saveRuleReq.GetRule().GetMode() != opsconfigrpc.RolloutMode_ROLLOUT_MODE_FULL {
		t.Fatalf("mode 未转换: %+v", fake.saveRuleReq.GetRule())
	}
}

func TestOpsSetRolloutStateRejectsUnknownState(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsSetRolloutStateLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	for _, bad := range []int32{0, 3} {
		_, err := l.OpsSetRolloutState(&types.ParamOpsSetRolloutState{
			Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-4"}, RuleId: 61, State: bad, Reason: "放量收口",
		})
		if err == nil {
			t.Fatalf("state=%d 应拒绝", bad)
		}
	}
	if _, err := l.OpsSetRolloutState(&types.ParamOpsSetRolloutState{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-4"}, RuleId: 0, State: 2, Reason: "放量收口",
	}); err == nil {
		t.Fatal("rule_id=0 应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	resp, err := l.OpsSetRolloutState(&types.ParamOpsSetRolloutState{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-4"}, RuleId: 61, State: 2, Reason: "放量收口",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if resp.Data.Rule.RuleId != 61 || resp.Data.Rule.State != 2 {
		t.Fatalf("状态切换结果未回读: %+v", resp.Data.Rule)
	}
}

func TestOpsSaveTopicRequiresSlugOnCreate(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsSaveTopicLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsSaveTopic(&types.ParamOpsSaveTopic{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-5"}, Title: "本周热播", Reason: "新建",
	})
	if err == nil {
		t.Fatal("新建（topic_id=0）缺 slug 时应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	// 更新时 slug 可空（不改寻址句柄），zone_ids/tag_ids 原样透传为引用集合。
	if _, err := l.OpsSaveTopic(&types.ParamOpsSaveTopic{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-5"}, TopicId: 21, Title: "本周热播",
		ZoneIds: []int64{1, 2}, TagIds: []int64{9}, ExpectVersion: 2, Reason: "调整分区引用",
	}); err != nil {
		t.Fatalf("合法更新不应失败: %v", err)
	}
	in := fake.saveTopicReq
	if in.GetSlug() != "" || in.GetTopicId() != 21 || len(in.GetZoneIds()) != 2 || in.GetExpectVersion() != 2 {
		t.Fatalf("更新参数未原样透传: %+v", in)
	}
}

func TestOpsSaveTopicItemsRejectsEmptyAndKeepsFullReplace(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsSaveTopicItemsLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsSaveTopicItems(&types.ParamOpsSaveTopicItems{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-6"}, TopicId: 21, Reason: "清空",
	})
	if err == nil {
		t.Fatal("items 为空应拒绝：空列表等于清空整个专题")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	_, err = l.OpsSaveTopicItems(&types.ParamOpsSaveTopicItems{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-6"}, TopicId: 21, Reason: "换入新内容",
		Items: []types.OpsTopicItemSpec{
			{ItemType: "ugc_video", ItemIid: "aid_1", Position: 1},
			{ItemType: "pgc_season", Position: 2},
		},
	})
	if err == nil {
		t.Fatal("条目缺 item_id 应拒绝：专题只存引用，缺引用定位不到内容")
	}
	if fake.calls != 0 {
		t.Fatalf("条目形状非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	resp, err := l.OpsSaveTopicItems(&types.ParamOpsSaveTopicItems{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-6"}, TopicId: 21, Reason: "换入新内容",
		Items: []types.OpsTopicItemSpec{
			{ItemType: "ugc_video", ItemIid: "aid_1", Position: 1},
			{ItemType: "pgc_season", ItemIid: "s_2", Position: 2, State: 1},
		},
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	in := fake.saveTopicItemsReq
	// 全量覆盖不携带 id/topic_id：归属由服务端按 topic_id 写入，网关代填会绕过归属校验。
	if in.GetItems()[0].GetId() != 0 || in.GetItems()[0].GetTopicId() != 0 {
		t.Fatalf("网关不应代填条目归属: %+v", in.GetItems()[0])
	}
	if in.GetItems()[1].GetItemId() != "s_2" || in.GetItems()[1].GetPosition() != 2 {
		t.Fatalf("条目声明未原样透传: %+v", in.GetItems()[1])
	}
	if resp.Data.Total != 2 || resp.Data.AuditEntryId != 4401 {
		t.Fatalf("覆盖结果未回读: %+v", resp.Data)
	}
}

func TestOpsSaveSlotRejectsNegativeCapacity(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsSaveSlotLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsSaveSlot(&types.ParamOpsSaveSlot{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-7"}, Code: "home.banner",
		Title: "首页横幅", Capacity: -1, Reason: "扩容",
	})
	if err == nil {
		t.Fatal("capacity<0 应拒绝")
	}
	_, err = l.OpsSaveSlot(&types.ParamOpsSaveSlot{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-7"}, Code: "",
		Title: "首页横幅", Reason: "扩容",
	})
	if err == nil {
		t.Fatal("code 为空应拒绝：端上按 code 取位")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	if _, err := l.OpsSaveSlot(&types.ParamOpsSaveSlot{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-7"}, Code: "home.banner",
		Title: "首页横幅", Platforms: []int32{1, 4}, Capacity: 6, ExpectVersion: 3, Reason: "扩容",
	}); err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	in := fake.saveSlotReq
	if len(in.GetPlatforms()) != 2 ||
		in.GetPlatforms()[1] != opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_DESKTOP {
		t.Fatalf("platforms 未转成 proto 枚举: %+v", in.GetPlatforms())
	}
}

func TestOpsSaveSlotItemsValidatesItemShape(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsSaveSlotItemsLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	cases := []struct {
		name  string
		items []types.OpsSlotItemSpec
	}{
		{"空列表", nil},
		{"负排期时间", []types.OpsSlotItemSpec{{ItemType: "ugc_video", ItemIid: "a", Position: 1, StartAt: -1}}},
		{"缺引用", []types.OpsSlotItemSpec{{ItemType: "ugc_video", Position: 1}}},
	}
	for _, tc := range cases {
		_, err := l.OpsSaveSlotItems(&types.ParamOpsSaveSlotItems{
			Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-8"}, SlotId: 51, Items: tc.items, Reason: "排期",
		})
		if err == nil {
			t.Fatalf("%s 应拒绝", tc.name)
		}
		if fake.calls != 0 {
			t.Fatalf("%s 时不应调用 ops-config，实际 %d 次", tc.name, fake.calls)
		}
	}
	resp, err := l.OpsSaveSlotItems(&types.ParamOpsSaveSlotItems{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-8"}, SlotId: 51, Reason: "排期",
		Items: []types.OpsSlotItemSpec{{
			ItemType: "pgc_episode", ItemIid: "ep_1", Position: 1, Weight: 10,
			StartAt: 1700000000, EndAt: 1700086400, State: 1,
		}},
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	in := fake.saveSlotItemsReq.GetItems()[0]
	if in.GetId() != 0 || in.GetSlotId() != 0 {
		t.Fatalf("网关不应代填坑位条目归属: %+v", in)
	}
	if in.GetWeight() != 10 || in.GetEndAt() != 1700086400 || in.GetItemId() != "ep_1" {
		t.Fatalf("排期参数未原样透传: %+v", in)
	}
	if resp.Data.Total != 1 || resp.Data.AuditEntryId != 4402 {
		t.Fatalf("覆盖结果未回读: %+v", resp.Data)
	}
}

func TestOpsSaveSwitchRequiresPlatform(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsSaveSwitchLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsSaveSwitch(&types.ParamOpsSaveSwitch{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-9"}, SwitchKey: "vertical_feed",
		Platform: 0, Reason: "开放竖屏",
	})
	if err == nil {
		t.Fatal("platform  unspecified 应拒绝：upsert 键是 (switch_key, platform)")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	resp, err := l.OpsSaveSwitch(&types.ParamOpsSaveSwitch{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-9"}, SwitchKey: "vertical_feed",
		Platform: int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID), MinVersion: "7.0.0",
		Enabled: 1, ConfigId: 11, ExpectVersion: 4, Reason: "开放竖屏",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if fake.saveSwitchReq.GetPlatform() != opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID {
		t.Fatalf("platform 未转成 proto 枚举: %+v", fake.saveSwitchReq)
	}
	if resp.Data.Switch.Platform != int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID) ||
		resp.Data.Switch.Enabled != 1 || resp.Data.Switch.Version != 5 {
		t.Fatalf("开关结果未回读: %+v", resp.Data.Switch)
	}
}

func TestOpsRefreshCacheRejectsUnknownTarget(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsRefreshCacheLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	// 缓存失效参数写错会「成功但什么都没失效」，属于必须卡在网关的静默失败。
	for _, bad := range []string{"", "configs", "ALL", "zone"} {
		_, err := l.OpsRefreshCache(&types.ParamOpsRefreshCache{
			Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-10"}, Target: bad, Reason: "故障兜底",
		})
		if err == nil {
			t.Fatalf("target=%q 应拒绝", bad)
		}
	}
	_, err := l.OpsRefreshCache(&types.ParamOpsRefreshCache{
		Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-10"}, Target: "all", Reason: "  ",
	})
	if err == nil {
		t.Fatal("reason 空白应拒绝：整域失效必须留下原因")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	for _, ok := range []string{"config", "topic", "slot", "all"} {
		if _, err := l.OpsRefreshCache(&types.ParamOpsRefreshCache{
			Ctx: types.OpsCallContext{OperatorId: 77, RequestId: "req-10"}, Target: ok, Reason: "手动收敛多实例",
		}); err != nil {
			t.Fatalf("target=%s 合法: %v", ok, err)
		}
	}
	if fake.calls != 4 {
		t.Fatalf("合法 target 应各调一次，实际 %d", fake.calls)
	}
}

func TestOpsPropagatesDownstreamErrorVerbatim(t *testing.T) {
	sentinel := errors.New("rpc error: code = FailedPrecondition desc = expect_version conflict")
	fake := &opsAdminFake{err: sentinel}
	l := NewOpsPublishConfigLogic(opsCtx(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsPublishConfig(&types.ParamOpsPublishConfig{
		Ctx:           types.OpsCallContext{OperatorId: 77, RequestId: "req-11"},
		CfgKey:        "k",
		Value:         "1",
		ExpectVersion: 4,
		Reason:        "并发更新冲突场景",
	})
	// 乐观锁冲突/未实现等业务错误必须原样上抛，网关不得折成 code=0 的空响应。
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误应原样上抛，实际 %v", err)
	}
}

func TestOpsListReadsRequireOperator(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsListTopicsLogic(context.Background(), &svc.ServiceContext{OpsConfig: fake})
	_, err := l.OpsListTopics(&types.ParamOpsListTopics{})
	if err == nil {
		t.Fatal("既无会话又无 operator_id 时应拒绝：读接口也要有主体")
	}
	if fake.calls != 0 {
		t.Fatalf("主体缺失时不应调用 ops-config，实际 %d 次", fake.calls)
	}
	if _, err := l.OpsListTopics(&types.ParamOpsListTopics{
		Ctx: types.OpsCallContext{OperatorId: 77}, OnlineOnly: true, ZoneId: 3, Pn: 1, Ps: 200,
	}); err != nil {
		t.Fatalf("带主体的读取不应失败: %v", err)
	}
	in := fake.listTopicsReq
	if !in.GetOnlineOnly() || in.GetZoneId() != 3 || in.GetPs() != 100 || in.GetCtx().GetOperatorId() != 77 {
		t.Fatalf("读取参数未归一: %+v", in)
	}
}

func TestOpsListSlotsAndSwitchesConvertPlatform(t *testing.T) {
	fake := &opsAdminFake{}
	l := NewOpsListSlotsLogic(context.Background(), &svc.ServiceContext{OpsConfig: fake})
	resp, err := l.OpsListSlots(&types.ParamOpsListSlots{
		Ctx: types.OpsCallContext{OperatorId: 77}, Page: "home", Platform: int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_IOS),
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if fake.listSlotsReq.GetPlatform() != opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_IOS {
		t.Fatalf("查询侧 platform 未转换: %+v", fake.listSlotsReq)
	}
	if resp.Data.Items[0].Platforms[0] != int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_IOS) {
		t.Fatalf("响应侧 platforms 未转回 int32: %+v", resp.Data.Items[0])
	}

	sl := NewOpsListSwitchesLogic(context.Background(), &svc.ServiceContext{OpsConfig: fake})
	sresp, err := sl.OpsListSwitches(&types.ParamOpsListSwitches{
		Ctx: types.OpsCallContext{OperatorId: 77}, Platform: int32(opsconfigrpc.ClientPlatform_CLIENT_PLATFORM_ANDROID),
		Enabled: 1, SwitchKey: "vertical_feed",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if fake.listSwitchesReq.GetSwitchKey() != "vertical_feed" || fake.listSwitchesReq.GetEnabled() != 1 {
		t.Fatalf("开关过滤条件未透传: %+v", fake.listSwitchesReq)
	}
	if sresp.Data.Items[0].MinVersion != "7.0.0" || sresp.Data.Items[0].ConfigId != 11 {
		t.Fatalf("开关投影不完整: %+v", sresp.Data.Items[0])
	}
}
