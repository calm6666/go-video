package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RiskCheckLog 风控裁决日志（risk_check_log 表）。
// 用途：解释「当时为什么这么判」+ 事后审计与规则回放。
// 敏感信息约束：只存 mid、action、decision、规则命中、平台与摘要 hash，
// 不存明文 IP、设备号、手机号，也不存 request_context 原值。
// 保留期由 services/cron 归档（见 README 缺口章节）。
type RiskCheckLog struct {
	ID         int64  `db:"id"`           // 主键 ID
	RequestID  string `db:"request_id"`   // 幂等键（唯一）
	Mid        int64  `db:"mid"`          // 账号 ID
	Action     int32  `db:"action"`       // 受保护动作
	Decision   int32  `db:"decision"`     // 裁决
	Score      int32  `db:"score"`        // 0-100 严重度
	HitRuleIDs string `db:"hit_rule_ids"` // 命中规则，格式 "rule_id@version,..."
	Basis      string `db:"basis"`        // 决策依据来源
	Platform   string `db:"platform"`     // 客户端平台
	AppVersion string `db:"app_version"`  // 客户端版本
	DeviceHash string `db:"device_hash"`  // 设备受控 ID（可为空）
	IPHash     string `db:"ip_hash"`      // 调用方预哈希 IP（可为空）
	TraceID    string `db:"trace_id"`     // 链路追踪 ID
	Degraded   int32  `db:"degraded"`     // 0 正常、1 依赖故障降级
	Ctime      int64  `db:"ctime"`        // 创建时间（Unix 秒）
}

// FormatHits 把命中规则序列化为 "rule_id@version,..." 便于回放历史裁决。
func FormatHits(ruleIDs []int64, versions []int32) string {
	if len(ruleIDs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ruleIDs))
	for i, id := range ruleIDs {
		if i >= maxHitsInLog {
			break
		}
		v := int32(0)
		if i < len(versions) {
			v = versions[i]
		}
		parts = append(parts, strconv.FormatInt(id, 10)+"@"+strconv.FormatInt(int64(v), 10))
	}
	return strings.Join(parts, ",")
}

// ParseHits 解析 hit_rule_ids 列，返回规则 ID 与版本（顺序保持）。
func ParseHits(s string) (ruleIDs []int64, versions []int32) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	for _, part := range strings.Split(s, ",") {
		idStr, verStr, _ := strings.Cut(part, "@")
		id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
		if err != nil {
			continue
		}
		var v int64
		if verStr != "" {
			v, _ = strconv.ParseInt(verStr, 10, 32)
		}
		ruleIDs = append(ruleIDs, id)
		versions = append(versions, int32(v))
	}
	return ruleIDs, versions
}

// maxHitsInLog 限制落库的命中条数，避免 VARCHAR(500) 溢出。
const maxHitsInLog = 40

// RiskCheckLogModel risk_check_log 表写入接口。
type RiskCheckLogModel interface {
	// Insert 幂等写入裁决日志（request_id 唯一）。
	// 冲突时不报错也不覆盖首次记录，返回 nil 表示日志已存在。
	Insert(ctx context.Context, l *RiskCheckLog) error
}

type defaultRiskCheckLogModel struct {
	conn sqlx.SqlConn
}

// NewRiskCheckLogModel 构造 RiskCheckLogModel 实现。
func NewRiskCheckLogModel(conn sqlx.SqlConn) RiskCheckLogModel {
	return &defaultRiskCheckLogModel{conn: conn}
}

func (m *defaultRiskCheckLogModel) Insert(ctx context.Context, l *RiskCheckLog) error {
	now := nowUnix()
	// 幂等写：重复 request_id 保留首次记录（decision 已由 Redis 裁决缓存回放），
	// 避免同一 request_id 的重试在审计表里产生两条不同时间的裁决。
	_, err := m.conn.ExecCtx(ctx,
		"INSERT IGNORE INTO risk_check_log (request_id, mid, action, decision, score, hit_rule_ids, basis, platform, app_version, device_hash, ip_hash, trace_id, degraded, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		l.RequestID, l.Mid, l.Action, l.Decision, l.Score, l.HitRuleIDs, l.Basis, l.Platform,
		l.AppVersion, l.DeviceHash, l.IPHash, l.TraceID, l.Degraded, now)
	if err != nil {
		return fmt.Errorf("risk_check_log Insert: %w", err)
	}
	l.Ctime = now
	return nil
}
