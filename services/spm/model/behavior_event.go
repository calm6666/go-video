package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// BehaviorEvent 脱敏后的行为事件事实行（spm_behavior_event 表投影）。
//
// 这张表是 spm 唯一的「事实源」：所有窗口指标、兴趣画像与留存投影都必须能从它重算
// （docs/data-design.md §5）。落库形态刻意不做 KV/JSON 大字段——事件原文只允许出现在
// 死信摘要里（sha256 + 脱敏前缀），事实表只保存白名单维度列：
//   - 标识列只有 mid / aid / content_id / zone_id / catalog_item_id / target_mid
//     （跨服务只传主键，AGENTS.md §5）；
//   - 设备与会话只允许 Pseudonym 摘要列（上游 event-collector 已加盐哈希的
//     device_hash / ip_segment，或 playback 的 mid_hash）。明文设备号、手机号、
//     原始 IP 一律禁止入库，mapping 阶段直接丢弃；
//   - 事件的数值（播放进度、完播比例、结果位次等）由 mapping 阶段归一化成
//     NumValue + 受控 DimKey/DimValue，避免「什么都塞进 payload」导致口径无法版本化。
//
// ActionKey 是受控动作词表（play/finish/click/…），与 EventType 分开存：
// 同一次点赞可能同时以 behavior.like 与 engagement.action 两种形态到达，
// 口径登记里的 source_event_types 决定哪个通道参与计算，事实行则保留全部来源以便重算。
type BehaviorEvent struct {
	ID            int64   `db:"id"`              // 主键 ID
	EventID       string  `db:"event_id"`        // 信封 event_id，幂等真值
	EventType     string  `db:"event_type"`      // behavior.play / playback.heartbeat ...
	ActionKey     string  `db:"action_key"`      // 归一化动作键（Action* 常量）
	SourceChannel int32   `db:"source_channel"`  // 来源通道（SourceChannel* 常量）
	SchemaVersion int32   `db:"schema_version"`  // 信封 schema_version，回放时按版本解析
	OccurredAt    int64   `db:"occurred_at"`     // 事件发生时间（Unix 秒）
	EventDay      int64   `db:"event_day"`       // 发生日 UTC 零点（天级窗口与留存的对齐键）
	Mid           int64   `db:"mid"`             // 行为发起用户（0 = 未登录或事件只带假名）
	Pseudonym     string  `db:"pseudonym"`       // 假名摘要（device_hash / mid_hash / ip_segment）
	PseudonymKind string  `db:"pseudonym_kind"`  // 假名来源：device / mid_hash / ip_segment
	Aid           int64   `db:"aid"`             // 稿件 aid：只有 event-collector 的 behavior.* 通道带；UGC 场景与 content_id 同值
	ContentID     int64   `db:"content_id"`      // 内容主键（上游 content_id：UGC=aid、PGC=episode_id，见 playback.proto:47）
	ContentType   int32   `db:"content_type"`    // 1 UGC、2 PGC、3 直播；behavior.* 的字符串枚举由 ContentTypeFromName 归一
	ZoneID        int64   `db:"zone_id"`         // 分区（上游 behavior.* 与 playback.heartbeat 都不带，只能由 mapping 查 spm_content_projection 补齐；0 = 未知）
	CatalogItemID int64   `db:"catalog_item_id"` // 作品级条目 ID（本期上游只给 episode 级 content_id，故多为 0，见 README 契约缺口）
	TargetMid     int64   `db:"target_mid"`      // 被作用用户（follow / 点赞 UP 主等）
	DimKey        string  `db:"dim_key"`         // 受控附加维度名，如 quality / result_index
	DimValue      string  `db:"dim_value"`       // 受控附加维度值（枚举/短串，不存自由文本）
	NumValue      float64 `db:"num_value"`       // 归一化数值（进度秒数、完播比例、位次等）
	TraceID       string  `db:"trace_id"`        // 链路追踪 ID
	Topic         string  `db:"topic"`           // 来源 topic（含版本后缀）
	PartitionNo   int32   `db:"partition_no"`    // Kafka 分区（kq 不上报时为 0）
	MsgOffset     int64   `db:"msg_offset"`      // Kafka 位点（可重放定位）
	Ctime         int64   `db:"ctime"`           // 落库时间（Unix 秒）
	Mtime         int64   `db:"mtime"`           // 修改时间（Unix 秒）
}

