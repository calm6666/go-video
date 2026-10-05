package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveRecordSegment 录制切片（live_record_segment）。
// 一条记录 = 录制任务时间轴上的一个序号（seq）。它是回放拼接的最小单位，
// 因此必须能表达「缺口」：断点续录时探测到的空洞用 state=MISSING 显式登记，
// 绝不静默跳过（否则回放出现时间轴空洞却无从排查）。
// 大文件不进 MySQL：只存 bucket/object_key 引用与 size/checksum 证据。
type LiveRecordSegment struct {
	Id           int64  `db:"id"`
	RecordId     int64  `db:"record_id"`
	RoomId       int64  `db:"room_id"`
	LiveSession  int64  `db:"live_session_id"`
	Seq          int64  `db:"seq"`
	StartAt      int64  `db:"start_at"`      // 切片起点（Unix 秒）
	EndAt        int64  `db:"end_at"`        // 切片终点（Unix 秒）
	DurationMs   int64  `db:"duration_ms"`   // 切片时长（毫秒）
	State        int32  `db:"state"`         // SegmentState*
	Bucket       string `db:"bucket"`        // 对象存储桶（MISSING 时可为空）
	ObjectKey    string `db:"object_key"`    // 切片对象 key（相对路径，不含签名）
	SizeBytes    int64  `db:"size_bytes"`    // 字节数（校验证据）
	Checksum     string `db:"checksum"`      // 内容摘要（sha256 hex）
	WorkerId     string `db:"worker_id"`     // 登记该切片的 Worker（仅审计）
	TraceId      string `db:"trace_id"`      // 最近一次登记/推进的 trace_id
	RegisteredAt int64  `db:"registered_at"` // 首次登记时间（Unix 秒，重放不覆盖）
	Mtime        int64  `db:"mtime"`         // 最近一次更新时间（Unix 秒）
}

// SegmentStats 区间统计（回放提交前的校验依据，全部来自单表条件聚合）。
// Expected 由调用方按 to_seq-from_seq+1 计算；Gaps = Expected-Registered，
// 表示「连行都没有」的隐式缺口，Missing/Corrupt 是「有行但不可用」的显式缺口。
type SegmentStats struct {
	Registered int64 `db:"registered"`  // 区间内已登记行数（含缺口）
	Uploading  int64 `db:"uploading"`   // UPLOADING
	Uploaded   int64 `db:"uploaded"`    // UPLOADED
	Verified   int64 `db:"verified"`    // VERIFIED（可参与拼接）
	Missing    int64 `db:"missing"`     // MISSING
	Corrupt    int64 `db:"corrupt"`     // CORRUPT
	DurationMs int64 `db:"duration_ms"` // VERIFIED 时长求和（毫秒）
	MinSeq     int64 `db:"min_seq"`     // 区间内最小登记序号（0 表示空）
	MaxSeq     int64 `db:"max_seq"`     // 区间内最大登记序号（0 表示空）
}

// Gaps 返回该区间不可拼接的缺口总数（隐式空洞 + 显式 MISSING + CORRUPT）。
func (s SegmentStats) Gaps(expected int64) int64 {
	gaps := expected - s.Registered
	if gaps < 0 {
		gaps = 0
	}
	return gaps + s.Missing + s.Corrupt
}

