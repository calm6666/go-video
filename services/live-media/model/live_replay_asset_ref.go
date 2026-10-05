package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveReplayAssetRef 回放产物与 asset/稿件的引用关系（live_replay_asset_ref）。
//
// 这张表是「回放走普通视频审核发布链路」的证据链：
//   - asset_id / aid / bvid 只是主键引用，本服务不写 asset_meta、不写 video_submission；
//   - review_state / published_at 是 video 事实状态在本地的只读投影，
//     只由 ApplyReplayContentState（或 content.published.v1 消费者）写入，
//     方向单一 video → live-media，绝不反向推进稿件；
//   - last_event_id 让投影同步按事件幂等（同一 event_id 重放不再刷新）。
//
// 唯一键 uniq_replay_id / uniq_asset_id / uniq_aid 保证「一场回放一个产物一份稿件」，
// 且一个媒资/稿件不会被两条回放引用串用（BindReplayAsset 覆盖不同值会撞唯一键）。
type LiveReplayAssetRef struct {
	Id             int64  `db:"id"`
	RoomId         int64  `db:"room_id"`
	LiveSession    int64  `db:"live_session_id"`
	ReplayId       int64  `db:"replay_id"`
	RecordId       int64  `db:"record_id"`
	AssetId        int64  `db:"asset_id"`
	Aid            int64  `db:"aid"`
	Bvid           string `db:"bvid"`
	AnchorMid      int64  `db:"anchor_mid"`
	Bucket         string `db:"bucket"`
	ObjectKey      string `db:"object_key"`
	DurationMs     int64  `db:"duration_ms"`
	SegmentFromSeq int64  `db:"segment_from_seq"`
	SegmentToSeq   int64  `db:"segment_to_seq"`
	GapCount       int64  `db:"gap_count"`
	ReviewState    int32  `db:"review_state"`    // 投影：ReviewState*（0 未同步）
	ReviewStateAt  int64  `db:"review_state_at"` // 投影同步时间（Unix 秒）
	RetentionState int32  `db:"retention_state"` // RefRetentionState*
	PublishedAt    int64  `db:"published_at"`    // video 侧发布时间（投影值，0 未发布）
	LastEventId    string `db:"last_event_id"`   // 最近一次驱动投影的事件 ID
	Source         string `db:"source"`          // 投影来源：content.published.v1 / video.rpc / manual
	RequestId      string `db:"request_id"`      // 绑定动作的幂等键
	TraceId        string `db:"trace_id"`
	Ctime          int64  `db:"ctime"`
	Mtime          int64  `db:"mtime"`
}

// ReplayRefFilter List 过滤条件。
type ReplayRefFilter struct {
	RoomId       int64
	SessionId    int64
	AnchorMid    int64
	ReviewState  int32 // <=0 不过滤（0 也是有意义的「未同步」，用 WithUnsynced 精确表达）
	OnlyUnsynced bool  // true 时只返回 review_state=0 的引用（投影补刷任务用）
	Pn           int32
	Ps           int32
	MaxPageSize  int32
}

