package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// featureValueColumns 是 feature_value 的完整列清单（顺序与 SELECT/INSERT 一致）。
const featureValueColumns = "value_id, feature_key, version, entity_scope, entity_id, value_type," +
	" int64_value, double_value, bool_value, string_value, list_values, event_time, expire_at," +
	" source_metric_key, write_request_id, written_by, backfill_job_id, ctime, mtime"

// featureValueInsertColumns 是写入列（不含自增主键），共 18 列。
const featureValueInsertColumns = "feature_key, version, entity_scope, entity_id, value_type," +
	" int64_value, double_value, bool_value, string_value, list_values, event_time, expire_at," +
	" source_metric_key, write_request_id, written_by, backfill_job_id, ctime, mtime"

// featureValueInsertArgs 是每行写入的参数个数（与 featureValueInsertColumns 严格同序）。
const featureValueInsertArgs = 18

// ValuePayload 是「一个特征值」的中立表示。
//
// model 不 import rpc 包：protobuf 结构到落库列的转换由 logic 完成，
// 这里只守住「同一时刻只填充与 value_type 相符的那一个字段」这条不变量
// （契约里的「多余填充视为格式错误」实现在 EncodeValue 一处，读写两侧共用）。
type ValuePayload struct {
	ValueType   int32
	Int64Value  int64
	DoubleValue float64
	BoolValue   bool
	StringValue string
	Int64List   []int64
	DoubleList  []float64
}

// ValueColumns 是编码后的落库列：一次编码只碰与类型对应的那一列，其余留零值。
type ValueColumns struct {
	ValueType   int32
	Int64Value  int64
	DoubleValue float64
	BoolValue   int32 // 存 0/1，避免不同驱动对 bool 的读写差异
	StringValue string
	ListValues  string
}

// EncodeValue 按定义校验并编码一个特征值。
//
// 校验点：value_type 必须与定义一致（跨版本串写会把 double 当 int 读）、
// 只能填充与该类型对应的字段、列表长度受 dimension 约束、字符串有长度上限。
// 任何一条不满足都返回哨兵错误，logic 把它写进 WriteFeaturesReply 的行级 error。
func EncodeValue(def *FeatureDefinition, p ValuePayload) (ValueColumns, error) {
	if def == nil {
		return ValueColumns{}, ErrFeatureNotFound
	}
	if !ValidValueType(def.ValueType) || p.ValueType != def.ValueType {
		return ValueColumns{}, ErrValueTypeInvalid
	}
	switch p.ValueType {
	case ValueTypeInt64:
		if p.DoubleValue != 0 || p.BoolValue || p.StringValue != "" ||
			len(p.Int64List) > 0 || len(p.DoubleList) > 0 {
			return ValueColumns{}, ErrMalformedValue
		}
		return ValueColumns{ValueType: p.ValueType, Int64Value: p.Int64Value}, nil
	case ValueTypeDouble:
		if p.Int64Value != 0 || p.BoolValue || p.StringValue != "" ||
			len(p.Int64List) > 0 || len(p.DoubleList) > 0 {
			return ValueColumns{}, ErrMalformedValue
		}
		return ValueColumns{ValueType: p.ValueType, DoubleValue: p.DoubleValue}, nil
	case ValueTypeBool:
		if p.Int64Value != 0 || p.DoubleValue != 0 || p.StringValue != "" ||
			len(p.Int64List) > 0 || len(p.DoubleList) > 0 {
			return ValueColumns{}, ErrMalformedValue
		}
		var bit int32
		if p.BoolValue {
			bit = 1
		}
		return ValueColumns{ValueType: p.ValueType, BoolValue: bit}, nil
	case ValueTypeString:
		if p.Int64Value != 0 || p.DoubleValue != 0 || p.BoolValue ||
			len(p.Int64List) > 0 || len(p.DoubleList) > 0 {
			return ValueColumns{}, ErrMalformedValue
		}
		if len(p.StringValue) > MaxStringValueLen {
			return ValueColumns{}, ErrMalformedValue
		}
		return ValueColumns{ValueType: p.ValueType, StringValue: p.StringValue}, nil
	case ValueTypeInt64List:
		if p.Int64Value != 0 || p.DoubleValue != 0 || p.BoolValue || p.StringValue != "" ||
			len(p.DoubleList) > 0 {
			return ValueColumns{}, ErrMalformedValue
		}
		raw, ok := FormatInt64List(p.Int64List, def.Dimension)
		if !ok {
			return ValueColumns{}, ErrDimensionExceeded
		}
		if len(raw) > MaxListValueBytes {
			return ValueColumns{}, ErrDimensionExceeded
		}
		return ValueColumns{ValueType: p.ValueType, ListValues: raw}, nil
	case ValueTypeDoubleList:
		if p.Int64Value != 0 || p.DoubleValue != 0 || p.BoolValue || p.StringValue != "" ||
			len(p.Int64List) > 0 {
			return ValueColumns{}, ErrMalformedValue
		}
		raw, ok := FormatFloat64List(p.DoubleList, def.Dimension)
		if !ok {
			return ValueColumns{}, ErrDimensionExceeded
		}
		if len(raw) > MaxListValueBytes {
			return ValueColumns{}, ErrDimensionExceeded
		}
		return ValueColumns{ValueType: p.ValueType, ListValues: raw}, nil
	default:
		return ValueColumns{}, ErrValueTypeInvalid
	}
}

// ExpireAt 按定义 TTL 计算到期时间。
//
// ttl<=0 直接报错而不是返回 0：0 会被读侧当成「永不过期」，
// 而注册已经禁止 TTL 缺省，走到这里只能是调用方漏传，必须暴露出来。
func ExpireAt(eventTime, ttlSeconds int64) (int64, error) {
	if ttlSeconds <= 0 {
		return 0, ErrTTLRequired
	}
	base := eventTime
	if base <= 0 {
		base = nowUnix()
	}
	if base > math.MaxInt64-ttlSeconds {
		return math.MaxInt64, nil // 溢出时钳到极大值：等价于「不过期」，但只有非法输入会到这里
	}
	return base + ttlSeconds, nil
}

