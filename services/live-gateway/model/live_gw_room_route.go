package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// LiveGwRoomRoute 房间→节点路由（live_gw_room_route）。
//
// 这是「重启后需要重建」的路由事实，不是逐条连接状态：
// 每房间一行，记录主承接节点与副本节点，节点重启或扩缩容后靠这张表恢复房间广播的第一跳。
// 连接数、订阅成员、心跳等易失数据一律只在 Redis（README 数据分层约束），
// 因此 RoomRouteInfo.serving_connections 由读路径实时从 Redis 取，本表不保存。
type LiveGwRoomRoute struct {
	RoomId       int64  `db:"room_id"`       // 主键：房间 ID（live-room 主键，只引用）
	PrimaryNode  string `db:"primary_node"`  // 主承接节点标识（WS 接入层上报）
	ReplicaNodes string `db:"replica_nodes"` // 副本节点，JSON 字符串数组（大房间分片广播）
	ShardCount   int32  `db:"shard_count"`   // 广播分片数（>=1）
	State        int32  `db:"state"`         // RouteState*
	Version      int64  `db:"version"`       // 乐观并发版本（DrainRoomRoute 必须回传）
	DrainReason  string `db:"drain_reason"`  // 最近一次排空/下线原因（审计必填）
	UpdatedBy    string `db:"updated_by"`    // 最后修改者（system/运营账号）
	TraceId      string `db:"trace_id"`
	Ctime        int64  `db:"ctime"`
	Mtime        int64  `db:"mtime"`
}

// RoomRouteFilter List 过滤条件。
type RoomRouteFilter struct {
	NodeId      string // 按节点过滤（主节点或副本节点命中即可）
	State       int32
	Pn          int32
	Ps          int32
	MaxPageSize int32
}

// RoutePatch 路由状态推进时随带写入的字段；nil 表示不更新。
type RoutePatch struct {
	PrimaryNode  *string
	ReplicaNodes *[]string
	ShardCount   *int32
	DrainReason  *string
	Operator     *string
	TraceID      *string
}

func (p RoutePatch) sets() ([]columnValue, error) {
	sets := make([]columnValue, 0, 6)
	if p.PrimaryNode != nil {
		if *p.PrimaryNode == "" {
			return nil, ErrEmptyNodeID
		}
		sets = append(sets, columnValue{"primary_node", *p.PrimaryNode})
	}
	if p.ReplicaNodes != nil {
		encoded, err := encodeReplicaNodes(*p.ReplicaNodes)
		if err != nil {
			return nil, err
		}
		sets = append(sets, columnValue{"replica_nodes", encoded})
	}
	if p.ShardCount != nil {
		if *p.ShardCount < 1 {
			return nil, fmt.Errorf("live_gw_room_route: shard_count=%d must be >= 1", *p.ShardCount)
		}
		sets = append(sets, columnValue{"shard_count", *p.ShardCount})
	}
	if p.DrainReason != nil {
		sets = append(sets, columnValue{"drain_reason", *p.DrainReason})
	}
	if p.Operator != nil && *p.Operator != "" {
		sets = append(sets, columnValue{"updated_by", *p.Operator})
	}
	if p.TraceID != nil && *p.TraceID != "" {
		sets = append(sets, columnValue{"trace_id", *p.TraceID})
	}
	return sets, nil
}

// ReplicaNodesList 把 replica_nodes 列（JSON 数组字符串）解成切片。
func (r *LiveGwRoomRoute) ReplicaNodesList() ([]string, error) {
	return decodeReplicaNodes(r.ReplicaNodes)
}

// NormalizeReplicaNodes 校准 replica_nodes 列：
// nodes 非 nil 时按列表编码（去重去空白）；为 nil 时保留已有值，空串补成 "[]"。
// JSON 列不接受空串，写库前必须过一次本方法。
func (r *LiveGwRoomRoute) NormalizeReplicaNodes(nodes []string) error {
	if nodes != nil {
		encoded, err := encodeReplicaNodes(nodes)
		if err != nil {
			return err
		}
		r.ReplicaNodes = encoded
		return nil
	}
	if strings.TrimSpace(r.ReplicaNodes) == "" {
		r.ReplicaNodes = "[]"
		return nil
	}
	_, err := decodeReplicaNodes(r.ReplicaNodes)
	return err
}

