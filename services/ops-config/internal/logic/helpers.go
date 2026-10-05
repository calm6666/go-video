// 本文件是 logic 包的手写扩展（入参校验、限额归一、缓存投影与审计存证），
// 不是 goctl 生成产物。
//
// 分工（AGENTS.md §4）：这里只放「不碰 SQL」的可测函数——校验、归一、限额夹取、
// 缓存键构造、审计摘要。SQL 一律留在 model（§5）。

package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/logx"

	auditrpc "go-video/services/audit/rpc"
	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"
)

// 服务端默认值：配置项留空/非法时回落到这些数字，与 etc/opsconfig.v1.yaml 的注释一致。
// 之所以在代码里也放一份：配置来自文件，文件可以被删字段；
// 没有兜底就会变成「ttl=0 ⇒ 全部不缓存」或「maxBatch=0 ⇒ 一次都不让读」这种隐蔽故障。
const (
	defValueMaxBytes      = 8192
	defReasonMaxChars     = 500
	defOperatorNameMax    = 64
	defTopicMaxItems      = 500
	defTopicMaxRefIDs     = 64
	defTopicItemLimit     = 100
	defTopicTTLSeconds    = 60
	defSlotMaxCapacity    = 200
	defSlotMaxItems       = 200
	defSlotTTLSeconds     = 30
	defWhitelistMax       = 200
	defRulesPerConfig     = 50
	defBatchKeys          = 50
	defResolveTTLSeconds  = 60
	defMaxTTLSeconds      = 3600
	defPageSize           = 100
	defaultKeyPrefix      = "govideo:opsconfig"
	actionDomainOpsConfig = "ops_config"
)

// itemPointerTTLSeconds 是「配置项主记录」投影的 TTL 上限。
// 它刻意远小于建议给调用方的 ttl：指针一旧，latest_version/epoch 就都可能旧，
// 而值快照与灰度规则投影都可以放心缓存更久（前者不可变、后者按 epoch 换代）。
// 因此本服务只在这一个键上承担「最长 5 秒收敛」的窗口，其余靠 DEL 即时失效。
const itemPointerTTLSeconds = 5

// limits 是把配置与代码硬上限调和后的生效限额。
// 每个字段都满足「配置只能在硬上限内收紧」，因此调用方不必再判一次 model 常量。
type limits struct {
	valueMaxBytes   int
	reasonMaxChars  int
	operatorNameMax int
	maxPageSize     int
	batchKeys       int
	resolveTTL      int
	topicTTL        int
	topicMaxItems   int
	topicMaxRefIDs  int
	topicItemLimit  int
	slotTTL         int
	slotMaxCapacity int
	slotMaxItems    int
	whitelistMax    int
	rulesPerConfig  int
	maxTTL          int
	keyPrefix       string
	deleteOnBump    bool
	countTotal      bool
}

// totalOf 按 Query.CountTotal 决定是否把 model 侧的 COUNT 回给调用方。
// 关掉时回 0 表示「本接口不提供总数」，README 里写明此时 total 不可信。
func (l limits) totalOf(total int64) int64 {
	if !l.countTotal {
		return 0
	}
	return total
}

