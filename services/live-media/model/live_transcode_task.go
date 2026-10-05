package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveTranscodeTask 直播转码任务记录（live_transcode_task）。
// 一条记录 = 「某房间某场次下，某个码率档位的一次转码生命周期」。
// 真实 FFmpeg 不在本服务进程内跑：本表只登记任务、由 Worker 领取并回报状态。
type LiveTranscodeTask struct {
	TaskId       int64  `db:"task_id"`         // 任务 ID（自增主键）
	RoomId       int64  `db:"room_id"`         // 直播间 ID（live-room 主键）
	LiveSession  int64  `db:"live_session_id"` // 直播场次 ID
	TemplateId   int64  `db:"template_id"`     // 转码模板 ID（transcode 主键，只引用）
	BitrateLevel int32  `db:"bitrate_level"`   // 码率档位（rpc.BitrateLevel）
	Protocol     int32  `db:"protocol"`        // 输出协议（rpc.StreamProtocol）
	SourceRef    string `db:"source_ref"`      // 拉流源引用（短期地址/流标识，禁止存长期密钥）
	AnchorMid    int64  `db:"anchor_mid"`      // 主播 mid（仅审计）
	State        int32  `db:"state"`           // 状态（TranscodeState*）
	Progress     int32  `db:"progress"`        // 0-100；RUNNING 时表示源流健康度采样
	Attempt      int32  `db:"attempt"`         // 已执行次数
	MaxAttempts  int32  `db:"max_attempts"`    // 重试上限
	StartedAt    int64  `db:"started_at"`      // 实际启动（Unix 秒）
	StoppedAt    int64  `db:"stopped_at"`      // 实际停止（Unix 秒）
	HeartbeatAt  int64  `db:"heartbeat_at"`    // 最近 Worker 心跳（Unix 秒）
	TimeoutAt    int64  `db:"timeout_at"`      // 心跳超时判定时刻（Unix 秒）
	Version      int64  `db:"version"`         // 乐观并发版本
	Reason       int32  `db:"reason"`          // 失败/停止原因（rpc.FailureReason）
	Errno        int32  `db:"errno"`           // 错误码
	ErrMsg       string `db:"err_msg"`         // 脱敏错误信息
	RequestId    string `db:"request_id"`      // 幂等键（唯一索引）
	TraceId      string `db:"trace_id"`        // 最近一次调用 trace_id
	Ctime        int64  `db:"ctime"`           // 创建时间（Unix 秒）
	Mtime        int64  `db:"mtime"`           // 修改时间（Unix 秒）
}

// TranscodeTaskFilter List 的过滤条件；零值字段表示不参与过滤。
type TranscodeTaskFilter struct {
	RoomId      int64
	SessionId   int64
	State       int32
	TemplateId  int64
	Pn          int32
	Ps          int32
	MaxPageSize int32 // 来自配置 LiveMedia.MaxListPageSize
}

// TranscodePatch 状态推进时随带写入的字段；nil 表示该列不更新。
// 用指针而不是零值判断，是因为 0 对这些列本身就是合法取值（如 progress=0、stopped_at 未设置）。
type TranscodePatch struct {
	Progress    *int32
	StartedAt   *int64
	StoppedAt   *int64
	HeartbeatAt *int64
	TimeoutAt   *int64
	Attempt     *int32
	Reason      *int32
	Errno       *int32
	ErrMsg      *string
	TraceID     *string
}

func (p TranscodePatch) sets() []columnValue {
	sets := make([]columnValue, 0, 10)
	if p.Progress != nil {
		sets = append(sets, columnValue{"progress", *p.Progress})
	}
	if p.StartedAt != nil {
		sets = append(sets, columnValue{"started_at", *p.StartedAt})
	}
	if p.StoppedAt != nil {
		sets = append(sets, columnValue{"stopped_at", *p.StoppedAt})
	}
	if p.HeartbeatAt != nil {
		sets = append(sets, columnValue{"heartbeat_at", *p.HeartbeatAt})
	}
	if p.TimeoutAt != nil {
		sets = append(sets, columnValue{"timeout_at", *p.TimeoutAt})
	}
	if p.Attempt != nil {
		sets = append(sets, columnValue{"attempt", *p.Attempt})
	}
	if p.Reason != nil {
		sets = append(sets, columnValue{"reason", *p.Reason})
	}
	if p.Errno != nil {
		sets = append(sets, columnValue{"errno", *p.Errno})
	}
	if p.ErrMsg != nil {
		sets = append(sets, columnValue{"err_msg", *p.ErrMsg})
	}
	if p.TraceID != nil {
		sets = append(sets, columnValue{"trace_id", *p.TraceID})
	}
	return sets
}

