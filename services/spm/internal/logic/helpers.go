// 本文件是 logic 包的手写扩展（入参校验、口径解析、窗口边界与水位解析、缓存与投影转换），
// 不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：这里只放「不碰 SQL」的可测函数——校验、归一、口径 ACTIVE 解析、
// 窗口边界推导、响应组装与缓存键。SQL、CAS 与事务边界一律留在 model。
//
// 范围边界（AGENTS.md §7）：SPM 在本项目只代表用户行为分析链路。本包不存在、也不得新增
// 广告位/投放/计费/分成/会员/订单相关的字段与语义；指标口径全部由 (metric_key,
// metric_version) 版本化描述，logic 只按登记的口径读写字段，不自创指标含义。
package logic

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"go-video/common/ratelimit"
	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 列宽上限（与 deploy/migrations/spm/000001~000003 的 VARCHAR 宽度一致）。
// 超长在 logic 层就拒绝，而不是让 model 的 truncate 静默截断：
// 口径说明与审计文本被截掉尾巴之后，「库里的值」和「评审时看到的值」就不是同一份东西。
const (
	maxMetricKeyBytes   = 100 // spm_metric_definition.metric_key / metric_window.metric_key
	maxNameBytes        = 100 // spm_metric_definition.name
	maxFormulaBytes     = 500 // spm_metric_definition.formula
	maxUnitBytes        = 32  // spm_metric_definition.unit
	maxEventTypesBytes  = 255 // spm_metric_definition.source_event_types
	maxDescriptionBytes = 500 // spm_metric_definition.description（状态迁移理由落这一列）
	maxOperatorBytes    = 64  // operator / created_by
	maxReasonBytes      = 500 // spm_aggregation_job.reason
	maxRequestIDBytes   = 128 // request_id / write_request_id
	maxTopicBytes       = 128 // topic
)

// 缓存键前缀：本服务自有命名空间，不与他服务共用（AGENTS.md §5 的 key 空间边界）。
const (
	// activeDefinitionCachePrefix 缓存「metric_key -> 当前 ACTIVE 口径行」。
	// 真值恒在 spm_metric_definition，登记与状态迁移都会显式失效它。
	activeDefinitionCachePrefix = "govideo:spm:def:active:"
	// hotListCachePrefix 缓存热榜的一页（键含已解析出的 window_start，
	// 所以同一页不会因水位前进而串味）。
	hotListCachePrefix = "govideo:spm:hot:"
)

// maxListOffset 是深翻页保护：OFFSET 达到这个量级时 MySQL 要先跳过同样多的行，
// 而 total 已经告诉调用方还有多少页，所以直接给空页而不是发起扫描。
const maxListOffset int64 = 1_000_000

// definitionUnits 是 unit 的受控集合（契约 MetricDefinition.unit 注释里逐字列出的四个）。
// 不收录任何新单位：单位决定跨窗口能不能合并、调用方怎么展示，加一个就等于改口径。
var definitionUnits = map[string]struct{}{
	"count": {}, "ratio": {}, "seconds": {}, "score": {},
}

// --- 限流（进程级令牌桶，见 ServiceContext.ReadLimiter/WriteLimiter）---

// acquireReadToken 读侧令牌：保护 MySQL 投影表。无令牌即显式 ErrRateLimited，
// 绝不返回空数据——空数据会被调用方读成「这段时间没有指标」。
func acquireReadToken(ctx context.Context, s *svc.ServiceContext, logger logx.Logger,
	rpcName string) (func(), error) {
	done, err := s.ReadLimiter.Allow(ctx)
	if err != nil {
		logger.Errorf("spm/%s: 读侧限流拒绝: %v", rpcName, err)
		return nil, model.ErrRateLimited
	}
	return func() { done(ratelimit.Success) }, nil
}

// acquireWriteToken 写侧令牌：WriteMetricWindow/SubmitAggregationJob 等计算链路入口。
func acquireWriteToken(ctx context.Context, s *svc.ServiceContext, logger logx.Logger,
	rpcName string) (func(), error) {
	done, err := s.WriteLimiter.Allow(ctx)
	if err != nil {
		logger.Errorf("spm/%s: 写侧限流拒绝: %v", rpcName, err)
		return nil, model.ErrRateLimited
	}
	return func() { done(ratelimit.Success) }, nil
}

