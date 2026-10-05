package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// UserInterest 用户兴趣投影行（spm_user_interest 表投影）。
//
// 这是「脱敏后的画像」，不是行为明细：InterestKey 只允许受控词表（zone:<id> / tag:<id> /
// catalog:<item_id> 这类以主键为后缀的键），绝不写搜索词原文、标题或任何自由文本
// （AGENTS.md §7 隐私边界）。同一 mid 的行由 UpsertBatch 整组替换，
// 因此「兴趣消失」是正常结果而不是残留。
type UserInterest struct {
	ID            int64   `db:"id"`             // 主键 ID
	Mid           int64   `db:"mid"`            // 用户 mid
	MetricVersion int32   `db:"metric_version"` // 兴趣口径版本（口径变更必须新增版本）
	InterestKey   string  `db:"interest_key"`   // 受控兴趣键，如 zone:1009
	Weight        float64 `db:"weight"`         // 归一化权重（同一 mid 同一版本内和为 1）
	SampleCount   int64   `db:"sample_count"`   // 支撑该兴趣的样本数
	EventTime     int64   `db:"event_time"`     // 最近一次推进时间（Unix 秒）
	Ctime         int64   `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64   `db:"mtime"`          // 修改时间（Unix 秒）
}

// UserInterestModel spm_user_interest 表读写接口（可从 spm_behavior_event 重算的投影）。
type UserInterestModel interface {
	// ReplaceForMid 整组替换某用户某口径版本的兴趣行：先删后插，在一个事务里完成。
	// 整组替换而不是逐行 upsert，才能保证「不再感兴趣的标签」真的从画像里消失。
	// 写入侧约束：行数 <= maxInterestRowsPerMid、interest_key 必须命中受控词表且批内不重复、
	// weight ∈ [0,1] 且整组和 <= 1、event_time 必填。
	ReplaceForMid(ctx context.Context, mid int64, metricVersion int32, rows []*UserInterest) error
	// ListTop 取某用户权重最高的 N 条兴趣。
	ListTop(ctx context.Context, mid int64, metricVersion int32, topN int32) ([]*UserInterest, error)
	// FindOne 查询单个兴趣项；不存在返回 nil。
	FindOne(ctx context.Context, mid int64, metricVersion int32, interestKey string) (*UserInterest, error)
	// DeleteByMid 删除某用户的全部兴趣（隐私删除链路的本地一步，跨服务清理由编排方负责）。
	DeleteByMid(ctx context.Context, mid int64) (int64, error)
	// CountStaleMids 统计 event_time 早于 before 的用户数（画像过期观测，非精确计数）。
	CountStaleMids(ctx context.Context, before int64) (int64, error)
}

type defaultUserInterestModel struct {
	conn sqlx.SqlConn
}

// NewUserInterestModel 创建 UserInterestModel 实现。
func NewUserInterestModel(conn sqlx.SqlConn) UserInterestModel {
	return &defaultUserInterestModel{conn: conn}
}

const userInterestColumns = "id, mid, metric_version, interest_key, weight, sample_count," +
	" event_time, ctime, mtime"

func (m *defaultUserInterestModel) ReplaceForMid(
	ctx context.Context, mid int64, metricVersion int32, rows []*UserInterest,
) error {
	if mid <= 0 {
		return ErrInvalidMid
	}
	if metricVersion <= 0 {
		return ErrMetricVersionRequired
	}
	// 每个 mid 的兴趣行数受 topN 配置约束：无上限写入会让画像退化成「行为明细表」，
	// 既拖慢读取又变相绕过隐私边界。
	if len(rows) > maxInterestRowsPerMid {
		return ErrTopNTooLarge
	}
	return m.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, err := session.ExecCtx(ctx,
			"DELETE FROM spm_user_interest WHERE mid = ? AND metric_version = ?",
			mid, metricVersion); err != nil {
			return fmt.Errorf("spm_user_interest replace delete: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		now := nowUnix()
		args := make([]any, 0, len(rows)*interestRowLen)
		// 权重之和必须 <= 1：整组替换写入的是「归一化后 top N」，
		// top N 的和只能不超过全集的 1。超过 1 说明上游归一化漏做或被重复累加，
		// 而画像读取侧没有任何线索能发现这件事。
		var weightSum float64
		seen := make(map[string]struct{}, len(rows))
		for _, r := range rows {
			// 受控词表而不是非空校验：兴趣键是画像的唯一可解释入口，
			// 放过一条自由文本就等于把用户输入原文长期存进了画像表。
			if !ValidInterestKey(r.InterestKey) {
				return ErrInvalidInterestKey
			}
			// 同批重复键会被 ON DUPLICATE 静默折叠成一行，权重之和因此不再等于
			// 上游算出来的那份分布；整组替换的语义要求「一行一个键」。
			if _, dup := seen[r.InterestKey]; dup {
				return ErrInvalidInterestKey
			}
			seen[r.InterestKey] = struct{}{}
			if r.Weight < 0 || r.Weight > 1 || r.SampleCount < 0 {
				return ErrInterestWeightOutOfRange
			}
			if r.EventTime <= 0 {
				// event_time 是画像过期观测（CountStaleMids）的依据，0 会让这一行
				// 永远被判为「陈旧」，进而被清理链路整体删掉。
				return ErrEventTimeRequired
			}
			weightSum += r.Weight
			args = append(args,
				mid, metricVersion, truncate(r.InterestKey, 100), r.Weight, r.SampleCount,
				r.EventTime, now, now)
		}
		if weightSum > 1+weightSumTolerance {
			return ErrInterestWeightOutOfRange
		}
		query := "INSERT INTO spm_user_interest (mid, metric_version, interest_key, weight," +
			" sample_count, event_time, ctime, mtime) VALUES " +
			rowPlaceholders(interestRowLen, len(rows)) +
			" ON DUPLICATE KEY UPDATE weight = VALUES(weight)," +
			" sample_count = VALUES(sample_count), event_time = VALUES(event_time)," +
			" mtime = VALUES(mtime)"
		_, err := session.ExecCtx(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("spm_user_interest replace insert: %w", err)
		}
		return nil
	})
}

// interestRowLen 是单个兴趣行的列数，与 ReplaceForMid 的参数顺序严格对应。
const interestRowLen = 8

func (m *defaultUserInterestModel) ListTop(
	ctx context.Context, mid int64, metricVersion int32, topN int32,
) ([]*UserInterest, error) {
	if mid <= 0 {
		return nil, ErrInvalidMid
	}
	if topN <= 0 {
		topN = maxInterestRowsPerMid
	}
	if topN > maxInterestRowsPerMid {
		return nil, ErrTopNTooLarge
	}
	if metricVersion <= 0 {
		// 版本 0 的解析（取当前 ACTIVE 口径）属于 logic 层职责：model 层若退化成
		// 「不过滤版本」，就会把多版本兴趣混在一起返回，权重之和不再可比。
		return nil, ErrMetricVersionRequired
	}
	query := "SELECT " + userInterestColumns +
		" FROM spm_user_interest WHERE mid = ? AND metric_version = ?" +
		" ORDER BY weight DESC, id ASC LIMIT ?"
	var rows []*UserInterest
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, metricVersion, topN); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_user_interest ListTop: %w", err)
	}
	return rows, nil
}

func (m *defaultUserInterestModel) FindOne(
	ctx context.Context, mid int64, metricVersion int32, interestKey string,
) (*UserInterest, error) {
	var row UserInterest
	query := "SELECT " + userInterestColumns +
		" FROM spm_user_interest WHERE mid = ? AND metric_version = ? AND interest_key = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, mid, metricVersion, interestKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_user_interest FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultUserInterestModel) DeleteByMid(ctx context.Context, mid int64) (int64, error) {
	if mid <= 0 {
		return 0, ErrInvalidMid
	}
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM spm_user_interest WHERE mid = ?", mid)
	if err != nil {
		return 0, fmt.Errorf("spm_user_interest DeleteByMid: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_user_interest DeleteByMid RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultUserInterestModel) CountStaleMids(ctx context.Context, before int64) (int64, error) {
	var n int64
	query := "SELECT COUNT(DISTINCT mid) FROM spm_user_interest WHERE event_time < ?"
	if err := m.conn.QueryRowCtx(ctx, &n, query, before); err != nil {
		return 0, fmt.Errorf("spm_user_interest CountStaleMids: %w", err)
	}
	return n, nil
}

// maxInterestRowsPerMid 是单用户兴趣行的硬上限（契约里 top_n 的上限同源）。
// 无上限返回等价于把画像变成无界集合，调用方与实现方都要守这条线。
const maxInterestRowsPerMid = 100

// weightSumTolerance 是「权重之和 <= 1」判定的浮点容差：
// 归一化本身经度量级为 1e-16 的累加误差，不给容差就会在满额分布上误拒。
const weightSumTolerance = 1e-9
