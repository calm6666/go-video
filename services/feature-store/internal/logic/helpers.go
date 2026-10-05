// 本文件是 logic 包的手写扩展（入参校验、回执接线、缓存读写、proto↔model 投影、
// 以及 GetFeature / BatchGetFeatures / ListEntityFeatures 共用的降级读路径），
// 不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：这里只做「编排与投影」——校验、版本解析、降级判定、
// 缓存键与响应装配；SQL、CAS 与事务边界一律留在 model。
// 三条不变量在本文件收口：
//  1. 每一次读都必须说明「服务的是哪个版本、是不是降级」（ClassifyDegradation 一处判定）；
//  2. 缓存/DB 不可用必须显式标成 SOURCE_UNAVAILABLE，绝不返回 0 值冒充真实特征；
//  3. 明文 PII 不进日志与响应：日志只打 feature_key/version/scope 与计数，不打 entity_id 与值原文。

package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/trace"
)

// 列宽约束（与 deploy/migrations/feature-store/*.sql 一致）。
// 校验发生在 logic：model 只保证「不合法就别写」，logic 保证「别把不合法的请求带进事务」。
const (
	// maxOperatorLen 对应各表 operator/created_by/written_by VARCHAR(64)。
	maxOperatorLen = 64
	// maxReasonLen 对应 feature_version_switch.reason / feature_backfill_job.reason
	// 的 VARCHAR(512)（留 12 字节余量给多字节边界的保守裁剪）。
	maxReasonLen = 500
	// maxRequestIDLen 对应 feature_write_receipt.request_id / feature_backfill_job.request_id
	// 的 VARCHAR(64)，与 model.maxReceiptRequestIDLen 同口径。
	maxRequestIDLen = 64
	// maxTraceIDLen 对应各表 trace_id VARCHAR(64)。
	maxTraceIDLen = 64
	// cacheMgetChunk 一次 MGET 的键数：条目上限本身有界（<=2000），
	// 分块只是避免单条 Redis 命令过长被代理层截断。
	cacheMgetChunk = 200
	// maxSourceMetricKeyLen 对应 feature_value.source_metric_key VARCHAR(128)。
	maxSourceMetricKeyLen = 128
)

// --- 通用入参校验 ---

// checkRequestID 校验写接口幂等键。空键一律拒绝：没有幂等键的写重放就是两次副作用。
func checkRequestID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ErrRequestIdRequired
	}
	if len(id) > maxRequestIDLen {
		return fmt.Errorf("%w: request_id longer than %d", model.ErrRequestIdRequired, maxRequestIDLen)
	}
	return nil
}

// checkOperator 校验操作人身份：无主变更不可审计。
func checkOperator(op string) error {
	if strings.TrimSpace(op) == "" {
		return model.ErrOperatorRequired
	}
	if len(op) > maxOperatorLen {
		return fmt.Errorf("%w: operator longer than %d", model.ErrOperatorRequired, maxOperatorLen)
	}
	return nil
}

// checkReason 校验变更理由（状态/隐私/切换/回填都必填）。
func checkReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return model.ErrReasonRequired
	}
	if len(reason) > maxReasonLen {
		return fmt.Errorf("%w: reason longer than %d", model.ErrReasonRequired, maxReasonLen)
	}
	return nil
}

// checkFeatureKey 校验 feature_key 形态与范围（范围外语义在 model 一处判定）。
func checkFeatureKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return model.ErrFeatureKeyRequired
	}
	if !model.ValidFeatureKey(key) {
		return fmt.Errorf("%w: %s", model.ErrFeatureKeyRequired, key)
	}
	if model.ForbiddenFeatureKey(key) {
		return model.ErrFeatureKeyForbidden
	}
	return nil
}

// checkVersion 要求「显式版本」的接口用这一条（写入与切换不允许按 ACTIVE 指针悄悄落版）。
func checkVersion(v int32) error {
	if v < 1 {
		return model.ErrFeatureVersionRequired
	}
	return nil
}

// checkEntity 校验主体引用，返回归一化后的 (scope, entity_id)。
// 这里是隐私的第一道闸：DEVICE/IP_HASH 只接受十六进制摘要，
// 任何非 hex 形态（明文设备号、原始 IP）在进 SQL 之前就被拒掉。
func checkEntity(e *rpc.EntityRef) (int32, string, error) {
	if e == nil {
		return 0, "", model.ErrEntityScopeRequired
	}
	scope := int32(e.GetEntityScope())
	id := strings.TrimSpace(e.GetEntityId())
	if !model.ValidEntityScope(scope) {
		return 0, "", model.ErrEntityScopeRequired
	}
	if !model.ValidEntityID(scope, id) {
		// 不回显 entity_id：它就是个体标识本身，错误信息会进日志与客户端。
		return 0, "", model.ErrEntityIDInvalid
	}
	return scope, id, nil
}

// clip 按字节上限裁剪（列宽是字节数，按 rune 数裁会写出超宽值）。
func clip(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for len(s) > maxBytes {
		_, cut := utf8LastRune(s)
		if cut == 0 {
			return ""
		}
		s = s[:cut]
	}
	return s
}

func utf8LastRune(s string) (string, int) {
	for i := len(s) - 1; i > 0 && i >= len(s)-4; i-- {
		if s[i]&0xC0 != 0x80 {
			return s[i:], i
		}
	}
	return s, 0
}

