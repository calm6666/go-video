package model

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 授予台账动作，与 rpc.GrantInfo.action 注释取值严格一致（只有这四个）。
// 新增动作等于改契约，必须先改 proto 再生成，不得在实现里私自定义。
const (
	// ActionGrant 首次开通（此前没有该档位身份行）。
	ActionGrant = "GRANT"
	// ActionExtend 续期/延长（在既有到期时间上顺延）。
	ActionExtend = "EXTEND"
	// ActionRevoke 收回（退款回收、运营纠错），delta_days 为负。
	ActionRevoke = "REVOKE"
	// ActionExpire 到期置灰（cron 判定 expire_at <= now 后写入）。
	ActionExpire = "EXPIRE"
)

// MaxReasonLength 是台账 reason 的字符上限，与 mb_grant.reason 列宽一致。
// reason 只写摘要，禁止写入手机号/证件号/支付凭据等 PII 与密钥。
const MaxReasonLength = 255

// grantColumns 是 mb_grant 的完整列清单。
const grantColumns = "grant_id, mid, vip_type, action, delta_days, plan_id, source, biz_order_no, " +
	"payment_no, before_expire_at, after_expire_at, operator, request_id, reason, ctime"

// Grant 授予/变更台账行：谁、因为什么、动了多少天，必须可回溯。
//
// 本表是「用户是否开通会员」的事实源：判定读的就是这里的未过期结果投影到
// mb_membership 的行，没有台账就没有开通，任何接口都不会凭空给出 granted=true。
type Grant struct {
	GrantID        int64  `db:"grant_id"`         // 自增主键
	Mid            int64  `db:"mid"`              // 用户 ID
	VipType        int32  `db:"vip_type"`         // 档位
	Action         string `db:"action"`           // GRANT/EXTEND/REVOKE/EXPIRE
	DeltaDays      int32  `db:"delta_days"`       // 本次影响天数，收回为负
	PlanID         int64  `db:"plan_id"`          // 关联套餐，0 表示无
	Source         int32  `db:"source"`           // 来源，见 GrantSource*
	BizOrderNo     string `db:"biz_order_no"`     // 订单号引用（跨服务只存主键）
	PaymentNo      string `db:"payment_no"`       // 资金流水号引用
	BeforeExpireAt int64  `db:"before_expire_at"` // 变更前到期时间
	AfterExpireAt  int64  `db:"after_expire_at"`  // 变更后到期时间
	Operator       string `db:"operator"`         // 操作者；用户自助为 "user"
	RequestID      string `db:"request_id"`       // 幂等键（唯一）
	Reason         string `db:"reason"`           // 台账摘要
	Ctime          int64  `db:"ctime"`            // 创建时间（Unix 秒）
}

// GrantQuery 台账分页条件；零值字段表示不过滤。
type GrantQuery struct {
	Mid        int64
	VipType    int32
	Source     int32
	BizOrderNo string
	FromTs     int64
	ToTs       int64
	Offset     int64
	Limit      int64
}

// GrantModel mb_grant 表接口。
type GrantModel interface {
	// InsertTx 写入台账并返回 grant_id。request_id 唯一索引是写幂等的最终防线：
	// 冲突时返回可被 IsDuplicate 识别的错误，调用方必须回查首次结果而不是重试加时长。
	InsertTx(ctx context.Context, session sqlx.Session, g *Grant) (int64, error)
	// FindByRequestID 幂等重放回查；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*Grant, error)
	// FindOne 按 grant_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, grantID int64) (*Grant, error)
	// FindAction 回查某档位针对 expireAt 的该动作台账（取最新一条）。
	// cron 换了 request_id 再来一次时用它去重：到期是幂等事件，不该记成两次，
	// 且 ExpireMembershipReply 只有 skipped 没有 duplicated，需要把首次 grant_id 还回去。
	FindAction(ctx context.Context, mid int64, vipType int32, action string, expireAt int64) (*Grant, error)
	// List 分页查询台账，返回当页数据与总数。
	List(ctx context.Context, q GrantQuery) ([]*Grant, int64, error)
	// IsDuplicate 暴露唯一索引冲突判定。
	IsDuplicate(err error) bool
}

