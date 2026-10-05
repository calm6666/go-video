// 本文件是 logic 包的手写扩展（入参校验、枚举映射、降级汇总、幂等回放与缓存键），
// 不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：这里只放「不碰 SQL」的可测函数——校验、归一、枚举映射、
// 降级矩阵的状态收集、幂等回放编解码与缓存键构造。
// SQL、CAS 与事务边界一律留在 model，事务编排留在各 logic 文件。

package logic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/idempotency"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// 列宽上限（与 deploy/migrations/recommend-recall/0000NN 的 DDL 一致）。
// 校验发生在 logic：model 只保证「不合法就别写」，logic 保证「别把不合法的请求带进事务」。
const (
	// colRefID 对应 request_id / snapshot_id / batch_id / generator / operator 的 VARCHAR(64)。
	colRefID = 64
	// colNote 对应 recall_pool_version.note / recall_pool_current.note 的 VARCHAR(255)。
	colNote = 255
	// colScene 对应 recall_request_log.scene 的 VARCHAR(64)。
	colScene = 64
	// colAppVersion 对应 recall_request_log.app_version 的 VARCHAR(32)。
	colAppVersion = 32
	// colRegion 对应 recall_request_log.region 的 VARCHAR(32)。
	colRegion = 32
	// colIdempotencyKey 对应 recall_idempotency.idempotency_key 的 VARCHAR(128)。
	colIdempotencyKey = model.MaxIdempotencyKeyLen
	// sha256HexLen 是 sha256 小写 hex 的长度（device_id_hash 的期望形态）。
	sha256HexLen = model.RequestHashLen
)

// 本服务自有缓存命名空间（AGENTS.md §5：Redis key 空间按服务隔离）。
const (
	// topnCacheKeyPrefix 是在线召回的「某池某版本 TopN」读透缓存键前缀。
	// 键里带 version：版本切换后天然读到新键，无需失效旧键（已发布版本不可变，
	// 所以旧键的 contents 永远不会变错，只会随 TTL 自然过期）。
	topnCacheKeyPrefix = "govideo:recall:topn:"
)

// --- 池寻址 ---

// requirePool 校验 rpc.PoolRef 并返回 (source, pool_key)。
// source 编号与 pool_key 语法的组合校验统一交给 model.ValidatePoolKey，
// 本服务任何路径都不接受未校验的池键字符串（否则同一个池会出现两个互不可见的键名）。
func requirePool(ref *rpc.PoolRef) (int32, string, error) {
	if ref == nil {
		return 0, "", fmt.Errorf("%w: pool is required", model.ErrInvalidPoolKey)
	}
	source := int32(ref.GetSource())
	if !model.ValidSource(source) {
		return 0, "", fmt.Errorf("%w: source=%d", model.ErrInvalidSource, source)
	}
	key := strings.TrimSpace(ref.GetPoolKey())
	if err := model.ValidatePoolKey(source, key); err != nil {
		return 0, "", err
	}
	return source, key, nil
}

// poolRef 构造 rpc.PoolRef 回显（响应必须说明"哪个池"，不能让调用方自己猜寻址结果）。
func poolRef(source int32, key string) *rpc.PoolRef {
	return &rpc.PoolRef{Source: rpc.Source(source), PoolKey: key}
}

// --- 枚举映射（model <-> rpc；编号一致性由 contract_consistency_test.go 断言）---

func toRPCState(state int32) rpc.PoolVersionState {
	if !model.ValidVersionState(state) {
		return rpc.PoolVersionState_POOL_VERSION_STATE_UNSPECIFIED
	}
	return rpc.PoolVersionState(state)
}

func toRPCSource(source int32) rpc.Source {
	if !model.ValidSource(source) {
		return rpc.Source_SOURCE_UNSPECIFIED
	}
	return rpc.Source(source)
}

// sourceFromRPC 把 rpc.Source 转成 model 编号；未定义取值直接报错而不是当 0 处理。
func sourceFromRPC(s rpc.Source) (int32, error) {
	source := int32(s)
	if !model.ValidSource(source) {
		return 0, fmt.Errorf("%w: %d", model.ErrInvalidSource, source)
	}
	return source, nil
}