// traceIDOf 取链路 ID 并按列宽裁剪；取不到时留空串（trace_id 不参与幂等摘要）。
func traceIDOf(ctx context.Context) string {
	return clip(trace.TraceIDFromContext(ctx), maxTraceIDLen)
}

// --- 枚举与投影转换（proto ↔ model 的唯一出入口）---

func scopeEnum(s int32) rpc.EntityScope              { return rpc.EntityScope(s) }
func valueTypeEnum(t int32) rpc.FeatureValueType     { return rpc.FeatureValueType(t) }
func sourceEnum(s int32) rpc.FeatureSource           { return rpc.FeatureSource(s) }
func privacyEnum(p int32) rpc.PrivacyLevel           { return rpc.PrivacyLevel(p) }
func stateEnum(s int32) rpc.FeatureState             { return rpc.FeatureState(s) }
func degradationEnum(d int32) rpc.FeatureDegradation { return rpc.FeatureDegradation(d) }
func backfillEnum(s int32) rpc.BackfillState         { return rpc.BackfillState(s) }

func featureRef(key string, version int32) *rpc.FeatureRef {
	return &rpc.FeatureRef{FeatureKey: key, Version: version}
}

func entityRef(scope int32, id string) *rpc.EntityRef {
	return &rpc.EntityRef{EntityScope: scopeEnum(scope), EntityId: id}
}

// defToProto 投影一条特征定义。只暴露定义列本身，不回带任何值或主体标识。
func defToProto(d *model.FeatureDefinition) *rpc.FeatureDefinition {
	if d == nil {
		return nil
	}
	return &rpc.FeatureDefinition{
		FeatureKey:    d.FeatureKey,
		Version:       d.Version,
		Name:          d.Name,
		ValueType:     valueTypeEnum(d.ValueType),
		EntityScope:   scopeEnum(d.EntityScope),
		Source:        sourceEnum(d.Source),
		PrivacyLevel:  privacyEnum(d.PrivacyLevel),
		WindowSeconds: d.WindowSeconds,
		TtlSeconds:    d.TTLSeconds,
		DefaultValue:  d.DefaultValue,
		Dimension:     d.Dimension,
		State:         stateEnum(d.State),
		Description:   d.Description,
		ChangeNote:    d.ChangeNote,
		CreatedBy:     d.CreatedBy,
		Ctime:         d.Ctime,
		Mtime:         d.Mtime,
	}
}

// backfillToProto 投影一条回填作业。
// 契约里没有 entity_ids 字段，因此主体列表不外泄；last_error 已由 model 截断，
// 不含 SQL 与特征值原文。
func backfillToProto(j *model.BackfillJob) *rpc.BackfillJob {
	if j == nil {
		return nil
	}
	return &rpc.BackfillJob{
		JobId:          j.JobID,
		FeatureKey:     j.FeatureKey,
		Version:        j.Version,
		EntityScope:    scopeEnum(j.EntityScope),
		Source:         sourceEnum(j.Source),
		State:          backfillEnum(j.State),
		WindowFrom:     j.WindowFrom,
		WindowTo:       j.WindowTo,
		EntitiesTotal:  j.EntitiesTotal,
		EntitiesDone:   j.EntitiesDone,
		EntitiesFailed: j.EntitiesFailed,
		CursorEntityId: j.CursorEntityID,
		AutoSwitch:     j.AutoSwitch,
		RequestId:      j.RequestID,
		Operator:       j.Operator,
		Reason:         j.Reason,
		LastError:      j.LastError,
		Ctime:          j.Ctime,
		Mtime:          j.Mtime,
		FinishedAt:     j.FinishedAt,
	}
}

// payloadFromProto 把契约值转成中立载荷。填充是否自洽由 model.EncodeValue 判定，
// 这里不做「顺手补齐」：漏填与错填必须是调用方的错误，而不是服务端给个 0。
func payloadFromProto(v *rpc.FeatureValue) (model.ValuePayload, error) {
	if v == nil {
		return model.ValuePayload{}, model.ErrMalformedValue
	}
	vt := int32(v.GetValueType())
	if !model.ValidValueType(vt) {
		return model.ValuePayload{}, model.ErrValueTypeInvalid
	}
	return model.ValuePayload{
		ValueType:   vt,
		Int64Value:  v.GetInt64Value(),
		DoubleValue: v.GetDoubleValue(),
		BoolValue:   v.GetBoolValue(),
		StringValue: v.GetStringValue(),
		Int64List:   v.GetInt64List(),
		DoubleList:  v.GetDoubleList(),
	}, nil
}

// payloadToProto 把中立载荷编回契约值。未知类型报错而不是返回空值：
// 空 FeatureValue 会被调用方读成「值为 0/空串」，那是最坏的一种伪装成功。
func payloadToProto(p model.ValuePayload) (*rpc.FeatureValue, error) {
	switch p.ValueType {
	case model.ValueTypeInt64:
		return &rpc.FeatureValue{ValueType: valueTypeEnum(p.ValueType), Int64Value: p.Int64Value}, nil
	case model.ValueTypeDouble:
		return &rpc.FeatureValue{ValueType: valueTypeEnum(p.ValueType), DoubleValue: p.DoubleValue}, nil
	case model.ValueTypeBool:
		return &rpc.FeatureValue{ValueType: valueTypeEnum(p.ValueType), BoolValue: p.BoolValue}, nil
	case model.ValueTypeString:
		return &rpc.FeatureValue{ValueType: valueTypeEnum(p.ValueType), StringValue: p.StringValue}, nil
	case model.ValueTypeInt64List:
		return &rpc.FeatureValue{ValueType: valueTypeEnum(p.ValueType), Int64List: p.Int64List}, nil
	case model.ValueTypeDoubleList:
		return &rpc.FeatureValue{ValueType: valueTypeEnum(p.ValueType), DoubleList: p.DoubleList}, nil
	default:
		return nil, model.ErrValueTypeInvalid
	}
}

