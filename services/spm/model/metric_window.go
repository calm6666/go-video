package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// MetricWindow 窗口指标投影行（spm_metric_window 表投影）。
//
// 这是「投影」而不是事实源：任何一行都可以由 spm_behavior_event + 对应口径版本重算得到
// （docs/data-design.md §5）。因此本表允许被覆盖式重写，这也是 WriteMetricWindow 采用
// 幂等覆盖而非累加的原因——同窗口重放不能把计数加两遍。
//
// 比率类指标必须同时写 numerator/denominator：只有 value 的话，跨窗口合并只能做加权
// 平均近似，回填后完播率会对不上。计数类指标把计数同时写进 sample_count。
type MetricWindow struct {
	ID            int64   `db:"id"`               // 主键 ID
	SubjectType   int32   `db:"subject_type"`     // 主体类型（rpc.SubjectType）
	SubjectID     int64   `db:"subject_id"`       // 主体主键
	MetricKey     string  `db:"metric_key"`       // 指标键
	MetricVersion int32   `db:"metric_version"`   // 口径版本
	WindowType    int32   `db:"window_type"`      // 窗口粒度（rpc.WindowType）
	WindowStart   int64   `db:"window_start"`     // 窗口左边界（Unix 秒，已规整；TOTAL 恒为 0）
	MetricValue   float64 `db:"metric_value"`     // 指标值
	Numerator     int64   `db:"numerator"`        // 分子（比率类必填）
	Denominator   int64   `db:"denominator"`      // 分母（比率类必填）
	SampleCount   int64   `db:"sample_count"`     // 参与聚合的样本数
	Source        int32   `db:"source"`           // 写入来源（rpc.MetricSource，只有计算链路）
	EventTime     int64   `db:"event_time"`       // 该窗口最后一次推进时间（迟到判定基准）
	WriteReqID    string  `db:"write_request_id"` // 最近一次写入的幂等键（排查重复回填写入）
	Ctime         int64   `db:"ctime"`            // 创建时间（Unix 秒）
	Mtime         int64   `db:"mtime"`            // 修改时间（Unix 秒）
}

// MetricKeyVersion 口径定位对。Version=0 表示「调用方要当前 ACTIVE 版本」，
// 由 logic 层在查口径后填成具体版本，model 层不参与这层解析。
type MetricKeyVersion struct {
	MetricKey string
	Version   int32
}

// HotSubject 热度榜行：只回主体主键与指标值，不回任何行为明细（AGENTS.md §7 隐私边界）。
type HotSubject struct {
	SubjectID   int64   `db:"subject_id"`
	MetricValue float64 `db:"metric_value"`
	Numerator   int64   `db:"numerator"`
	Denominator int64   `db:"denominator"`
}

