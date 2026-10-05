package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// membershipColumns 是 mb_membership 的完整列清单，
// 与 deploy/migrations/membership/000001_create_membership_tables.sql 一一对应。
const membershipColumns = "membership_id, mid, vip_type, start_at, expire_at, auto_renew, " +
	"auto_renew_channel, auto_renew_signed_at, source, paid_month_count, version, ctime, mtime"

// Membership 用户会员身份行（(mid, vip_type) 唯一）。
//
// 「是不是会员」的唯一事实源就是本表：expire_at 是与服务端 now 比较的门槛，
// 没有落库的行一定判定为未开通，任何默认放行的写法都是缺陷。
type Membership struct {
	MembershipID      int64  `db:"membership_id"`        // 自增主键
	Mid               int64  `db:"mid"`                  // 用户 ID
	VipType           int32  `db:"vip_type"`             // 档位，见 VipType*
	StartAt           int64  `db:"start_at"`             // 首次开通时间（Unix 秒）
	ExpireAt          int64  `db:"expire_at"`            // 到期时间（Unix 秒），<= now 即已过期
	AutoRenew         int32  `db:"auto_renew"`           // 自动续费签约位（沙箱只记意愿）
	AutoRenewChannel  string `db:"auto_renew_channel"`   // 签约渠道，未签约为空
	AutoRenewSignedAt int64  `db:"auto_renew_signed_at"` // 最近一次签约/解约时间
	Source            int32  `db:"source"`               // 最近一次变更来源，见 GrantSource*
	PaidMonthCount    int32  `db:"paid_month_count"`     // 累计付费月数快照（单调不减）
	Version           int64  `db:"version"`              // CAS 位
	Ctime             int64  `db:"ctime"`                // 创建时间（Unix 秒）
	Mtime             int64  `db:"mtime"`                // 修改时间（Unix 秒）
}

// IsActive 在给定时刻是否仍然有效（expire_at 恰等于 now 视为已过期，与 cron 口径一致）。
func (m *Membership) IsActive(now int64) bool {
	return m != nil && m.ExpireAt > now
}

// TierSufficient 判定持有档位是否满足要求档位。
// 档位序为真：PREMIUM(1) < PREMIUM_PLUS(2)，超级大会员拥有大会员的全部权益（超集）。
func TierSufficient(held, required int32) bool { return held >= required }

// MembershipModel mb_membership 表接口。
type MembershipModel interface {
	// InsertTx 新建会员身份行，返回 membership_id；(mid,vip_type) 冲突由调用方按重放处理。
	InsertTx(ctx context.Context, session sqlx.Session, m *Membership) (int64, error)
	// UpdateTx 以 m.Version 为 CAS 条件写入到期/签约/来源/付费月数（version 自增）。
	// 返回 false 表示并发已改动，调用方需重读重试。
	UpdateTx(ctx context.Context, session sqlx.Session, m *Membership) (bool, error)
	// FindOne 按 (mid, vip_type) 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, mid int64, vipType int32) (*Membership, error)
	// FindOneTx 事务内按 (mid, vip_type) 查询：授予/收回必须在同一事务里读到
	// 当前行（含并发未提交的自身写入），否则会退化成「先插后撞唯一键」的错误路径。
	FindOneTx(ctx context.Context, session sqlx.Session, mid int64, vipType int32) (*Membership, error)
	// ListByMid 列出该用户全部档位身份（含已过期行，判定由调用方按 now 完成）。
	ListByMid(ctx context.Context, mid int64) ([]*Membership, error)
	// ListExpiring 按到期时间闭区间 [fromExpireAt, toExpireAt] 升序扫描，limit 截断；
	// autoRenewOnly=true 时只返回签约行（cron 续费批次）。
	ListExpiring(ctx context.Context, fromExpireAt, toExpireAt int64, autoRenewOnly bool, limit int64) ([]*Membership, error)
	// IsDuplicate 暴露唯一索引冲突判定。
	IsDuplicate(err error) bool
}

type defaultMembershipModel struct {
	conn sqlx.SqlConn
}

// NewMembershipModel 创建 MembershipModel 实现。
func NewMembershipModel(conn sqlx.SqlConn) MembershipModel {
	return &defaultMembershipModel{conn: conn}
}

func (m *defaultMembershipModel) InsertTx(ctx context.Context, session sqlx.Session, mm *Membership) (int64, error) {
	const query = "INSERT INTO mb_membership (mid, vip_type, start_at, expire_at, auto_renew, auto_renew_channel, " +
		"auto_renew_signed_at, source, paid_month_count, version, ctime, mtime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	now := nowUnix()
	if mm.Ctime == 0 {
		mm.Ctime = now
	}
	mm.Mtime = now
	mm.Version = 1
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		mm.Mid, mm.VipType, mm.StartAt, mm.ExpireAt, mm.AutoRenew, mm.AutoRenewChannel,
		mm.AutoRenewSignedAt, mm.Source, mm.PaidMonthCount, mm.Version, mm.Ctime, mm.Mtime)
	if err != nil {
		return 0, fmt.Errorf("mb_membership Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("mb_membership Insert LastInsertId: %w", err)
	}
	mm.MembershipID = id
	return id, nil
}

