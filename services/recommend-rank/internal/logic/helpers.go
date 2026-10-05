// 本文件是 logic 包的手写扩展（列宽与入参校验、model/rpc 枚举映射、权重与 overrides 解析、
// 降级状态汇总、配置指纹与分桶纯计算），不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：这里只放「不碰 SQL」的可测函数——校验、归一、纯计算与投影口径。
// SQL、CAS 与事务边界一律留在 model / internal/repository。
package logic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"go-video/services/recommend-rank/internal/repository"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"
)

// 列宽约束（与 deploy/migrations/recommend-rank/0000NN 的 DDL 一致）。
// 校验发生在 logic：model 只保证「不合法就别写」，logic 保证「别把不合法的请求带进事务」。
const (
	// colIdent 对应 model_key / version / feature_config_version / config_version /
	// exp_key / variant_key / layer_key / hash_seed / operator / request_id /
	// decision_id / idempotency_key / trace_id / snapshot_id / scene / subject_id 的 VARCHAR(64)。
	colIdent = 64
	// colNote 对应三张配置表 note 列的 VARCHAR(255)。
	colNote = 255
	// colWeights 对应 rank_model_version.objective_weights 的 VARCHAR(1024)。
	colWeights = 1024
	// colMetrics 对应 rank_model_version.offline_metrics 的 VARCHAR(2048)。
	colMetrics = 2048
	// colArtifactRef 对应 rank_model_version.artifact_ref 的 VARCHAR(512)。
	colArtifactRef = 512
	// colAppVersion 对应 rank_decision_log.app_version 的 VARCHAR(32)。
	colAppVersion = 32
	// colRegion 对应 rank_decision_log.region 的 VARCHAR(16)。
	colRegion = 16
	// colReasonKey 对应 degrade_reason / fallback_strategy 的 VARCHAR(32)。
	colReasonKey = 32
	// colDetail 对应 rank_decision_log.degrade_detail 的 VARCHAR(1024)。
	colDetail = 1024
	// colSourceSummary 对应 rank_decision_log.source_summary 的 VARCHAR(255)。
	colSourceSummary = 255
	// sha256HexLen 是 sha256 小写 hex 长度（device_id_hash 的期望形态）。
	sha256HexLen = 64
)

// 在线排序的行为上界（契约里没有配置项、但必须有界的地方）。
const (
	// maxEchoBucketCount 是 GetExperimentAssignment 允许的自定义桶空间上界。
	// 桶号回显换算只做线性缩放，给到百万分位已远超任何真实分流口径；
	// 再大就是入参攻击（ScaleBucket 的乘法会溢出）。
	maxEchoBucketCount = 1_000_000
	// maxDecisionOffset 是 ListRankDecisions 允许的 offset 上界：
	// 深翻页在这张增长最快的表上是 OFFSET 扫行，审计读应当改用时间窗而不是无限翻页。
	maxDecisionOffset = 20000
	// maxDetailParts 限制 degrade_detail 拼接的片段数，避免异常场景把文本撑爆列。
	maxDetailParts = 8
	// maxFeatureKeyLen 是单个特征 key 的长度上界（特征 key 要拼成 csv 进 TEXT 列）。
	maxFeatureKeyLen = 128
	// maxObjectiveCount 是受控优化目标的数量上界（当前登记 4 个，多目标不会无界增长）。
	maxObjectiveCount = 8
)

// --- 必填与文本校验 ---

// requiredIdent 校验业务主键类文本：去空白、非空、落在列宽内。
// 超长一律拒绝而不是截断入库：截断后的 key 会在审计里指向另一个对象。
func requiredIdent(name, v string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", fmt.Errorf("%w: %s required", model.ErrKeyRequired, name)
	}
	if err := checkIdent(name, t); err != nil {
		return "", err
	}
	return t, nil
}

// optionalIdent 归一可选文本标识（layer_key / feature_store_scene / region 等）。
func optionalIdent(name, v string, max int) (string, error) {
	t := strings.TrimSpace(v)
	if err := checkMaxLen(name, t, max); err != nil {
		return "", err
	}
	return t, nil
}