// --- 通用入参校验 ---

func checkRequestID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ErrRequestIdRequired
	}
	if len(id) > maxRequestIDBytes {
		return fmt.Errorf("%w: request_id %d bytes, max %d", model.ErrRequestIdRequired,
			len(id), maxRequestIDBytes)
	}
	return nil
}

// checkOperator 校验留痕主体：口径与作业变更必须能追到发起人。
func checkOperator(operator string) error {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return model.ErrOperatorRequired
	}
	if len(operator) > maxOperatorBytes {
		return fmt.Errorf("%w: operator %d bytes, max %d", model.ErrOperatorRequired,
			len(operator), maxOperatorBytes)
	}
	return nil
}

// checkReason 校验变更理由：状态迁移与作业提交没有理由就不可受理。
func checkReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return model.ErrReasonRequired
	}
	if len(reason) > maxReasonBytes {
		return fmt.Errorf("%w: reason %d bytes, max %d", model.ErrReasonRequired,
			len(reason), maxReasonBytes)
	}
	return nil
}

func checkMetricKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", model.ErrMetricKeyEmpty
	}
	if len(key) > maxMetricKeyBytes {
		return "", fmt.Errorf("%w: metric_key %d bytes, max %d", model.ErrMetricKeyEmpty,
			len(key), maxMetricKeyBytes)
	}
	return key, nil
}

func checkTopic(topic string) (string, error) {
	topic = strings.TrimSpace(topic)
	if len(topic) > maxTopicBytes {
		return "", fmt.Errorf("%w: topic %d bytes, max %d", model.ErrMetricKeyEmpty,
			len(topic), maxTopicBytes)
	}
	return topic, nil
}

// checkSubject 校验聚合主体：UNSPECIFIED 与越界值都拒绝，主体主键必须是正数。
func checkSubject(subjectType rpc.SubjectType, subjectID int64) (int32, error) {
	t := int32(subjectType)
	if !model.ValidSubjectType(t) {
		return 0, fmt.Errorf("%w: subject_type=%d", model.ErrInvalidSubject, t)
	}
	if subjectID <= 0 {
		return 0, fmt.Errorf("%w: subject_id=%d", model.ErrInvalidSubject, subjectID)
	}
	return t, nil
}

// checkSubjectAllowUnscoped 校验「0 = 全部主体」形态的作业维度：
// 只允许 (0,0) 与 (合法类型, 正数主键) 两种组合，混合形态（类型 0 + 主键非 0）是参数矛盾。
func checkSubjectAllowUnscoped(subjectType rpc.SubjectType, subjectID int64) (int32, error) {
	t := int32(subjectType)
	if t == model.SubjectTypeUnspecified {
		if subjectID != 0 {
			return 0, fmt.Errorf("%w: subject_type=0 时 subject_id 必须为 0", model.ErrInvalidSubject)
		}
		return model.SubjectTypeUnspecified, nil
	}
	return checkSubject(subjectType, subjectID)
}

// checkWindowType 校验窗口粒度，并要求它落在口径登记的 supported_windows 内。
// 只查枚举不查口径就等于允许「用 5 分钟粒度去读只有天级窗口的指标」，
// 那种读法恒空，看起来却像「这个主体没人看」。
func checkWindowType(windowType rpc.WindowType, def *model.MetricDefinition) (int32, error) {
	t := int32(windowType)
	if !model.ValidWindowType(t) {
		return 0, fmt.Errorf("%w: window_type=%d", model.ErrInvalidWindow, t)
	}
	if def != nil && !definitionSupportsWindow(def, t) {
		return 0, fmt.Errorf("%w: 口径 %s@v%d 未登记粒度 %d（supported_windows=%s）",
			model.ErrInvalidWindow, def.MetricKey, def.MetricVersion, t, def.SupportedWindows)
	}
	return t, nil
}

