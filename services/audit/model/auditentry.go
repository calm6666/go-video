package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// entryColumns 是 audit_entry 的列清单，必须与
// deploy/migrations/audit/000001_create_audit_entry_and_chain_tables.sql 一致。
const entryColumns = "entry_id, event_id, schema_version, chain_key, seq, actor_type, actor_id," +
	" actor_name, action, action_domain, target_type, target_id, result, before_digest, after_digest," +
	" reason, source_app, ip_hash, device_hash, trace_id, request_id, caller_service, occurred_at," +
	" prev_hash, entry_hash, ctime, archived_at"

// AuditEntry 对应 audit_entry 表：一条不可抵赖的审计存证。
//
// 追加式约束：本类型的模型接口**没有** UPDATE/DELETE（唯一的例外是 MarkArchived，
// 它只写不参与哈希的 archived_at 列，见下）。要「修正」一条审计只能再写一条
// 补偿条目（action 形如 <对象>.revoke），绝不允许改行。
//
// 链字段（ChainKey/Seq/PrevHash/EntryHash）由服务端在事务内计算，
// 语义与算法见 hashchain.go 与 rpc/audit.proto 文件头。
type AuditEntry struct {
	// EntryID 自增主键；由 chain_key + seq 唯一决定，因此不参与哈希。
	EntryID int64 `db:"entry_id"`
	// EventID 全局幂等键（唯一索引）。重复提交返回原条目而不是新增行。
	EventID string `db:"event_id"`
	// SchemaVersion 条目契约版本，当前固定 1；参与哈希。
	SchemaVersion int32 `db:"schema_version"`
	// ChainKey 所属哈希链："<action_domain>/<UTC 日>"。
	ChainKey string `db:"chain_key"`
	// Seq 链内序号，从 1 连续递增；出现空洞即视为链被截断。
	Seq int64 `db:"seq"`
	// ActorType 发起者类型（1 管理员、2 用户、3 系统、4 未知）。
	ActorType int32 `db:"actor_type"`
	// ActorID 发起者 ID（admin_id / mid）；类型未知时为 0。
	ActorID int64 `db:"actor_id"`
	// ActorName 发起者展示名快照（不随源资料变化，因此可长期解释历史）。
	ActorName string `db:"actor_name"`
	// Action 动作标识：<对象>.<动作>。
	Action string `db:"action"`
	// ActionDomain 动作域，决定所属链与保留策略。
	ActionDomain string `db:"action_domain"`
	// TargetType 目标类型（跨服务只存类型标识，如 ops_config:release）。
	TargetType string `db:"target_type"`
	// TargetID 目标主键（字符串，兼容不同服务的主键形态）。
	TargetID string `db:"target_id"`
	// Result 结果：1 ok、2 denied、3 error。
	Result int32 `db:"result"`
	// BeforeDigest 变更前摘要（白名单格式，见 ValidDigest）。
	BeforeDigest string `db:"before_digest"`
	// AfterDigest 变更后摘要（白名单格式）。
	AfterDigest string `db:"after_digest"`
	// Reason 业务原因（脱敏自由文本，参与哈希）。
	Reason string `db:"reason"`
	// SourceApp 来源端（1 android、2 ios、3 harmony、4 desktop、5 后台 Web、6 内部 RPC、7 cron）。
	SourceApp int32 `db:"source_app"`
	// IPHash 来源 IP 的加盐短哈希；明文 IP 永不入库（列名即哈希，不存在原文列）。
	IPHash string `db:"ip_hash"`
	// DeviceHash 设备标识的加盐短哈希。
	DeviceHash string `db:"device_hash"`
	// TraceID 链路 ID。
	TraceID string `db:"trace_id"`
	// RequestID 调用方幂等键（同一次请求可能写多条条目，故不建唯一索引）。
	RequestID string `db:"request_id"`
	// CallerService 写入方服务名：回答「谁声称做了这件事」，与 ActorID 互补。
	CallerService string `db:"caller_service"`
	// OccurredAt 业务发生时间（Unix 秒）；决定所属链与查询排序。
	OccurredAt int64 `db:"occurred_at"`
	// PrevHash 同链上一条的 EntryHash；链头取 GenesisHash(chainKey)。
	PrevHash string `db:"prev_hash"`
	// EntryHash 本条目完整性摘要。
	EntryHash string `db:"entry_hash"`
	// Ctime 入库时间（Unix 秒），不参与哈希。
	Ctime int64 `db:"ctime"`
	// ArchivedAt 归档完成时间，0 表示仍在热表；不参与哈希。
	ArchivedAt int64 `db:"archived_at"`
}