type defaultGrantModel struct {
	conn sqlx.SqlConn
}

// NewGrantModel 创建 GrantModel 实现。
func NewGrantModel(conn sqlx.SqlConn) GrantModel {
	return &defaultGrantModel{conn: conn}
}

func (m *defaultGrantModel) InsertTx(ctx context.Context, session sqlx.Session, g *Grant) (int64, error) {
	const query = "INSERT INTO mb_grant (mid, vip_type, action, delta_days, plan_id, source, biz_order_no, " +
		"payment_no, before_expire_at, after_expire_at, operator, request_id, reason, ctime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	if g.Ctime == 0 {
		g.Ctime = nowUnix()
	}
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		g.Mid, g.VipType, g.Action, g.DeltaDays, g.PlanID, g.Source, g.BizOrderNo,
		g.PaymentNo, g.BeforeExpireAt, g.AfterExpireAt, g.Operator, g.RequestID, g.Reason, g.Ctime)
	if err != nil {
		return 0, fmt.Errorf("mb_grant Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("mb_grant Insert LastInsertId: %w", err)
	}
	g.GrantID = id
	return id, nil
}

func (m *defaultGrantModel) FindByRequestID(ctx context.Context, requestID string) (*Grant, error) {
	var g Grant
	query := "SELECT " + grantColumns + " FROM mb_grant WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &g, query, requestID); err != nil {
		return noRowsAsNil(&g, err, "mb_grant FindByRequestID")
	}
	return &g, nil
}

func (m *defaultGrantModel) FindOne(ctx context.Context, grantID int64) (*Grant, error) {
	var g Grant
	query := "SELECT " + grantColumns + " FROM mb_grant WHERE grant_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &g, query, grantID); err != nil {
		return noRowsAsNil(&g, err, "mb_grant FindOne")
	}
	return &g, nil
}

func (m *defaultGrantModel) FindAction(ctx context.Context, mid int64, vipType int32, action string, expireAt int64) (*Grant, error) {
	var g Grant
	const query = "SELECT " + grantColumns + " FROM mb_grant WHERE mid = ? AND vip_type = ? AND action = ? AND after_expire_at = ?" +
		" ORDER BY grant_id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &g, query, mid, vipType, action, expireAt); err != nil {
		return noRowsAsNil(&g, err, "mb_grant FindAction")
	}
	return &g, nil
}

func (m *defaultGrantModel) List(ctx context.Context, q GrantQuery) ([]*Grant, int64, error) {
	where, args := grantQueryClause(q)

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(1) FROM mb_grant WHERE "+where, args...); err != nil {
		return nil, 0, fmt.Errorf("mb_grant List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	query := "SELECT " + grantColumns + " FROM mb_grant WHERE " + where +
		" ORDER BY grant_id DESC LIMIT ? OFFSET ?"
	queryArgs := append(append([]interface{}{}, args...), q.Limit, q.Offset)

	var rows []*Grant
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, queryArgs...); err != nil {
		return nil, 0, fmt.Errorf("mb_grant List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultGrantModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }

func grantQueryClause(q GrantQuery) (string, []interface{}) {
	conds := []string{"1 = 1"}
	var args []interface{}
	if q.Mid != 0 {
		conds = append(conds, "mid = ?")
		args = append(args, q.Mid)
	}
	if q.VipType != 0 {
		conds = append(conds, "vip_type = ?")
		args = append(args, q.VipType)
	}
	if q.Source != 0 {
		conds = append(conds, "source = ?")
		args = append(args, q.Source)
	}
	if strings.TrimSpace(q.BizOrderNo) != "" {
		conds = append(conds, "biz_order_no = ?")
		args = append(args, strings.TrimSpace(q.BizOrderNo))
	}
	if q.FromTs > 0 {
		conds = append(conds, "ctime >= ?")
		args = append(args, q.FromTs)
	}
	if q.ToTs > 0 {
		conds = append(conds, "ctime <= ?")
		args = append(args, q.ToTs)
	}
	return strings.Join(conds, " AND "), args
}
