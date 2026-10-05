package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// streamColumns 是 live_stream 的列清单，必须与
// deploy/migrations/live-ingest/000002_create_live_stream_tables.sql 完全一致。
// 表里没有密钥列：只有 key_id 引用（README「不保存长期明文推流密钥」）。
const streamColumns = "stream_id, key_id, stream_name, room_id, session_id, anchor_mid, protocol, node_id, " +
	"state, seq, publish_request_id, publish_started_at, state_changed_at, last_heartbeat_at, " +
	"interrupted_total_seconds, interruption_count, stop_reason, stop_detail, health_state, health_reported_at, " +
	"video_bitrate_bps, audio_bitrate_bps, fps_x100, packet_loss_ppm, trace_id, ctime, mtime"

// Stream 一条推流会话（live_stream 表投影）。
//
// stream_id 由 VerifyPublishAuth 建档时分配（ULID），代表「一次推流会话」而不是
// 一个流标识：下播后重新开播是新 stream_id，断流重连复用同一 stream_id。
// seq 是该流 live.state.v1 事件的单调序号，只有 ApplyTransition 成功才会 +1，
// 因此消费方（live-room）可以用它拒绝乱序回退。
type Stream struct {
	StreamID             string `db:"stream_id"`                 // 推流会话 ID（ULID，主键）
	KeyID                int64  `db:"key_id"`                    // 使用的密钥 ID（引用，不含密钥内容）
	StreamName           string `db:"stream_name"`               // 流标识（冗余，排障用）
	RoomID               int64  `db:"room_id"`                   // 房间引用（live-room 主键）
	SessionID            int64  `db:"session_id"`                // 场次引用，0 表示未绑定
	AnchorMid            int64  `db:"anchor_mid"`                // 主播 ID
	Protocol             int32  `db:"protocol"`                  // 接入协议，见 rpc.IngestProtocol
	NodeID               string `db:"node_id"`                   // 当前接入节点，空串表示未分配
	State                int32  `db:"state"`                     // 流状态，见 StreamState*
	Seq                  int64  `db:"seq"`                       // 当前事件序号（单调递增）
	PublishRequestID     string `db:"publish_request_id"`        // 建档来源的鉴权幂等键（唯一索引）
	PublishStartedAt     int64  `db:"publish_started_at"`        // 首次进入 PUBLISHING（Unix 秒）
	StateChangedAt       int64  `db:"state_changed_at"`          // 最近一次状态变更（Unix 秒）
	LastHeartbeatAt      int64  `db:"last_heartbeat_at"`         // 最近心跳/上报（Unix 秒）
	InterruptedTotalSecs int64  `db:"interrupted_total_seconds"` // 本次推流累计中断秒数
	InterruptionCount    int32  `db:"interruption_count"`        // 本次推流累计断流次数
	StopReason           int32  `db:"stop_reason"`               // 停流原因，见 StopReason*
	StopDetail           string `db:"stop_detail"`               // 停流原因摘要（不含明文密钥）
	HealthState          int32  `db:"health_state"`              // 健康判定，见 HealthState*
	HealthReportedAt     int64  `db:"health_reported_at"`        // 最近健康上报（Unix 秒）
	VideoBitrateBps      int64  `db:"video_bitrate_bps"`         // 最近视频码率（bps）
	AudioBitrateBps      int64  `db:"audio_bitrate_bps"`         // 最近音频码率（bps）
	FpsX100              int32  `db:"fps_x100"`                  // 最近帧率 ×100
	PacketLossPpm        int32  `db:"packet_loss_ppm"`           // 最近丢包率（百万分比）
	TraceID              string `db:"trace_id"`                  // 建档时的 trace_id
	Ctime                int64  `db:"ctime"`                     // 建档时间（Unix 秒）
	Mtime                int64  `db:"mtime"`                     // 修改时间（Unix 秒）
}

// StreamFilter 是流列表查询条件；零值表示该维度不过滤。
type StreamFilter struct {
	RoomIDs      []int64
	AnchorMid    int64
	NodeID       string
	State        int32 // 0 表示只看非终态（ActiveStreamStates）
	Protocol     int32
	MaxHeartbeat int64 // 只返回 last_heartbeat_at <= 该值的流（断流扫描）
	Offset       int32
	Limit        int32
}

