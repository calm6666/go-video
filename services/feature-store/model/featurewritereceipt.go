package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ReceiptDigestVersion 是「请求内容摘要」的算法版本，与特征定义摘要（DigestVersion）分开编号：
// 两者序列化的字段完全不同，共用一个版本号会在改其中一个时误伤另一个的判定。
const ReceiptDigestVersion = "fs-req-v1"

// featureWriteReceiptColumns 是 feature_write_receipt 的列清单（含自增主键）。
const featureWriteReceiptColumns = "receipt_id, request_id, op_type, feature_key, version," +
	" entity_scope, entity_id, row_count, request_digest, digest_ver, state, result_json," +
	" result_digest, detail_kept, affected_rows, error_code, operator, trace_id," +
	" lease_owner, lease_expire_at, ctime, mtime, finished_at"

// featureWriteReceiptInsertColumns 是写入列（不含自增主键，共 22 列）。
const featureWriteReceiptInsertColumns = "request_id, op_type, feature_key, version, entity_scope," +
	" entity_id, row_count, request_digest, digest_ver, state, result_json, result_digest," +
	" detail_kept, affected_rows, error_code, operator, trace_id, lease_owner, lease_expire_at," +
	" ctime, mtime, finished_at"

// featureWriteReceiptInsertArgs 是写入参数个数（与上面的列清单严格同序）。
const featureWriteReceiptInsertArgs = 22

// 回执覆盖的操作类型。契约里 8 个写方法共用这一张表：
// 「同一 request_id 在不同接口间串味」是最难查的一类幂等事故（注册请求的回放被
// 当成切换请求的回放），所以 op_type 参与唯一键而不是只存在日志里。
const (
	// ReceiptOpRegister RegisterFeature。
	ReceiptOpRegister = "register"
	// ReceiptOpStateChange UpdateFeatureState。
	ReceiptOpStateChange = "state_change"
	// ReceiptOpPrivacyChange UpdateFeaturePrivacy。
	ReceiptOpPrivacyChange = "privacy_change"
	// ReceiptOpWrite WriteFeatures。
	ReceiptOpWrite = "write"
	// ReceiptOpSwitch SwitchFeatureVersion。
	ReceiptOpSwitch = "switch"
	// ReceiptOpBackfill SubmitBackfillJob。
	ReceiptOpBackfill = "backfill"
	// ReceiptOpPurge PurgeExpired。
	ReceiptOpPurge = "purge"
	// ReceiptOpErase EraseEntityFeatures。
	ReceiptOpErase = "erase"
)

// ValidReceiptOp 判断回执操作类型是否已声明。
func ValidReceiptOp(op string) bool {
	switch op {
	case ReceiptOpRegister, ReceiptOpStateChange, ReceiptOpPrivacyChange, ReceiptOpWrite,
		ReceiptOpSwitch, ReceiptOpBackfill, ReceiptOpPurge, ReceiptOpErase:
		return true
	default:
		return false
	}
}

// 回执状态机：in_progress → done / failed。
// 用字符串而不是枚举数字：回执不进对外契约，运维直接读这一列时 "in_progress" 比 "2" 有用。
const (
	// ReceiptStateInProgress 请求已取得执行权但尚未收尾。持有者由 lease_owner/lease_expire_at
	// 标定：进程崩溃后租约到期，同一个 request_id 可以被重新取得，不会永久卡死。
	ReceiptStateInProgress = "in_progress"
	// ReceiptStateDone 操作已完成，result_json 是可回放的响应快照。
	ReceiptStateDone = "done"
	// ReceiptStateFailed 操作明确失败（未产生副作用）。允许同 request_id 重试：
	// 幂等键的意义就是「同一件事只发生一次」，而不是「一次失败就作废」。
	ReceiptStateFailed = "failed"
)

// ValidReceiptState 判断回执状态是否已声明。
func ValidReceiptState(s string) bool {
	return s == ReceiptStateInProgress || s == ReceiptStateDone || s == ReceiptStateFailed
}

// IsReceiptTerminal 判断回执是否终态（终态才能被回放）。
func IsReceiptTerminal(s string) bool {
	return s == ReceiptStateDone || s == ReceiptStateFailed
}