// checkHotSubjectType 限定可出榜的主体维度：MID 出榜等价于「按用户排热度」，
// 本期没有任何这类榜单口径（model.buildHotQuery 同样拒绝，这里提前给出可读错误）。
func checkHotSubjectType(subjectType rpc.SubjectType) (int32, error) {
	switch int32(subjectType) {
	case model.SubjectTypeAid, model.SubjectTypeZone, model.SubjectTypeCatalogItem:
		return int32(subjectType), nil
	default:
		return 0, fmt.Errorf("%w: 热度榜只支持 AID/ZONE/CATALOG_ITEM，收到 %d",
			model.ErrInvalidSubject, int32(subjectType))
	}
}

// --- 分页 ---

// pageSize 归一每页条数：<=0 用配置默认值；超过上限直接拒绝而不是静默 clamp
// （clamp 会让「调用方以为取了 500 行」变成「实际只取了 100 行」，而 total 看着完全正常）。
func pageSize(s *svc.ServiceContext, ps int32) (int32, error) {
	cfg := s.Config.Spm
	if ps < 0 {
		return 0, fmt.Errorf("%w: ps=%d", model.ErrPsTooLarge, ps)
	}
	if ps == 0 {
		size := cfg.PageSize
		if size > cfg.MaxPageSize {
			size = cfg.MaxPageSize
		}
		if size <= 0 {
			// 配置自检已在启动期拒绝这种组合，这里只是防止有人拿零值 svc 手搓请求。
			return 0, fmt.Errorf("%w: 服务端未配置默认页大小", model.ErrPsTooLarge)
		}
		return size, nil
	}
	if ps > cfg.MaxPageSize {
		return 0, fmt.Errorf("%w: ps=%d > %d", model.ErrPsTooLarge, ps, cfg.MaxPageSize)
	}
	return ps, nil
}

// pageOffset 把页码换成 SQL OFFSET。pn<=1 归一为第一页（客户端省略分页参数的常见形态）；
// 超过 maxListOffset 的深页由调用方配合 total 直接给空页，不发 SQL。
func pageOffset(pn int32, ps int32) int64 {
	page := int64(pn)
	if page <= 1 {
		return 0
	}
	return (page - 1) * int64(ps)
}

// pageFits 判断「这一页要不要真的去查」：偏移越过 total 或越过深翻页保护线时给空页。
func pageFits(offset, total int64, size int32) bool {
	if offset >= maxListOffset {
		return false
	}
	return offset < total
}

func offsetTo32(offset int64) int32 {
	if offset > int64(maxListOffset) {
		return int32(maxListOffset)
	}
	return int32(offset)
}

// --- 口径解析（读接口按 ACTIVE 指针，写接口同样只认 ACTIVE）---

// resolveDefinition 把 (metric_key, metric_version) 解析成一行可用口径。
//
// 失败一律返回显式错误而不是「空结果 + nil」：
//   - 未登记 -> ErrMetricDefinitionNotFound
//   - DRAFT/RETIRED -> ErrMetricNotActive（契约：不对外可读、不接受写入）
//   - version=0 且同键存在多个 ACTIVE -> ErrMultipleActiveDefinition（语义二义必须先由人工退役）
//
// version=0 的结果走 ACTIVE 指针缓存；显式版本是精确点查，不缓存（历史版本不可变，
// 缓存它没有收益，反而让「退役后立刻停读」变成要等 TTL）。
func resolveDefinition(ctx context.Context, s *svc.ServiceContext, metricKey string,
	version int32) (*model.MetricDefinition, error) {
	key, err := checkMetricKey(metricKey)
	if err != nil {
		return nil, err
	}
	if version > 0 {
		row, err := s.Definitions.FindByKeyVersion(ctx, key, version)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, fmt.Errorf("%w: %s@v%d", model.ErrMetricDefinitionNotFound, key, version)
		}
		if row.State != model.DefinitionStateActive {
			return nil, fmt.Errorf("%w: %s@v%d state=%d", model.ErrMetricNotActive,
				key, version, row.State)
		}
		return row, nil
	}
	if row := cachedActiveDefinition(ctx, s, key); row != nil {
		return row, nil
	}
	row, err := s.Definitions.FindActive(ctx, key)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("%w: %s 没有 ACTIVE 版本", model.ErrMetricDefinitionNotFound, key)
	}
	// FindActive 是 LIMIT 1：多 ACTIVE 时它随机给一条，必须显式数一遍才能判定二义。
	active, err := s.Definitions.CountActive(ctx, key)
	if err != nil {
		return nil, err
	}
	if active > 1 {
		return nil, fmt.Errorf("%w: %s 有 %d 个 ACTIVE 版本",
			model.ErrMultipleActiveDefinition, key, active)
	}
	cacheActiveDefinition(ctx, s, key, row)
	return row, nil
}