func newLimits(c config.Config) limits {
	l := limits{
		valueMaxBytes:   clampedPositive(c.OpsValue.MaxBytes, defValueMaxBytes, model.MaxCfgValueBytes),
		reasonMaxChars:  clampedPositive(c.OpsValue.MaxReasonLen, defReasonMaxChars, model.MaxReasonChars),
		operatorNameMax: clampedPositive(c.OpsValue.MaxOperatorNameLen, defOperatorNameMax, model.MaxOperatorNameChars),
		maxPageSize:     clampedPositive(c.Query.MaxPageSize, defPageSize, model.MaxPageSizeHard),
		batchKeys:       clampedPositive(c.Resolve.MaxBatchKeys, defBatchKeys, model.MaxResolveKeys),
		resolveTTL:      clampedPositive(c.Resolve.DefaultTTLSeconds, defResolveTTLSeconds, model.MaxTTLSecondsHard),
		topicTTL:        clampedPositive(c.OpsTopic.ListTTLSeconds, defTopicTTLSeconds, model.MaxTTLSecondsHard),
		topicMaxItems:   clampedPositive(c.OpsTopic.MaxItems, defTopicMaxItems, model.MaxTopicItems),
		topicMaxRefIDs:  clampedPositive(c.OpsTopic.MaxRefIDs, defTopicMaxRefIDs, model.MaxTopicRefIDs),
		topicItemLimit:  clampedPositive(c.OpsTopic.DefaultItemLimit, defTopicItemLimit, model.MaxTopicItems),
		slotTTL:         clampedPositive(c.OpsSlot.DefaultTTLSeconds, defSlotTTLSeconds, model.MaxTTLSecondsHard),
		slotMaxCapacity: clampedPositive(c.OpsSlot.MaxCapacity, defSlotMaxCapacity, model.MaxSlotCapacityHard),
		slotMaxItems:    clampedPositive(c.OpsSlot.MaxItems, defSlotMaxItems, model.MaxSlotCapacityHard),
		whitelistMax:    clampedPositive(c.Rollout.MaxWhitelistMids, defWhitelistMax, model.MaxWhitelistMids),
		rulesPerConfig:  clampedPositive(c.Rollout.MaxRulesPerConfig, defRulesPerConfig, model.MaxRolloutCandidates),
		maxTTL:          clampedPositive(c.Cache.MaxTTLSeconds, defMaxTTLSeconds, model.MaxTTLSecondsHard),
		keyPrefix:       keyPrefixOr(c.Cache.KeyPrefix),
		deleteOnBump:    c.Cache.DeleteOnBump,
		countTotal:      c.Query.CountTotal,
	}
	return l
}

// keyPrefixOr 拒绝空前缀：没有前缀的多环境共用 Redis 会让一次 RefreshCache
// 删到别人（甚至同前缀的其它域）的键，违反 AGENTS.md §5 的 key 空间边界。
func keyPrefixOr(prefix string) string {
	p := strings.TrimSuffix(strings.TrimSpace(prefix), ":")
	if p == "" {
		return defaultKeyPrefix
	}
	return p
}

// clampedPositive 把配置值夹进 [1, hard]；<=0 用默认值，默认值本身也被 hard 兜住。
func clampedPositive(v, def, hard int) int {
	if v <= 0 {
		v = def
	}
	if v <= 0 {
		return hard
	}
	if hard > 0 && v > hard {
		return hard
	}
	return v
}

// clampTTL 把建议 TTL 夹进 [0, Cache.MaxTTLSeconds]。
// 上限存在的理由见 CacheConf.MaxTTLSeconds 的注释：一次错误放量最长要等一个 TTL 才被踢掉。
func (l limits) clampTTL(seconds int) int32 {
	if seconds <= 0 {
		return 0
	}
	if seconds > l.maxTTL {
		return int32(l.maxTTL)
	}
	return int32(seconds)
}

// --- CallContext 与通用入参校验 ---

// checkWriteContext 校验写接口的身份三件套：request_id（幂等键）、operator_id（归因）。
// 读接口不调用它：ResolveConfig 这类热路径要求 request_id 只会让调用方造假数据。
func checkWriteContext(in *rpc.CallContext, l limits) error {
	if in == nil {
		return model.ErrRequestIDRequired
	}
	if strings.TrimSpace(in.GetRequestId()) == "" {
		return model.ErrRequestIDRequired
	}
	// request_id 是 ops_config_version.uniq_request_id 的列（VARCHAR(64)）。
	// 超长必须在入口拒：交给 MySQL 只会得到 1406，而且幂等回放还会因此静默失效。
	if utf8.RuneCountInString(in.GetRequestId()) > model.MaxRequestIDChars {
		return model.ErrRequestIDTooLong
	}
	if in.GetOperatorId() <= 0 {
		return model.ErrOperatorRequired
	}
	if len(in.GetOperatorName()) > l.operatorNameMax {
		return model.ErrOperatorNameTooLong
	}
	return nil
}

