// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// event-collector RPC ↔ 管理后台投影 + 运营写入口门槛。
//
// 职责边界（AGENTS.md §4/§5/§7），与 conv_cron.go / conv_recommend.go / conv_livemedia.go
// 同一套口径：
//  1. 网关只做四件事：入参形态门槛（主体键非空、幂等键非空、会话身份存在、数值非负、
//     时间窗不倒置）、调用下游、逐字段投影、把「未配置客户端」如实报错。
//     采样比例区间、event_type 白名单、schema_version 上限、时钟漂移与回补窗口、
//     策略生命周期（DRAFT→ACTIVE→ARCHIVED）、盐是否可取、page_size 上限与 cursor 语法
//     全部由 services/event-collector 判定，网关不复算、不把下游错误改写成
//     「查无数据」那样的伪结论；
//  2. **不在此实现任何采集语义**：网关不生成 event_id、不代填 occurred_at、不做采样判定，
//     也不把台账读数二次聚合成「健康/不健康」的结论——GetCollectorHealth 已经给了
//     active_salt_version=0 这种可判定的事实位，网关再造一个布尔只会多一处会失真的解释；
//  3. 隐私口径（AGENTS.md §7）：本域响应只出现 device_hash（加盐摘要）、ip_segment（脱敏段）、
//     salt_version 与 salt_ref（环境变量名），投影里没有承载明文设备号/IP/手机号的字段，
//     日志也只打主键与计数（event_id / batch_id / topic / admin_id）。
//     last_error、reason_detail 在服务侧已先过 redactSensitive 再收敛列宽，网关原样投影、
//     不再拼接错误原文，避免把下游未脱敏的异常文本带到后台响应里；
//  4. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// collectorOperatorPrefix 与 cronOperatorPrefix / recommendOperatorPrefix 同口径：
// operator 字段的前缀说明「哪个入口提交的」（服务名，不是实例地址），后面接会话 admin_id，
// 让策略版本行与死信处置行能回溯到人。
const collectorOperatorPrefix = "gateway/admin:"

// errCollectorServiceNotConfigured：未配置 EventCollectorRPC 时本域 13 条路由一律返回它。
// 不退化成伪造空台账——那会让后台把「下游没接」读成「这段时间没有事件」，
// 而「没有事件」在采集域是最容易被误读成「埋点正常但用户没来」的假线索。
var errCollectorServiceNotConfigured = errors.New("event-collector service not configured")

// errCollectorRequestMissing：请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errCollectorRequestMissing = errors.New("gateway/admin: request body required")

// errCollectorSessionRequired：受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed。
var errCollectorSessionRequired = errors.New("gateway/admin: admin session identity required")

// errCollectorSubjectRequired 精读接口的主体键（batch_id / event_id）为空：
// 下游会按空主键查一条不存在的行，回给后台的是「未命中」而不是「你少传了参数」。
var errCollectorSubjectRequired = errors.New("gateway/admin: subject key required")

// errCollectorDeadLetterIDsRequired 重放死信至少要给一个 ID：空数组在下游只会变成
// 「处置了 0 条」的成功结论，白占一个幂等键还看不出错。
var errCollectorDeadLetterIDsRequired = errors.New("gateway/admin: dead_letter_ids required")

// collectorOperator 从会话渲染 operator（proto 里有 operator 位的四条写路由都用它）。
// 表单**不能**声明操作者：那等于让请求体自己说「我是某个后台账号」。
// 日志只打路由与 admin_id：reason 正文与幂等键都不进日志（前者是人读文案、后者可被重放）。
func collectorOperator(ctx context.Context, route string) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errCollectorSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d", route, id.AdminID)
	return fmt.Sprintf("%s%d", collectorOperatorPrefix, id.AdminID), nil
}

