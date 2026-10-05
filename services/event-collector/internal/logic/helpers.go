// 本文件是 logic 包的手写公共件（事件校验、脱敏接线、限流、策略解析、幂等回放），
// 不是 goctl 生成产物，goctl 重新生成 internal/logic 不会覆盖它。
//
// 铁律（AGENTS.md §7）：明文 device_id / 出口 IP 只在参数里活一瞬间。本文件对外返回的
// 一切结构体、字符串与日志字段里只允许出现加盐哈希（h1:<hex>）与脱敏网段（/24），
// 绝不出现明文；盐取不到时一律返回 model.ErrSaltMissing，不退化成无盐哈希。

package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go-video/common/eventenvelope"
	"go-video/services/event-collector/internal/config"
	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 列宽常量与 deploy/migrations/event-collector 的 DDL 一一对应。
//
// 为什么 logic 侧也要卡长度：写台账用 INSERT IGNORE，超长值会被 MySQL 静默丢掉，
// 表现为「实际新增行数 < 期望行数」，logic 只能当成并发冲突回滚，整批反复失败。
// 所以入库前必须自己收敛到列宽内（fitColumn），让「丢行」只可能是真的重复键。
const (
	maxBatchIDBytes       = 64  // ec_ingest_batch.batch_id
	maxEventIDBytes       = 64  // ec_event_record.event_id
	maxIdemKeyBytes       = 128 // ec_ingest_batch.idempotency_key
	maxOperatorBytes      = 64  // ec_dispatch_policy.operator
	maxReasonBytes        = 512 // reason_detail / last_error
	maxTraceIDBytes       = 64  // trace_id
	maxRequestIDBytes     = 64  // ec_ingest_batch.request_id
	maxCallerSrvBytes     = 64  // caller_service
	maxAppIDBytes         = 64  // app_id
	maxTagBytes           = 32  // app_version / sdk_version / sanitize_version
	maxEventTypeBytes     = 64  // event_type
	maxTopicBytes         = 128 // topic
	maxContentTypeBytes   = 32  // content_type
	maxVidBytes           = 64  // vid
	maxSessionIDBytes     = 64  // session_id
	maxPolicyVerBytes     = 64  // policy_version
	maxLeaseOwnerBytes    = 64  // ec_pending_delivery.lease_owner
	maxDigestBytes        = 80  // payload_digest / keyword_digest / device_hash
	maxIPSegmentBytes     = 43  // ip_segment
	maxReasonClassBytes   = 64  // ec_dead_letter.reason
	maxReplayReasonBytes  = 512 // ec_dead_letter.replay_reason
	maxNoteBytes          = 512 // ec_dispatch_policy.note
	maxVersionTagBytes    = 64  // rpc 里的 version 入参
	maxSaltRefBytes       = 128 // salt_ref
	maxDropFieldItemBytes = 128 // drop_fields / field_whitelist 单项
)

// listChunkSize 与 model.maxStringIDList 同源：批量回查/批量写入的单次上限。
const listChunkSize = 500

// Redis 只当加速器，真值恒在 MySQL（svc.ServiceContext.Cache 注释同义）。
const (
	activePolicyCacheKey  = "govideo:ec:policy:active"
	opDedupKeyPrefix      = "govideo:ec:op:"
	rateLimitKeyPrefix    = "govideo:ec:rl:"
	activePolicyTTLSecs   = 30  // ACTIVE 策略短 TTL：切换即失效，TTL 只兜住漏失效
	opDedupTTLSecs        = 600 // 运营/兜底动作的轮次结论回放窗口
	rateLimitWindowSecs   = 1   // 固定窗口 1 秒，与 *Qps 配置同单位
	opInflightMarker      = "__inflight__"
	policyMissingSentinel = ""
)

// 需要轮次幂等的 RPC 名（写进 Redis key，避免同一个 key 被两个方法复用）。
const (
	rpcRetryPendingDelivery = "RetryPendingDelivery"
)

// 对外提示文案：只允许出现字段名与口径，绝不允许出现事件正文或明文标识。
const (
	msgBatchReplayed   = "batch_id 已处理过：以下结论为首次接收时落库的回放，未重复落库、未重复投递"
	msgDuplicated      = "event_id 已存在于接收台账（uniq_event_id 去重），本条不重复落库、不重复投递"
	msgDeferredSize    = "批次条数或字节超过 ACTIVE 策略上限，整批未接收；请按 retry_after_ms 退避后重发同一 batch_id"
	msgDeferredLimit   = "触发采集容量限流，整批未接收；请按 retry_after_ms 退避后重发同一 batch_id"
	msgStoreFailed     = "接收库写入失败，整批未落库；请退避后重发同一 batch_id（batch_id 幂等，重试不会产生重复数据）"
	msgSampledOut      = "命中采样规则，本条主动丢弃（不落投递队列）"
	msgInRequestDup    = "同一批次内 event_id 重复：只处理首次出现，本条按重复计数"
	msgOwnRowLost      = "批次台账存在但本条没有落库痕迹，按内部错误处理（不伪造已接收）"
	msgNoDispatcher    = "dispatcher 未启用，事件留在 Outbox 由 RetryPendingDelivery 推进"
	msgPolicyNoActive  = "无 ACTIVE 采样策略，本批按 config 保守默认（全量）接收"
	msgPolicyHintStale = "客户端声明的 policy_version 与服务端 ACTIVE 不一致，按 ACTIVE 策略裁决"
)

// categorySuffix 把行为类别映射为 event_type 后缀，同时充当 event_type 白名单。
//
// proto 里 REJECT_UNSUPPORTED_EVENT_TYPE 的语义是「不在白名单」，而本服务没有独立的
// schema 注册中心（README「疑点」）：类别枚举就是白名单真值。新增类别必须同时补
// 枚举、后缀与 topic，否则事件会被静默拒收。
var categorySuffix = map[rpc.BehaviorCategory]string{
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_PLAY:     "play",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_CLICK:    "click",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_SEARCH:   "search",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_SKIP:     "skip",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_LIKE:     "like",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_FAVORITE: "favorite",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_FOLLOW:   "follow",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_SHARE:    "share",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_QUALITY:  "quality",
	rpc.BehaviorCategory_BEHAVIOR_CATEGORY_EXPOSURE: "exposure",
}

const eventTypePrefix = "behavior."