// ValidReceiptTransition 校验回执状态迁移：in_progress → done|failed，终态不再出边。
// 与作业状态矩阵分开写，是因为这里的「失败后重试」由 Begin 重新把 failed 置回
// in_progress 表达，不算这条矩阵的边（否则「失败可重试」会污染终态定义）。
func ValidReceiptTransition(from, to string) bool {
	if from != ReceiptStateInProgress {
		return false
	}
	return to == ReceiptStateDone || to == ReceiptStateFailed
}

// 回执承载的上限。
const (
	// MaxReceiptResultBytes 是 result_json 的字节上限。
	// 回执不是响应缓存：它只存「重放这次请求所需的最小事实」（计数、ID、版本、行级短码）。
	// 上限存在的理由是 WriteFeatures 的逐行结果最坏能到几十 KB，
	// 无界地塞进回执会让幂等回放的读放大比首次请求还大。
	MaxReceiptResultBytes = 2048
	// MaxReceiptErrorCodeLen 是 error_code 列宽（与 DDL 一致）。
	MaxReceiptErrorCodeLen = 64
	// MaxReceiptLeaseSeconds 是执行权租约的上限：超过这个值说明调用方把回执当成了
	// 「长期占位」，正常请求（含 500 行批量写）都在秒级完成。
	MaxReceiptLeaseSeconds = 300
)

// defaultReceiptLeaseSeconds 是配置缺失时的兜底租约。
const defaultReceiptLeaseSeconds int64 = 60

// ReceiptSnapshot 是收尾时写入的响应快照。
type ReceiptSnapshot struct {
	// ResultJSON 可回应的最小事实（计数/ID/版本/行级短码），不超过 MaxReceiptResultBytes。
	ResultJSON string
	// ResultDigest 完整首次响应的摘要（sha256 of 序列化后的 reply）。
	// 当 DetailKept=false 时逐行内容不入库，但摘要仍在：回放时能如实告诉调用方
	// 「首次请求的结果就是这份，只是本表没存逐行明细」，而不是伪造一份明细。
	ResultDigest string
	// AffectedRows 本次操作影响的行数（written / purged / erased / switched 的统一起点）。
	AffectedRows int64
	// DetailKept true = result_json 已包含回放所需的全部明细；
	// false = 只存了汇总计数，回放必须返回「计数 + reused=true」并明确告知明细不可重放。
	DetailKept bool
}

// WriteReceipt 对应 feature_write_receipt 表：写请求的执行权与幂等回执。
//
// 为什么需要这张表而不是「靠各业务表的唯一键天然幂等」：
//   - SwitchFeatureVersion 的副作用是一行指针更新 + 一条审计，两者都没有能承接
//     「这个 request_id」的列，重放时无法判断该不该再切一次；
//   - WriteFeatures 是整批语义，逐行唯一键只能防单行重复，防不了「整批重放一次」
//     造成的 written 计数翻倍；
//   - 更关键的是并发：两条同 request_id 的请求同时通过业务表检查，
//     会各自留一条互相矛盾的审计。执行权必须先落到一行上，再干活。
type WriteReceipt struct {
	// ReceiptID 自增主键。
	ReceiptID int64 `db:"receipt_id"`
	// RequestID 调用方给的幂等键（与 op_type 组成唯一键）。
	RequestID string `db:"request_id"`
	// OpType 操作类型，见 ReceiptOp* 常量。
	OpType string `db:"op_type"`
	// FeatureKey 目标特征键（purge/erase 这类无 key 的操作留空串）。
	FeatureKey string `db:"feature_key"`
	// Version 目标版本，0 = 不针对具体版本。
	Version int32 `db:"version"`
	// EntityScope 主体类型，0 = 不针对具体主体。
	EntityScope int32 `db:"entity_scope"`
	// EntityID 主体标识（受控形态，明文标识不允许出现在这里）。
	EntityID string `db:"entity_id"`
	// RowCount 请求体量（WriteFeatures 的行数、EraseEntityFeatures 的目标行数），
	// 用于事后解释「这个 request_id 当初有多大一只鸟」。
	RowCount int32 `db:"row_count"`
	// RequestDigest 请求内容摘要（见 RequestDigestOf）：同一 request_id 换了内容即冲突。
	RequestDigest string `db:"request_digest"`
	// DigestVer 摘要算法版本（见 ReceiptDigestVersion）。
	DigestVer string `db:"digest_ver"`
	// State 见 ReceiptState*。
	State string `db:"state"`
	// ResultJSON 首次成功响应的最小快照（见 ReceiptSnapshot.ResultJSON）。
	ResultJSON string `db:"result_json"`
	// ResultDigest 首次响应的完整摘要（明细未入库时仍能证明回放的是同一份结果）。
	ResultDigest string `db:"result_digest"`
	// DetailKept 1 = ResultJSON 足以完整回放；0 = 只存了汇总计数。
	DetailKept int32 `db:"detail_kept"`
	// AffectedRows 实际影响行数（与 Reply 里的 written/purged/erased 对齐）。
	AffectedRows int64 `db:"affected_rows"`
	// ErrorCode 失败时记录的哨兵短码（不含 SQL 与特征值原文，见 AGENTS.md §6）。
	ErrorCode string `db:"error_code"`
	// Operator 调用方身份。
	Operator string `db:"operator"`
	// TraceID 链路 ID。
	TraceID string `db:"trace_id"`
	// LeaseOwner 执行权持有者（<pod 名>#<worker id>）。
	LeaseOwner string `db:"lease_owner"`
	// LeaseExpireAt 执行权租约到期时间（Unix 秒）：崩溃的请求不会永久占用 request_id。
	LeaseExpireAt int64 `db:"lease_expire_at"`
	// Ctime 首次受理时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后更新时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
	// FinishedAt 进入 done/failed 的时间（in_progress 为 0）。
	FinishedAt int64 `db:"finished_at"`
}

