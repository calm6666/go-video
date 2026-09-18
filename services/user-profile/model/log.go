package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UserLog 用户变更日志（参考 model.UserLog）。
// 参考仓库把经验/节操日志写入报表搜索服务（HBase/ES），本服务落地为
// 本地 member_log 表，便于 MoralLog/ExpLog/MoralLogByID/UndoMoral 直接查询。
type UserLog struct {
	// Mid 用户 ID
	Mid int64 `json:"mid"`
	// IP 操作来源 IP
	IP string `json:"ip"`
	// TS 操作时间（Unix 秒）
	TS int64 `json:"ts"`
	// LogID 日志唯一 ID（UUID）
	LogID string `json:"log_id"`
	// Content 日志内容明细（from_moral/to_moral/origin/status/remark/operater/reason 等）
	Content map[string]string `json:"content"`
}

// MemberLog 对应数据库 member_log 表，记录经验与节操变更日志。
// 替代参考仓库的报表搜索服务存储（hbase.go/member_log.go 的替代方案）。
type MemberLog struct {
	// ID 自增主键
	ID int64 `db:"id"`
	// LogType 日志类型：11 经验、12 节操
	LogType int8 `db:"log_type"`
	// Mid 用户 ID
	Mid int64 `db:"mid"`
	// LogID 日志唯一 ID（UUID，唯一索引）
	LogID string `db:"log_id"`
	// TS 操作时间（Unix 秒）
	TS int64 `db:"ts"`
	// IP 操作来源 IP
	IP string `db:"ip"`
	// Content 日志内容 JSON（map[string]string）
	Content string `db:"content"`
	// Status 日志状态：0 有效、1 已撤销（UndoMoral 置位）
	Status int8 `db:"status"`
	// CTime 写入时间（Unix 秒）
	CTime int64 `db:"ctime"`
}

// ToUserLog 转换为接口返回结构。
func (l *MemberLog) ToUserLog() (*UserLog, error) {
	content := map[string]string{}
	if err := json.Unmarshal([]byte(l.Content), &content); err != nil {
		return nil, err
	}
	return &UserLog{
		Mid:     l.Mid,
		IP:      l.IP,
		TS:      l.TS,
		LogID:   l.LogID,
		Content: content,
	}, nil
}

// MemberLogModel 抽象 member_log 表的查询接口。
type MemberLogModel interface {
	// Add 新增日志（返回自增 ID）。
	Add(ctx context.Context, logType int8, ul *UserLog) (int64, error)
	// FindByMid 查询某用户最近 7 天的有效日志（按时间倒序，最多 1000 条）。
	FindByMid(ctx context.Context, logType int8, mid int64) ([]*UserLog, error)
	// FindByLogID 按日志 ID 查询单条日志。
	FindByLogID(ctx context.Context, logType int8, logID string) (*UserLog, error)
	// MarkRevoked 把日志标记为已撤销。
	MarkRevoked(ctx context.Context, logType int8, logID string) error
}

type defaultMemberLogModel struct {
	conn sqlx.SqlConn
}

// NewMemberLogModel 创建基于 sqlx 的 MemberLogModel 实现。
func NewMemberLogModel(conn sqlx.SqlConn) MemberLogModel {
	return &defaultMemberLogModel{conn: conn}
}

func (m *defaultMemberLogModel) Add(ctx context.Context, logType int8, ul *UserLog) (int64, error) {
	bs, err := json.Marshal(ul.Content)
	if err != nil {
		return 0, err
	}
	query := `INSERT INTO member_log (log_type, mid, log_id, ts, ip, content, status, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := m.conn.ExecCtx(ctx, query, logType, ul.Mid, ul.LogID, ul.TS, ul.IP, string(bs), LogStatusActive, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (m *defaultMemberLogModel) FindByMid(ctx context.Context, logType int8, mid int64) ([]*UserLog, error) {
	// 参考仓库查询最近 7 天日志，最多 1000 条
	since := time.Now().Add(-time.Hour * 24 * 7).Unix()
	query := `SELECT id, log_type, mid, log_id, ts, ip, content, status, ctime FROM member_log
		WHERE log_type = ? AND mid = ? AND ts >= ? AND status = 0 ORDER BY ts DESC, id DESC LIMIT 1000`
	var rows []*MemberLog
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, logType, mid, since); err != nil {
		return nil, err
	}
	logs := make([]*UserLog, 0, len(rows))
	for _, r := range rows {
		ul, err := r.ToUserLog()
		if err != nil {
			continue
		}
		logs = append(logs, ul)
	}
	return logs, nil
}

func (m *defaultMemberLogModel) FindByLogID(ctx context.Context, logType int8, logID string) (*UserLog, error) {
	query := `SELECT id, log_type, mid, log_id, ts, ip, content, status, ctime FROM member_log
		WHERE log_type = ? AND log_id = ? ORDER BY id DESC LIMIT 1`
	var row MemberLog
	if err := m.conn.QueryRowCtx(ctx, &row, query, logType, logID); err != nil {
		return nil, err
	}
	return row.ToUserLog()
}

func (m *defaultMemberLogModel) MarkRevoked(ctx context.Context, logType int8, logID string) error {
	query := `UPDATE member_log SET status = ? WHERE log_type = ? AND log_id = ?`
	_, err := m.conn.ExecCtx(ctx, query, LogStatusRevoked, logType, logID)
	return err
}