// LiveReplayAssetRefModel live_replay_asset_ref 表接口。
type LiveReplayAssetRefModel interface {
	// Upsert 绑定/重放引用：天然键 replay_id 已存在时刷新展示与产物字段，
	// 但绝不覆盖投影列（review_state/published_at/last_event_id 由投影通道独占）。
	Upsert(ctx context.Context, r *LiveReplayAssetRef) (int64, error)
	// UpsertTx 与 Upsert 同语义，但写入跑在调用方事务里。
	UpsertTx(ctx context.Context, sess sqlx.Session, r *LiveReplayAssetRef) (int64, error)
	FindOne(ctx context.Context, id int64) (*LiveReplayAssetRef, error)
	FindByReplayID(ctx context.Context, replayID int64) (*LiveReplayAssetRef, error)
	FindByAssetID(ctx context.Context, assetID int64) (*LiveReplayAssetRef, error)
	FindByAid(ctx context.Context, aid int64) (*LiveReplayAssetRef, error)
	List(ctx context.Context, f ReplayRefFilter) ([]*LiveReplayAssetRef, int32, error)
	// ApplyContentState 写入 video 侧投影，返回受影响行数。
	// 幂等与乱序保护都写在同一条 UPDATE 的 WHERE 里：
	//   last_event_id <> eventID  —— 同一事件重放不再刷新；
	//   review_state_at <= at     —— 旧事件不会把新投影覆盖回去。
	// 0 行需要由调用方回读区分「事件已应用过」与「引用不存在」。
	ApplyContentState(ctx context.Context, in ContentStateApply) (int64, error)
	// ApplyContentStateTx 与 ApplyContentState 同语义，但条件 UPDATE 跑在调用方事务里
	// （投影 + 回放任务终态 + Outbox 必须同生共死，AGENTS.md §5）。
	ApplyContentStateTx(ctx context.Context, sess sqlx.Session, in ContentStateApply) (int64, error)
	// MarkRetentionState 回收流程推进引用行的生命周期（正常→待回收→已回收），
	// 条件 UPDATE 校验当前值，返回受影响行数。
	MarkRetentionState(ctx context.Context, id int64, from, to int32) (int64, error)
	// MarkRetentionStateTx 与 MarkRetentionState 同语义，但条件 UPDATE 跑在调用方事务里。
	MarkRetentionStateTx(ctx context.Context, sess sqlx.Session, id int64, from, to int32) (int64, error)
}

// ContentStateApply 是 ApplyContentState 的入参：replay_id 与 asset_id 至少给一个。
type ContentStateApply struct {
	ReplayId    int64
	AssetId     int64
	ReviewState int32  // 必填：来自 video 的事实状态（1..5）
	PublishedAt int64  // video 侧发布时间（Unix 秒，0 表示不刷新）
	StateAt     int64  // 投影时刻（Unix 秒，0 表示服务端取当前时间）
	EventId     string // 驱动本次同步的事件 ID（按 event_id 幂等，必填）
	Source      string // content.published.v1 / video.rpc / manual
	TraceId     string
}

const liveReplayAssetRefColumns = "SELECT id, room_id, live_session_id, replay_id, record_id, asset_id, aid, bvid, " +
	"anchor_mid, bucket, object_key, duration_ms, segment_from_seq, segment_to_seq, gap_count, review_state, " +
	"review_state_at, retention_state, published_at, last_event_id, source, request_id, trace_id, ctime, mtime"

type defaultLiveReplayAssetRefModel struct {
	conn sqlx.SqlConn
}

// NewLiveReplayAssetRefModel 构造 live_replay_asset_ref 的 model。
func NewLiveReplayAssetRefModel(conn sqlx.SqlConn) LiveReplayAssetRefModel {
	return &defaultLiveReplayAssetRefModel{conn: conn}
}

func (m *defaultLiveReplayAssetRefModel) Upsert(ctx context.Context, r *LiveReplayAssetRef) (int64, error) {
	return m.UpsertTx(ctx, m.conn, r)
}