// EntryFilter 是审计条目查询条件；零值表示不过滤。
// StartAt/EndAt 由 logic 层强制必填（大表禁止无界扫描），model 只负责拼条件。
type EntryFilter struct {
	StartAt      int64
	EndAt        int64
	ActorType    int32
	ActorID      int64
	Action       string
	ActionDomain string
	TargetType   string
	TargetID     string
	TraceID      string
	Result       int32
	SourceApp    int32
	// Archived 三态：0 不过滤、1 只看已归档、2 只看热表（归档作业用）。
	Archived int32
	Pn       int32
	Ps       int32
}

// AuditEntryModel 抽象 audit_entry 表。
// 注意接口上没有任何「修改业务字段」或「删除」的方法：这是追加式审计的
// 代码级体现，评审时按接口签名即可判定服务有没有越界。
type AuditEntryModel interface {
	// Insert 在给定事务内追加一条条目。调用方必须已经填好 ChainKey/Seq/PrevHash/EntryHash，
	// 且与同事务内锁定的链头严格一致；(chain_key, seq) 冲突会返回驱动错误，
	// repository 捕获后转成 ErrChainConflict 重试。
	// session 允许为 nil（此时自建于 conn 的单语句执行）。
	Insert(ctx context.Context, session sqlx.Session, e *AuditEntry) (int64, error)
	// FindOne 按主键查询。
	FindOne(ctx context.Context, entryID int64) (*AuditEntry, error)
	// FindByEventID 按幂等键查询；不存在返回 (nil, nil)，让 repository 决定语义。
	FindByEventID(ctx context.Context, eventID string) (*AuditEntry, error)
	// FindByEventIDs 批量按幂等键查询（批量写入前先查已有条目，避免逐条撞唯一键）。
	FindByEventIDs(ctx context.Context, eventIDs []string) (map[string]*AuditEntry, error)
	// List 按条件分页查询，固定 occurred_at DESC, entry_id DESC。
	List(ctx context.Context, f EntryFilter) ([]*AuditEntry, int64, error)
	// ListByChainRange 按链 + seq 升序取区间，供 VerifyAuditChain 重放与归档扫描使用。
	// limit <= 0 时不限制（调用方需自觉传上限，校验路径必须限制内存占用）。
	ListByChainRange(ctx context.Context, chainKey string, fromSeq, toSeq int64, limit int32) ([]*AuditEntry, error)
	// MarkArchived 归档成功后把区间内条目 archived_at 置为 ts（仅当原值为 0）。
	// 这是本表唯一的写入型变更，且只碰不参与哈希的列，因此不破坏完整性自证。
	MarkArchived(ctx context.Context, chainKey string, fromSeq, toSeq int64, ts int64) (int64, error)
	// MaxSeq 返回某链当前最大序号（0 表示空链）；用于校验尾号与断链快速判定。
	MaxSeq(ctx context.Context, chainKey string) (int64, error)
	// ScanAfter 按 entry_id 升序做游标扫描（导出任务专用，第二轮新增）。
	// 为什么不能用 List：List 的排序键是 occurred_at DESC（在线查询要最新优先），
	// 而导出是「把一段区间整搬走」，必须有一个单调、可续传、与插入顺序一致的游标。
	// afterEntryID 传上一次已读到的最大 entry_id（0 表示从头）；返回行数 < limit 即扫完。
	// 时间范围必填：沿用 EntryFilter.build 的约束，导出同样禁止无界扫描。
	ScanAfter(ctx context.Context, f EntryFilter, afterEntryID int64, limit int32) ([]*AuditEntry, error)
}