// StreamHealthPatch 是健康指标写回参数（ReportStreamHealth 用）。
type StreamHealthPatch struct {
	HealthState        int32
	ReportedAt         int64
	VideoBitrateBps    int64
	AudioBitrateBps    int64
	FpsX100            int32
	PacketLossPpm      int32
	TriggeredInterrupt bool
}

// StreamModel live_stream 表查询与写入接口。
type StreamModel interface {
	// Insert 建档一条 IDLE 流。uniq_publish_request 是鉴权重放的最终防线。
	Insert(ctx context.Context, tx sqlx.Session, s *Stream) error
	// FindOne 按 stream_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, streamID string) (*Stream, error)
	// FindByPublishRequest 按建档幂等键查询；不存在返回 (nil, nil)。
	FindByPublishRequest(ctx context.Context, requestID string) (*Stream, error)
	// FindActiveByRoom 返回某房间当前的非终态流（按 ctime 倒序取最新一条）；
	// 不存在返回 (nil, nil)。
	FindActiveByRoom(ctx context.Context, roomID int64) (*Stream, error)
	// FindActiveByStreamName 返回该流标识当前的非终态流（按 ctime 倒序取最新一条）；
	// CDN 回调只有 stream_name，靠它把回调归属到 stream_id。不存在返回 (nil, nil)。
	FindActiveByStreamName(ctx context.Context, streamName string) (*Stream, error)
	// ListActiveByKey 返回该密钥当前非终态的流（配额判定与吊销级联停流），limit 强制生效。
	ListActiveByKey(ctx context.Context, keyID int64, limit int32) ([]*Stream, error)
	// LockByID 在事务内对该行加写锁（SELECT ... FOR UPDATE），
	// 供「读当前 seq → 迁移 → 写事件」串行化；tx 为空时等价于普通查询（不推荐）。
	LockByID(ctx context.Context, tx sqlx.Session, streamID string) (*Stream, error)
	// ApplyTransition 以 state + seq 双条件推进状态并占用 seq（CAS）。
	// newSeq 必须等于调用方读到的 seq+1，否则返回 false（并发抢占或乱序上报）。
	// 进入 PUBLISHING 时回填 publish_started_at（仅当原值为 0），进入 STOPPED 时写停流原因。
	ApplyTransition(ctx context.Context, tx sqlx.Session, streamID string, fromState, toState int32, newSeq, at int64, stopReason int32, stopDetail string) (bool, error)
	// TouchHeartbeat 刷新 last_heartbeat_at（仅非终态流有效）。
	TouchHeartbeat(ctx context.Context, streamID string, at int64) (bool, error)
	// ApplyHealth 回写最新健康字段；TriggeredInterrupt 为 true 时同时把 health 判定
	// 记为 CRITICAL，供后续扫描定位。仅非终态流有效。
	// 健康上报本身就是存活信号，因此同步抬高 last_heartbeat_at（只抬高不回落）。
	ApplyHealth(ctx context.Context, streamID string, p StreamHealthPatch) (bool, error)
	// ApplyHealthTx 与 ApplyHealth 同语义，但跑在调用方的事务里：
	// ReportStreamHealth 要把「采样落库 + 投影回写 + 断流开合 + 事件/Outbox」放进同一事务。
	ApplyHealthTx(ctx context.Context, tx sqlx.Session, streamID string, p StreamHealthPatch) (bool, error)
	// OpenInterruption 累计断流次数（INTERRUPTED 迁移成功后调用）。
	OpenInterruption(ctx context.Context, tx sqlx.Session, streamID string) (bool, error)
	// CloseInterruption 累加本次中断秒数（断流结束时调用）。
	CloseInterruption(ctx context.Context, tx sqlx.Session, streamID string, seconds int64) (bool, error)
	// SetNode 回填/迁移当前接入节点（AssignIngestNode 与 ReleaseIngestNode 使用）。
	SetNode(ctx context.Context, tx sqlx.Session, streamID, nodeID string, expectPrev string) (bool, error)
	// MarkStopped 在停流事务里把健康判定归位为 NO_DATA（终态流不再参与健康巡检）。
	// 密钥活跃指针的释放由 StreamKeyModel.ReleaseActiveStream 负责，两者必须同事务。
	MarkStopped(ctx context.Context, tx sqlx.Session, streamID string) error
	// ListByFilter 分页查询流列表，按 last_heartbeat_at 升序（最可疑的在前），必须带 LIMIT。
	ListByFilter(ctx context.Context, f StreamFilter) ([]*Stream, error)
	// CountByFilter 统计条件命中行数（分页 total）。
	CountByFilter(ctx context.Context, f StreamFilter) (int64, error)
	// ListStaleActive 返回超过 graceSeconds 未心跳的非终态流（断流扫描，limit 强制）。
	ListStaleActive(ctx context.Context, now, graceSeconds, limit int64) ([]*Stream, error)
}