// LiveGwRoomRouteModel live_gw_room_route 表接口。
type LiveGwRoomRouteModel interface {
	// Register 登记/刷新房间路由：
	//  1. 先做条件 UPDATE（只有 SERVING/OFFLINE 可被重新承接，version+1）；
	//  2. 行不存在则 INSERT（room_id 主键兜住并发重复登记）；
	//  3. 行处于 DRAINING 时返回该行 + ErrRouteDraining，调用方必须把新连接引到别的节点，
	//     不得偷偷把排空中的节点重新启用。
	// 返回操作后的有效行。
	Register(ctx context.Context, route *LiveGwRoomRoute) (*LiveGwRoomRoute, error)
	FindOne(ctx context.Context, roomID int64) (*LiveGwRoomRoute, error)
	List(ctx context.Context, f RoomRouteFilter) ([]*LiveGwRoomRoute, int32, error)
	// CountByNode 某节点当前承接的房间数（节点优雅下线前的排空进度）。
	CountByNode(ctx context.Context, nodeID string, states []int32) (int64, error)
	// UpdateState 条件 UPDATE 推进路由状态：
	// nodeID 非空时额外要求 primary_node = nodeID（DrainRoomRoute 只能排空自己节点的路由）。
	// 返回受影响行数（0 行由调用方翻译成 NotFound / VersionConflict / InvalidTransition）。
	UpdateState(ctx context.Context, roomID int64, nodeID string, fromStates []int32, expectedVersion int64,
		to int32, patch RoutePatch) (int64, error)
	// ListByNode 按节点分页取房间路由（节点下线时批量迁移用），按 room_id 升序保证可重放。
	ListByNode(ctx context.Context, nodeID string, states []int32, afterRoomID int64, limit int32) ([]*LiveGwRoomRoute, error)
}

const liveGwRoomRouteColumns = "SELECT room_id, primary_node, replica_nodes, shard_count, state, version, " +
	"drain_reason, updated_by, trace_id, ctime, mtime"

type defaultLiveGwRoomRouteModel struct {
	conn sqlx.SqlConn
}

// NewLiveGwRoomRouteModel 构造 live_gw_room_route 的 model。
func NewLiveGwRoomRouteModel(conn sqlx.SqlConn) LiveGwRoomRouteModel {
	return &defaultLiveGwRoomRouteModel{conn: conn}
}

func (m *defaultLiveGwRoomRouteModel) Register(ctx context.Context, route *LiveGwRoomRoute) (*LiveGwRoomRoute, error) {
	if route.RoomId <= 0 {
		return nil, ErrInvalidRoomID
	}
	if route.PrimaryNode == "" {
		return nil, ErrEmptyNodeID
	}
	if route.ShardCount < 1 {
		route.ShardCount = 1
	}
	// replica_nodes 是 JSON 列：无副本也要写 "[]"，不能写空串（MySQL 会判为无效 JSON）。
	if err := route.NormalizeReplicaNodes(nil); err != nil {
		return nil, err
	}
	// 先尝试条件更新：同节点重连/节点重启后重建路由走这条路（version+1 便于并发观察）。
	sets := []columnValue{
		{"primary_node", route.PrimaryNode},
		{"replica_nodes", route.ReplicaNodes},
		{"shard_count", route.ShardCount},
		{"state", RouteStateServing},
		{"trace_id", route.TraceId},
	}
	if route.UpdatedBy != "" {
		sets = append(sets, columnValue{"updated_by", route.UpdatedBy})
	}
	aff, err := conditionalUpdate(ctx, m.conn, "live_gw_room_route", sets, true, []whereFragment{
		{"room_id = ?", []any{route.RoomId}},
		stateInFragment([]int32{RouteStateServing, RouteStateOffline}),
	})
	if err != nil {
		return nil, err
	}
	if aff > 0 {
		return m.FindOne(ctx, route.RoomId)
	}
	current, err := m.FindOne(ctx, route.RoomId)
	if err != nil {
		return nil, err
	}
	if current != nil {
		if current.State == RouteStateDraining {
			return current, fmt.Errorf("room_id=%d node=%s: %w", route.RoomId, route.PrimaryNode, ErrRouteDraining)
		}
		return current, nil
	}
	// 首次登记：主键冲突说明并发下另一路已建行，回读它并按同一规则判定。
	now := nowUnix()
	if route.Ctime == 0 {
		route.Ctime = now
	}
	if route.Mtime == 0 {
		route.Mtime = now
	}
	if route.Version == 0 {
		route.Version = 1
	}
	route.State = RouteStateServing
	if _, err := m.conn.ExecCtx(ctx,
		"INSERT INTO live_gw_room_route (room_id, primary_node, replica_nodes, shard_count, state, version, "+
			"drain_reason, updated_by, trace_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		route.RoomId, route.PrimaryNode, route.ReplicaNodes, route.ShardCount, route.State, route.Version,
		route.DrainReason, route.UpdatedBy, route.TraceId, route.Ctime, route.Mtime); err != nil {
		if !isDuplicateErr(err) {
			return nil, fmt.Errorf("live_gw_room_route Register insert: %w", err)
		}
	}
	after, err := m.FindOne(ctx, route.RoomId)
	if err != nil {
		return nil, err
	}
	if after == nil {
		return nil, ErrRouteNotFound
	}
	if after.State == RouteStateDraining {
		return after, fmt.Errorf("room_id=%d: %w", route.RoomId, ErrRouteDraining)
	}
	return after, nil
}

