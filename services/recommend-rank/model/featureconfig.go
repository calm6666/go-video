package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RankFeatureConfig 特征配置版本（rank_feature_config 表）。
//
// 一行 = 一个不可变的特征清单版本：模型版本通过 feature_config_version 引用它。
// 拆成两张表的原因：同一份特征配置可以被多个模型版本复用，改特征必须产生新版本号，
// 否则"模型版本可审计"会被偷偷变化的特征清单破坏。
// 实际取数由 feature-store 提供（本期为 stub，见 internal/repository）。
type RankFeatureConfig struct {
	ID                int64  `db:"id"`                  // 自增主键
	ConfigVersion     string `db:"config_version"`      // 特征配置版本（唯一）
	FeatureKeys       string `db:"feature_keys"`        // 特征 key 清单（升序去重 csv）
	FeatureCount      int32  `db:"feature_count"`       // 特征条数（冗余列，便于巡检）
	MissingPolicy     string `db:"missing_policy"`      // 缺失策略：default/drop_source/reject
	FeatureStoreScene string `db:"feature_store_scene"` // 将来接 feature-store 的读取场景 key
	KeysDigest        string `db:"keys_digest"`         // sha256(feature_keys)，配置指纹
	State             int32  `db:"state"`               // 0 停用、1 生效
	Revision          int32  `db:"revision"`            // 元数据修订号
	Operator          string `db:"operator"`            // 最后操作者
	Note              string `db:"note"`                // 变更原因
	Ctime             int64  `db:"ctime"`               // 创建时间（Unix 秒）
	Mtime             int64  `db:"mtime"`               // 修改时间（Unix 秒）
}

// 特征配置状态。
const (
	// FeatureStateDisabled 停用（不可被新版本引用）。
	FeatureStateDisabled int32 = 0
	// FeatureStateEnabled 生效。
	FeatureStateEnabled int32 = 1
)

// ValidFeatureState 判定特征配置状态是否在枚举内（只有启停两态，版本语义不可改）。
func ValidFeatureState(state int32) bool {
	return state == FeatureStateDisabled || state == FeatureStateEnabled
}

// RankFeatureConfigModel rank_feature_config 表读写接口。
type RankFeatureConfigModel interface {
	// Insert 登记特征配置版本（uniq_config_version 幂等；冲突返回 ErrFeatureConfigExists）。
	Insert(ctx context.Context, c *RankFeatureConfig) error
	// FindOne 按 config_version 查询；不存在返回 ErrFeatureConfigNotFound。
	FindOne(ctx context.Context, configVersion string) (*RankFeatureConfig, error)
	// List 分页列出（id DESC）；state < 0 表示不按状态过滤。
	List(ctx context.Context, state int32, offset, limit int) ([]*RankFeatureConfig, error)
	// UpdateState 启停配置（被 ACTIVE 模型引用的版本禁止停用，由调用方先检查引用）。
	UpdateState(ctx context.Context, configVersion string, fromState, toState int32, operator, note string) (bool, error)
}

type defaultRankFeatureConfigModel struct {
	conn sqlx.SqlConn
}

// NewRankFeatureConfigModel 创建 RankFeatureConfigModel 实现。
func NewRankFeatureConfigModel(conn sqlx.SqlConn) RankFeatureConfigModel {
	return &defaultRankFeatureConfigModel{conn: conn}
}

const featureConfigSelect = "SELECT id, config_version, feature_keys, feature_count, missing_policy, " +
	"feature_store_scene, keys_digest, state, revision, operator, note, ctime, mtime FROM rank_feature_config"

func (m *defaultRankFeatureConfigModel) Insert(ctx context.Context, c *RankFeatureConfig) error {
	if c.ConfigVersion == "" {
		return ErrFeatureConfigNotFound
	}
	if !ValidMissingPolicy(c.MissingPolicy) {
		return ErrInvalidMissingPolicy
	}
	if !ValidFeatureState(c.State) {
		return ErrInvalidFeatureState
	}
	// feature_keys 与 feature_count 由调用方用 JoinFeatureKeys 生成后传入，
	// 这里再算一次条数，避免清单与计数不一致（迁移里 feature_count 只是冗余列）。
	c.FeatureCount = int32(len(splitKeys(c.FeatureKeys)))
	if c.Revision == 0 {
		c.Revision = 1
	}
	now := nowUnix()
	c.Ctime = now
	c.Mtime = now
	query := "INSERT INTO rank_feature_config (config_version, feature_keys, feature_count, missing_policy, " +
		"feature_store_scene, keys_digest, state, revision, operator, note, ctime, mtime) VALUES (" +
		inPlaceholders(12) + ")"
	if _, err := m.conn.ExecCtx(ctx, query,
		c.ConfigVersion, c.FeatureKeys, c.FeatureCount, c.MissingPolicy, c.FeatureStoreScene, c.KeysDigest,
		c.State, c.Revision, c.Operator, c.Note, c.Ctime, c.Mtime); err != nil {
		if isDuplicateErr(err) {
			return ErrFeatureConfigExists
		}
		return fmt.Errorf("rank_feature_config Insert: %w", err)
	}
	return nil
}

func (m *defaultRankFeatureConfigModel) FindOne(ctx context.Context, configVersion string) (*RankFeatureConfig, error) {
	var row RankFeatureConfig
	if err := m.conn.QueryRowCtx(ctx, &row, featureConfigSelect+" WHERE config_version = ?", configVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFeatureConfigNotFound
		}
		return nil, fmt.Errorf("rank_feature_config FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRankFeatureConfigModel) List(ctx context.Context, state int32, offset, limit int) ([]*RankFeatureConfig, error) {
	if limit <= 0 {
		return nil, nil
	}
	if offset < 0 {
		offset = 0
	}
	query := featureConfigSelect
	var args []interface{}
	if state >= 0 {
		query += " WHERE state = ?"
		args = append(args, state)
	}
	query += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []*RankFeatureConfig
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_feature_config List: %w", err)
	}
	return rows, nil
}

func (m *defaultRankFeatureConfigModel) UpdateState(ctx context.Context, configVersion string, fromState, toState int32, operator, note string) (bool, error) {
	if !ValidFeatureState(fromState) {
		return false, ErrInvalidFeatureState
	}
	if !ValidFeatureState(toState) {
		return false, ErrInvalidFeatureState
	}
	query := "UPDATE rank_feature_config SET state = ?, operator = ?, note = ?, revision = revision + 1, mtime = ? " +
		"WHERE config_version = ? AND state = ?"
	res, err := m.conn.ExecCtx(ctx, query, toState, operator, note, nowUnix(), configVersion, fromState)
	if err != nil {
		return false, fmt.Errorf("rank_feature_config UpdateState: %w", err)
	}
	return rowAffected(res, "rank_feature_config UpdateState")
}

// splitKeys 拆分 csv 清单（去空白与空项）。
func splitKeys(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 16)
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			part := strings.TrimSpace(s[start:i])
			if part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}
