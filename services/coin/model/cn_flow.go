package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 流水类型常量，与 cn_flow.flow_type 列和 rpc.CoinFlowType 取值严格一致，禁止重排。
const (
	// FlowTypeToss 投币扣减（delta 为负）。
	FlowTypeToss int32 = 1
	// FlowTypeCancelToss 取消投币退回（delta 为正）。
	FlowTypeCancelToss int32 = 2
	// FlowTypeOrderPack 硬币包订单履约发放（delta 为正，biz_no 必须是订单号）。
	FlowTypeOrderPack int32 = 3
	// FlowTypeAdminGrant 运营发放或扣回（delta 可正可负，operator/reason 必填）。
	FlowTypeAdminGrant int32 = 4
	// FlowTypeExpire 硬币过期：本项目未开启过期能力，恒不写入，仅占位对齐枚举。
	FlowTypeExpire int32 = 5
)

// 建仓初始币流水的固定标记：request_id 由 mid 唯一决定，
// 因此「同一用户被重复发初始币」在唯一索引上就会被挡死。
const (
	// InitialGrantBizNo 初始发放流水的 biz_no（对账时区分「白送的」和「买来的」）。
	InitialGrantBizNo = "INITIAL_BALANCE"
	// InitialGrantOperator 初始发放流水的操作者。
	InitialGrantOperator = "system"
	// InitialGrantRequestIDPrefix 初始发放流水 request_id 前缀。
	InitialGrantRequestIDPrefix = "coin:init:"
)

// InitialGrantRequestID 返回某用户建仓初始币流水的幂等键。
func InitialGrantRequestID(mid int64) string {
	return fmt.Sprintf("%s%d", InitialGrantRequestIDPrefix, mid)
}

// 文本列的字符上限，与 000001_create_coin_tables.sql 的 VARCHAR(n) 逐列对齐。
// 这里是唯一真值：写库前入参必须按**字符**收敛到这些宽度（见 internal/logic 的
// clipID / clipRemark），否则一条超长中文 remark 会把整笔已判定成功的扣币事务
// 拖成 MySQL 1406/1366 回滚。列宽被迁移改动时 model/migration_parity_test.go 会失败。
const (
	// MaxIDChars request_id / biz_no / operator / trace_id / last_request_id 的列宽。
	MaxIDChars = 64
	// MaxRemarkChars remark 的列宽（只放摘要，不得含 PII 与凭据）。
	MaxRemarkChars = 255
)

// flowColumns 与 000001_create_coin_tables.sql 的 cn_flow 逐列对应。
const flowColumns = "id, mid, flow_type, delta, balance_after, target_aid, biz_no, operator," +
	" request_id, remark, trace_id, ctime"

// Flow 硬币流水行（cn_flow 投影，对应 rpc.CoinFlowInfo）。
//
// append-only：本表只插入、不更新、不删除，是「余额从哪来、到哪去」的唯一可复算证据。
// 不变式：任意时刻 balance == SUM(该用户所有流水 delta)，
// 因此建仓发放初始币也必须写一条流水（见 InitialGrantRequestID），
// 否则余额比流水多出来的部分无法解释，对账直接失真。
// request_id 唯一索引同时承担两个职责：写接口幂等锚点 + 同参数校验（冲突由 logic 判定）。
type Flow struct {
	ID           int64  `db:"id"`            // 流水 ID（rpc.CoinFlowInfo.flow_id）
	Mid          int64  `db:"mid"`           // 归属用户
	FlowType     int32  `db:"flow_type"`     // 见 FlowType* 常量
	Delta        int64  `db:"delta"`         // 正入负出
	BalanceAfter int64  `db:"balance_after"` // 本笔落库后的余额快照（重放时据此回显，不必再猜）
	TargetAid    int64  `db:"target_aid"`    // 投币/取消类流水才有，发放类为 0
	BizNo        string `db:"biz_no"`        // 订单号/工单号/INITIAL_BALANCE
	Operator     string `db:"operator"`      // user / trade-order / 运营工号 / system / cron
	RequestID    string `db:"request_id"`    // 幂等键（唯一索引，utf8mb4_bin 逐字节比较）
	Remark       string `db:"remark"`        // 摘要，禁止放 PII 与原因明文长文
	TraceID      string `db:"trace_id"`      // 链路追踪 ID
	Ctime        int64  `db:"ctime"`         // 落库时间（Unix 秒）
}

// FlowFilter ListCoinFlows 的查询条件。零值一律表示「不限制该维度」，
// 因此跨用户台账（Mid=0）必须由调用方保证 FromTs/ToTs/BizNo 至少给一个，
// 否则会退化成全表扫描（model 侧会拒绝，见 List/Count 的 ErrUnboundedLedgerQuery）。
type FlowFilter struct {
	Mid      int64
	FlowType int32
	BizNo    string
	FromTs   int64
	ToTs     int64
}

// Bounded 判断条件是否足以走索引、不会扫全表。
func (f FlowFilter) Bounded() bool {
	return f.Mid > 0 || f.BizNo != "" || (f.FromTs > 0 && f.ToTs > 0 && f.ToTs >= f.FromTs)
}