// Replayable 判断回执能否直接回答重放请求。
//
// DetailKept=false 时只有计数可回放：logic 必须带着「明细不可重放」的说明返回，
// 而不是现攒一份逐行结果 —— 后者就是「伪造成功」，是本轮明令禁止的。
func (r *WriteReceipt) Replayable() bool {
	return r != nil && r.State == ReceiptStateDone && r.DetailKept == 1
}

// RequestDigestOf 计算请求内容摘要。参与摘要的只有决定副作用的字段：
// trace_id 这类链路元数据不进摘要，否则同一次请求换个 trace_id 重放就会变成「不同请求」。
func (r *WriteReceipt) RequestDigestOf() string {
	var b strings.Builder
	write := func(v string) {
		b.WriteString(v)
		b.WriteString(digestSeparator)
	}
	write(r.OpType)
	write(r.FeatureKey)
	write(strconv.FormatInt(int64(r.Version), 10))
	write(strconv.FormatInt(int64(r.EntityScope), 10))
	write(strings.TrimSpace(r.EntityID))
	write(strconv.FormatInt(int64(r.RowCount), 10))
	write(r.Operator)
	return digestOf(b.String())
}

// ValidateWriteReceipt 校验一条回执登记请求（不含唯一性，唯一性由 Begin 处理）。
func ValidateWriteReceipt(r *WriteReceipt) error {
	if r == nil {
		return ErrReceiptOpRequired
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return ErrRequestIdRequired
	}
	if len(r.RequestID) > maxReceiptRequestIDLen {
		return fmt.Errorf("%w: request_id longer than %d", ErrRequestIdRequired, maxReceiptRequestIDLen)
	}
	if strings.TrimSpace(r.OpType) == "" {
		return ErrReceiptOpRequired
	}
	if !ValidReceiptOp(r.OpType) {
		return fmt.Errorf("%w: op_type %q", ErrReceiptOpRequired, r.OpType)
	}
	if r.Version < 0 || r.EntityScope < 0 {
		return ErrFeatureVersionRequired
	}
	if r.EntityScope > 0 && !ValidEntityScope(r.EntityScope) {
		return ErrEntityScopeRequired
	}
	if r.EntityID != "" && !ValidEntityID(r.EntityScope, r.EntityID) {
		return ErrEntityIDInvalid
	}
	if r.RowCount < 0 {
		return ErrTooManyRows
	}
	if strings.TrimSpace(r.Operator) == "" {
		return ErrOperatorRequired
	}
	return nil
}