// --- 错误短码 ---

// errorCodeOf 把哨兵错误映射成可枚举的短码：写进逐行结果与回执 error_code。
// 响应与库里都不出现 SQL 片段、值原文或标识符明文（AGENTS.md §6）。
func errorCodeOf(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, model.ErrFeatureNotFound):
		return "FEATURE_NOT_FOUND"
	case errors.Is(err, model.ErrFeatureNotActive):
		return "FEATURE_NOT_ACTIVE"
	case errors.Is(err, model.ErrFeatureRetired):
		return "FEATURE_RETIRED"
	case errors.Is(err, model.ErrNoActiveVersion):
		return "NO_ACTIVE_VERSION"
	case errors.Is(err, model.ErrEntityScopeMismatch):
		return "ENTITY_SCOPE_MISMATCH"
	case errors.Is(err, model.ErrEntityIDInvalid):
		return "ENTITY_ID_INVALID"
	case errors.Is(err, model.ErrEntityScopeRequired):
		return "ENTITY_SCOPE_REQUIRED"
	case errors.Is(err, model.ErrValueTypeInvalid):
		return "VALUE_TYPE_INVALID"
	case errors.Is(err, model.ErrMalformedValue):
		return "MALFORMED_VALUE"
	case errors.Is(err, model.ErrMalformedListValue):
		return "MALFORMED_LIST_VALUE"
	case errors.Is(err, model.ErrDimensionExceeded):
		return "DIMENSION_EXCEEDED"
	case errors.Is(err, model.ErrSourceMismatch):
		return "SOURCE_MISMATCH"
	case errors.Is(err, model.ErrStaleEventTime):
		return "STALE_EVENT_TIME"
	case errors.Is(err, model.ErrRowDisappeared):
		return "ROW_DISAPPEARED"
	case errors.Is(err, model.ErrDuplicateRowInBatch):
		return "DUPLICATE_ROW_IN_BATCH"
	case errors.Is(err, model.ErrTooManyRows):
		return "TOO_MANY_ROWS"
	case errors.Is(err, model.ErrTooManyEntries):
		return "TOO_MANY_ENTRIES"
	case errors.Is(err, model.ErrTooManyEntities):
		return "TOO_MANY_ENTITIES"
	case errors.Is(err, model.ErrLimitTooLarge):
		return "LIMIT_TOO_LARGE"
	case errors.Is(err, model.ErrRequestIdRequired):
		return "REQUEST_ID_REQUIRED"
	case errors.Is(err, model.ErrOperatorRequired):
		return "OPERATOR_REQUIRED"
	case errors.Is(err, model.ErrReasonRequired):
		return "REASON_REQUIRED"
	case errors.Is(err, model.ErrRequestIdReused):
		return "REQUEST_ID_REUSED"
	case errors.Is(err, model.ErrFeatureDefinitionImmutable):
		return "DEFINITION_IMMUTABLE"
	case errors.Is(err, model.ErrFeatureMetadataImmutable):
		return "VERSION_METADATA_IMMUTABLE"
	case errors.Is(err, model.ErrFeatureStateTransition):
		return "STATE_TRANSITION"
	case errors.Is(err, model.ErrVersionConflict):
		return "VERSION_CONFLICT"
	case errors.Is(err, model.ErrImmutableFieldMismatch):
		return "IMMUTABLE_FIELD_MISMATCH"
	case errors.Is(err, model.ErrMultipleActiveVersions):
		return "MULTIPLE_ACTIVE_VERSIONS"
	case errors.Is(err, model.ErrSwitchNotFound):
		return "SWITCH_NOT_FOUND"
	case errors.Is(err, model.ErrBackfillTargetNotDraft):
		return "BACKFILL_TARGET_NOT_DRAFT"
	case errors.Is(err, model.ErrBackfillSourceMismatch):
		return "BACKFILL_SOURCE_MISMATCH"
	case errors.Is(err, model.ErrBackfillWindowInvalid):
		return "BACKFILL_WINDOW_INVALID"
	case errors.Is(err, model.ErrBackfillEntityIDsTooLarge):
		return "BACKFILL_ENTITY_IDS_TOO_LARGE"
	case errors.Is(err, model.ErrJobNotFound):
		return "JOB_NOT_FOUND"
	case errors.Is(err, model.ErrJobStateInvalid):
		return "JOB_STATE_INVALID"
	case errors.Is(err, model.ErrCutoffRequired):
		return "CUTOFF_REQUIRED"
	case errors.Is(err, model.ErrPrivacyScopeMismatch):
		return "PRIVACY_SCOPE_MISMATCH"
	case errors.Is(err, model.ErrPrivacyUnsetNotAllowed):
		return "PRIVACY_UNSET"
	case errors.Is(err, model.ErrPrivacyOperatorForbidden):
		return "PRIVACY_OPERATOR_FORBIDDEN"
	case errors.Is(err, model.ErrDefaultValueInvalid):
		return "DEFAULT_VALUE_INVALID"
	case errors.Is(err, model.ErrReceiptInProgress):
		return "RECEIPT_IN_PROGRESS"
	default:
		return "INTERNAL"
	}
}

