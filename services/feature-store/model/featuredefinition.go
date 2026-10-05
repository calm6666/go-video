package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DigestVersion 是特征定义摘要的算法版本，随摘要一起入库（digest_ver 列）。
// 变更序列化方式时必须递增：否则历史行的摘要与新代码算出的对不上，
// RegisterFeature 会把「完全相同的重复注册」误判成「改了不可变字段」。
const DigestVersion = "fs-def-v1"

// digestSeparator 用不可见控制字符做字段分隔：feature_key、default_value 都是
// 可变长文本，用 ","/":" 分隔会与字段内容撞车，从而出现「不同定义同摘要」的碰撞。
const digestSeparator = "\x1f"

// featureDefinitionColumns 是 feature_definition 的列清单（顺序与 INSERT 占位符一致）。
const featureDefinitionColumns = "feature_key, version, name, value_type, entity_scope, source," +
	" privacy_level, window_seconds, ttl_seconds, default_value, dimension, state, description," +
	" change_note, created_by, immutable_digest, definition_digest, digest_ver, ctime, mtime"

// FeatureDefinition 对应 feature_definition 表：一个特征的一个版本。
//
// 「来源、时间窗口、隐私级别」三列是必填语义而不是可选：
//   - source 必须是允许的计算链路（ValidSource），结构上不存在广告/支付/会员来源；
//   - window_seconds 对行为类来源必须 > 0（SourceRequiresWindow）；
//   - privacy_level 必须显式声明且与 entity_scope 自洽（PrivacyMatchesScope）。
//
// 一行 = 一个版本，(feature_key, version) 唯一。已有版本永不原地改写：
// 口径变化只能注册新版本并切 ACTIVE，这样历史值永远能被正确的定义解释。
type FeatureDefinition struct {
	// FeatureKey 特征键（snake_case，见 ValidFeatureKey）。
	FeatureKey string `db:"feature_key"`
	// Version 版本号，>= 1，由调用方显式指定（不提供「自动 +1」是为了让离线评估
	// 与线上切换能用同一个号，避免两侧各自猜一个版本）。
	Version int32 `db:"version"`
	// Name 展示名（不参与摘要，可随口径说明调整）。
	Name string `db:"name"`
	// ValueType 值类型，不可变。
	ValueType int32 `db:"value_type"`
	// EntityScope 主体类型，不可变。
	EntityScope int32 `db:"entity_scope"`
	// Source 来源计算链路，不可变。
	Source int32 `db:"source"`
	// PrivacyLevel 隐私级别。注册必填；只能通过 UpdateFeaturePrivacy 变更并留审计行。
	PrivacyLevel int32 `db:"privacy_level"`
	// WindowSeconds 统计时间窗口（秒），0 = 无窗口（仅静态配置/实时滑窗允许）。
	WindowSeconds int64 `db:"window_seconds"`
	// TTLSeconds 值存活时间（秒），必须 > 0：没有 TTL 的特征值会永久留在库里，
	// 既无法做隐私到期清理，也无法判断新鲜度。
	TTLSeconds int64 `db:"ttl_seconds"`
	// DefaultValue 缺失降级用的默认值（按 ValueType 序列化的字符串形态），必须可解析。
	DefaultValue string `db:"default_value"`
	// Dimension 列表/向量类的元素上限；标量类必须为 0。
	Dimension int32 `db:"dimension"`
	// State 见 ValidFeatureStateTransition。
	State int32 `db:"state"`
	// Description 口径说明：为什么存在、怎么算（注册时必填，见 ValidateFeatureDefinition）。
	Description string `db:"description"`
	// ChangeNote 本版本相对上一版本的变更说明。
	ChangeNote string `db:"change_note"`
	// CreatedBy 登记人（admin:<id> 或 system:<svc>）。
	CreatedBy string `db:"created_by"`
	// ImmutableDigest 契约声明的五个不可变字段（value_type/entity_scope/source/
	// window_seconds/dimension）的摘要，用于注册时一次比完「值语义是否同一件事」。
	ImmutableDigest string `db:"immutable_digest"`
	// DefinitionDigest 上面五字段 + ttl_seconds + default_value 的摘要：
	// 命中「不可变字段相同但 TTL/默认值不同」时拒绝复用（ErrFeatureMetadataImmutable）。
	DefinitionDigest string `db:"definition_digest"`
	// DigestVer 摘要算法版本（见 DigestVersion）。
	DigestVer string `db:"digest_ver"`
	// Ctime 注册时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后更新时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// ValidateFeatureDefinition 校验一次注册请求（不含唯一性，唯一性由 Insert + 摘要比对处理）。
//
// 校验顺序刻意「先范围、再形态、后自洽」：范围外特征（越界命名、非法来源）要在
// 形态检查之前就拒掉，避免调用方从错误信息里试探出哪些命名可用。
func ValidateFeatureDefinition(d *FeatureDefinition) error {
	if d == nil {
		return ErrFeatureKeyRequired
	}
	if strings.TrimSpace(d.FeatureKey) == "" {
		return ErrFeatureKeyRequired
	}
	if !ValidFeatureKey(d.FeatureKey) {
		return fmt.Errorf("%w: %s", ErrFeatureKeyRequired, d.FeatureKey)
	}
	if ForbiddenFeatureKey(d.FeatureKey) {
		// 不回显命中的段名：名单本身是范围边界，回显等于给绕过者做字典。
		return ErrFeatureKeyForbidden
	}
	if d.Version < 1 {
		return ErrFeatureVersionRequired
	}
	if !ValidValueType(d.ValueType) {
		return ErrValueTypeInvalid
	}
	if !ValidEntityScope(d.EntityScope) {
		return ErrEntityScopeRequired
	}
	if !ValidSource(d.Source) {
		return ErrSourceRequired
	}
	if !ValidPrivacyLevel(d.PrivacyLevel) {
		return ErrPrivacyUnsetNotAllowed
	}
	if !PrivacyMatchesScope(d.EntityScope, d.PrivacyLevel) {
		return ErrPrivacyScopeMismatch
	}
	if d.WindowSeconds < 0 {
		return fmt.Errorf("%w: window_seconds must be >= 0", ErrWindowRequired)
	}
	if SourceRequiresWindow(d.Source) && d.WindowSeconds <= 0 {
		return ErrWindowRequired
	}
	if d.TTLSeconds <= 0 {
		return ErrTTLRequired
	}
	if ValueTypeIsList(d.ValueType) {
		if d.Dimension < 1 || d.Dimension > MaxDimension {
			return ErrDimensionInvalid
		}
	} else if d.Dimension != 0 {
		return ErrDimensionInvalid
	}
	if err := ValidateDefaultValue(d.ValueType, d.DefaultValue, d.Dimension); err != nil {
		return err
	}
	if strings.TrimSpace(d.Description) == "" {
		// 「口径说明」必填是 README 约束的落点：没有它，一年后就没人知道这个特征怎么算的。
		return fmt.Errorf("%w: description is required", ErrDefinitionIncomplete)
	}
	if len(d.Description) > maxDescriptionLen {
		return fmt.Errorf("%w: description too long", ErrDefinitionIncomplete)
	}
	return nil
}

const (
	maxDescriptionLen = 1024
	maxChangeNoteLen  = 512
	maxNameLen        = 128
)

// ValidateDefaultValue 校验默认值可按 value_type 解析（降级返回的东西必须可用）。
func ValidateDefaultValue(valueType int32, raw string, dimension int32) error {
	if !ValidValueType(valueType) {
		return ErrValueTypeInvalid
	}
	switch valueType {
	case ValueTypeInt64:
		if _, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err != nil {
			return ErrDefaultValueInvalid
		}
	case ValueTypeDouble:
		if _, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err != nil {
			return ErrDefaultValueInvalid
		}
	case ValueTypeBool:
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "false", "1", "0":
		default:
			return ErrDefaultValueInvalid
		}
	case ValueTypeString:
		if len(raw) > MaxStringValueLen {
			return ErrMalformedValue
		}
	case ValueTypeInt64List:
		values, err := ParseInt64List(raw)
		if err != nil || !withinDimension(len(values), dimension) {
			return ErrDefaultValueInvalid
		}
	case ValueTypeDoubleList:
		values, err := ParseFloat64List(raw)
		if err != nil || !withinDimension(len(values), dimension) {
			return ErrDefaultValueInvalid
		}
	default:
		return ErrValueTypeInvalid
	}
	return nil
}