const maxReceiptRequestIDLen = 64

// ReceiptBeginResult 是 Begin 的返回值：把「该干活」与「该回放」区分成一个显式枚举，
// 而不是让 logic 去猜 created=false 时到底该返回什么。
type ReceiptBeginResult struct {
	// Receipt 当前持有执行权（或已可回放）的那一行。
	Receipt *WriteReceipt
	// Execute true = 本次调用取得执行权，必须执行操作并收尾（MarkDone/MarkFailed）。
	// false = 首次请求已完成，按 Receipt 回放；此时绝不能重复执行副作用。
	Execute bool
}

// WriteReceiptModel 抽象 feature_write_receipt 表。
//
// 使用协议（logic 侧固定三步，缺一步就是错的）：
//
//	Begin → 执行操作 → MarkDone / MarkFailed
//
// Begin 已经把执行权落到行上，因此「忘了 MarkFailed」的后果被租约兜住：
// 到期后同一 request_id 可被重新取得，而不是永久卡死。
type WriteReceiptModel interface {
	// Begin 取得 request_id+op_type 的执行权。
	//   - 新请求：插入 in_progress 行，Execute=true；
	//   - 已完成请求：返回首次快照，Execute=false（回放）；
	//   - 同 request_id 不同内容：ErrRequestIdReused（幂等回放会给出错误结果）；
	//   - 首次请求仍在执行中且租约未过期：ErrReceiptInProgress（调用方退避重试，
	//     绝不允许「顺手再执行一遍」）；
	//   - 首次请求失败（failed）或执行中但租约已过期：把该行重置为 in_progress 交给本次，
	//     Execute=true。
	Begin(ctx context.Context, r *WriteReceipt, leaseSeconds int64) (ReceiptBeginResult, error)
	// MarkDone 收尾为 done 并写入响应快照；只有持有租约的 leaseOwner 能改写。
	// 快照超过 MaxReceiptResultBytes 时返回 ErrReceiptResultTooLarge（不落库、不改状态），
	// 由调用方改提一份汇总快照 + DetailKept=false，而不是服务端悄悄截断。
	// 返回 false = 执行权已不在自己手上（超时被接管），此时副作用可能已经发生，
	// 调用方必须报「结果未知、请换新 request_id」而不是谎报成功或假装没做过。
	MarkDone(ctx context.Context, requestID, opType, leaseOwner string,
		snap ReceiptSnapshot) (bool, error)
	// MarkFailed 收尾为 failed 并记录哨兵短码；只有持有租约的 leaseOwner 能改写。
	MarkFailed(ctx context.Context, requestID, opType, leaseOwner, errorCode string) (bool, error)
	// Find 按 (request_id, op_type) 查询；不存在返回 (nil, nil)。
	Find(ctx context.Context, requestID, opType string) (*WriteReceipt, error)
	// CountStuck 统计 in_progress 且租约已过期超过 before 的行数（cron 巡检报警用）。
	// 这批行是「拿了执行权却没收尾」的证据：数量上升说明有请求在收尾前崩溃。
	CountStuck(ctx context.Context, before int64) (int64, error)
	// SelectAgedIDs 取一批「已进入终态且 mtime < before」的主键（保留期清理，走 idx_state_mtime）。
	// 先选主键再按主键删，不做范围 DELETE：范围锁会堵住同 request_id 前缀上的在线写。
	SelectAgedIDs(ctx context.Context, before int64, limit int32) ([]int64, error)
	// DeleteByIDs 按主键批量删除，返回删除行数。
	DeleteByIDs(ctx context.Context, ids []int64) (int64, error)
}

type defaultWriteReceiptModel struct {
	conn sqlx.SqlConn
}

// NewWriteReceiptModel 构造 feature_write_receipt 的 sqlx 实现。
func NewWriteReceiptModel(conn sqlx.SqlConn) WriteReceiptModel {
	return &defaultWriteReceiptModel{conn: conn}
}

const writeReceiptSelect = "SELECT " + featureWriteReceiptColumns + " FROM feature_write_receipt"