func (m *defaultLiveGwRoomRouteModel) FindOne(ctx context.Context, roomID int64) (*LiveGwRoomRoute, error) {
	var r LiveGwRoomRoute
	query := liveGwRoomRouteColumns + " FROM live_gw_room_route WHERE room_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &r, query, roomID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_room_route FindOne: %w", err)
	}
	return &r, nil
}

func (m *defaultLiveGwRoomRouteModel) List(ctx context.Context, f RoomRouteFilter) ([]*LiveGwRoomRoute, int32, error) {
	frags := []whereFragment{
		whereFragment{"state = ?", []any{}}.when(f.State > 0, f.State),
	}
	if f.NodeId != "" {
		// 主节点或副本节点都算「该节点承接了这个房间」。
		frags = append(frags, whereFragment{"(primary_node = ? OR JSON_CONTAINS(replica_nodes, JSON_QUOTE(?)))",
			[]any{f.NodeId, f.NodeId}})
	}
	where, args := buildWhere(frags...)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_gw_room_route "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_gw_room_route List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveGwRoomRoute
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveGwRoomRouteColumns+" FROM live_gw_room_route "+where+" ORDER BY room_id ASC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_gw_room_route List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveGwRoomRouteModel) CountByNode(ctx context.Context, nodeID string, states []int32) (int64, error) {
	if nodeID == "" {
		return 0, ErrEmptyNodeID
	}
	var n int64
	if len(states) == 0 {
		states = []int32{RouteStateServing, RouteStateDraining}
	}
	inSQL, inArgs := placeholders(states)
	query := "SELECT COUNT(*) FROM live_gw_room_route WHERE (primary_node = ? OR JSON_CONTAINS(replica_nodes, JSON_QUOTE(?))) " +
		"AND state IN (" + inSQL + ")"
	args := append([]any{nodeID, nodeID}, inArgs...)
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_gw_room_route CountByNode: %w", err)
	}
	return n, nil
}

func (m *defaultLiveGwRoomRouteModel) UpdateState(ctx context.Context, roomID int64, nodeID string,
	fromStates []int32, expectedVersion int64, to int32, patch RoutePatch) (int64, error) {
	if len(fromStates) == 0 {
		return 0, fmt.Errorf("live_gw_room_route UpdateState: empty fromStates %w", ErrInvalidTransition)
	}
	sets, err := patch.sets()
	if err != nil {
		return 0, err
	}
	sets = append(sets, columnValue{"state", to})
	wheres := []whereFragment{
		{"room_id = ?", []any{roomID}},
		stateInFragment(fromStates),
	}
	if nodeID != "" {
		wheres = append(wheres, whereFragment{"primary_node = ?", []any{nodeID}})
	}
	if expectedVersion > 0 {
		wheres = append(wheres, whereFragment{"version = ?", []any{expectedVersion}})
	}
	return conditionalUpdate(ctx, m.conn, "live_gw_room_route", sets, true, wheres)
}

