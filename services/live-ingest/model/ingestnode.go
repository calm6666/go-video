package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ingestNodeColumns 是 live_ingest_node 的列清单，必须与
// deploy/migrations/live-ingest/000001_create_live_ingest_key_tables.sql 完全一致。
const ingestNodeColumns = "node_id, name, region, protocol_mask, endpoint_rtmp, endpoint_srt, endpoint_webrtc, " +
	"state, capacity_streams, active_streams, health_score, last_heartbeat_at, labels, ctime, mtime"

// IngestNode 接入节点（live_ingest_node 表投影）。
//
// 节点由运维注册（UpsertIngestNode）并通过心跳维持 ONLINE；本表只保存可下发的
// 接入地址，不含任何密钥或厂商签名（AGENTS.md §6：客户端只拿短期签名与接入地址）。
type IngestNode struct {
	NodeID          string `db:"node_id"`           // 节点 ID（主键，运维分配的稳定标识）
	Name            string `db:"name"`              // 展示名
	Region          string `db:"region"`            // 地理区域码
	ProtocolMask    uint32 `db:"protocol_mask"`     // 支持协议位图，见 ProtocolMask*
	EndpointRtmp    string `db:"endpoint_rtmp"`     // RTMP 接入地址
	EndpointSrt     string `db:"endpoint_srt"`      // SRT 接入地址
	EndpointWebrtc  string `db:"endpoint_webrtc"`   // WebRTC 信令地址
	State           int32  `db:"state"`             // 见 NodeState*
	CapacityStreams int32  `db:"capacity_streams"`  // 并发流配额
	ActiveStreams   int32  `db:"active_streams"`    // 当前占用（由分配记录派生，允许短暂偏差）
	HealthScore     int32  `db:"health_score"`      // 健康分 0~100，分配打分依据
	LastHeartbeatAt int64  `db:"last_heartbeat_at"` // 最近心跳（Unix 秒）
	Labels          string `db:"labels"`            // 运维标签（k=v;k=v）
	Ctime           int64  `db:"ctime"`             // 创建时间（Unix 秒）
	Mtime           int64  `db:"mtime"`             // 修改时间（Unix 秒）
}

// IngestNodeFilter 节点列表查询条件；零值表示不过滤。
type IngestNodeFilter struct {
	Region   string
	Protocol int32 // 必须支持的协议（枚举值），0 表示不限制
	State    int32
	Offset   int32
	Limit    int32
}

// IngestNodeModel live_ingest_node 表查询与写入接口。
type IngestNodeModel interface {
	// Upsert 注册或更新节点（ON DUPLICATE KEY UPDATE，node_id 为幂等键）。
	// 返回 created 表示本次是否新建。heartbeat_only 时只刷新心跳/占用/健康分。
	Upsert(ctx context.Context, n *IngestNode, heartbeatOnly bool) (created bool, err error)
	// FindOne 按 node_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, nodeID string) (*IngestNode, error)
	// ListCandidates 返回可分配的在线节点（协议匹配 + 未满配额），
	// 按 health_score 降序、active_streams 升序，limit 强制生效。
	ListCandidates(ctx context.Context, protocolMask uint32, region string, limit int32) ([]*IngestNode, error)
	// ReserveQuota 占用一个配额：仅当 ONLINE 且未超配额时成功（CAS on active_streams）。
	ReserveQuota(ctx context.Context, tx sqlx.Session, nodeID string) (bool, error)
	// ReleaseQuota 释放一个配额（不低于 0，终态幂等）。
	ReleaseQuota(ctx context.Context, tx sqlx.Session, nodeID string) (bool, error)
	// MarkOfflineByHeartbeatTimeout 把心跳超时的 ONLINE 节点批量置 OFFLINE，
	// 返回影响行数（后台扫描器调用，limit 强制）。
	MarkOfflineByHeartbeatTimeout(ctx context.Context, now, timeoutSeconds, limit int64) (int64, error)
	// ListByFilter 分页查询节点，按 health_score 降序，必须带 LIMIT。
	ListByFilter(ctx context.Context, f IngestNodeFilter) ([]*IngestNode, error)
	// CountByFilter 统计条件命中行数（分页 total）。
	CountByFilter(ctx context.Context, f IngestNodeFilter) (int64, error)
}