// definitionSupportsWindow 判断口径是否登记了该窗口粒度。supported_windows 是 CSV，
// 解析失败（脏数据）按「不支持」处理：宁可读不到，也不能拿一个未登记的粒度当合法口径用。
func definitionSupportsWindow(def *model.MetricDefinition, windowType int32) bool {
	for _, w := range parseCSVList(def.SupportedWindows) {
		n, err := strconv.ParseInt(w, 10, 32)
		if err != nil {
			continue
		}
		if int32(n) == windowType {
			return true
		}
	}
	return false
}

// parseCSVList 拆分受控 CSV 列（supported_windows / source_event_types），去空白与空项。
func parseCSVList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// canonicalCSVList 去重 + 升序 + 逗号拼接（无空格）。
// 规范化是必需的：口径的「一致」要按语义比，"1,2" 与 "2, 1" 必须判成同一份登记。
func canonicalCSVList(items []string) string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, it := range items {
		if _, dup := seen[it]; dup {
			continue
		}
		seen[it] = struct{}{}
		out = append(out, it)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// supportedWindowsCSV 把 rpc.WindowType 列表落成 supported_windows 列。
// 空集合拒绝：没有合法粒度的口径永远读不到东西，登记它只是把错误推到运行时。
func supportedWindowsCSV(windows []rpc.WindowType) (string, error) {
	items := make([]string, 0, len(windows))
	for _, w := range windows {
		t := int32(w)
		if !model.ValidWindowType(t) {
			return "", fmt.Errorf("%w: supported_windows 含未定义粒度 %d", model.ErrInvalidWindow, t)
		}
		items = append(items, strconv.FormatInt(int64(t), 10))
	}
	if len(items) == 0 {
		return "", fmt.Errorf("%w: supported_windows 不能为空", model.ErrInvalidWindow)
	}
	return canonicalCSVList(items), nil
}

// sourceEventTypesCSV 校验并归一 source_event_types：每一项必须落在
// model.SupportedEventType 白名单内（AGENTS.md §7 的行为链路），不接受自造事件名。
func sourceEventTypesCSV(raw string) (string, error) {
	items := parseCSVList(raw)
	if len(items) == 0 {
		return "", fmt.Errorf("%w: source_event_types 不能为空", model.ErrInvalidDefinitionSpec)
	}
	for _, it := range items {
		if !model.SupportedEventType(it) {
			return "", fmt.Errorf("%w: 未订阅的事件类型 %q", model.ErrInvalidDefinitionSpec, it)
		}
	}
	if len(strings.Join(items, ",")) > maxEventTypesBytes {
		return "", fmt.Errorf("%w: source_event_types 超过 %d 字节",
			model.ErrInvalidDefinitionSpec, maxEventTypesBytes)
	}
	return canonicalCSVList(items), nil
}

// --- 窗口边界 ---

// windowStart 归一单点窗口边界：TOTAL 恒为 0（对它「最近闭合窗口」没有意义，
// 且水位表按 ErrInvalidWindow 拒绝 TOTAL），其余粒度必须规整到左边界。
func windowStart(windowStartTS int64, windowType int32) int64 {
	if windowType == model.WindowTypeTotal {
		return 0
	}
	return model.AlignWindow(windowStartTS, windowType)
}

// resolveWindowStart 把契约里的 window_start=0 解析成「最近一个已闭合窗口」。
//
// 先查 spm_window_watermark（uniq_watermark 点查），水位行尚未建立时才退回
// spm_metric_window 的 MAX(window_start) 极值索引，并且只统计已闭合的窗口
// （左边界 <= now - 窗口长度），否则会把正在写入的窗口当成榜的基准。
// 返回 windowStart=0 表示该口径确实还没有闭合窗口，调用方按「无数据」处理，
// 绝不伪造 now。第三返回值是水位的推进时刻（0 = 兜底路径拿不到，调用方据此跳过停摆判定）。
func resolveWindowStart(ctx context.Context, s *svc.ServiceContext, logger logx.Logger,
	subjectType int32, metricKey string, version, windowType int32) (int64, int64, error) {
	if windowType == model.WindowTypeTotal {
		return 0, 0, nil
	}
	wm, err := s.Watermarks.Find(ctx, subjectType, metricKey, version, windowType)
	if err != nil {
		return 0, 0, err
	}
	if wm != nil && wm.LastClosedStart > 0 {
		lag := time.Now().Unix() - wm.LastEventTime
		if s.Config.Spm.WatermarkLagSeconds > 0 && lag > s.Config.Spm.WatermarkLagSeconds {
			// 聚合器停摆：仍返回最近闭合窗口（真实 window_start 一并回显给调用方），
			// 但必须在日志里留下证据，否则「榜停在几点」只能靠人肉比对时间戳。
			logger.Errorf("spm: 水位落后 %d 秒（阈值 %d）subject_type=%d metric=%s@v%d window_type=%d",
				lag, s.Config.Spm.WatermarkLagSeconds, subjectType, metricKey, version, windowType)
		}
		return wm.LastClosedStart, wm.LastEventTime, nil
	}
	closedBefore := time.Now().Unix() - model.WindowSeconds(windowType)
	latest, err := s.Windows.FindLatestWindowStart(ctx, subjectType, metricKey, version,
		windowType, closedBefore)
	if err != nil {
		return 0, 0, err
	}
	return windowStart(latest, windowType), 0, nil
}

// consecutiveWindowStarts 生成从 from（含）开始、共 count 个已规整的窗口左边界。
// TOTAL 只有一个「窗口」（window_start=0），count 再大也只给一个点。
func consecutiveWindowStarts(from int64, windowType int32, count int32) []int64 {
	if windowType == model.WindowTypeTotal {
		return []int64{0}
	}
	sec := model.WindowSeconds(windowType)
	if sec <= 0 || count <= 0 {
		return nil
	}
	start := model.AlignWindow(from, windowType)
	out := make([]int64, 0, count)
	for i := int32(0); i < count; i++ {
		out = append(out, start+int64(i)*sec)
	}
	return out
}

// --- 迟到判定 ---

// lateWindow 判断一次写入是否已经越过迟到容忍线。
//
// 基准是水位而不是本地时钟：容忍的是「比最近闭合窗口再早 LateToleranceSeconds 之外」的数据，
// 这样 5 分钟窗口配 2 分钟容忍时，最多只会改写刚刚闭合的那一个窗口。
// 水位还不存在（口径首日）时不做判定——没有任何已闭合窗口能被改写坏。
func lateWindow(wm *model.WindowWatermark, toleranceSeconds int64, start int64) bool {
	if wm == nil || wm.LastClosedStart <= 0 {
		return false
	}
	return start < wm.LastClosedStart-toleranceSeconds
}

// --- 投影转换 ---

func metricPointOf(row *model.MetricWindow) *rpc.MetricPoint {
	return &rpc.MetricPoint{
		MetricKey:     row.MetricKey,
		MetricVersion: row.MetricVersion,
		SubjectType:   rpc.SubjectType(row.SubjectType),
		SubjectId:     row.SubjectID,
		WindowType:    rpc.WindowType(row.WindowType),
		WindowStart:   row.WindowStart,
		Value:         row.MetricValue,
		Numerator:     row.Numerator,
		Denominator:   row.Denominator,
		SampleCount:   row.SampleCount,
		EventTime:     row.EventTime,
	}
}

// pointKey 是 BatchGetMetricsReply.points 的 map 键形态，与契约注释逐字一致：
// "<metric_key>@v<metric_version>:<window_start>"。
func pointKey(metricKey string, version int32, windowStart int64) string {
	return fmt.Sprintf("%s@v%d:%d", metricKey, version, windowStart)
}

// rejectedPointKey 是 WriteMetricWindowReply.rejected_keys 的摘要形态（契约注释同源）。
// 只含主体类型、主体主键与指标键：不带任何行为明细（AGENTS.md §7 隐私边界）。
func rejectedPointKey(subjectType, subjectID int64, metricKey string) string {
	return fmt.Sprintf("%d:%d:%s", subjectType, subjectID, metricKey)
}

func definitionOf(row *model.MetricDefinition) *rpc.MetricDefinition {
	if row == nil {
		return nil
	}
	windows := make([]rpc.WindowType, 0, 4)
	for _, w := range parseCSVList(row.SupportedWindows) {
		n, err := strconv.ParseInt(w, 10, 32)
		if err != nil || !model.ValidWindowType(int32(n)) {
			// 脏登记不猜：跳过该粒度，调用方仍能看到口径的其余说明。
			continue
		}
		windows = append(windows, rpc.WindowType(n))
	}
	return &rpc.MetricDefinition{
		MetricKey:        row.MetricKey,
		MetricVersion:    row.MetricVersion,
		Name:             row.Name,
		Formula:          row.Formula,
		Unit:             row.Unit,
		SupportedWindows: windows,
		SourceEventTypes: row.SourceEventTypes,
		State:            rpc.DefinitionState(row.State),
		Description:      row.Description,
		CreatedBy:        row.CreatedBy,
		Ctime:            row.Ctime,
		Mtime:            row.Mtime,
	}
}

func jobOf(row *model.AggregationJob) *rpc.AggregationJob {
	if row == nil {
		return nil
	}
	return &rpc.AggregationJob{
		JobId:           row.ID,
		JobType:         rpc.JobType(row.JobType),
		State:           rpc.JobState(row.State),
		SubjectType:     rpc.SubjectType(row.SubjectType),
		SubjectId:       row.SubjectID,
		MetricKey:       row.MetricKey,
		MetricVersion:   row.MetricVersion,
		WindowType:      rpc.WindowType(row.WindowType),
		WindowStartFrom: row.WindowStartFrom,
		WindowStartTo:   row.WindowStartTo,
		WindowsTotal:    row.WindowsTotal,
		WindowsDone:     row.WindowsDone,
		WindowsFailed:   row.WindowsFailed,
		RequestId:       row.RequestID,
		Operator:        row.Operator,
		Reason:          row.Reason,
		LastError:       row.LastError,
		Ctime:           row.Ctime,
		Mtime:           row.Mtime,
		FinishedAt:      row.FinishedAt,
	}
}

func jobList(rows []*model.AggregationJob) []*rpc.AggregationJob {
	out := make([]*rpc.AggregationJob, 0, len(rows))
	for _, r := range rows {
		out = append(out, jobOf(r))
	}
	return out
}

func definitionList(rows []*model.MetricDefinition) []*rpc.MetricDefinition {
	out := make([]*rpc.MetricDefinition, 0, len(rows))
	for _, r := range rows {
		out = append(out, definitionOf(r))
	}
	return out
}

// --- 计划窗口数 ---

// plannedWindows 计算 [from,to] 区间（含端点，均为已规整左边界）覆盖的窗口数。
// TOTAL 只有一个累计窗口。to<from 由调用方在此之前拒绝；这里再兜一次防算术倒挂。
func plannedWindows(from, to int64, windowType int32) (int32, error) {
	if windowType == model.WindowTypeTotal {
		return 1, nil
	}
	sec := model.WindowSeconds(windowType)
	if sec <= 0 {
		return 0, fmt.Errorf("%w: window_type=%d", model.ErrInvalidWindow, windowType)
	}
	if to < from {
		return 0, fmt.Errorf("%w: 窗口区间倒挂 %d > %d", model.ErrInvalidWindow, to, from)
	}
	n := (to-from)/sec + 1
	if n > int64(^uint32(0)>>1) {
		return 0, fmt.Errorf("%w: 窗口跨度溢出", model.ErrWindowRangeTooLarge)
	}
	return int32(n), nil
}

// checkJobWindowRange 规整并校验作业的窗口区间：to=0 时按「现在」补，
// 跨度超过 Spm.MaxWindowsPerJob 直接拒绝（拆半执行会让同一 request_id
// 有时覆盖整段、有时只覆盖一段，重算进度就没法解释）。
func checkJobWindowRange(s *svc.ServiceContext, from, to int64, windowType int32,
	maxWindows int32) (int64, int64, int32, error) {
	if windowType != model.WindowTypeTotal && from <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: window_start_from 必填", model.ErrInvalidWindow)
	}
	if to == 0 {
		to = time.Now().Unix()
	}
	from = windowStart(from, windowType)
	to = windowStart(to, windowType)
	total, err := plannedWindows(from, to, windowType)
	if err != nil {
		return 0, 0, 0, err
	}
	if maxWindows > 0 && int32(total) > maxWindows {
		return 0, 0, 0, fmt.Errorf("%w: 计划 %d 个窗口，上限 %d，请由调用方拆分提交",
			model.ErrWindowRangeTooLarge, total, maxWindows)
	}
	return from, to, total, nil
}