// --- 幂等回执接线 ---

// newLeaseOwner 生成执行权持有者标识：<服务>:<operator>#<纳秒>。
// 纳秒是必须的：同一个 operator 用同一个 request_id 并发重试两次时，
// 只能有一个持有租约，MarkDone 的 lease_owner 条件更新才有判别力。
func newLeaseOwner(operator string) string {
	return clip(fmt.Sprintf("feature-store:%s#%d", clip(strings.TrimSpace(operator), 40),
		time.Now().UnixNano()), 128)
}

// receiptSpec 是一次写操作的回执定位信息（request_id + op_type 唯一）。
// reason 不落回执表（回执表没有理由列，理由落在 feature_version_switch.reason），
// 放在一起只是为了让事务内的审计行与回执共用同一份入参，不必在每个方法里多传一个串。
type receiptSpec struct {
	requestID   string
	opType      string
	featureKey  string
	version     int32
	entityScope int32
	entityID    string
	rowCount    int32
	operator    string
	reason      string
}

// beginReceipt 取得执行权。返回 execute=false 时调用方必须回放，绝不重复副作用。
func beginReceipt(ctx context.Context, svcCtx *svc.ServiceContext, l logx.Logger,
	spec receiptSpec) (model.ReceiptBeginResult, string, error) {
	owner := newLeaseOwner(spec.operator)
	res, err := svcCtx.Receipts.Begin(ctx, &model.WriteReceipt{
		RequestID:   strings.TrimSpace(spec.requestID),
		OpType:      spec.opType,
		FeatureKey:  spec.featureKey,
		Version:     spec.version,
		EntityScope: spec.entityScope,
		EntityID:    spec.entityID,
		RowCount:    spec.rowCount,
		Operator:    clip(strings.TrimSpace(spec.operator), maxOperatorLen),
		TraceID:     traceIDOf(ctx),
		LeaseOwner:  owner,
	}, svcCtx.ReceiptLeaseSeconds())
	if err != nil {
		return res, "", err
	}
	if !res.Execute {
		l.Infow("receipt replay", logx.Field("op", spec.opType),
			logx.Field("request_id", spec.requestID))
	}
	return res, owner, nil
}

// finishReceipt 收尾为 done。MarkDone 返回 false 表示执行权已被接管：
// 此时副作用可能已发生，必须报「结果未知」而不是谎报成功。
func finishReceipt(ctx context.Context, svcCtx *svc.ServiceContext, l logx.Logger,
	spec receiptSpec, owner string, snap model.ReceiptSnapshot) error {
	done, err := svcCtx.Receipts.MarkDone(ctx, spec.requestID, spec.opType, owner, snap)
	if err != nil {
		return err
	}
	if !done {
		l.Errorw("receipt lease lost", logx.Field("op", spec.opType),
			logx.Field("request_id", spec.requestID))
		return fmt.Errorf("%w: idempotency lease was taken over, retry with a new request_id",
			model.ErrReceiptInProgress)
	}
	return nil
}

// failReceipt 收尾为 failed（不掩盖原始错误）。
func failReceipt(ctx context.Context, svcCtx *svc.ServiceContext, l logx.Logger,
	spec receiptSpec, owner string, cause error) {
	if _, err := svcCtx.Receipts.MarkFailed(ctx, spec.requestID, spec.opType, owner,
		errorCodeOf(cause)); err != nil {
		l.Errorw("receipt MarkFailed", logx.Field("op", spec.opType), logx.Field("err", err.Error()))
	}
}

// --- 缓存 ---

// cacheGetJSON 读一条 JSON 缓存；返回 (hit, decoded?, err)。
// err 非空表示缓存层不可用（不是「没命中」），调用方据此决定降级口径。
func cacheGetJSON(ctx context.Context, cache *redis.Redis, key string, out any) (bool, error) {
	if cache == nil {
		return false, nil
	}
	raw, err := cache.GetCtx(ctx, key)
	if err != nil {
		if errors.Is(err, redis.Nil) || raw == "" {
			return false, nil
		}
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		// 脏缓存按 miss 处理：它一定能在 DB 侧被重算出来，不能让一条坏值永久生效。
		return false, nil
	}
	return true, nil
}

func cacheSetJSON(ctx context.Context, cache *redis.Redis, l logx.Logger, key string,
	val any, ttlSeconds int64) {
	if cache == nil || ttlSeconds <= 0 {
		return
	}
	raw, err := json.Marshal(val)
	if err != nil {
		l.Errorw("cache marshal", logx.Field("err", err.Error()))
		return
	}
	if err := cache.SetexCtx(ctx, key, string(raw), int(ttlSeconds)); err != nil {
		l.Errorw("cache setex", logx.Field("key", key), logx.Field("err", err.Error()))
	}
}

