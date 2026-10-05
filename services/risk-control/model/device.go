package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RiskDeviceProfile 设备画像（risk_device_profile 表）。
// 只保存 device_hash（SHA-256 摘要），不保存设备号原文；labels 为逗号分隔的风险标签集合。
type RiskDeviceProfile struct {
	ID              int64  `db:"id"`                // 主键 ID
	DeviceHash      string `db:"device_hash"`       // 设备受控 ID（唯一）
	Labels          string `db:"labels"`            // 风险标签，逗号分隔、字典序、去重
	FirstSeen       int64  `db:"first_seen"`        // 首次出现时间（Unix 秒）
	LastSeen        int64  `db:"last_seen"`         // 最近出现时间（Unix 秒）
	RelatedMidCount int64  `db:"related_mid_count"` // 关联账号数（由 risk_device_mid 重算）
	RiskScore       int32  `db:"risk_score"`        // 0-100 设备风险分
	Source          string `db:"source"`            // 最近写入来源：login/gateway/operation/system
	Operator        int64  `db:"operator"`          // 最近人工写入的运营 ID（0 表示系统）
	Ctime           int64  `db:"ctime"`             // 创建时间（Unix 秒）
	Mtime           int64  `db:"mtime"`             // 修改时间（Unix 秒）
}

const deviceColumns = "id, device_hash, labels, first_seen, last_seen, related_mid_count, risk_score, source, operator, ctime, mtime"

// LabelList 返回标签切片（保持字典序）。
func (d *RiskDeviceProfile) LabelList() []string {
	return splitLabels(d.Labels)
}

// splitLabels 解析逗号分隔标签，去空、去重并保持字典序。
func splitLabels(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// MergeLabels 合并新旧标签并序列化为存储形式（去重 + 字典序，保证同集合恒定同串）。
func MergeLabels(existing string, added []string) string {
	set := make(map[string]struct{})
	for _, l := range splitLabels(existing) {
		set[l] = struct{}{}
	}
	for _, l := range added {
		l = strings.TrimSpace(l)
		if l == "" || len(l) > 32 {
			continue // 丢弃不合法标签而不是报错，避免运营批量写入被单个脏值打断
		}
		set[l] = struct{}{}
	}
	merged := make([]string, 0, len(set))
	for l := range set {
		merged = append(merged, l)
	}
	sort.Strings(merged)
	return strings.Join(merged, ",")
}

// RiskDeviceMid 设备-账号关联（risk_device_mid 表）。
// related_mid_count 是「至少登录过几个不同账号」的事实投影，可重算。
type RiskDeviceMid struct {
	ID         int64  `db:"id"`          // 主键 ID
	DeviceHash string `db:"device_hash"` // 设备受控 ID
	Mid        int64  `db:"mid"`         // 账号 ID
	FirstSeen  int64  `db:"first_seen"`  // 首次关联时间
	LastSeen   int64  `db:"last_seen"`   // 最近关联时间
}

// RiskDeviceProfileModel risk_device_profile 表读写接口。
type RiskDeviceProfileModel interface {
	// Upsert 插入或更新画像；first_seen 保留最早值，last_seen 取最新值。
	// 返回 (行, 是否新建)。
	Upsert(ctx context.Context, d *RiskDeviceProfile) (*RiskDeviceProfile, bool, error)
	// FindOne 按 device_hash 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, deviceHash string) (*RiskDeviceProfile, error)
	// UpdateRelatedCount 重算并写回关联账号数。
	UpdateRelatedCount(ctx context.Context, deviceHash string, count int64) error
}

type defaultRiskDeviceProfileModel struct {
	conn sqlx.SqlConn
}

// NewRiskDeviceProfileModel 构造 RiskDeviceProfileModel 实现。
func NewRiskDeviceProfileModel(conn sqlx.SqlConn) RiskDeviceProfileModel {
	return &defaultRiskDeviceProfileModel{conn: conn}
}

func (m *defaultRiskDeviceProfileModel) Upsert(ctx context.Context, d *RiskDeviceProfile) (*RiskDeviceProfile, bool, error) {
	now := nowUnix()
	if d.FirstSeen == 0 {
		d.FirstSeen = now
	}
	d.LastSeen = now
	d.Ctime, d.Mtime = now, now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO risk_device_profile (device_hash, labels, first_seen, last_seen, related_mid_count, risk_score, source, operator, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE labels = VALUES(labels), first_seen = LEAST(first_seen, VALUES(first_seen)), "+
			"last_seen = GREATEST(last_seen, VALUES(last_seen)), risk_score = VALUES(risk_score), "+
			"source = VALUES(source), operator = VALUES(operator), mtime = VALUES(mtime)",
		d.DeviceHash, d.Labels, d.FirstSeen, d.LastSeen, d.RelatedMidCount, d.RiskScore, d.Source, d.Operator, d.Ctime, d.Mtime)
	if err != nil {
		return nil, false, fmt.Errorf("risk_device_profile Upsert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("risk_device_profile Upsert RowsAffected: %w", err)
	}
	// ON DUPLICATE KEY UPDATE 的 affected：1 = 新插入，2 = 命中唯一键并更新，0 = 命中但无字段变化。
	created := aff == 1

	fresh, err := m.FindOne(ctx, d.DeviceHash)
	if err != nil {
		return nil, created, err
	}
	if fresh != nil {
		*d = *fresh
	}
	return d, created, nil
}

func (m *defaultRiskDeviceProfileModel) FindOne(ctx context.Context, deviceHash string) (*RiskDeviceProfile, error) {
	var d RiskDeviceProfile
	query := "SELECT " + deviceColumns + " FROM risk_device_profile WHERE device_hash = ?"
	if err := m.conn.QueryRowCtx(ctx, &d, query, deviceHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("risk_device_profile FindOne: %w", err)
	}
	return &d, nil
}

func (m *defaultRiskDeviceProfileModel) UpdateRelatedCount(ctx context.Context, deviceHash string, count int64) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE risk_device_profile SET related_mid_count = ?, mtime = ? WHERE device_hash = ?",
		count, nowUnix(), deviceHash)
	if err != nil {
		return fmt.Errorf("risk_device_profile UpdateRelatedCount: %w", err)
	}
	return nil
}