// collectorNonNeg 传输层门槛：0 在本域普遍是「用服务默认值」的合法哨兵
// （now=0 用服务端当前时间、limit=0 用 Dispatch.BatchSize、page_size=0 用默认页大小、
// ctime_from/ctime_to=0 表示该侧不设界、mid=0 游客），负数没有任何对应语义，
// 透传只会换来一次无意义的往返。上限（MaxDispatchLimit、页大小上限）一律由服务夹取或拒绝。
func collectorNonNeg(field string, v int64) error {
	if v < 0 {
		return errors.New("gateway/admin: " + field + " must be >= 0")
	}
	return nil
}

// collectorNonNeg32 是 collectorNonNeg 的 int32 位（limit / page_size / 各策略数值位）。
func collectorNonNeg32(field string, v int32) error {
	return collectorNonNeg(field, int64(v))
}

// collectorTimeWindow 只挡住「倒着给」与「负数」的时间窗：from>to 在服务侧查不到任何行，
// 却会先做一次带条件的扫描。窗口跨度上限（maxListWindowSeconds）由服务判定，网关不复算。
func collectorTimeWindow(from, to int64) error {
	if err := collectorNonNeg("ctime_from", from); err != nil {
		return err
	}
	if err := collectorNonNeg("ctime_to", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: ctime_from must be <= ctime_to")
	}
	return nil
}

// collectorTrim 只在判空意义上 Trim，返回原值：
// batch_id/event_id/topic/cursor 与 idempotency_key 的任何改写都会让它失去语义
// （幂等键改一个字符就等于换了一次执行权）。
func collectorTrim(v string) (string, bool) {
	t := strings.TrimSpace(v)
	return v, t != ""
}

// --- rpc → 后台 types 投影 ---

// collectorStrings 复制字符串切片并保证非 nil（缺省语义由调用方的空数组表达）。
func collectorStrings(list []string) []string {
	out := make([]string, 0, len(list))
	out = append(out, list...)
	return out
}

// collectorInt64s 同上，用于 failed_ids。
func collectorInt64s(list []int64) []int64 {
	out := make([]int64, 0, len(list))
	out = append(out, list...)
	return out
}

// collectorSampleRulesToAPI 采样规则投影：event_type="*" 的兜底规则与只作用于质量类事件的
// 规则都要能看见，否则「为什么这个 topic 一条都没投给 spm」在后台就只剩猜。
func collectorSampleRulesToAPI(list []*collectorrpc.SampleRule) []types.CollectorSampleRule {
	out := make([]types.CollectorSampleRule, 0, len(list))
	for _, r := range list {
		out = append(out, types.CollectorSampleRule{
			EventType:     r.GetEventType(),
			SampleBps:     r.GetSampleBps(),
			QualityEvents: r.GetQualityEvents(),
		})
	}
	return out
}

// collectorPolicyToAPI 策略版本投影。
// 只回 salt_ref（环境变量名）与 salt_version，盐值本身既不在 RPC 响应里也不在后台响应里；
// 各数值位回的是**服务落库后回读的真值**（inherit32/inherit64 继承过默认的那些），
// 不是入参回显——否则调用方会把自己没填的 0 当成「我配了 0」。
func collectorPolicyToAPI(p *collectorrpc.DispatchPolicy) types.CollectorDispatchPolicy {
	return types.CollectorDispatchPolicy{
		Version:              p.GetVersion(),
		State:                int32(p.GetState()),
		SampleRules:          collectorSampleRulesToAPI(p.GetSampleRules()),
		SaltVersion:          p.GetSaltVersion(),
		SaltRef:              p.GetSaltRef(),
		FieldWhitelist:       collectorStrings(p.GetFieldWhitelist()),
		DropFields:           collectorStrings(p.GetDropFields()),
		MaxEventsPerBatch:    p.GetMaxEventsPerBatch(),
		MaxRequestBytes:      p.GetMaxRequestBytes(),
		MaxEventPayloadBytes: p.GetMaxEventPayloadBytes(),
		MaxClockSkewSeconds:  p.GetMaxClockSkewSeconds(),
		MaxBackfillSeconds:   p.GetMaxBackfillSeconds(),
		KeywordMaxRunes:      p.GetKeywordMaxRunes(),
		RetentionDays:        p.GetRetentionDays(),
		DeliverMaxAttempts:   p.GetDeliverMaxAttempts(),
		RetryBaseSeconds:     p.GetRetryBaseSeconds(),
		RetryMaxSeconds:      p.GetRetryMaxSeconds(),
		Note:                 p.GetNote(),
		Operator:             p.GetOperator(),
		Ctime:                p.GetCtime(),
		Mtime:                p.GetMtime(),
	}
}

