package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/eventenvelope"
	"go-video/services/search-query/model"
)

// QueryReportOutcome 上报结果。
type QueryReportOutcome struct {
	// Accepted 表示查询日志与事件已在同一事务内落库。
	Accepted bool
	// Deduplicated 表示 query_id 已存在（重复上报），本次未产生新行。
	Deduplicated bool
	// EventID 事件 ID；重复上报时回填首次产生的 ID。
	EventID string
}

// ReportQuery 写入查询日志摘要并产生领域事件（AGENTS.md §5、§7）。
//
// 事务边界：search_query_log 与 search_outbox 在同一事务提交，
// 保证“有日志必有事件”。MQ 投递器尚未接入（见 README「已知缺口」），
// 因此这里只保证事件被可靠产生并持久化，不声称已投递。
//
// 幂等：query_id 唯一索引，重复上报返回 Deduplicated=true 并回填原 event_id。
// Redis 计数（关键词、当日总量）是旁路投影，失败只记录日志，不影响上报结果。
func (r *Repository) ReportQuery(ctx context.Context, l *model.SearchQueryLog, deviceIDHash, traceID string) (*QueryReportOutcome, error) {
	if l == nil || l.QueryId == "" {
		return nil, model.ErrInvalidQueryID
	}
	if l.Keyword == "" {
		return nil, model.ErrInvalidKeyword
	}
	if l.KeywordHash == "" {
		l.KeywordHash = model.KeywordHash(l.Keyword)
	}
	if l.Ctime == 0 {
		l.Ctime = time.Now().Unix()
	}

	out := &QueryReportOutcome{}
	err := r.conn.TransactCtx(ctx, func(tctx context.Context, tx sqlx.Session) error {
		inserted, ierr := r.logMd.InsertIgnore(tctx, tx, l)
		if ierr != nil {
			return ierr
		}
		if !inserted {
			out.Deduplicated = true
			return nil
		}
		env, err := BuildQueryEvent(l, deviceIDHash, traceID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(env)
		if err != nil {
			return fmt.Errorf("search-query/report: marshal envelope: %w", err)
		}
		if err := r.outboxMd.Insert(tctx, tx, &model.SearchOutbox{
			EventId:       env.EventID,
			EventType:     env.EventType,
			SchemaVersion: int32(env.SchemaVersion),
			AggregateType: model.AggregateTypeQuery,
			AggregateId:   l.QueryId,
			Payload:       string(payload),
			State:         model.OutboxStatePending,
			OccurredAt:    l.Ctime,
		}); err != nil {
			return err
		}
		out.Accepted = true
		out.EventID = env.EventID
		return nil
	})
	if err != nil {
		return nil, err
	}

	if out.Deduplicated {
		eventID, ferr := r.outboxMd.FindEventIdByAggregate(ctx, model.AggregateTypeQuery, l.QueryId)
		if ferr != nil {
			// 幂等语义仍然成立（未写入新行），只是无法回传事件 ID：记录但不掩盖结果。
			logx.Errorf("search-query/report: lookup event id for duplicate query_id=%s err=%v", l.QueryId, ferr)
		}
		out.EventID = eventID
	}

	if r.cache != nil {
		if cerr := r.cache.IncrQueryCounters(ctx, l.KeywordHash, todayKey(time.Now())); cerr != nil {
			logx.Errorf("search-query/report: incr counters query_id=%s err=%v", l.QueryId, cerr)
		}
	}
	return out, nil
}

// queryEventPayload 事件负载。
//
// 隐私：只带受控标识（ip_hash/device_id_hash），不含原始 IP、User-Agent、明文设备号；
// 关键词是搜索分析与推荐必需的业务信号，已经是规范化后的文本。
// 范围：不含广告位、投放或商业化字段（AGENTS.md §7）。
type queryEventPayload struct {
	QueryID      string `json:"query_id"`
	Mid          int64  `json:"mid"`
	Keyword      string `json:"keyword"`
	KeywordHash  string `json:"keyword_hash"`
	HitCount     int64  `json:"hit_count"`
	ResultState  string `json:"result_state"`
	LatencyMs    int64  `json:"latency_ms"`
	Platform     string `json:"platform,omitempty"`
	AppVersion   string `json:"app_version,omitempty"`
	IpHash       string `json:"ip_hash,omitempty"`
	DeviceIDHash string `json:"device_id_hash,omitempty"`
}

// BuildQueryEvent 按统一信封（docs/api-and-events.md §4）构造搜索行为事件，
// topic 为 search.query.v1，消费者为 event-collector / spm。
//
// 纯函数（除 event_id 与 occurred_at 取当前值），便于单测校验字段与 envelope.Validate 约束。
func BuildQueryEvent(l *model.SearchQueryLog, deviceIDHash, traceID string) (*eventenvelope.Envelope, error) {
	raw, err := json.Marshal(queryEventPayload{
		QueryID:      l.QueryId,
		Mid:          l.Mid,
		Keyword:      l.Keyword,
		KeywordHash:  l.KeywordHash,
		HitCount:     l.HitCount,
		ResultState:  l.ResultState,
		LatencyMs:    l.LatencyMs,
		Platform:     l.Platform,
		AppVersion:   l.AppVersion,
		IpHash:       l.IpHash,
		DeviceIDHash: deviceIDHash,
	})
	if err != nil {
		return nil, fmt.Errorf("search-query/report: marshal payload: %w", err)
	}
	env, err := eventenvelope.New(
		model.ProducerName,
		model.EventQueryReported,
		model.AggregateTypeQuery,
		l.QueryId,
		model.EventSchemaVersion,
		raw,
		traceID,
	)
	if err != nil {
		return nil, fmt.Errorf("search-query/report: build envelope: %w", err)
	}
	return env, nil
}
