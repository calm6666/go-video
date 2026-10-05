// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：notification RPC → 运营后台投影。
//
// 网关在这里只有三件事（AGENTS.md §4/§5/§9）：
//  1. 把共享的 AdminOpContext 收成 notification 契约要求的 operator 字符串
//     （notification 只有 operator 一个审计字段，因此主体 ID 必须先确认存在）；
//     其中 operator_id 以 AdminPermission 中间件解析出的会话身份为准（后台账号 ID 空间），
//     客户端声明值不一致时只留痕，理由见 adminsubject.go 文件头；
//  2. 枚举降维/升维：后台 JSON 只有 int32，proto 枚举是独立 Go 类型，必须显式转换；
//  3. 分页口径与服务端对齐（services/notification/internal/logic.normalizePage）。
//
// 频次限制、免打扰时段计算、供应商调用、模板渲染与变量缺失判定全部留在服务侧；
// 网关不复制任何一条规则，也不解释枚举含义。枚举口径：
//   - 写接口（Upsert/Publish/Render）传未知 channel/language 时由 notification 显式拒绝；
//   - 列表过滤条件里 0（*_UNSPECIFIED）表示「不过滤」，越界值只会筛出空页而不是报错，
//     因此网关原样透传、绝不替后台改写或补默认过滤条件（0 的语义只有服务侧能解释）。
//
// 隐私约束：模板正文与渲染变量可能含用户内容，网关日志只记录 code/channel/language/operator id/error。

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/common/validation"
	"go-video/gateway/admin/internal/types"
	notificationrpc "go-video/services/notification/rpc"
)

const (
	// notificationMaxPageSize 与 notification 服务 normalizePage 的 ps 上限一致，
	// 网关不替后台放大页大小。
	notificationMaxPageSize = 100
	// notificationDefaultPageSize 同服务侧口径：ps 缺省或非法时回落 20。
	notificationDefaultPageSize = 20
)

// normalizeNotificationPage 复用 common/validation.NormalizePage，
// 与 notification 的 normalizePage 同口径（pn<=0→1、ps<=0→20、ps>100→100）。
// 服务侧本身也会归一，这里归一只是让后台拿到确定的一页，而不是让网关放大批量。
func normalizeNotificationPage(pn, ps int32) (int32, int32) {
	page := validation.NormalizePage(int(pn), int(ps), notificationMaxPageSize)
	return int32(page.Page), int32(page.PageSize)
}

// notificationOperator 把请求体的 op 收成下游的 operator 字符串。
//
// notification 的模板/死信表只保存 operator 一列（没有 operator_id），
// 所以主体必须先确认存在：没有后台会话一律拒绝，否则审计会写成无主操作。
// 主体 ID 取自会话（后台账号 ID 空间），客户端声明的 op.operator_id 只在与会话不一致时留痕。
// 有 operator_name 时用它（后台表单显示名，服务侧同样会 TrimSpace），
// 没有则回落成稳定可反查的 "admin:<会话 admin_id>"，不伪造用户名。
//
// 已知缺口：会话身份里只有 admin_id 和角色，没有可信的显示名，所以 operator_name 仍是自报文本；
// 单列表结构下无法同时落稳定主键和显示名，需要 notification 契约补 operator_id 列才能消除。
func notificationOperator(ctx context.Context, route string, op types.AdminOpContext) (string, error) {
	operatorID, err := adminOperatorID(ctx, route, op.OperatorId)
	if err != nil {
		return "", err
	}
	if name := strings.TrimSpace(op.OperatorName); name != "" {
		return name, nil
	}
	return fmt.Sprintf("admin:%d", operatorID), nil
}

// ==================== 投影 ====================

// notifyTemplateToAPI 投影单个模板版本。
// state/version 一律以服务端回显为准：版本号按 (code, channel, language) 递增，
// 同一行只会处于草稿/已发布/已下线之一，网关不推算状态迁移（见 model.PublishDraft）。
// 枚举在这里降回 int32（后台 JSON 没有 proto 枚举类型）。
func notifyTemplateToAPI(t *notificationrpc.TemplateInfo) types.NotifyTemplateInfo {
	return types.NotifyTemplateInfo{
		Id:           t.GetId(),
		TemplateCode: t.GetTemplateCode(),
		Channel:      int32(t.GetChannel()),
		Language:     int32(t.GetLanguage()),
		TitleTpl:     t.GetTitleTpl(),
		BodyTpl:      t.GetBodyTpl(),
		Version:      t.GetVersion(),
		State:        int32(t.GetState()),
		Operator:     t.GetOperator(),
		Ctime:        t.GetCtime(),
		Mtime:        t.GetMtime(),
	}
}