// checkIdent 校验标识类文本落在 VARCHAR(64) 内且不含空白/控制字符。
// 含空白的 model_key/exp_key 会让 source_summary、缓存 key 与日志的解析口径全部失效。
func checkIdent(name, v string) error {
	if err := checkMaxLen(name, v, colIdent); err != nil {
		return err
	}
	for i := 0; i < len(v); i++ {
		if v[i] <= ' ' || v[i] == 0x7f {
			return fmt.Errorf("%w: %s must not contain blank or control characters", model.ErrKeyRequired, name)
		}
	}
	return nil
}

func checkMaxLen(name, v string, max int) error {
	if len(v) > max {
		return fmt.Errorf("%w: %s %d > %d", model.ErrFieldTooLong, name, len(v), max)
	}
	return nil
}

// requireOperator 校验写接口操作者：没有归因主体的配置变更不可受理（AGENTS.md §4 审计要求）。
func requireOperator(v string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", model.ErrOperatorRequired
	}
	if err := checkMaxLen("operator", t, colIdent); err != nil {
		return "", err
	}
	return t, nil
}

// requireReason 校验写接口变更原因：激活/回滚/换盐都是审计事件，理由必填。
func requireReason(name, v string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", fmt.Errorf("%w: %s", model.ErrReasonRequired, name)
	}
	if err := checkMaxLen(name, t, colNote); err != nil {
		return "", err
	}
	return t, nil
}

// requireIdempotencyKey 校验写接口幂等键。
// 本服务不为写接口另建幂等结果表（契约里没有该表），幂等性由业务唯一键
// （uniq_model_version / uniq_config_version / uniq_variant）与「同语义回 deduplicated」保证；
// 这里的必填校验是为了让调用方无法把「重放」和「第二次变更」混成一个请求。
func requireIdempotencyKey(v string) (string, error) {
	t := strings.TrimSpace(v)
	if t == "" {
		return "", model.ErrIdempotencyKeyRequired
	}
	if err := checkMaxLen("idempotency_key", t, colIdent); err != nil {
		return "", err
	}
	return t, nil
}

// checkDeviceHash 是隐私红线（AGENTS.md §2/§7）：设备维度只接受 sha256 摘要，
// 明文设备号一律拒绝，绝不进表也不进日志。空串合法（表示未知设备）。
func checkDeviceHash(hash string) error {
	h := strings.TrimSpace(hash)
	if h == "" {
		return nil
	}
	if !model.IsSha256Hex(h) {
		return fmt.Errorf("%w: len=%d want=%d hex", model.ErrRawDeviceID, len(h), sha256HexLen)
	}
	return nil
}

// subjectOf 决定这次排序用哪个主体做分桶：
// 登录用户优先（mid 十进制串），其次设备摘要；两者都没有时返回 0/""，
// 表示「本次不参与分桶」（不能凭空造一个主体，否则 control 组会被污染）。
func subjectOf(mid int64, deviceHash string) (int32, string) {
	if mid > 0 {
		return model.SubjectMid, strconv.FormatInt(mid, 10)
	}
	h := strings.TrimSpace(deviceHash)
	if h != "" && model.IsSha256Hex(h) {
		return model.SubjectDevice, strings.ToLower(h)
	}
	return 0, ""
}