// sourcesFromRPC 去重后的召回路列表（保持调用方给出的顺序，排序在选路阶段做）。
// 同一个路重复出现不构成错误（幂等语义：请求三路 == 请求这三路去重后），
// 但必须先收敛成集合，否则逐路取数会重复消耗预算。
func sourcesFromRPC(in []rpc.Source) ([]int32, error) {
	out := make([]int32, 0, len(in))
	seen := make(map[int32]struct{}, len(in))
	for _, s := range in {
		source, err := sourceFromRPC(s)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, source)
	}
	return out, nil
}

// enabledSet 把配置里的 []int64 召回路转成集合。
func enabledSet(in []int64) map[int32]struct{} {
	set := make(map[int32]struct{}, len(in))
	for _, s := range in {
		set[int32(s)] = struct{}{}
	}
	return set
}

// sourceList 把 []int64 转成 int32 列表（配置项下发用）。
func sourceList(in []int64) []int32 {
	out := make([]int32, 0, len(in))
	for _, s := range in {
		out = append(out, int32(s))
	}
	return out
}

func rpcSourceList(in []int32) []rpc.Source {
	out := make([]rpc.Source, 0, len(in))
	for _, s := range in {
		out = append(out, toRPCSource(s))
	}
	return out
}

// platformFromRPC 归一平台编号：UNSPECIFIED 归 0（"未知端"，只走 global 池）。
// 这里不拒绝未知值：rpc 枚举由编译期保证，0 与 1..4 之外的取值一律按未知处理，
// 让召回继续出数而不是把客户端版本差异变成在线故障。
func platformFromRPC(p rpc.Platform) int32 {
	if model.ValidPlatform(int32(p)) {
		return int32(p)
	}
	return 0
}

func toRPCPlatform(p int32) rpc.Platform {
	return rpc.Platform(p)
}

// degradeReasonFromKey 把落库/日志的稳定 key 转成 rpc 枚举。
// 未知 key 归 UNSPECIFIED 而不是硬编一个：枚举编号是冻结契约，
// 新增 key 时必须先在 model 侧补常量（contract_consistency_test 会挡住漂移）。
func degradeReasonFromKey(key string) rpc.DegradeReason {
	if key == model.DegradeReasonNone {
		return rpc.DegradeReason_DEGRADE_REASON_UNSPECIFIED
	}
	name := "DEGRADE_REASON_" + strings.ToUpper(key)
	val, ok := rpc.DegradeReason_value[name]
	if !ok {
		return rpc.DegradeReason_DEGRADE_REASON_UNSPECIFIED
	}
	return rpc.DegradeReason(val)
}

// degradeKeyFromReason 是反向映射（日志回放用）：0 表示未降级 -> 空串。
func degradeKeyFromReason(reason rpc.DegradeReason) string {
	if reason == rpc.DegradeReason_DEGRADE_REASON_UNSPECIFIED {
		return model.DegradeReasonNone
	}
	return strings.ToLower(strings.TrimPrefix(reason.String(), "DEGRADE_REASON_"))
}

// --- 文本字段校验（列宽与隐私红线）---

// checkMaxLen 校验文本字段落在列宽内；超长拒绝而不是静默截断。
// 例外是 trace_id（见 sanitizeTraceID）：它是关联句柄而非业务事实。
func checkMaxLen(name, val string, max int) error {
	if len(val) > max {
		return fmt.Errorf("%w: %s %d > %d", model.ErrFieldTooLong, name, len(val), max)
	}
	return nil
}

// requiredRef 校验必填的引用类文本（batch_id / generator / operator / idempotency_key）。
// cause 由调用方给出对应的 model 哨兵：缺哪个字段就报哪个错，
// 统一包成一个哨兵会让调用方无法区分"作业没传批次号"和"运营没传幂等键"。
func requiredRef(name, val string, max int, cause error) (string, error) {
	v := strings.TrimSpace(val)
	if v == "" {
		return "", fmt.Errorf("%w: %s required", cause, name)
	}
	if err := checkMaxLen(name, v, max); err != nil {
		return "", err
	}
	return v, nil
}

// optionalText 归一可选文本：去空白、限长；空串合法。
func optionalText(name, val string, max int) (string, error) {
	v := strings.TrimSpace(val)
	if err := checkMaxLen(name, v, max); err != nil {
		return "", err
	}
	return v, nil
}