// WindowAggRow 窗口聚合输入行：实时聚合器与离线回填共用的「事实 → 窗口」原始统计。
// 这里刻意只给原始量（计数、数值和、去重主体数），比率类口径的分子分母组合由
// logic 层按 metric_version 的公式决定——否则口径版本化就失去了意义。
type WindowAggRow struct {
	SubjectID      int64   `db:"subject_id"`
	WindowStart    int64   `db:"window_start"`
	EventCount     int64   `db:"event_count"`
	NumSum         float64 `db:"num_sum"`
	DistinctActors int64   `db:"distinct_actors"`
	LastEventTime  int64   `db:"last_event_time"`
}

// MidActivity 单用户在时间区间内的最早活动时间，留存分桶的原始输入。
type MidActivity struct {
	Mid     int64 `db:"mid"`
	FirstAt int64 `db:"first_at"`
}

// BehaviorEventModel spm_behavior_event 表读写接口。
//
// 写入口只有一个（InsertIfAbsent），且只被内部消费者调用：契约层不提供事件明细写 RPC，
// 避免任何服务绕过 event-collector 的限流、采样与脱敏直接灌数据（AGENTS.md §7）。
type BehaviorEventModel interface {
	// InsertIfAbsent 按 uniq_event_id 幂等落库。返回 false 表示该事件已落库（重复投递）。
	InsertIfAbsent(ctx context.Context, e *BehaviorEvent) (bool, error)
	// FindByEventID 按事件 ID 查事实行；不存在返回 nil。
	FindByEventID(ctx context.Context, eventID string) (*BehaviorEvent, error)
	// AggregateWindow 按「主体列 × 窗口」聚合原始量，是实时窗口与离线回填共用的输入源。
	// subjectColumn 只接受白名单列名（见 allowedSubjectColumns），杜绝 SQL 注入面。
	// eventTypes 为空表示不限事件类型（调用方必须显式传，model 层不做默认值猜测）。
	// contentTypes 是内容类型约束（1 UGC、2 PGC、3 直播）：**按 content_id 或 aid 分组时必须传**。
	//   上游的 UGC 主键（aid）与 PGC 主键（episode_id）住在两个互不相干的 ID 空间里，
	//   数值可以相同；不加 content_type 条件就把它们并成一组，等于把某部剧的一集
	//   和某个稿件的完播率加在一起，而 GROUP BY 的结果看起来完全正常。
	// timeFrom/timeTo 是事实时间区间（含），由调用方从窗口区间推出来，model 不做窗口推断。
	AggregateWindow(
		ctx context.Context, subjectColumn string, eventTypes []string, contentTypes []int32,
		windowType int32, timeFrom, timeTo, subjectID int64,
	) ([]*WindowAggRow, error)
	// ScanForRetention 按 mid 游标扫描区间内的活跃用户及其最早活动时间。
	// afterMid 传上一页最后一个 mid（首页传 0），limit 为批大小（<=0 用默认批，上限 clampBatch）。
	// 游标而不是 OFFSET 分页：留存回填要遍历全部活跃 mid，OFFSET 越翻越慢，
	// 且中途有新 mid 落库时会漏扫或重扫。
	// 只统计 mid > 0：playback.heartbeat 的观看者只以 mid_hash 到达（不带 mid），
	// 把假名与 mid 混在一个游标里会让「游客」与「登录用户」无法区分；
	// 因此留存的分桶输入只能用带 mid 的事件（behavior.* 通道的 mid 来自 EventContext）。
	ScanForRetention(
		ctx context.Context, eventTypes []string, from, to, afterMid int64, limit int32,
	) ([]*MidActivity, error)
	// CountDistinctMids 统计区间内去重活跃用户数（留存的 cohort_size / retained 输入）。
	// 与 ScanForRetention 同一条 mid > 0 约束；zoneID>0 时按分区过滤，
	// 而分区列只能由 mapping 从 spm_content_projection 补齐，分区未知的行为会被排除在外。
	CountDistinctMids(ctx context.Context, eventTypes []string, from, to, zoneID int64) (int64, error)
	// DeleteExpired 删除 ctime 早于 before 的事实行，单批最多 limit 行。
	// 事实保留期由配置决定（README「数据保留策略」）；到期删除后，
	// 对应窗口的投影就不再可重算，这是保留期换存储的已知代价。
	DeleteExpired(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultBehaviorEventModel struct {
	conn sqlx.SqlConn
}

// NewBehaviorEventModel 创建 BehaviorEventModel 实现。
func NewBehaviorEventModel(conn sqlx.SqlConn) BehaviorEventModel {
	return &defaultBehaviorEventModel{conn: conn}
}

const behaviorEventColumns = "id, event_id, event_type, action_key, source_channel," +
	" schema_version, occurred_at, event_day, mid, pseudonym, pseudonym_kind, aid, content_id," +
	" content_type, zone_id, catalog_item_id, target_mid, dim_key, dim_value, num_value," +
	" trace_id, topic, partition_no, msg_offset, ctime, mtime"

// behaviorEventInsertCols 与参数顺序严格对应（不含自增主键）。
const behaviorEventInsertCols = "event_id, event_type, action_key, source_channel, schema_version," +
	" occurred_at, event_day, mid, pseudonym, pseudonym_kind, aid, content_id, content_type," +
	" zone_id, catalog_item_id, target_mid, dim_key, dim_value, num_value, trace_id, topic," +
	" partition_no, msg_offset, ctime, mtime"

// behaviorEventRowLen 是单个 VALUES 元组的列数。
const behaviorEventRowLen = 25

// allowedSubjectColumns 是 AggregateWindow 可接受的分组列白名单。
// 白名单而不是拼接任意字符串：主体维度必须由契约的 SubjectType 显式声明，
// 新增维度要在 rpc.SubjectType 里先加枚举值。
//
// 列与主体的对应关系（按上游实际能给出的字段，而不是按名字好看）：
//   - SubjectTypeAid / SubjectTypeCatalogItem -> content_id（必须再带 contentTypes 条件）；
//     aid 列只有 event-collector 通道有值，拿它分组会漏掉 playback.heartbeat 与
//     engagement.action 的完播与互动数据，因此只作溯源列保留。
//   - SubjectTypeZone -> zone_id（依赖 mapping 从内容投影补齐，0 行不参与分组）
//   - SubjectTypeMid -> mid / target_mid（前者是行为发起者，后者是被作用者）
var allowedSubjectColumns = map[string]struct{}{
	"aid": {}, "zone_id": {}, "mid": {}, "catalog_item_id": {}, "target_mid": {}, "content_id": {},
}

// requiresContentType 判断按该主体列分组时是否必须限定 content_type。
//
// content_id / aid / catalog_item_id 都住在「内容主键」这一列里，但上游对
// 不同内容类型发的是不同 ID 空间（UGC=稿件 aid、PGC=剧集 episode_id、直播=房间号），
// 数值可以相同。分组时不加 content_type 条件，就等于把「某剧第 3 集」和
// 「某个 aid 恰为 3 的稿件」加进同一个窗口——GROUP BY 的结果看起来完全正常，
// 所以必须在 model 层硬拒，而不是写进注释指望调用方记得。
// zone_id / mid / target_mid 与内容类型正交（分区本身不区分 UGC/PGC，
// 用户维度更是），限定它只会让调用方多传一个无意义参数。
func requiresContentType(column string) bool {
	switch column {
	case "content_id", "aid", "catalog_item_id":
		return true
	default:
		return false
	}
}

func (m *defaultBehaviorEventModel) InsertIfAbsent(
	ctx context.Context, e *BehaviorEvent,
) (bool, error) {
	if strings.TrimSpace(e.EventID) == "" {
		return false, ErrEventIDEmpty
	}
	if !SupportedEventType(e.EventType) {
		return false, ErrUnsupportedEventType
	}
	if e.OccurredAt <= 0 {
		return false, ErrInvalidWindow
	}
	if !ValidSourceChannel(e.SourceChannel) {
		return false, ErrInvalidSourceChannel
	}
	if strings.TrimSpace(e.ActionKey) == "" {
		return false, ErrInvalidActionKey
	}
	// content_type 只在非零时校验：0 表示「这条事件本来就不针对内容」（如纯曝光心跳），
	// 而一个越界值一定是 mapping 写错了，绝不能让它进事实表——
	// 内容维度口径（完播率、分区热度）会按它选表，脏值会让投影静默算错。
	if e.ContentType != 0 && !ValidContentType(e.ContentType) {
		return false, ErrInvalidSubject
	}
	e.EventDay = dayStartUnix(e.OccurredAt)
	now := nowUnix()
	// 自赋值让重复键 affected=0，从而区分「新事件」与「已落库事件」。
	query := "INSERT INTO spm_behavior_event (" + behaviorEventInsertCols +
		") VALUES (" + rowPlaceholders(behaviorEventRowLen, 1) + ")" +
		" ON DUPLICATE KEY UPDATE event_id = event_id"
	res, err := m.conn.ExecCtx(ctx, query,
		e.EventID, e.EventType, truncate(e.ActionKey, 32), e.SourceChannel, e.SchemaVersion,
		e.OccurredAt, e.EventDay, e.Mid,
		truncate(e.Pseudonym, 128), truncate(e.PseudonymKind, 32),
		e.Aid, e.ContentID, e.ContentType, e.ZoneID, e.CatalogItemID, e.TargetMid,
		truncate(e.DimKey, 64), truncate(e.DimValue, 128), e.NumValue,
		truncate(e.TraceID, 64), truncate(e.Topic, 128), e.PartitionNo, e.MsgOffset, now, now)
	if err != nil {
		return false, fmt.Errorf("spm_behavior_event InsertIfAbsent: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spm_behavior_event InsertIfAbsent RowsAffected: %w", err)
	}
	if affected == 0 {
		return false, nil
	}
	e.Ctime, e.Mtime = now, now
	return true, nil
}

func (m *defaultBehaviorEventModel) FindByEventID(
	ctx context.Context, eventID string,
) (*BehaviorEvent, error) {
	var row BehaviorEvent
	query := "SELECT " + behaviorEventColumns + " FROM spm_behavior_event WHERE event_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, eventID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_behavior_event FindByEventID: %w", err)
	}
	return &row, nil
}