// batchDefinitions 一次调用内的口径解析缓存。
//
// WriteMetricWindow 要按行校验口径，而一批 500 行通常同属一个 (metric_key, version)：
// 不缓存就是 500 次注册表点查。解析失败同样缓存 —— 本次请求内的口径状态视为一份快照，
// 中途被改（并发退役）本来就不该让同一批数据一半按旧口径写、一半被拒。
type batchDefinitions struct {
	entries map[string]batchDefinition
}

type batchDefinition struct {
	def *model.MetricDefinition
	err error
}

func newBatchDefinitions() *batchDefinitions {
	return &batchDefinitions{entries: make(map[string]batchDefinition)}
}

func (b *batchDefinitions) resolve(ctx context.Context, s *svc.ServiceContext,
	metricKey string, version int32) (*model.MetricDefinition, error) {
	key := pointKey(metricKey, version, 0)
	if hit, ok := b.entries[key]; ok {
		return hit.def, hit.err
	}
	def, err := resolveDefinition(ctx, s, metricKey, version)
	b.entries[key] = batchDefinition{def: def, err: err}
	return def, err
}

// --- 作业提交（RecomputeMetrics 与 SubmitAggregationJob 共用）---

// submitJob 幂等落一条 PENDING 作业并回读整行。
//
// InsertIfAbsent 只告诉「是不是新插的」，拿不到主键（AggregationJobModel 不回填 ID），
// 所以两条路径都要按 uniq_request_id 回读一次：job_id 与进度都以库里的行为准。
// 回读命中但内容与本次请求不一致时返回 ErrRequestIdConflict —— 幂等键的语义是
// 「同一份请求的重复投递」，同键改内容后再返回首次的 job_id，调用方会以为新任务已排队。
func submitJob(ctx context.Context, s *svc.ServiceContext, logger logx.Logger,
	j *model.AggregationJob) (*model.AggregationJob, bool, error) {
	created, err := s.Jobs.InsertIfAbsent(ctx, j)
	if err != nil {
		return nil, false, err
	}
	existing, err := s.Jobs.FindByRequestID(ctx, j.RequestID)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		// 插成功却读不到（或读失败后主从延迟之外的可能几乎为 0）：宁可报错，
		// 也不能回一个 job_id=0 让调用方去查一个不存在的作业。
		if created {
			return nil, false, fmt.Errorf("%w: 作业已提交但回读失败 request_id=%s",
				model.ErrJobNotFound, j.RequestID)
		}
		return nil, false, fmt.Errorf("%w: request_id=%s", model.ErrJobNotFound, j.RequestID)
	}
	if sameJobRequest(j, existing) {
		return existing, !created, nil
	}
	logger.Errorf("spm: request_id=%s 复用到不同内容的作业（已存在 job_id=%d type=%d metric=%s@v%d）",
		j.RequestID, existing.ID, existing.JobType, existing.MetricKey, existing.MetricVersion)
	return nil, false, fmt.Errorf("%w: request_id=%s 已用于 job_id=%d",
		model.ErrRequestIdConflict, j.RequestID, existing.ID)
}