// notifyTemplatesToAPI 投影模板列表；nil 元素（proto 未填）投影成零值条目而不是 panic。
func notifyTemplatesToAPI(list []*notificationrpc.TemplateInfo) []types.NotifyTemplateInfo {
	out := make([]types.NotifyTemplateInfo, 0, len(list))
	for _, t := range list {
		out = append(out, notifyTemplateToAPI(t))
	}
	return out
}

// notifyDeliveryToAPI 投影投递记录（含供应商回执）。
// payload_digest/last_error 已由服务侧脱敏，网关不做二次加工也不补投递结论。
func notifyDeliveryToAPI(d *notificationrpc.DeliveryInfo) types.NotifyDeliveryInfo {
	return types.NotifyDeliveryInfo{
		DeliveryId:    d.GetDeliveryId(),
		BizKey:        d.GetBizKey(),
		Mid:           d.GetMid(),
		Channel:       int32(d.GetChannel()),
		TemplateCode:  d.GetTemplateCode(),
		TemplateVer:   d.GetTemplateVersion(),
		TargetRef:     d.GetTargetRef(),
		PayloadDigest: d.GetPayloadDigest(),
		State:         int32(d.GetState()),
		Provider:      d.GetProvider(),
		ProviderMsgId: d.GetProviderMsgId(),
		RetryCount:    d.GetRetryCount(),
		NextRetryAt:   d.GetNextRetryAt(),
		LastError:     d.GetLastError(),
		SentAt:        d.GetSentAt(),
		ExpireAt:      d.GetExpireAt(),
		Priority:      d.GetPriority(),
		TraceId:       d.GetTraceId(),
		Ctime:         d.GetCtime(),
		Mtime:         d.GetMtime(),
	}
}

func notifyDeliveriesToAPI(list []*notificationrpc.DeliveryInfo) []types.NotifyDeliveryInfo {
	out := make([]types.NotifyDeliveryInfo, 0, len(list))
	for _, d := range list {
		out = append(out, notifyDeliveryToAPI(d))
	}
	return out
}

// notifyDeadLetterToAPI 投影死信。契约缺口：rpc.DeadLetterInfo 没有 source/delivery_id
// （见 services/notification/internal/policy/projection.go 注释），
// 因此后台看不到「这条死信来自投递任务还是事件信封」，只能按 reason/event_id 判断，
// 重投是否可行由 notification 决定并在错误里说明。
func notifyDeadLetterToAPI(d *notificationrpc.DeadLetterInfo) types.NotifyDeadLetterInfo {
	return types.NotifyDeadLetterInfo{
		Id:            d.GetId(),
		EventId:       d.GetEventId(),
		EventType:     d.GetEventType(),
		Topic:         d.GetTopic(),
		PayloadDigest: d.GetPayloadDigest(),
		Reason:        d.GetReason(),
		State:         int32(d.GetState()),
		Operator:      d.GetOperator(),
		Ctime:         d.GetCtime(),
		Mtime:         d.GetMtime(),
	}
}

func notifyDeadLettersToAPI(list []*notificationrpc.DeadLetterInfo) []types.NotifyDeadLetterInfo {
	out := make([]types.NotifyDeadLetterInfo, 0, len(list))
	for _, d := range list {
		out = append(out, notifyDeadLetterToAPI(d))
	}
	return out
}

// notifyMissingVarsToAPI 投影缺失变量清单。
// nil 归一成空切片：后台按「数组」渲染这个字段，拿到 null 会当成接口异常，
// 而「没有缺失变量」和「字段缺失」必须是两回事。
func notifyMissingVarsToAPI(list []string) []string {
	out := make([]string, 0, len(list))
	return append(out, list...)
}