// MetricWindowModel spm_metric_window 表读写接口。
type MetricWindowModel interface {
	// UpsertBatch 幂等覆盖写入一批窗口指标。
	// allowLate=false 时，晚于已存在 event_time 的迟到数据不覆盖既有值（只刷新 mtime 之外
	// 的字段都不做），用于保护实时链路已经推进的窗口不被旧回填数据盖回去。
	// 返回实际影响的行数（MySQL 对「值未变化」的行不计入 affected，故仅作参考）。
	UpsertBatch(ctx context.Context, rows []*MetricWindow, allowLate bool) (int64, error)
	// FindOne 精确读取一个「主体 × 口径版本 × 窗口」。不存在返回 nil。
	FindOne(
		ctx context.Context, subjectType int32, subjectID int64, metricKey string,
		version, windowType int32, windowStart int64,
	) (*MetricWindow, error)
	// ListByKeyWindows 读取单主体、多口径、连续窗口区间，供 BatchGetMetrics 使用。
	// windowStarts 必须是同一 windowType 的已规整左边界集合。
	ListByKeyWindows(
		ctx context.Context, subjectType int32, subjectID int64, windowType int32,
		keys []MetricKeyVersion, windowStarts []int64,
	) ([]*MetricWindow, error)
	// ListHot 按指标值倒序取榜单主体。zoneID>0 时经由 spm_content_projection 过滤分区
	// （分区归属不在 spm 的写权限内，因此用本地只读投影做 JOIN，见 AGENTS.md §5）。
	// subjectType 只允许 AID/CATALOG_ITEM/ZONE：MID 维度出榜等价于「按用户排热度」，
	// 不是本期任何榜单的口径。
	ListHot(
		ctx context.Context, subjectType int32, metricKey string, version, windowType int32,
		windowStart, zoneID int64, offset, limit int32,
	) ([]*HotSubject, error)
	// CountHot 与 ListHot 同条件的主体总数（分页 total）。
	CountHot(
		ctx context.Context, subjectType int32, metricKey string, version, windowType int32,
		windowStart, zoneID int64,
	) (int64, error)
	// FindLatestWindowStart 取某口径版本在某粒度下已落库的最大 window_start；无数据返回 0。
	// 契约里 GetMetric/BatchGetMetrics/ListHotSubjects 的 window_start=0 表示
	// 「最近一个已闭合窗口」：首选 spm_window_watermark 的水位点查，本方法是水位行尚未建立时
	// （口径刚上线、水位回填未跑）的兜底，走 idx_metric_latest 的索引极值，不做全表扫描。
	// closedBefore>0 时只统计该时刻（含）之前左边界的水位，用来排除「还在写入的最新窗口」。
	FindLatestWindowStart(
		ctx context.Context, subjectType int32, metricKey string, version, windowType int32,
		closedBefore int64,
	) (int64, error)
	// DeleteByWindow 删除指定口径版本在某窗口区间内的投影行，单批最多 limit 行。
	// 重算前的「先清后写」入口：不清就覆盖的话，事实里已消失的主体会留下幽灵窗口。
	// limit 是硬约束而不是优化：一个「口径 × 小时区间 × 全部主体」的删除可以命中
	// 上百万行，无 LIMIT 的 DELETE 会在事务里持有大量行锁（迁移脚本的锁风险说明同源）。
	// 调用方按「返回行数 == limit 就继续」循环到清空为止。
	DeleteByWindow(
		ctx context.Context, metricKey string, version, windowType int32,
		windowStartFrom, windowStartTo int64, subjectType int32, subjectID int64, limit int32,
	) (int64, error)
	// ListByNaturalKeys 按 uniq_metric 六列回读已存在的投影行，用于 WriteMetricWindow 的
	// 「同 request_id 重放判定」：write_request_id 上没有索引（迁移 000002 的索引清单可查），
	// 按它查询等于扫全表，所以重放判定只能走唯一键。
	// 命中行由调用方比对 write_request_id：六列全部已带同一幂等键即说明这批已写过。
	// rows 里自然键不合法的条目直接报错而不是跳过——跳过会让「键写错了」伪装成「没写过」。
	ListByNaturalKeys(ctx context.Context, rows []*MetricWindow) ([]*MetricWindow, error)
	// WithSession 绑定事务句柄：指标行必须与它描述的水位推进在同一事务里落库，
	// 否则会出现「水位已前进、对应窗口还没落库」的空窗口读（见 WindowWatermarkModel.WithSession）。
	WithSession(session sqlx.Session) MetricWindowModel
}

type defaultMetricWindowModel struct {
	// session 而不是 conn：sqlx.SqlConn 本身实现 sqlx.Session，
	// WithSession 之后读写都落在调用方的事务连接上（事务内不能再拿新连接）。
	session sqlx.Session
}

// NewMetricWindowModel 创建 MetricWindowModel 实现。
func NewMetricWindowModel(conn sqlx.SqlConn) MetricWindowModel {
	return &defaultMetricWindowModel{session: conn}
}

