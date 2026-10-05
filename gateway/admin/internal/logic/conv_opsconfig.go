// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// ops-config RPC → 管理后台投影 + 调用上下文装配。
//
// 与 conv_audit.go 同一套职责边界（AGENTS.md §4/§5）：
//  1. 装配 CallContext：caller_service 固定为 gateway/admin，operator_id 以 AdminPermission
//     中间件解析出的会话身份为准，写接口强制 request_id（proto 注释「所有写方法必填」）；
//  2. 只卡门槛不复述规则：版本号非负、时间窗非负、分页收敛到 100；「expect_version 是否冲突」
//     「position 是否连续」「capacity 是否超限」全部由 ops-config 判定，网关不重复实现一遍，
//     否则两边规则一旦漂移就会出现网关放行、服务端拒绝的假成功路径；
//  3. 投影：审计引用字段 audit_entry_id 必须逐条透传，它是「谁改了这行」的证据指针，
//     0 表示审计补偿待写，后台要能区分「没有证据」和「证据还没落库」。

package logic

import (
	"context"
	"errors"

	"go-video/common/validation"
	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// opsCallerService 是网关在 ops-config CallContext 里的自报身份，不接受客户端声明。
const opsCallerService = "gateway/admin"

// opsMaxPageSize 对齐 ops-config 各 List 请求的 ps 上限（proto 注释「上限 100」）。
const opsMaxPageSize = 100

// errOpsServiceNotConfigured：未配置 OpsConfigRPC 时受影响的 /admin/ops/* 路由一律返回它，
// 不退化成伪造空列表——那会让后台把「下游没接」误读成「配置项不存在」。
var errOpsServiceNotConfigured = errors.New("ops-config service not configured")

// errOpsExpectVersionInvalid 拦住负版本号：0 是 ops-config 的「新建」语义，负数没有对应状态。
var errOpsExpectVersionInvalid = errors.New("gateway/admin: expect_version must be >= 0")

// normalizeOpsPage 复用 common/validation.NormalizePage（pn<=0→1、ps<=0→20、ps>100→100）。
func normalizeOpsPage(pn, ps int32) (int32, int32) {
	page := validation.NormalizePage(int(pn), int(ps), opsMaxPageSize)
	return int32(page.Page), int32(page.PageSize)
}

// opsCallContext 装配 opsconfig.v1.CallContext。write=true 时 request_id 必填。
//
// operator_id 以会话为准：客户端声称的 operator_id 只在与会话不一致时留痕（与 operation、
// audit 两个域同一口径），避免后台表单把自己写成别人。
func opsCallContext(ctx context.Context, cc types.OpsCallContext, write bool) (*opsconfigrpc.CallContext, error) {
	adminID := cc.OperatorId
	if id, ok := middleware.AdminFromContext(ctx); ok {
		if cc.OperatorId > 0 && cc.OperatorId != id.AdminID {
			logx.WithContext(ctx).Errorf("gateway/admin/ops: operator_id mismatch session=%d claimed=%d",
				id.AdminID, cc.OperatorId)
		}
		adminID = id.AdminID
	}
	if err := requireOperatorID(adminID); err != nil {
		return nil, err
	}
	if write {
		if err := requireNonEmpty("ctx.request_id", cc.RequestId); err != nil {
			return nil, err
		}
	}
	return &opsconfigrpc.CallContext{
		CallerService: opsCallerService,
		OperatorId:    adminID,
		OperatorName:  cc.OperatorName,
		RequestId:     cc.RequestId,
		TraceId:       cc.TraceId,
		Ip:            cc.Ip,
	}, nil
}

// opsTimeWindow 只校验「时间戳不为负」；生效窗口是否自洽（start<end、是否已过期）由 ops-config 判定。
func opsTimeWindow(startAt, endAt int64) error {
	if startAt < 0 || endAt < 0 {
		return errors.New("gateway/admin: start_at/end_at must be >= 0")
	}
	return nil
}

// opsPlatformsToRPC 把端枚举值原样转成 protobuf 枚举；空切片保持 nil，
// 语义是「不限端」而不是「零个端」。
func opsPlatformsToRPC(list []int32) []opsconfigrpc.ClientPlatform {
	if len(list) == 0 {
		return nil
	}
	out := make([]opsconfigrpc.ClientPlatform, 0, len(list))
	for _, p := range list {
		out = append(out, opsconfigrpc.ClientPlatform(p))
	}
	return out
}

func opsPlatformsFromRPC(list []opsconfigrpc.ClientPlatform) []int32 {
	if len(list) == 0 {
		return nil
	}
	out := make([]int32, 0, len(list))
	for _, p := range list {
		out = append(out, int32(p))
	}
	return out
}

// ---------------------------------------------------------------- 配置项与版本

func opsConfigItemToAPI(i *opsconfigrpc.ConfigItem) types.OpsConfigItem {
	if i == nil {
		return types.OpsConfigItem{}
	}
	return types.OpsConfigItem{
		ConfigId:      i.GetConfigId(),
		CfgKey:        i.GetCfgKey(),
		Scope:         i.GetScope(),
		ValueType:     int32(i.GetValueType()),
		Title:         i.GetTitle(),
		Description:   i.GetDescription(),
		State:         i.GetState(),
		LatestVersion: i.GetLatestVersion(),
		Epoch:         i.GetEpoch(),
		OperatorId:    i.GetOperatorId(),
		Ctime:         i.GetCtime(),
		Mtime:         i.GetMtime(),
	}
}

func opsConfigItemsToAPI(list []*opsconfigrpc.ConfigItem) []types.OpsConfigItem {
	out := make([]types.OpsConfigItem, 0, len(list))
	for _, i := range list {
		out = append(out, opsConfigItemToAPI(i))
	}
	return out
}

func opsConfigVersionToAPI(v *opsconfigrpc.ConfigVersion) types.OpsConfigVersion {
	if v == nil {
		return types.OpsConfigVersion{}
	}
	return types.OpsConfigVersion{
		VersionId:    v.GetVersionId(),
		ConfigId:     v.GetConfigId(),
		Version:      v.GetVersion(),
		Value:        v.GetValue(),
		ValueType:    int32(v.GetValueType()),
		ChangeType:   v.GetChangeType(),
		RollbackFrom: v.GetRollbackFrom(),
		OperatorId:   v.GetOperatorId(),
		OperatorName: v.GetOperatorName(),
		Reason:       v.GetReason(),
		AuditEntryId: v.GetAuditEntryId(),
		RequestId:    v.GetRequestId(),
		PublishedAt:  v.GetPublishedAt(),
		Ctime:        v.GetCtime(),
	}
}

func opsConfigVersionsToAPI(list []*opsconfigrpc.ConfigVersion) []types.OpsConfigVersion {
	out := make([]types.OpsConfigVersion, 0, len(list))
	for _, v := range list {
		out = append(out, opsConfigVersionToAPI(v))
	}
	return out
}

// ---------------------------------------------------------------- 灰度规则

func opsRolloutRuleSpecToRPC(s types.OpsRolloutRuleSpec) *opsconfigrpc.RolloutRuleSpec {
	return &opsconfigrpc.RolloutRuleSpec{
		Name:          s.Name,
		Mode:          opsconfigrpc.RolloutMode(s.Mode),
		Percentage:    s.Percentage,
		AppVersionMin: s.AppVersionMin,
		AppVersionMax: s.AppVersionMax,
		Platforms:     opsPlatformsToRPC(s.Platforms),
		MidSuffixes:   s.MidSuffixes,
		WhitelistMids: s.WhitelistMids,
		Priority:      s.Priority,
		Remark:        s.Remark,
		StartAt:       s.StartAt,
		EndAt:         s.EndAt,
	}
}

func opsRolloutRuleSpecsToRPC(list []types.OpsRolloutRuleSpec) []*opsconfigrpc.RolloutRuleSpec {
	if len(list) == 0 {
		return nil
	}
	out := make([]*opsconfigrpc.RolloutRuleSpec, 0, len(list))
	for _, s := range list {
		out = append(out, opsRolloutRuleSpecToRPC(s))
	}
	return out
}

func opsRolloutRuleToAPI(r *opsconfigrpc.RolloutRule) types.OpsRolloutRule {
	if r == nil {
		return types.OpsRolloutRule{}
	}
	return types.OpsRolloutRule{
		RuleId:        r.GetRuleId(),
		ConfigId:      r.GetConfigId(),
		Version:       r.GetVersion(),
		Name:          r.GetName(),
		Mode:          int32(r.GetMode()),
		Percentage:    r.GetPercentage(),
		AppVersionMin: r.GetAppVersionMin(),
		AppVersionMax: r.GetAppVersionMax(),
		Platforms:     opsPlatformsFromRPC(r.GetPlatforms()),
		MidSuffixes:   r.GetMidSuffixes(),
		WhitelistMids: r.GetWhitelistMids(),
		Priority:      r.GetPriority(),
		State:         r.GetState(),
		OperatorId:    r.GetOperatorId(),
		Remark:        r.GetRemark(),
		StartAt:       r.GetStartAt(),
		EndAt:         r.GetEndAt(),
		Ctime:         r.GetCtime(),
		Mtime:         r.GetMtime(),
	}
}

func opsRolloutRulesToAPI(list []*opsconfigrpc.RolloutRule) []types.OpsRolloutRule {
	out := make([]types.OpsRolloutRule, 0, len(list))
	for _, r := range list {
		out = append(out, opsRolloutRuleToAPI(r))
	}
	return out
}

// ---------------------------------------------------------------- 专题

func opsTopicToAPI(t *opsconfigrpc.Topic) types.OpsTopic {
	if t == nil {
		return types.OpsTopic{}
	}
	return types.OpsTopic{
		TopicId:     t.GetTopicId(),
		Slug:        t.GetSlug(),
		Title:       t.GetTitle(),
		Description: t.GetDescription(),
		Cover:       t.GetCover(),
		ZoneIds:     t.GetZoneIds(),
		TagIds:      t.GetTagIds(),
		State:       t.GetState(),
		Sort:        t.GetSort(),
		StartAt:     t.GetStartAt(),
		EndAt:       t.GetEndAt(),
		Version:     t.GetVersion(),
		OperatorId:  t.GetOperatorId(),
		Ctime:       t.GetCtime(),
		Mtime:       t.GetMtime(),
	}
}

func opsTopicsToAPI(list []*opsconfigrpc.Topic) []types.OpsTopic {
	out := make([]types.OpsTopic, 0, len(list))
	for _, t := range list {
		out = append(out, opsTopicToAPI(t))
	}
	return out
}

func opsTopicItemToAPI(i *opsconfigrpc.TopicItem) types.OpsTopicItem {
	if i == nil {
		return types.OpsTopicItem{}
	}
	return types.OpsTopicItem{
		Id:         i.GetId(),
		TopicId:    i.GetTopicId(),
		ItemType:   i.GetItemType(),
		ItemIid:    i.GetItemId(),
		Position:   i.GetPosition(),
		State:      i.GetState(),
		OperatorId: i.GetOperatorId(),
		Ctime:      i.GetCtime(),
		Mtime:      i.GetMtime(),
	}
}

func opsTopicItemsToAPI(list []*opsconfigrpc.TopicItem) []types.OpsTopicItem {
	out := make([]types.OpsTopicItem, 0, len(list))
	for _, i := range list {
		out = append(out, opsTopicItemToAPI(i))
	}
	return out
}

// opsTopicItemsToRPC 把后台条目声明转成 TopicItem：全量覆盖语义下不携带 id/topic_id，
// 归属由服务端按 req.topic_id 写入（网关代填会绕过归属校验）。
func opsTopicItemsToRPC(list []types.OpsTopicItemSpec) []*opsconfigrpc.TopicItem {
	out := make([]*opsconfigrpc.TopicItem, 0, len(list))
	for _, i := range list {
		out = append(out, &opsconfigrpc.TopicItem{
			ItemType: i.ItemType,
			ItemId:   i.ItemIid,
			Position: i.Position,
			State:    i.State,
		})
	}
	return out
}

// ---------------------------------------------------------------- 推荐位与坑位条目

func opsSlotToAPI(s *opsconfigrpc.RecommendSlot) types.OpsRecommendSlot {
	if s == nil {
		return types.OpsRecommendSlot{}
	}
	return types.OpsRecommendSlot{
		SlotId:     s.GetSlotId(),
		Code:       s.GetCode(),
		Page:       s.GetPage(),
		Title:      s.GetTitle(),
		Platforms:  opsPlatformsFromRPC(s.GetPlatforms()),
		Capacity:   s.GetCapacity(),
		State:      s.GetState(),
		Version:    s.GetVersion(),
		OperatorId: s.GetOperatorId(),
		Remark:     s.GetRemark(),
		Ctime:      s.GetCtime(),
		Mtime:      s.GetMtime(),
	}
}

func opsSlotsToAPI(list []*opsconfigrpc.RecommendSlot) []types.OpsRecommendSlot {
	out := make([]types.OpsRecommendSlot, 0, len(list))
	for _, s := range list {
		out = append(out, opsSlotToAPI(s))
	}
	return out
}

func opsSlotItemToAPI(i *opsconfigrpc.SlotItem) types.OpsSlotItem {
	if i == nil {
		return types.OpsSlotItem{}
	}
	return types.OpsSlotItem{
		Id:         i.GetId(),
		SlotId:     i.GetSlotId(),
		Position:   i.GetPosition(),
		ItemType:   i.GetItemType(),
		ItemIid:    i.GetItemId(),
		Weight:     i.GetWeight(),
		StartAt:    i.GetStartAt(),
		EndAt:      i.GetEndAt(),
		State:      i.GetState(),
		OperatorId: i.GetOperatorId(),
		Ctime:      i.GetCtime(),
		Mtime:      i.GetMtime(),
	}
}

func opsSlotItemsToAPI(list []*opsconfigrpc.SlotItem) []types.OpsSlotItem {
	out := make([]types.OpsSlotItem, 0, len(list))
	for _, i := range list {
		out = append(out, opsSlotItemToAPI(i))
	}
	return out
}

// opsSlotItemsToRPC 同 opsTopicItemsToRPC：不带 id/slot_id，归属由服务端写。
func opsSlotItemsToRPC(list []types.OpsSlotItemSpec) []*opsconfigrpc.SlotItem {
	out := make([]*opsconfigrpc.SlotItem, 0, len(list))
	for _, i := range list {
		out = append(out, &opsconfigrpc.SlotItem{
			Position: i.Position,
			ItemType: i.ItemType,
			ItemId:   i.ItemIid,
			Weight:   i.Weight,
			StartAt:  i.StartAt,
			EndAt:    i.EndAt,
			State:    i.State,
		})
	}
	return out
}

// ---------------------------------------------------------------- 客户端开关

func opsClientSwitchToAPI(s *opsconfigrpc.ClientSwitch) types.OpsClientSwitch {
	if s == nil {
		return types.OpsClientSwitch{}
	}
	return types.OpsClientSwitch{
		SwitchId:   s.GetSwitchId(),
		SwitchKey:  s.GetSwitchKey(),
		Platform:   int32(s.GetPlatform()),
		MinVersion: s.GetMinVersion(),
		MaxVersion: s.GetMaxVersion(),
		Enabled:    s.GetEnabled(),
		ConfigId:   s.GetConfigId(),
		OperatorId: s.GetOperatorId(),
		Remark:     s.GetRemark(),
		Version:    s.GetVersion(),
		Ctime:      s.GetCtime(),
		Mtime:      s.GetMtime(),
	}
}

func opsClientSwitchesToAPI(list []*opsconfigrpc.ClientSwitch) []types.OpsClientSwitch {
	out := make([]types.OpsClientSwitch, 0, len(list))
	for _, s := range list {
		out = append(out, opsClientSwitchToAPI(s))
	}
	return out
}