// FeatureValue 对应 feature_value 表：某个主体的某个特征在某个版本上的一个值。
//
// 三个时间字段各有各的用途，不能互相替代：
//   - event_time：值在上游的产出时间（乱序保护与幂等的依据）；
//   - expire_at：TTL 到期时间（定义 ttl_seconds + event_time），在线读的新鲜度判据；
//   - ctime/mtime：本行的入库与变更时间，只用于运维排障，绝不参与新鲜度判定
//     （否则一次重放就会把历史值伪装成刚产出的值）。
type FeatureValue struct {
	// ValueID 自增主键（清理与回填按它分批，避免范围删除的间隙锁）。
	ValueID int64 `db:"value_id"`
	// FeatureKey 特征键。
	FeatureKey string `db:"feature_key"`
	// Version 特征版本：值永远属于某个具体版本，切 ACTIVE 指针不搬值。
	Version int32 `db:"version"`
	// EntityScope 主体类型（入库前由 ValidEntityID 校验形态）。
	EntityScope int32 `db:"entity_scope"`
	// EntityID 主体标识：十进制主键串或受控哈希摘要，明文标识不允许出现在这一列。
	EntityID string `db:"entity_id"`
	// ValueType 值类型（自定义冗余一份，便于直读与解码自校验）。
	ValueType int32 `db:"value_type"`
	// Int64Value 标量 int64。
	Int64Value int64 `db:"int64_value"`
	// DoubleValue 标量 double。
	DoubleValue float64 `db:"double_value"`
	// BoolValue 标量 bool（0/1）。
	BoolValue int32 `db:"bool_value"`
	// StringValue 标量字符串（上限 MaxStringValueLen）。
	StringValue string `db:"string_value"`
	// ListValues 列表/向量：逗号分隔的纯数字，无 JSON、无自由文本。
	ListValues string `db:"list_values"`
	// EventTime 值的产出时间（Unix 秒）。
	EventTime int64 `db:"event_time"`
	// ExpireAt TTL 到期时间（Unix 秒）。0 只可能是历史脏数据：
	// 注册已强制 ttl_seconds > 0，因此新写入恒为正；读侧遇到 0 一律按「已过期」处理。
	ExpireAt int64 `db:"expire_at"`
	// SourceMetricKey 上游口径追溯（来自 spm 时为 "<metric_key>@v<n>"）。
	SourceMetricKey string `db:"source_metric_key"`
	// WriteRequestID 最近一次写入该行的幂等键（排障：这一行是哪一批写进来的）。
	WriteRequestID string `db:"write_request_id"`
	// WrittenBy 最近一次写入者身份（system:<svc> / offline-job:<id>）。
	WrittenBy string `db:"written_by"`
	// BackfillJobID 回填作业写行时记录的 job_id（0 = 实时链路写入）。
	// 有这个列，「某个回填批次改了哪些行」才可查、可重算，
	// 不必靠时间窗猜（同一时间窗里实时链路也在写）。
	BackfillJobID int64 `db:"backfill_job_id"`
	// Ctime 首次入库时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后更新时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// NewFeatureValue 是构造待写行的唯一入口：把「值编码 + TTL 计算 + 主体类型自洽」
// 三条约束收在一处，logic 就不可能写出没有到期时间的特征值。
//
// eventTime<=0 时按服务端当前时间补齐（契约里 0 = 服务端当前时间的语义在此实现）。
func NewFeatureValue(def *FeatureDefinition, key ValueKey, p ValuePayload, eventTime int64,
	sourceMetricKey, requestID, operator string, backfillJobID int64) (*FeatureValue, error) {
	if def == nil {
		return nil, ErrFeatureNotFound
	}
	if err := key.validate(); err != nil {
		return nil, err
	}
	if key.EntityScope != def.EntityScope {
		return nil, ErrEntityScopeMismatch
	}
	// 状态闸门只挡「契约明确禁止写入」的状态，不在这里复刻写入 RPC 入口的策略：
	//   * FeatureState 的契约注释只规定一种状态不接受写入：RETIRED
	//     （rpc/featurestore.proto:93），它的专用哨兵就是 ErrFeatureRetired
	//     （model/errors.go:48-49）。读侧表达「这版不对外」用的是降级码
	//     FEATURE_RETIRED（internal/logic/helpers.go 按 def.State 判定），不是错误值，
	//     所以这里没有理由替读侧把 DRAFT 也一起挡掉。
	//   * DRAFT 必须能落行：回填目标版本恒为 DRAFT（rpc/featurestore.proto:345
	//     「目标版本必须处于 DRAFT（回填完成后才允许切 ACTIVE）」），而「升 ACTIVE 前
	//     该版本已有值」是 UpdateFeatureState 的前置条件（README §5 表 UpdateFeatureState 行、
	//     internal/logic/updatefeaturestatelogic.go checkActivationPreconditions）。
	//     本函数是构造待写行的唯一入口，这里挡住 DRAFT 就等于没有任何路径能让
	//     一个 feature_key 的**首个**版本（无上一版本可降级）满足上线条件。
	//   * 「只有 ACTIVE 才接受外部写入」由写入入口判定
	//     （internal/logic/writefeatureslogic.go checkRow，它同时给出 FEATURE_RETIRED
	//     与 FEATURE_NOT_ACTIVE 两种短码），外部调用方到不了这里。
	switch def.State {
	case FeatureStateDraft, FeatureStateActive:
	case FeatureStateRetired:
		return nil, fmt.Errorf("%w: %s@v%d", ErrFeatureRetired, def.FeatureKey, def.Version)
	default:
		// 定义行的状态不在已声明枚举内（脏数据）：不猜它相当于哪一档，与
		// UpdateFeatureState 对未声明状态的处置同一口径（model.ValidFeatureState）。
		return nil, fmt.Errorf("%w: definition state %d is not a declared value",
			ErrFeatureStateTransition, def.State)
	}
	if eventTime <= 0 {
		eventTime = nowUnix()
	}
	cols, err := EncodeValue(def, p)
	if err != nil {
		return nil, err
	}
	expireAt, err := ExpireAt(eventTime, def.TTLSeconds)
	if err != nil {
		return nil, err
	}
	if len(sourceMetricKey) > MaxSourceMetricKeyLen {
		return nil, ErrMalformedValue
	}
	if SourceRequiresWindow(def.Source) && sourceMetricKey == "" && backfillJobID == 0 {
		// 行为类特征必须能追溯到口径：一个说得出「7 日完播率」却对不上任何指标键的值，
		// 事后无法解释，也无法重算。回填路径靠 job_id 追溯，所以允许为空。
		return nil, ErrSourceRequired
	}
	now := nowUnix()
	return &FeatureValue{
		FeatureKey:      key.FeatureKey,
		Version:         key.Version,
		EntityScope:     key.EntityScope,
		EntityID:        key.EntityID,
		ValueType:       cols.ValueType,
		Int64Value:      cols.Int64Value,
		DoubleValue:     cols.DoubleValue,
		BoolValue:       cols.BoolValue,
		StringValue:     cols.StringValue,
		ListValues:      cols.ListValues,
		EventTime:       eventTime,
		ExpireAt:        expireAt,
		SourceMetricKey: sourceMetricKey,
		WriteRequestID:  requestID,
		WrittenBy:       operator,
		BackfillJobID:   backfillJobID,
		Ctime:           now,
		Mtime:           now,
	}, nil
}

// ValueKey 定位一行：与 uniq_feature_entity 完全同构。
type ValueKey struct {
	FeatureKey  string
	Version     int32
	EntityScope int32
	EntityID    string
}

func (k ValueKey) validate() error {
	if strings.TrimSpace(k.FeatureKey) == "" {
		return ErrFeatureKeyRequired
	}
	if k.Version < 1 {
		return ErrFeatureVersionRequired
	}
	if !ValidEntityScope(k.EntityScope) {
		return ErrEntityScopeRequired
	}
	if !ValidEntityID(k.EntityScope, k.EntityID) {
		return ErrEntityIDInvalid
	}
	return nil
}

// CacheKey 返回该行的 Redis 主读键（键里带版本，见 ValueCacheKey 的注释）。
func (k ValueKey) CacheKey() string {
	return ValueCacheKey(k.FeatureKey, k.Version, k.EntityScope, k.EntityID)
}

// Key 返回该行的唯一定位键。
func (v *FeatureValue) Key() ValueKey {
	if v == nil {
		return ValueKey{}
	}
	return ValueKey{FeatureKey: v.FeatureKey, Version: v.Version, EntityScope: v.EntityScope, EntityID: v.EntityID}
}

// Payload 把落库列解码回中立值。列组合不合法（脏数据或跨版本误读）时报错，
// 绝不返回「零值 + nil」：那等于把损坏数据当成合法的 0 值喂给排序模型。
func (v *FeatureValue) Payload() (ValuePayload, error) {
	if v == nil {
		return ValuePayload{}, ErrMalformedValue
	}
	switch v.ValueType {
	case ValueTypeInt64:
		return ValuePayload{ValueType: v.ValueType, Int64Value: v.Int64Value}, nil
	case ValueTypeDouble:
		return ValuePayload{ValueType: v.ValueType, DoubleValue: v.DoubleValue}, nil
	case ValueTypeBool:
		return ValuePayload{ValueType: v.ValueType, BoolValue: v.BoolValue != 0}, nil
	case ValueTypeString:
		return ValuePayload{ValueType: v.ValueType, StringValue: v.StringValue}, nil
	case ValueTypeInt64List:
		values, err := ParseInt64List(v.ListValues)
		if err != nil {
			return ValuePayload{}, err
		}
		return ValuePayload{ValueType: v.ValueType, Int64List: values}, nil
	case ValueTypeDoubleList:
		values, err := ParseFloat64List(v.ListValues)
		if err != nil {
			return ValuePayload{}, err
		}
		return ValuePayload{ValueType: v.ValueType, DoubleList: values}, nil
	default:
		return ValuePayload{}, ErrMalformedValue
	}
}

// IsExpired 判断在 now 这一秒该值是否已过 TTL。expire_at<=0 视为已过期（见字段注释）。
func (v *FeatureValue) IsExpired(now int64) bool {
	if v == nil {
		return true
	}
	return v.ExpireAt <= 0 || v.ExpireAt <= now
}

// DefaultValuePayload 把定义里的 default_value 解成中立值（降级响应用）。
// 默认值不可解析说明数据或注册流程坏了，这里返回错误而不是给一个 0 值。
func DefaultValuePayload(def *FeatureDefinition) (ValuePayload, error) {
	if def == nil {
		return ValuePayload{}, ErrFeatureNotFound
	}
	if !ValidValueType(def.ValueType) {
		return ValuePayload{}, ErrValueTypeInvalid
	}
	base := ValuePayload{ValueType: def.ValueType}
	switch def.ValueType {
	case ValueTypeInt64:
		v, err := strconv.ParseInt(strings.TrimSpace(def.DefaultValue), 10, 64)
		if err != nil {
			return ValuePayload{}, ErrDefaultValueInvalid
		}
		base.Int64Value = v
	case ValueTypeDouble:
		v, err := strconv.ParseFloat(strings.TrimSpace(def.DefaultValue), 64)
		if err != nil {
			return ValuePayload{}, ErrDefaultValueInvalid
		}
		base.DoubleValue = v
	case ValueTypeBool:
		switch strings.ToLower(strings.TrimSpace(def.DefaultValue)) {
		case "true", "1":
			base.BoolValue = true
		case "false", "0":
		default:
			return ValuePayload{}, ErrDefaultValueInvalid
		}
	case ValueTypeString:
		base.StringValue = def.DefaultValue
	case ValueTypeInt64List:
		values, err := ParseInt64List(def.DefaultValue)
		if err != nil || !withinDimension(len(values), def.Dimension) {
			return ValuePayload{}, ErrDefaultValueInvalid
		}
		base.Int64List = values
	case ValueTypeDoubleList:
		values, err := ParseFloat64List(def.DefaultValue)
		if err != nil || !withinDimension(len(values), def.Dimension) {
			return ValuePayload{}, ErrDefaultValueInvalid
		}
		base.DoubleList = values
	default:
		return ValuePayload{}, ErrValueTypeInvalid
	}
	return base, nil
}

// UpsertOptions 批量写行为开关。
type UpsertOptions struct {
	// AllowStaleOverwrite true = 允许 event_time 更早的入参覆盖库里较新的值。
	// 只有回填/离线重算路径可以置 true，且行必须由 NewFeatureValue 带上 backfillJobID
	// 以便追溯；外部 WriteFeatures 一律 false，否则一次迟到的旧批次就能把线上生效值退回历史。
	AllowStaleOverwrite bool
}

// UpsertOutcome 单行写入结果（WriteFeaturesReply.results 的行级来源）。
type UpsertOutcome struct {
	Key ValueKey
	OK  bool
	Err error
}

// FeatureValueModel 抽象 feature_value 表。
//
// 在线读与回填是两类完全不同的访问模式，方法按这两类分开（索引见迁移文件注释）：
//   - 在线读：Redis 主读，miss 用 FindEntries 一次行构造器 IN 回源（条数有界、走唯一索引）；
//   - 回填/清理：ScanKeys 按 entity_id 升序翻页、SelectExpiredIDs 按主键分批，
//     删除一律先选主键再按主键删，不做范围 DELETE（范围锁会堵住在线写）。
type FeatureValueModel interface {
	// BatchUpsert 在给定事务内整批写入，逐行返回结果；格式/乱序问题是行级错误，不中断整批。
	// 但数据库层面的失败（连接、唯一键竞争）会返回错误并由调用方重试整批，
	// 因为幂等键已经落库，重放不会产生第二次效果。
	// session 为 nil 时退化为非事务执行，只允许单行批次使用。
	BatchUpsert(ctx context.Context, session sqlx.Session, rows []*FeatureValue,
		opts UpsertOptions) ([]UpsertOutcome, error)
	// FindOne 按唯一键取一行；不存在返回 (nil, nil)（「没有值」是正常读结果，不是错误）。
	FindOne(ctx context.Context, key ValueKey) (*FeatureValue, error)
	// FindEntries 批量按唯一键回源（一次 IN，条数受 MaxBatchReadEntries 约束）。
	FindEntries(ctx context.Context, keys []ValueKey) (map[ValueKey]*FeatureValue, error)
	// ListByEntity 主体维度分页导出（隐私核对）：JOIN 定义与 ACTIVE 指针，
	// 只返回当前对外生效且未 RETIRED 的版本；minPrivacyLevel>0 时按级别下限过滤，
	// maxPrivacyLevel>0 时再按级别上限收敛（自助导出的可见范围由配置收紧时用）。
	ListByEntity(ctx context.Context, entityScope int32, entityID string, minPrivacyLevel,
		maxPrivacyLevel int32, pn, ps int32) ([]*FeatureValue, int64, error)
	// ListForErase 按 (entity_scope, entity_id) 取一批待擦除的完整行（含历史版本残值、
	// 不按 ACTIVE 指针收窄），按 value_id 升序、带 LIMIT。
	// 与 DeleteByEntity 的分工：后者只回计数，拿不到键就无法同步清理 Redis 主读缓存；
	// 「库里删了、缓存还在」等于擦除后仍能读到已删的个体数据，是不可接受的。
	// 擦除路径因此走三步：ListForErase 选行 → DeleteByIDs 按主键删 → 用行上的键清缓存。
	ListForErase(ctx context.Context, entityScope int32, entityID string, minPrivacyLevel,
		limit int32) ([]*FeatureValue, error)
	// CountByEntity 统计某主体对外生效的特征行数（EraseEntityFeatures 的 features_touched）。
	CountByEntity(ctx context.Context, entityScope int32, entityID string, minPrivacyLevel int32) (int64, error)
	// DeleteByEntity 隐私删除：按 limit 分批删该主体的值（定义与审计保留），返回删除行数。
	// 与 ListByEntity 不同，这里不按 ACTIVE 指针收窄：历史版本的残值也属于个体数据，
	// 只抹当前视图等于没删。
	DeleteByEntity(ctx context.Context, entityScope int32, entityID string, minPrivacyLevel int32,
		limit int32) (int64, error)
	// SelectExpiredIDs 取一批已过期行的主键（走 idx_expire，不锁范围）。
	SelectExpiredIDs(ctx context.Context, before int64, limit int32) ([]int64, error)
	// ListExpiredForPurge 与 SelectExpiredIDs 同一谓词、同一排序，但返回完整行：
	// 清理必须同时 DEL 命中的缓存键，只有主键就无法定位缓存里的那一份副本。
	// （库里删了、缓存还在 = 过期值继续被在线读到，TTL 承诺失效。）
	ListExpiredForPurge(ctx context.Context, before int64, limit int32) ([]*FeatureValue, error)
	// DeleteByIDs 按主键批量删除，返回删除行数。
	DeleteByIDs(ctx context.Context, ids []int64) (int64, error)
	// CountExpired 估算剩余过期行数（PurgeExpired 的收敛判断用）。
	CountExpired(ctx context.Context, before int64) (int64, error)
	// CountByVersion 某版本已有多少行（切换前「上一版本有值可降级」预检）。
	CountByVersion(ctx context.Context, featureKey string, version int32) (int64, error)
	// ScanKeys 按 (feature_key, version) 以 entity_id 升序分批扫键（回填重算与全量核对）。
	ScanKeys(ctx context.Context, featureKey string, version int32, afterEntityID string,
		limit int32) ([]ValueKey, error)
	// DeleteByVersion 按 (feature_key, version) 分批删值（RETIRED 且过保留期后清理）。
	DeleteByVersion(ctx context.Context, featureKey string, version int32, limit int32) (int64, error)
}

type defaultFeatureValueModel struct {
	conn sqlx.SqlConn
}

// NewFeatureValueModel 构造 feature_value 的 sqlx 实现。
func NewFeatureValueModel(conn sqlx.SqlConn) FeatureValueModel {
	return &defaultFeatureValueModel{conn: conn}
}

// exec 在 session 与 conn 之间选择执行器：两者都满足 sqlx.Session。
// 事务内必须传 session；传 nil 只适用于「这条语句自成事务」的单语句场景。
func (m *defaultFeatureValueModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

func (m *defaultFeatureValueModel) BatchUpsert(ctx context.Context, session sqlx.Session,
	rows []*FeatureValue, opts UpsertOptions) ([]UpsertOutcome, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) > MaxBatchWriteRows {
		return nil, fmt.Errorf("%w: %d rows > %d", ErrTooManyRows, len(rows), MaxBatchWriteRows)
	}
	sess := m.exec(session)

	outcomes := make([]UpsertOutcome, len(rows))
	for i, row := range rows {
		outcomes[i].Key = row.Key()
		outcomes[i].OK = true
	}

	// 1) 入参校验 + 批内去重：同一批里同一个键写两次，后一条生效，
	//    前一条标 ErrDuplicateRowInBatch —— 不报错出去上游永远看不到自己的重复计算。
	writable := make([]*FeatureValue, 0, len(rows))
	seen := make(map[ValueKey]int, len(rows))
	for i, row := range rows {
		if err := validateFeatureValueRow(row); err != nil {
			outcomes[i].OK, outcomes[i].Err = false, err
			continue
		}
		if prev, dup := seen[row.Key()]; dup {
			outcomes[prev].OK, outcomes[prev].Err = false, ErrDuplicateRowInBatch
		}
		seen[row.Key()] = i
		writable = append(writable, row)
	}
	if len(writable) == 0 {
		return outcomes, nil
	}

	// 2) 一次查回这些键当前是否存在及其 event_time（行构造器 IN，走唯一索引）。
	existing, err := m.queryExisting(ctx, sess, writable)
	if err != nil {
		return nil, err
	}

	// 3) 分流：新行走一条多行 INSERT，已有行逐条带 event_time 守卫 UPDATE。
	//    守卫写在 SQL 里而不是只靠第 2 步的读结果：两次读之间的并发写必须被条件更新挡住。
	var insertIdx, updateIdx []int
	for i, row := range writable {
		cur, exists := existing[row.Key()]
		if !exists {
			insertIdx = append(insertIdx, i)
			continue
		}
		if !opts.AllowStaleOverwrite && row.EventTime < cur {
			outcomes[i].OK, outcomes[i].Err = false, ErrStaleEventTime
			continue
		}
		updateIdx = append(updateIdx, i)
	}

	if len(insertIdx) > 0 {
		inserts := make([]*FeatureValue, 0, len(insertIdx))
		for _, i := range insertIdx {
			inserts = append(inserts, writable[i])
		}
		if err := m.insertRows(ctx, sess, inserts); err != nil {
			return nil, err
		}
	}
	for _, i := range updateIdx {
		row := writable[i]
		guard := row.EventTime
		if opts.AllowStaleOverwrite {
			guard = math.MaxInt64 // 回填路径：不做乱序守卫（追溯靠 backfill_job_id 列）
		}
		res, err := m.updateRow(ctx, sess, row, guard)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("feature_value BatchUpsert RowsAffected: %w", err)
		}
		if n == 0 {
			// RowsAffected = 0 在这条语句上有三种成因，必须分辨后再报错：
			//   a) 第 2 步读完后有并发写入把更大的 event_time 抢先落库 —— 真乱序冲突；
			//   b) 整行值与本次入参完全相同（同一批的幂等重放）。go-sql-driver 默认返回
			//      「实际改变的行数」而不是「匹配的行数」，全仓 DSN 也都不开 clientFoundRows，
			//      因此同值重写会给出 0 —— 这是写成功，不能报成冲突；
			//   c) 行在两句之间被 PurgeExpired / 隐私删除抹掉 —— 本次没写成，可重试。
			// 分辨方式是在这条稀有分支上回查一次现值（不为区分给表加计数器列）。
			cur, ok, err := m.currentEventTime(ctx, sess, row.Key())
			if err != nil {
				return nil, err
			}
			switch {
			case !ok:
				outcomes[i].OK, outcomes[i].Err = false, ErrRowDisappeared
			case cur > row.EventTime:
				// 调用方必须知道这一行没写成，才能决定重试还是放弃。
				outcomes[i].OK, outcomes[i].Err = false, ErrStaleEventTime
			default:
				// 库里就是本次要写的值：幂等重放，计为写入成功（不重复递增任何计数）。
				outcomes[i].OK, outcomes[i].Err = true, nil
			}
		}
	}
	return outcomes, nil
}