// lowerHex 归一摘要类文本为小写 hex：subject_id 落库口径必须唯一，
// 否则同一台设备的两种写法会分成两组（分桶不再稳定）。
func lowerHex(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// --- 枚举映射（model <-> rpc；编号一致性由 contract_consistency_test.go 断言）---

func toRPCModelState(state int32) rpc.ModelVersionState {
	if !model.ValidModelState(state) {
		return rpc.ModelVersionState_MODEL_VERSION_STATE_UNSPECIFIED
	}
	return rpc.ModelVersionState(state)
}

// modelStateFromRPC 校验状态切换目标：只接受 READY/ACTIVE/RETIRED。
// DRAFT 不是可切换目标（登记即 DRAFT，没有「退回草稿」的合法边），UNSPECIFIED 是漏填。
func modelStateFromRPC(state rpc.ModelVersionState) (int32, error) {
	v := int32(state)
	switch v {
	case model.ModelStateReady, model.ModelStateActive, model.ModelStateRetired:
		return v, nil
	default:
		return 0, fmt.Errorf("%w: target_state=%d（只允许 READY/ACTIVE/RETIRED）", model.ErrInvalidModelState, v)
	}
}

// modelStateName 给出状态稳定短名（日志与错误文案用；与 rpc 枚举名同义，便于人工比对）。
func modelStateName(state int32) string {
	switch state {
	case model.ModelStateDraft:
		return "DRAFT"
	case model.ModelStateReady:
		return "READY"
	case model.ModelStateActive:
		return "ACTIVE"
	case model.ModelStateRetired:
		return "RETIRED"
	default:
		return "UNKNOWN"
	}
}

// expStateName 是实验状态的同口径短名。
func expStateName(state int32) string {
	switch state {
	case model.ExpStateDraft:
		return "DRAFT"
	case model.ExpStateRunning:
		return "RUNNING"
	case model.ExpStatePaused:
		return "PAUSED"
	case model.ExpStateStopped:
		return "STOPPED"
	default:
		return "UNKNOWN"
	}
}

func toRPCExpState(state int32) rpc.ExperimentState {
	if !model.ValidExpState(state) {
		return rpc.ExperimentState_EXPERIMENT_STATE_UNSPECIFIED
	}
	return rpc.ExperimentState(state)
}

// expStateFromRPC 校验实验状态切换目标：只接受 RUNNING/PAUSED/STOPPED。
func expStateFromRPC(state rpc.ExperimentState) (int32, error) {
	v := int32(state)
	switch v {
	case model.ExpStateRunning, model.ExpStatePaused, model.ExpStateStopped:
		return v, nil
	default:
		return 0, fmt.Errorf("%w: target_state=%d（只允许 RUNNING/PAUSED/STOPPED）", model.ErrInvalidExpState, v)
	}
}

func toRPCSource(source int32) rpc.RankSource {
	if !model.ValidSource(source) {
		return rpc.RankSource_RANK_SOURCE_UNSPECIFIED
	}
	return rpc.RankSource(source)
}

func toRPCPlatform(platform int32) rpc.Platform {
	if !model.ValidPlatform(platform) {
		return rpc.Platform_PLATFORM_UNSPECIFIED
	}
	return rpc.Platform(platform)
}

func toRPCSubject(subjectType int32) rpc.SubjectType {
	if !model.ValidSubjectType(subjectType) {
		return rpc.SubjectType_SUBJECT_TYPE_UNSPECIFIED
	}
	return rpc.SubjectType(subjectType)
}

// degradeReasonFromKey 把落库的稳定 key 转成 rpc 枚举。
// 未知 key 归 UNSPECIFIED 而不是硬编一个：枚举编号是冻结契约，
// 新增 key 必须先在 model 侧补常量（contract_consistency_test 会挡住漂移）。
func degradeReasonFromKey(key string) rpc.RankDegradeReason {
	if key == model.DegradeReasonNone {
		return rpc.RankDegradeReason_RANK_DEGRADE_REASON_UNSPECIFIED
	}
	val, ok := rpc.RankDegradeReason_value["RANK_DEGRADE_REASON_"+strings.ToUpper(key)]
	if !ok {
		return rpc.RankDegradeReason_RANK_DEGRADE_REASON_UNSPECIFIED
	}
	return rpc.RankDegradeReason(val)
}

// fallbackFromKey 是兜底策略 key -> rpc 枚举的同口径映射。
func fallbackFromKey(key string) rpc.FallbackStrategy {
	if key == model.FallbackNone {
		return rpc.FallbackStrategy_FALLBACK_STRATEGY_UNSPECIFIED
	}
	val, ok := rpc.FallbackStrategy_value["FALLBACK_STRATEGY_"+strings.ToUpper(key)]
	if !ok {
		return rpc.FallbackStrategy_FALLBACK_STRATEGY_UNSPECIFIED
	}
	return rpc.FallbackStrategy(val)
}

// sourceName 给出召回路的稳定短名（reason_code 用，文案由客户端渲染）。
func sourceName(source int32) string {
	switch source {
	case model.SourceHot:
		return "hot"
	case model.SourceFollow:
		return "follow"
	case model.SourceTag:
		return "tag"
	case model.SourceCollab:
		return "collab"
	case model.SourceVector:
		return "vector"
	case model.SourceCold:
		return "cold"
	default:
		return "unknown"
	}
}

// reasonCodeFor 产出 RankedItem.reason_code：稳定 key，不带任何文案与用户数据。
// 降级出参额外带 rk.because.<fallback>，让客户端/日志能区分「模型给的这一位」和
// 「兜底顺序里的这一位」——两者对推荐质量的解释完全不同。
func reasonCodeFor(source int32, fallback string) string {
	if fallback != model.FallbackNone {
		return "rk.because." + fallback
	}
	return "rk.because." + sourceName(source)
}

// --- 摘要与指纹 ---

// sha256Hex 计算小写 hex 摘要（keys_digest / config_revision）。
// 与 model.DigestAids 同一套哈希口径，人工复算只需要一个函数。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// configRevisionOf 计算「当前生效配置」的代次摘要（灰度核对用）。
// 输入必须按固定顺序拼接：任一版本、revision 或 RUNNING 变体集合变化都会改变它。
func configRevisionOf(modelKey, activeVersion string, activeRevision int32,
	featureVersion string, featureRevision int32, variants []*model.RankExperiment, opsRevision string) string {
	var sb strings.Builder
	sb.WriteString("rk.cfg|" + modelKey + "|")
	fmt.Fprintf(&sb, "%s:%d|", activeVersion, activeRevision)
	fmt.Fprintf(&sb, "%s:%d|", featureVersion, featureRevision)
	ordered := make([]string, 0, len(variants))
	for _, v := range variants {
		if v == nil {
			continue
		}
		ordered = append(ordered, fmt.Sprintf("%s/%s:%d:%d-%d", v.ExpKey, v.VariantKey, v.Revision, v.BucketStart, v.BucketEnd))
	}
	sort.Strings(ordered)
	sb.WriteString(strings.Join(ordered, ","))
	sb.WriteString("|ops=" + opsRevision)
	return sha256Hex(sb.String())
}

// --- 多目标权重 ---

// objectiveWeightsFromRPC 校验 rpc 权重并渲染成规范 JSON（落库口径）。
// 约束：目标 key 必须已登记（AGENTS.md §7 禁止广告/付费/会员目标）、不重复、
// 权重非负且有限、总和 <= maxSum（rpc 注释承诺的校验口径）。
func objectiveWeightsFromRPC(in []*rpc.ObjectiveWeight, maxSum float64) (map[string]float64, string, error) {
	if len(in) == 0 {
		return nil, "", fmt.Errorf("%w: objective_weights is empty", model.ErrInvalidWeight)
	}
	if len(in) > maxObjectiveCount {
		return nil, "", fmt.Errorf("%w: %d > %d", model.ErrInvalidWeight, len(in), maxObjectiveCount)
	}
	weights := make(map[string]float64, len(in))
	for _, w := range in {
		if w == nil {
			return nil, "", fmt.Errorf("%w: nil objective weight entry", model.ErrInvalidWeight)
		}
		key := strings.TrimSpace(w.GetObjective())
		if !model.ValidObjective(key) {
			return nil, "", fmt.Errorf("%w: %q（未登记的优化目标）", model.ErrInvalidObjective, key)
		}
		if _, dup := weights[key]; dup {
			return nil, "", fmt.Errorf("%w: %s", model.ErrDuplicateObjective, key)
		}
		val := w.GetWeight()
		if math.IsNaN(val) || math.IsInf(val, 0) || val < 0 {
			return nil, "", fmt.Errorf("%w: %s=%v", model.ErrInvalidWeight, key, val)
		}
		weights[key] = val
	}
	total := 0.0
	for _, v := range weights {
		total += v
	}
	if total > maxSum+1e-9 {
		return nil, "", fmt.Errorf("%w: sum %.4f > %.4f", model.ErrInvalidWeight, total, maxSum)
	}
	raw, err := weightsToJSON(weights)
	if err != nil {
		return nil, "", err
	}
	return weights, raw, nil
}

// weightsToJSON 渲染权重为「按 key 升序」的规范 JSON。
// 规范化的意义：判断「同版本号是否同语义」只能靠字节比较，
// 而 map 遍历序随机，不排序就会把同一个版本判成两次不同的登记。
func weightsToJSON(weights map[string]float64) (string, error) {
	keys := make([]string, 0, len(weights))
	for k := range weights {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return "", fmt.Errorf("recommend-rank: marshal objective key: %w", err)
		}
		sb.Write(keyJSON)
		sb.WriteByte(':')
		sb.WriteString(strconv.FormatFloat(weights[k], 'g', -1, 64))
	}
	sb.WriteByte('}')
	if err := checkMaxLen("objective_weights", sb.String(), colWeights); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// parseObjectiveWeights 解析落库的权重 JSON。
// 空串合法（表示模型未登记目标，排序侧必须据此降级而不是「用默认权重」）；
// 未登记目标、负权重与非法 JSON 一律报错——这是「配置坏了」，不是「没有配置」。
func parseObjectiveWeights(raw string) (map[string]float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil, nil
	}
	var parsed map[string]float64
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, fmt.Errorf("%w: objective_weights is not a JSON number map: %v", model.ErrInvalidWeight, err)
	}
	out := make(map[string]float64, len(parsed))
	for k, v := range parsed {
		key := strings.TrimSpace(k)
		if !model.ValidObjective(key) {
			return nil, fmt.Errorf("%w: %q", model.ErrInvalidObjective, key)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil, fmt.Errorf("%w: %s=%v", model.ErrInvalidWeight, key, v)
		}
		out[key] = v
	}
	return out, nil
}