// invalidateCache 在事务提交后删除命中的键（DEL 而非更新：半新半旧的缓存比空缓存更难解释）。
// 删除失败只记日志：DB 是回源与重放依据，缓存里的残留值最长活到自己的 TTL。
func invalidateCache(ctx context.Context, svcCtx *svc.ServiceContext, l logx.Logger, keys []string) {
	if svcCtx.Cache == nil || len(keys) == 0 {
		return
	}
	for start := 0; start < len(keys); start += cacheMgetChunk {
		end := min(start+cacheMgetChunk, len(keys))
		if _, err := svcCtx.Cache.DelCtx(ctx, keys[start:end]...); err != nil {
			l.Errorw("cache del", logx.Field("count", end-start), logx.Field("err", err.Error()))
		}
	}
}

// chunkKeys 把待取键按 DB 回源上限切块（FindEntries 自带条数上限，
// 一次读请求最多「主版本 + 上一版本」两套键）。
func chunkKeys[T any](in []T, size int) [][]T {
	if size <= 0 {
		size = model.MaxBatchReadEntries
	}
	out := make([][]T, 0, (len(in)+size-1)/size)
	for start := 0; start < len(in); start += size {
		end := min(start+size, len(in))
		out = append(out, in[start:end])
	}
	return out
}

// --- ACTIVE 版本指针（缓存 + 回源）---

// activePointer 是 fs:active:<key> 的缓存形态。
// 只存两个版本号：把定义或值塞进指针键会让「切了版本在线还读旧版」的窗口被放大。
type activePointer struct {
	Active   int32 `json:"a"`
	Previous int32 `json:"p"`
}

// resolvePointers 批量解析 ACTIVE 指针。DB 错误直接上抛：
// 指针解析不出来就无法说明「服务的是哪个版本」，按契约只能是失败而不是猜一个。
func (r *featureReader) resolvePointers(keys []string) (map[string]activePointer, error) {
	out := make(map[string]activePointer, len(keys))
	var miss []string
	for _, k := range keys {
		var p activePointer
		hit, err := cacheGetJSON(r.ctx, r.svcCtx.Cache, model.ActiveVersionCacheKey(k), &p)
		if err != nil {
			// 指针缓存不可用不等于读失败：回源 DB 即可，只是多一次点查。
			r.Errorw("active pointer cache", logx.Field("err", err.Error()))
		} else if hit {
			out[k] = p
			continue
		}
		miss = append(miss, k)
	}
	if len(miss) == 0 {
		return out, nil
	}
	// 指针表是「同一时刻只有一个生效版本」的事实源，批量一次取回。
	rows, err := r.svcCtx.ActiveVersions.ListByKeys(r.ctx, miss)
	if err != nil {
		return nil, fmt.Errorf("feature-store: resolve active pointers: %w", err)
	}
	ttl := r.svcCtx.Config.Read.ActivePointerCacheSeconds
	for _, k := range miss {
		p := activePointer{}
		if av := rows[k]; av != nil {
			p = activePointer{Active: av.ActiveVersion, Previous: av.PreviousVersion}
			out[k] = p
			cacheSetJSON(r.ctx, r.svcCtx.Cache, r.Logger, model.ActiveVersionCacheKey(k), p, ttl)
			continue
		}
		// 指针行不存在：注册流程未走完。缓存里留一个空指针会让「已注册」与「未注册」
		// 在 ActivePointerCacheSeconds 内无法区分，因此不写缓存，由调用方按缺失处理。
		out[k] = p
	}
	return out, nil
}

// --- 降级读路径（GetFeature / BatchGetFeatures / ListEntityFeatures 共用）---

// readTarget 是一条「已解析到具体版本」的读请求。
type readTarget struct {
	def         *model.FeatureDefinition
	version     int32 // 解析后的具体版本（请求 version=0 时已按指针替换）
	prev        int32 // 指针里的上一生效版本，0 = 没有可降级版本
	entityScope int32
	entityID    string
	// scopeOK false = 请求的主体维度与定义不符：不越权读别的维度，直接按默认值降级。
	scopeOK bool
}

func (t readTarget) key(version int32) model.ValueKey {
	return model.ValueKey{
		FeatureKey:  t.def.FeatureKey,
		Version:     version,
		EntityScope: t.def.EntityScope,
		EntityID:    t.entityID,
	}
}

// featureReader 承载一次读请求的上下文与参数。
type featureReader struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
	allowStale bool
	now        int64
}

func newFeatureReader(ctx context.Context, svcCtx *svc.ServiceContext, l logx.Logger,
	allowStale bool) *featureReader {
	return &featureReader{ctx: ctx, svcCtx: svcCtx, Logger: l, allowStale: allowStale,
		now: model.NowUnix()}
}

// cachedValue 是 fs:val:* 的缓存形态：与 feature_value 的值列同构，
// 不含 entity_id（键里已有），因此缓存内容不额外引入主体标识。
type cachedValue struct {
	Version         int32   `json:"ver"`
	ValueType       int32   `json:"t"`
	Int64Value      int64   `json:"i,omitempty"`
	DoubleValue     float64 `json:"d,omitempty"`
	BoolValue       int32   `json:"b,omitempty"`
	StringValue     string  `json:"s,omitempty"`
	ListValues      string  `json:"l,omitempty"`
	EventTime       int64   `json:"et"`
	ExpireAt        int64   `json:"ea"`
	SourceMetricKey string  `json:"smk,omitempty"`
}

func cachedValueOf(v *model.FeatureValue) cachedValue {
	return cachedValue{
		Version:         v.Version,
		ValueType:       v.ValueType,
		Int64Value:      v.Int64Value,
		DoubleValue:     v.DoubleValue,
		BoolValue:       v.BoolValue,
		StringValue:     v.StringValue,
		ListValues:      v.ListValues,
		EventTime:       v.EventTime,
		ExpireAt:        v.ExpireAt,
		SourceMetricKey: v.SourceMetricKey,
	}
}