// currentEventTime 回查一行的 event_time；第二返回值 false = 行不存在。
// 只服务于 BatchUpsert 的 RowsAffected = 0 分支（见那里的三分类），不参与正常读路径。
func (m *defaultFeatureValueModel) currentEventTime(ctx context.Context, sess sqlx.Session,
	key ValueKey) (int64, bool, error) {
	var cur int64
	err := sess.QueryRowCtx(ctx, &cur,
		"SELECT event_time FROM feature_value"+
			" WHERE feature_key = ? AND version = ? AND entity_scope = ? AND entity_id = ? LIMIT 1",
		key.FeatureKey, key.Version, key.EntityScope, key.EntityID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("feature_value currentEventTime: %w", err)
	}
	return cur, true, nil
}

func (m *defaultFeatureValueModel) queryExisting(ctx context.Context, sess sqlx.Session,
	rows []*FeatureValue) (map[ValueKey]int64, error) {
	args := make([]any, 0, len(rows)*4)
	var b strings.Builder
	b.WriteString("SELECT feature_key, version, entity_scope, entity_id, event_time FROM feature_value WHERE (feature_key, version, entity_scope, entity_id) IN (")
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?, ?, ?)")
		args = append(args, r.FeatureKey, r.Version, r.EntityScope, r.EntityID)
	}
	b.WriteString(")")
	type existingRow struct {
		FeatureKey  string `db:"feature_key"`
		Version     int32  `db:"version"`
		EntityScope int32  `db:"entity_scope"`
		EntityID    string `db:"entity_id"`
		EventTime   int64  `db:"event_time"`
	}
	var found []existingRow
	if err := sess.QueryRowsCtx(ctx, &found, b.String(), args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[ValueKey]int64{}, nil
		}
		return nil, fmt.Errorf("feature_value queryExisting: %w", err)
	}
	out := make(map[ValueKey]int64, len(found))
	for _, f := range found {
		out[ValueKey{f.FeatureKey, f.Version, f.EntityScope, f.EntityID}] = f.EventTime
	}
	return out, nil
}