// WithSession 见接口注释：session 为 nil 时保持原连接，避免调用方误传空句柄后静默丢写。
func (m *defaultMetricWindowModel) WithSession(session sqlx.Session) MetricWindowModel {
	if session == nil {
		return m
	}
	return &defaultMetricWindowModel{session: session}
}

const metricWindowColumns = "id, subject_type, subject_id, metric_key, metric_version," +
	" window_type, window_start, metric_value, numerator, denominator, sample_count," +
	" source, event_time, write_request_id, ctime, mtime"

// metricWindowRowLen 是单个 VALUES 元组的列数，改动 UpsertBatch 列清单时必须同步。
const metricWindowRowLen = 15

// 批量读写上限，逐条对齐 rpc 契约的注释（改这里必须同时改契约与 README，两边不能各说一套）：
//   - WriteMetricWindowReq.points「单次上限 500」；
//   - BatchGetMetricsReq.keys「最多 50 个口径」× window_count「1..30」。
const (
	maxMetricPointsPerBatch = 500
	maxMetricKeyLen         = 100
	maxMetricKeysPerRequest = 50
	maxWindowsPerRequest    = 30
)

// metricWindowInsertCols 与 metricWindowRowLen 必须和 UpsertBatch 的参数顺序严格一致。
const metricWindowInsertCols = "subject_type, subject_id, metric_key, metric_version," +
	" window_type, window_start, metric_value, numerator, denominator, sample_count," +
	" source, event_time, write_request_id, ctime, mtime"

func (m *defaultMetricWindowModel) UpsertBatch(
	ctx context.Context, rows []*MetricWindow, allowLate bool,
) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	// 与契约 WriteMetricWindowReq.points 的「单次上限 500」同源。model 层也守一次：
	// 单条多值 INSERT 的占位符数 = 行数 × 15，越界的批次会在解析阶段就把连接打满。
	if len(rows) > maxMetricPointsPerBatch {
		return 0, ErrTooManyPoints
	}
	now := nowUnix()
	args := make([]any, 0, len(rows)*metricWindowRowLen)
	// 批内重复的自然键会被 ON DUPLICATE 折叠成「后写覆盖先写」，返回的行数却按提交数算，
	// 调用方据此以为写入了两行。这里显式拒绝，逼上游先把批次去重做完。
	// 键在下面窗口边界规整之后才构造：100 与 120 在 5 分钟窗口下是同一个窗口。
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		if !ValidSubjectType(r.SubjectType) || r.SubjectID <= 0 {
			return 0, ErrInvalidSubject
		}
		if strings.TrimSpace(r.MetricKey) == "" || r.MetricVersion <= 0 {
			return 0, ErrMetricVersionRequired
		}
		if !ValidWindowType(r.WindowType) {
			return 0, ErrInvalidWindow
		}
		if !ValidMetricSource(r.Source) {
			return 0, ErrInvalidMetricSource
		}
		// 计数列非负：比率类可以合法地小于 1，但分子/分母/样本数出现负值只可能是
		// 上游算错或重复回写，落到榜上就是「负播放量」。
		if r.Numerator < 0 || r.Denominator < 0 || r.SampleCount < 0 {
			return 0, ErrNegativeMetricPoint
		}
		// event_time 是迟到判定的基准，必须真给出：0 在 allowLate=false 下会让
		// 既有的 event_time <= 0 恒不成立，这一行就被静默丢弃——写接口返回成功，
		// 库里却一个点都没有，是最难查的那种失败。
		if r.EventTime <= 0 {
			return 0, ErrEventTimeRequired
		}
		// 窗口边界由 model 层再规整一次：logic 层漏规整会让同一分钟的数据散到多行，
		// 而这种数据错误在榜单上表现为「重复主体」，事后很难归因。
		r.WindowStart = AlignWindow(r.WindowStart, r.WindowType)
		naturalKey := fmt.Sprintf("%d:%d:%s:%d:%d:%d", r.SubjectType, r.SubjectID,
			truncate(r.MetricKey, maxMetricKeyLen), r.MetricVersion, r.WindowType, r.WindowStart)
		if _, dup := seen[naturalKey]; dup {
			return 0, ErrDuplicatePointInBatch
		}
		seen[naturalKey] = struct{}{}
		if r.Ctime == 0 {
			r.Ctime = now
		}
		r.Mtime = now
		args = append(args,
			r.SubjectType, r.SubjectID, truncate(r.MetricKey, 100), r.MetricVersion, r.WindowType,
			r.WindowStart, r.MetricValue, r.Numerator, r.Denominator, r.SampleCount, r.Source,
			r.EventTime, truncate(r.WriteReqID, 128), r.Ctime, now)
	}

	// 幂等覆盖：同 uniq_metric 的行整行替换，绝不累加。
	// 迟到保护不用占位符而用两种固定表达式分支：占位符出现在 ON DUPLICATE 子句里时，
	// 多行 VALUES 与子句参数的绑定顺序极易写错，而 allowLate 是服务端自己算出的布尔值，
	// 直接决定 SQL 形态更安全。allowLate=false 时，只有 event_time 不早于已写入数据的
	// 新值才生效，旧回填不会把实时链路已推进的窗口盖回去。
	// 赋值顺序有讲究：MySQL 的 ON DUPLICATE KEY UPDATE 从左到右求值，event_time 必须放最后，
	// 否则前面几列的迟到判定读到的就是刚被覆盖的新 event_time，迟到保护会失效。
	guard := "col = IF(event_time <= VALUES(event_time), VALUES(col), col)"
	if allowLate {
		guard = "col = VALUES(col)"
	}
	sets := make([]string, 0, 6)
	for _, col := range []string{
		"metric_value", "numerator", "denominator", "sample_count", "source", "event_time",
	} {
		sets = append(sets, strings.ReplaceAll(guard, "col", col))
	}
	query := "INSERT INTO spm_metric_window (" + metricWindowInsertCols +
		") VALUES " + rowPlaceholders(metricWindowRowLen, len(rows)) +
		" ON DUPLICATE KEY UPDATE " + strings.Join(sets, ", ") +
		", write_request_id = VALUES(write_request_id), mtime = VALUES(mtime)"

	res, err := m.session.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("spm_metric_window UpsertBatch: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_metric_window UpsertBatch RowsAffected: %w", err)
	}
	return affected, nil
}

