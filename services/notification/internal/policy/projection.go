package policy

import (
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// model 行 -> rpc 投影。
//
// 放在 policy 而不是 logic，是因为投递用例（internal/send）与 gRPC 用例
// （internal/logic）都要回传同样的投影，两处各写一份必然漂移。
// 这里只做字段搬运与枚举转换，不含任何业务判定。

// PriorityCodeOf 把 rpc 优先级枚举转成落库数值；未指定时回落普通。
func PriorityCodeOf(p rpc.Priority) int32 {
	if p == rpc.Priority_PRIORITY_UNSPECIFIED {
		return PriorityNormal
	}
	return int32(p)
}

// PriorityEnumOf 把落库数值转回 rpc 枚举；未知值回落普通，避免把脏数据当高优先。
func PriorityEnumOf(code int32) rpc.Priority {
	switch code {
	case PriorityLow:
		return rpc.Priority_PRIORITY_LOW
	case PriorityHigh:
		return rpc.Priority_PRIORITY_HIGH
	default:
		return rpc.Priority_PRIORITY_NORMAL
	}
}

// DeliveryStateEnumOf 把落库状态转成 rpc 枚举。
func DeliveryStateEnumOf(state int32) rpc.DeliveryState {
	return rpc.DeliveryState(state)
}

// DeliveryStateCodeOf 把 rpc 枚举转成落库状态。
func DeliveryStateCodeOf(s rpc.DeliveryState) int32 {
	return int32(s)
}

// ToDeliveryInfo 投影一条投递任务。
func ToDeliveryInfo(d *model.NotificationDelivery) *rpc.DeliveryInfo {
	if d == nil {
		return nil
	}
	return &rpc.DeliveryInfo{
		DeliveryId:      d.DeliveryId,
		BizKey:          d.BizKey,
		Mid:             d.Mid,
		Channel:         rpc.Channel(d.Channel),
		TemplateCode:    d.TemplateCode,
		TemplateVersion: d.TemplateVersion,
		TargetRef:       d.TargetRef,
		PayloadDigest:   d.PayloadDigest,
		State:           DeliveryStateEnumOf(d.State),
		Provider:        d.Provider,
		ProviderMsgId:   d.ProviderMsgId,
		RetryCount:      d.RetryCount,
		NextRetryAt:     d.NextRetryAt,
		LastError:       d.LastError,
		SentAt:          d.SentAt,
		ExpireAt:        d.ExpireAt,
		Priority:        d.Priority,
		TraceId:         d.TraceId,
		Ctime:           d.Ctime,
		Mtime:           d.Mtime,
	}
}

// ToDeliveryInfos 批量投影，nil 元素直接跳过。
func ToDeliveryInfos(rows []*model.NotificationDelivery) []*rpc.DeliveryInfo {
	out := make([]*rpc.DeliveryInfo, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, ToDeliveryInfo(r))
	}
	return out
}

// ToTemplateInfo 投影一个模板版本。
func ToTemplateInfo(t *model.NotificationTemplate) *rpc.TemplateInfo {
	if t == nil {
		return nil
	}
	return &rpc.TemplateInfo{
		Id:           t.Id,
		TemplateCode: t.TemplateCode,
		Channel:      rpc.Channel(t.Channel),
		Language:     langEnumOrUnspecified(t.Lang),
		TitleTpl:     t.TitleTpl,
		BodyTpl:      t.BodyTpl,
		Version:      t.Version,
		State:        rpc.TemplateState(t.State),
		Operator:     t.Operator,
		Ctime:        t.Ctime,
		Mtime:        t.Mtime,
	}
}

// ToTemplateInfos 批量投影模板。
func ToTemplateInfos(rows []*model.NotificationTemplate) []*rpc.TemplateInfo {
	out := make([]*rpc.TemplateInfo, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, ToTemplateInfo(r))
	}
	return out
}

// ToDeadLetterInfo 投影一条死信。
// 注意：rpc.DeadLetterInfo 未定义 source/delivery_id 字段（契约缺口，见服务 README），
// 因此这里只回传契约已有的字段，来源判定仍由服务端读 model 完成。
func ToDeadLetterInfo(d *model.NotificationDeadLetter) *rpc.DeadLetterInfo {
	if d == nil {
		return nil
	}
	return &rpc.DeadLetterInfo{
		Id:            d.Id,
		EventId:       d.EventId,
		EventType:     d.EventType,
		Topic:         d.Topic,
		PayloadDigest: d.PayloadDigest,
		Reason:        d.Reason,
		State:         rpc.DeadLetterState(d.State),
		Operator:      d.Operator,
		Ctime:         d.Ctime,
		Mtime:         d.Mtime,
	}
}

// ToDeadLetterInfos 批量投影死信。
func ToDeadLetterInfos(rows []*model.NotificationDeadLetter) []*rpc.DeadLetterInfo {
	out := make([]*rpc.DeadLetterInfo, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, ToDeadLetterInfo(r))
	}
	return out
}

// ToDndPreference 投影用户偏好；nil 表示用户从未设置过，返回全默认值而不是 nil，
// 让调用方可以无条件读取。enabled 落库为 state，muted_channels 展开为位掩码对应的通道列表。
func ToDndPreference(p *model.NotificationDndPref) *rpc.DndPreference {
	if p == nil {
		return &rpc.DndPreference{}
	}
	channels := make([]rpc.Channel, 0, 3)
	for _, c := range model.ChannelsFromMask(p.MutedChannels) {
		channels = append(channels, rpc.Channel(c))
	}
	return &rpc.DndPreference{
		Mid:           p.Mid,
		MutedChannels: channels,
		QuietStart:    p.QuietStart,
		QuietEnd:      p.QuietEnd,
		Timezone:      p.Timezone,
		Enabled:       p.State == model.DndStateOn,
		Ctime:         p.Ctime,
		Mtime:         p.Mtime,
	}
}

// langEnumOrUnspecified 把语言编码转成枚举，未知值回落到 UNSPECIFIED（不伪造语言）。
func langEnumOrUnspecified(code string) rpc.Language {
	if l, ok := LangCodeToEnum(code); ok {
		return l
	}
	return rpc.Language_LANGUAGE_UNSPECIFIED
}
