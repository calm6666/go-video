package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RecallOutbox 领域事件表（recall_outbox）。
//
// 与业务写操作在同一事务内落库，由 internal/publisher 的发布循环投递（AGENTS.md §5 Outbox）。
// 投递进度只有 state/retry_count/next_retry_at 三列是权威：判死后的行停在 state=2 等人工放行，
// 轮询永远不会再把它投出去（见 ListPending 与 README 缺口 B6）。
// 事件语义只有池版本变化（recall.pool.published.v1），不含任何商业化字段。
type RecallOutbox struct {
	ID            int64  `db:"id"`             // 自增主键（发布器按 id 升序投递）
	EventID       string `db:"event_id"`       // 事件唯一 ID（ULID，消费者按此幂等去重）
	EventType     string `db:"event_type"`     // 事件类型（recall.pool.published）
	SchemaVersion int32  `db:"schema_version"` // payload schema 版本
	AggregateType string `db:"aggregate_type"` // 聚合根类型（recall_pool）
	AggregateID   string `db:"aggregate_id"`   // 聚合根 ID（source:pool_key:version）
	Payload       string `db:"payload"`        // 事件信封完整 JSON（common/eventenvelope.Envelope）
	State         int32  `db:"state"`          // 0 待发布、1 已发布、2 失败（超重试上限）
	RetryCount    int32  `db:"retry_count"`    // 已重试次数
	NextRetryAt   int64  `db:"next_retry_at"`  // 下次重试时间（Unix 秒，0 表示可立即投递）
	OccurredAt    int64  `db:"occurred_at"`    // 事件发生时间（Unix 秒）
	LastError     string `db:"last_error"`     // 最近一次投递错误
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// RecallOutboxModel recall_outbox 表读写接口。
//
// 三个 Mark* 的签名与 common/outbox.Store 逐一对齐（本服务发布器就是那个引擎的适配层），
// 因此这里不再保留批量 MarkSent：引擎一轮内逐行给结论，批量接口只会多出一个
// 「一批里第 3 行失败后前 2 行怎么算」的第二口径。
type RecallOutboxModel interface {
	// Insert 写入事件行；event_id 重复返回 ErrEventExists（生产者重试的幂等命中）。
	// session != nil 时与业务写操作同事务（AGENTS.md §5：事务内写业务数据 + Outbox 记录）。
	// 传 nil 只用于补偿脚本；PublishPoolVersion / RollbackPoolVersion 必须传事务 session。
	Insert(ctx context.Context, session sqlx.Session, e *RecallOutbox) error
	// ListPending 拉取到期待发布的事件（id 升序，保证同库事件顺序）。
	// 只取 state=待发布：已判死（state=2）的行不在候选集里，否则每轮都会把
	// 转人工的事件重新投一遍，而「已转人工」这个结论本身会被轮询反复推翻。
	// limit 必须落在 (0, MaxOutboxBatch]：无上限拉取会让发布器一次锁住整张表。
	ListPending(ctx context.Context, now int64, limit int32) ([]*RecallOutbox, error)
	// MarkPublished 置为已发布并刷新 mtime（本表不另存发布时间列，mtime 即发布时间）。
	// 前置状态允许 待发布/失败：后者只在多副本竞争时出现（本副本投成功、别副本先判死），
	// 此时「已送达」是事实，必须盖过「已判死」。返回实际影响行数，0 行表示行已被别的副本判定。
	MarkPublished(ctx context.Context, id, publishedAt int64) (int64, error)
	// MarkRetry 保持待发布并写入退避到期时间与错误：retry_count 由 SQL 侧 +1，
	// 不采用调用方传来的计数（多副本各自 +1 才不会互相覆盖）。
	MarkRetry(ctx context.Context, id, nextRetryAt int64, lastError string) (int64, error)
	// MarkFailed 判死（state=2），之后只能人工放行；已是死信的行不再被本语句改写。
	MarkFailed(ctx context.Context, id int64, lastError string) (int64, error)
	// CountPending 统计待发布事件数（监控用）。
	// 候选集刻意包含失败态：运维必须看得见「积压里有多少已经转人工」。
	CountPending(ctx context.Context) (int64, error)
	// CountStuck 统计 occurred_at 早于 before 仍未发布的事件数（事件滞留告警口径）。
	CountStuck(ctx context.Context, before int64) (int64, error)
	// DeleteSentBefore 清理早于 before 的已发布事件（保留期治理，maxRows 限制单次行数）。
	DeleteSentBefore(ctx context.Context, before, maxRows int64) (int64, error)
}

type defaultRecallOutboxModel struct {
	conn sqlx.SqlConn
}

// NewRecallOutboxModel 创建 RecallOutboxModel 实现。
func NewRecallOutboxModel(conn sqlx.SqlConn) RecallOutboxModel {
	return &defaultRecallOutboxModel{conn: conn}
}

// exec 在事务 session 与全局连接之间选择执行器（与 audit 模型同一约定）。
func (m *defaultRecallOutboxModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

const outboxSelect = "SELECT id, event_id, event_type, schema_version, aggregate_type, aggregate_id, payload, " +
	"state, retry_count, next_retry_at, occurred_at, last_error, ctime, mtime FROM recall_outbox"

func (m *defaultRecallOutboxModel) Insert(ctx context.Context, session sqlx.Session, e *RecallOutbox) error {
	if strings.TrimSpace(e.EventID) == "" || strings.TrimSpace(e.EventType) == "" {
		return ErrEventRequired
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = PoolVersionSchemaVersion
	}
	if e.Ctime == 0 {
		e.Ctime = nowUnix()
	}
	e.Mtime = e.Ctime
	if e.OccurredAt == 0 {
		e.OccurredAt = e.Ctime
	}
	query := "INSERT INTO recall_outbox (event_id, event_type, schema_version, aggregate_type, aggregate_id, " +
		"payload, state, retry_count, next_retry_at, occurred_at, last_error, ctime, mtime) VALUES (" +
		inPlaceholders(13) + ")"
	if _, err := m.exec(session).ExecCtx(ctx, query,
		e.EventID, e.EventType, e.SchemaVersion, e.AggregateType, e.AggregateID, e.Payload,
		e.State, e.RetryCount, e.NextRetryAt, e.OccurredAt, e.LastError, e.Ctime, e.Mtime); err != nil {
		if isDuplicateErr(err) {
			return ErrEventExists
		}
		return fmt.Errorf("recall_outbox Insert: %w", err)
	}
	return nil
}

func (m *defaultRecallOutboxModel) ListPending(ctx context.Context, now int64, limit int32) ([]*RecallOutbox, error) {
	if err := CheckLimit(int(limit), MaxOutboxBatch); err != nil {
		return nil, err
	}
	// 只取待发布态：退避中的行由 next_retry_at 条件负责（MarkRetry 把行留在 state=0 并写未来时刻），
	// 所以不需要放宽 state 就能重投；反过来把 state=2 放进候选集等于「判死不存在」。
	query := outboxSelect + " WHERE state = ? AND next_retry_at <= ? ORDER BY id ASC LIMIT ?"
	var rows []*RecallOutbox
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, OutboxStatePending, now, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_outbox ListPending: %w", err)
	}
	return rows, nil
}

func (m *defaultRecallOutboxModel) MarkPublished(ctx context.Context, id, publishedAt int64) (int64, error) {
	if publishedAt <= 0 {
		publishedAt = nowUnix()
	}
	// 前置状态含失败态只在多副本竞争下可达（本副本投递成功、别副本先判死）：
	// 事件确实已送达，这个事实必须能盖过「判死」。反过来不加 state 条件就等于
	// 任何人拿到一个 id 都能把死信改写成语义不明的已发布。
	return affectedOf(ctx, m.conn, "recall_outbox MarkPublished",
		"UPDATE recall_outbox SET state = ?, last_error = '', mtime = ? WHERE id = ? AND state IN (?, ?)",
		OutboxStateSent, publishedAt, id, OutboxStatePending, OutboxStateFailed)
}

func (m *defaultRecallOutboxModel) MarkRetry(ctx context.Context, id, nextRetryAt int64, lastError string) (int64, error) {
	if nextRetryAt <= 0 {
		nextRetryAt = nowUnix()
	}
	// retry_count 在 SQL 侧自增：调用方（发布引擎）给的计数在多副本下会互相覆盖，
	// 而本表没有租约列，无法判定谁才是这条事件的第 n 次尝试。
	return affectedOf(ctx, m.conn, "recall_outbox MarkRetry",
		"UPDATE recall_outbox SET state = ?, retry_count = retry_count + 1, next_retry_at = ?, last_error = ?, mtime = ? "+
			"WHERE id = ? AND state IN (?, ?)",
		OutboxStatePending, nextRetryAt, trimLastError(lastError), nowUnix(), id,
		OutboxStatePending, OutboxStateFailed)
}

func (m *defaultRecallOutboxModel) MarkFailed(ctx context.Context, id int64, lastError string) (int64, error) {
	// WHERE 限定待发布：已是死信的行不能被第二次判定改写 last_error，
	// 否则人工排障时看到的是「后来某轮」的错误，而不是判死那一次的错误。
	return affectedOf(ctx, m.conn, "recall_outbox MarkFailed",
		"UPDATE recall_outbox SET state = ?, last_error = ?, mtime = ? WHERE id = ? AND state = ?",
		OutboxStateFailed, trimLastError(lastError), nowUnix(), id, OutboxStatePending)
}

// affectedOf 执行一条条件 UPDATE 并回传实际影响行数，错误串用 op 点名表名与操作。
//
// 影响行数 0 不是错误：调用方（发布器适配层）据此分辨「行已被别的副本判定」与
// 「写库失败」，前者只记日志，后者必须让整批中断。
func affectedOf(ctx context.Context, conn sqlx.SqlConn, op, query string, args ...any) (int64, error) {
	res, err := conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", op, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return affected, nil
}

// trimLastError 把投递错误夹进 last_error 列（VARCHAR(512)），并且绝不切断 UTF-8 序列。
//
// 为什么这条不能省：本仓库发布器产出的错误串是中文（label 与包装文案都是），
// 按字节硬切会在多字节字符中间留下半个序列，而 utf8mb4 列在严格模式下直接拒绝非法序列。
// 后果不是「日志少几个字」：MarkRetry/MarkFailed 本身报错会让整批中断，
// 同一批里其余本来能给出结论的行也一起没结论。
func trimLastError(s string) string {
	const maxBytes = 512
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	// 0x80..0xBF 是 UTF-8 续字节：从切点往前退到首个非续字节，保证 s[:cut] 以完整字符结尾。
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func (m *defaultRecallOutboxModel) CountPending(ctx context.Context) (int64, error) {
	var count int64
	query := "SELECT COUNT(1) FROM recall_outbox WHERE state IN (?, ?)"
	if err := m.conn.QueryRowCtx(ctx, &count, query, OutboxStatePending, OutboxStateFailed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("recall_outbox CountPending: %w", err)
	}
	return count, nil
}

// CountStuck 统计产生时间早于 before 仍未发布的事件数（事件滞留告警口径）。
func (m *defaultRecallOutboxModel) CountStuck(ctx context.Context, before int64) (int64, error) {
	if before <= 0 {
		return 0, ErrInvalidLimit
	}
	var count int64
	query := "SELECT COUNT(1) FROM recall_outbox WHERE state IN (?, ?) AND occurred_at < ?"
	if err := m.conn.QueryRowCtx(ctx, &count, query, OutboxStatePending, OutboxStateFailed, before); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("recall_outbox CountStuck: %w", err)
	}
	return count, nil
}

// DeleteSentBefore 清理早于 before 的已发布事件行（保留期治理）。
// 只删 state=SENT 的行：待发布与死信行必须保留，否则丢事件证据。
func (m *defaultRecallOutboxModel) DeleteSentBefore(ctx context.Context, before, maxRows int64) (int64, error) {
	if before <= 0 {
		return 0, ErrInvalidLimit
	}
	if err := CheckInt64Limit(maxRows, MaxDeleteRows); err != nil {
		return 0, err
	}
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM recall_outbox WHERE state = ? AND occurred_at < ? LIMIT ?",
		OutboxStateSent, before, maxRows)
	if err != nil {
		return 0, fmt.Errorf("recall_outbox DeleteSentBefore: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recall_outbox DeleteSentBefore RowsAffected: %w", err)
	}
	return deleted, nil
}

// isDuplicateErr 识别 MySQL 唯一索引冲突（错误号 1062 / Duplicate entry 文本）。
// 本仓库不引入驱动私有错误类型，沿用 notification 服务的字符串判定口径。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