type defaultStreamModel struct {
	conn sqlx.SqlConn
}

// NewStreamModel 创建 StreamModel 实现。
func NewStreamModel(conn sqlx.SqlConn) StreamModel {
	return &defaultStreamModel{conn: conn}
}

func (m *defaultStreamModel) Insert(ctx context.Context, tx sqlx.Session, s *Stream) error {
	session := pickSession(m.conn, tx)
	if s.Ctime == 0 {
		s.Ctime = nowUnix()
	}
	s.Mtime = s.Ctime
	if s.State == 0 {
		s.State = StreamStateIdle
	}
	_, err := session.ExecCtx(ctx,
		"INSERT INTO live_stream (stream_id, key_id, stream_name, room_id, session_id, anchor_mid, protocol, node_id, "+
			"state, seq, publish_request_id, state_changed_at, last_heartbeat_at, health_state, stop_detail, trace_id, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		s.StreamID, s.KeyID, s.StreamName, s.RoomID, s.SessionID, s.AnchorMid, s.Protocol, s.NodeID,
		s.State, s.Seq, s.PublishRequestID, s.Ctime, s.Ctime, HealthStateNoData, truncate(s.StopDetail, 255),
		s.TraceID, s.Ctime, s.Mtime)
	if err != nil {
		return fmt.Errorf("live_stream Insert: %w", err)
	}
	return nil
}

func (m *defaultStreamModel) FindOne(ctx context.Context, streamID string) (*Stream, error) {
	var s Stream
	query := "SELECT " + streamColumns + " FROM live_stream WHERE stream_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &s, query, streamID); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultStreamModel) FindByPublishRequest(ctx context.Context, requestID string) (*Stream, error) {
	var s Stream
	query := "SELECT " + streamColumns + " FROM live_stream WHERE publish_request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &s, query, requestID); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream FindByPublishRequest: %w", err)
	}
	return &s, nil
}

func (m *defaultStreamModel) FindActiveByRoom(ctx context.Context, roomID int64) (*Stream, error) {
	in, args := statePlaceholders()
	query := "SELECT " + streamColumns + " FROM live_stream WHERE room_id = ? AND state IN (" + in + ") " +
		"ORDER BY ctime DESC LIMIT 1"
	var s Stream
	if err := m.conn.QueryRowCtx(ctx, &s, query, append([]interface{}{roomID}, args...)...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream FindActiveByRoom: %w", err)
	}
	return &s, nil
}

func (m *defaultStreamModel) FindActiveByStreamName(ctx context.Context, streamName string) (*Stream, error) {
	if streamName == "" {
		return nil, nil
	}
	in, args := statePlaceholders()
	query := "SELECT " + streamColumns + " FROM live_stream WHERE stream_name = ? AND state IN (" + in + ") " +
		"ORDER BY ctime DESC LIMIT 1"
	var s Stream
	if err := m.conn.QueryRowCtx(ctx, &s, query, append([]interface{}{streamName}, args...)...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream FindActiveByStreamName: %w", err)
	}
	return &s, nil
}

func (m *defaultStreamModel) ListActiveByKey(ctx context.Context, keyID int64, limit int32) ([]*Stream, error) {
	if keyID <= 0 {
		return nil, ErrInvalidKeyId
	}
	in, args := statePlaceholders()
	query := "SELECT " + streamColumns + " FROM live_stream WHERE key_id = ? AND state IN (" + in + ") " +
		"ORDER BY stream_id ASC LIMIT ?"
	args = append([]interface{}{keyID}, args...)
	args = append(args, clampLimit(limit, 100))

	var rows []*Stream
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream ListActiveByKey: %w", err)
	}
	return rows, nil
}

func (m *defaultStreamModel) LockByID(ctx context.Context, tx sqlx.Session, streamID string) (*Stream, error) {
	if tx == nil {
		// 不加锁的读-改-写会丢更新，宁可直接拒绝也不给 logic「看起来能跑」的假象。
		return nil, errors.New("live_stream LockByID requires a transaction session")
	}
	var s Stream
	query := "SELECT " + streamColumns + " FROM live_stream WHERE stream_id = ? FOR UPDATE"
	if err := tx.QueryRowCtx(ctx, &s, query, streamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream LockByID: %w", err)
	}
	return &s, nil
}