func (m *defaultMembershipModel) UpdateTx(ctx context.Context, session sqlx.Session, mm *Membership) (bool, error) {
	// mid/vip_type 是身份主键，不可改；start_at 只在创建时落定，这里回写原值。
	const query = "UPDATE mb_membership SET expire_at = ?, auto_renew = ?, auto_renew_channel = ?, " +
		"auto_renew_signed_at = ?, source = ?, paid_month_count = ?, version = version + 1, mtime = ? " +
		"WHERE membership_id = ? AND version = ?"
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		mm.ExpireAt, mm.AutoRenew, mm.AutoRenewChannel, mm.AutoRenewSignedAt, mm.Source,
		mm.PaidMonthCount, nowUnix(), mm.MembershipID, mm.Version)
	if err != nil {
		return false, fmt.Errorf("mb_membership Update: %w", err)
	}
	return rowsAffected(res, "mb_membership Update")
}

func (m *defaultMembershipModel) FindOne(ctx context.Context, mid int64, vipType int32) (*Membership, error) {
	var mm Membership
	query := "SELECT " + membershipColumns + " FROM mb_membership WHERE mid = ? AND vip_type = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &mm, query, mid, vipType); err != nil {
		return noRowsAsNil(&mm, err, "mb_membership FindOne")
	}
	return &mm, nil
}

func (m *defaultMembershipModel) FindOneTx(ctx context.Context, session sqlx.Session, mid int64, vipType int32) (*Membership, error) {
	var mm Membership
	query := "SELECT " + membershipColumns + " FROM mb_membership WHERE mid = ? AND vip_type = ? LIMIT 1"
	var err error
	if session != nil {
		err = session.QueryRowCtx(ctx, &mm, query, mid, vipType)
	} else {
		err = m.conn.QueryRowCtx(ctx, &mm, query, mid, vipType)
	}
	if err != nil {
		return noRowsAsNil(&mm, err, "mb_membership FindOneTx")
	}
	return &mm, nil
}

func (m *defaultMembershipModel) ListByMid(ctx context.Context, mid int64) ([]*Membership, error) {
	var rows []*Membership
	query := "SELECT " + membershipColumns + " FROM mb_membership WHERE mid = ? ORDER BY vip_type DESC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid); err != nil {
		return nil, fmt.Errorf("mb_membership ListByMid: %w", err)
	}
	return rows, nil
}

func (m *defaultMembershipModel) ListExpiring(ctx context.Context, fromExpireAt, toExpireAt int64, autoRenewOnly bool, limit int64) ([]*Membership, error) {
	if limit <= 0 {
		limit = 100
	}
	query := "SELECT " + membershipColumns + " FROM mb_membership WHERE expire_at BETWEEN ? AND ?"
	args := []interface{}{fromExpireAt, toExpireAt}
	if autoRenewOnly {
		query += " AND auto_renew = 1"
	}
	// 按 (expire_at, membership_id) 升序：cron 以 expire_at 为游标推进，
	// 同秒内的行必须以主键定序，否则同一秒行数超过批大小时会反复取到同一批。
	query += " ORDER BY expire_at ASC, membership_id ASC LIMIT ?"
	args = append(args, limit)

	var rows []*Membership
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("mb_membership ListExpiring: %w", err)
	}
	return rows, nil
}

func (m *defaultMembershipModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }

// PickMembershipFor 从该用户的身份行里挑出「判定依据行」。
//
// wantVip != 0：返回该档位的行（不管是否过期，过期与否由调用方按 now 判定，
// 这样才能区分 ENTITLEMENT_NO_MEMBERSHIP 与 ENTITLEMENT_EXPIRED）。
// wantVip == 0：优先返回生效中的最高档；全都过期时退回曾经达到的最高档，
// 让「我的会员」页仍能展示到期时间与档位。
func PickMembershipFor(rows []*Membership, wantVip int32, now int64) *Membership {
	var active, any *Membership
	for _, r := range rows {
		if r == nil {
			continue
		}
		if wantVip != 0 {
			if r.VipType != wantVip {
				continue
			}
			return r // (mid,vip_type) 唯一，最多一行
		}
		if any == nil || r.VipType > any.VipType {
			any = r
		}
		if r.IsActive(now) && (active == nil || r.VipType > active.VipType) {
			active = r
		}
	}
	if wantVip != 0 {
		return nil
	}
	if active != nil {
		return active
	}
	return any
}