func (m *defaultFeatureValueModel) insertRows(ctx context.Context, sess sqlx.Session, rows []*FeatureValue) error {
	var b strings.Builder
	b.WriteString("INSERT INTO feature_value (" + featureValueInsertColumns + ") VALUES ")
	args := make([]any, 0, len(rows)*featureValueInsertArgs)
	for i, r := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(" + placeholders(featureValueInsertArgs) + ")")
		args = append(args, insertArgs(r)...)
	}
	// 唯一键冲突时不覆盖：新行的判定来自同事务内的一次读，读到「不存在」后被并发插入的
	// 极少数情况下宁可留给下一次重放，也不能把别人的值当成自己的。
	b.WriteString(" ON DUPLICATE KEY UPDATE mtime = mtime")
	if _, err := sess.ExecCtx(ctx, b.String(), args...); err != nil {
		return fmt.Errorf("feature_value insertRows: %w", err)
	}
	return nil
}

// updateRow 更新已有行。WHERE 带 event_time <= ? 守卫：
// 只有不早于库里现值的新写入才生效，未命中的行由调用方定性（见 BatchUpsert 第 4 步）。
//
// 本语句不使用 clientFoundRows 这类驱动开关：go-sql-driver 默认返回「实际改变的行数」，
// 同值重写会得到 0，BatchUpsert 因此在 0 分支上回查现值再定成败——
// 用「匹配行数」换取少一次回查，代价是全仓 DSN 要多一个非默认参数并改变其它表
// RowsAffected == 0 幂等判定的含义，不划算。
func (m *defaultFeatureValueModel) updateRow(ctx context.Context, sess sqlx.Session,
	r *FeatureValue, guardEventTime int64) (sql.Result, error) {
	query := "UPDATE feature_value SET value_type = ?, int64_value = ?, double_value = ?, bool_value = ?," +
		" string_value = ?, list_values = ?, event_time = ?, expire_at = ?, source_metric_key = ?," +
		" write_request_id = ?, written_by = ?, backfill_job_id = ?, mtime = ?" +
		" WHERE feature_key = ? AND version = ? AND entity_scope = ? AND entity_id = ? AND event_time <= ?"
	args := []any{
		r.ValueType, r.Int64Value, r.DoubleValue, r.BoolValue, r.StringValue, r.ListValues,
		r.EventTime, r.ExpireAt, r.SourceMetricKey, r.WriteRequestID, r.WrittenBy, r.BackfillJobID, r.Mtime,
		r.FeatureKey, r.Version, r.EntityScope, r.EntityID, guardEventTime,
	}
	res, err := sess.ExecCtx(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("feature_value updateRow: %w", err)
	}
	return res, nil
}