// LiveTranscodeTaskModel live_transcode_task 表的查询与写入接口。
type LiveTranscodeTaskModel interface {
	// Insert 新建任务并返回 task_id；request_id 撞唯一索引时返回包装 ErrRequestIdDuplicated 的错误。
	Insert(ctx context.Context, t *LiveTranscodeTask) (int64, error)
	// InsertTx 与 Insert 同语义，但写入走调用方给定的事务会话（AGENTS.md §5：
	// 「任务行 + Outbox 事件」必须同事务提交，否则事件必然丢）。
	InsertTx(ctx context.Context, sess sqlx.Session, t *LiveTranscodeTask) (int64, error)
	// FindOne 查询单任务；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, taskID int64) (*LiveTranscodeTask, error)
	// FindByRequestID 幂等重放：按 request_id 查已存在的任务，不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*LiveTranscodeTask, error)
	// FindActiveByRoom 查房间当前处于 PENDING/RUNNING/STOPPING 的同档位任务（重复开播复用），
	// 不存在返回 (nil, nil)。
	FindActiveByRoom(ctx context.Context, roomID, sessionID int64, bitrateLevel, protocol int32) (*LiveTranscodeTask, error)
	// List 分页查询。
	List(ctx context.Context, f TranscodeTaskFilter) ([]*LiveTranscodeTask, int32, error)
	// UpdateState 条件 UPDATE 推进状态：只有 state ∈ fromStates 且（expectedVersion<=0 或
	// version=expectedVersion）才写入，同时 version+1、mtime=now。返回受影响行数。
	// 0 行表示前置状态或版本不匹配，由调用方区分 ErrTranscodeTaskNotFound / ErrVersionConflict。
	UpdateState(ctx context.Context, taskID int64, fromStates []int32, expectedVersion int64,
		to int32, patch TranscodePatch) (int64, error)
	// UpdateStateTx 与 UpdateState 同语义，但条件 UPDATE 跑在调用方的事务里。
	UpdateStateTx(ctx context.Context, sess sqlx.Session, taskID int64, fromStates []int32,
		expectedVersion int64, to int32, patch TranscodePatch) (int64, error)
	// ListTimedOut 扫描超过 timeout_at 仍未终态的任务（超时清扫 Worker 用），按 timeout_at 升序。
	ListTimedOut(ctx context.Context, now int64, limit int32) ([]*LiveTranscodeTask, error)
}

const liveTranscodeTaskColumns = "SELECT task_id, room_id, live_session_id, template_id, bitrate_level, protocol, " +
	"source_ref, anchor_mid, state, progress, attempt, max_attempts, started_at, stopped_at, heartbeat_at, " +
	"timeout_at, version, reason, errno, err_msg, request_id, trace_id, ctime, mtime"

type defaultLiveTranscodeTaskModel struct {
	conn sqlx.SqlConn
}

// NewLiveTranscodeTaskModel 构造 live_transcode_task 的 model。
func NewLiveTranscodeTaskModel(conn sqlx.SqlConn) LiveTranscodeTaskModel {
	return &defaultLiveTranscodeTaskModel{conn: conn}
}

func (m *defaultLiveTranscodeTaskModel) Insert(ctx context.Context, t *LiveTranscodeTask) (int64, error) {
	return m.InsertTx(ctx, m.conn, t)
}