// eventSuffixOf 返回类别对应的 event_type 后缀。
func eventSuffixOf(cat rpc.BehaviorCategory) (string, bool) {
	s, ok := categorySuffix[cat]
	return s, ok
}

// isQualityCategory 判定是否播放质量类事件（采样规则里的 quality_events 维度）。
func isQualityCategory(cat rpc.BehaviorCategory) bool {
	return cat == rpc.BehaviorCategory_BEHAVIOR_CATEGORY_QUALITY
}

// requiresContentKey 判定该类别是否必须带内容主键。
//
// FOLLOW 的对象是用户（target_mid），不是内容；其余类别都必须能回答「对哪条内容」，
// 否则下游连曝光都归因不到作品，宁可当场拒收也不收无法使用的数据。
func requiresContentKey(cat rpc.BehaviorCategory) bool {
	return cat != rpc.BehaviorCategory_BEHAVIOR_CATEGORY_UNSPECIFIED &&
		cat != rpc.BehaviorCategory_BEHAVIOR_CATEGORY_FOLLOW
}

// validEventTypeString 与 eventenvelope.isValidEventType 同规则：只允许小写字母、
// 数字与点号，且首尾不能有句点。topic 名由 event_type 拼出，放行非法字符等于
// 放行任意 topic（Kafka 侧会自动建 topic，是真实的运维事故）。
func validEventTypeString(s string) bool {
	if s == "" || len(s) > maxEventTypeBytes {
		return false
	}
	if strings.HasPrefix(s, eventTypePrefix) {
		s = strings.TrimPrefix(s, eventTypePrefix)
	}
	if s == "" || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.') {
			return false
		}
	}
	return true
}

// topicForEvent 生成投递 topic：behavior.play + 1 -> behavior.play.v1。
func topicForEvent(eventType string, schemaVersion int32) string {
	return eventenvelope.Topic(eventType, int(schemaVersion))
}

// --- 采集硬约束（ACTIVE 策略优先，缺省回落 config）---

// limits 是一次采集请求实际生效的全部硬上限与规则集合。
//
// 语义（proto 注释、README）：ec_dispatch_policy 是参数真值，config.Collector 只是
// 「策略缺失时的保守兜底」。activeIsConfig=true 时必须让 degraded=true 并可被健康度看到，
// 不允许把「查不到策略」当成「采样率为 0」把事件静默丢掉。
type limits struct {
	policyVersion        string
	activeIsConfig       bool
	stalePolicyHint      bool
	supportedSchema      int32
	saltVersion          int32
	saltRef              string
	maxEventsPerBatch    int32
	maxRequestBytes      int64
	maxEventPayloadBytes int32
	maxClockSkewSeconds  int32
	maxBackfillSeconds   int32
	keywordMaxRunes      int32
	retentionDays        int32
	deliverMaxAttempts   int32
	retryBaseSeconds     int64
	retryMaxSeconds      int64
	defaultSampleBps     int32
	ipSegmentBits        int
	midQps               int32
	deviceQps            int32
	ipSegmentQps         int32
	callerQps            int32
	reportIntervalSecs   int32
	retryHintMs          int32
	sampleRules          []model.SampleRule
	fieldWhitelist       []string
	dropFields           []string
}

// limitsFromConfig 构造兜底限额：全量采样、不丢数据。
func limitsFromConfig(c config.Config) limits {
	col := c.Collector
	return limits{
		policyVersion:        policyMissingSentinel,
		activeIsConfig:       true,
		supportedSchema:      col.SupportedSchemaVersion,
		saltVersion:          c.Privacy.SaltVersion,
		saltRef:              c.Privacy.SaltRef,
		maxEventsPerBatch:    col.MaxEventsPerBatch,
		maxRequestBytes:      col.MaxRequestBytes,
		maxEventPayloadBytes: col.MaxEventPayloadBytes,
		maxClockSkewSeconds:  col.MaxClockSkewSeconds,
		maxBackfillSeconds:   col.MaxBackfillSeconds,
		keywordMaxRunes:      col.KeywordMaxRunes,
		retentionDays:        col.RetentionDays,
		deliverMaxAttempts:   c.Dispatch.DeliverMaxAttempts,
		retryBaseSeconds:     c.Dispatch.RetryBaseSeconds,
		retryMaxSeconds:      c.Dispatch.RetryMaxSeconds,
		defaultSampleBps:     col.DefaultSampleBps,
		ipSegmentBits:        col.IPSegmentBits,
		midQps:               col.MidQps,
		deviceQps:            col.DeviceQps,
		ipSegmentQps:         col.IPSegmentQps,
		callerQps:            col.MidQps,
		reportIntervalSecs:   col.ReportIntervalSeconds,
		retryHintMs:          col.RetryHintMs,
	}
}

// limitsFromPolicy 用 ACTIVE/指定策略覆盖兜底值。
//
// JSON 列解析失败必须报错：空 drop_fields 在调用方看来等于「什么都不禁止」，
// 静默降级会把隐私底线抹掉（AGENTS.md §9）。
func limitsFromPolicy(c config.Config, p *model.DispatchPolicy) (limits, error) {
	lim := limitsFromConfig(c)
	rules, err := model.DecodeSampleRules(p.SampleRulesJSON)
	if err != nil {
		return lim, fmt.Errorf("event-collector: policy %s sample_rules 解析失败: %w", p.Version, err)
	}
	whitelist, err := model.DecodeStringList(p.FieldWhitelistJSON)
	if err != nil {
		return lim, fmt.Errorf("event-collector: policy %s field_whitelist 解析失败: %w", p.Version, err)
	}
	drop, err := model.DecodeStringList(p.DropFieldsJSON)
	if err != nil {
		return lim, fmt.Errorf("event-collector: policy %s drop_fields 解析失败: %w", p.Version, err)
	}
	lim.policyVersion = p.Version
	lim.activeIsConfig = false
	lim.saltVersion = p.SaltVersion
	lim.saltRef = p.SaltRef
	lim.sampleRules = rules
	lim.fieldWhitelist = whitelist
	lim.dropFields = drop
	if p.MaxEventsPerBatch > 0 {
		lim.maxEventsPerBatch = p.MaxEventsPerBatch
	}
	if p.MaxRequestBytes > 0 {
		lim.maxRequestBytes = p.MaxRequestBytes
	}
	if p.MaxEventPayloadBytes > 0 {
		lim.maxEventPayloadBytes = p.MaxEventPayloadBytes
	}
	if p.MaxClockSkewSeconds > 0 {
		lim.maxClockSkewSeconds = p.MaxClockSkewSeconds
	}
	if p.MaxBackfillSeconds > 0 {
		lim.maxBackfillSeconds = p.MaxBackfillSeconds
	}
	if p.KeywordMaxRunes > 0 {
		lim.keywordMaxRunes = p.KeywordMaxRunes
	}
	if p.RetentionDays > 0 {
		lim.retentionDays = p.RetentionDays
	}
	if p.DeliverMaxAttempts > 0 {
		lim.deliverMaxAttempts = p.DeliverMaxAttempts
	}
	if p.RetryBaseSeconds > 0 {
		lim.retryBaseSeconds = p.RetryBaseSeconds
	}
	if p.RetryMaxSeconds > 0 {
		lim.retryMaxSeconds = p.RetryMaxSeconds
	}
	// 配置层已校验过的「代码能力」项以 config 为准：策略不能把支持版本改大，
	// 否则本进程会接受自己解析不了的事件结构。
	if lim.maxEventsPerBatch > c.Collector.MaxEventsPerBatch {
		lim.maxEventsPerBatch = c.Collector.MaxEventsPerBatch
	}
	if lim.maxRequestBytes > c.Collector.MaxRequestBytes {
		lim.maxRequestBytes = c.Collector.MaxRequestBytes
	}
	if int64(lim.maxEventPayloadBytes) > int64(c.Collector.MaxEventPayloadBytes) {
		lim.maxEventPayloadBytes = c.Collector.MaxEventPayloadBytes
	}
	return lim, nil
}