// checkReason 校验变更原因：影响线上展示的动作必须能回答「为什么」。
// 超长是拒绝而不是截断——理由会进审计摘要，截断让摘要与库值不一致。
func checkReason(reason string, l limits) error {
	if strings.TrimSpace(reason) == "" {
		return model.ErrReasonRequired
	}
	if len([]rune(reason)) > l.reasonMaxChars {
		return model.ErrReasonTooLong
	}
	return nil
}

// checkCfgKey 校验配置键。scope 为空时归一为 global（契约里明确写了这个默认值）。
func checkCfgKey(key string, l limits) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return model.ErrConfigKeyRequired
	}
	if !model.ValidCfgKey(key) {
		return model.ErrConfigKeyInvalid
	}
	if l.valueMaxBytes > 0 && len(key) > 64 {
		return model.ErrConfigKeyInvalid
	}
	return nil
}

// normalizeScope 空 → global；其它原样返回（大小写敏感：库里的 scope 就是这几个小写词）。
func normalizeScope(scope string) string {
	s := strings.TrimSpace(scope)
	if s == "" {
		return model.ScopeGlobal
	}
	return s
}

func checkScope(scope string) error {
	if !model.ValidScope(scope) {
		return model.ErrScopeUnknown
	}
	return nil
}

// checkState 校验通用启停：0 由调用方先按「默认停用/默认生效」归一再判。
func checkState(v int32) error {
	if !model.ValidState(v) {
		return fmt.Errorf("%w: state=%d", model.ErrRuleStateInvalid, v)
	}
	return nil
}

// checkPlatformEnum 校验单个端标识（客户端开关用）。
// 0 → ErrPlatformRequired：开关必须按端定义；越界 → ErrPlatformUnknown：
// 本项目只有 Android/iOS/HarmonyOS/桌面四端，**没有小程序**（AGENTS.md §1、§6），
// 因此第五个取值绝不能被当成「不限端」放过——那会把一个不存在的端的配置推给四端。
func checkPlatformEnum(p rpc.ClientPlatform) error {
	return checkPlatformInt(int32(p), true)
}

func checkPlatformInt(p int32, required bool) error {
	if p == 0 {
		if required {
			return model.ErrPlatformRequired
		}
		return nil
	}
	if !model.ValidPlatform(p) {
		return model.ErrPlatformUnknown
	}
	return nil
}

// checkPlatformSliceEnum 校验可重复端字段（灰度规则、坑位）。空数组合法（不限端）。
func checkPlatformSliceEnum(in []rpc.ClientPlatform) error {
	for _, p := range in {
		if err := checkPlatformEnum(p); err != nil {
			// 空数组才是「不限端」，数组里出现 0 是调用方漏填，不是「不限」。
			if errors.Is(err, model.ErrPlatformRequired) {
				return model.ErrPlatformUnknown
			}
			return err
		}
	}
	return nil
}

// validateValueText 按声明的值类型校验待发布的值。
// 目的：把脏值挡在写入侧，而不是让四端各自的解析器去猜。
func validateValueText(valueType int32, value string, l limits) error {
	if !model.ValidValueType(valueType) {
		return model.ErrValueTypeUnsupported
	}
	if len(value) > l.valueMaxBytes {
		return model.ErrValueTooLong
	}
	switch valueType {
	case model.ValueTypeString:
		return nil
	case model.ValueTypeInt:
		if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
			return model.ErrValueInvalid
		}
		return nil
	case model.ValueTypeBool:
		switch strings.TrimSpace(value) {
		case "true", "false":
			return nil
		default:
			return model.ErrValueInvalid
		}
	case model.ValueTypeJSON:
		if !json.Valid([]byte(value)) {
			return model.ErrValueInvalid
		}
		return nil
	default:
		return model.ErrValueTypeUnsupported
	}
}