// collectorSampleRulesForRPC 把后台表单的采样规则交给服务：原样传递，不做区间夹取、
// 不去重、不补 "*" 兜底规则（这些都在 policy.go 的校验里）。
func collectorSampleRulesForRPC(list []types.CollectorSampleRule) []*collectorrpc.SampleRule {
	out := make([]*collectorrpc.SampleRule, 0, len(list))
	for _, r := range list {
		out = append(out, &collectorrpc.SampleRule{
			EventType:     r.EventType,
			SampleBps:     r.SampleBps,
			QualityEvents: r.QualityEvents,
		})
	}
	return out
}

// collectorPolicyForRPC 组装 UpsertDispatchPolicyReq.policy。
// **不设置 State**：Upsert 只能碰 DRAFT，网关不把表单意图 transcribe 成状态声明，
// 生效只能走 /policy/activate（独立权限点 collector:policy/enable）。
// 也不设置 Operator/Ctime/Mtime——前者由会话渲染后放在 req.operator 顶层，后两者是库的事实列。
func collectorPolicyForRPC(p *types.ParamCollectorPolicyUpsert) *collectorrpc.DispatchPolicy {
	return &collectorrpc.DispatchPolicy{
		Version:              p.Version,
		SampleRules:          collectorSampleRulesForRPC(p.SampleRules),
		SaltVersion:          p.SaltVersion,
		SaltRef:              p.SaltRef,
		FieldWhitelist:       p.FieldWhitelist,
		DropFields:           p.DropFields,
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
	}
}

// collectorBatchToAPI 批次台账投影：没有任何事件明文，只有计数与脱敏后的主体标识。
func collectorBatchToAPI(b *collectorrpc.IngestBatch) types.CollectorIngestBatch {
	return types.CollectorIngestBatch{
		Id:            b.GetId(),
		BatchId:       b.GetBatchId(),
		Source:        int32(b.GetSource()),
		CallerService: b.GetCallerService(),
		Platform:      int32(b.GetPlatform()),
		AppId:         b.GetAppId(),
		AppVersion:    b.GetAppVersion(),
		SdkVersion:    b.GetSdkVersion(),
		Mid:           b.GetMid(),
		DeviceHash:    b.GetDeviceHash(),
		IpSegment:     b.GetIpSegment(),
		SaltVersion:   b.GetSaltVersion(),
		PolicyVersion: b.GetPolicyVersion(),
		Total:         b.GetTotal(),
		Accepted:      b.GetAccepted(),
		Duplicated:    b.GetDuplicated(),
		Rejected:      b.GetRejected(),
		SampledOut:    b.GetSampledOut(),
		Dispatched:    b.GetDispatched(),
		Dead:          b.GetDead(),
		RequestBytes:  b.GetRequestBytes(),
		State:         int32(b.GetState()),
		TopReason:     int32(b.GetTopReason()),
		LastError:     b.GetLastError(),
		TraceId:       b.GetTraceId(),
		ReceivedAt:    b.GetReceivedAt(),
		FinishedAt:    b.GetFinishedAt(),
		Ctime:         b.GetCtime(),
		Mtime:         b.GetMtime(),
	}
}

func collectorBatchListToAPI(list []*collectorrpc.IngestBatch) []types.CollectorIngestBatch {
	out := make([]types.CollectorIngestBatch, 0, len(list))
	for _, b := range list {
		out = append(out, collectorBatchToAPI(b))
	}
	return out
}