func insertArgs(r *FeatureValue) []any {
	return []any{
		r.FeatureKey, r.Version, r.EntityScope, r.EntityID, r.ValueType,
		r.Int64Value, r.DoubleValue, r.BoolValue, r.StringValue, r.ListValues,
		r.EventTime, r.ExpireAt, r.SourceMetricKey, r.WriteRequestID, r.WrittenBy,
		r.BackfillJobID, r.Ctime, r.Mtime,
	}
}

func validateFeatureValueRow(row *FeatureValue) error {
	if row == nil {
		return ErrMalformedValue
	}
	if err := row.Key().validate(); err != nil {
		return err
	}
	if len(row.StringValue) > MaxStringValueLen {
		return ErrMalformedValue
	}
	if len(row.ListValues) > MaxListValueBytes {
		return ErrDimensionExceeded
	}
	if ValueTypeIsList(row.ValueType) {
		if row.ValueType == ValueTypeInt64List {
			if _, err := ParseInt64List(row.ListValues); err != nil {
				return err
			}
		} else if _, err := ParseFloat64List(row.ListValues); err != nil {
			return err
		}
	}
	if row.EventTime <= 0 {
		return ErrMalformedValue
	}
	if row.ExpireAt <= 0 {
		// 没有到期时间的值就是「永远清不掉的个体数据」，写入侧必须拒收（ErrTTLRequired）。
		return ErrTTLRequired
	}
	if len(row.SourceMetricKey) > MaxSourceMetricKeyLen {
		return ErrMalformedValue
	}
	return nil
}

