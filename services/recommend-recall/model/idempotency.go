package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/common/idempotency"
)

// RecallIdempotency 写接口幂等记录（recall_idempotency 表）。
//
// 为什么必须有这张表：rpc 的 UpsertPoolItemsReply / PublishPoolVersionReply /
// RollbackPoolVersionReply 都带 `deduplicated` 字段（"本次是重放还是新执行"）。
// 没有落库的幂等记录就无法区分二者——只能靠调用方自觉，作业重试一次就会
// 把同一批候选当成两次执行、把一次切换当成两次切换（AGENTS.md §5 写接口幂等）。
//
// 定位：控制位，不是事实源。按 expire_at 清理后最坏的副作用是"重复请求再执行一次"，
// 而条目写入本身是 ON DUPLICATE KEY UPDATE、指针切换是 CAS，重复执行不产生第二份数据。
//
// 隐私：只落请求指纹（sha256 hex），不落请求原文；result_payload 只存回复投影，
// 不含用户敏感信息（本服务的写接口调用方是作业与运营，不是终端用户）。
type RecallIdempotency struct {
	ID             int64  `db:"id"`              // 自增主键
	Scope          string `db:"scope"`           // 幂等作用域，参见 IdempotencyScope*
	IdempotencyKey string `db:"idempotency_key"` // 调用方提供的幂等键（与 scope 组成唯一键）
	RequestHash    string `db:"request_hash"`    // sha256(请求关键字段)，同键不同参数即冲突
	State          string `db:"state"`           // pending/succeeded/failed（common/idempotency.State）
	ResultPayload  string `db:"result_payload"`  // 成功回复的 JSON 投影，重放时原样回给调用方
	EventID        string `db:"event_id"`        // 本次执行产生的 outbox event_id（切换类操作）
	Operator       string `db:"operator"`        // 执行者（作业/运营标识）
	LeaseExpireAt  int64  `db:"lease_expire_at"` // PENDING 租约到期时间（Unix 秒），过期后可被重新认领
	ExpireAt       int64  `db:"expire_at"`       // 记录保留期（Unix 秒），到点由清理作业删除
	ExecutionCount int32  `db:"execution_count"` // 同一键被执行（含认领尝试）的次数，观测重试风暴
	LastError      string `db:"last_error"`      // 最近一次失败原因（不含敏感信息）
	Ctime          int64  `db:"ctime"`           // 创建时间（Unix 秒）
	Mtime          int64  `db:"mtime"`           // 修改时间（Unix 秒）
}

// 幂等作用域：同一字符串空间里不同接口的键互不冲突，避免调用方跨接口复用键导致语义错乱。
const (
	// IdempotencyScopeUpsertPoolItems UpsertPoolItems 的写入批次幂等。
	IdempotencyScopeUpsertPoolItems = "upsert_pool_items"
	// IdempotencyScopePublishPoolVersion PublishPoolVersion 的版本切换幂等。
	IdempotencyScopePublishPoolVersion = "publish_pool_version"
	// IdempotencyScopeRollbackPoolVersion RollbackPoolVersion 的回滚幂等。
	IdempotencyScopeRollbackPoolVersion = "rollback_pool_version"
)

// ValidIdempotencyScope 判定作用域是否受控（新写接口必须在此登记，禁止自由字符串）。
func ValidIdempotencyScope(scope string) bool {
	switch scope {
	case IdempotencyScopeUpsertPoolItems, IdempotencyScopePublishPoolVersion, IdempotencyScopeRollbackPoolVersion:
		return true
	default:
		return false
	}
}

// IdempotencyClaim 是认领幂等键的结果。
type IdempotencyClaim struct {
	// Held 为 true 表示该键已被占用：调用方必须回放 Existing，不得再执行一次业务写。
	Held bool
	// Existing 是已存在的记录（Held=true 时非空）。
	Existing *RecallIdempotency
	// First 为 true 表示本次调用是首次认领成功（可以执行业务写）。
	First bool
}

