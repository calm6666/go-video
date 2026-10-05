// 本文件是 logic 包的手写策略装配层（RPC 策略入参 → 待写入草稿 + 入库前全量自检），
// 不是 goctl 生成产物。
//
// 为什么校验必须写在 logic 而不是只靠 model.checkPolicy：
// model 只守住「这一行能存下来」（列宽、必填、JSON 可解析），
// 而策略是**采集裁决的真值**——一条把 drop_fields 写空的草稿，存得下来，
// 一旦激活就等于把 AGENTS.md §7 的隐私底线整条抹掉。
// 因此这里补上 model 无从得知的语义约束：字段名白名单不得覆盖内置底线、
// 采样规则只能指向已登记的 event_type、数值不得越过 config 声明的服务端硬上限。
//
// 版本化底线（AGENTS.md §5）：采样比例与盐版本都随策略版本冻结，
// 重放同一批次必须得到同样的裁决，所以任何会影响裁决的参数都必须进策略列，
// 且不允许「原地改 ACTIVE」——那是篡改历史归因依据。

package logic

import (
	"fmt"
	"strings"

	"go-video/services/event-collector/internal/config"
	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"
)

// 策略数值的服务端硬区间。
//
// 上限取 config.Config.Validate 里的同一批常数：config 自检已经把「危险的宽配置」
// 挡在启动期，策略就不能成为绕过它的后门（例如把 max_request_bytes 配成 2GB 打爆 MySQL 包）。
const (
	hardMaxEventsPerBatch  = 5000
	hardMaxRequestBytes    = int64(8 << 20)
	hardMinPayloadBytes    = 256
	hardMaxKeywordRunes    = 4096
	hardMaxDeliverAttempts = 50
	hardMaxRetryBaseSecs   = int64(86400)
	hardMaxRetentionDays   = 3660
)

// knownEventTypes 采样规则可引用的 event_type 白名单（与 categorySuffix 同源）。
//
// 采样规则写了一个本服务不认识的 event_type 不会报错，只会「永远匹配不上、
// 于是走兜底规则」——这种静默失配是排查「为什么这类事件全量进来了」最费劲的一类坑，
// 所以入库前就拒。
var knownEventTypes = func() map[string]struct{} {
	out := make(map[string]struct{}, len(categorySuffix))
	for _, suffix := range categorySuffix {
		out[eventTypePrefix+suffix] = struct{}{}
	}
	return out
}()