const featureValueSelect = "SELECT " + featureValueColumns + " FROM feature_value"

// featureValueSelectPrefixed 是多表 JOIN 版本（列全部带 v. 前缀，避免与定义表撞名）。
const featureValueSelectPrefixed = "SELECT v.value_id, v.feature_key, v.version, v.entity_scope, v.entity_id," +
	" v.value_type, v.int64_value, v.double_value, v.bool_value, v.string_value, v.list_values," +
	" v.event_time, v.expire_at, v.source_metric_key, v.write_request_id, v.written_by," +
	" v.backfill_job_id, v.ctime, v.mtime FROM feature_value v"

func (m *defaultFeatureValueModel) FindOne(ctx context.Context, key ValueKey) (*FeatureValue, error) {
	if err := key.validate(); err != nil {
		return nil, err
	}
	var row FeatureValue
	err := m.conn.QueryRowCtx(ctx, &row,
		featureValueSelect+" WHERE feature_key = ? AND version = ? AND entity_scope = ? AND entity_id = ? LIMIT 1",
		key.FeatureKey, key.Version, key.EntityScope, key.EntityID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_value FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultFeatureValueModel) FindEntries(ctx context.Context, keys []ValueKey) (map[ValueKey]*FeatureValue, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > MaxBatchReadEntries {
		return nil, fmt.Errorf("%w: %d keys > %d", ErrTooManyEntries, len(keys), MaxBatchReadEntries)
	}
	for _, k := range keys {
		if err := k.validate(); err != nil {
			return nil, err
		}
	}
	args := make([]any, 0, len(keys)*4)
	var b strings.Builder
	b.WriteString(featureValueSelect + " WHERE (feature_key, version, entity_scope, entity_id) IN (")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?, ?, ?)")
		args = append(args, k.FeatureKey, k.Version, k.EntityScope, k.EntityID)
	}
	b.WriteString(")")
	var rows []*FeatureValue
	if err := m.conn.QueryRowsCtx(ctx, &rows, b.String(), args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[ValueKey]*FeatureValue{}, nil
		}
		return nil, fmt.Errorf("feature_value FindEntries: %w", err)
	}
	out := make(map[ValueKey]*FeatureValue, len(rows))
	for _, r := range rows {
		out[r.Key()] = r
	}
	return out, nil
}