// sanitizeTraceID 裁剪 trace_id 到列宽：trace 只用于关联日志，
// 截断尾巴不改变正确性，而因它让整个写路径失败反而丢审计。
func sanitizeTraceID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > colRefID {
		return id[:colRefID]
	}
	return id
}

// checkDeviceIDHash 是隐私红线（AGENTS.md §2/§7）：设备维度只接受 sha256 摘要，
// 明文设备号/IMEI/UUID 原文一律拒绝，绝不进日志也不进任何表。
// 空串合法（表示未知设备，游客召回照常出数）。
func checkDeviceIDHash(hash string) error {
	h := strings.TrimSpace(hash)
	if h == "" {
		return nil
	}
	if len(h) != sha256HexLen {
		return fmt.Errorf("%w: len=%d want=%d", model.ErrRawDeviceID, len(h), sha256HexLen)
	}
	if _, err := hex.DecodeString(h); err != nil {
		return fmt.Errorf("%w: not a hex digest", model.ErrRawDeviceID)
	}
	return nil
}

// --- 幂等回放 ---

// claimWrite 认领写接口幂等键。
//
// 认领必须在业务事务之外自动提交（model.RecallIdempotencyModel 的推荐时序）：
// 进程崩溃时 PENDING 行仍在，由 lease_expire_at 到期后允许重新认领；
// 若把认领放进业务事务，回滚会把"我占了这把键"一起抹掉，重试就变成第二次真实执行。
func claimWrite(ctx context.Context, repo *repository.Repository, scope, key, fingerprint, operator string) (*model.IdempotencyClaim, error) {
	now := time.Now().Unix()
	return repo.Idempotency.Claim(ctx, nil, &model.RecallIdempotency{
		Scope:          scope,
		IdempotencyKey: key,
		RequestHash:    fingerprint,
		State:          string(idempotency.StatePending),
		Operator:       operator,
		LeaseExpireAt:  now + repo.Options().IdempotencyLeaseSeconds,
		ExpireAt:       now + repo.Options().IdempotencyRetentionSeconds,
	}, now)
}

// replayPayload 把已存的幂等结果反序列化回放。
// raw 为空说明上一次执行没写完结果（租约过期重放、或清理任务删了记录后 Claim 判定为新执行），
// 此时必须报错让调用方重发，而不是回一个零值响应冒充上次的结果。
func replayPayload(raw string, into interface{}) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%w: stored result payload is empty", model.ErrIdempotencyExists)
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		return fmt.Errorf("%w: unmarshal stored result: %v", model.ErrIdempotencyExists, err)
	}
	return nil
}

// payloadOf 把回复投影序列化成幂等回放载荷（不含敏感原文，只含版本/条数等控制信息）。
func payloadOf(msg interface{}) (string, error) {
	raw, err := json.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("recommend-recall: marshal idempotent payload: %w", err)
	}
	if len(raw) > model.MaxIdempotencyPayloadLen {
		return "", fmt.Errorf("%w: payload %d > %d", model.ErrTooManyItems, len(raw), model.MaxIdempotencyPayloadLen)
	}
	return string(raw), nil
}

// heldOrConflict 把"键还在别人租约内且没有可回放结果"的情况翻译成明确错误。
// SUCCEEDED 的重放走 payload 回放；PENDING 且没结果说明前一次执行还没收敛，
// 这时再执行一次就是两次副作用，所以让调用方重试。
func heldOrConflict(claim *model.IdempotencyClaim) error {
	if claim == nil || claim.Existing == nil {
		return model.ErrIdempotencyExists
	}
	if strings.TrimSpace(claim.Existing.ResultPayload) != "" {
		return nil
	}
	return fmt.Errorf("%w: key=%s state=%s", model.ErrIdempotencyExists,
		claim.Existing.IdempotencyKey, claim.Existing.State)
}

// markFailedBestEffort 在业务事务失败后把幂等键放回可重试状态。
// 返回值只用于调用方记日志：业务写已经回滚，键留在 PENDING 只会让重试等到租约过期，
// 不能因为"清不掉标记"再对外报一个与原错误无关的错。
func markFailedBestEffort(ctx context.Context, repo *repository.Repository, scope, key string, cause error) error {
	if _, err := repo.Idempotency.MarkFailed(ctx, scope, key, clipErr(cause)); err != nil {
		return fmt.Errorf("recommend-recall: mark idempotency failed (scope=%s key=%s): %w", scope, key, err)
	}
	return nil
}

