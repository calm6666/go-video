package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SearchQueryLog 查询日志摘要（search_query_log 表）。
//
// 隐私与脱敏（AGENTS.md §7）：只保存规范化后的关键词与 ip_hash/device_id_hash，
// 不保存原始 IP、User-Agent、设备号明文；mid 可为 0（游客）。
// 保留期由清理任务按 ctime 截断（README 记录默认保留 90 天），本服务不长期堆积明文。
// 幂等：query_id 唯一索引，重复上报由 InsertIgnore 返回 inserted=false。
type SearchQueryLog struct {
	Id          int64  `db:"id"`           // 自增主键
	QueryId     string `db:"query_id"`     // 幂等键（客户端/网关生成，唯一）
	Mid         int64  `db:"mid"`          // 用户 ID，0 表示游客
	Keyword     string `db:"keyword"`      // 规范化关键词
	KeywordHash string `db:"keyword_hash"` // 关键词 sha256，供热词聚合
	HitCount    int64  `db:"hit_count"`    // 命中数
	ResultState string `db:"result_state"` // ok/empty/degraded/blocked
	LatencyMs   int64  `db:"latency_ms"`   // 引擎耗时（毫秒）
	Platform    string `db:"platform"`     // 端标识
	AppVersion  string `db:"app_version"`  // 客户端版本
	IpHash      string `db:"ip_hash"`      // 脱敏后的来源标识
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
}

// SearchQueryLogModel search_query_log 表访问接口。
type SearchQueryLogModel interface {
	// InsertIgnore 幂等写入；tx 非 nil 时在调用方事务内执行（与 Outbox 同事务）。
	// 返回 inserted=false 表示 query_id 已存在（重复上报）。
	InsertIgnore(ctx context.Context, tx sqlx.Session, l *SearchQueryLog) (bool, error)
	// FindByQueryId 按幂等键查询；不存在返回 (nil, nil)。
	FindByQueryId(ctx context.Context, queryId string) (*SearchQueryLog, error)
}

type defaultSearchQueryLogModel struct {
	conn sqlx.SqlConn
}

// NewSearchQueryLogModel 创建 SearchQueryLogModel 实现。
func NewSearchQueryLogModel(conn sqlx.SqlConn) SearchQueryLogModel {
	return &defaultSearchQueryLogModel{conn: conn}
}

const logRows = "id, query_id, mid, keyword, keyword_hash, hit_count, result_state, latency_ms, platform, app_version, ip_hash, ctime"

// insertIgnoreLog 幂等写入语句：query_id 唯一索引冲突时静默跳过（0 行受影响）。
const insertIgnoreLog = "INSERT IGNORE INTO search_query_log (query_id, mid, keyword, keyword_hash, hit_count, result_state, latency_ms, platform, app_version, ip_hash, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

func (m *defaultSearchQueryLogModel) InsertIgnore(ctx context.Context, tx sqlx.Session, l *SearchQueryLog) (bool, error) {
	if l.QueryId == "" {
		return false, ErrInvalidQueryID
	}
	if !IsValidResultState(l.ResultState) {
		return false, fmt.Errorf("search_query_log InsertIgnore: invalid result_state %q", l.ResultState)
	}
	if l.KeywordHash == "" {
		l.KeywordHash = KeywordHash(l.Keyword)
	}
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	session := tx
	if session == nil {
		session = m.conn
	}
	res, err := session.ExecCtx(ctx, insertIgnoreLog,
		l.QueryId, l.Mid, l.Keyword, l.KeywordHash, l.HitCount, l.ResultState, l.LatencyMs, l.Platform, l.AppVersion, l.IpHash, l.Ctime)
	if err != nil {
		return false, fmt.Errorf("search_query_log InsertIgnore: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("search_query_log InsertIgnore RowsAffected: %w", err)
	}
	return n > 0, nil
}

func (m *defaultSearchQueryLogModel) FindByQueryId(ctx context.Context, queryId string) (*SearchQueryLog, error) {
	var l SearchQueryLog
	query := fmt.Sprintf("SELECT %s FROM search_query_log WHERE query_id = ? LIMIT 1", logRows)
	if err := m.conn.QueryRowCtx(ctx, &l, query, queryId); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("search_query_log FindByQueryId: %w", err)
	}
	return &l, nil
}