// resolveLimits 取本次请求生效的策略：
//   - hint 非空且等于 ACTIVE 版本：正常（客户端缓存未过期）；
//   - hint 非空但不等于 ACTIVE：仍按 ACTIVE 裁决（策略是服务端真值，客户端无权指定
//     历史/ARCHIVED 版本），把冲突写进批次 last_error 供排障，口径同
//     REJECT_SAMPLING_POLICY_STALE；
//   - 无 ACTIVE 策略：回落 config 保守默认（全量），activeIsConfig=true 由调用方标 degraded。
func resolveLimits(ctx context.Context, s *svc.ServiceContext, hint string) (limits, error) {
	lim := limitsFromConfig(s.Config)
	p, err := cachedActivePolicy(ctx, s)
	switch {
	case errorsIsNoActivePolicy(err):
		logx.Errorf("event-collector/logic: 无 ACTIVE 采样策略，本批回落 config 默认（全量采样），请检查 ec_dispatch_policy")
		lim.stalePolicyHint = hint != ""
		return lim, nil
	case err != nil:
		// 读策略失败不能伪造「没有策略所以全量」之外的任何结论：MySQL 故障时整批
		// 会在后续写入处报错，这里把错误原样交给调用方更诚实。
		return lim, err
	}
	lim, err = limitsFromPolicy(s.Config, p)
	if err != nil {
		return lim, err
	}
	lim.supportedSchema = s.Config.Collector.SupportedSchemaVersion
	lim.stalePolicyHint = hint != "" && hint != p.Version
	return lim, nil
}

func errorsIsNoActivePolicy(err error) bool {
	return errors.Is(err, model.ErrNoActivePolicy)
}

// cachedActivePolicy 读 ACTIVE 策略，Redis 短 TTL 兜一层，降低每条上报的 DB 读压。
// Redis 任何异常都只记日志并回源 MySQL（缓存缺失绝不能变成拒收理由）。
func cachedActivePolicy(ctx context.Context, s *svc.ServiceContext) (*model.DispatchPolicy, error) {
	if s.Cache != nil {
		if raw, err := s.Cache.GetCtx(ctx, activePolicyCacheKey); err == nil && raw != "" {
			var p model.DispatchPolicy
			if jsonErr := json.Unmarshal([]byte(raw), &p); jsonErr == nil && p.State == model.PolicyStateActive {
				return &p, nil
			} else if jsonErr != nil {
				logx.Errorf("event-collector/logic: ACTIVE 策略缓存反序列化失败，回源 MySQL: %v", jsonErr)
			}
		} else if err != nil {
			logx.Errorf("event-collector/logic: 读 ACTIVE 策略缓存失败，回源 MySQL: %v", err)
		}
	}
	p, err := s.Policies.FindActive(ctx)
	if err != nil {
		return nil, err
	}
	writeActivePolicyCache(ctx, s, p)
	return p, nil
}

func writeActivePolicyCache(ctx context.Context, s *svc.ServiceContext, p *model.DispatchPolicy) {
	if s.Cache == nil || p == nil {
		return
	}
	raw, err := json.Marshal(p)
	if err != nil {
		logx.Errorf("event-collector/logic: ACTIVE 策略序列化失败: %v", err)
		return
	}
	if err := s.Cache.SetexCtx(ctx, activePolicyCacheKey, string(raw), activePolicyTTLSecs); err != nil {
		logx.Errorf("event-collector/logic: 写 ACTIVE 策略缓存失败: %v", err)
	}
}

// invalidateActivePolicyCache 在草稿写入/版本切换后调用，避免新策略被当成旧值读 30 秒。
func invalidateActivePolicyCache(ctx context.Context, s *svc.ServiceContext) {
	if s.Cache == nil {
		return
	}
	if _, err := s.Cache.DelCtx(ctx, activePolicyCacheKey); err != nil {
		logx.Errorf("event-collector/logic: 清 ACTIVE 策略缓存失败（TTL %ds 后自愈）: %v",
			activePolicyTTLSecs, err)
	}
}

// resolveSalt 取当前脱敏盐值与盐版本。
//
// 只接受「策略声明的 salt_ref」对应的那份盐：用别的版本的盐算哈希却按本版本归因，
// 事后既不可复现也无法轮换，等价于污染数据，因此宁可报错。
func resolveSalt(s *svc.ServiceContext, lim limits) (string, int32, error) {
	ref := strings.TrimSpace(lim.saltRef)
	version := lim.saltVersion
	if ref == "" {
		ref = strings.TrimSpace(s.Config.Privacy.SaltRef)
	}
	if version <= 0 {
		version = s.Config.Privacy.SaltVersion
	}
	if version <= 0 || ref == "" {
		return "", 0, model.ErrSaltMissing
	}
	if ref == strings.TrimSpace(s.Config.Privacy.SaltRef) {
		// 走 svc.Salt()：本地/测试可用 Privacy.SaltValue 显式注入，生产从 Secret 环境变量读。
		salt, err := s.Salt()
		if err != nil {
			return "", 0, err
		}
		return salt, version, nil
	}
	salt := os.Getenv(ref)
	if salt == "" {
		return "", 0, model.ErrSaltMissing
	}
	return salt, version, nil
}