// sortedObjectives 返回升序目标 key（下发 runtime config 用，顺序必须稳定）。
func sortedObjectives(weights map[string]float64) []string {
	out := make([]string, 0, len(weights))
	for k := range weights {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- 实验参数覆盖（overrides）的值侧校验 ---

// overrideSet 是解析并校验后的实验参数覆盖。
// key 白名单由 model.ValidateOverrideKeys 在写库前把守，本结构补齐取值校验，
// 让「登记」与「打分」用同一份解析结果，不各写一遍 JSON 处理。
// 结构里没有、也不允许有：指定 aid、置顶位、广告/商业化参数（AGENTS.md §7）。
type overrideSet struct {
	// scoreWeights 是对模型登记权重的乘子：0 关闭该目标，1 原样，(1,2] 放大。
	scoreWeights map[string]float64
	// sourceQuota 是各召回路的出参比例，(0,1] 且总和 <= 1。
	sourceQuota map[int32]float64
	// diversityGap / frequencyCap 只登记、本期不生效（需要作者与标签元数据，见 README 已知缺口）。
	diversityGap   int32
	frequencyCap   int32
	coldStartBoost float64
	raw            string
}

func (o *overrideSet) isEmpty() bool {
	return o == nil || (len(o.scoreWeights) == 0 && len(o.sourceQuota) == 0 &&
		o.diversityGap == 0 && o.frequencyCap == 0 && o.coldStartBoost == 0)
}

// effectiveWeights 把覆盖乘子作用到模型登记权重上，得到本次打分真正使用的权重表。
// 只允许改变已登记目标的权重：未登记的目标即使出现在 overrides 里也会被忽略，
// 「实验凭空引入一个新目标」等于绕过模型版本登记。
func (o *overrideSet) effectiveWeights(base map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(base))
	for k, v := range base {
		out[k] = v
	}
	if o == nil {
		return out
	}
	for k, mult := range o.scoreWeights {
		if _, ok := out[k]; !ok {
			continue
		}
		out[k] = out[k] * mult
	}
	return out
}

// parseOverrides 解析并校验 overrides JSON（含字节上限，配置只能收紧 model 的硬上限）。
func parseOverrides(raw string, maxBytes int) (*overrideSet, error) {
	if maxBytes <= 0 || maxBytes > model.MaxOverridesBytes {
		maxBytes = model.MaxOverridesBytes
	}
	trimmed := strings.TrimSpace(raw)
	if err := model.ValidateOverrideKeys(trimmed, maxBytes); err != nil {
		return nil, err
	}
	set := &overrideSet{raw: trimmed, scoreWeights: map[string]float64{}, sourceQuota: map[int32]float64{}}
	if trimmed == "" || trimmed == "{}" {
		return set, nil
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, fmt.Errorf("%w: %v", model.ErrInvalidOverrideKey, err)
	}
	if raw, ok := parsed[model.OverrideScoreWeights]; ok {
		if err := json.Unmarshal(raw, &set.scoreWeights); err != nil {
			return nil, fmt.Errorf("%w: %s must be a number map: %v", model.ErrOverrideValueInvalid, model.OverrideScoreWeights, err)
		}
		for k, v := range set.scoreWeights {
			if !model.ValidObjective(k) {
				return nil, fmt.Errorf("%w: %s 未登记", model.ErrInvalidObjective, k)
			}
			if !finite(v) || v < 0 || v > 2 {
				return nil, fmt.Errorf("%w: %s.%s=%v（乘子需落在 [0,2]）", model.ErrOverrideValueInvalid, model.OverrideScoreWeights, k, v)
			}
		}
	}
	if raw, ok := parsed[model.OverrideSourceQuota]; ok {
		var quota map[string]float64
		if err := json.Unmarshal(raw, &quota); err != nil {
			return nil, fmt.Errorf("%w: %s must be a number map: %v", model.ErrOverrideValueInvalid, model.OverrideSourceQuota, err)
		}
		total := 0.0
		for k, v := range quota {
			source, err := strconv.Atoi(strings.TrimSpace(k))
			if err != nil || !model.ValidSource(int32(source)) {
				return nil, fmt.Errorf("%w: %s.%q 不是已登记的召回路编号", model.ErrOverrideValueInvalid, model.OverrideSourceQuota, k)
			}
			if !finite(v) || v <= 0 || v > 1 {
				return nil, fmt.Errorf("%w: %s.%d=%v（配额需落在 (0,1]）", model.ErrOverrideValueInvalid, model.OverrideSourceQuota, source, v)
			}
			set.sourceQuota[int32(source)] = v
			total += v
		}
		if total > 1+1e-9 {
			return nil, fmt.Errorf("%w: %s 之和 %.4f > 1", model.ErrOverrideValueInvalid, model.OverrideSourceQuota, total)
		}
	}
	if raw, ok := parsed[model.OverrideDiversityGap]; ok {
		gap, err := parseIntValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", model.ErrOverrideValueInvalid, model.OverrideDiversityGap, err)
		}
		if gap < 0 || gap > 100 {
			return nil, fmt.Errorf("%w: %s=%d（允许 [0,100]）", model.ErrOverrideValueInvalid, model.OverrideDiversityGap, gap)
		}
		set.diversityGap = int32(gap)
	}
	if raw, ok := parsed[model.OverrideFrequencyCap]; ok {
		limit, err := parseIntValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", model.ErrOverrideValueInvalid, model.OverrideFrequencyCap, err)
		}
		if limit < 0 || limit > 1000 {
			return nil, fmt.Errorf("%w: %s=%d（允许 [0,1000]）", model.ErrOverrideValueInvalid, model.OverrideFrequencyCap, limit)
		}
		set.frequencyCap = int32(limit)
	}
	if raw, ok := parsed[model.OverrideColdStartBoost]; ok {
		var boost float64
		if err := json.Unmarshal(raw, &boost); err != nil {
			return nil, fmt.Errorf("%w: %s: %v", model.ErrOverrideValueInvalid, model.OverrideColdStartBoost, err)
		}
		if !finite(boost) || boost < 0 || boost > 1 {
			return nil, fmt.Errorf("%w: %s=%v（冷启动加分系数需落在 [0,1]）", model.ErrOverrideValueInvalid, model.OverrideColdStartBoost, boost)
		}
		set.coldStartBoost = boost
	}
	return set, nil
}