func (m *defaultLiveGwRoomRouteModel) ListByNode(ctx context.Context, nodeID string, states []int32,
	afterRoomID int64, limit int32) ([]*LiveGwRoomRoute, error) {
	if nodeID == "" {
		return nil, ErrEmptyNodeID
	}
	if limit <= 0 {
		limit = 100
	}
	if len(states) == 0 {
		states = []int32{RouteStateServing}
	}
	inSQL, inArgs := placeholders(states)
	frags := []whereFragment{
		{"(primary_node = ? OR JSON_CONTAINS(replica_nodes, JSON_QUOTE(?)))", []any{nodeID, nodeID}},
		{"state IN (" + inSQL + ")", inArgs},
		whereFragment{"room_id > ?", []any{}}.when(afterRoomID > 0, afterRoomID),
	}
	where, args := buildWhere(frags...)
	listArgs := append(append([]any{}, args...), limit)
	var rows []*LiveGwRoomRoute
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveGwRoomRouteColumns+" FROM live_gw_room_route "+where+" ORDER BY room_id ASC LIMIT ?", listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_room_route ListByNode: %w", err)
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// 本包共享的条件更新构件（其它 live_gw_* 表 model 文件复用）
//
// AGENTS.md §5：状态推进必须是「条件 UPDATE + RowsAffected」，禁止读-改-写两步更新
// （并发下会丢推进）。路由/配额/票据三张表都靠这里的 conditionalUpdate。
// 放在路由文件里是为了让 model 目录保持「一表一文件 + errors.go + now.go」的文件清单。
// ---------------------------------------------------------------------------

// columnValue 一个待写入列与其值。
type columnValue struct {
	Column string
	Value  any
}

// rawExpr SET 里需要直接嵌入表达式的赋值（例如 version = version + 1、retry_count = retry_count + 1）。
// SQL 只能来自本包常量；外部输入必须走 Args 占位符（防注入）。
type rawExpr struct {
	SQL  string
	Args []any
}

// whereFragment 一个 WHERE 条件片段及其参数。
type whereFragment struct {
	SQL  string
	Args []any
}

// when 条件成立时把 value 绑定为该片段的参数，否则返回空片段（buildWhere 丢弃）。
func (w whereFragment) when(ok bool, value any) whereFragment {
	if !ok {
		return whereFragment{}
	}
	return whereFragment{SQL: w.SQL, Args: []any{value}}
}

// empty 判断片段是否不参与拼接。
func (w whereFragment) empty() bool { return w.SQL == "" }

// buildWhere 拼接条件片段，无条件时返回 "WHERE 1=1"（不生成缺 WHERE 的语句）。
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
	sqlText, args := placeholders(states)
	return whereFragment{SQL: "state IN (" + sqlText + ")", Args: args}
}

// placeholders 生成 "?, ?, ?" 与对应的 int32 参数切片（用于 IN 列表）。
func placeholders(values []int32) (string, []any) {
	parts := make([]string, 0, len(values))
	args := make([]any, 0, len(values))
	for _, v := range values {
		parts = append(parts, "?")
		args = append(args, v)
	}
	return strings.Join(parts, ", "), args
}

// conditionalUpdate 执行「条件 UPDATE」：自动追加 version = version + 1（bumpVersion 时）与 mtime = now，
// 返回受影响行数。0 行不是错误，由调用方按业务语义翻译成 NotFound / Conflict / InvalidTransition。
// SET 为空（无效 SQL）或 WHERE 无绑定参数（等于全表更新）都会被拒绝。
func conditionalUpdate(ctx context.Context, conn sqlx.SqlConn, table string, sets []columnValue,
	bumpVersion bool, wheres []whereFragment) (int64, error) {
	if len(sets) == 0 {
		return 0, fmt.Errorf("%s conditionalUpdate: empty SET", table)
	}
	whereSQL, whereArgs := buildWhere(wheres...)
	if len(whereArgs) == 0 {
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

	res, err := conn.ExecCtx(ctx, sb.String(), args...)
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

// encodeReplicaNodes 把节点列表规范成 JSON 数组字符串（列是 JSON 类型，空集合写 "[]"）。
// 去重、去空白；任何空节点标识都拒绝（写进路由表会让下游按 "" 去连节点）。
func encodeReplicaNodes(nodes []string) (string, error) {
	if len(nodes) == 0 {
		return "[]", nil
	}
	seen := make(map[string]struct{}, len(nodes))
	clean := make([]string, 0, len(nodes))
	for _, n := range nodes {
		n = strings.TrimSpace(n)
		if n == "" {
			return "", ErrEmptyNodeID
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		clean = append(clean, n)
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return "", fmt.Errorf("live_gw_room_route: encode replica_nodes: %w", err)
	}
	return string(raw), nil
}

// decodeReplicaNodes 解析 replica_nodes；空串/"[]"/"null" 视为无副本。
func decodeReplicaNodes(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil, nil
	}
	var nodes []string
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return nil, fmt.Errorf("live_gw_room_route: replica_nodes %q is not a JSON string array: %w", raw, err)
	}
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}