// policyFromRPC 把 RPC 策略入参装配成待写入的 DRAFT 行，并完成全量自检。
//
// 数值为 0 表示「调用方没配，继承 config 默认」（proto3 无 optional 语义），
// 非 0 值一律落在 (0, config 同项] 区间内：策略可以收紧，但不能放宽到本进程
// 自身上限之外——limitsFromPolicy 会在读路径按 config 封顶，
// 若允许写进一个更大的值，库里存的数与实际生效的数就会不一致，事后归因即失真。
func policyFromRPC(in *rpc.DispatchPolicy, operator string, c config.Config) (*model.DispatchPolicy, error) {
	if in == nil {
		return nil, fmt.Errorf("event-collector: policy 必填")
	}
	if !validPolicyVersion(in.Version) {
		return nil, fmt.Errorf("event-collector: policy.version 非法：需要形如 2026.09.20-1 的语义化版本，"+
			"不超过 %d 字节且不含空白/通配/引号字符", maxVersionTagBytes)
	}
	// 状态只能由 ActivateDispatchPolicy 迁移：Upsert 把 state 当「我要生效」来用是最常见的
	// 误操作，静默忽略等于替调用方改掉了它自己的意图。
	if in.State != rpc.PolicyState_POLICY_STATE_UNSPECIFIED && in.State != rpc.PolicyState_POLICY_STATE_DRAFT {
		return nil, fmt.Errorf("%w: policy.version=%s 的 state=%s 不能由 Upsert 指定，草稿请用 %s，"+
			"生效请调用 ActivateDispatchPolicy", model.ErrActivePolicyImmutable, in.Version, in.State,
			rpc.PolicyState_POLICY_STATE_DRAFT)
	}
	if !validOperatorName(operator) {
		return nil, fmt.Errorf("%w: operator 必填且不超过 %d 字节、不含空白", model.ErrOperatorRequired,
			maxOperatorBytes)
	}

	col := c.Collector
	p := &model.DispatchPolicy{
		Version:              in.Version,
		State:                model.PolicyStateDraft,
		SaltVersion:          in.SaltVersion,
		SaltRef:              strings.TrimSpace(in.SaltRef),
		MaxEventsPerBatch:    inherit32(in.MaxEventsPerBatch, col.MaxEventsPerBatch),
		MaxRequestBytes:      inherit64(in.MaxRequestBytes, col.MaxRequestBytes),
		MaxEventPayloadBytes: inherit32(in.MaxEventPayloadBytes, col.MaxEventPayloadBytes),
		MaxClockSkewSeconds:  inherit32(in.MaxClockSkewSeconds, col.MaxClockSkewSeconds),
		MaxBackfillSeconds:   inherit32(in.MaxBackfillSeconds, col.MaxBackfillSeconds),
		KeywordMaxRunes:      inherit32(in.KeywordMaxRunes, col.KeywordMaxRunes),
		RetentionDays:        inherit32(in.RetentionDays, col.RetentionDays),
		DeliverMaxAttempts:   inherit32(in.DeliverMaxAttempts, c.Dispatch.DeliverMaxAttempts),
		RetryBaseSeconds:     inherit64(in.RetryBaseSeconds, c.Dispatch.RetryBaseSeconds),
		RetryMaxSeconds:      inherit64(in.RetryMaxSeconds, c.Dispatch.RetryMaxSeconds),
		Note:                 fitColumn(strings.TrimSpace(in.Note), maxNoteBytes),
		Operator:             fitColumn(operator, maxOperatorBytes),
	}

	// --- 脱敏参数 ---
	if p.SaltVersion <= 0 {
		return nil, fmt.Errorf("%w: policy.salt_version 必须 > 0：同一盐版本下哈希才可复现、才可轮换",
			model.ErrSaltMissing)
	}
	if !validSaltRef(p.SaltRef) {
		return nil, fmt.Errorf("%w: policy.salt_ref 必须是「环境变量名」（大写字母/数字/下划线，<= %d 字节），"+
			"不是盐值本身：盐值永不入库、不出 RPC 响应", model.ErrSaltMissing, maxSaltRefBytes)
	}

	// --- 采样规则 ---
	rulesJSON, err := encodeSampleRulesForPolicy(in.SampleRules)
	if err != nil {
		return nil, err
	}
	p.SampleRulesJSON = rulesJSON

	// --- 字段名单 ---
	whitelist, err := clampListItems("field_whitelist", in.FieldWhitelist)
	if err != nil {
		return nil, err
	}
	for _, name := range whitelist {
		// 隐私底线不可被配置覆盖：把 token/phone 放进白名单，等于声明「这些明文允许入库」。
		if isBuiltinForbidden(name) {
			return nil, fmt.Errorf("%w: field_whitelist 含内置底线字段 %q，白名单不能放宽 AGENTS.md §7 的禁止项",
				model.ErrPrivacyFieldForbidden, normalizeFieldName(name))
		}
	}
	whitelistJSON, err := model.EncodeStringList(whitelist)
	if err != nil {
		return nil, fmt.Errorf("event-collector: field_whitelist 编码失败: %w", err)
	}
	p.FieldWhitelistJSON = whitelistJSON

	dropFields, err := clampListItems("drop_fields", in.DropFields)
	if err != nil {
		return nil, err
	}
	dropJSON, err := model.EncodeStringList(dropFields)
	if err != nil {
		return nil, fmt.Errorf("event-collector: drop_fields 编码失败: %w", err)
	}
	p.DropFieldsJSON = dropJSON

	// --- 数值区间 ---
	if err := checkPolicyRanges(p, col, c.Dispatch); err != nil {
		return nil, err
	}
	return p, nil
}