// --- 脱敏 ---

// privacyFields 是批次级脱敏结果，落库/进信封只允许用这几个字段。
type privacyFields struct {
	mid         int64
	deviceHash  string
	ipSegment   string
	saltVersion int32
}

// hasSubject 判断事件能否归属到主体（REJECT_MISSING_SUBJECT）。
func (p privacyFields) hasSubject() bool {
	return p.mid > 0 || p.deviceHash != ""
}

// desensitize 把明文设备号/出口 IP 收敛成加盐哈希与脱敏段。
//
// 明文只在函数入参里存在，不出日志、不入库、不进信封。device_id 非空却算不出哈希
// （盐缺失）时直接返回 model.ErrSaltMissing，由调用方整批拒绝。
func desensitize(salt string, saltVersion int32, mc *rpc.EventContext, bits int) (privacyFields, error) {
	out := privacyFields{mid: mc.GetMid(), saltVersion: saltVersion}
	if dev := strings.TrimSpace(mc.GetDeviceId()); dev != "" {
		hash, err := model.SaltedHash(salt, saltVersion, dev)
		if err != nil {
			return out, err
		}
		out.deviceHash = fitColumn(hash, maxDigestBytes)
	}
	if ip := strings.TrimSpace(mc.GetIp()); ip != "" {
		out.ipSegment = fitColumn(model.IPSegment(ip, bits), maxIPSegmentBytes)
	}
	return out, nil
}

// --- 字符串入库收敛 ---

// fitColumn 按字节截断并保证 UTF-8 边界完整（列宽是字节数，不是 rune 数）。
// 截断本身不是错误：被截的都是排障用的描述性字段，业务真值（ID/摘要）长度已由校验保证。
func fitColumn(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := s[:max]
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r == utf8.RuneError && size <= 1 {
			// 截断点把一个多字节 rune 切坏了，逐字节回退到完整边界。
			cut = cut[:len(cut)-1]
			continue
		}
		break
	}
	return cut
}

// --- payload 清洗 ---

// builtinForbiddenPayloadKeys 是任何策略都不允许进 payload 的字段（内置底线白名单）。
//
// 策略的 drop_fields 只能「再加严」，不能把这些字段放回白名单：AGENTS.md §7 的
// 隐私底线不能靠一次运营配置改策略就绕过。
var builtinForbiddenPayloadKeys = map[string]struct{}{
	"ip": {}, "ip_addr": {}, "client_ip": {}, "remote_ip": {}, "user_ip": {}, "x_forwarded_for": {},
	"device_id": {}, "deviceid": {}, "imei": {}, "idfa": {}, "oaid": {}, "odid": {}, "android_id": {},
	"caid": {}, "mac": {}, "mac_address": {}, "serial": {}, "serial_number": {},
	"phone": {}, "mobile": {}, "tel": {}, "phone_number": {}, "telephone": {},
	"email": {}, "email_addr": {},
	"token": {}, "access_token": {}, "refresh_token": {}, "session_token": {}, "auth_token": {},
	"cookie": {}, "authorization": {}, "password": {}, "passwd": {}, "pwd": {},
	"id_card": {}, "idcard": {}, "id_no": {}, "identity_no": {}, "passport": {}, "real_name": {},
	"name": {}, "nickname": {}, "true_name": {}, "address": {}, "home_address": {}, "gps": {},
	"latitude": {}, "longitude": {}, "lng": {}, "lat": {}, "contacts": {}, "sms_code": {},
}

// normalizeFieldName 统一字段名比较口径：小写、'-' 归一为 '_'、去首尾空白。
// MySQL 是 utf8mb4_bin，但 JSON 字段名的大小写不该成为隐私底线漏口的理由。
func normalizeFieldName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	return strings.ReplaceAll(s, "-", "_")
}

func isBuiltinForbidden(name string) bool {
	_, ok := builtinForbiddenPayloadKeys[normalizeFieldName(name)]
	return ok
}

// payloadCheck 是 payload 的清洗结论。offending 只可能是「字段名」，绝不带字段值。
type payloadCheck struct {
	ok       bool
	reason   int32
	offended string
	digest   string
	size     int32
	notes    []string
}

// checkPayload 校验并清洗事件正文，返回「清洗后正文的摘要」。
//
// 顺序：大小 → 是否 JSON 对象 → 递归禁止字段 → 顶层白名单过滤 → 重序列化取摘要。
// 摘要算在清洗之后的正文上：dispatcher 发的就是这份正文，摘要才能用于比对
// 「重放的是不是同一条内容」。
func checkPayload(raw string, lim limits, dropFields []string) payloadCheck {
	res := payloadCheck{reason: int32(rpc.RejectReason_REJECT_REASON_NONE)}
	if strings.TrimSpace(raw) == "" {
		return res
	}
	if int64(len(raw)) > int64(lim.maxEventPayloadBytes) {
		res.ok = false
		res.reason = int32(rpc.RejectReason_REJECT_PAYLOAD_TOO_LARGE)
		res.offended = "payload"
		return res
	}
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		res.ok = false
		res.reason = int32(rpc.RejectReason_REJECT_INVALID_PAYLOAD)
		res.offended = "payload"
		return res
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		res.ok = false
		res.reason = int32(rpc.RejectReason_REJECT_INVALID_PAYLOAD)
		res.offended = "payload"
		return res
	}
	forbidden := buildForbiddenSet(dropFields)
	dropped, bad := scanForbidden(obj, forbidden)
	if bad != "" {
		res.ok = false
		res.reason = int32(rpc.RejectReason_REJECT_PRIVACY_FIELD)
		res.offended = bad
		return res
	}
	if len(dropped) > 0 {
		res.notes = append(res.notes, "已剥离禁止入库字段："+strings.Join(dropped, ","))
	}
	if n := applyTopLevelWhitelist(obj, lim.fieldWhitelist); n > 0 {
		res.notes = append(res.notes, "已剥离非白名单字段 "+strconv.Itoa(n)+" 个")
	}
	cleaned, err := json.Marshal(obj)
	if err != nil {
		res.ok = false
		res.reason = int32(rpc.RejectReason_REJECT_INVALID_PAYLOAD)
		res.offended = "payload"
		return res
	}
	res.digest, res.size = model.PayloadDigest(string(cleaned))
	res.digest = fitColumn(res.digest, maxDigestBytes)
	res.ok = true
	return res
}