type defaultIngestNodeModel struct {
	conn sqlx.SqlConn
}

// NewIngestNodeModel 创建 IngestNodeModel 实现。
func NewIngestNodeModel(conn sqlx.SqlConn) IngestNodeModel {
	return &defaultIngestNodeModel{conn: conn}
}

func (m *defaultIngestNodeModel) Upsert(ctx context.Context, n *IngestNode, heartbeatOnly bool) (bool, error) {
	if n.NodeID == "" {
		return false, fmt.Errorf("live_ingest_node Upsert: node_id required")
	}
	now := nowUnix()
	if heartbeatOnly {
		// 心跳路径只碰计数列，避免与注册路径互相覆盖配置字段。
		res, err := m.conn.ExecCtx(ctx,
			"UPDATE live_ingest_node SET active_streams = ?, health_score = ?, last_heartbeat_at = ?, mtime = ? "+
				"WHERE node_id = ? AND state <> ?",
			n.ActiveStreams, n.HealthScore, n.LastHeartbeatAt, now, n.NodeID, NodeStateOffline)
		if err != nil {
			return false, fmt.Errorf("live_ingest_node Upsert heartbeat: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("live_ingest_node Upsert heartbeat RowsAffected: %w", err)
		}
		if affected == 0 {
			// 心跳命中未注册或已置 OFFLINE 的节点：不静默建档（未注册节点不能进分配池），
			// 回查确认存在性后交给 logic 判定 ErrNodeNotFound。
			existing, findErr := m.FindOne(ctx, n.NodeID)
			if findErr != nil {
				return false, findErr
			}
			if existing == nil {
				return false, ErrNodeNotFound
			}
		}
		return false, nil
	}

	if n.Ctime == 0 {
		n.Ctime = now
	}
	n.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO live_ingest_node (node_id, name, region, protocol_mask, endpoint_rtmp, endpoint_srt, "+
			"endpoint_webrtc, state, capacity_streams, active_streams, health_score, last_heartbeat_at, labels, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE name = VALUES(name), region = VALUES(region), protocol_mask = VALUES(protocol_mask), "+
			"endpoint_rtmp = VALUES(endpoint_rtmp), endpoint_srt = VALUES(endpoint_srt), "+
			"endpoint_webrtc = VALUES(endpoint_webrtc), state = VALUES(state), capacity_streams = VALUES(capacity_streams), "+
			"health_score = VALUES(health_score), last_heartbeat_at = VALUES(last_heartbeat_at), "+
			"labels = VALUES(labels), mtime = VALUES(mtime)",
		n.NodeID, truncate(n.Name, 64), truncate(n.Region, 16), n.ProtocolMask,
		truncate(n.EndpointRtmp, 191), truncate(n.EndpointSrt, 191), truncate(n.EndpointWebrtc, 191),
		n.State, n.CapacityStreams, n.ActiveStreams, n.HealthScore, n.LastHeartbeatAt,
		truncate(n.Labels, 255), n.Ctime, n.Mtime)
	if err != nil {
		return false, fmt.Errorf("live_ingest_node Upsert: %w", err)
	}
	// MySQL 对 ON DUPLICATE KEY UPDATE：新建返回 1，更新返回 2（无变化 0）。
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_ingest_node Upsert RowsAffected: %w", err)
	}
	return affected == 1, nil
}

func (m *defaultIngestNodeModel) FindOne(ctx context.Context, nodeID string) (*IngestNode, error) {
	var n IngestNode
	query := "SELECT " + ingestNodeColumns + " FROM live_ingest_node WHERE node_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &n, query, nodeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_ingest_node FindOne: %w", err)
	}
	return &n, nil
}