func (m *defaultLiveTranscodeTaskModel) InsertTx(ctx context.Context, sess sqlx.Session,
	t *LiveTranscodeTask) (int64, error) {
	now := nowUnix()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	if t.Mtime == 0 {
		t.Mtime = now
	}
	if t.Version == 0 {
		t.Version = 1
	}
	res, err := sess.ExecCtx(ctx,
		"INSERT INTO live_transcode_task (room_id, live_session_id, template_id, bitrate_level, protocol, "+
			"source_ref, anchor_mid, state, progress, attempt, max_attempts, started_at, stopped_at, heartbeat_at, "+
			"timeout_at, version, reason, errno, err_msg, request_id, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.RoomId, t.LiveSession, t.TemplateId, t.BitrateLevel, t.Protocol, t.SourceRef, t.AnchorMid,
		t.State, t.Progress, t.Attempt, t.MaxAttempts, t.StartedAt, t.StoppedAt, t.HeartbeatAt,
		t.TimeoutAt, t.Version, t.Reason, t.Errno, t.ErrMsg, t.RequestId, t.TraceId, t.Ctime, t.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			// 唯一索引 uniq_request_id 命中：调用方改用 FindByRequestID 取回既有任务。
			return 0, fmt.Errorf("request_id=%s: %w", t.RequestId, ErrRequestIdDuplicated)
		}
		return 0, fmt.Errorf("live_transcode_task Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_transcode_task Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveTranscodeTaskModel) FindOne(ctx context.Context, taskID int64) (*LiveTranscodeTask, error) {
	var t LiveTranscodeTask
	query := liveTranscodeTaskColumns + " FROM live_transcode_task WHERE task_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_transcode_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveTranscodeTaskModel) FindByRequestID(ctx context.Context, requestID string) (*LiveTranscodeTask, error) {
	var t LiveTranscodeTask
	query := liveTranscodeTaskColumns + " FROM live_transcode_task WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_transcode_task FindByRequestID: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveTranscodeTaskModel) FindActiveByRoom(ctx context.Context, roomID, sessionID int64, bitrateLevel, protocol int32) (*LiveTranscodeTask, error) {
	var t LiveTranscodeTask
	// 进行中的状态集合与状态机一致：终态任务不参与复用，避免把已停止的历史任务当活跃任务返回。
	query := liveTranscodeTaskColumns + " FROM live_transcode_task WHERE room_id = ? AND live_session_id = ? " +
		"AND bitrate_level = ? AND protocol = ? AND state IN (?, ?, ?) ORDER BY task_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &t, query, roomID, sessionID, bitrateLevel, protocol,
		TranscodeStatePending, TranscodeStateRunning, TranscodeStateStopping); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_transcode_task FindActiveByRoom: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveTranscodeTaskModel) List(ctx context.Context, f TranscodeTaskFilter) ([]*LiveTranscodeTask, int32, error) {
	where, args := buildWhere(
		whereFragment{"room_id = ?", []any{}}.when(f.RoomId > 0, f.RoomId),
		whereFragment{"live_session_id = ?", []any{}}.when(f.SessionId > 0, f.SessionId),
		whereFragment{"state = ?", []any{}}.when(f.State > 0, f.State),
		whereFragment{"template_id = ?", []any{}}.when(f.TemplateId > 0, f.TemplateId),
	)
	maxPS := f.MaxPageSize
	if maxPS <= 0 {
		maxPS = 50
	}
	limit, offset := clampPage(f.Pn, f.Ps, maxPS)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_transcode_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_transcode_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveTranscodeTask
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveTranscodeTaskColumns+" FROM live_transcode_task "+where+
			" ORDER BY task_id DESC LIMIT ? OFFSET ?", listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_transcode_task List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveTranscodeTaskModel) UpdateState(ctx context.Context, taskID int64, fromStates []int32,
	expectedVersion int64, to int32, patch TranscodePatch) (int64, error) {
	return m.UpdateStateTx(ctx, m.conn, taskID, fromStates, expectedVersion, to, patch)
}

func (m *defaultLiveTranscodeTaskModel) UpdateStateTx(ctx context.Context, sess sqlx.Session, taskID int64,
	fromStates []int32, expectedVersion int64, to int32, patch TranscodePatch) (int64, error) {
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_transcode_task UpdateState: empty fromStates %w", ErrInvalidTransition)
	}
	sets := append(patch.sets(), columnValue{"state", to})
	wheres := []whereFragment{
		{"task_id = ?", []any{taskID}},
		stateInFragment(fromStates),
	}
	if expectedVersion > 0 {
		wheres = append(wheres, whereFragment{"version = ?", []any{expectedVersion}})
	}
	return conditionalUpdate(ctx, sess, "live_transcode_task", sets, true, wheres)
}

func (m *defaultLiveTranscodeTaskModel) ListTimedOut(ctx context.Context, now int64, limit int32) ([]*LiveTranscodeTask, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*LiveTranscodeTask
	// 只扫 RUNNING/STOPPING：这两个状态有 heartbeat/timeout 语义，PENDING 由 Worker 领取超时另算。
	query := liveTranscodeTaskColumns + " FROM live_transcode_task " +
		"WHERE state IN (?, ?) AND timeout_at > 0 AND timeout_at < ? ORDER BY timeout_at ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, TranscodeStateRunning, TranscodeStateStopping, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_transcode_task ListTimedOut: %w", err)
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// 本包共享的条件更新构件（其它 live_* 状态表的 model 文件复用）
//
// AGENTS.md §5 要求所有写接口可幂等、状态推进需可验证：这里集中实现
// 「条件 UPDATE + RowsAffected」，禁止读-改-写两步更新（并发下会丢状态推进）。
// ---------------------------------------------------------------------------

// columnValue 一个待写入列与其值。
type columnValue struct {
	Column string
	Value  any
}