// buildForbiddenSet 把策略的 drop_fields 与内置底线合并（策略只能加严）。
func buildForbiddenSet(dropFields []string) map[string]struct{} {
	set := make(map[string]struct{}, len(dropFields))
	for _, f := range dropFields {
		if s := normalizeFieldName(f); s != "" {
			set[s] = struct{}{}
		}
	}
	return set
}

// scanForbidden 递归查找禁止字段：命中即整条事件拒绝，命中字段本身也一并剥离。
// 返回 (被剥离的字段名列表, 第一个命中的禁止字段名)。
func scanForbidden(v any, forbidden map[string]struct{}) ([]string, string) {
	var dropped []string
	var first string
	switch node := v.(type) {
	case map[string]json.RawMessage:
		for key, child := range node {
			hit := isForbiddenKey(key, forbidden)
			if hit && first == "" {
				first = normalizeFieldName(key)
			}
			if hit {
				delete(node, key)
				dropped = append(dropped, normalizeFieldName(key))
				continue
			}
			var sub any
			if err := json.Unmarshal(child, &sub); err != nil {
				continue
			}
			subDropped, subFirst := scanForbidden(sub, forbidden)
			dropped = append(dropped, subDropped...)
			if subFirst != "" && first == "" {
				first = subFirst
			}
			if len(subDropped) > 0 {
				if reb, err := json.Marshal(sub); err == nil {
					node[key] = json.RawMessage(reb)
				}
			}
		}
	case []any:
		for _, item := range node {
			subDropped, subFirst := scanForbidden(item, forbidden)
			dropped = append(dropped, subDropped...)
			if subFirst != "" && first == "" {
				first = subFirst
			}
		}
	case map[string]any:
		// 嵌套对象（json.Unmarshal 到 any 得到 map[string]any）里的明文标识同样要拦：
		// 只查顶层等于给「把 phone 塞进 ext 里」开绿灯。
		for key, child := range node {
			if isForbiddenKey(key, forbidden) {
				if first == "" {
					first = normalizeFieldName(key)
				}
				delete(node, key)
				dropped = append(dropped, normalizeFieldName(key))
				continue
			}
			subDropped, subFirst := scanForbidden(child, forbidden)
			dropped = append(dropped, subDropped...)
			if subFirst != "" && first == "" {
				first = subFirst
			}
		}
	}
	return dropped, first
}

func isForbiddenKey(key string, forbidden map[string]struct{}) bool {
	n := normalizeFieldName(key)
	if _, ok := builtinForbiddenPayloadKeys[n]; ok {
		return true
	}
	if len(forbidden) == 0 {
		return false
	}
	_, ok := forbidden[n]
	return ok
}

// applyTopLevelWhitelist 只过滤顶层字段：白名单是「允许保留的字段名」口径，
// 嵌套结构里的同名子字段不该被连带删空（策略配了白名单却把 payload 抹成 {} 是事故）。
// field_whitelist 为空表示不启用白名单（只执行禁止字段剥离），由 README 记录该口径。
func applyTopLevelWhitelist(obj map[string]json.RawMessage, whitelist []string) int {
	if len(obj) == 0 || len(whitelist) == 0 {
		return 0
	}
	allowed := make(map[string]struct{}, len(whitelist))
	for _, f := range whitelist {
		if s := normalizeFieldName(f); s != "" {
			allowed[s] = struct{}{}
		}
	}
	dropped := 0
	for key := range obj {
		if _, ok := allowed[normalizeFieldName(key)]; !ok {
			delete(obj, key)
			dropped++
		}
	}
	return dropped
}

// --- 逐条校验（与 CollectEvents / IngestServerEvents / ValidateEventSchema 共用）---

// eventVerdict 是单条事件的校验结论 + 已脱敏的待落库行。
type eventVerdict struct {
	decision int32
	reason   int32
	message  string
	missing  []string
	notes    []string
	rec      *model.EventRecord
	// eventID 归一化（trim）后的事件幂等键；为空表示连去重键都没有。
	eventID string
	// eventType/topic 归一化结果（干跑校验要回带给调用方）。
	eventType string
	topic     string
	stored    bool // 是否需要落 ec_event_record
}

// reject 将结论改为拒绝（detail 只允许带字段名与口径，绝不带事件正文）。
//
// event_id 合法时被拒事件也要留 ec_event_record 行（decision=3, delivery_state=NONE），
// 否则事后无法解释「客户端说报了、库里没有」；骨架行里不落 topic/envelope，
// 因为它永远不该被投递。
func (v *eventVerdict) reject(reason rpc.RejectReason, detail string, missing ...string) *eventVerdict {
	v.decision = model.DecisionRejected
	v.reason = int32(reason)
	v.message = detail
	v.topic = ""
	v.missing = append(v.missing, missing...)
	if v.rec == nil {
		v.stored = false
		return v
	}
	v.rec.Decision = model.DecisionRejected
	v.rec.Reason = v.reason
	v.rec.ReasonDetail = fitColumn(detail, maxReasonBytes)
	v.rec.DeliveryState = model.DeliveryStateNone
	v.rec.Topic = ""
	v.rec.EnvelopeEventID = ""
	v.rec.NextRetryAt = 0
	v.stored = true
	return v
}