func (m *defaultMetricWindowModel) FindOne(
	ctx context.Context, subjectType int32, subjectID int64, metricKey string,
	version, windowType int32, windowStart int64,
) (*MetricWindow, error) {
	var row MetricWindow
	query := "SELECT " + metricWindowColumns + " FROM spm_metric_window" +
		" WHERE subject_type = ? AND subject_id = ? AND metric_key = ? AND metric_version = ?" +
		" AND window_type = ? AND window_start = ?"
	err := m.session.QueryRowCtx(ctx, &row, query,
		subjectType, subjectID, metricKey, version, windowType, AlignWindow(windowStart, windowType))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_metric_window FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultMetricWindowModel) ListByKeyWindows(
	ctx context.Context, subjectType int32, subjectID int64, windowType int32,
	keys []MetricKeyVersion, windowStarts []int64,
) ([]*MetricWindow, error) {
	if len(keys) == 0 || len(windowStarts) == 0 {
		return nil, nil
	}
	// 两个 IN 列表都是拼出来的占位符，不设上限就等于让一次 RPC 决定要生成多大的 SQL。
	if len(keys) > maxMetricKeysPerRequest {
		return nil, ErrTooManyKeys
	}
	if len(windowStarts) > maxWindowsPerRequest {
		return nil, ErrWindowRangeTooLarge
	}
	pairs := make([]string, 0, len(keys))
	args := []any{subjectType, subjectID, windowType}
	for _, k := range keys {
		pairs = append(pairs, "(metric_key = ? AND metric_version = ?)")
		args = append(args, k.MetricKey, k.Version)
	}
	starts := make([]string, 0, len(windowStarts))
	for _, s := range windowStarts {
		starts = append(starts, "?")
		args = append(args, AlignWindow(s, windowType))
	}
	query := "SELECT " + metricWindowColumns + " FROM spm_metric_window" +
		" WHERE subject_type = ? AND subject_id = ? AND window_type = ?" +
		" AND (" + strings.Join(pairs, " OR ") + ")" +
		" AND window_start IN (" + strings.Join(starts, ",") + ")" +
		" ORDER BY window_start ASC, metric_key ASC"
	var rows []*MetricWindow
	if err := m.session.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_metric_window ListByKeyWindows: %w", err)
	}
	return rows, nil
}