func (m *defaultWriteReceiptModel) Begin(ctx context.Context, r *WriteReceipt,
	leaseSeconds int64) (ReceiptBeginResult, error) {
	if err := ValidateWriteReceipt(r); err != nil {
		return ReceiptBeginResult{}, err
	}
	if strings.TrimSpace(r.LeaseOwner) == "" {
		// 执行权必须有主：没有 lease_owner 的回执等于「谁都能收尾」，
		// 崩溃的请求也无法被判定为过期，request_id 会被永久占用。
		return ReceiptBeginResult{}, ErrOperatorRequired
	}
	if leaseSeconds <= 0 {
		leaseSeconds = defaultReceiptLeaseSeconds
	}
	if leaseSeconds > MaxReceiptLeaseSeconds {
		leaseSeconds = MaxReceiptLeaseSeconds
	}
	now := nowUnix()
	r.RequestDigest = r.RequestDigestOf()
	r.DigestVer = ReceiptDigestVersion
	r.State = ReceiptStateInProgress
	r.LeaseExpireAt = now + leaseSeconds
	r.Ctime = now
	r.Mtime = now
	r.FinishedAt = 0

	// 依赖 uniq_request_op：冲突时 mtime 自等 → RowsAffected == 0 → 「已存在」，
	// 不需要读驱动专有错误码（与全仓其余 Insert 同一写法）。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO feature_write_receipt ("+featureWriteReceiptInsertColumns+") VALUES ("+
			placeholders(featureWriteReceiptInsertArgs)+") ON DUPLICATE KEY UPDATE mtime = mtime",
		r.RequestID, r.OpType, r.FeatureKey, r.Version, r.EntityScope, r.EntityID, r.RowCount,
		r.RequestDigest, r.DigestVer, r.State, "", "", 0, 0, "", r.Operator, r.TraceID,
		r.LeaseOwner, r.LeaseExpireAt, r.Ctime, r.Mtime, r.FinishedAt)
	if err != nil {
		return ReceiptBeginResult{}, fmt.Errorf("feature_write_receipt Begin: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ReceiptBeginResult{}, fmt.Errorf("feature_write_receipt Begin RowsAffected: %w", err)
	}
	if n == 1 {
		id, err := res.LastInsertId()
		if err != nil {
			return ReceiptBeginResult{}, fmt.Errorf("feature_write_receipt Begin LastInsertId: %w", err)
		}
		r.ReceiptID = id
		return ReceiptBeginResult{Receipt: r, Execute: true}, nil
	}

	// 已存在：按三种既有状态分别处置。
	existing, err := m.Find(ctx, r.RequestID, r.OpType)
	if err != nil {
		return ReceiptBeginResult{}, err
	}
	if existing == nil {
		// 唯一键冲突却查不到行，只可能是这句之前该行为主键被清理（保留期清理正好删了它）。
		// 报 ErrReceiptNotFound 让调用方重试整把 request_id，比强行假定可执行安全。
		return ReceiptBeginResult{}, ErrReceiptNotFound
	}
	if existing.RequestDigest != r.RequestDigest {
		// 不回显两侧摘要：调用方拿着摘要也推不出正确的请求体，只会开始猜。
		return ReceiptBeginResult{}, ErrRequestIdReused
	}
	switch existing.State {
	case ReceiptStateDone:
		// 回放：首次结果已在行上，绝不能重复执行副作用。
		return ReceiptBeginResult{Receipt: existing}, nil
	case ReceiptStateFailed:
		// 失败未产生副作用，同 request_id 重试是幂等语义的一部分：重置为 in_progress 再接手。
		ok, err := m.retake(ctx, existing, r.LeaseOwner, now, leaseSeconds, false)
		if err != nil {
			return ReceiptBeginResult{}, err
		}
		if !ok {
			// 带上行内容：调用方按 lease_expire_at 决定退避多久。
			return ReceiptBeginResult{Receipt: existing}, ErrReceiptInProgress
		}
		return ReceiptBeginResult{Receipt: existing, Execute: true}, nil
	case ReceiptStateInProgress:
		if existing.LeaseExpireAt > now {
			// 首次请求仍在执行中：并发跑第二遍才是真事故（双份审计、双份计数）。
			return ReceiptBeginResult{Receipt: existing}, ErrReceiptInProgress
		}
		// 租约过期 = 持有者已崩溃，接管它。
		ok, err := m.retake(ctx, existing, r.LeaseOwner, now, leaseSeconds, true)
		if err != nil {
			return ReceiptBeginResult{}, err
		}
		if !ok {
			return ReceiptBeginResult{Receipt: existing}, ErrReceiptInProgress
		}
		return ReceiptBeginResult{Receipt: existing, Execute: true}, nil
	default:
		return ReceiptBeginResult{}, fmt.Errorf("%w: receipt state %q", ErrReceiptStateInvalid,
			existing.State)
	}
}