// collectorRecordToAPI 事件台账投影。payload 只有摘要与字节数（原文永不入库，AGENTS.md §7）；
// delivery_* 在单条读接口上已被服务用 Outbox 真值覆盖，这里不再做任何补偿推断。
func collectorRecordToAPI(r *collectorrpc.EventRecord) types.CollectorEventRecord {
	return types.CollectorEventRecord{
		Id:               r.GetId(),
		EventId:          r.GetEventId(),
		BatchId:          r.GetBatchId(),
		EventType:        r.GetEventType(),
		Category:         int32(r.GetCategory()),
		SchemaVersion:    r.GetSchemaVersion(),
		OccurredAt:       r.GetOccurredAt(),
		ReceivedAt:       r.GetReceivedAt(),
		ClockSkewSeconds: r.GetClockSkewSeconds(),
		Decision:         int32(r.GetDecision()),
		Reason:           int32(r.GetReason()),
		ReasonDetail:     r.GetReasonDetail(),
		DeliveryState:    int32(r.GetDeliveryState()),
		Topic:            r.GetTopic(),
		EnvelopeEventId:  r.GetEnvelopeEventId(),
		DeliveryAttempts: r.GetDeliveryAttempts(),
		NextRetryAt:      r.GetNextRetryAt(),
		LastError:        r.GetLastError(),
		Mid:              r.GetMid(),
		DeviceHash:       r.GetDeviceHash(),
		IpSegment:        r.GetIpSegment(),
		SaltVersion:      r.GetSaltVersion(),
		ContentType:      r.GetContentType(),
		ContentId:        r.GetContentId(),
		Vid:              r.GetVid(),
		TargetMid:        r.GetTargetMid(),
		PayloadDigest:    r.GetPayloadDigest(),
		PayloadBytes:     r.GetPayloadBytes(),
		SanitizeVersion:  r.GetSanitizeVersion(),
		PolicyVersion:    r.GetPolicyVersion(),
		TraceId:          r.GetTraceId(),
		Ctime:            r.GetCtime(),
		Mtime:            r.GetMtime(),
	}
}

func collectorRecordListToAPI(list []*collectorrpc.EventRecord) []types.CollectorEventRecord {
	out := make([]types.CollectorEventRecord, 0, len(list))
	for _, r := range list {
		out = append(out, collectorRecordToAPI(r))
	}
	return out
}

// collectorDeadLetterListToAPI 死信投影：reason 是服务侧归类出的稳定枚举，
// 网关不把它翻译成人读文案（翻译过一次，筛选条件就再也对不上第二个人）。
func collectorDeadLetterListToAPI(list []*collectorrpc.DeadLetter) []types.CollectorDeadLetterInfo {
	out := make([]types.CollectorDeadLetterInfo, 0, len(list))
	for _, d := range list {
		out = append(out, types.CollectorDeadLetterInfo{
			Id:            d.GetId(),
			EventId:       d.GetEventId(),
			BatchId:       d.GetBatchId(),
			EventType:     d.GetEventType(),
			Topic:         d.GetTopic(),
			PayloadDigest: d.GetPayloadDigest(),
			Reason:        d.GetReason(),
			Attempts:      d.GetAttempts(),
			State:         d.GetState(),
			CreatedAt:     d.GetCreatedAt(),
			HandledAt:     d.GetHandledAt(),
			Operator:      d.GetOperator(),
		})
	}
	return out
}

// collectorPolicyListToAPI 策略版本台账投影（含 ARCHIVED：历史批次的归因依据）。
func collectorPolicyListToAPI(list []*collectorrpc.DispatchPolicy) []types.CollectorDispatchPolicy {
	out := make([]types.CollectorDispatchPolicy, 0, len(list))
	for _, p := range list {
		out = append(out, collectorPolicyToAPI(p))
	}
	return out
}