// LiveRecordSegmentModel live_record_segment 表接口。
type LiveRecordSegmentModel interface {
	// InsertIgnore 登记切片；(record_id, seq) 已存在时不报错、返回 0 行（幂等首插入）。
	// 返回 1 表示新行，0 表示序号已被登记过（调用方改用 UpdateState 推进）。
	InsertIgnore(ctx context.Context, s *LiveRecordSegment) (int64, error)
	// InsertIgnoreTx 与 InsertIgnore 同语义，但写入跑在调用方事务里。
	InsertIgnoreTx(ctx context.Context, sess sqlx.Session, s *LiveRecordSegment) (int64, error)
	// InsertIgnoreMissing 批量登记缺口切片（state=MISSING），已存在的序号不覆盖。
	// 断点续录时把 last_seq 之后探测到的空洞补成显式行。
	InsertIgnoreMissing(ctx context.Context, rows []*LiveRecordSegment) (int64, error)
	// InsertIgnoreMissingTx 与 InsertIgnoreMissing 同语义，但写入跑在调用方事务里。
	InsertIgnoreMissingTx(ctx context.Context, sess sqlx.Session, rows []*LiveRecordSegment) (int64, error)
	// FindOne 按主键查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, id int64) (*LiveRecordSegment, error)
	// FindBySeq 按 (record_id, seq) 天然键查询；不存在返回 (nil, nil)。
	FindBySeq(ctx context.Context, recordID, seq int64) (*LiveRecordSegment, error)
	// LastSeq 返回该录制任务已登记的最大序号（无切片时 0）。
	LastSeq(ctx context.Context, recordID int64) (int64, error)
	// ListAfter keyset 分页：只返回 seq > afterSeq 的切片，按 seq 升序。
	// 切片表只增不减（一场 3 小时直播按 10s 分片 = 1080 行/小时），深翻页不用 OFFSET。
	ListAfter(ctx context.Context, recordID, afterSeq int64, state int32, limit int32) ([]*LiveRecordSegment, error)
	// CountByRecord 该录制任务的切片总数（含缺口）。
	CountByRecord(ctx context.Context, recordID int64) (int64, error)
	// StatsInRange 区间条件聚合，供 SubmitReplayTask 校验切片可用性。
	StatsInRange(ctx context.Context, recordID, fromSeq, toSeq int64) (*SegmentStats, error)
	// UpdateState 条件 UPDATE 推进切片状态：fromStates 由状态机表反推（见 segmentFromStates），
	// 因此非法回退（VERIFIED→UPLOADED）天然匹配 0 行。返回受影响行数。
	// 注意：MySQL 在「行存在但值未变化」时也返回 0 行，幂等重放须回读确认状态。
	UpdateState(ctx context.Context, recordID, seq int64, to int32, patch SegmentPatch) (int64, error)
	// UpdateStateTx 与 UpdateState 同语义，但条件 UPDATE 跑在调用方事务里。
	UpdateStateTx(ctx context.Context, sess sqlx.Session, recordID, seq int64, to int32,
		patch SegmentPatch) (int64, error)
	// PurgeByRecord 回收任务用：按录制任务分批删除切片行（只删本地引用记录，
	// 对象存储的真删在 internal/repository 的 Storage 接口里）。
	// 跳过 UPLOADING：Worker 仍在写的行被删掉会留下无人认领的孤儿对象。
	PurgeByRecord(ctx context.Context, recordID int64, limit int32) (int64, error)
	// PurgeByRecordTx 与 PurgeByRecord 同语义，但删除跑在调用方事务里：
	// 回收结果计数（ReportRetentionResult）与引用行的删除必须同生共死，否则崩溃后
	// 会出现「已回报删除 N 行、本地引用仍在」的不一致。
	PurgeByRecordTx(ctx context.Context, sess sqlx.Session, recordID int64, limit int32) (int64, error)
}

// SegmentPatch 推进切片状态时随带写入的字段；nil 表示不更新。
// 空字符串表示「本次不提供」，不会把已有的 bucket/object_key/checksum 抹掉。
type SegmentPatch struct {
	StartAt    *int64
	EndAt      *int64
	DurationMs *int64
	SizeBytes  *int64
	Bucket     *string
	ObjectKey  *string
	Checksum   *string
	WorkerID   *string
	TraceID    *string
}

func (p SegmentPatch) sets() []columnValue {
	sets := make([]columnValue, 0, 9)
	if p.StartAt != nil {
		sets = append(sets, columnValue{"start_at", *p.StartAt})
	}
	if p.EndAt != nil {
		sets = append(sets, columnValue{"end_at", *p.EndAt})
	}
	if p.DurationMs != nil {
		sets = append(sets, columnValue{"duration_ms", *p.DurationMs})
	}
	if p.SizeBytes != nil {
		sets = append(sets, columnValue{"size_bytes", *p.SizeBytes})
	}
	if p.Bucket != nil && *p.Bucket != "" {
		sets = append(sets, columnValue{"bucket", *p.Bucket})
	}
	if p.ObjectKey != nil && *p.ObjectKey != "" {
		sets = append(sets, columnValue{"object_key", *p.ObjectKey})
	}
	if p.Checksum != nil && *p.Checksum != "" {
		sets = append(sets, columnValue{"checksum", *p.Checksum})
	}
	if p.WorkerID != nil && *p.WorkerID != "" {
		sets = append(sets, columnValue{"worker_id", *p.WorkerID})
	}
	if p.TraceID != nil && *p.TraceID != "" {
		sets = append(sets, columnValue{"trace_id", *p.TraceID})
	}
	return sets
}

// segmentFromStates 从合法迁移表反推「目标状态的前置状态集合」，
// 用作条件 UPDATE 的 WHERE state IN (...)：状态机只有一处定义（errors.go）。
func segmentFromStates(to int32) []int32 {
	all := []int32{SegmentStateUploading, SegmentStateUploaded, SegmentStateVerified,
		SegmentStateMissing, SegmentStateCorrupt}
	froms := make([]int32, 0, len(all))
	for _, s := range all {
		if IsValidSegmentTransition(s, to) {
			froms = append(froms, s)
		}
	}
	return froms
}