// --- 灰度输入与规则构造 ---

// rolloutInputOf 把契约 TargetContext 转成 model 的灰度输入。
// platform 越界直接拒绝（同上：不能把未知端当成「不做端过滤」放过，
// 那会让一条只针对某端的规则命中所有端）。
func rolloutInputOf(t *rpc.TargetContext) (model.RolloutInput, error) {
	in := model.RolloutInput{}
	if t == nil {
		return in, nil
	}
	if err := checkPlatformInt(int32(t.GetPlatform()), false); err != nil {
		return in, err
	}
	in.Platform = int32(t.GetPlatform())
	in.AppVersion = strings.TrimSpace(t.GetAppVersion())
	in.Mid = t.GetMid()
	in.IgnoreRollout = t.GetIgnoreRollout()
	return in, nil
}

// ruleFromSpec 把契约里的规则声明转成 model 行：先做存储形态归一化，
// 形态自洽性（mode 与已填维度、区间不得倒挂等）交给 model.ValidateRuleShape，
// 写侧与读侧因此共用同一套判定（model/rollout.go 是唯一实现）。
func ruleFromSpec(spec *rpc.RolloutRuleSpec, configID, version, operatorID int64, l limits) (*model.RolloutRule, error) {
	if spec == nil {
		return nil, model.ErrRuleNameRequired
	}
	name := strings.TrimSpace(spec.GetName())
	if name == "" {
		return nil, model.ErrRuleNameRequired
	}
	if err := checkPlatformSliceEnum(spec.GetPlatforms()); err != nil {
		return nil, err
	}
	platforms, err := model.NormalizePlatformList(platformInt32List(spec.GetPlatforms()))
	if err != nil {
		return nil, err
	}
	suffixes, err := model.NormalizeMidSuffixes(spec.GetMidSuffixes())
	if err != nil {
		return nil, err
	}
	wl, err := model.ValidateIDList(spec.GetWhitelistMids(), l.whitelistMax)
	if err != nil {
		if errors.Is(err, model.ErrBatchTooLarge) {
			return nil, model.ErrWhitelistTooLarge
		}
		return nil, err
	}
	return &model.RolloutRule{
		ConfigID:      configID,
		Version:       version,
		Name:          name,
		Mode:          int32(spec.GetMode()),
		Percentage:    spec.GetPercentage(),
		AppVersionMin: strings.TrimSpace(spec.GetAppVersionMin()),
		AppVersionMax: strings.TrimSpace(spec.GetAppVersionMax()),
		Platforms:     platforms,
		MidSuffixes:   suffixes,
		WhitelistMids: model.IDListString(wl),
		Priority:      spec.GetPriority(),
		OperatorID:    operatorID,
		Remark:        spec.GetRemark(),
		StartAt:       spec.GetStartAt(),
		EndAt:         spec.GetEndAt(),
	}, nil
}

// --- 缓存投影（键只落在本服务自己的前缀下，见 AGENTS.md §5）---

func (l limits) itemKey(scope, cfgKey string) string {
	return l.keyPrefix + ":item:" + scope + ":" + cfgKey
}

// versionKey 指向一条**不可变**快照，因此键里没有 epoch：
// 版本号本身就决定了内容，永远不需要失效，只需要 TTL 回收。
func (l limits) versionKey(configID, version int64) string {
	return l.keyPrefix + ":ver:" + strconv.FormatInt(configID, 10) + ":" + strconv.FormatInt(version, 10)
}

func (l limits) topicKey(locator string) string {
	return l.keyPrefix + ":topic:" + locator
}

func (l limits) slotKey(locator string) string {
	return l.keyPrefix + ":slot:" + locator
}

// topicKeyLocators / slotKeyLocators 回一份专题（坑位）的全部读投影键。
// 端上按 slug/code 寻址、后台按 ID 操作，两份键都必须一起删，
// 否则「后台改了、端上还是旧的」是最难复现的一类投诉。
func (l limits) topicKeyLocators(t *model.Topic) []string {
	if t == nil {
		return nil
	}
	keys := []string{l.topicKey("id:" + strconv.FormatInt(t.TopicID, 10))}
	if t.Slug != "" {
		keys = append(keys, l.topicKey("slug:"+t.Slug))
	}
	return keys
}