// clipErr 把错误文本压到 last_error 列的宽度内（model 侧也裁一次，这里提前裁避免超长文本进日志）。
func clipErr(cause error) string {
	if cause == nil {
		return ""
	}
	msg := cause.Error()
	if len(msg) > 512 {
		return msg[:512]
	}
	return msg
}

// --- 集合与条数归一 ---

// uniquePositive 去重并保持顺序（种子、排除 aid 都用它）。
func uniquePositive(in []int64) []int64 {
	out := make([]int64, 0, len(in))
	seen := make(map[int64]struct{}, len(in))
	for _, v := range in {
		if v <= 0 {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// checkAidList 校验 aid/tag 列表：每条必须是正数、去重后不得超上限。
// 非正数与超限都拒绝（静默丢掉一条会让"排除已看"这类语义漏放行）。
func checkAidList(name string, in []int64, max int) ([]int64, error) {
	for _, v := range in {
		if v <= 0 {
			return nil, fmt.Errorf("%w: %s contains %d", model.ErrInvalidAid, name, v)
		}
	}
	list := uniquePositive(in)
	if len(list) > max {
		return nil, fmt.Errorf("%w: %s %d > %d", model.ErrTooManySeeds, name, len(list), max)
	}
	return list, nil
}

// int64Set 把列表转成集合（排除集判定用）。
func int64Set(in []int64) map[int64]struct{} {
	set := make(map[int64]struct{}, len(in))
	for _, v := range in {
		set[v] = struct{}{}
	}
	return set
}

// --- 版本摘要与分页 ---

// versionPair 是一次召回实际读到的 (source, pool_key, version) 三元组。
// pool_key 必须进摘要：同一个 source 可能读多个池（标签/协同有 N 个种子池），
// 只按 source 聚合会把"换了池"误判成"没换版本"。
type versionPair struct {
	Source  int32
	PoolKey string
	Version int64
}

// versionsDigest 计算 sha256(source:pool_key:version 升序序列)。
// 排序保证同一集合恒定同串，回放时先比摘要即可判断"是不是同一批池快照"。
func versionsDigest(pairs []versionPair) string {
	ordered := make([]string, 0, len(pairs))
	for _, p := range pairs {
		ordered = append(ordered, fmt.Sprintf("%d:%s:%d", p.Source, p.PoolKey, p.Version))
	}
	sort.Strings(ordered)
	sum := sha256.Sum256([]byte(strings.Join(ordered, "|")))
	return hex.EncodeToString(sum[:])
}

// pageArgs 归一页码/页大小：pn<1 归 1，ps<=0 用默认值，超上限报错（不静默裁剪）。
func pageArgs(pn, ps int32, defaultSize, maxSize, maxOffset int) (offset, limit int, err error) {
	if pn < 0 {
		return 0, 0, fmt.Errorf("%w: pn=%d", model.ErrPageTooDeep, pn)
	}
	page := int(pn)
	if page < 1 {
		page = 1
	}
	size := int(ps)
	if size <= 0 {
		size = defaultSize
	}
	if size > maxSize {
		return 0, 0, fmt.Errorf("%w: ps=%d > %d", model.ErrLimitTooLarge, size, maxSize)
	}
	// 乘法溢出保护：page 是 int32，(page-1)*size 在 32 位平台上可能溢出成负数，
	// 那会把"深翻"变成一个 OFFSET 为负的合法请求。
	if size > 0 && page-1 > maxOffset/size {
		return 0, 0, fmt.Errorf("%w: offset exceeds %d", model.ErrPageTooDeep, maxOffset)
	}
	offset = (page - 1) * size
	if offset > maxOffset {
		return 0, 0, fmt.Errorf("%w: offset=%d", model.ErrPageTooDeep, offset)
	}
	return offset, size, nil
}

// --- 逐路统计的落库编解码 ---

// encodeSourceStats 序列化逐路统计（recall_request_log.per_source 列，TEXT/JSON）。
func encodeSourceStats(stats []*rpc.SourceStat) (string, error) {
	if len(stats) == 0 {
		// 列是 NOT NULL，且"没有逐路统计"本身是可解释的状态：写空数组而不是空串，
		// 回放时能区分"没统计"与"JSON 解析失败"。
		return "[]", nil
	}
	raw, err := json.Marshal(stats)
	if err != nil {
		return "", fmt.Errorf("recommend-recall: marshal per_source: %w", err)
	}
	return string(raw), nil
}

// decodeSourceStats 反序列化逐路统计。空串/空数组都归一成 nil（无逐路明细）。
// 解析失败必须上抛：静默给"干净的没有逐路统计"的响应会让排障误判为未降级。
func decodeSourceStats(raw string) ([]*rpc.SourceStat, error) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "[]" {
		return nil, nil
	}
	var stats []*rpc.SourceStat
	if err := json.Unmarshal([]byte(raw), &stats); err != nil {
		return nil, fmt.Errorf("recommend-recall: unmarshal per_source: %w", err)
	}
	return stats, nil
}

// encodeSourcesCSV / decodeSourcesCSV 对应 requested_sources、dropped_sources 列
// （model.SourceListString 保证同集合恒定同串，可直接进 GROUP BY）。
func encodeSourcesCSV(sources []int32) string {
	return model.SourceListString(sources)
}

// decodeSourcesCSV 解析 model.SourceListString 的产物（升序去重、逗号分隔、无空格）。
// 这里手写 Split 而不用 encoding/csv：写侧是本服务自己渲染的纯数字串，
// 引入 CSV 解析器等于给同一份数据两套语法（引号/转义），反而扩大不一致面。
// 任一字段不合法都必须上抛：把脏数据当"没有请求任何路"会让审计回放读到假事实。
func decodeSourcesCSV(raw string) ([]int32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int32, 0, len(parts))
	for _, field := range parts {
		field = strings.TrimSpace(field)
		n, err := strconv.Atoi(field)
		if err != nil {
			return nil, fmt.Errorf("recommend-recall: source field %q in %q: %w", field, raw, err)
		}
		if !model.ValidSource(int32(n)) {
			return nil, fmt.Errorf("%w: %d", model.ErrInvalidSource, n)
		}
		out = append(out, int32(n))
	}
	return out, nil
}

// --- 池版本条目读取（在线召回与快照读共用）---

// poolEntry 是缓存里的一条候选（不落 aid 之外的业务字段：稿件数据归 video）。
type poolEntry struct {
	Aid   int64   `json:"aid"`
	Score float64 `json:"score"`
}

// topNCacheKey 把 (source, pool_key, version, limit) 编进键。
// 带 version 是关键：切版本后读新键，旧键随 TTL 过期，不需要任何失效动作。
func topNCacheKey(source int32, poolKey string, version int64, limit int) string {
	return topnCacheKeyPrefix + strconv.FormatInt(int64(source), 10) + "|" + poolKey + "|" +
		strconv.FormatInt(version, 10) + "|" + strconv.Itoa(limit)
}

// cacheGetTopN 读池 TopN 缓存。
// 返回 (entries, cacheUsable)：entries 为 nil 表示未命中（或缓存里的值不可信）；
// cacheUsable=false 表示缓存层此刻不可用（未配置或读写报错），调用方必须把它记成
// store_unavailable 降级，而不是当成"没命中所以回源 MySQL 就算正常"——
// 缓存整体不可用时在线召回对 MySQL 的压力才是这里要显式暴露的风险。
func cacheGetTopN(ctx context.Context, cache *redis.Redis, key string, limit int) ([]poolEntry, bool) {
	if cache == nil {
		return nil, false
	}
	raw, err := cache.GetCtx(ctx, key)
	if err != nil {
		if isRedisUnavailable(err) {
			return nil, false
		}
		// 其它 Redis 错误（如 WRONGTYPE）按未命中处理并回源：值本身可覆盖，
		// 下一次 SET 会把它纠正回来，不该因此把一整路召回判死。
		return nil, true
	}
	if raw == "" {
		return nil, true
	}
	var entries []poolEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, true
	}
	if len(entries) == 0 {
		// 空数组是"这个版本真的没有条目"的事实，但在线侧的空池同样可疑：
		// 交给调用方按 pool_not_ready/条目数判定，这里不当缓存污染。
		return entries, true
	}
	if len(entries) > limit {
		// 键里带 limit，正常不会超；超了说明键被写错，宁缺不滥。
		return nil, true
	}
	return entries, true
}