const liveRecordSegmentColumns = "SELECT id, record_id, room_id, live_session_id, seq, start_at, end_at, " +
	"duration_ms, state, bucket, object_key, size_bytes, checksum, worker_id, trace_id, registered_at, mtime"

type defaultLiveRecordSegmentModel struct {
	conn sqlx.SqlConn
}

// NewLiveRecordSegmentModel 构造 live_record_segment 的 model。
func NewLiveRecordSegmentModel(conn sqlx.SqlConn) LiveRecordSegmentModel {
	return &defaultLiveRecordSegmentModel{conn: conn}
}

func (m *defaultLiveRecordSegmentModel) InsertIgnore(ctx context.Context, s *LiveRecordSegment) (int64, error) {
	return m.InsertIgnoreTx(ctx, m.conn, s)
}

func (m *defaultLiveRecordSegmentModel) InsertIgnoreTx(ctx context.Context, sess sqlx.Session,
	s *LiveRecordSegment) (int64, error) {
	if s.Seq <= 0 {
		return 0, fmt.Errorf("live_record_segment InsertIgnore: seq=%d %w", s.Seq, ErrInvalidSeq)
	}
	if s.State == SegmentStateUploading || s.State == SegmentStateUploaded || s.State == SegmentStateVerified {
		// 非缺口切片必须有对象存储引用：只有 bucket+object_key 才能被 Worker 拼接与校验。
		if s.Bucket == "" || s.ObjectKey == "" {
			return 0, fmt.Errorf("live_record_segment seq=%d state=%d: %w", s.Seq, s.State, ErrInvalidBucketRef)
		}
	}
	now := nowUnix()
	if s.RegisteredAt == 0 {
		s.RegisteredAt = now
	}
	s.Mtime = now
	res, err := sess.ExecCtx(ctx,
		"INSERT IGNORE INTO live_record_segment (record_id, room_id, live_session_id, seq, start_at, end_at, "+
			"duration_ms, state, bucket, object_key, size_bytes, checksum, worker_id, trace_id, registered_at, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		s.RecordId, s.RoomId, s.LiveSession, s.Seq, s.StartAt, s.EndAt, s.DurationMs, s.State,
		s.Bucket, s.ObjectKey, s.SizeBytes, s.Checksum, s.WorkerId, s.TraceId, s.RegisteredAt, s.Mtime)
	if err != nil {
		return 0, fmt.Errorf("live_record_segment InsertIgnore: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_record_segment InsertIgnore RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultLiveRecordSegmentModel) InsertIgnoreMissing(ctx context.Context,
	rows []*LiveRecordSegment) (int64, error) {
	return m.InsertIgnoreMissingTx(ctx, m.conn, rows)
}

func (m *defaultLiveRecordSegmentModel) InsertIgnoreMissingTx(ctx context.Context, sess sqlx.Session,
	rows []*LiveRecordSegment) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	now := nowUnix()
	var sb strings.Builder
	args := make([]any, 0, len(rows)*11)
	sb.WriteString("INSERT IGNORE INTO live_record_segment (record_id, room_id, live_session_id, seq, start_at, " +
		"end_at, duration_ms, state, worker_id, trace_id, registered_at, mtime) VALUES ")
	for i, r := range rows {
		if r.Seq <= 0 {
			return 0, fmt.Errorf("live_record_segment InsertIgnoreMissing: index=%d %w", i, ErrInvalidSeq)
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
		// 缺口行不写 bucket/object_key：产物本来就不存在，写空引用会让回收误判为可删对象。
		args = append(args, r.RecordId, r.RoomId, r.LiveSession, r.Seq, r.StartAt, r.EndAt, r.DurationMs,
			SegmentStateMissing, r.WorkerId, r.TraceId, now, now)
	}
	res, err := sess.ExecCtx(ctx, sb.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("live_record_segment InsertIgnoreMissing: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_record_segment InsertIgnoreMissing RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultLiveRecordSegmentModel) FindOne(ctx context.Context, id int64) (*LiveRecordSegment, error) {
	var s LiveRecordSegment
	if err := m.conn.QueryRowCtx(ctx, &s, liveRecordSegmentColumns+" FROM live_record_segment WHERE id = ?", id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_record_segment FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultLiveRecordSegmentModel) FindBySeq(ctx context.Context, recordID, seq int64) (*LiveRecordSegment, error) {
	var s LiveRecordSegment
	query := liveRecordSegmentColumns + " FROM live_record_segment WHERE record_id = ? AND seq = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, recordID, seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_record_segment FindBySeq: %w", err)
	}
	return &s, nil
}

func (m *defaultLiveRecordSegmentModel) LastSeq(ctx context.Context, recordID int64) (int64, error) {
	var last int64
	query := "SELECT COALESCE(MAX(seq), 0) FROM live_record_segment WHERE record_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &last, query, recordID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_record_segment LastSeq: %w", err)
	}
	return last, nil
}

func (m *defaultLiveRecordSegmentModel) ListAfter(ctx context.Context, recordID, afterSeq int64, state int32,
	limit int32) ([]*LiveRecordSegment, error) {
	if limit <= 0 {
		limit = 200
	}
	where, args := buildWhere(
		whereFragment{"record_id = ?", []any{recordID}},
		whereFragment{"seq > ?", []any{}}.when(afterSeq > 0, afterSeq),
		whereFragment{"state = ?", []any{}}.when(state > 0, state),
	)
	listArgs := append(append([]any{}, args...), limit)
	var rows []*LiveRecordSegment
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveRecordSegmentColumns+" FROM live_record_segment "+where+" ORDER BY seq ASC LIMIT ?", listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_record_segment ListAfter: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveRecordSegmentModel) CountByRecord(ctx context.Context, recordID int64) (int64, error) {
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM live_record_segment WHERE record_id = ?", recordID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_record_segment CountByRecord: %w", err)
	}
	return total, nil
}