// validateEvent 执行「校验 → 脱敏归一化」，不做采样（采样在脱敏之后，见 ingest.go）。
//
// 抽成独立函数的目的是让 ValidateEventSchema 与采集路径共用同一份规则：
// 两套规则必然漂移，最后变成「干跑通过、真跑被拒」。
func validateEvent(ev *rpc.BehaviorEvent, source int32, pv privacyFields, lim limits,
	now int64, batchTraceID string) *eventVerdict {

	v := &eventVerdict{decision: model.DecisionAccepted, reason: reasonNone()}
	if ev == nil {
		return v.reject(rpc.RejectReason_REJECT_INTERNAL, "event is nil")
	}
	// event_id 先判：它是 uniq_event_id 的去重键，没有键就留不了痕，后面所有归因都无从谈起。
	v.eventID = strings.TrimSpace(ev.GetEventId())
	if v.eventID == "" || len(v.eventID) > maxEventIDBytes {
		return v.reject(rpc.RejectReason_REJECT_MISSING_EVENT_ID,
			"event_id 必填且不超过 "+strconv.Itoa(maxEventIDBytes)+" 字节", "event_id")
	}
	traceID := fitColumn(strings.TrimSpace(ev.GetTraceId()), maxTraceIDBytes)
	if traceID == "" {
		traceID = fitColumn(strings.TrimSpace(batchTraceID), maxTraceIDBytes)
	}
	// 骨架台账行：event_id 合法之后，无论通过还是被拒都要留痕（model.DecisionStored 口径，
	// RecountFromRecords 也按 decision=3 统计 rejected）。这里只写脱敏列与定长安全列，
	// 正文、搜索词、可疑 event_type 都不进骨架，避免被拒事件把脏值写进热表。
	v.rec = &model.EventRecord{
		EventID:         fitColumn(v.eventID, maxEventIDBytes),
		Category:        int32(ev.GetCategory()),
		SchemaVersion:   ev.GetSchemaVersion(),
		OccurredAt:      ev.GetOccurredAt(),
		ReceivedAt:      now,
		Decision:        model.DecisionAccepted,
		Reason:          reasonNone(),
		DeliveryState:   model.DeliveryStateNone,
		Mid:             pv.mid,
		DeviceHash:      pv.deviceHash,
		IPSegment:       pv.ipSegment,
		SaltVersion:     pv.saltVersion,
		ContentType:     fitColumn(strings.TrimSpace(ev.GetContentType()), maxContentTypeBytes),
		ContentID:       ev.GetContentId(),
		Aid:             ev.GetAid(),
		Vid:             fitColumn(strings.TrimSpace(ev.GetVid()), maxVidBytes),
		SessionID:       fitColumn(strings.TrimSpace(ev.GetSessionId()), maxSessionIDBytes),
		TargetMid:       ev.GetTargetMid(),
		SanitizeVersion: fitColumn(lim.policyVersion, maxTagBytes),
		PolicyVersion:   fitColumn(lim.policyVersion, maxPolicyVerBytes),
		TraceID:         traceID,
	}
	v.stored = true

	cat := ev.GetCategory()
	suffix, catOK := eventSuffixOf(cat)
	eventType, ok := model.NormalizeEventType(ev.GetEventType(), suffix)
	if !ok {
		return v.reject(rpc.RejectReason_REJECT_MISSING_EVENT_TYPE,
			"event_type 缺失且 category 无法推导", "event_type", "category")
	}
	v.eventType = eventType
	if !validEventTypeString(eventType) {
		return v.reject(rpc.RejectReason_REJECT_UNSUPPORTED_EVENT_TYPE,
			"event_type 只允许小写字母/数字/点号且不超过 "+strconv.Itoa(maxEventTypeBytes)+" 字节")
	}
	// category 未给出时按 event_type 反推，保证台账的 category 维度不为 0（下游按类别聚合）。
	if !catOK {
		if s := strings.TrimPrefix(eventType, eventTypePrefix); s != "" {
			for c, suf := range categorySuffix {
				if suf == s {
					cat = c
					break
				}
			}
		}
	}
	v.rec.Category = int32(cat)
	v.rec.EventType = fitColumn(eventType, maxEventTypeBytes)

	sv := ev.GetSchemaVersion()
	if sv <= 0 {
		return v.reject(rpc.RejectReason_REJECT_MISSING_SCHEMA_VERSION,
			"schema_version 必填（>=1）", "schema_version")
	}
	if !model.SchemaSupported(sv, lim.supportedSchema) {
		return v.reject(rpc.RejectReason_REJECT_UNSUPPORTED_SCHEMA_VERSION,
			"schema_version="+strconv.Itoa(int(sv))+" 不在 (1,"+strconv.Itoa(int(lim.supportedSchema))+"] 区间")
	}

	occurred := ev.GetOccurredAt()
	if occurred <= 0 {
		return v.reject(rpc.RejectReason_REJECT_MISSING_OCCURRED_AT, "occurred_at 必填（Unix 秒）", "occurred_at")
	}
	skew := occurred - now
	if skew > int64(lim.maxClockSkewSeconds) {
		return v.reject(rpc.RejectReason_REJECT_TIME_IN_FUTURE,
			"occurred_at 比服务端时间晚 "+strconv.FormatInt(skew, 10)+" 秒，超过 max_clock_skew_seconds")
	}
	if skew < 0 && -skew > int64(lim.maxClockSkewSeconds) {
		if -skew > int64(lim.maxBackfillSeconds) {
			return v.reject(rpc.RejectReason_REJECT_EVENT_TOO_OLD,
				"事件年龄 "+strconv.FormatInt(-skew, 10)+" 秒超过回补窗口 max_backfill_seconds")
		}
		return v.reject(rpc.RejectReason_REJECT_CLOCK_SKEW,
			"occurred_at 偏差 "+strconv.FormatInt(skew, 10)+" 秒超过 max_clock_skew_seconds")
	}
	if rep := ev.GetReportedAt(); rep > 0 && rep-now > int64(lim.maxClockSkewSeconds) {
		return v.reject(rpc.RejectReason_REJECT_CLOCK_SKEW,
			"reported_at 在未来，客户端时钟需要重新校准", "reported_at")
	}

	if !pv.hasSubject() {
		return v.reject(rpc.RejectReason_REJECT_MISSING_SUBJECT,
			"mid 与设备标识同时缺失，事件无法归属", "mid", "device_id")
	}

	hasContentKey := ev.GetContentId() > 0 || ev.GetAid() > 0 ||
		strings.TrimSpace(ev.GetVid()) != "" || strings.TrimSpace(ev.GetSessionId()) != ""
	if requiresContentKey(cat) && !hasContentKey {
		return v.reject(rpc.RejectReason_REJECT_MISSING_CONTENT_KEY,
			"content_id/aid/vid/session_id 至少给一个", "content_id", "aid", "vid", "session_id")
	}
	if cat == rpc.BehaviorCategory_BEHAVIOR_CATEGORY_FOLLOW && ev.GetTargetMid() <= 0 {
		return v.reject(rpc.RejectReason_REJECT_MISSING_CONTENT_KEY,
			"关注事件必须给出目标用户 target_mid", "target_mid")
	}

	if bad := invalidMetricField(ev); bad != "" {
		return v.reject(rpc.RejectReason_REJECT_INVALID_METRIC, "数值指标越界: "+bad, bad)
	}

	if source == model.SourceServer && traceID == "" {
		return v.reject(rpc.RejectReason_REJECT_MISSING_TRACE_ID,
			"服务端埋点必须携带 trace_id（事件级或批次级均可）", "trace_id")
	}

	pc := checkPayload(ev.GetPayload(), lim, lim.dropFields)
	if !pc.ok {
		reason := rpc.RejectReason(pc.reason)
		detail := "payload 必须是 JSON 对象"
		switch reason {
		case rpc.RejectReason_REJECT_PRIVACY_FIELD:
			detail = "payload 含禁止入库字段（明文标识/凭据），整条拒绝: " + pc.offended
		case rpc.RejectReason_REJECT_PAYLOAD_TOO_LARGE:
			detail = "payload 超过 max_event_payload_bytes=" + strconv.Itoa(int(lim.maxEventPayloadBytes))
		}
		return v.reject(reason, detail, pc.offended)
	}
	v.notes = append(v.notes, pc.notes...)

	keyword, kwRunes, kwTruncated := model.KeywordDigest(ev.GetKeyword(), int(lim.keywordMaxRunes))
	if kwTruncated {
		v.notes = append(v.notes, "keyword 超过 keyword_max_runes="+
			strconv.Itoa(int(lim.keywordMaxRunes))+"，只保留前缀且仅存摘要")
	}
	topic := topicForEvent(eventType, sv)
	if topic == "" {
		return v.reject(rpc.RejectReason_REJECT_UNSUPPORTED_EVENT_TYPE,
			"topic 归一化失败（event_type 与 schema_version 组合非法）")
	}

	v.rec.SchemaVersion = sv
	v.rec.OccurredAt = occurred
	v.rec.ClockSkewSeconds = skew
	v.rec.KeywordDigest = fitColumn(keyword, maxDigestBytes)
	v.rec.KeywordRunes = kwRunes
	v.rec.PayloadDigest = pc.digest
	v.rec.PayloadBytes = pc.size
	v.rec.Topic = fitColumn(topic, maxTopicBytes)
	v.rec.DeliveryState = model.DeliveryStatePending
	v.topic = v.rec.Topic
	return v
}