func (l limits) slotKeys(s *model.RecommendSlot) []string {
	if s == nil {
		return nil
	}
	keys := []string{l.slotKey("id:" + strconv.FormatInt(s.SlotID, 10))}
	if s.Code != "" {
		keys = append(keys, l.slotKey("code:"+s.Code))
	}
	return keys
}

// configKeysOf 一个配置项当前的读投影键。
//
// 刻意**不缓存灰度规则**：规则带 start_at/end_at 时间窗，缓存会让「定时开闸」延后一个
// TTL 才生效，那是运营完全无法解释的现象；一次带 idx_config_state 的索引查询足够便宜。
// 值快照（:ver:）不可变、无需失效；项指针（:item:）用极短 TTL + 发布时显式删除。
func (l limits) configKeysOf(item *model.ConfigItem) []string {
	if item == nil {
		return nil
	}
	return []string{l.itemKey(item.Scope, item.CfgKey)}
}

// cacheLoad 读一个 JSON 投影；miss 时回源并回填。
// 语义边界（internal/svc 的 CacheKV 注释、model/doc.go）：Redis 只是可整域重建的投影，
// 因此**读失败与写失败都只记日志**，判定结果一律以 MySQL 为准；
// 这里不返回缓存错误，避免一次 Redis 抖动把运行时解析变成 gRPC 失败。
func cacheLoad[T any](ctx context.Context, cache svc.CacheKV, key string, ttl int, logger logx.Logger, load func() (T, error)) (T, error) {
	var zero T
	if cache == nil || ttl <= 0 {
		return load()
	}
	raw, err := cache.Get(ctx, key)
	if err != nil {
		logger.Errorf("ops-config/cache: 读 %s 失败，按未命中回源: %v", key, err)
	} else if raw != "" {
		var v T
		if uerr := json.Unmarshal([]byte(raw), &v); uerr == nil {
			return v, nil
		} else {
			logger.Errorf("ops-config/cache: 解析 %s 失败，按未命中回源: %v", key, uerr)
		}
	}
	v, lerr := load()
	if lerr != nil {
		return zero, lerr
	}
	bs, merr := json.Marshal(v)
	if merr != nil {
		return v, nil
	}
	if serr := cache.Setex(ctx, key, string(bs), ttl); serr != nil {
		logger.Errorf("ops-config/cache: 回填 %s 失败（本次响应仍正确）: %v", key, serr)
	}
	return v, nil
}

// cacheDel 删除投影键并回实际删除条数。失败只记日志：投影没删干净 = 最多等一个 TTL，
// 而 epoch 已经换代，下一次解析必然读到新代次的键。
func cacheDel(ctx context.Context, cache svc.CacheKV, keys []string, logger logx.Logger) int64 {
	if cache == nil || len(keys) == 0 {
		return 0
	}
	n, err := cache.Del(ctx, keys...)
	if err != nil {
		logger.Errorf("ops-config/cache: 删除 %d 个投影键失败（epoch 已换代，最多影响一个 TTL 内的读取）: %v", len(keys), err)
		return 0
	}
	return n
}

// --- 审计存证（本服务是自身配置动作的发起者，见 rpc/opsconfig.proto 文件头）---

// auditEventID 固定形态 "ops-config:<request_id>:<action>"：
// 同一次请求重放不会产生第二条审计条目（audit 侧按 event_id 幂等）。
func auditEventID(requestID, action string) string {
	return "ops-config:" + requestID + ":" + action
}