// checkPolicyRanges 校验策略数值区间（含跨字段一致性）。
func checkPolicyRanges(p *model.DispatchPolicy, col config.CollectorConf, dsp config.DispatchConf) error {
	switch {
	case p.MaxEventsPerBatch <= 0 || p.MaxEventsPerBatch > hardMaxEventsPerBatch:
		return fmt.Errorf("event-collector: max_events_per_batch=%d 必须落在 (0,%d]", p.MaxEventsPerBatch,
			hardMaxEventsPerBatch)
	case p.MaxEventsPerBatch > col.MaxEventsPerBatch:
		return fmt.Errorf("event-collector: max_events_per_batch=%d 超过服务端上限 %d，"+
			"策略只能收紧不能放宽（读路径会按 config 封顶，写进来就成了库里的数与生效的数不一致）",
			p.MaxEventsPerBatch, col.MaxEventsPerBatch)
	}
	switch {
	case p.MaxRequestBytes <= 0 || p.MaxRequestBytes > hardMaxRequestBytes:
		return fmt.Errorf("event-collector: max_request_bytes=%d 必须落在 (0,%d]", p.MaxRequestBytes,
			hardMaxRequestBytes)
	case p.MaxRequestBytes > col.MaxRequestBytes:
		return fmt.Errorf("event-collector: max_request_bytes=%d 超过服务端上限 %d", p.MaxRequestBytes,
			col.MaxRequestBytes)
	}
	switch {
	case p.MaxEventPayloadBytes < hardMinPayloadBytes:
		return fmt.Errorf("event-collector: max_event_payload_bytes=%d 不能小于 %d，"+
			"否则正常事件也会被整批拒成 REJECT_PAYLOAD_TOO_LARGE", p.MaxEventPayloadBytes, hardMinPayloadBytes)
	case int64(p.MaxEventPayloadBytes) > p.MaxRequestBytes:
		return fmt.Errorf("event-collector: max_event_payload_bytes=%d 不能超过 max_request_bytes=%d",
			p.MaxEventPayloadBytes, p.MaxRequestBytes)
	case p.MaxEventPayloadBytes > col.MaxEventPayloadBytes:
		return fmt.Errorf("event-collector: max_event_payload_bytes=%d 超过服务端上限 %d",
			p.MaxEventPayloadBytes, col.MaxEventPayloadBytes)
	}
	if p.MaxClockSkewSeconds <= 0 {
		return fmt.Errorf("event-collector: max_clock_skew_seconds=%d 必须 > 0", p.MaxClockSkewSeconds)
	}
	if p.MaxBackfillSeconds <= p.MaxClockSkewSeconds {
		return fmt.Errorf("event-collector: max_backfill_seconds=%d 必须大于 max_clock_skew_seconds=%d，"+
			"否则离线回补永远被拒", p.MaxBackfillSeconds, p.MaxClockSkewSeconds)
	}
	if p.MaxBackfillSeconds > col.MaxBackfillSeconds {
		return fmt.Errorf("event-collector: max_backfill_seconds=%d 超过服务端上限 %d", p.MaxBackfillSeconds,
			col.MaxBackfillSeconds)
	}
	if p.KeywordMaxRunes <= 0 || p.KeywordMaxRunes > hardMaxKeywordRunes {
		return fmt.Errorf("event-collector: keyword_max_runes=%d 必须落在 (0,%d]", p.KeywordMaxRunes,
			hardMaxKeywordRunes)
	}
	if p.RetentionDays <= 0 || p.RetentionDays > hardMaxRetentionDays {
		return fmt.Errorf("event-collector: retention_days=%d 必须落在 (0,%d]：无上限等于台账无限增长",
			p.RetentionDays, hardMaxRetentionDays)
	}
	if p.DeliverMaxAttempts <= 0 || p.DeliverMaxAttempts > hardMaxDeliverAttempts {
		return fmt.Errorf("event-collector: deliver_max_attempts=%d 必须落在 (0,%d]：0 会让失败事件直接变死信",
			p.DeliverMaxAttempts, hardMaxDeliverAttempts)
	}
	if p.RetryBaseSeconds <= 0 || p.RetryBaseSeconds > hardMaxRetryBaseSecs {
		return fmt.Errorf("event-collector: retry_base_seconds=%d 必须落在 (0,%d]", p.RetryBaseSeconds,
			hardMaxRetryBaseSecs)
	}
	if p.RetryMaxSeconds < p.RetryBaseSeconds {
		return fmt.Errorf("event-collector: retry_max_seconds=%d 不能小于 retry_base_seconds=%d",
			p.RetryMaxSeconds, p.RetryBaseSeconds)
	}
	if p.RetryMaxSeconds > dsp.RetryMaxSeconds {
		return fmt.Errorf("event-collector: retry_max_seconds=%d 超过服务端上限 %d，"+
			"退避超过该值的事件会在 Outbox 里积压到人工介入", p.RetryMaxSeconds, dsp.RetryMaxSeconds)
	}
	return nil
}