// validHotSubject 限定可出榜的主体维度。MID 出榜等价于「按用户排热度」，
// 本期没有任何这类榜单口径，放开只会给误用留口子；TOTAL 窗口不参与实时榜。
func validHotSubject(t int32) bool {
	return t == SubjectTypeAid || t == SubjectTypeZone || t == SubjectTypeCatalogItem
}

// buildHotQuery 组装榜单查询。内容维度的过滤走本地只读投影 spm_content_projection：
// 分区归属与上下架状态由 content.published.v1 维护，spm 不写 video/catalog 的表
// （AGENTS.md §5）。
//
// 两条正确性要求，都不是可选装饰：
//  1. LEFT JOIN + 剔除 state=hidden：**任何**榜单都要剔掉已下架/过期/删除的内容，
//     不限分区时也不例外。早期实现只在 zone_id>0 时才 JOIN，等于「不选分区就能把
//     违规内容留在榜上」，而推荐召回正是拿不限分区的榜去用的。
//  2. 没有投影行的主体按「可见」放行：投影滞后于事件投递，如果按未知即隐藏，
//     content.published 消费者一中断所有榜单就集体清空，故障面远大于收益。
//     下架内容一定有事件、因而一定有投影行，所以这条放行不会放过已知违规项。
func buildHotQuery(
	subjectType int32, metricKey string, version, windowType int32, windowStart, zoneID int64,
	selectCols string,
) (string, []any, error) {
	if !validHotSubject(subjectType) {
		return "", nil, ErrInvalidSubject
	}
	if strings.TrimSpace(metricKey) == "" || version <= 0 {
		return "", nil, ErrMetricVersionRequired
	}
	if !ValidWindowType(windowType) {
		return "", nil, ErrInvalidWindow
	}
	args := []any{subjectType, metricKey, version, windowType, AlignWindow(windowStart, windowType)}
	// 分区榜按分区自身聚合，主体就是分区，与内容投影无关。
	needProjection := subjectType != SubjectTypeZone
	query := selectCols + " FROM spm_metric_window mw"
	if needProjection {
		query += " LEFT JOIN spm_content_projection cp" +
			" ON cp.subject_type = mw.subject_type AND cp.subject_id = mw.subject_id"
	}
	if zoneID > 0 && !needProjection {
		// 分区榜再按分区过滤是调用方参数矛盾，直接拒绝而不是静默忽略。
		return "", nil, ErrInvalidSubject
	}
	query += " WHERE mw.subject_type = ? AND mw.metric_key = ? AND mw.metric_version = ?" +
		" AND mw.window_type = ? AND mw.window_start = ?"
	// 占位符与参数必须同序追加：JOIN 段本身没有参数，所以 state / zone_id 排在五个
	// WHERE 条件之后。顺序写错不会报错，只会拿错值去比，是这类拼接最阴的坑。
	if needProjection {
		query += " AND (cp.id IS NULL OR cp.state = ?)"
		args = append(args, ContentStateNormal)
		if zoneID > 0 {
			query += " AND cp.zone_id = ?"
			args = append(args, zoneID)
		}
	}
	return query, args, nil
}