// RiskDeviceMidModel risk_device_mid 表读写接口。
type RiskDeviceMidModel interface {
	// AddRelation 幂等登记设备-账号关联；返回本次是否新增。
	AddRelation(ctx context.Context, deviceHash string, mid int64) (bool, error)
	// CountByDevice 统计设备关联过的账号数。
	CountByDevice(ctx context.Context, deviceHash string) (int64, error)
}

type defaultRiskDeviceMidModel struct {
	conn sqlx.SqlConn
}

// NewRiskDeviceMidModel 构造 RiskDeviceMidModel 实现。
func NewRiskDeviceMidModel(conn sqlx.SqlConn) RiskDeviceMidModel {
	return &defaultRiskDeviceMidModel{conn: conn}
}

func (m *defaultRiskDeviceMidModel) AddRelation(ctx context.Context, deviceHash string, mid int64) (bool, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO risk_device_mid (device_hash, mid, first_seen, last_seen) VALUES (?, ?, ?, ?)",
		deviceHash, mid, now, now)
	if err != nil {
		return false, fmt.Errorf("risk_device_mid AddRelation: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("risk_device_mid AddRelation RowsAffected: %w", err)
	}
	if aff == 0 {
		// 已关联过：只刷新 last_seen，计数不变。
		if _, err := m.conn.ExecCtx(ctx,
			"UPDATE risk_device_mid SET last_seen = ? WHERE device_hash = ? AND mid = ?",
			now, deviceHash, mid); err != nil {
			return false, fmt.Errorf("risk_device_mid touch last_seen: %w", err)
		}
	}
	return aff == 1, nil
}

func (m *defaultRiskDeviceMidModel) CountByDevice(ctx context.Context, deviceHash string) (int64, error) {
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, "SELECT COUNT(*) FROM risk_device_mid WHERE device_hash = ?", deviceHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("risk_device_mid CountByDevice: %w", err)
	}
	return n, nil
}