// rawExpr 需要在 SET 里直接嵌入表达式的赋值（例如 last_seq = GREATEST(last_seq, ?)）。
// SQL 只能由本包常量拼接，外部输入必须走 Args 占位符，禁止拼进 SQL（防注入）。
type rawExpr struct {
	SQL  string
	Args []any
}

// whereFragment 一个 WHERE 条件片段及其参数。
type whereFragment struct {
	SQL  string
	Args []any
}

// when 条件成立时把 value 作为该片段的参数，否则返回空片段（buildWhere 会丢弃）。
func (w whereFragment) when(ok bool, value any) whereFragment {
	if !ok {
		return whereFragment{}
	}
	return whereFragment{SQL: w.SQL, Args: []any{value}}
}

// empty 判断片段是否不参与拼接。
func (w whereFragment) empty() bool { return w.SQL == "" }

// buildWhere 拼接条件片段，无条件时返回 "WHERE 1=1"（永不退化成全表扫描语句）。
func buildWhere(frags ...whereFragment) (string, []any) {
	parts := make([]string, 0, len(frags)+1)
	args := make([]any, 0, len(frags))
	parts = append(parts, "1=1")
	for _, f := range frags {
		if f.empty() {
			continue
		}
		parts = append(parts, f.SQL)
		args = append(args, f.Args...)
	}
	return "WHERE " + strings.Join(parts, " AND "), args
}

// stateInFragment 生成 "state IN (?, ?, ...)"。
func stateInFragment(states []int32) whereFragment {
	parts := make([]string, 0, len(states))
	args := make([]any, 0, len(states))
	for _, s := range states {
		parts = append(parts, "?")
		args = append(args, s)
	}
	return whereFragment{SQL: "state IN (" + strings.Join(parts, ", ") + ")", Args: args}
}

// conditionalUpdate 执行「条件 UPDATE」：自动追加 version = version + 1（bumpVersion 时）与
// mtime = now，返回受影响行数。0 行不是错误，由调用方按业务语义翻译成 NotFound / Conflict。
// sets 为空或 wheres 为空都会被拒绝：前者是无效 SQL，后者等于全表更新。
// sess 取 sqlx.Session 而非 SqlConn：同一份条件 UPDATE 既能在自动提交下跑，
// 也能被 logic 放进 TransactCtx 的事务里（sqlx.SqlConn 本身实现了 Session）。
func conditionalUpdate(ctx context.Context, sess sqlx.Session, table string, sets []columnValue,
	bumpVersion bool, wheres []whereFragment) (int64, error) {
	if len(sets) == 0 {
		return 0, fmt.Errorf("%s conditionalUpdate: empty SET", table)
	}
	whereSQL, whereArgs := buildWhere(wheres...)
	if len(whereArgs) == 0 {
		// buildWhere 在无片段时只产生 "WHERE 1=1"：条件更新必须有定位条件，否则拒绝。
		return 0, fmt.Errorf("%s conditionalUpdate: empty WHERE", table)
	}
	var sb strings.Builder
	sb.WriteString("UPDATE ")
	sb.WriteString(table)
	sb.WriteString(" SET ")
	args := make([]any, 0, len(sets)+len(whereArgs)+2)
	for i, s := range sets {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(s.Column)
		sb.WriteString(" = ")
		if expr, ok := s.Value.(rawExpr); ok {
			sb.WriteString(expr.SQL)
			args = append(args, expr.Args...)
			continue
		}
		sb.WriteString("?")
		args = append(args, s.Value)
	}
	if bumpVersion {
		sb.WriteString(", version = version + 1")
	}
	sb.WriteString(", mtime = ?")
	args = append(args, nowUnix())
	sb.WriteString(" ")
	sb.WriteString(whereSQL)
	args = append(args, whereArgs...)

	res, err := sess.ExecCtx(ctx, sb.String(), args...)
	if err != nil {
		return 0, fmt.Errorf("%s conditionalUpdate: %w", table, err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s conditionalUpdate RowsAffected: %w", table, err)
	}
	return aff, nil
}

// clampPage 归一化 pn/ps：ps 越界时夹到默认 20 与上限 maxPS 之间，返回 (limit, offset)。
func clampPage(pn, ps, maxPS int32) (limit, offset int32) {
	if maxPS <= 0 {
		maxPS = 50
	}
	if pn < 1 {
		pn = 1
	}
	switch {
	case ps <= 0 || ps > maxPS:
		limit = 20
		if limit > maxPS {
			limit = maxPS
		}
	default:
		limit = ps
	}
	return limit, (pn - 1) * limit
}