// sameJobRequest 比较作业的业务身份：区间与维度相同即同一份请求。
// 进度、状态、留痕列不参与（它们由执行器与首次提交决定，不是调用方能改的东西）。
func sameJobRequest(want, got *model.AggregationJob) bool {
	return want.JobType == got.JobType &&
		want.SubjectType == got.SubjectType &&
		want.SubjectID == got.SubjectID &&
		want.MetricKey == got.MetricKey &&
		want.MetricVersion == got.MetricVersion &&
		want.WindowType == got.WindowType &&
		want.WindowStartFrom == got.WindowStartFrom &&
		want.WindowStartTo == got.WindowStartTo
}

// --- 缓存（只加速，真值恒在 MySQL；缓存缺失或脏数据一律回源）---

// shortCacheTTL 返回短缓存秒数。
//
// 取 LateToleranceSeconds 而不是另加一个配置项：这条线的语义正是
// 「已闭合窗口还会被改写的时间上界」（config.SpmConf.LateToleranceSeconds），
// 超过它迟到数据只能走显式回填，缓存也就不可能再给出与库里不同的值。
// 配 0 表示不允许任何缓存（口径要么立刻生效，要么不缓存）。
func shortCacheTTL(s *svc.ServiceContext) int64 {
	if s.Cache == nil {
		return 0
	}
	ttl := s.Config.Spm.LateToleranceSeconds
	if ttl < 0 {
		return 0
	}
	return ttl
}