func (m *defaultMetricWindowModel) ListHot(
	ctx context.Context, subjectType int32, metricKey string, version, windowType int32,
	windowStart, zoneID int64, offset, limit int32,
) ([]*HotSubject, error) {
	// limit<=0 不再返回空页：那会把「调用方漏传分页参数」伪装成「这个榜没有数据」，
	// 与禁止假成功同一条线。收敛到默认页大小后由调用方决定是否翻页。
	query, args, err := buildHotQuery(subjectType, metricKey, version, windowType, windowStart,
		zoneID, "SELECT mw.subject_id, mw.metric_value, mw.numerator, mw.denominator")
	if err != nil {
		return nil, err
	}
	query += " ORDER BY mw.metric_value DESC, mw.subject_id ASC LIMIT ? OFFSET ?"
	args = append(args, clampLimit(limit), clampOffset(offset))
	var rows []*HotSubject
	if err := m.session.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_metric_window ListHot: %w", err)
	}
	return rows, nil
}

func (m *defaultMetricWindowModel) CountHot(
	ctx context.Context, subjectType int32, metricKey string, version, windowType int32,
	windowStart, zoneID int64,
) (int64, error) {
	query, args, err := buildHotQuery(subjectType, metricKey, version, windowType, windowStart,
		zoneID, "SELECT COUNT(*)")
	if err != nil {
		return 0, err
	}
	var n int64
	if err := m.session.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_metric_window CountHot: %w", err)
	}
	return n, nil
}

func (m *defaultMetricWindowModel) FindLatestWindowStart(
	ctx context.Context, subjectType int32, metricKey string, version, windowType int32,
	closedBefore int64,
) (int64, error) {
	if !ValidSubjectType(subjectType) {
		return 0, ErrInvalidSubject
	}
	if strings.TrimSpace(metricKey) == "" || version <= 0 {
		return 0, ErrMetricVersionRequired
	}
	if !ValidWindowType(windowType) {
		return 0, ErrInvalidWindow
	}
	// MAX(window_start) 是索引极值查询：等值条件必须凑齐 idx_metric_latest 的前四列，
	// 少一个条件就退化成扫描，所以这里不接受「不限主体/不限口径」的调用。
	query := "SELECT MAX(window_start) FROM spm_metric_window" +
		" WHERE metric_key = ? AND metric_version = ? AND subject_type = ? AND window_type = ?"
	args := []any{metricKey, version, subjectType, windowType}
	if closedBefore > 0 {
		query += " AND window_start <= ?"
		args = append(args, AlignWindow(closedBefore, windowType))
	}
	// 空集时 MAX 返回 NULL，直接扫进 int64 会报「converting NULL」，
	// 而这里 NULL 是有意义的答案（该口径还没有窗口），必须用 NullInt64 承接成 0。
	var maxStart sql.NullInt64
	if err := m.session.QueryRowCtx(ctx, &maxStart, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("spm_metric_window FindLatestWindowStart: %w", err)
	}
	if !maxStart.Valid {
		return 0, nil
	}
	return maxStart.Int64, nil
}