// cacheSetTopN 写池 TopN 缓存；ttl<=0 表示不缓存。
// 返回错误由调用方记日志：缓存只是加速器，真值在 MySQL，写失败不该改变本次响应，
// 但"一直写不进去"意味着全部压力都会落在 MySQL 上，必须能被看见。
func cacheSetTopN(ctx context.Context, cache *redis.Redis, key string, entries []poolEntry, ttl int64) error {
	if cache == nil || ttl <= 0 {
		return nil
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return fmt.Errorf("recommend-recall: marshal topn cache value: %w", err)
	}
	if err := cache.SetexCtx(ctx, key, string(raw), int(ttl)); err != nil {
		return fmt.Errorf("recommend-recall: set topn cache %s: %w", key, err)
	}
	return nil
}

// isRedisUnavailable 判定"缓存层整体不可用"（go-redis 的连接类错误与 nil client）。
func isRedisUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, redis.Nil) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "client is closed") ||
		strings.Contains(msg, "context deadline exceeded")
}

// --- 依赖错误分类（降级原因映射）---

// degradeKeyForDependencyErr 把依赖读取的错误映射成受控降级 key。
// 未接线的下游（repository.ErrSourceNotConfigured）与超时/熔断必须区分：
// 前者是"这条链路还没接"，后者是"接了但此刻不通"，运维动作完全不同。
func degradeKeyForDependencyErr(err error) string {
	switch {
	case err == nil:
		return model.DegradeReasonNone
	case errors.Is(err, repository.ErrSourceNotConfigured):
		return model.DegradeReasonFeatureUnavailable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled),
		errors.Is(err, sql.ErrConnDone):
		return model.DegradeReasonDownstreamTimeout
	case errors.Is(err, model.ErrPoolNotFound), errors.Is(err, model.ErrVersionNotFound):
		return model.DegradeReasonPoolNotReady
	default:
		return model.DegradeReasonStoreUnavailable
	}
}