// invalidMetricField 返回第一个越界的数值指标字段名（只给字段名，不给值）。
func invalidMetricField(ev *rpc.BehaviorEvent) string {
	switch {
	case ev.GetPositionMs() < 0:
		return "position_ms"
	case ev.GetDurationMs() < 0:
		return "duration_ms"
	case ev.GetPositionMs() > 0 && ev.GetDurationMs() > 0 && ev.GetPositionMs() > ev.GetDurationMs():
		return "position_ms"
	case ev.GetBufferCount() < 0:
		return "buffer_count"
	case ev.GetFirstFrameMs() < 0:
		return "first_frame_ms"
	case ev.GetAvgBitrate() < 0:
		return "avg_bitrate"
	case ev.GetResultIndex() < 0:
		return "result_index"
	}
	return ""
}

// --- 运营/兜底动作的轮次幂等（Redis 加速，真值在 MySQL）---

// claimOpDedup 抢占某个 idempotency_key 的执行权。
//
// 返回 first=true 表示本次是首次执行；first=false 时 raw 是首次结论（可能为空，
// 表示首次仍在跑）。Redis 不可用时返回 first=true 并记日志：本服务的真值判定
// 全在 MySQL（批次 uniq、Outbox uniq、租约 CAS、死信 state CAS），
// 缓存故障绝不能变成「拒绝运维推进」，也不能变成「重复副作用」。
func claimOpDedup(ctx context.Context, s *svc.ServiceContext, rpcName, key string) (bool, string, error) {
	if s.Cache == nil || rpcName == "" || key == "" {
		return true, "", nil
	}
	opKey := opDedupKeyPrefix + rpcName + ":" + key
	ok, err := s.Cache.SetnxExCtx(ctx, opKey, opInflightMarker, opDedupTTLSecs)
	if err != nil {
		logx.Errorf("event-collector/logic: 抢占 %s 幂等键失败（按无缓存执行，DB 层幂等兜底）: %v", rpcName, err)
		return true, "", nil
	}
	if ok {
		return true, "", nil
	}
	raw, err := s.Cache.GetCtx(ctx, opKey)
	if err != nil {
		logx.Errorf("event-collector/logic: 读 %s 幂等结论失败: %v", rpcName, err)
		return true, "", nil
	}
	if raw == "" || raw == opInflightMarker {
		return false, "", fmt.Errorf("%w: %s idempotency_key is still running", model.ErrConcurrentUpdate, rpcName)
	}
	return false, raw, nil
}

// saveOpDedupResult 回填首次执行的结论快照。失败只记日志：副作用已经产生，
// 不能因为「记不下结果」再对外报错，否则调用方会以为可以安全重试。
func saveOpDedupResult(ctx context.Context, s *svc.ServiceContext, rpcName, key string,
	msg proto.Message, logger logx.Logger) {
	if s.Cache == nil || rpcName == "" || key == "" || msg == nil {
		return
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		logx.Errorf("event-collector/logic: 序列化 %s 幂等结论失败: %v", rpcName, err)
		return
	}
	opKey := opDedupKeyPrefix + rpcName + ":" + key
	if err := s.Cache.SetexCtx(ctx, opKey, string(raw), opDedupTTLSecs); err != nil {
		if logger != nil {
			logger.Errorf("event-collector/logic: 回填 %s 幂等结论失败: %v", rpcName, err)
			return
		}
		logx.Errorf("event-collector/logic: 回填 %s 幂等结论失败: %v", rpcName, err)
	}
}

// unmarshalOpResult 回放首次结论；解析失败明确报错，不返回空 Reply 伪造成功。
func unmarshalOpResult(raw string, into proto.Message) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: empty stored result", model.ErrConcurrentUpdate)
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		return fmt.Errorf("%w: unmarshal stored result: %v", model.ErrConcurrentUpdate, err)
	}
	return nil
}

// --- 维度限流（mid / 设备 / IP 段 / caller_service）---