// retake 把一行重置回 in_progress 并换上新租约。requireLease 决定接管条件：
// failed 只看状态，过期 in_progress 还要在语句里再确认一次租约确实已过期——
// 「读-改-写」之间的窗口必须由条件更新本身关掉，否则两个接管者会同时拿到执行权。
// state 一定改变（done/failed→in_progress 或 lease_expire_at 必然推后），
// 因此 RowsAffected 可信，不依赖 clientFoundRows。
func (m *defaultWriteReceiptModel) retake(ctx context.Context, existing *WriteReceipt,
	owner string, now, leaseSeconds int64, requireLease bool) (bool, error) {
	query := "UPDATE feature_write_receipt SET state = ?, lease_owner = ?, lease_expire_at = ?," +
		" result_json = '', result_digest = '', detail_kept = 0, affected_rows = 0, error_code = ''," +
		" finished_at = 0, mtime = ? WHERE receipt_id = ? AND state = ?"
	args := []any{ReceiptStateInProgress, owner, now + leaseSeconds, now, existing.ReceiptID,
		existing.State}
	if requireLease {
		query += " AND lease_expire_at <= ?"
		args = append(args, now)
	}
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("feature_write_receipt retake: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_write_receipt retake RowsAffected: %w", err)
	}
	if n == 1 {
		existing.State = ReceiptStateInProgress
		existing.LeaseOwner = owner
		existing.LeaseExpireAt = now + leaseSeconds
		existing.ResultJSON = ""
		existing.ResultDigest = ""
		existing.DetailKept = 0
		existing.AffectedRows = 0
		existing.ErrorCode = ""
		existing.FinishedAt = 0
		existing.Mtime = now
	}
	return n == 1, nil
}

func (m *defaultWriteReceiptModel) MarkDone(ctx context.Context, requestID, opType, leaseOwner string,
	snap ReceiptSnapshot) (bool, error) {
	if err := validateReceiptKey(requestID, opType, leaseOwner); err != nil {
		return false, err
	}
	if len(snap.ResultJSON) > MaxReceiptResultBytes {
		return false, fmt.Errorf("%w: %d bytes > %d", ErrReceiptResultTooLarge,
			len(snap.ResultJSON), MaxReceiptResultBytes)
	}
	var detail int32
	if snap.DetailKept {
		detail = 1
	}
	now := nowUnix()
	// state 由 in_progress 变 done，必然改变行：RowsAffected 可信。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_write_receipt SET state = ?, result_json = ?, result_digest = ?,"+
			" detail_kept = ?, affected_rows = ?, error_code = '', finished_at = ?, mtime = ?"+
			" WHERE request_id = ? AND op_type = ? AND state = ? AND lease_owner = ?"+
			" AND lease_expire_at > ?",
		ReceiptStateDone, snap.ResultJSON, snap.ResultDigest, detail, snap.AffectedRows, now, now,
		requestID, opType, ReceiptStateInProgress, leaseOwner, now)
	if err != nil {
		return false, fmt.Errorf("feature_write_receipt MarkDone: %w", err)
	}
	return changedRows(res, "feature_write_receipt MarkDone")
}

