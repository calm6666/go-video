package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 接收范围门槛，与 pm_user_setting.allow_from、rpc.AllowFrom 一致。
const (
	// AllowFromAnyone 所有人可发（仍受风控与黑名单约束）。
	AllowFromAnyone int32 = 1
	// AllowFromFollowed 仅我关注的人。
	AllowFromFollowed int32 = 2
	// AllowFromMutual 仅互相关注。
	AllowFromMutual int32 = 3
	// AllowFromNone 关闭私信。
	AllowFromNone int32 = 4
)

// ValidAllowFrom 判断接收范围取值合法。
func ValidAllowFrom(v int32) bool { return v >= AllowFromAnyone && v <= AllowFromNone }

// UserSetting 用户私信偏好（pm_user_setting 表），反骚扰的唯一本地配置来源。
//
// 判定链路（README 详解）：黑名单/互关/陌生人门槛的**真值**在 social-graph 与 risk-control，
// 本表只存“我要不要收”的本人意愿；logic 在发送与列表两条路径上把两者合并成过滤结果，
// 缺失设置行时按 DefaultUserSetting 处理（保守：默认不接收陌生人由站点级配置决定）。
type UserSetting struct {
	// Mid 用户 ID（主键）
	Mid int64 `db:"mid"`
	// AllowFrom 接收范围门槛
	AllowFrom int32 `db:"allow_from"`
	// RejectStranger 拒绝未互关用户（1 开启）
	RejectStranger int8 `db:"reject_stranger"`
	// KeywordFilter 启用敏感词/引流词前置过滤
	KeywordFilter int8 `db:"keyword_filter"`
	// MuteConversation 会话免打扰（只影响推送，不影响入库）
	MuteConversation int8 `db:"mute_conversation"`
	// CreatedAt 创建时间（Unix 秒）
	CreatedAt int64 `db:"created_at"`
	// UpdatedAt 最近更新时间（Unix 秒）
	UpdatedAt int64 `db:"updated_at"`
}

// DefaultUserSetting 返回缺省偏好：允许所有人、不拒陌生人、开启关键词过滤。
// defaultAllowFrom 取值必须是 AllowFrom* 常量之一，由服务配置提供。
func DefaultUserSetting(mid int64, defaultAllowFrom int32) *UserSetting {
	if !ValidAllowFrom(defaultAllowFrom) {
		defaultAllowFrom = AllowFromAnyone
	}
	return &UserSetting{
		Mid:           mid,
		AllowFrom:     defaultAllowFrom,
		KeywordFilter: 1,
	}
}

// AcceptsUnknownSender 判断在缺少关系数据（下游不可用）时是否应保守拒收。
// 反骚扰门禁的原则是 fail-closed：门槛不是“任何人”时，拿不到关系就拒。
func (s *UserSetting) AcceptsUnknownSender() bool {
	if s == nil {
		return false
	}
	return s.AllowFrom == AllowFromAnyone && s.RejectStranger == 0
}

// UserSettingModel pm_user_setting 表读写接口。
type UserSettingModel interface {
	// FindByMid 查询偏好；不存在返回 (nil, nil)，由 logic 落 DefaultUserSetting。
	FindByMid(ctx context.Context, mid int64) (*UserSetting, error)
	// FindByMids 批量查询（会话列表按对方偏好过滤时使用）。
	FindByMids(ctx context.Context, mids []int64) (map[int64]*UserSetting, error)
	// Upsert 幂等写入偏好（mid 主键 + ON DUPLICATE KEY UPDATE）。
	Upsert(ctx context.Context, s *UserSetting) error
}

type defaultUserSettingModel struct {
	conn sqlx.SqlConn
}

// NewUserSettingModel 创建 UserSettingModel 实现。
func NewUserSettingModel(conn sqlx.SqlConn) UserSettingModel {
	return &defaultUserSettingModel{conn: conn}
}

const userSettingColumns = `mid, allow_from, reject_stranger, keyword_filter, mute_conversation, created_at, updated_at`

func (m *defaultUserSettingModel) FindByMid(ctx context.Context, mid int64) (*UserSetting, error) {
	var s UserSetting
	err := m.conn.QueryRowCtx(ctx, &s,
		"SELECT "+userSettingColumns+" FROM pm_user_setting WHERE mid = ? LIMIT 1", mid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("pm_user_setting FindByMid: %w", err)
	}
	return &s, nil
}

func (m *defaultUserSettingModel) FindByMids(ctx context.Context, mids []int64) (map[int64]*UserSetting, error) {
	out := make(map[int64]*UserSetting, len(mids))
	if len(mids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(mids))
	for _, v := range mids {
		args = append(args, v)
	}
	query := "SELECT " + userSettingColumns + " FROM pm_user_setting WHERE mid IN (?" +
		strings.Repeat(",?", len(mids)-1) + ")"
	var rows []*UserSetting
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("pm_user_setting FindByMids: %w", err)
	}
	for _, r := range rows {
		out[r.Mid] = r
	}
	return out, nil
}

func (m *defaultUserSettingModel) Upsert(ctx context.Context, s *UserSetting) error {
	if s == nil || s.Mid <= 0 {
		return ErrInvalidMid
	}
	if !ValidAllowFrom(s.AllowFrom) {
		return ErrInvalidAllowFrom
	}
	now := nowUnix()
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO pm_user_setting (mid, allow_from, reject_stranger, keyword_filter, mute_conversation, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE allow_from = VALUES(allow_from), reject_stranger = VALUES(reject_stranger), "+
			"keyword_filter = VALUES(keyword_filter), mute_conversation = VALUES(mute_conversation), updated_at = VALUES(updated_at)",
		s.Mid, s.AllowFrom, s.RejectStranger, s.KeywordFilter, s.MuteConversation, now, now)
	if err != nil {
		return fmt.Errorf("pm_user_setting Upsert: %w", err)
	}
	return nil
}
