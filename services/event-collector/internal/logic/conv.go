// 本文件是 logic 包的手写投影层（model 行 → rpc 响应），不是 goctl 生成产物。
//
// 铁律（AGENTS.md §7）：投影只输出脱敏列。device_hash / ip_segment / keyword_digest /
// payload_digest 都是不可逆摘要或脱敏段；本文件不接收、也永不输出明文设备号、完整 IP、
// 手机号、token 或事件正文。新增字段时必须先确认它是「可安全外发」的口径。

package logic

import (
	"fmt"

	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"
)

// batchToRPC 输出批次台账投影。计数列是 ec_event_record 的可重算投影，滞后属预期。
func batchToRPC(b *model.IngestBatch) *rpc.IngestBatch {
	if b == nil {
		return nil
	}
	return &rpc.IngestBatch{
		Id:            b.ID,
		BatchId:       b.BatchID,
		Source:        rpc.Source(b.Source),
		CallerService: b.CallerService,
		Platform:      rpc.Platform(b.Platform),
		AppId:         b.AppID,
		AppVersion:    b.AppVersion,
		SdkVersion:    b.SdkVersion,
		Mid:           b.Mid,
		DeviceHash:    b.DeviceHash,
		IpSegment:     b.IPSegment,
		SaltVersion:   b.SaltVersion,
		PolicyVersion: b.PolicyVersion,
		Total:         b.Total,
		Accepted:      b.Accepted,
		Duplicated:    b.Duplicated,
		Rejected:      b.Rejected,
		SampledOut:    b.SampledOut,
		Dispatched:    b.Dispatched,
		Dead:          b.Dead,
		RequestBytes:  b.RequestBytes,
		State:         rpc.BatchState(b.State),
		TopReason:     rpc.RejectReason(b.TopReason),
		LastError:     b.LastError,
		TraceId:       b.TraceID,
		ReceivedAt:    b.ReceivedAt,
		FinishedAt:    b.FinishedAt,
		Ctime:         b.Ctime,
		Mtime:         b.Mtime,
	}
}

// recordToRPC 输出事件台账投影：正文只有 payload_digest + payload_bytes，绝无原文。
func recordToRPC(r *model.EventRecord) *rpc.EventRecord {
	if r == nil {
		return nil
	}
	return &rpc.EventRecord{
		Id:               r.ID,
		EventId:          r.EventID,
		BatchId:          r.BatchID,
		EventType:        r.EventType,
		Category:         rpc.BehaviorCategory(r.Category),
		SchemaVersion:    r.SchemaVersion,
		OccurredAt:       r.OccurredAt,
		ReceivedAt:       r.ReceivedAt,
		ClockSkewSeconds: r.ClockSkewSeconds,
		Decision:         rpc.EventDecision(r.Decision),
		Reason:           rpc.RejectReason(r.Reason),
		ReasonDetail:     r.ReasonDetail,
		DeliveryState:    rpc.DeliveryState(r.DeliveryState),
		Topic:            r.Topic,
		EnvelopeEventId:  r.EnvelopeEventID,
		DeliveryAttempts: r.DeliveryAttempts,
		NextRetryAt:      r.NextRetryAt,
		LastError:        r.LastError,
		Mid:              r.Mid,
		DeviceHash:       r.DeviceHash,
		IpSegment:        r.IPSegment,
		SaltVersion:      r.SaltVersion,
		ContentType:      r.ContentType,
		ContentId:        r.ContentID,
		Vid:              r.Vid,
		TargetMid:        r.TargetMid,
		PayloadDigest:    r.PayloadDigest,
		PayloadBytes:     r.PayloadBytes,
		SanitizeVersion:  r.SanitizeVersion,
		PolicyVersion:    r.PolicyVersion,
		TraceId:          r.TraceID,
		Ctime:            r.Ctime,
		Mtime:            r.Mtime,
	}
}