func (m *defaultLiveReplayAssetRefModel) UpsertTx(ctx context.Context, sess sqlx.Session,
	r *LiveReplayAssetRef) (int64, error) {
	if r.ReplayId <= 0 {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: replay_id=%d %w", r.ReplayId, ErrInvalidTransition)
	}
	if r.AssetId <= 0 {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: asset_id=%d %w", r.AssetId, ErrInvalidAssetID)
	}
	if r.Aid <= 0 {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: aid=%d %w", r.Aid, ErrInvalidAid)
	}
	if r.Bucket == "" || r.ObjectKey == "" {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: replay_id=%d %w", r.ReplayId, ErrInvalidBucketRef)
	}
	now := nowUnix()
	if r.ReviewState == 0 {
		// 绑定时刻拿不到 video 结论：投影保持未同步，由 ApplyReplayContentState 后续刷新。
		r.ReviewState = ReviewStateUnsynced
		r.ReviewStateAt = 0
	}
	res, err := sess.ExecCtx(ctx,
		"INSERT INTO live_replay_asset_ref (room_id, live_session_id, replay_id, record_id, asset_id, aid, bvid, "+
			"anchor_mid, bucket, object_key, duration_ms, segment_from_seq, segment_to_seq, gap_count, review_state, "+
			"review_state_at, retention_state, published_at, last_event_id, source, request_id, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE room_id = VALUES(room_id), live_session_id = VALUES(live_session_id), "+
			"record_id = VALUES(record_id), bvid = VALUES(bvid), anchor_mid = VALUES(anchor_mid), "+
			"bucket = VALUES(bucket), object_key = VALUES(object_key), duration_ms = VALUES(duration_ms), "+
			"segment_from_seq = VALUES(segment_from_seq), segment_to_seq = VALUES(segment_to_seq), "+
			"gap_count = VALUES(gap_count), request_id = VALUES(request_id), trace_id = VALUES(trace_id), mtime = VALUES(mtime)",
		r.RoomId, r.LiveSession, r.ReplayId, r.RecordId, r.AssetId, r.Aid, r.Bvid, r.AnchorMid, r.Bucket,
		r.ObjectKey, r.DurationMs, r.SegmentFromSeq, r.SegmentToSeq, r.GapCount, r.ReviewState, r.ReviewStateAt,
		r.RetentionState, r.PublishedAt, r.LastEventId, r.Source, r.RequestId, r.TraceId, now, now)
	if err != nil {
		if isDuplicateErr(err) {
			// uniq_asset_id / uniq_aid 命中：该媒资或稿件已被别的回放引用，禁止串用覆盖。
			return 0, fmt.Errorf("replay_id=%d asset_id=%d aid=%d: %w", r.ReplayId, r.AssetId, r.Aid, ErrAssetRefConflict)
		}
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert LastInsertId: %w", err)
	}
	if id > 0 {
		return id, nil
	}
	// 走 ON DUPLICATE 分支时 LastInsertId 不可靠，用 replay_id 回读主键。
	row, err := m.FindByReplayID(ctx, r.ReplayId)
	if err != nil {
		return 0, err
	}
	if row == nil {
		return 0, fmt.Errorf("live_replay_asset_ref Upsert: replay_id=%d %w", r.ReplayId, ErrReplayRefNotFound)
	}
	return row.Id, nil
}

func (m *defaultLiveReplayAssetRefModel) FindOne(ctx context.Context, id int64) (*LiveReplayAssetRef, error) {
	var r LiveReplayAssetRef
	query := liveReplayAssetRefColumns + " FROM live_replay_asset_ref WHERE id = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_asset_ref FindOne: %w", err)
	}
	return &r, nil
}

func (m *defaultLiveReplayAssetRefModel) FindByReplayID(ctx context.Context, replayID int64) (*LiveReplayAssetRef, error) {
	var r LiveReplayAssetRef
	query := liveReplayAssetRefColumns + " FROM live_replay_asset_ref WHERE replay_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, replayID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_asset_ref FindByReplayID: %w", err)
	}
	return &r, nil
}

func (m *defaultLiveReplayAssetRefModel) FindByAssetID(ctx context.Context, assetID int64) (*LiveReplayAssetRef, error) {
	if assetID <= 0 {
		return nil, ErrInvalidAssetID
	}
	var r LiveReplayAssetRef
	query := liveReplayAssetRefColumns + " FROM live_replay_asset_ref WHERE asset_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, assetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_asset_ref FindByAssetID: %w", err)
	}
	return &r, nil
}

func (m *defaultLiveReplayAssetRefModel) FindByAid(ctx context.Context, aid int64) (*LiveReplayAssetRef, error) {
	if aid <= 0 {
		return nil, ErrInvalidAid
	}
	var r LiveReplayAssetRef
	query := liveReplayAssetRefColumns + " FROM live_replay_asset_ref WHERE aid = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, aid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_replay_asset_ref FindByAid: %w", err)
	}
	return &r, nil
}