// --- 事务会话取值 ---

// sessionOf 是留给未来"logic 直接组事务片段"的窄口子：本包不拼 SQL，
// 只把 model 需要 sqlx.Session 的方法接上 Repository.Transact 的回调参数。
func sessionOf(session sqlx.Session) sqlx.Session { return session }

// nowUnix 当前 Unix 秒（本服务全部时间列都是 BIGINT 秒）。
// 不在 logic 里做时区换算：迁移文件与 rpc 注释都按 UTC 秒约定。
func nowUnix() int64 { return time.Now().Unix() }

// --- 审计日志投影（GetRecallRequestLog 与 ListRecallRequestLogs 共用）---

// requestLogInfo 把审计行转成 rpc 结构。
//
// 逐路统计与召回路 csv 都是本服务自己写的受控文本，解析失败即数据被外部改过或写入侧有缺陷，
// 必须上抛而不是静默给一个"没有逐路统计"的干净响应（那会把降级读成正常）。
// 输出只含受控字段：本表永不存明文设备号/IP（写入侧只允许 sha256 摘要），
// 因此这里不需要额外脱敏，但也不得新增回显原文的字段。
func requestLogInfo(l *model.RecallRequestLog) (*rpc.RecallRequestLogInfo, error) {
	if l == nil {
		return nil, fmt.Errorf("%w: request log row is nil", model.ErrRequestNotFound)
	}
	perSource, err := decodeSourceStats(l.PerSource)
	if err != nil {
		return nil, err
	}
	requested, err := decodeSourcesCSV(l.RequestedSources)
	if err != nil {
		return nil, err
	}
	degraded := l.Degraded != 0
	reason := rpc.DegradeReason_DEGRADE_REASON_UNSPECIFIED
	if degraded {
		reason = degradeReasonFromKey(l.DegradeReason)
	}
	return &rpc.RecallRequestLogInfo{
		RequestId:        l.RequestID,
		SnapshotId:       l.SnapshotID,
		Mid:              l.Mid,
		Scene:            l.Scene,
		Platform:         toRPCPlatform(l.Platform),
		AppVersion:       l.AppVersion,
		Region:           l.Region,
		RequestedSources: rpcSourceList(requested),
		PerSource:        perSource,
		CandidateCount:   l.CandidateCount,
		ReturnedCount:    l.ReturnedCount,
		Degraded:         degraded,
		Reason:           reason,
		CostMs:           l.CostMs,
		VersionsDigest:   l.VersionsDigest,
		TraceId:          l.TraceID,
		Ctime:            l.Ctime,
	}, nil
}