func recordToRPCList(rows []*model.EventRecord) []*rpc.EventRecord {
	out := make([]*rpc.EventRecord, 0, len(rows))
	for _, r := range rows {
		if v := recordToRPC(r); v != nil {
			out = append(out, v)
		}
	}
	return out
}

func batchToRPCList(rows []*model.IngestBatch) []*rpc.IngestBatch {
	out := make([]*rpc.IngestBatch, 0, len(rows))
	for _, b := range rows {
		if v := batchToRPC(b); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// deadLetterToRPC 输出死信摘要投影。死信表本身不存正文，这里也不补任何正文线索。
func deadLetterToRPC(d *model.DeadLetter) *rpc.DeadLetter {
	if d == nil {
		return nil
	}
	return &rpc.DeadLetter{
		Id:            d.ID,
		EventId:       d.EventID,
		BatchId:       d.BatchID,
		EventType:     d.EventType,
		Topic:         d.Topic,
		PayloadDigest: d.PayloadDigest,
		Reason:        d.Reason,
		Attempts:      d.Attempts,
		State:         d.State,
		CreatedAt:     d.CreatedAt,
		HandledAt:     d.HandledAt,
		Operator:      d.Operator,
	}
}

func deadLetterToRPCList(rows []*model.DeadLetter) []*rpc.DeadLetter {
	out := make([]*rpc.DeadLetter, 0, len(rows))
	for _, d := range rows {
		if v := deadLetterToRPC(d); v != nil {
			out = append(out, v)
		}
	}
	return out
}

// policyToRPC 把策略行的 JSON 文本列解码成结构化字段。
//
// 解码失败必须报错而不是返回「空规则」：空 sample_rules 在调用方看来等于「不采样」，
// 空 drop_fields 等于「什么都不禁止」，静默降级会把隐私底线抹掉（AGENTS.md §9）。
func policyToRPC(p *model.DispatchPolicy) (*rpc.DispatchPolicy, error) {
	if p == nil {
		return nil, model.ErrPolicyNotFound
	}
	rules, err := model.DecodeSampleRules(p.SampleRulesJSON)
	if err != nil {
		return nil, fmt.Errorf("event-collector: policy %s sample_rules 解析失败: %w", p.Version, err)
	}
	whitelist, err := model.DecodeStringList(p.FieldWhitelistJSON)
	if err != nil {
		return nil, fmt.Errorf("event-collector: policy %s field_whitelist 解析失败: %w", p.Version, err)
	}
	dropFields, err := model.DecodeStringList(p.DropFieldsJSON)
	if err != nil {
		return nil, fmt.Errorf("event-collector: policy %s drop_fields 解析失败: %w", p.Version, err)
	}
	out := &rpc.DispatchPolicy{
		Version:              p.Version,
		State:                rpc.PolicyState(p.State),
		SaltVersion:          p.SaltVersion,
		SaltRef:              p.SaltRef,
		FieldWhitelist:       whitelist,
		DropFields:           dropFields,
		MaxEventsPerBatch:    p.MaxEventsPerBatch,
		MaxRequestBytes:      p.MaxRequestBytes,
		MaxEventPayloadBytes: p.MaxEventPayloadBytes,
		MaxClockSkewSeconds:  p.MaxClockSkewSeconds,
		MaxBackfillSeconds:   p.MaxBackfillSeconds,
		KeywordMaxRunes:      p.KeywordMaxRunes,
		RetentionDays:        p.RetentionDays,
		DeliverMaxAttempts:   p.DeliverMaxAttempts,
		RetryBaseSeconds:     p.RetryBaseSeconds,
		RetryMaxSeconds:      p.RetryMaxSeconds,
		Note:                 p.Note,
		Operator:             p.Operator,
		Ctime:                p.Ctime,
		Mtime:                p.Mtime,
	}
	for _, r := range rules {
		out.SampleRules = append(out.SampleRules, &rpc.SampleRule{
			EventType:     r.EventType,
			SampleBps:     r.SampleBps,
			QualityEvents: r.QualityEvents,
		})
	}
	return out, nil
}