func (m *defaultLiveRecordSegmentModel) StatsInRange(ctx context.Context, recordID, fromSeq, toSeq int64) (*SegmentStats, error) {
	if fromSeq <= 0 {
		fromSeq = 1
	}
	if toSeq < fromSeq {
		return nil, fmt.Errorf("live_record_segment StatsInRange: [%d,%d] %w", fromSeq, toSeq, ErrInvalidSegmentRange)
	}
	// 一次条件聚合给出拼接决策所需的全部事实：可用切片数、缺口数、总时长、实际覆盖区间。
	query := "SELECT COUNT(*) AS registered, " +
		"COALESCE(SUM(state = ?), 0) AS uploading, " +
		"COALESCE(SUM(state = ?), 0) AS uploaded, " +
		"COALESCE(SUM(state = ?), 0) AS verified, " +
		"COALESCE(SUM(state = ?), 0) AS missing, " +
		"COALESCE(SUM(state = ?), 0) AS corrupt, " +
		"COALESCE(SUM(CASE WHEN state = ? THEN duration_ms ELSE 0 END), 0) AS duration_ms, " +
		"COALESCE(MIN(seq), 0) AS min_seq, COALESCE(MAX(seq), 0) AS max_seq " +
		"FROM live_record_segment WHERE record_id = ? AND seq BETWEEN ? AND ?"
	var st SegmentStats
	if err := m.conn.QueryRowCtx(ctx, &st, query, SegmentStateUploading, SegmentStateUploaded, SegmentStateVerified,
		SegmentStateMissing, SegmentStateCorrupt, SegmentStateVerified, recordID, fromSeq, toSeq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &SegmentStats{}, nil
		}
		return nil, fmt.Errorf("live_record_segment StatsInRange: %w", err)
	}
	return &st, nil
}

func (m *defaultLiveRecordSegmentModel) UpdateState(ctx context.Context, recordID, seq int64, to int32,
	patch SegmentPatch) (int64, error) {
	return m.UpdateStateTx(ctx, m.conn, recordID, seq, to, patch)
}

func (m *defaultLiveRecordSegmentModel) UpdateStateTx(ctx context.Context, sess sqlx.Session,
	recordID, seq int64, to int32, patch SegmentPatch) (int64, error) {
	fromStates := segmentFromStates(to)
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_record_segment UpdateState: to=%d %w", to, ErrInvalidTransition)
	}
	sets := append(patch.sets(), columnValue{"state", to})
	return conditionalUpdate(ctx, sess, "live_record_segment", sets, false, []whereFragment{
		{"record_id = ?", []any{recordID}},
		{"seq = ?", []any{seq}},
		stateInFragment(fromStates),
	})
}

func (m *defaultLiveRecordSegmentModel) PurgeByRecord(ctx context.Context, recordID int64, limit int32) (int64, error) {
	return m.PurgeByRecordTx(ctx, m.conn, recordID, limit)
}

func (m *defaultLiveRecordSegmentModel) PurgeByRecordTx(ctx context.Context, sess sqlx.Session,
	recordID int64, limit int32) (int64, error) {
	if recordID <= 0 {
		return 0, fmt.Errorf("live_record_segment PurgeByRecord: record_id=%d %w", recordID, ErrRecordTaskNotFound)
	}
	if limit <= 0 {
		limit = 100
	}
	res, err := sess.ExecCtx(ctx,
		"DELETE FROM live_record_segment WHERE record_id = ? AND state <> ? LIMIT ?",
		recordID, SegmentStateUploading, limit)
	if err != nil {
		return 0, fmt.Errorf("live_record_segment PurgeByRecord: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_record_segment PurgeByRecord RowsAffected: %w", err)
	}
	return aff, nil
}