// ImmutableDigestOf 只覆盖契约声明的五个不可变字段：回答「这两个版本是不是同一个特征」。
func (d *FeatureDefinition) ImmutableDigestOf() string {
	return digestOf(d.immutablePayload())
}

// DefinitionDigestOf 在不可变五字段之外再覆盖 TTL 与默认值：
// 回答「这两次注册请求是不是同一份定义」。privacy_level 故意不进来 ——
// 它由 UpdateFeaturePrivacy 独立变更并留审计，把它算进注册摘要会让
// 调整过隐私级别的特征无法幂等重放注册。
func (d *FeatureDefinition) DefinitionDigestOf() string {
	return digestOf(d.immutablePayload() + digestSeparator +
		strconv.FormatInt(d.TTLSeconds, 10) + digestSeparator + d.DefaultValue)
}

func (d *FeatureDefinition) immutablePayload() string {
	return strings.Join([]string{
		DigestVersion,
		strconv.FormatInt(int64(d.ValueType), 10),
		strconv.FormatInt(int64(d.EntityScope), 10),
		strconv.FormatInt(int64(d.Source), 10),
		strconv.FormatInt(d.WindowSeconds, 10),
		strconv.FormatInt(int64(d.Dimension), 10),
	}, digestSeparator)
}

func digestOf(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// FillDigests 计算并写回两个摘要与算法版本，供 Insert 前统一调用
// （摘要只能由 model 算，logic 自己拼会算出对不上的值）。
func (d *FeatureDefinition) FillDigests() {
	d.DigestVer = DigestVersion
	d.ImmutableDigest = d.ImmutableDigestOf()
	d.DefinitionDigest = d.DefinitionDigestOf()
}

// SameImmutableAs 判断两个版本是否「同一个特征」。摘要列缺失时（历史行或未走 FillDigests
// 的构造）退回逐字段比对，绝不因为列是空串就判定相同。
func (d *FeatureDefinition) SameImmutableAs(other *FeatureDefinition) bool {
	if d == nil || other == nil {
		return false
	}
	if d.ImmutableDigest != "" && other.ImmutableDigest != "" && d.DigestVer == other.DigestVer {
		return d.ImmutableDigest == other.ImmutableDigest
	}
	return d.ValueType == other.ValueType && d.EntityScope == other.EntityScope &&
		d.Source == other.Source && d.WindowSeconds == other.WindowSeconds &&
		d.Dimension == other.Dimension
}

// SameDefinitionAs 完整口径比对（含 TTL 与默认值），用于幂等注册判定。
func (d *FeatureDefinition) SameDefinitionAs(other *FeatureDefinition) bool {
	if d == nil || other == nil {
		return false
	}
	if d.DefinitionDigest != "" && other.DefinitionDigest != "" && d.DigestVer == other.DigestVer {
		return d.DefinitionDigest == other.DefinitionDigest
	}
	return d.SameImmutableAs(other) && d.TTLSeconds == other.TTLSeconds &&
		d.DefaultValue == other.DefaultValue
}

// IsWritable 判断该版本能否接受**外部**写入：WriteFeatures 只服务已生效版本，
// 所以只有 ACTIVE 可以（DRAFT 的对外写入会绕过「上线前复核」，RETIRED 已下线）。
// 注意它不是「这一行能不能出现在 feature_value 里」的判据：DRAFT 正是回填链路的
// 目标版本（rpc/featurestore.proto:345），因此行构造入口 NewFeatureValue 只挡
// RETIRED，不挡 DRAFT；调用方是外部写入还是回填由写入逻辑各自把关。
func (d *FeatureDefinition) IsWritable() bool { return d != nil && d.State == FeatureStateActive }

// CacheTTLSeconds 返回该特征值的缓存存活秒数（未抖动前的上界）。
func (d *FeatureDefinition) CacheTTLSeconds() int64 {
	if d == nil || d.TTLSeconds <= 0 {
		return 0
	}
	return d.TTLSeconds
}

// DefinitionKey 定位一个特征版本，与 uniq_key_version 同构。
type DefinitionKey struct {
	FeatureKey string
	Version    int32
}

// FeatureDefinitionFilter 是定义列表的过滤条件（零值 = 该维度不限）。
type FeatureDefinitionFilter struct {
	KeyPrefix   string
	EntityScope int32 // EntityScopeUnspecified = 不限
	Source      int32 // SourceUnspecified = 不限
	State       int32 // FeatureStateUnspecified = 不限
	// MaxPrivacyLevel 0 = 不限；>0 时只返回 <= 该级别的行（调用方按自身授权收敛）。
	MaxPrivacyLevel int32
	// OnlyActivePointers true 时只返回当前 feature_active_version 有指针的版本，
	// 供「可切换版本清单」这类视图使用。
	OnlyActivePointers bool
	Pn                 int32
	Ps                 int32
}

// Normalize 收敛分页参数：pn 从 1 起，ps 夹到 MaxListPageSize。
// 这里夹取而不是报错，是因为「页大小略大」不是越界读写；
// 而批量读的条数上限是另一回事，超了必须报错（见 ValidateBatchLimits）。
func (f *FeatureDefinitionFilter) Normalize() {
	if f.Pn < 1 {
		f.Pn = 1
	}
	if f.Ps <= 0 || f.Ps > MaxListPageSize {
		f.Ps = MaxListPageSize
	}
}

// FeatureDefinitionModel 抽象 feature_definition 表。
type FeatureDefinitionModel interface {
	// Insert 注册新版本。命中 uniq_key_version 时返回 ErrFeatureVersionExists
	// （ON DUPLICATE KEY UPDATE mtime = mtime + RowsAffected == 0 识别，不依赖驱动错误码）。
	Insert(ctx context.Context, d *FeatureDefinition) error
	// FindOne 按 (feature_key, version) 精确查；未命中返回 ErrFeatureNotFound。
	FindOne(ctx context.Context, featureKey string, version int32) (*FeatureDefinition, error)
	// ListByKey 返回某个 key 的全部版本（版本切换预检、PREVIOUS_VERSION 降级用）。
	ListByKey(ctx context.Context, featureKey string) ([]*FeatureDefinition, error)
	// List 分页查询。
	List(ctx context.Context, f FeatureDefinitionFilter) ([]*FeatureDefinition, int64, error)
	// ListByKeys 批量按 (feature_key, version) 取定义（批量读解析版本后用，条数受 MaxBatchFeatures 约束）。
	ListByKeys(ctx context.Context, keys []DefinitionKey) (map[DefinitionKey]*FeatureDefinition, error)
	// UpdateState 带状态机校验的乐观迁移（WHERE 带 state = fromState）。
	// 返回 false 表示当前状态已不是 fromState（并发变更或重复提交）。
	UpdateState(ctx context.Context, featureKey string, version int32, fromState, toState int32) (bool, error)
	// UpdatePrivacy 带 CAS 的隐私级别调整（WHERE 带 privacy_level = fromLevel），
	// 防两个并发调整互相覆盖后只留下一条审计。
	UpdatePrivacy(ctx context.Context, featureKey string, version, fromLevel, toLevel int32) (bool, error)
	// CountByState 统计某 key 处于该状态的版本数（「是否还有 ACTIVE 版本」预检）。
	CountByState(ctx context.Context, featureKey string, state int32) (int64, error)
}

type defaultFeatureDefinitionModel struct {
	conn sqlx.SqlConn
}

// NewFeatureDefinitionModel 构造 feature_definition 的 sqlx 实现。
func NewFeatureDefinitionModel(conn sqlx.SqlConn) FeatureDefinitionModel {
	return &defaultFeatureDefinitionModel{conn: conn}
}

func (m *defaultFeatureDefinitionModel) Insert(ctx context.Context, d *FeatureDefinition) error {
	if d == nil {
		return ErrFeatureKeyRequired
	}
	if strings.TrimSpace(d.FeatureKey) == "" {
		return ErrFeatureKeyRequired
	}
	if d.DigestVer == "" {
		d.FillDigests()
	}
	if d.State == FeatureStateUnspecified {
		// 新注册一律 DRAFT：跳过评审直接 ACTIVE 等于没有回滚点（README「版本切换」）。
		d.State = FeatureStateDraft
	}
	if d.Ctime == 0 {
		d.Ctime = nowUnix()
	}
	d.Mtime = d.Ctime
	if len(d.ChangeNote) > maxChangeNoteLen {
		return fmt.Errorf("%w: change_note too long", ErrDefinitionIncomplete)
	}
	if len(d.Name) > maxNameLen {
		return fmt.Errorf("%w: name too long", ErrDefinitionIncomplete)
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feature_definition ("+featureDefinitionColumns+") VALUES ("+placeholders(20)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		d.FeatureKey, d.Version, d.Name, d.ValueType, d.EntityScope, d.Source, d.PrivacyLevel,
		d.WindowSeconds, d.TTLSeconds, d.DefaultValue, d.Dimension, d.State, d.Description,
		d.ChangeNote, d.CreatedBy, d.ImmutableDigest, d.DefinitionDigest, d.DigestVer, d.Ctime, d.Mtime)
	if err != nil {
		return fmt.Errorf("feature_definition Insert: %w", err)
	}
	// 依赖 uniq_key_version：命中已有行时 mtime = mtime 不产生任何变更，RowsAffected == 0，
	// 因此不需要驱动专有错误码就能识别「这行已存在」，调用方据此走摘要比对分支。
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("feature_definition Insert RowsAffected: %w", err)
	}
	if n == 0 {
		return ErrFeatureVersionExists
	}
	return nil
}

const featureDefinitionSelect = "SELECT " + featureDefinitionColumns + " FROM feature_definition"

func (m *defaultFeatureDefinitionModel) FindOne(ctx context.Context, featureKey string, version int32) (*FeatureDefinition, error) {
	if strings.TrimSpace(featureKey) == "" {
		return nil, ErrFeatureKeyRequired
	}
	var row FeatureDefinition
	err := m.conn.QueryRowCtx(ctx, &row,
		featureDefinitionSelect+" WHERE feature_key = ? AND version = ? LIMIT 1", featureKey, version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFeatureNotFound
		}
		return nil, fmt.Errorf("feature_definition FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultFeatureDefinitionModel) ListByKey(ctx context.Context, featureKey string) ([]*FeatureDefinition, error) {
	if strings.TrimSpace(featureKey) == "" {
		return nil, ErrFeatureKeyRequired
	}
	var rows []*FeatureDefinition
	err := m.conn.QueryRowsCtx(ctx, &rows,
		featureDefinitionSelect+" WHERE feature_key = ? ORDER BY version DESC", featureKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_definition ListByKey: %w", err)
	}
	return rows, nil
}

func (m *defaultFeatureDefinitionModel) List(ctx context.Context, f FeatureDefinitionFilter) ([]*FeatureDefinition, int64, error) {
	f.Normalize()
	where := "WHERE 1 = 1"
	args := make([]any, 0, 6)
	if prefix := strings.TrimSpace(f.KeyPrefix); prefix != "" {
		// 前缀匹配带尾下划线由调用方负责（"u_play" 会同时命中 "u_play_x" 与 "u_player"）：
		// 这里不自动补 "_"，因为契约把 prefix 定义为字面前缀，服务端偷偷改写规则更危险。
		where += " AND feature_key LIKE ?"
		args = append(args, prefix+"%")
	}
	if f.EntityScope != EntityScopeUnspecified {
		where += " AND entity_scope = ?"
		args = append(args, f.EntityScope)
	}
	if f.Source != SourceUnspecified {
		where += " AND source = ?"
		args = append(args, f.Source)
	}
	if f.State != FeatureStateUnspecified {
		where += " AND state = ?"
		args = append(args, f.State)
	}
	if f.MaxPrivacyLevel != 0 {
		if !ValidPrivacyLevel(f.MaxPrivacyLevel) {
			return nil, 0, ErrPrivacyUnsetNotAllowed
		}
		where += " AND privacy_level <= ?"
		args = append(args, f.MaxPrivacyLevel)
	}
	if f.OnlyActivePointers {
		// 相关子查询而不是 JOIN：JOIN 会让 COUNT(*) 与 LIMIT 作用在笛卡尔积上。
		where += " AND EXISTS (SELECT 1 FROM feature_active_version av" +
			" WHERE av.feature_key = feature_definition.feature_key AND av.active_version = feature_definition.version)"
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM feature_definition "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("feature_definition List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*FeatureDefinition
	// 排序固定 (feature_key, version) 升序：与 uniq_key_version 的物理序一致，
	// 不产生 filesort；契约未提供排序参数，所以分页不会中途跳行。
	query := featureDefinitionSelect + where + " ORDER BY feature_key ASC, version ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("feature_definition List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultFeatureDefinitionModel) UpdateState(ctx context.Context, featureKey string, version int32,
	fromState, toState int32) (bool, error) {
	if strings.TrimSpace(featureKey) == "" {
		return false, ErrFeatureKeyRequired
	}
	if !ValidFeatureStateTransition(fromState, toState) {
		return false, ErrFeatureStateTransition
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_definition SET state = ?, mtime = ?"+
			" WHERE feature_key = ? AND version = ? AND state = ?",
		toState, nowUnix(), featureKey, version, fromState)
	if err != nil {
		return false, fmt.Errorf("feature_definition UpdateState: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_definition UpdateState RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultFeatureDefinitionModel) UpdatePrivacy(ctx context.Context, featureKey string, version,
	fromLevel, toLevel int32) (bool, error) {
	if strings.TrimSpace(featureKey) == "" {
		return false, ErrFeatureKeyRequired
	}
	if !ValidPrivacyLevel(fromLevel) || !ValidPrivacyLevel(toLevel) {
		return false, ErrPrivacyUnsetNotAllowed
	}
	if fromLevel == toLevel {
		// 无变化的调整不写审计：否则「谁调了隐私级别」会被大量空记录淹掉。
		return false, nil
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_definition SET privacy_level = ?, mtime = ?"+
			" WHERE feature_key = ? AND version = ? AND privacy_level = ?",
		toLevel, nowUnix(), featureKey, version, fromLevel)
	if err != nil {
		return false, fmt.Errorf("feature_definition UpdatePrivacy: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_definition UpdatePrivacy RowsAffected: %w", err)
	}
	return n == 1, nil
}

func (m *defaultFeatureDefinitionModel) CountByState(ctx context.Context, featureKey string, state int32) (int64, error) {
	if strings.TrimSpace(featureKey) == "" {
		return 0, ErrFeatureKeyRequired
	}
	if !ValidFeatureState(state) {
		return 0, ErrFeatureStateTransition
	}
	var count int64
	err := m.conn.QueryRowCtx(ctx, &count,
		"SELECT COUNT(*) FROM feature_definition WHERE feature_key = ? AND state = ? LIMIT 1",
		featureKey, state)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_definition CountByState: %w", err)
	}
	return count, nil
}

// ListByKeys 一次取回多个 (feature_key, version) 定义，供批量读的「先解析版本再取定义」用。
// 批量读最坏是 50 个特征 × 20 个主体：定义如果逐个查就是 50 次往返，
// 单点延迟会盖过取值本身，所以定义侧也必须有批读（行构造器 IN，走 uniq_key_version）。
func (m *defaultFeatureDefinitionModel) ListByKeys(ctx context.Context, keys []DefinitionKey) (map[DefinitionKey]*FeatureDefinition, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > MaxBatchFeatures {
		return nil, fmt.Errorf("%w: %d definitions > %d", ErrTooManyEntries, len(keys), MaxBatchFeatures)
	}
	args := make([]any, 0, len(keys)*2)
	var b strings.Builder
	b.WriteString(featureDefinitionSelect + " WHERE (feature_key, version) IN (")
	for i, k := range keys {
		if strings.TrimSpace(k.FeatureKey) == "" || k.Version < 1 {
			return nil, ErrFeatureVersionRequired
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?)")
		args = append(args, k.FeatureKey, k.Version)
	}
	b.WriteString(")")
	var rows []*FeatureDefinition
	if err := m.conn.QueryRowsCtx(ctx, &rows, b.String(), args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[DefinitionKey]*FeatureDefinition{}, nil
		}
		return nil, fmt.Errorf("feature_definition ListByKeys: %w", err)
	}
	out := make(map[DefinitionKey]*FeatureDefinition, len(rows))
	for _, r := range rows {
		out[DefinitionKey{FeatureKey: r.FeatureKey, Version: r.Version}] = r
	}
	return out, nil
}