func (m *defaultStreamModel) ApplyTransition(
	ctx context.Context,
	tx sqlx.Session,
	streamID string,
	fromState, toState int32,
	newSeq, at int64,
	stopReason int32,
	stopDetail string,
) (bool, error) {
	if !ValidStreamState(fromState) || !ValidStreamState(toState) {
		return false, ErrInvalidStreamState
	}
	session := pickSession(m.conn, tx)

	sets := []string{"state = ?", "seq = ?", "state_changed_at = ?", "last_heartbeat_at = ?", "mtime = ?"}
	args := []interface{}{toState, newSeq, at, at, at}
	if toState == StreamStatePublishing {
		// 仅回填首次进入 PUBLISHING 的时间，重连不覆盖原始开播时刻。
		sets = append(sets, "publish_started_at = IF(publish_started_at = 0, ?, publish_started_at)")
		args = append(args, at)
	}
	if toState == StreamStateStopped {
		sets = append(sets, "stop_reason = ?", "stop_detail = ?")
		args = append(args, stopReason, truncate(stopDetail, 255))
	}
	args = append(args, streamID, fromState, newSeq-1)

	query := "UPDATE live_stream SET " + strings.Join(sets, ", ") +
		" WHERE stream_id = ? AND state = ? AND seq = ?"
	res, err := session.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_stream ApplyTransition: %w", err)
	}
	return rowsAffected(res, "live_stream ApplyTransition")
}

func (m *defaultStreamModel) TouchHeartbeat(ctx context.Context, streamID string, at int64) (bool, error) {
	in, args := statePlaceholders()
	query := "UPDATE live_stream SET last_heartbeat_at = ?, mtime = ? WHERE stream_id = ? AND state IN (" + in + ")"
	res, err := m.conn.ExecCtx(ctx, query, append([]interface{}{at, at, streamID}, args...)...)
	if err != nil {
		return false, fmt.Errorf("live_stream TouchHeartbeat: %w", err)
	}
	return rowsAffected(res, "live_stream TouchHeartbeat")
}

func (m *defaultStreamModel) ApplyHealth(ctx context.Context, streamID string, p StreamHealthPatch) (bool, error) {
	return m.ApplyHealthTx(ctx, nil, streamID, p)
}

func (m *defaultStreamModel) ApplyHealthTx(ctx context.Context, tx sqlx.Session, streamID string, p StreamHealthPatch) (bool, error) {
	if p.ReportedAt == 0 {
		p.ReportedAt = nowUnix()
	}
	if p.TriggeredInterrupt {
		// 采样已经触发断流：投影必须留下 CRITICAL 痕迹，不能被后续「暂时正常」的采样抹掉。
		p.HealthState = HealthStateCritical
	}
	sets := []string{"health_state = ?", "health_reported_at = ?", "video_bitrate_bps = ?",
		"audio_bitrate_bps = ?", "fps_x100 = ?", "packet_loss_ppm = ?",
		"last_heartbeat_at = GREATEST(last_heartbeat_at, ?)", "mtime = ?"}
	args := []interface{}{p.HealthState, p.ReportedAt, p.VideoBitrateBps, p.AudioBitrateBps,
		p.FpsX100, p.PacketLossPpm, p.ReportedAt, nowUnix()}
	in, stateArgs := statePlaceholders()
	query := "UPDATE live_stream SET " + strings.Join(sets, ", ") +
		" WHERE stream_id = ? AND state IN (" + in + ")"
	args = append(args, streamID)
	args = append(args, stateArgs...)

	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("live_stream ApplyHealth: %w", err)
	}
	return rowsAffected(res, "live_stream ApplyHealth")
}

func (m *defaultStreamModel) OpenInterruption(ctx context.Context, tx sqlx.Session, streamID string) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream SET interruption_count = interruption_count + 1, mtime = ? WHERE stream_id = ?",
		nowUnix(), streamID)
	if err != nil {
		return false, fmt.Errorf("live_stream OpenInterruption: %w", err)
	}
	return rowsAffected(res, "live_stream OpenInterruption")
}