// parseIntValue 解析 JSON 整数值（允许 3 与 3.0 两种写法，拒绝小数与字符串）。
func parseIntValue(raw json.RawMessage) (int64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, err
	}
	if !finite(f) || f != math.Trunc(f) {
		return 0, fmt.Errorf("expected integer, got %v", f)
	}
	return int64(f), nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// configuredFallback 归一配置的兜底策略 key。
// 空串或 FallbackNone 时落到 recall_order：这是唯一「不依赖任何下游、只依赖入参顺序」的兜底，
// 因此它也是配置缺省时的安全默认值（而不是「不兜底」）。
// 注意：本函数只用于「本次确实降级」时选择兜底口径，不改变是否降级的判定。
func configuredFallback(key string) string {
	if model.ValidFallback(key) && key != model.FallbackNone {
		return key
	}
	return model.FallbackRecallOrder
}

// --- 降级状态汇总 ---

// degradation 汇总一次排序的降级状态。
//
// 契约的 RankDegradeReason 只能回传一个原因，因此「第一个原因」即回传值，
// 后续原因全部追加进 detail：一个 reason 不许掩盖多个故障（AGENTS.md §9）。
type degradation struct {
	occurred bool
	reason   string
	fallback string
	details  []string
}

// mark 记录一次降级。reason 必须是 model.DegradeReason* 受控 key，
// fallback 传空表示沿用已选定的兜底策略。
func (d *degradation) mark(reason, fallback, detail string) {
	if !model.ValidDegradeReason(reason) {
		// 编程错误：宁可写进 detail 也不落一个无法解释的 reason key。
		d.appendDetail("invalid degrade reason: " + reason)
		return
	}
	if fallback != "" && !model.ValidFallback(fallback) {
		d.appendDetail("invalid fallback: " + fallback)
		fallback = ""
	}
	if !d.occurred || d.reason == model.DegradeReasonNone {
		d.reason = reason
	}
	if fallback != "" && d.fallback == "" {
		d.fallback = fallback
	}
	d.occurred = true
	if detail != "" {
		d.appendDetail(reason + ": " + detail)
	}
}