// entityFeatureFrom 是「按主体 + 隐私级别 + 当前生效版本」的共用 FROM/WHERE。
//
// 读侧 JOIN feature_definition / feature_active_version 而不是让 logic 分三次查：
// privacy_level 与 ACTIVE 指针必须和值同属一个快照，分开查会出现
// 「按旧指针读到新版本值」这种越权读，一条 SQL 才拿得到一致视图。
const entityFeatureFromRead = " JOIN feature_definition d ON d.feature_key = v.feature_key AND d.version = v.version" +
	" JOIN feature_active_version av ON av.feature_key = v.feature_key AND av.active_version = v.version" +
	" WHERE v.entity_scope = ? AND v.entity_id = ? AND d.state = ?"

// entityFeatureFromDelete 不带 ACTIVE 指针与状态条件：隐私擦除要连历史版本残值一起删。
const entityFeatureFromDelete = " JOIN feature_definition d ON d.feature_key = v.feature_key AND d.version = v.version" +
	" WHERE v.entity_scope = ? AND v.entity_id = ?"

func privacyClause(minPrivacyLevel int32) (string, error) {
	if minPrivacyLevel == 0 {
		return "", nil
	}
	if !ValidPrivacyLevel(minPrivacyLevel) {
		return "", ErrPrivacyUnsetNotAllowed
	}
	return " AND d.privacy_level >= ?", nil
}

// privacyMaxClause 是导出侧的上限过滤：Privacy.ExportMaxPrivacyLevel 收紧时，
// 自助导出不能把更敏感的行带出去（下限过滤表达的是「调用方只关心某级别以上」，
// 与「本环境允许看到某级别以下」是两件事，必须分列表达）。
func privacyMaxClause(maxPrivacyLevel int32) (string, error) {
	if maxPrivacyLevel == 0 {
		return "", nil
	}
	if !ValidPrivacyLevel(maxPrivacyLevel) {
		return "", ErrPrivacyUnsetNotAllowed
	}
	return " AND d.privacy_level <= ?", nil
}

func (m *defaultFeatureValueModel) ListByEntity(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel, maxPrivacyLevel int32, pn, ps int32) ([]*FeatureValue, int64, error) {
	if !ValidEntityScope(entityScope) {
		return nil, 0, ErrEntityScopeRequired
	}
	if !ValidEntityID(entityScope, entityID) {
		return nil, 0, ErrEntityIDInvalid
	}
	privacySQL, err := privacyClause(minPrivacyLevel)
	if err != nil {
		return nil, 0, err
	}
	maxSQL, err := privacyMaxClause(maxPrivacyLevel)
	if err != nil {
		return nil, 0, err
	}
	if err := ValidatePageSize(pn, ps); err != nil {
		return nil, 0, err
	}
	base := entityFeatureFromRead + privacySQL + maxSQL
	args := []any{entityScope, entityID, FeatureStateActive}
	if privacySQL != "" {
		args = append(args, minPrivacyLevel)
	}
	if maxSQL != "" {
		args = append(args, maxPrivacyLevel)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*)"+base, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("feature_value ListByEntity count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*FeatureValue
	query := featureValueSelectPrefixed + base + " ORDER BY v.feature_key ASC, v.version ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("feature_value ListByEntity: %w", err)
	}
	return rows, total, nil
}

func (m *defaultFeatureValueModel) ListForErase(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel, limit int32) ([]*FeatureValue, error) {
	if !ValidEntityScope(entityScope) {
		return nil, ErrEntityScopeRequired
	}
	if !ValidEntityID(entityScope, entityID) {
		return nil, ErrEntityIDInvalid
	}
	privacySQL, err := privacyClause(minPrivacyLevel)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxPurgeRows {
		return nil, fmt.Errorf("%w: %d", ErrLimitTooLarge, limit)
	}
	args := []any{entityScope, entityID}
	if privacySQL != "" {
		args = append(args, minPrivacyLevel)
	}
	// 谓词与 DeleteByEntity 完全一致（entityFeatureFromDelete：不带 ACTIVE 指针与状态条件），
	// 这样「选出来的行」就是「要删的行」，缓存清理的键集合与删除集合不会错位。
	var rows []*FeatureValue
	query := featureValueSelectPrefixed + entityFeatureFromDelete + privacySQL +
		" ORDER BY v.value_id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, append(args, limit)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_value ListForErase: %w", err)
	}
	return rows, nil
}

func (m *defaultFeatureValueModel) CountByEntity(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel int32) (int64, error) {
	if !ValidEntityScope(entityScope) {
		return 0, ErrEntityScopeRequired
	}
	if !ValidEntityID(entityScope, entityID) {
		return 0, ErrEntityIDInvalid
	}
	privacySQL, err := privacyClause(minPrivacyLevel)
	if err != nil {
		return 0, err
	}
	base := entityFeatureFromRead + privacySQL
	args := []any{entityScope, entityID, FeatureStateActive}
	if privacySQL != "" {
		args = append(args, minPrivacyLevel)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*)"+base, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_value CountByEntity: %w", err)
	}
	return total, nil
}