func (m *defaultStreamModel) CloseInterruption(ctx context.Context, tx sqlx.Session, streamID string, seconds int64) (bool, error) {
	if seconds < 0 {
		seconds = 0
	}
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream SET interrupted_total_seconds = interrupted_total_seconds + ?, mtime = ? WHERE stream_id = ?",
		seconds, nowUnix(), streamID)
	if err != nil {
		return false, fmt.Errorf("live_stream CloseInterruption: %w", err)
	}
	return rowsAffected(res, "live_stream CloseInterruption")
}

func (m *defaultStreamModel) SetNode(ctx context.Context, tx sqlx.Session, streamID, nodeID string, expectPrev string) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_stream SET node_id = ?, mtime = ? WHERE stream_id = ? AND node_id = ?",
		nodeID, nowUnix(), streamID, expectPrev)
	if err != nil {
		return false, fmt.Errorf("live_stream SetNode: %w", err)
	}
	return rowsAffected(res, "live_stream SetNode")
}

func (m *defaultStreamModel) MarkStopped(ctx context.Context, tx sqlx.Session, streamID string) error {
	session := pickSession(m.conn, tx)
	_, err := session.ExecCtx(ctx,
		"UPDATE live_stream SET health_state = ?, mtime = ? WHERE stream_id = ? AND state = ?",
		HealthStateNoData, nowUnix(), streamID, StreamStateStopped)
	if err != nil {
		return fmt.Errorf("live_stream MarkStopped: %w", err)
	}
	return nil
}

func (m *defaultStreamModel) ListByFilter(ctx context.Context, f StreamFilter) ([]*Stream, error) {
	where, args := f.build()
	tail, tailArgs := pageArgs(f.Limit, 200, f.Offset)
	query := "SELECT " + streamColumns + " FROM live_stream WHERE " + where +
		" ORDER BY last_heartbeat_at ASC" + tail
	args = append(args, tailArgs...)

	var rows []*Stream
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream ListByFilter: %w", err)
	}
	return rows, nil
}

func (m *defaultStreamModel) CountByFilter(ctx context.Context, f StreamFilter) (int64, error) {
	where, args := f.build()
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt, "SELECT COUNT(*) FROM live_stream WHERE "+where, args...); err != nil {
		if isNoRows(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_stream CountByFilter: %w", err)
	}
	return cnt, nil
}

func (m *defaultStreamModel) ListStaleActive(ctx context.Context, now, graceSeconds, limit int64) ([]*Stream, error) {
	in, args := statePlaceholders()
	query := "SELECT " + streamColumns + " FROM live_stream WHERE state IN (" + in +
		") AND last_heartbeat_at <= ? ORDER BY last_heartbeat_at ASC LIMIT ?"
	tail := append([]interface{}{now - graceSeconds}, args...)
	tail = append(tail, limit)

	var rows []*Stream
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, tail...); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_stream ListStaleActive: %w", err)
	}
	return rows, nil
}

// build 组装 WHERE 片段与参数；恒真条件用 state <> 0 占位。
func (f StreamFilter) build() (string, []interface{}) {
	conds := []string{"state <> 0"}
	args := make([]interface{}, 0, 6)

	if len(f.RoomIDs) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(f.RoomIDs)), ",")
		conds = append(conds, "room_id IN ("+placeholders+")")
		for _, id := range f.RoomIDs {
			args = append(args, id)
		}
	}
	if f.AnchorMid > 0 {
		conds = append(conds, "anchor_mid = ?")
		args = append(args, f.AnchorMid)
	}
	if f.NodeID != "" {
		conds = append(conds, "node_id = ?")
		args = append(args, f.NodeID)
	}
	if ValidStreamState(f.State) {
		conds = append(conds, "state = ?")
		args = append(args, f.State)
	} else {
		in, stateArgs := statePlaceholders()
		conds = append(conds, "state IN ("+in+")")
		args = append(args, stateArgs...)
	}
	if ValidProtocol(f.Protocol) {
		conds = append(conds, "protocol = ?")
		args = append(args, f.Protocol)
	}
	if f.MaxHeartbeat > 0 {
		conds = append(conds, "last_heartbeat_at <= ?")
		args = append(args, f.MaxHeartbeat)
	}
	return strings.Join(conds, " AND "), args
}

// ValidProtocol 判断是否为已定义的接入协议枚举值（1/2/3）。
func ValidProtocol(protocol int32) bool {
	_, ok := ProtocolMask(protocol)
	return ok
}
