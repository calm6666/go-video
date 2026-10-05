package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveRoomSettingColumns 与 000002_create_live_room_setting.sql 逐列对应。
const liveRoomSettingColumns = "room_id, danmaku_enabled, reply_enabled, record_enabled, " +
	"linkmic_enabled, live_type, min_client_version_code, ctime, mtime"

// LiveRoomSetting 房间直播配置（live_room_setting 表投影，对应 rpc.RoomSetting）。
// 与 live_room 1:1，主键即 room_id：UPSERT 依赖 PRIMARY KEY 冲突，天然幂等。
//
// 开关语义：RoomSetting 的 bool 字段「为 false 表示显式关闭」，因此 UpdateRoomSetting
// 是整段覆盖而不是增量合并（proto 注释已锁定该语义），本表不提供列级局部更新。
type LiveRoomSetting struct {
	RoomID               int64 `db:"room_id"`                 // 房间 ID（主键，1:1）
	DanmakuEnabled       int32 `db:"danmaku_enabled"`         // 1 开启弹幕、0 关闭
	ReplyEnabled         int32 `db:"reply_enabled"`           // 1 开启回复/评论、0 关闭
	RecordEnabled        int32 `db:"record_enabled"`          // 1 允许录制回放、0 关闭
	LinkmicEnabled       int32 `db:"linkmic_enabled"`         // 1 允许连麦、0 关闭
	LiveType             int32 `db:"live_type"`               // 1 视频、2 语音、3 屏幕分享
	MinClientVersionCode int32 `db:"min_client_version_code"` // 允许的最低客户端版本，0 不限制
	Ctime                int64 `db:"ctime"`                   // 创建时间（Unix 秒）
	Mtime                int64 `db:"mtime"`                   // 修改时间（Unix 秒）
}

// BoolToInt32 把 Go bool 转成列存储的 0/1（本服务不用真 BOOLEAN 列，
// 以免 db tag 与 sql 扫描类型在 TINYINT/bool 之间来回转换）。
func BoolToInt32(v bool) int32 {
	if v {
		return 1
	}
	return 0
}

// Int32ToBool 把列存储的 0/1 转成 bool，非 0 一律视为开启。
func Int32ToBool(v int32) bool { return v != 0 }

// LiveRoomSettingModel live_room_setting 表读写接口。
type LiveRoomSettingModel interface {
	// Upsert 整段写入配置：主键冲突时覆盖全部业务列（mtime 取当前时间），
	// 使 UpdateRoomSetting 在客户端重试下可安全重放。
	Upsert(ctx context.Context, s *LiveRoomSetting) error
	// UpsertTx 在事务内整段写入配置，供 CreateRoom「房间 + 绑定 + 配置」同事务提交。
	// session 为 nil 时退化为 Upsert。
	UpsertTx(ctx context.Context, session sqlx.Session, s *LiveRoomSetting) error
	// FindOne 读配置；房间还没写过配置时返回 (nil, nil)，由调用方套默认值。
	FindOne(ctx context.Context, roomID int64) (*LiveRoomSetting, error)
	// RecordEnabledFor 只读录制开关（开播时决定是否通知 live-media 起录制）。
	// 无配置行时返回 ErrNoSettingRow，由调用方按「服务端默认」处理，不静默当作关闭。
	RecordEnabledFor(ctx context.Context, roomID int64) (bool, error)
}

type defaultLiveRoomSettingModel struct {
	conn sqlx.SqlConn
}

// NewLiveRoomSettingModel 创建 LiveRoomSettingModel 实现。
func NewLiveRoomSettingModel(conn sqlx.SqlConn) LiveRoomSettingModel {
	return &defaultLiveRoomSettingModel{conn: conn}
}

func (m *defaultLiveRoomSettingModel) Upsert(ctx context.Context, s *LiveRoomSetting) error {
	return m.upsert(ctx, m.conn, s)
}

func (m *defaultLiveRoomSettingModel) UpsertTx(ctx context.Context, session sqlx.Session, s *LiveRoomSetting) error {
	if session == nil {
		return m.Upsert(ctx, s)
	}
	return m.upsert(ctx, session, s)
}

// upsert 是配置写入的唯一 SQL 实现，execer 可以是连接或事务句柄。
func (m *defaultLiveRoomSettingModel) upsert(ctx context.Context, execer sqlx.Session, s *LiveRoomSetting) error {
	if s.RoomID <= 0 {
		return ErrInvalidRoomID
	}
	if !ValidLiveType(s.LiveType) {
		return ErrSettingInvalid
	}
	if s.MinClientVersionCode < 0 {
		return ErrSettingInvalid
	}
	const query = "INSERT INTO live_room_setting (room_id, danmaku_enabled, reply_enabled, record_enabled, " +
		"linkmic_enabled, live_type, min_client_version_code, ctime, mtime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) " +
		"ON DUPLICATE KEY UPDATE danmaku_enabled = VALUES(danmaku_enabled), " +
		"reply_enabled = VALUES(reply_enabled), record_enabled = VALUES(record_enabled), " +
		"linkmic_enabled = VALUES(linkmic_enabled), live_type = VALUES(live_type), " +
		"min_client_version_code = VALUES(min_client_version_code), mtime = VALUES(mtime)"
	now := nowUnix()
	if s.Ctime == 0 {
		s.Ctime = now
	}
	s.Mtime = now
	_, err := execer.ExecCtx(ctx, query,
		s.RoomID, s.DanmakuEnabled, s.ReplyEnabled, s.RecordEnabled,
		s.LinkmicEnabled, s.LiveType, s.MinClientVersionCode, s.Ctime, s.Mtime)
	if err != nil {
		return fmt.Errorf("live_room_setting Upsert: %w", err)
	}
	return nil
}

func (m *defaultLiveRoomSettingModel) FindOne(ctx context.Context, roomID int64) (*LiveRoomSetting, error) {
	if roomID <= 0 {
		return nil, ErrInvalidRoomID
	}
	var s LiveRoomSetting
	query := "SELECT " + liveRoomSettingColumns + " FROM live_room_setting WHERE room_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &s, query, roomID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_room_setting FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultLiveRoomSettingModel) RecordEnabledFor(ctx context.Context, roomID int64) (bool, error) {
	s, err := m.FindOne(ctx, roomID)
	if err != nil {
		return false, err
	}
	if s == nil {
		return false, ErrNoSettingRow
	}
	return Int32ToBool(s.RecordEnabled), nil
}