// counterOver 固定窗口计数并判断是否超限。delta 是本次计入的事件条数（一次上报可能带
// 50 条事件，只加 1 的话 *Qps 阈值就成了「批次/秒」，与配置注释的事件口径不符）。
//
// 注意 key 只能用已脱敏维度值（device_hash / ip_segment / mid / caller_service）：
// 把明文设备号拼进 Redis key 等于在另一套存储里留明文，AGENTS.md §7 直接破防。
// Redis 出错返回 error，由调用方 fail-open + 标 degraded：这三个阈值只是
// 「保护自身」的下限（真值判定在 risk-control，见 README），
// 缓存抖动时宁可少限流也不要把全部流量整批拒掉。
func counterOver(ctx context.Context, s *svc.ServiceContext, dim, value string, qps int32,
	delta int64, now int64) (bool, error) {
	if s.Cache == nil {
		return false, fmt.Errorf("event-collector: redis not configured")
	}
	if value == "" || qps <= 0 {
		return false, nil
	}
	if delta <= 0 {
		delta = 1
	}
	key := rateLimitKeyPrefix + dim + ":" + fitColumn(value, maxLeaseOwnerBytes) + ":" +
		strconv.FormatInt(now/rateLimitWindowSecs, 10)
	n, err := s.Cache.IncrbyCtx(ctx, key, delta)
	if err != nil {
		return false, err
	}
	if n == delta {
		// INCRBY 对不存在的键返回 delta 本身，即「本次建键」：只有这时才设 TTL，
		// 否则反复 Expire 会把固定窗口无限拉长（限流形同虚设）。
		if err := s.Cache.ExpireCtx(ctx, key, rateLimitWindowSecs*2); err != nil {
			logx.Errorf("event-collector/logic: 设置限流窗口 TTL 失败: %v", err)
		}
	}
	return n > int64(qps), nil
}

// --- 分页 ---

// clampPage 收敛每页条数：0 用默认值，越界直接报错（不静默截断成上限，
// 那会让调用方误以为自己拿到了整页）。
func clampPage(c config.Config, ps int32) (int32, error) {
	size := model.ClampPageSize(ps, c.Collector.PageSize, c.Collector.MaxPageSize)
	if size <= 0 {
		return 0, model.ErrInvalidPage
	}
	return size, nil
}

// fetchMoreLimit 多取一条探测 has_more，避免额外 COUNT 查询。
func fetchMoreLimit(ps int32) int32 { return ps + 1 }

// trimPage 裁掉探测用的最后一行，返回 (本页行, 是否还有更多)。
func trimPage[T any](rows []T, ps int32) ([]T, bool) {
	if int32(len(rows)) > ps {
		return rows[:ps], true
	}
	return rows, false
}

// --- 其它 ---

// timeNow 服务端当前 Unix 秒。集中一个取时点：一次请求内所有判定共用同一时刻，
// 避免「校验用 t1、落库用 t2」导致同一批事件的时钟偏差结论不一致。
func timeNow() int64 { return time.Now().Unix() }

// buildVersion 返回构建注入/模块版本；读不到就返回空串并让调用方记日志，
// 不伪造 "unknown" 当成「版本正常」（AGENTS.md §9）。
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return ""
	}
	if v := strings.TrimSpace(info.Main.Version); v != "" && v != "(devel)" {
		return v
	}
	return ""
}

// requestBytes 估算请求体大小（proto 编码后字节数），用于 max_request_bytes 闸门。
func requestBytes(m proto.Message) int64 {
	if m == nil {
		return 0
	}
	return int64(proto.Size(m))
}

// chunkIDs 按 n 切分字符串切片（model 侧批量上限兜底）。
func chunkIDs(ids []string, n int) [][]string {
	if n <= 0 {
		n = listChunkSize
	}
	var out [][]string
	for i := 0; i < len(ids); i += n {
		end := i + n
		if end > len(ids) {
			end = len(ids)
		}
		out = append(out, ids[i:end])
	}
	return out
}

// chunkRows 通用行切分。
func chunkRows[T any](rows []T, n int) [][]T {
	if n <= 0 {
		n = listChunkSize
	}
	var out [][]T
	for i := 0; i < len(rows); i += n {
		end := i + n
		if end > len(rows) {
			end = len(rows)
		}
		out = append(out, rows[i:end])
	}
	return out
}

// workerID 生成投递租约持有者标识（写进 ec_pending_delivery.lease_owner，<=64 字节）。
// 租约是「谁在推进这一行」的唯一线索，崩溃接管与事故追责都靠它，所以带上轮次时刻。
func workerID(operator string, round int64) string {
	name := strings.TrimSpace(operator)
	if name == "" {
		name = "event-collector"
	}
	return fitColumn(model.LeaseKey(name, round), maxLeaseOwnerBytes)
}

// --- 枚举取值helper：集中做 rpc.* → int32 转换，避免各处手写 int32() 漏掉一个 ---

func reasonNone() int32 { return int32(rpc.RejectReason_REJECT_REASON_NONE) }

func pendingReason() int32 { return int32(rpc.RejectReason_REJECT_STORE_FAILED) }

func dispatchedReason() int32 { return int32(rpc.RejectReason_REJECT_SAMPLING_POLICY_STALE) }

func rateLimitedReason() int32 { return int32(rpc.RejectReason_REJECT_RATE_LIMITED) }

func batchTooLargeReason() int32 { return int32(rpc.RejectReason_REJECT_BATCH_TOO_LARGE) }

func internalReason() int32 { return int32(rpc.RejectReason_REJECT_INTERNAL) }

// topReasonFor 决定批次台账的 top_reason：有通过事件时写 NONE，
// 全批没收下时写最严重的那个原因码（数值越大越严重只是稳定顺序，供排障取一个代表）。
func topReasonFor(accepted int32, worst int32) int32 {
	if accepted > 0 {
		return reasonNone()
	}
	if worst > 0 {
		return worst
	}
	return reasonNone()
}

func worstReason(cur, candidate int32) int32 {
	if candidate > cur {
		return candidate
	}
	return cur
}

// batchStateName 给日志用（不把枚举数字留在人读文案里）。
func batchStateName(state int32) string {
	if s, ok := rpc.BatchState_name[state]; ok {
		return s
	}
	return "BATCH_STATE_UNKNOWN"
}

// terminalDeliveryFrom 返回可以安全地「直接判为已投递」的历史状态集合。
//
// PENDING 不在其中：Outbox 行还在，事件就没发出去过，直接标 SENT 等于伪造投递事实
// （AGENTS.md §9）。SENT 重复标记由 MarkDelivery 的 state IN 条件天然幂等。
func terminalDeliveryFrom() []int32 {
	return []int32{
		model.DeliveryStateRetrying,
		model.DeliveryStateDead,
		model.DeliveryStateSent,
	}
}