func (m *defaultFeatureValueModel) DeleteByEntity(ctx context.Context, entityScope int32, entityID string,
	minPrivacyLevel int32, limit int32) (int64, error) {
	if !ValidEntityScope(entityScope) {
		return 0, ErrEntityScopeRequired
	}
	if !ValidEntityID(entityScope, entityID) {
		return 0, ErrEntityIDInvalid
	}
	privacySQL, err := privacyClause(minPrivacyLevel)
	if err != nil {
		return 0, err
	}
	if limit <= 0 || limit > MaxPurgeRows {
		return 0, fmt.Errorf("%w: %d", ErrLimitTooLarge, limit)
	}
	args := []any{entityScope, entityID}
	if privacySQL != "" {
		args = append(args, minPrivacyLevel)
	}
	// 先选主键再按主键删：DELETE ... JOIN 会锁住被扫描的定义行，
	// 隐私删除是在线接口，不能因为一次擦除把注册与读路径堵住。
	var ids []int64
	query := "SELECT v.value_id" + entityFeatureFromDelete + privacySQL + " ORDER BY v.value_id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &ids, query, append(args, limit)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_value DeleteByEntity select: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return m.DeleteByIDs(ctx, ids)
}

func (m *defaultFeatureValueModel) SelectExpiredIDs(ctx context.Context, before int64, limit int32) ([]int64, error) {
	if before <= 0 {
		before = nowUnix()
	}
	if limit <= 0 || limit > MaxPurgeRows {
		return nil, fmt.Errorf("%w: %d", ErrLimitTooLarge, limit)
	}
	var ids []int64
	// expire_at > 0 的条件不能省：0 是「未声明 TTL」的脏数据哨兵。
	// 让它被清理循环跳过，比让它在一次批量删除里把整表范围扫一遍更安全
	// （这类行由按主键的巡检作业单独处理，见 README「已知缺口」）。
	err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT value_id FROM feature_value WHERE expire_at > 0 AND expire_at < ?"+
			" ORDER BY expire_at ASC, value_id ASC LIMIT ?", before, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_value SelectExpiredIDs: %w", err)
	}
	return ids, nil
}

func (m *defaultFeatureValueModel) ListExpiredForPurge(ctx context.Context, before int64,
	limit int32) ([]*FeatureValue, error) {
	if before <= 0 {
		before = nowUnix()
	}
	if limit <= 0 || limit > MaxPurgeRows {
		return nil, fmt.Errorf("%w: %d", ErrLimitTooLarge, limit)
	}
	// 谓词与排序必须与 SelectExpiredIDs 逐字一致：两条路径给出的必须是同一批行，
	// 否则「删了 A 批的行、清了 B 批的缓存」会留下读得到的过期值。
	var rows []*FeatureValue
	err := m.conn.QueryRowsCtx(ctx, &rows,
		featureValueSelect+" WHERE expire_at > 0 AND expire_at < ?"+
			" ORDER BY expire_at ASC, value_id ASC LIMIT ?", before, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_value ListExpiredForPurge: %w", err)
	}
	return rows, nil
}

func (m *defaultFeatureValueModel) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > MaxPurgeRows {
		return 0, fmt.Errorf("%w: %d ids > %d", ErrLimitTooLarge, len(ids), MaxPurgeRows)
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM feature_value WHERE value_id IN ("+placeholders(len(ids))+")", args...)
	if err != nil {
		return 0, fmt.Errorf("feature_value DeleteByIDs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("feature_value DeleteByIDs RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultFeatureValueModel) CountExpired(ctx context.Context, before int64) (int64, error) {
	if before <= 0 {
		before = nowUnix()
	}
	var count int64
	err := m.conn.QueryRowCtx(ctx, &count,
		"SELECT COUNT(*) FROM feature_value WHERE expire_at > 0 AND expire_at < ? LIMIT 1", before)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_value CountExpired: %w", err)
	}
	return count, nil
}

func (m *defaultFeatureValueModel) CountByVersion(ctx context.Context, featureKey string, version int32) (int64, error) {
	if strings.TrimSpace(featureKey) == "" {
		return 0, ErrFeatureKeyRequired
	}
	if version < 1 {
		return 0, ErrFeatureVersionRequired
	}
	var count int64
	err := m.conn.QueryRowCtx(ctx, &count,
		"SELECT COUNT(*) FROM feature_value WHERE feature_key = ? AND version = ? LIMIT 1", featureKey, version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_value CountByVersion: %w", err)
	}
	return count, nil
}

func (m *defaultFeatureValueModel) ScanKeys(ctx context.Context, featureKey string, version int32,
	afterEntityID string, limit int32) ([]ValueKey, error) {
	if strings.TrimSpace(featureKey) == "" {
		return nil, ErrFeatureKeyRequired
	}
	if version < 1 {
		return nil, ErrFeatureVersionRequired
	}
	if limit <= 0 || limit > MaxPurgeRows {
		return nil, fmt.Errorf("%w: %d", ErrLimitTooLarge, limit)
	}
	type keyRow struct {
		EntityScope int32  `db:"entity_scope"`
		EntityID    string `db:"entity_id"`
	}
	var rows []keyRow
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT entity_scope, entity_id FROM feature_value"+
			" WHERE feature_key = ? AND version = ? AND entity_id > ? ORDER BY entity_id ASC LIMIT ?",
		featureKey, version, afterEntityID, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_value ScanKeys: %w", err)
	}
	out := make([]ValueKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, ValueKey{FeatureKey: featureKey, Version: version,
			EntityScope: r.EntityScope, EntityID: r.EntityID})
	}
	return out, nil
}

func (m *defaultFeatureValueModel) DeleteByVersion(ctx context.Context, featureKey string, version int32,
	limit int32) (int64, error) {
	if strings.TrimSpace(featureKey) == "" {
		return 0, ErrFeatureKeyRequired
	}
	if version < 1 {
		return 0, ErrFeatureVersionRequired
	}
	if limit <= 0 || limit > MaxPurgeRows {
		return 0, fmt.Errorf("%w: %d", ErrLimitTooLarge, limit)
	}
	var ids []int64
	err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT value_id FROM feature_value WHERE feature_key = ? AND version = ?"+
			" ORDER BY value_id ASC LIMIT ?", featureKey, version, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_value DeleteByVersion select: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return m.DeleteByIDs(ctx, ids)
}