func cacheGetJSON(ctx context.Context, s *svc.ServiceContext, key string, into any) bool {
	if s.Cache == nil {
		return false
	}
	raw, err := s.Cache.GetCtx(ctx, key)
	if err != nil || raw == "" {
		// 缓存未命中与 Redis 故障同样只回源，不影响任何口径值（ServiceContext 注释同源）。
		return false
	}
	return json.Unmarshal([]byte(raw), into) == nil
}

func cacheSetJSON(ctx context.Context, s *svc.ServiceContext, key string, val any) {
	ttl := shortCacheTTL(s)
	if ttl == 0 {
		return
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return
	}
	if err := s.Cache.SetexCtx(ctx, key, string(raw), int(ttl)); err != nil {
		logx.Errorf("spm/logic: 写缓存 %s 失败: %v", key, err)
	}
}

func cacheDel(ctx context.Context, s *svc.ServiceContext, keys ...string) {
	if s.Cache == nil || len(keys) == 0 {
		return
	}
	if _, err := s.Cache.DelCtx(ctx, keys...); err != nil {
		// 失效失败只影响陈旧上界（TTL 仍然兜住），不能让已经成功的写回滚。
		logx.Errorf("spm/logic: 失效缓存 %v 失败: %v", keys, err)
	}
}