func (d *degradation) appendDetail(detail string) {
	if len(d.details) >= maxDetailParts {
		return
	}
	d.details = append(d.details, detail)
}

// addDetail 追加一条非故障说明（如运营配置代次、drop_source 明细）。
func (d *degradation) addDetail(detail string) {
	if detail != "" {
		d.appendDetail(detail)
	}
}

// reasonKey 返回落库的 degrade_reason（未降级为空串）。
func (d *degradation) reasonKey() string {
	if d == nil || !d.occurred {
		return model.DegradeReasonNone
	}
	return d.reason
}

// fallbackKey 返回落库的 fallback_strategy（未降级为空串）。
func (d *degradation) fallbackKey() string {
	if d == nil || !d.occurred {
		return model.FallbackNone
	}
	return d.fallback
}

// detailText 渲染 degrade_detail 列（按列宽截断，内容只有 key/计数/版本，无用户数据）。
func (d *degradation) detailText() string {
	if d == nil || len(d.details) == 0 {
		return ""
	}
	return clip(d.details, colDetail)
}

// clip 用 "; " 连接片段并压进列宽；被截断时补 clipped 标记，
// 让读日志的人知道「后面还有」而不是「就这些」。
func clip(parts []string, max int) string {
	joined := strings.Join(parts, "; ")
	if len(joined) <= max {
		return joined
	}
	suffix := ";clipped"
	if max <= len(suffix) {
		return joined[:max]
	}
	return joined[:max-len(suffix)] + suffix
}