// RecallIdempotencyModel recall_idempotency 表读写接口。
//
// 推荐时序（写接口三步，第二轮 logic 按此实现）：
//  1. Claim 以自动提交方式认领（session=nil）：认领成功后即使进程崩溃，PENDING 行仍在，
//     由 lease_expire_at 到期后允许被重新认领；
//  2. 业务事务内 MarkSucceeded（传 session）：幂等标记与数据变更同提交，
//     不会出现"标记成功但数据没落"或反之；
//  3. 业务事务失败后 MarkFailed（自动提交）：把键放回可重试状态。
type RecallIdempotencyModel interface {
	// Claim 认领幂等键：靠 uniq_scope_key 的 INSERT 影响行数判定"这次是谁插进去的"。
	// 返回 First=true 的调用方才允许执行业务写；其余情况返回 Held=true + 已有记录（回放用）。
	// 同键不同 request_hash 返回 ErrIdempotencyFingerprintMismatch。
	Claim(ctx context.Context, session sqlx.Session, in *RecallIdempotency, now int64) (*IdempotencyClaim, error)
	// MarkSucceeded 写入执行结果并置 SUCCEEDED（与业务写同事务提交，避免"标记成功但数据没落"）。
	MarkSucceeded(ctx context.Context, session sqlx.Session, scope, key, resultPayload, eventID string) (bool, error)
	// MarkFailed 置 FAILED 并清空租约：允许调用方用同一键立即重试（业务写已回滚，没有半成功状态）。
	MarkFailed(ctx context.Context, scope, key, lastError string) (bool, error)
	// Find 读取单条记录；不存在返回 ErrNotFound。
	Find(ctx context.Context, scope, key string) (*RecallIdempotency, error)
	// ListExpired 列出 expire_at 早于 before 的记录 id（分批，limit 限幅）。
	ListExpired(ctx context.Context, before int64, limit int) ([]int64, error)
	// DeleteExpired 删除 expire_at 早于 before 的记录，maxRows 限制单次行数。
	DeleteExpired(ctx context.Context, before, maxRows int64) (int64, error)
}

// ErrNotFound 幂等记录不存在（与业务侧的"未命中"区分：本错误只用于 Claim/Mark 的前置读）。
var ErrNotFound = errors.New("recommend-recall: idempotency record not found")

type defaultRecallIdempotencyModel struct {
	conn sqlx.SqlConn
}

// NewRecallIdempotencyModel 创建 RecallIdempotencyModel 实现。
func NewRecallIdempotencyModel(conn sqlx.SqlConn) RecallIdempotencyModel {
	return &defaultRecallIdempotencyModel{conn: conn}
}