// encodeSampleRulesForPolicy 校验并编码采样规则。
func encodeSampleRulesForPolicy(in []*rpc.SampleRule) (string, error) {
	if len(in) > maxPolicyListItems {
		return "", fmt.Errorf("%w: sample_rules 条目数 %d 超过上限 %d", model.ErrBatchLimitTooLarge,
			len(in), maxPolicyListItems)
	}
	rules := make([]model.SampleRule, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, r := range in {
		if r == nil {
			return "", fmt.Errorf("event-collector: sample_rules 含空条目")
		}
		et := strings.TrimSpace(r.EventType)
		if et == "" {
			return "", fmt.Errorf("event-collector: sample_rules.event_type 必填（或用 %q 表示兜底规则）",
				model.SampleWildcard)
		}
		if et != model.SampleWildcard {
			if !validEventTypeString(et) {
				return "", fmt.Errorf("event-collector: sample_rules.event_type %q 形状非法", et)
			}
			if _, ok := knownEventTypes[et]; !ok {
				return "", fmt.Errorf("event-collector: sample_rules.event_type %q 不在已登记的事件类型白名单里"+
					"（未知类型永远匹配不上，只会静默走兜底规则）", et)
			}
		}
		if r.SampleBps < 0 || r.SampleBps > model.SampleBase {
			return "", fmt.Errorf("event-collector: sample_rules[%s].sample_bps=%d 必须落在 [0,%d]（基点）",
				et, r.SampleBps, model.SampleBase)
		}
		key := et + "|" + boolTag(r.QualityEvents)
		if _, dup := seen[key]; dup {
			return "", fmt.Errorf("event-collector: sample_rules 出现重复规则 event_type=%s quality_events=%s，"+
				"两条规则同时命中时采样比例不可判定", et, boolTag(r.QualityEvents))
		}
		seen[key] = struct{}{}
		rules = append(rules, model.SampleRule{EventType: et, SampleBps: r.SampleBps, QualityEvents: r.QualityEvents})
	}
	if !model.ValidateSampleRules(rules) {
		return "", fmt.Errorf("event-collector: 采样规则自检失败：event_type 非空、sample_bps 落在 [0,%d]、"+
			"兜底规则 %q 至多一条且不得带 quality_events 标记", model.SampleBase, model.SampleWildcard)
	}
	raw, err := model.EncodeSampleRules(rules)
	if err != nil {
		return "", fmt.Errorf("event-collector: sample_rules 编码失败: %w", err)
	}
	return raw, nil
}

// ensureSaltUsable 确认这份策略声明的盐真的取不到。
//
// 复用采集路径的同一个 resolveSalt：判定口径必须与真正写台账时一致，
// 否则会出现「激活时检查通过、第一条上报就 ErrSaltMissing」的错位。
// 激活是最后一道可以失败的位置——一旦生效，采集侧就会拿它给设备号打哈希。
func ensureSaltUsable(s *svc.ServiceContext, p *model.DispatchPolicy) error {
	if p == nil {
		return model.ErrPolicyNotFound
	}
	salt, _, err := resolveSalt(s, limits{saltRef: p.SaltRef, saltVersion: p.SaltVersion})
	if err != nil {
		return err
	}
	if strings.TrimSpace(salt) == "" {
		return model.ErrSaltMissing
	}
	return nil
}

// inherit32 / inherit64：0 表示继承 config 默认值（proto3 没有字段存在性）。
func inherit32(v, def int32) int32 {
	if v == 0 {
		return def
	}
	return v
}

func inherit64(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

func boolTag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