type defaultAuditEntryModel struct {
	conn sqlx.SqlConn
}

// NewAuditEntryModel 构造 audit_entry 的 sqlx 实现。
func NewAuditEntryModel(conn sqlx.SqlConn) AuditEntryModel {
	return &defaultAuditEntryModel{conn: conn}
}

// execSession 在 session 与 conn 之间选择执行器：两者都满足 sqlx.Session。
func (m *defaultAuditEntryModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

func (m *defaultAuditEntryModel) Insert(ctx context.Context, session sqlx.Session, e *AuditEntry) (int64, error) {
	if e.EventID == "" {
		return 0, ErrEventIDRequired
	}
	if !ValidDigest(e.BeforeDigest) || !ValidDigest(e.AfterDigest) {
		return 0, ErrDigestInvalid
	}
	if e.Ctime == 0 {
		e.Ctime = nowUnix()
	}
	s := m.exec(session)
	res, err := s.ExecCtx(ctx,
		"INSERT INTO audit_entry ("+entryColumns+") VALUES ("+placeholders(27)+")",
		e.EntryID, e.EventID, e.SchemaVersion, e.ChainKey, e.Seq, e.ActorType, e.ActorID,
		e.ActorName, e.Action, e.ActionDomain, e.TargetType, e.TargetID, e.Result,
		e.BeforeDigest, e.AfterDigest, e.Reason, e.SourceApp, e.IPHash, e.DeviceHash,
		e.TraceID, e.RequestID, e.CallerService, e.OccurredAt, e.PrevHash, e.EntryHash,
		e.Ctime, e.ArchivedAt)
	if err != nil {
		return 0, fmt.Errorf("audit_entry Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("audit_entry Insert LastInsertId: %w", err)
	}
	e.EntryID = id
	return id, nil
}

const entrySelect = "SELECT " + entryColumns + " FROM audit_entry"

func (m *defaultAuditEntryModel) FindOne(ctx context.Context, entryID int64) (*AuditEntry, error) {
	var row AuditEntry
	err := m.conn.QueryRowCtx(ctx, &row, entrySelect+" WHERE entry_id = ? LIMIT 1", entryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrEntryNotFound
		}
		return nil, fmt.Errorf("audit_entry FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultAuditEntryModel) FindByEventID(ctx context.Context, eventID string) (*AuditEntry, error) {
	if eventID == "" {
		return nil, ErrEventIDRequired
	}
	var row AuditEntry
	err := m.conn.QueryRowCtx(ctx, &row, entrySelect+" WHERE event_id = ? LIMIT 1", eventID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit_entry FindByEventID: %w", err)
	}
	return &row, nil
}

func (m *defaultAuditEntryModel) FindByEventIDs(ctx context.Context, eventIDs []string) (map[string]*AuditEntry, error) {
	out := make(map[string]*AuditEntry, len(eventIDs))
	if len(eventIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(eventIDs))
	for _, id := range eventIDs {
		args = append(args, id)
	}
	var rows []*AuditEntry
	query := entrySelect + " WHERE event_id IN (" + placeholders(len(eventIDs)) + ")"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("audit_entry FindByEventIDs: %w", err)
	}
	for _, r := range rows {
		out[r.EventID] = r
	}
	return out, nil
}

func (m *defaultAuditEntryModel) List(ctx context.Context, f EntryFilter) ([]*AuditEntry, int64, error) {
	where, args := f.build()
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM audit_entry "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("audit_entry List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	// 排序键固定，不接受调用方指定：audit_entry 是千万级大表，
	// 任意排序都会退化成 filesort + 临时表。
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*AuditEntry
	query := entrySelect + where + " ORDER BY occurred_at DESC, entry_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("audit_entry List: %w", err)
	}
	return rows, total, nil
}

// build 把过滤条件拼成 WHERE 子句与参数。
// 条件顺序刻意与索引前缀对齐（见迁移文件的索引清单），
// 让优化器在「时间范围 + 单一维度」这种最常见组合上走区间索引。
func (f EntryFilter) build() (string, []any) {
	where := "WHERE occurred_at >= ? AND occurred_at < ?"
	args := []any{f.StartAt, f.EndAt}
	if f.ActorType > 0 {
		where += " AND actor_type = ?"
		args = append(args, f.ActorType)
	}
	if f.ActorID > 0 {
		where += " AND actor_id = ?"
		args = append(args, f.ActorID)
	}
	if f.Action != "" {
		where += " AND action = ?"
		args = append(args, f.Action)
	}
	if f.ActionDomain != "" {
		where += " AND action_domain = ?"
		args = append(args, f.ActionDomain)
	}
	if f.TargetType != "" {
		where += " AND target_type = ?"
		args = append(args, f.TargetType)
	}
	if f.TargetID != "" {
		where += " AND target_id = ?"
		args = append(args, f.TargetID)
	}
	if f.TraceID != "" {
		where += " AND trace_id = ?"
		args = append(args, f.TraceID)
	}
	if f.Result > 0 {
		where += " AND result = ?"
		args = append(args, f.Result)
	}
	if f.SourceApp > 0 {
		where += " AND source_app = ?"
		args = append(args, f.SourceApp)
	}
	switch f.Archived {
	case 1:
		where += " AND archived_at > 0"
	case 2:
		where += " AND archived_at = 0"
	}
	return where, args
}

func (m *defaultAuditEntryModel) ListByChainRange(ctx context.Context, chainKey string, fromSeq, toSeq int64, limit int32) ([]*AuditEntry, error) {
	if chainKey == "" {
		return nil, ErrChainKeyRequired
	}
	where := "WHERE chain_key = ?"
	args := []any{chainKey}
	if fromSeq > 0 {
		where += " AND seq >= ?"
		args = append(args, fromSeq)
	}
	if toSeq > 0 {
		where += " AND seq <= ?"
		args = append(args, toSeq)
	}
	var rows []*AuditEntry
	query := entrySelect + where + " ORDER BY seq ASC"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit_entry ListByChainRange: %w", err)
	}
	return rows, nil
}