// FlowModel cn_flow 表读写接口。
type FlowModel interface {
	// InsertTx 追加流水（事务内）。唯一键冲突直接上抛让事务回滚：
	// 幂等在 logic 层已按 request_id 先查后判，走到这里还撞键说明有并发绕过，
	// 宁可以失败告终也不能重复记账。
	InsertTx(ctx context.Context, session sqlx.Session, f *Flow) (int64, error)
	// FindByRequestID 按幂等键查流水；不存在返回 (nil, nil)。
	// 在事务内调用时传 session，保证读到自己所在事务的最新视图。
	FindByRequestID(ctx context.Context, session sqlx.Session, requestID string) (*Flow, error)
	// FindLatestByTarget 取某用户对某内容最近一条指定类型的流水（取消投币重放时回 flow_id）。
	// 不存在返回 (nil, nil)。
	FindLatestByTarget(ctx context.Context, mid, targetAid int64, flowType int32) (*Flow, error)
	// List 分页读流水，ctime 倒序；limit 必须由调用方夹到配置上限。
	List(ctx context.Context, f FlowFilter, offset int64, limit int) ([]*Flow, error)
	// Count 与 List 同一条件的总数。
	Count(ctx context.Context, f FlowFilter) (int64, error)
	// SumDelta 某用户流水净额（= 当前余额的独立算法，供对账与排障脚本使用）。
	SumDelta(ctx context.Context, mid int64) (int64, error)
}

type defaultFlowModel struct {
	conn sqlx.SqlConn
}

// NewFlowModel 构造 cn_flow 的 sqlx 实现。
func NewFlowModel(conn sqlx.SqlConn) FlowModel {
	return &defaultFlowModel{conn: conn}
}

const flowSelect = "SELECT " + flowColumns + " FROM cn_flow"

func (m *defaultFlowModel) InsertTx(ctx context.Context, session sqlx.Session, f *Flow) (int64, error) {
	if f.Mid <= 0 {
		return 0, ErrInvalidMid
	}
	if f.RequestID == "" {
		return 0, ErrRequestIDRequired
	}
	if f.Delta == 0 {
		return 0, ErrGrantDeltaInvalid
	}
	if f.Ctime == 0 {
		f.Ctime = nowUnix()
	}
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"INSERT INTO cn_flow (mid, flow_type, delta, balance_after, target_aid, biz_no,"+
			" operator, request_id, remark, trace_id, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		f.Mid, f.FlowType, f.Delta, f.BalanceAfter, f.TargetAid, f.BizNo,
		f.Operator, f.RequestID, f.Remark, f.TraceID, f.Ctime)
	if err != nil {
		return 0, fmt.Errorf("cn_flow InsertTx: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cn_flow InsertTx LastInsertId: %w", err)
	}
	f.ID = id
	return id, nil
}

func (m *defaultFlowModel) FindByRequestID(ctx context.Context, session sqlx.Session, requestID string) (*Flow, error) {
	if requestID == "" {
		return nil, ErrRequestIDRequired
	}
	var row Flow
	query := flowSelect + " WHERE request_id = ? LIMIT 1"
	if err := executor(session, m.conn).QueryRowCtx(ctx, &row, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cn_flow FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultFlowModel) FindLatestByTarget(ctx context.Context, mid, targetAid int64, flowType int32) (*Flow, error) {
	var row Flow
	query := flowSelect + " WHERE mid = ? AND target_aid = ? AND flow_type = ? ORDER BY id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, mid, targetAid, flowType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cn_flow FindLatestByTarget: %w", err)
	}
	return &row, nil
}

func (m *defaultFlowModel) List(ctx context.Context, f FlowFilter, offset int64, limit int) ([]*Flow, error) {
	query, args, err := flowWhere(f)
	if err != nil {
		return nil, err
	}
	query = flowSelect + query + " ORDER BY ctime DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	var rows []*Flow
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []*Flow{}, nil
		}
		return nil, fmt.Errorf("cn_flow List: %w", err)
	}
	if rows == nil {
		rows = []*Flow{}
	}
	return rows, nil
}

func (m *defaultFlowModel) Count(ctx context.Context, f FlowFilter) (int64, error) {
	query, args, err := flowWhere(f)
	if err != nil {
		return 0, err
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM cn_flow"+query, args...); err != nil {
		return 0, fmt.Errorf("cn_flow Count: %w", err)
	}
	return total, nil
}

func (m *defaultFlowModel) SumDelta(ctx context.Context, mid int64) (int64, error) {
	var sum sql.NullInt64
	err := m.conn.QueryRowCtx(ctx, &sum, "SELECT SUM(delta) FROM cn_flow WHERE mid = ?", mid)
	if err != nil {
		return 0, fmt.Errorf("cn_flow SumDelta: %w", err)
	}
	return sum.Int64, nil
}

// flowWhere 组装台账查询条件。所有值一律走占位符，不做字符串拼接。
func flowWhere(f FlowFilter) (string, []any, error) {
	if !f.Bounded() {
		return "", nil, ErrUnboundedLedgerQuery
	}
	// Bounded() 已保证至少一个真实条件，不拼 1 = 1 这种恒真谓词。
	conds := make([]string, 0, 5)
	args := make([]any, 0, 5)
	if f.Mid > 0 {
		conds = append(conds, "mid = ?")
		args = append(args, f.Mid)
	}
	if f.FlowType > 0 {
		conds = append(conds, "flow_type = ?")
		args = append(args, f.FlowType)
	}
	if f.BizNo != "" {
		conds = append(conds, "biz_no = ?")
		args = append(args, f.BizNo)
	}
	if f.FromTs > 0 {
		conds = append(conds, "ctime >= ?")
		args = append(args, f.FromTs)
	}
	if f.ToTs > 0 {
		conds = append(conds, "ctime <= ?")
		args = append(args, f.ToTs)
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}