func (m *defaultMetricWindowModel) DeleteByWindow(
	ctx context.Context, metricKey string, version, windowType int32,
	windowStartFrom, windowStartTo int64, subjectType int32, subjectID int64, limit int32,
) (int64, error) {
	if strings.TrimSpace(metricKey) == "" || version <= 0 {
		return 0, ErrMetricVersionRequired
	}
	if !ValidWindowType(windowType) {
		return 0, ErrInvalidWindow
	}
	// 区间颠倒不静默当成空集：那会让「重算跑到 0 行」看起来像数据本来就干净。
	if windowStartTo < windowStartFrom {
		return 0, ErrInvalidWindow
	}
	from := AlignWindow(windowStartFrom, windowType)
	query := "DELETE FROM spm_metric_window WHERE metric_key = ? AND metric_version = ?" +
		" AND window_type = ? AND window_start BETWEEN ? AND ?"
	args := []any{metricKey, version, windowType, from, AlignWindow(windowStartTo, windowType)}
	// 主体维度缺省时按「全部主体 + 该口径该窗口区间」删除。这是修复链路唯一允许的
	// 批量删除形态：没有显式口径版本与窗口区间就删不到任何数据（WHERE 1=0 兜底）。
	if subjectType != SubjectTypeUnspecified || subjectID != 0 {
		if !ValidSubjectType(subjectType) || subjectID <= 0 {
			return 0, ErrInvalidSubject
		}
		query += " AND subject_type = ? AND subject_id = ?"
		args = append(args, subjectType, subjectID)
	}
	// ORDER BY id 让分批删除有确定顺序：没有 ORDER BY 时两批之间可能反复挑到同一批
	// 行（优化器按索引顺序变化选择），循环退出条件就不可信。
	query += " ORDER BY id ASC LIMIT ?"
	args = append(args, clampBatch(limit))
	res, err := m.session.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("spm_metric_window DeleteByWindow: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_metric_window DeleteByWindow RowsAffected: %w", err)
	}
	return n, nil
}

// naturalKeyChunkSize 是 ListByNaturalKeys 单条 SQL 携带的自然键个数。
// 每个键是 uniq_metric 的六列等值条件，100 个键 = 600 个占位符、100 次索引下钻；
// 再大就让单条 SQL 的解析与优化成本线性上升，而调用方一批本来就有 500 行上限。
const naturalKeyChunkSize = 100

func (m *defaultMetricWindowModel) ListByNaturalKeys(
	ctx context.Context, rows []*MetricWindow,
) ([]*MetricWindow, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	// 与 UpsertBatch 同一批大小上界：这个方法是给「写前先查重放」用的，
	// 允许更大的入参只会让重放判定本身变成一次大查询。
	if len(rows) > maxMetricPointsPerBatch {
		return nil, ErrTooManyPoints
	}
	out := make([]*MetricWindow, 0, len(rows))
	for start := 0; start < len(rows); start += naturalKeyChunkSize {
		end := start + naturalKeyChunkSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		conds := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*6)
		for _, r := range chunk {
			if !ValidSubjectType(r.SubjectType) || r.SubjectID <= 0 {
				return nil, ErrInvalidSubject
			}
			if strings.TrimSpace(r.MetricKey) == "" || r.MetricVersion <= 0 {
				return nil, ErrMetricVersionRequired
			}
			if !ValidWindowType(r.WindowType) {
				return nil, ErrInvalidWindow
			}
			// 只查幂等判定需要的列：值本身由调用方稍后用 UPSERT 覆盖，
			// 这里回读整行会把 DOUBLE 与文本列一起搬进内存。
			conds = append(conds, " (subject_type = ? AND subject_id = ? AND metric_key = ?"+
				" AND metric_version = ? AND window_type = ? AND window_start = ?)")
			args = append(args, r.SubjectType, r.SubjectID,
				truncate(r.MetricKey, maxMetricKeyLen), r.MetricVersion, r.WindowType,
				// 与 UpsertBatch 相同的规整规则：不规整就会拿 100 去查 0，
				// 明明已写过的行被读成「没写过」，重放就会再覆盖一次。
				AlignWindow(r.WindowStart, r.WindowType))
		}
		query := "SELECT id, subject_type, subject_id, metric_key, metric_version, window_type," +
			" window_start, source, event_time, write_request_id, ctime, mtime" +
			" FROM spm_metric_window WHERE " + strings.Join(conds, " OR ")
		var found []*MetricWindow
		if err := m.session.QueryRowsCtx(ctx, &found, query, args...); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("spm_metric_window ListByNaturalKeys: %w", err)
		}
		out = append(out, found...)
	}
	return out, nil
}