// toModelValue 把缓存条目还原成与 DB 行同构的对象，供后面的新鲜度判定与解码复用同一段代码。
func (c cachedValue) toModelValue(t readTarget, version int32) *model.FeatureValue {
	return &model.FeatureValue{
		FeatureKey:      t.def.FeatureKey,
		Version:         version,
		EntityScope:     t.def.EntityScope,
		EntityID:        t.entityID,
		ValueType:       c.ValueType,
		Int64Value:      c.Int64Value,
		DoubleValue:     c.DoubleValue,
		BoolValue:       c.BoolValue,
		StringValue:     c.StringValue,
		ListValues:      c.ListValues,
		EventTime:       c.EventTime,
		ExpireAt:        c.ExpireAt,
		SourceMetricKey: c.SourceMetricKey,
	}
}

// loadValues 取一批键的值：Redis 主读 + miss 回源 feature_value。
// 第二个返回值是「值来源是否可用」：只有 DB 回源失败（或两侧都拿不到）才为 false，
// 因为 SOURCE_UNAVAILABLE 与「这个主体就是没有值」在调用方那边是两种处置
// （前者退避重试、后者冷启动），误判会让排序侧对冷启动做无谓重试。
func (r *featureReader) loadValues(keys []model.ValueKey) (map[model.ValueKey]*model.FeatureValue, bool) {
	vals := make(map[model.ValueKey]*model.FeatureValue, len(keys))
	unique := make([]model.ValueKey, 0, len(keys))
	seen := make(map[model.ValueKey]struct{}, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		unique = append(unique, k)
	}

	var pending []model.ValueKey
	if r.svcCtx.Cache != nil {
		for _, chunk := range chunkKeys(unique, cacheMgetChunk) {
			cacheKeys := make([]string, 0, len(chunk))
			for _, k := range chunk {
				cacheKeys = append(cacheKeys, k.CacheKey())
			}
			raws, err := r.svcCtx.Cache.MgetCtx(r.ctx, cacheKeys...)
			if err != nil {
				// 缓存故障不判「整体不可用」：还有 DB 兜底，只是这一次读更贵。
				r.Errorw("feature value mget", logx.Field("count", len(cacheKeys)),
					logx.Field("err", err.Error()))
				pending = append(pending, chunk...)
				continue
			}
			for i, k := range chunk {
				if i >= len(raws) || raws[i] == "" {
					pending = append(pending, k)
					continue
				}
				var cv cachedValue
				if err := json.Unmarshal([]byte(raws[i]), &cv); err != nil {
					r.Errorw("feature value cache decode", logx.Field("feature_key", k.FeatureKey))
					pending = append(pending, k)
					continue
				}
				vals[k] = &model.FeatureValue{
					FeatureKey: k.FeatureKey, Version: k.Version, EntityScope: k.EntityScope,
					EntityID: k.EntityID, ValueType: cv.ValueType, Int64Value: cv.Int64Value,
					DoubleValue: cv.DoubleValue, BoolValue: cv.BoolValue, StringValue: cv.StringValue,
					ListValues: cv.ListValues, EventTime: cv.EventTime, ExpireAt: cv.ExpireAt,
					SourceMetricKey: cv.SourceMetricKey,
				}
			}
		}
	} else {
		pending = append(pending, unique...)
	}

	if len(pending) == 0 {
		return vals, true
	}
	if !r.svcCtx.Config.Read.DBFallbackEnabled {
		// 演练开关：明确不回源，miss 就是 miss（降级为 DEFAULT_VALUE / PREVIOUS_VERSION）。
		return vals, r.svcCtx.Cache != nil
	}
	for _, chunk := range chunkKeys(pending, model.MaxBatchReadEntries) {
		dbVals, err := r.svcCtx.Values.FindEntries(r.ctx, chunk)
		if err != nil {
			r.Errorw("feature value fallback", logx.Field("count", len(chunk)),
				logx.Field("err", err.Error()))
			// DB 也拿不到 = 两条路径都不可用，必须标 SOURCE_UNAVAILABLE 而不是默认值。
			return vals, false
		}
		for k, v := range dbVals {
			if v == nil {
				continue
			}
			vals[k] = v
			r.fillCache(v, r.svcCtx.Config.Read.CacheJitterRatio)
		}
	}
	return vals, true
}

// fillCache 把回源到的行写回主读键：TTL 用「库里 expire_at 的剩余寿命」而不是定义的
// ttl_seconds —— 一条 23 小时前产出、TTL 24h 的值只该再被缓存 1h。
// 再按 Read.CacheJitterRatio 向下抖动（model.CacheTTL），因为缓存比库里的 expire_at
// 活得更久等于把旧值的可见窗口偷偷延长。
func (r *featureReader) fillCache(v *model.FeatureValue, jitterRatio float64) {
	remaining := v.ExpireAt - r.now
	if remaining <= 0 {
		return // 过期值不进缓存：缓存的意义是加速新鲜读，不是延长旧值可见期。
	}
	ttl := model.CacheTTL(remaining, jitterRatio, rand.Float64())
	cacheSetJSON(r.ctx, r.svcCtx.Cache, r.Logger, v.Key().CacheKey(), cachedValueOf(v), ttl)
}