func (m *defaultWriteReceiptModel) MarkFailed(ctx context.Context, requestID, opType, leaseOwner,
	errorCode string) (bool, error) {
	if err := validateReceiptKey(requestID, opType, leaseOwner); err != nil {
		return false, err
	}
	code := strings.TrimSpace(errorCode)
	if code == "" {
		return false, ErrReceiptOpRequired
	}
	if len(code) > MaxReceiptErrorCodeLen {
		// 按 rune 截断：错误短码正常都是 ASCII，但截半字会留下无法匹配的脏值。
		code = truncateRunes(code, MaxReceiptErrorCodeLen)
	}
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE feature_write_receipt SET state = ?, error_code = ?, finished_at = ?, mtime = ?,"+
			" lease_expire_at = ?"+
			" WHERE request_id = ? AND op_type = ? AND state = ? AND lease_owner = ?"+
			" AND lease_expire_at > ?",
		ReceiptStateFailed, code, now, now, now, requestID, opType, ReceiptStateInProgress,
		leaseOwner, now)
	if err != nil {
		return false, fmt.Errorf("feature_write_receipt MarkFailed: %w", err)
	}
	return changedRows(res, "feature_write_receipt MarkFailed")
}

func (m *defaultWriteReceiptModel) Find(ctx context.Context, requestID, opType string) (*WriteReceipt, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, ErrRequestIdRequired
	}
	if !ValidReceiptOp(opType) {
		return nil, fmt.Errorf("%w: op_type %q", ErrReceiptOpRequired, opType)
	}
	var row WriteReceipt
	err := m.conn.QueryRowCtx(ctx, &row,
		writeReceiptSelect+" WHERE request_id = ? AND op_type = ? LIMIT 1", requestID, opType)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_write_receipt Find: %w", err)
	}
	return &row, nil
}

func (m *defaultWriteReceiptModel) CountStuck(ctx context.Context, before int64) (int64, error) {
	if before <= 0 {
		// 与 PurgeExpired 的 before 同语义：0 = 以当前时刻为准（此刻所有租约过期的行）。
		before = nowUnix()
	}
	var count int64
	err := m.conn.QueryRowCtx(ctx, &count,
		"SELECT COUNT(*) FROM feature_write_receipt WHERE state = ? AND lease_expire_at < ? LIMIT 1",
		ReceiptStateInProgress, before)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("feature_write_receipt CountStuck: %w", err)
	}
	return count, nil
}

func (m *defaultWriteReceiptModel) SelectAgedIDs(ctx context.Context, before int64,
	limit int32) ([]int64, error) {
	if limit <= 0 || limit > MaxPurgeRows {
		return nil, fmt.Errorf("%w: limit %d out of 1..%d", ErrLimitTooLarge, limit, MaxPurgeRows)
	}
	if before <= 0 {
		// 这里不给「0 = 现在」的默认值：清理保留期的语义就是「删掉多久以前的」，
		// 传 0 会被解释成「删掉全部终态回执」，那是一次误操作而不是缺省值。
		return nil, ErrCutoffRequired
	}
	var ids []int64
	err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT receipt_id FROM feature_write_receipt"+
			" WHERE state IN (?, ?) AND mtime < ? ORDER BY receipt_id ASC LIMIT ?",
		ReceiptStateDone, ReceiptStateFailed, before, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_write_receipt SelectAgedIDs: %w", err)
	}
	return ids, nil
}

func (m *defaultWriteReceiptModel) DeleteByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	if len(ids) > MaxPurgeRows {
		return 0, fmt.Errorf("%w: %d ids > %d", ErrLimitTooLarge, len(ids), MaxPurgeRows)
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return 0, ErrLimitTooLarge
		}
		args = append(args, id)
	}
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM feature_write_receipt WHERE receipt_id IN ("+placeholders(len(ids))+")", args...)
	if err != nil {
		return 0, fmt.Errorf("feature_write_receipt DeleteByIDs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("feature_write_receipt DeleteByIDs RowsAffected: %w", err)
	}
	return n, nil
}

func validateReceiptKey(requestID, opType, leaseOwner string) error {
	if strings.TrimSpace(requestID) == "" {
		return ErrRequestIdRequired
	}
	if !ValidReceiptOp(opType) {
		return fmt.Errorf("%w: op_type %q", ErrReceiptOpRequired, opType)
	}
	if strings.TrimSpace(leaseOwner) == "" {
		return ErrOperatorRequired
	}
	return nil
}

// changedRows 把 RowsAffected 的取数样板收在一处。
// 语义前提：调用方保证被更新的集合里至少有一列必然改变（见 retake / MarkDone 的注释）。
func changedRows(res sql.Result, op string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return n == 1, nil
}
