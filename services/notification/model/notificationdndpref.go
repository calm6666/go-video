package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 通道位掩码：notification_dnd_pref.muted_channels 用位标记已关闭的通道，
// 新增通道时只需扩展一位，不需要改表结构。
const (
	// MaskPush 关闭 Push。
	MaskPush int32 = 1 << 0
	// MaskSMS 关闭短信。
	MaskSMS int32 = 1 << 1
	// MaskEmail 关闭邮件。
	MaskEmail int32 = 1 << 2
)

// 免打扰总开关。
const (
	// DndStateOff 未开启免打扰。
	DndStateOff int32 = 0
	// DndStateOn 已开启免打扰。
	DndStateOn int32 = 1
)

// NotificationDndPref 用户通道偏好与免打扰设置（notification_dnd_pref 表）。
// 一个用户一行（mid 主键），全量覆盖语义：更新时由调用方给出完整通道集合。
type NotificationDndPref struct {
	Mid           int64  `db:"mid"`            // 用户 ID（主键）
	MutedChannels int32  `db:"muted_channels"` // 已关闭通道的位掩码，见 Mask* 常量
	QuietStart    string `db:"quiet_start"`    // 免打扰开始时间 HH:MM，空串表示不设时段
	QuietEnd      string `db:"quiet_end"`      // 免打扰结束时间 HH:MM，支持跨天
	Timezone      string `db:"timezone"`       // IANA 时区名
	State         int32  `db:"state"`          // 免打扰总开关，见 DndState*
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// NotificationDndPrefModel notification_dnd_pref 表读写接口。
type NotificationDndPrefModel interface {
	// Upsert 以 mid 为主键全量覆盖（幂等：重复提交同一偏好结果一致）。
	Upsert(ctx context.Context, p *NotificationDndPref) error
	// FindOne 按 mid 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, mid int64) (*NotificationDndPref, error)
}

type defaultNotificationDndPrefModel struct {
	conn sqlx.SqlConn
}

// NewNotificationDndPrefModel 创建 NotificationDndPrefModel 实现。
func NewNotificationDndPrefModel(conn sqlx.SqlConn) NotificationDndPrefModel {
	return &defaultNotificationDndPrefModel{conn: conn}
}

func (m *defaultNotificationDndPrefModel) Upsert(ctx context.Context, p *NotificationDndPref) error {
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO notification_dnd_pref (mid, muted_channels, quiet_start, quiet_end, timezone, state, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE muted_channels = VALUES(muted_channels), quiet_start = VALUES(quiet_start), "+
			"quiet_end = VALUES(quiet_end), timezone = VALUES(timezone), state = VALUES(state), mtime = VALUES(mtime)",
		p.Mid, p.MutedChannels, p.QuietStart, p.QuietEnd, p.Timezone, p.State, p.Ctime, p.Mtime)
	if err != nil {
		return fmt.Errorf("notification_dnd_pref Upsert: %w", err)
	}
	return nil
}

func (m *defaultNotificationDndPrefModel) FindOne(ctx context.Context, mid int64) (*NotificationDndPref, error) {
	var p NotificationDndPref
	err := m.conn.QueryRowCtx(ctx, &p,
		"SELECT mid, muted_channels, quiet_start, quiet_end, timezone, state, ctime, mtime FROM notification_dnd_pref WHERE mid = ? LIMIT 1", mid)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("notification_dnd_pref FindOne: %w", err)
	}
	return &p, nil
}

// MaskOfChannels 把通道列表转成位掩码。
func MaskOfChannels(channels []int32) int32 {
	var mask int32
	for _, c := range channels {
		switch c {
		case ChannelPush:
			mask |= MaskPush
		case ChannelSMS:
			mask |= MaskSMS
		case ChannelEmail:
			mask |= MaskEmail
		}
	}
	return mask
}

// ChannelsFromMask 把位掩码还原成升序通道列表。
func ChannelsFromMask(mask int32) []int32 {
	out := make([]int32, 0, 3)
	if mask&MaskPush != 0 {
		out = append(out, ChannelPush)
	}
	if mask&MaskSMS != 0 {
		out = append(out, ChannelSMS)
	}
	if mask&MaskEmail != 0 {
		out = append(out, ChannelEmail)
	}
	return out
}

// IsChannelMuted 判断某通道是否被掩码关闭。
func IsChannelMuted(mask, channel int32) bool {
	switch channel {
	case ChannelPush:
		return mask&MaskPush != 0
	case ChannelSMS:
		return mask&MaskSMS != 0
	case ChannelEmail:
		return mask&MaskEmail != 0
	default:
		return true // 未知通道一律拒绝投递，避免通道枚举漂移
	}
}