// entryFor 按降级矩阵把一次读装配成对外条目（矩阵在 model.ClassifyDegradation 一处）。
func (r *featureReader) entryFor(t readTarget, vals map[model.ValueKey]*model.FeatureValue,
	sourceAvailable bool) (*rpc.FeatureEntry, bool, error) {
	if !t.scopeOK {
		// 不越权读别的主体维度：定义说不存在的维度，就没有可信的值可给。
		return r.degradedEntry(t, model.DegradationDefaultValue)
	}
	if t.def.State == model.FeatureStateRetired {
		return r.degradedEntry(t, model.DegradationFeatureRetired)
	}
	primary, found := vals[t.key(t.version)]
	expired := found && primary.IsExpired(r.now)

	prevAvailable := false
	var prev *model.FeatureValue
	if t.prev > 0 {
		if p, ok := vals[t.key(t.prev)]; ok && p != nil && !p.IsExpired(r.now) {
			prev, prevAvailable = p, true
		}
	}
	deg, _, err := model.ClassifyDegradation(model.ReadOutcome{
		DefinitionFound:          true,
		State:                    t.def.State,
		ResolvedVersion:          t.version,
		ValueFound:               found,
		Expired:                  expired,
		AllowStale:               r.allowStale,
		PreviousVersionAvailable: prevAvailable,
		SourceAvailable:          sourceAvailable,
	})
	if err != nil {
		return nil, false, err
	}
	switch deg {
	case model.DegradationNone, model.DegradationExpired:
		entry, err := valueEntry(t, primary, deg, primary.Version)
		if err != nil {
			return nil, false, err
		}
		return entry, true, nil
	case model.DegradationPreviousVersion:
		entry, err := valueEntry(t, prev, deg, prev.Version)
		if err != nil {
			return nil, false, err
		}
		return entry, true, nil
	default:
		return r.degradedEntry(t, deg)
	}
}

// degradedEntry 用定义的 default_value 组一条降级条目。
// 默认值不可解析说明数据坏了，这里报错而不是给 0 值（ErrDefaultValueInvalid）。
func (r *featureReader) degradedEntry(t readTarget, deg int32) (*rpc.FeatureEntry, bool, error) {
	p, err := model.DefaultValuePayload(t.def)
	if err != nil {
		return nil, false, fmt.Errorf("feature-store: %s: %w", t.def.FeatureKey, err)
	}
	v, err := payloadToProto(p)
	if err != nil {
		return nil, false, err
	}
	found := deg == model.DegradationPreviousVersion || deg == model.DegradationExpired
	return &rpc.FeatureEntry{
		Feature:         featureRef(t.def.FeatureKey, t.version),
		Entity:          entityRef(t.entityScope, t.entityID),
		Value:           v,
		ResolvedVersion: t.version,
		Degradation:     degradationEnum(deg),
		TtlSeconds:      t.def.TTLSeconds,
	}, found, nil
}

// valueEntry 用真实值组条目：版本、产出时间、到期时间与上游口径键全部带出，
// 让调用方能自行判断新鲜度（读到的到底是哪一版，必须由响应说明而不是靠约定）。
// 值解码失败必须报错：库里这一行是脏数据时，返回一个「类型对得上的 0 值」
// 会被排序侧当成真实特征使用（model.FeatureValue.Payload 的同一条纪律）。
func valueEntry(t readTarget, v *model.FeatureValue, deg int32,
	resolved int32) (*rpc.FeatureEntry, error) {
	p, err := v.Payload()
	if err != nil {
		return nil, fmt.Errorf("feature-store: %s@v%d: %w", t.def.FeatureKey, resolved, err)
	}
	value, err := payloadToProto(p)
	if err != nil {
		return nil, fmt.Errorf("feature-store: %s@v%d: %w", t.def.FeatureKey, resolved, err)
	}
	return &rpc.FeatureEntry{
		Feature:         featureRef(t.def.FeatureKey, resolved),
		Entity:          entityRef(t.entityScope, t.entityID),
		Value:           value,
		ResolvedVersion: resolved,
		Degradation:     degradationEnum(deg),
		EventTime:       v.EventTime,
		ExpireAt:        v.ExpireAt,
		SourceMetricKey: v.SourceMetricKey,
		TtlSeconds:      t.def.TTLSeconds,
	}, nil
}

// isDegraded 判定条目是否为降级（NONE 之外都算）。
func isDegraded(e *rpc.FeatureEntry) bool {
	return e.GetDegradation() != rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE
}

// --- 定义加载与目标装配 ---