func (m *defaultIngestNodeModel) ListCandidates(ctx context.Context, protocolMask uint32, region string, limit int32) ([]*IngestNode, error) {
	conds := []string{"state = ?", "capacity_streams > active_streams", "protocol_mask & ? <> 0"}
	args := []interface{}{NodeStateOnline, protocolMask}
	if region != "" {
		conds = append(conds, "region = ?")
		args = append(args, region)
	}
	query := "SELECT " + ingestNodeColumns + " FROM live_ingest_node WHERE " + strings.Join(conds, " AND ") +
		" ORDER BY health_score DESC, active_streams ASC, node_id ASC LIMIT ?"
	args = append(args, clampLimit(limit, 50))

	var rows []*IngestNode
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_ingest_node ListCandidates: %w", err)
	}
	return rows, nil
}

func (m *defaultIngestNodeModel) ReserveQuota(ctx context.Context, tx sqlx.Session, nodeID string) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_ingest_node SET active_streams = active_streams + 1, mtime = ? "+
			"WHERE node_id = ? AND state = ? AND active_streams < capacity_streams",
		nowUnix(), nodeID, NodeStateOnline)
	if err != nil {
		return false, fmt.Errorf("live_ingest_node ReserveQuota: %w", err)
	}
	return rowsAffected(res, "live_ingest_node ReserveQuota")
}

func (m *defaultIngestNodeModel) ReleaseQuota(ctx context.Context, tx sqlx.Session, nodeID string) (bool, error) {
	session := pickSession(m.conn, tx)
	res, err := session.ExecCtx(ctx,
		"UPDATE live_ingest_node SET active_streams = active_streams - 1, mtime = ? "+
			"WHERE node_id = ? AND active_streams > 0",
		nowUnix(), nodeID)
	if err != nil {
		return false, fmt.Errorf("live_ingest_node ReleaseQuota: %w", err)
	}
	return rowsAffected(res, "live_ingest_node ReleaseQuota")
}

func (m *defaultIngestNodeModel) MarkOfflineByHeartbeatTimeout(ctx context.Context, now, timeoutSeconds, limit int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_ingest_node SET state = ?, mtime = ? WHERE state = ? AND last_heartbeat_at <= ? LIMIT ?",
		NodeStateOffline, nowUnix(), NodeStateOnline, now-timeoutSeconds, clampLimit(int32(limit), 500))
	if err != nil {
		return 0, fmt.Errorf("live_ingest_node MarkOfflineByHeartbeatTimeout: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_ingest_node MarkOfflineByHeartbeatTimeout RowsAffected: %w", err)
	}
	return affected, nil
}

func (m *defaultIngestNodeModel) ListByFilter(ctx context.Context, f IngestNodeFilter) ([]*IngestNode, error) {
	where, args := f.build()
	tail, tailArgs := pageArgs(f.Limit, 100, f.Offset)
	query := "SELECT " + ingestNodeColumns + " FROM live_ingest_node WHERE " + where +
		" ORDER BY health_score DESC, node_id ASC" + tail
	args = append(args, tailArgs...)

	var rows []*IngestNode
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_ingest_node ListByFilter: %w", err)
	}
	return rows, nil
}

func (m *defaultIngestNodeModel) CountByFilter(ctx context.Context, f IngestNodeFilter) (int64, error) {
	where, args := f.build()
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt, "SELECT COUNT(*) FROM live_ingest_node WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_ingest_node CountByFilter: %w", err)
	}
	return cnt, nil
}

// build 组装 WHERE 片段与参数；恒真条件用 capacity_streams <> -1 占位。
func (f IngestNodeFilter) build() (string, []interface{}) {
	conds := []string{"capacity_streams <> -1"}
	args := make([]interface{}, 0, 3)
	if f.Region != "" {
		conds = append(conds, "region = ?")
		args = append(args, f.Region)
	}
	if mask, ok := ProtocolMask(f.Protocol); ok {
		conds = append(conds, "protocol_mask & ? <> 0")
		args = append(args, mask)
	}
	if validNodeState(f.State) {
		conds = append(conds, "state = ?")
		args = append(args, f.State)
	}
	return strings.Join(conds, " AND "), args
}

func validNodeState(state int32) bool {
	switch state {
	case NodeStateOnline, NodeStateDraining, NodeStateOffline:
		return true
	default:
		return false
	}
}