// buildEventFilter 组装「事件类型白名单 + 时间区间」的公共条件。
// 时间区间必须显式给出且方向正确：无界扫描事实表会拖垮在线库，
// 而 from>to 的区间在 BETWEEN 下恒为空集——那会把「参数算错了」伪装成「这段时间没数据」。
func buildEventFilter(eventTypes []string, from, to int64) (string, []any, error) {
	if from <= 0 || to < from {
		return "", nil, ErrInvalidWindow
	}
	clause := " occurred_at BETWEEN ? AND ?"
	args := []any{from, to}
	if len(eventTypes) > 0 {
		holders := make([]string, 0, len(eventTypes))
		for range eventTypes {
			holders = append(holders, "?")
		}
		clause += " AND event_type IN (" + strings.Join(holders, ",") + ")"
		for _, t := range eventTypes {
			args = append(args, t)
		}
	}
	return clause, args, nil
}

func (m *defaultBehaviorEventModel) AggregateWindow(
	ctx context.Context, subjectColumn string, eventTypes []string, contentTypes []int32,
	windowType int32, timeFrom, timeTo, subjectID int64,
) ([]*WindowAggRow, error) {
	if _, ok := allowedSubjectColumns[subjectColumn]; !ok {
		return nil, ErrInvalidSubject
	}
	if !ValidWindowType(windowType) {
		return nil, ErrInvalidWindow
	}
	// 按内容主键分组却不限制 content_type，就会把 UGC 的 aid 与 PGC 的 episode_id
	// 这两个互不相干的 ID 空间并成一组（数值相同就并成同一个主体）。
	// 这类错误在结果里看不出来，所以在这里硬拒而不是「提醒调用方」。
	if requiresContentType(subjectColumn) && len(contentTypes) == 0 {
		return nil, ErrInvalidSubject
	}
	for _, ct := range contentTypes {
		if !ValidContentType(ct) {
			return nil, ErrInvalidSubject
		}
	}
	clause, args, err := buildEventFilter(eventTypes, timeFrom, timeTo)
	if err != nil {
		return nil, err
	}
	if len(contentTypes) > 0 {
		holders := make([]string, 0, len(contentTypes))
		for range contentTypes {
			holders = append(holders, "?")
		}
		clause += " AND content_type IN (" + strings.Join(holders, ",") + ")"
		for _, ct := range contentTypes {
			args = append(args, ct)
		}
	}
	// window_start 在 SQL 侧用整数除法规整，语义与 AlignWindow 一致；
	// 两侧必须同步修改，否则实时与离线会算出两套窗口。
	sec := WindowSeconds(windowType)
	expr := "0"
	if sec > 0 {
		expr = fmt.Sprintf("(occurred_at - occurred_at %% %d)", sec)
	}
	query := "SELECT " + subjectColumn + " AS subject_id, " + expr + " AS window_start," +
		" COUNT(*) AS event_count, SUM(num_value) AS num_sum," +
		" COUNT(DISTINCT mid) AS distinct_actors, MAX(occurred_at) AS last_event_time" +
		" FROM spm_behavior_event WHERE" + clause +
		" AND " + subjectColumn + " > 0"
	if subjectID > 0 {
		query += " AND " + subjectColumn + " = ?"
		args = append(args, subjectID)
	}
	query += " GROUP BY subject_id, window_start ORDER BY subject_id ASC, window_start ASC"
	var rows []*WindowAggRow
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_behavior_event AggregateWindow: %w", err)
	}
	return rows, nil
}