func activeDefinitionCacheKey(metricKey string) string {
	return activeDefinitionCachePrefix + metricKey
}

// cachedActiveDefinition 读 ACTIVE 指针。缓存里的行必须与键同指标，
// 否则说明键被别的口径写过，宁缺不滥。
func cachedActiveDefinition(ctx context.Context, s *svc.ServiceContext,
	metricKey string) *model.MetricDefinition {
	var row model.MetricDefinition
	if !cacheGetJSON(ctx, s, activeDefinitionCacheKey(metricKey), &row) {
		return nil
	}
	if row.MetricKey != metricKey || row.State != model.DefinitionStateActive ||
		row.MetricVersion <= 0 {
		return nil
	}
	return &row
}

func cacheActiveDefinition(ctx context.Context, s *svc.ServiceContext, metricKey string,
	row *model.MetricDefinition) {
	if row == nil {
		return
	}
	// 只缓存「ACTIVE 且唯一」这一件事实：状态与版本都由登记/迁移流程显式失效维护，
	// TTL 只是漏失效时的兜底上界。
	cacheSetJSON(ctx, s, activeDefinitionCacheKey(metricKey), row)
}

// invalidateActiveDefinition 在口径登记与状态迁移后调用。
func invalidateActiveDefinition(ctx context.Context, s *svc.ServiceContext, metricKey string) {
	if key, err := checkMetricKey(metricKey); err == nil {
		cacheDel(ctx, s, activeDefinitionCacheKey(key))
	}
}