// degradeError 把「本次已降级 + 调用方禁止降级」转成错误。
// reason/fallback 都写进错误文案：运维只看 gRPC message 也能判断是哪一环坏了。
func degradeError(d *degradation) error {
	if d == nil || !d.occurred {
		return model.ErrDegradationDisabled
	}
	detail := d.detailText()
	if detail == "" {
		detail = "无明细"
	}
	return fmt.Errorf("%w: reason=%s fallback=%s detail=%s", model.ErrDegradationDisabled,
		d.reasonKey(), configuredFallback(d.fallbackKey()), detail)
}

// fillRow 把降级状态写进决策摘要行的四个列。
// degraded 只落 0/1（model 层会拒绝其它值），且「degraded=1 但 reason 为空」
// 是一种无法解释的事实，因此这里兜底成 store_unavailable 而不是留空。
func (d *degradation) fillRow(row *model.RankDecisionLog) {
	if d == nil || row == nil {
		return
	}
	if !d.occurred {
		row.Degraded = 0
		row.DegradeReason = model.DegradeReasonNone
		row.FallbackStrategy = model.FallbackNone
		row.DegradeDetail = ""
		return
	}
	row.Degraded = 1
	row.DegradeReason = d.reasonKey()
	if !model.ValidDegradeReason(row.DegradeReason) || row.DegradeReason == model.DegradeReasonNone {
		row.DegradeReason = model.DegradeReasonStoreUnavailable
	}
	row.FallbackStrategy = configuredFallback(d.fallbackKey())
	row.DegradeDetail = d.detailText()
}

// info 组装 rpc 降级声明。
func (d *degradation) info(scored, costMs int32) *rpc.DegradationInfo {
	if d == nil {
		return &rpc.DegradationInfo{Degraded: false, ScoredItems: scored, CostMs: costMs}
	}
	return &rpc.DegradationInfo{
		Degraded:    d.occurred,
		Reason:      degradeReasonFromKey(d.reasonKey()),
		Fallback:    fallbackFromKey(d.fallbackKey()),
		ScoredItems: scored,
		CostMs:      costMs,
		Detail:      d.detailText(),
	}
}

// --- 下游接线判定 ---

// isUnwired 判定「这条下游还没接上」：契约轮 stub 返回 model.ErrNotImplemented。
// 配置声明启用却拿到该错误 = 配置与部署不一致，必须显式报错而不是当成「特征为 0」。
func isUnwired(err error) bool { return errors.Is(err, model.ErrNotImplemented) }