// appendAudit 写一条审计并回 entry_id。
// 返回 0 的两种情况都不阻塞业务：未配 AuditRPC、或调用失败——两者都打 Error 日志，
// 让「审计缺口」在日志里可见，而不是被静默吞掉，也绝不伪造一个 entry_id。
func appendAudit(ctx context.Context, client auditrpc.AuditClient, logger logx.Logger,
	in *rpc.CallContext, action, targetType, targetID, before, after, reason string, occurredAt int64) int64 {
	if client == nil {
		logger.Errorf("ops-config/audit: AuditRPC 未配置，动作 %s 的存证待补偿（event_id=%s）",
			action, auditEventID(callRequestID(in), action))
		return 0
	}
	req := &auditrpc.AppendAuditReq{
		Ctx: &auditrpc.CallContext{
			CallerService: callerService(in),
			OperatorId:    in.GetOperatorId(),
			TraceId:       in.GetTraceId(),
			RequestId:     in.GetRequestId(),
			Ip:            in.GetIp(),
		},
		Entry: &auditrpc.AuditEntryDraft{
			EventId:       auditEventID(in.GetRequestId(), action),
			SchemaVersion: 1,
			ActorType:     auditrpc.ActorType_ACTOR_TYPE_ADMIN,
			ActorId:       in.GetOperatorId(),
			ActorName:     in.GetOperatorName(),
			Action:        "ops_config." + action,
			ActionDomain:  actionDomainOpsConfig,
			TargetType:    targetType,
			TargetId:      targetID,
			Result:        auditrpc.AuditResult_AUDIT_RESULT_OK,
			BeforeDigest:  before,
			AfterDigest:   after,
			Reason:        reason,
			SourceApp:     sourceApp(in),
			Ip:            in.GetIp(),
			OccurredAt:    occurredAt,
		},
	}
	reply, err := client.AppendAudit(ctx, req)
	if err != nil {
		logger.Errorf("ops-config/audit: 写入失败，动作 %s 待补偿（event_id=%s）: %v",
			action, req.Entry.EventId, err)
		return 0
	}
	if reply.GetEntry() == nil || reply.GetEntry().GetEntryId() <= 0 {
		logger.Errorf("ops-config/audit: 返回里没有 entry_id，动作 %s 待补偿（event_id=%s）",
			action, req.Entry.EventId)
		return 0
	}
	return reply.GetEntry().GetEntryId()
}

func callRequestID(in *rpc.CallContext) string { return in.GetRequestId() }

func callerService(in *rpc.CallContext) string {
	if s := strings.TrimSpace(in.GetCallerService()); s != "" {
		return s
	}
	return "ops-config"
}

// sourceApp 只决定审计的「来源端」维度，不驱动任何响应差异。
// 后台写接口没有 TargetContext，按 caller_service 兜底成后台/内部/定时进程。
func sourceApp(in *rpc.CallContext) auditrpc.SourceApp {
	switch strings.ToLower(callerService(in)) {
	case "cron", "services/cron":
		return auditrpc.SourceApp_SOURCE_APP_CRON
	case "gateway/admin", "admin":
		return auditrpc.SourceApp_SOURCE_APP_ADMIN_WEB
	case "ops-config":
		return auditrpc.SourceApp_SOURCE_APP_INTERNAL_RPC
	default:
		return auditrpc.SourceApp_SOURCE_APP_ADMIN_WEB
	}
}

// --- 分页 ---

// pageOf 归一化分页；负数页码是入参错误（不是「回到第一页」），必须显式拒绝，
// 否则调用方拼错 pn 时会以为自己翻页成功。
func pageOf(pn, ps int32, maxPs int) (int32, int32, error) {
	if pn < 0 || ps < 0 {
		return 0, 0, model.ErrInvalidPage
	}
	a, b := model.PageOrDefault(pn, ps, maxPs)
	return a, b, nil
}

// checkKeywordLen 限制模糊词长度：关键词只进 LIKE，超长多半是把整段文本塞进了搜索框。
func checkKeywordLen(kw string) (string, error) {
	kw = strings.TrimSpace(kw)
	if len([]rune(kw)) > model.MaxTopicKeywordChars {
		return "", model.ErrBatchTooLarge
	}
	return kw, nil
}