func (m *defaultAuditEntryModel) MarkArchived(ctx context.Context, chainKey string, fromSeq, toSeq int64, ts int64) (int64, error) {
	if chainKey == "" {
		return 0, ErrChainKeyRequired
	}
	if toSeq <= 0 {
		return 0, fmt.Errorf("audit_entry MarkArchived: %w", ErrInvalidPage)
	}
	if ts == 0 {
		ts = nowUnix()
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE audit_entry SET archived_at = ? WHERE chain_key = ? AND seq >= ? AND seq <= ? AND archived_at = 0",
		ts, chainKey, fromSeq, toSeq)
	if err != nil {
		return 0, fmt.Errorf("audit_entry MarkArchived: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("audit_entry MarkArchived RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultAuditEntryModel) MaxSeq(ctx context.Context, chainKey string) (int64, error) {
	if chainKey == "" {
		return 0, ErrChainKeyRequired
	}
	var seq sql.NullInt64
	err := m.conn.QueryRowCtx(ctx, &seq, "SELECT MAX(seq) FROM audit_entry WHERE chain_key = ?", chainKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("audit_entry MaxSeq: %w", err)
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

func (m *defaultAuditEntryModel) ScanAfter(ctx context.Context, f EntryFilter,
	afterEntryID int64, limit int32) ([]*AuditEntry, error) {
	if f.StartAt <= 0 || f.EndAt <= 0 || f.StartAt >= f.EndAt {
		return nil, ErrQueryRangeRequired
	}
	if afterEntryID < 0 {
		return nil, ErrInvalidPage
	}
	if limit <= 0 || limit > MaxScanBatchRows {
		limit = MaxScanBatchRows
	}
	where, args := f.build()
	where += " AND entry_id > ?"
	args = append(args, afterEntryID)
	var rows []*AuditEntry
	query := entrySelect + where + " ORDER BY entry_id ASC LIMIT ?"
	args = append(args, limit)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit_entry ScanAfter: %w", err)
	}
	return rows, nil
}