// exec 在事务 session 与全局连接之间选择执行器（与 audit 模型同一约定）。
func (m *defaultRecallIdempotencyModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

const idempotencySelect = "SELECT id, scope, idempotency_key, request_hash, state, result_payload, event_id, " +
	"operator, lease_expire_at, expire_at, execution_count, last_error, ctime, mtime FROM recall_idempotency"

func (m *defaultRecallIdempotencyModel) Claim(ctx context.Context, session sqlx.Session,
	in *RecallIdempotency, now int64) (*IdempotencyClaim, error) {
	if err := checkIdempotencyArgs(in); err != nil {
		return nil, err
	}
	if !ValidIdempotencyState(in.State) {
		return nil, ErrIdempotencyStateInvalid
	}
	if in.Ctime == 0 {
		in.Ctime = now
	}
	// ON DUPLICATE KEY UPDATE id = id 是显式 no-op：MySQL 对"值未改变"的重复键更新返回
	// RowsAffected=0，对新插入返回 1。用这个差值判定"这次是谁把行插进来的"，
	// 比读回 execution_count 可靠——并发第二个调用方读回时会看到第一条自己写的 PENDING 行，
	// 从而误判为自己认领成功。
	res, err := m.exec(session).ExecCtx(ctx,
		"INSERT INTO recall_idempotency (scope, idempotency_key, request_hash, state, result_payload, event_id, "+
			"operator, lease_expire_at, expire_at, execution_count, last_error, ctime, mtime) "+
			"VALUES (?, ?, ?, ?, '', '', ?, ?, ?, 1, '', ?, ?) "+
			"ON DUPLICATE KEY UPDATE id = id",
		in.Scope, in.IdempotencyKey, in.RequestHash, in.State, in.Operator,
		in.LeaseExpireAt, in.ExpireAt, in.Ctime, in.Ctime)
	if err != nil {
		return nil, fmt.Errorf("recall_idempotency Claim: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("recall_idempotency Claim RowsAffected: %w", err)
	}
	if affected == 1 {
		// 首次认领成功：本行由本次调用插入，状态即写入时的 PENDING + 租约。
		return &IdempotencyClaim{First: true, Existing: in}, nil
	}
	exist, err := m.find(ctx, session, in.Scope, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if exist.RequestHash != in.RequestHash {
		// 同键不同参数：调用方复用了键却改了请求体，硬失败而不是执行其中任一份语义。
		return nil, ErrIdempotencyFingerprintMismatch
	}
	if exist.State == string(idempotency.StateSucceeded) {
		return &IdempotencyClaim{Held: true, Existing: exist}, nil
	}
	if exist.State == string(idempotency.StatePending) && exist.LeaseExpireAt > now {
		// 另一个执行者还在租约内：调用方等待或回放，不得启动第二次执行。
		return &IdempotencyClaim{Held: true, Existing: exist}, nil
	}
	// PENDING 租约过期 或 FAILED：允许重新认领。条件 UPDATE（带 state 与租约上界）
	// 保证并发下只有一个赢家拿到 First=true。
	upd, err := m.exec(session).ExecCtx(ctx,
		"UPDATE recall_idempotency SET state = ?, lease_expire_at = ?, request_hash = ?, operator = ?, "+
			"execution_count = execution_count + 1, last_error = '', mtime = ? "+
			"WHERE scope = ? AND idempotency_key = ? AND state IN (?, ?) AND lease_expire_at <= ?",
		string(idempotency.StatePending), in.LeaseExpireAt, in.RequestHash, in.Operator, now,
		in.Scope, in.IdempotencyKey, string(idempotency.StatePending), string(idempotency.StateFailed), now)
	if err != nil {
		return nil, fmt.Errorf("recall_idempotency Claim re-claim: %w", err)
	}
	reclaimed, err := rowChanged(upd, "recall_idempotency Claim re-claim")
	if err != nil {
		return nil, err
	}
	if !reclaimed {
		return &IdempotencyClaim{Held: true, Existing: exist}, nil
	}
	return &IdempotencyClaim{First: true, Existing: exist}, nil
}

func (m *defaultRecallIdempotencyModel) MarkSucceeded(ctx context.Context, session sqlx.Session,
	scope, key, resultPayload, eventID string) (bool, error) {
	if !ValidIdempotencyScope(scope) {
		return false, ErrIdempotencyStateInvalid
	}
	if strings.TrimSpace(key) == "" {
		return false, ErrIdempotencyKeyRequired
	}
	if len(resultPayload) > MaxIdempotencyPayloadLen {
		return false, ErrTooManyItems
	}
	now := nowUnix()
	res, err := m.exec(session).ExecCtx(ctx,
		"UPDATE recall_idempotency SET state = ?, result_payload = ?, event_id = ?, lease_expire_at = 0, "+
			"last_error = '', mtime = ? WHERE scope = ? AND idempotency_key = ?",
		string(idempotency.StateSucceeded), resultPayload, eventID, now, scope, key)
	if err != nil {
		return false, fmt.Errorf("recall_idempotency MarkSucceeded: %w", err)
	}
	return rowChanged(res, "recall_idempotency MarkSucceeded")
}

func (m *defaultRecallIdempotencyModel) MarkFailed(ctx context.Context, scope, key, lastError string) (bool, error) {
	if !ValidIdempotencyScope(scope) {
		return false, ErrIdempotencyStateInvalid
	}
	if strings.TrimSpace(key) == "" {
		return false, ErrIdempotencyKeyRequired
	}
	if len(lastError) > 512 {
		lastError = lastError[:512]
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE recall_idempotency SET state = ?, lease_expire_at = 0, last_error = ?, mtime = ? "+
			"WHERE scope = ? AND idempotency_key = ? AND state <> ?",
		string(idempotency.StateFailed), lastError, now, scope, key, string(idempotency.StateSucceeded))
	if err != nil {
		return false, fmt.Errorf("recall_idempotency MarkFailed: %w", err)
	}
	// 已是 SUCCEEDED 的记录不能被失败覆盖（重放路径拿到的仍是成功结果）。
	return rowChanged(res, "recall_idempotency MarkFailed")
}

func (m *defaultRecallIdempotencyModel) Find(ctx context.Context, scope, key string) (*RecallIdempotency, error) {
	return m.find(ctx, nil, scope, key)
}

func (m *defaultRecallIdempotencyModel) find(ctx context.Context, session sqlx.Session, scope, key string) (*RecallIdempotency, error) {
	if !ValidIdempotencyScope(scope) {
		return nil, ErrIdempotencyStateInvalid
	}
	if strings.TrimSpace(key) == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	var row RecallIdempotency
	query := idempotencySelect + " WHERE scope = ? AND idempotency_key = ? LIMIT 1"
	if err := m.exec(session).QueryRowCtx(ctx, &row, query, scope, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("recall_idempotency find: %w", err)
	}
	return &row, nil
}

func (m *defaultRecallIdempotencyModel) ListExpired(ctx context.Context, before int64, limit int) ([]int64, error) {
	if before <= 0 {
		return nil, ErrInvalidLimit
	}
	if err := CheckLimit(limit, MaxOutboxBatch); err != nil {
		return nil, err
	}
	query := "SELECT id FROM recall_idempotency WHERE expire_at > 0 AND expire_at < ? ORDER BY id ASC LIMIT ?"
	var ids []int64
	if err := m.conn.QueryRowsCtx(ctx, &ids, query, before, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("recall_idempotency ListExpired: %w", err)
	}
	return ids, nil
}

func (m *defaultRecallIdempotencyModel) DeleteExpired(ctx context.Context, before, maxRows int64) (int64, error) {
	if before <= 0 {
		return 0, ErrInvalidLimit
	}
	if err := CheckInt64Limit(maxRows, MaxDeleteRows); err != nil {
		return 0, err
	}
	res, err := m.conn.ExecCtx(ctx, "DELETE FROM recall_idempotency WHERE expire_at > 0 AND expire_at < ? LIMIT ?",
		before, maxRows)
	if err != nil {
		return 0, fmt.Errorf("recall_idempotency DeleteExpired: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recall_idempotency DeleteExpired RowsAffected: %w", err)
	}
	return deleted, nil
}

// checkIdempotencyArgs 是幂等写入的公共参数校验。
func checkIdempotencyArgs(in *RecallIdempotency) error {
	if !ValidIdempotencyScope(in.Scope) {
		return ErrIdempotencyStateInvalid
	}
	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		return ErrIdempotencyKeyRequired
	}
	if len(key) > MaxIdempotencyKeyLen {
		return ErrIdempotencyKeyTooLong
	}
	if len(in.RequestHash) != RequestHashLen {
		return ErrIdempotencyFingerprintMismatch
	}
	if strings.TrimSpace(in.Operator) == "" {
		return ErrOperatorRequired
	}
	return nil
}

// ValidIdempotencyState 判定幂等状态是否受控（复用 common/idempotency 的状态词表）。
func ValidIdempotencyState(state string) bool {
	_, err := idempotency.ParseState(state)
	return err == nil
}

// rowChanged 统一处理 RowsAffected 判定：MySQL 在"值完全相同"时返回 0，
// 因此调用方拿到 false 后必须重读确认状态，不能直接判定为失败。
func rowChanged(res sql.Result, op string) (bool, error) {
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return affected > 0, nil
}