// buildTargets 把 (refs × entities) 组装成读目标：
// 先解析 version=0 的 ACTIVE 指针，再批量取定义，缺失定义按哨兵错误失败（无定义就没有默认值，
// 返回任何值都是伪造）。
func (r *featureReader) buildTargets(refs []*rpc.FeatureRef,
	entities []*rpc.EntityRef) ([]readTarget, error) {
	if len(refs) == 0 || len(entities) == 0 {
		return nil, model.ErrFeatureKeyRequired
	}
	// 需要解析指针的 key 去重后一次批量取。
	var needPointer []string
	seenKey := make(map[string]struct{}, len(refs))
	for _, f := range refs {
		key := strings.TrimSpace(f.GetFeatureKey())
		if err := checkFeatureKey(key); err != nil {
			return nil, err
		}
		if _, ok := seenKey[key]; ok {
			continue
		}
		seenKey[key] = struct{}{}
		if f.GetVersion() == 0 {
			needPointer = append(needPointer, key)
		}
	}
	pointers := map[string]activePointer{}
	if len(needPointer) > 0 {
		p, err := r.resolvePointers(needPointer)
		if err != nil {
			return nil, err
		}
		pointers = p
	}
	defKeys := make([]model.DefinitionKey, 0, len(refs))
	resolved := make([]struct {
		key     string
		version int32
		prev    int32
	}, 0, len(refs))
	for _, f := range refs {
		key := strings.TrimSpace(f.GetFeatureKey())
		version := f.GetVersion()
		var prev int32
		if version == 0 {
			p := pointers[key]
			if p.Active < 1 {
				// 没有 ACTIVE 版本：读「当前生效版本」的请求无法用任何值回答，
				// 显式报错而不是返回默认值（DEFAULT_VALUE 降级只针对「有定义但没写过值」）。
				return nil, fmt.Errorf("%w: %s", model.ErrNoActiveVersion, key)
			}
			version, prev = p.Active, p.Previous
		} else if err := checkVersion(version); err != nil {
			return nil, err
		}
		defKeys = append(defKeys, model.DefinitionKey{FeatureKey: key, Version: version})
		resolved = append(resolved, struct {
			key     string
			version int32
			prev    int32
		}{key: key, version: version, prev: prev})
	}
	defs, err := r.svcCtx.Definitions.ListByKeys(r.ctx, defKeys)
	if err != nil {
		return nil, fmt.Errorf("feature-store: load definitions: %w", err)
	}
	// 显式请求某版本时也要知道「有没有可降级的上一版本」，否则 PREVIOUS_VERSION
	// 这一档降级在契约上就永远不可达。按 key 批量补一次指针。
	extraKeys := make([]string, 0, len(resolved))
	for _, rs := range resolved {
		if rs.prev == 0 {
			if _, ok := pointers[rs.key]; !ok {
				extraKeys = append(extraKeys, rs.key)
			}
		}
	}
	if len(extraKeys) > 0 {
		p, err := r.resolvePointers(extraKeys)
		if err != nil {
			return nil, err
		}
		for k, v := range p {
			pointers[k] = v
		}
	}
	for i := range resolved {
		if resolved[i].prev == 0 {
			resolved[i].prev = pointers[resolved[i].key].Previous
		}
	}

	targets := make([]readTarget, 0, len(defKeys)*len(entities))
	for i, dk := range defKeys {
		def := defs[dk]
		if def == nil {
			return nil, fmt.Errorf("%w: %s@v%d", model.ErrFeatureNotFound, dk.FeatureKey, dk.Version)
		}
		for _, e := range entities {
			scope, id, err := checkEntity(e)
			if err != nil {
				return nil, err
			}
			targets = append(targets, readTarget{
				def:         def,
				version:     resolved[i].version,
				prev:        resolved[i].prev,
				entityScope: scope,
				entityID:    id,
				scopeOK:     scope == def.EntityScope,
			})
		}
	}
	return targets, nil
}

// read 完成一整次批量读（GetFeature 是它长度为 1 的特例）。
func (r *featureReader) read(refs []*rpc.FeatureRef,
	entities []*rpc.EntityRef) ([]*rpc.FeatureEntry, int32, error) {
	targets, err := r.buildTargets(refs, entities)
	if err != nil {
		return nil, 0, err
	}
	keys := make([]model.ValueKey, 0, len(targets)*2)
	for _, t := range targets {
		if !t.scopeOK || t.def.State == model.FeatureStateRetired {
			continue
		}
		keys = append(keys, t.key(t.version))
		if t.prev > 0 {
			keys = append(keys, t.key(t.prev))
		}
	}
	var (
		vals   map[model.ValueKey]*model.FeatureValue
		avail  = true
		degrad int32
	)
	if len(keys) > 0 {
		vals, avail = r.loadValues(keys)
	}
	entries := make([]*rpc.FeatureEntry, 0, len(targets))
	for _, t := range targets {
		entry, _, err := r.entryFor(t, vals, avail)
		if err != nil {
			return nil, 0, err
		}
		if isDegraded(entry) {
			degrad++
		}
		entries = append(entries, entry)
	}
	return entries, degrad, nil
}

// foundByDegradation 把「是否真取到了值」从降级码推出来，
// 保证 GetFeatureReply.found 与 entry.degradation 永远说同一件事。
func foundByDegradation(d rpc.FeatureDegradation) bool {
	switch d {
	case rpc.FeatureDegradation_FEATURE_DEGRADATION_NONE,
		rpc.FeatureDegradation_FEATURE_DEGRADATION_EXPIRED,
		rpc.FeatureDegradation_FEATURE_DEGRADATION_PREVIOUS_VERSION:
		return true
	default:
		return false
	}
}

// --- JSON 小工具 ---

// resultDigest 计算回执快照的 sha256（feature_write_receipt.result_digest 列）。
// 明细未入库（DetailKept=false）时，这一列仍能证明「回放的就是当时那份结果」，
// 而不是让服务端伪造一份看起来一致的明细。
func resultDigest(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func marshalJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

func unmarshalJSON(raw string, out any) error {
	if strings.TrimSpace(raw) == "" {
		return model.ErrMalformedValue
	}
	return json.Unmarshal([]byte(raw), out)
}