func (m *defaultLiveReplayAssetRefModel) List(ctx context.Context, f ReplayRefFilter) ([]*LiveReplayAssetRef, int32, error) {
	frags := []whereFragment{
		whereFragment{"room_id = ?", []any{}}.when(f.RoomId > 0, f.RoomId),
		whereFragment{"live_session_id = ?", []any{}}.when(f.SessionId > 0, f.SessionId),
		whereFragment{"anchor_mid = ?", []any{}}.when(f.AnchorMid > 0, f.AnchorMid),
		whereFragment{"review_state = ?", []any{}}.when(f.ReviewState > 0, f.ReviewState),
	}
	if f.OnlyUnsynced {
		frags = append(frags, whereFragment{"review_state = ?", []any{ReviewStateUnsynced}})
	}
	where, args := buildWhere(frags...)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_replay_asset_ref "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_replay_asset_ref List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveReplayAssetRef
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveReplayAssetRefColumns+" FROM live_replay_asset_ref "+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_replay_asset_ref List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveReplayAssetRefModel) ApplyContentState(ctx context.Context, in ContentStateApply) (int64, error) {
	return m.ApplyContentStateTx(ctx, m.conn, in)
}

func (m *defaultLiveReplayAssetRefModel) ApplyContentStateTx(ctx context.Context, sess sqlx.Session,
	in ContentStateApply) (int64, error) {
	if !ValidReviewState(in.ReviewState) {
		return 0, fmt.Errorf("live_replay_asset_ref ApplyContentState: review_state=%d %w", in.ReviewState, ErrInvalidReviewState)
	}
	if in.EventId == "" {
		return 0, ErrEmptyEventID
	}
	key := whereFragment{}
	switch {
	case in.ReplayId > 0:
		key = whereFragment{"replay_id = ?", []any{in.ReplayId}}
	case in.AssetId > 0:
		key = whereFragment{"asset_id = ?", []any{in.AssetId}}
	default:
		// replay_id 与 asset_id 都没给：宁可报错也不能退化成全表刷新投影。
		return 0, ErrReplayRefNotFound
	}
	stateAt := in.StateAt
	if stateAt <= 0 {
		stateAt = nowUnix()
	}
	sets := []columnValue{
		{"review_state", in.ReviewState},
		{"review_state_at", stateAt},
		// published_at 单调：撤销发布由 review_state 表达，本地时间戳只记录「首次被投影为已发布」。
		{"published_at", rawExpr{"GREATEST(published_at, ?)", []any{in.PublishedAt}}},
		{"last_event_id", in.EventId},
		{"source", in.Source},
	}
	if in.TraceId != "" {
		sets = append(sets, columnValue{"trace_id", in.TraceId})
	}
	wheres := []whereFragment{
		key,
		{"last_event_id <> ?", []any{in.EventId}},
		{"review_state_at <= ?", []any{stateAt}},
	}
	return conditionalUpdate(ctx, sess, "live_replay_asset_ref", sets, false, wheres)
}

func (m *defaultLiveReplayAssetRefModel) MarkRetentionState(ctx context.Context, id int64, from, to int32) (int64, error) {
	return m.MarkRetentionStateTx(ctx, m.conn, id, from, to)
}

func (m *defaultLiveReplayAssetRefModel) MarkRetentionStateTx(ctx context.Context, sess sqlx.Session,
	id int64, from, to int32) (int64, error) {
	if !ValidRefRetentionState(from) || !IsValidRefRetentionTransition(from, to) {
		return 0, fmt.Errorf("live_replay_asset_ref MarkRetentionState: %d->%d %w", from, to, ErrInvalidTransition)
	}
	return conditionalUpdate(ctx, sess, "live_replay_asset_ref",
		[]columnValue{{"retention_state", to}}, false, []whereFragment{
			{"id = ?", []any{id}},
			{"retention_state = ?", []any{from}},
		})
}