// collectorHealthToAPI 健康度投影：只搬「计数 + 布尔 + 变量名」，不合成结论。
// active_salt_version=0 与 salt_available=false 是服务给出的事实位，
// 后台据此自己判断是否告警；网关不缓存本结果（ttl 恒为 0）。
func collectorHealthToAPI(h *collectorrpc.GetCollectorHealthReply) types.CollectorHealthData {
	topics := make([]types.CollectorTopicHealth, 0, len(h.GetTopics()))
	for _, t := range h.GetTopics() {
		topics = append(topics, types.CollectorTopicHealth{
			Topic:              t.GetTopic(),
			Pending:            t.GetPending(),
			Retrying:           t.GetRetrying(),
			DeadOpen:           t.GetDeadOpen(),
			SentLastHour:       t.GetSentLastHour(),
			OldestPendingCtime: t.GetOldestPendingCtime(),
		})
	}
	return types.CollectorHealthData{
		ServerTime:              h.GetServerTime(),
		Topics:                  topics,
		BatchesRejectedLastHour: h.GetBatchesRejectedLastHour(),
		RateLimitedLastHour:     h.GetRateLimitedLastHour(),
		ActiveSaltVersion:       h.GetActiveSaltVersion(),
		PolicyVersion:           h.GetPolicyVersion(),
		SaltRef:                 h.GetSaltRef(),
		SaltAvailable:           h.GetSaltAvailable(),
		Version:                 h.GetVersion(),
	}
}

// collectorValidateEventForRPC 把干跑入参组装成 BehaviorEvent（字段一一对应，不改写、不补齐）。
// 网关不生成 event_id、不填 occurred_at：那会让「客户端漏字段」这条判定被测不出来。
func collectorValidateEventForRPC(req *types.ParamCollectorSchemaValidate) *collectorrpc.BehaviorEvent {
	return &collectorrpc.BehaviorEvent{
		EventId:       req.EventId,
		EventType:     req.EventType,
		SchemaVersion: req.SchemaVersion,
		OccurredAt:    req.OccurredAt,
		ReportedAt:    req.ReportedAt,
		Category:      collectorrpc.BehaviorCategory(req.Category),
		TraceId:       req.TraceId,
		ContentType:   req.ContentType,
		ContentId:     req.ContentId,
		Aid:           req.Aid,
		Vid:           req.Vid,
		TargetMid:     req.TargetMid,
		SessionId:     req.SessionId,
		PositionMs:    req.PositionMs,
		DurationMs:    req.DurationMs,
		BufferCount:   req.BufferCount,
		FirstFrameMs:  req.FirstFrameMs,
		AvgBitrate:    req.AvgBitrate,
		ErrorCode:     req.ErrorCode,
		Keyword:       req.Keyword,
		ResultIndex:   req.ResultIndex,
		TargetUrl:     req.TargetUrl,
		Payload:       req.Payload,
	}
}

// collectorValidateContextForRPC 组装 EventContext 的**入参位**。
// DeviceId/Ip 是明文且只在请求生命周期内使用（干跑不落库、不投递），
// 响应投影里没有能承载它们的字段；服务回填的 DeviceHash/IpSegment 在此一律不设置。
func collectorValidateContextForRPC(req *types.ParamCollectorSchemaValidate) *collectorrpc.EventContext {
	return &collectorrpc.EventContext{
		Mid:         req.CtxMid,
		DeviceId:    req.CtxDeviceId,
		DeviceType:  req.CtxDeviceType,
		Ip:          req.CtxIp,
		Platform:    collectorrpc.Platform(req.CtxPlatform),
		AppId:       req.CtxAppId,
		AppVersion:  req.CtxAppVersion,
		SdkVersion:  req.CtxSdkVersion,
		OsVersion:   req.CtxOsVersion,
		NetworkType: req.CtxNetworkType,
		Model:       req.CtxModel,
		Region:      req.CtxRegion,
		SessionId:   req.CtxSessionId,
		Page:        req.CtxPage,
		Spm:         req.CtxSpm,
	}
}