// featureSource 返回特征下游；未启用时返回 (nil,nil)，由调用方按显式降级处理。
func featureSource(d repository.Downstream, enabled bool) (repository.FeatureSource, error) {
	if !enabled {
		return nil, nil
	}
	if d.Features == nil {
		return nil, model.ErrFeatureSourceNotConfigured
	}
	return d.Features, nil
}

// behaviorSource 返回 SPM 行为指标下游（只有质量目标，无任何商业化字段）。
func behaviorSource(d repository.Downstream, enabled bool) (repository.BehaviorSource, error) {
	if !enabled {
		return nil, nil
	}
	if d.Behaviors == nil {
		return nil, model.ErrBehaviorSourceNotConfigured
	}
	return d.Behaviors, nil
}

// safetyGate 返回内容安全复核下游。
func safetyGate(d repository.Downstream, enabled bool) (repository.SafetyGate, error) {
	if !enabled {
		return nil, nil
	}
	if d.Safety == nil {
		return nil, model.ErrSafetyGateNotConfigured
	}
	return d.Safety, nil
}

// opsConfigReader 返回运营干预参数下游。
func opsConfigReader(d repository.Downstream, enabled bool) (repository.OpsConfigReader, error) {
	if !enabled {
		return nil, nil
	}
	if d.OpsConfigs == nil {
		return nil, model.ErrOpsConfigNotConfigured
	}
	return d.OpsConfigs, nil
}

// --- 分页与数值归一 ---

// pageArgs 归一审计分页：ps<=0 或 pn<=0 直接报错（不猜默认页大小），
// ps 超过 maxPage 报 ErrPageTooDeep 而不是静默裁剪，offset 超过扫描窗口同样拒绝。
func pageArgs(pn, ps int32, maxPage int32) (offset, limit int, err error) {
	if pn <= 0 || ps <= 0 {
		return 0, 0, fmt.Errorf("%w: pn=%d ps=%d", model.ErrInvalidPage, pn, ps)
	}
	if maxPage <= 0 {
		maxPage = 100
	}
	if ps > maxPage {
		return 0, 0, fmt.Errorf("%w: ps=%d > %d", model.ErrPageTooDeep, ps, maxPage)
	}
	offset = int(pn-1) * int(ps)
	if offset >= maxDecisionOffset {
		return 0, 0, fmt.Errorf("%w: offset %d >= %d，请改用时间窗过滤", model.ErrPageTooDeep, offset, maxDecisionOffset)
	}
	return offset, int(ps), nil
}

// checkTimeRange 校验审计时间窗：负数非法，给了上界就必须不早于下界。
// 「from>to」多半是调用方把两个参数写反，静默返回空列表会让人以为系统没数据。
func checkTimeRange(from, to int64) error {
	if from < 0 || to < 0 {
		return fmt.Errorf("%w: from=%d to=%d", model.ErrInvalidTimeRange, from, to)
	}
	if from > 0 && to > 0 && to < from {
		return fmt.Errorf("%w: to=%d < from=%d", model.ErrInvalidTimeRange, to, from)
	}
	return nil
}

// positiveInt32 把非正的上线参数兜底为默认值（配置写错时不能让服务变成「接受 0 条候选」）。
func positiveInt32(v, fallback int32) int32 {
	if v <= 0 {
		return fallback
	}
	return v
}

func positiveInt(v, fallback int) int {
	if v <= 0 {
		return fallback
	}
	return v
}

func positiveInt64(v, fallback int64) int64 {
	if v <= 0 {
		return fallback
	}
	return v
}

// int32Clamp 把 int64 毫秒数收敛进 int32（cost_ms 列是 INT）；
// 溢出时给上限而不是回绕成负数。
func int32Clamp(v int64) int32 {
	const maxInt32 = int64(^uint32(0) >> 1)
	if v > maxInt32 {
		return int32(maxInt32)
	}
	if v < 0 {
		return 0
	}
	return int32(v)
}

// roundScore 把合成分收敛到 6 位小数：落库摘要与 rpc 回传都要能逐位复算，
// 浮点尾数差异会让「同一输入两次排序」看起来不一致。
func roundScore(v float64) float64 {
	if !finite(v) {
		return 0
	}
	return math.Round(v*1e6) / 1e6
}