func (m *defaultBehaviorEventModel) ScanForRetention(
	ctx context.Context, eventTypes []string, from, to, afterMid int64, limit int32,
) ([]*MidActivity, error) {
	clause, args, err := buildEventFilter(eventTypes, from, to)
	if err != nil {
		return nil, err
	}
	query := "SELECT mid, MIN(occurred_at) AS first_at FROM spm_behavior_event WHERE" + clause +
		" AND mid > ?"
	args = append(args, afterMid)
	query += " GROUP BY mid ORDER BY mid ASC LIMIT ?"
	args = append(args, clampBatch(limit))
	var rows []*MidActivity
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_behavior_event ScanForRetention: %w", err)
	}
	return rows, nil
}

func (m *defaultBehaviorEventModel) CountDistinctMids(
	ctx context.Context, eventTypes []string, from, to, zoneID int64,
) (int64, error) {
	clause, args, err := buildEventFilter(eventTypes, from, to)
	if err != nil {
		return 0, err
	}
	query := "SELECT COUNT(DISTINCT mid) FROM spm_behavior_event WHERE" + clause + " AND mid > 0"
	if zoneID > 0 {
		query += " AND zone_id = ?"
		args = append(args, zoneID)
	}
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_behavior_event CountDistinctMids: %w", err)
	}
	return n, nil
}

func (m *defaultBehaviorEventModel) DeleteExpired(
	ctx context.Context, before int64, limit int32,
) (int64, error) {
	// 不校验 before 的合理性：0 或负数只会让 ctime < ? 命中空集，删不坏数据；
	// 而「保留期填 0 天」这种配置错误必须由 logic 层在算 cutoff 时拦住
	// （见 README「数据保留策略」），model 层看不到业务意图，不该自作主张。
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM spm_behavior_event WHERE ctime < ? ORDER BY id ASC LIMIT ?",
		before, clampBatch(limit))
	if err != nil {
		return 0, fmt.Errorf("spm_behavior_event DeleteExpired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_behavior_event DeleteExpired RowsAffected: %w", err)
	}
	return n, nil
}
