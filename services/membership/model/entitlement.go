package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 授予来源，与 rpc.GrantSource 取值严格一致。
// 除 ADMIN_OPS/EXPERIENCE 外都应能回溯到一条 payment 流水（AGENTS.md §1 资金语义）。
const (
	// GrantSourceSandboxPurchase 沙箱订单履约开通。
	GrantSourceSandboxPurchase int32 = 1
	// GrantSourceSandboxAutoRenew 沙箱自动续费（cron 触发，不产生真实扣款）。
	GrantSourceSandboxAutoRenew int32 = 2
	// GrantSourceAdminOps 运营手工开通/延长（必须有 reason）。
	GrantSourceAdminOps int32 = 3
	// GrantSourceExperience 体验会员（活动发放）。
	GrantSourceExperience int32 = 4
	// GrantSourceLegacyImport 存量数据迁移。
	GrantSourceLegacyImport int32 = 5
)

// ValidGrantSource 判定来源是否可落库。
func ValidGrantSource(s int32) bool {
	return s >= GrantSourceSandboxPurchase && s <= GrantSourceLegacyImport
}

// IsPaidSource 判定来源是否代表「真实付费过」（沙箱台账也算，因为资金语义就是它）。
// 只有付费来源才累加 paid_month_count，运营赠送/体验/迁移不计入付费月数。
func IsPaidSource(s int32) bool {
	return s == GrantSourceSandboxPurchase || s == GrantSourceSandboxAutoRenew
}

// NeedsPaymentTrace 判定该来源是否必须带订单号或资金流水号。
// 运营手工与体验发放是本服务自己授权的两类，其余都必须可回溯。
func NeedsPaymentTrace(s int32) bool {
	switch s {
	case GrantSourceSandboxPurchase, GrantSourceSandboxAutoRenew, GrantSourceLegacyImport:
		return true
	default:
		return false
	}
}

// 权益码列宽（与 mb_entitlement DDL 逐字对齐）。
const (
	// MaxEntitlementCodeLength code 最大字符数。
	MaxEntitlementCodeLength = 64
	// MaxEntitlementNameLength name 最大字符数。
	MaxEntitlementNameLength = 64
	// MaxEntitlementDescLength description 最大字符数。
	MaxEntitlementDescLength = 255
)

// entitlementColumns 是 mb_entitlement 的完整列清单。
const entitlementColumns = "entitlement_id, code, name, description, min_vip_type, enabled, " +
	"version, updated_by, ctime, mtime"

// Entitlement 权益码目录项：某个能力需要哪一档会员（全站唯一判定口径的事实源）。
type Entitlement struct {
	EntitlementID int64  `db:"entitlement_id"` // 自增主键
	Code          string `db:"code"`           // 权益码（唯一，utf8mb4_bin 逐字节比较）
	Name          string `db:"name"`           // 展示名
	Description   string `db:"description"`    // 描述
	MinVipType    int32  `db:"min_vip_type"`   // 达标档位（含更高档）
	Enabled       int32  `db:"enabled"`        // 1 生效、0 下线
	Version       int64  `db:"version"`        // CAS 位
	UpdatedBy     string `db:"updated_by"`     // 最后修改者
	Ctime         int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime         int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// IsEnabled 权益码是否在生效中；下线码必须判定为 CODE_DISABLED，不得放行。
func (e *Entitlement) IsEnabled() bool { return e.Enabled == 1 }

// EntitlementModel mb_entitlement 表接口。
type EntitlementModel interface {
	// InsertTx 新增权益码，返回 entitlement_id。
	InsertTx(ctx context.Context, session sqlx.Session, e *Entitlement) (int64, error)
	// UpdateTx 以 expectedVersion 为条件更新（CAS），返回 false 表示版本冲突。
	UpdateTx(ctx context.Context, session sqlx.Session, e *Entitlement, expectedVersion int64) (bool, error)
	// FindByCode 按权益码查询；不存在返回 (nil, nil)。
	FindByCode(ctx context.Context, code string) (*Entitlement, error)
	// ListByCodes 批量按权益码查询（CheckEntitlements 一次问多项）；
	// 返回结果不保证覆盖所有入参，缺失的 code 由调用方判定为 CODE_UNKNOWN。
	ListByCodes(ctx context.Context, codes []string) ([]*Entitlement, error)
	// List 列出目录；enabledOnly=true 时只返回生效中的码。
	List(ctx context.Context, enabledOnly bool) ([]*Entitlement, error)
	// IsDuplicate 暴露唯一索引冲突判定。
	IsDuplicate(err error) bool
}

type defaultEntitlementModel struct {
	conn sqlx.SqlConn
}

// NewEntitlementModel 创建 EntitlementModel 实现。
func NewEntitlementModel(conn sqlx.SqlConn) EntitlementModel {
	return &defaultEntitlementModel{conn: conn}
}

func (m *defaultEntitlementModel) InsertTx(ctx context.Context, session sqlx.Session, e *Entitlement) (int64, error) {
	const query = "INSERT INTO mb_entitlement (code, name, description, min_vip_type, enabled, version, " +
		"updated_by, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"
	now := nowUnix()
	e.Ctime = now
	e.Mtime = now
	e.Version = 1
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		e.Code, e.Name, e.Description, e.MinVipType, e.Enabled, e.Version, e.UpdatedBy, e.Ctime, e.Mtime)
	if err != nil {
		return 0, fmt.Errorf("mb_entitlement Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("mb_entitlement Insert LastInsertId: %w", err)
	}
	e.EntitlementID = id
	return id, nil
}

func (m *defaultEntitlementModel) UpdateTx(ctx context.Context, session sqlx.Session, e *Entitlement, expectedVersion int64) (bool, error) {
	// code 不参与更新：权益码是跨服务的稳定引用，改名必须走「新增码 + 下线旧码」。
	const query = "UPDATE mb_entitlement SET name = ?, description = ?, min_vip_type = ?, enabled = ?, " +
		"version = version + 1, updated_by = ?, mtime = ? WHERE entitlement_id = ? AND version = ?"
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		e.Name, e.Description, e.MinVipType, e.Enabled, e.UpdatedBy, nowUnix(), e.EntitlementID, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("mb_entitlement Update: %w", err)
	}
	return rowsAffected(res, "mb_entitlement Update")
}

func (m *defaultEntitlementModel) FindByCode(ctx context.Context, code string) (*Entitlement, error) {
	var e Entitlement
	query := "SELECT " + entitlementColumns + " FROM mb_entitlement WHERE code = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &e, query, code); err != nil {
		return noRowsAsNil(&e, err, "mb_entitlement FindByCode")
	}
	return &e, nil
}

func (m *defaultEntitlementModel) ListByCodes(ctx context.Context, codes []string) ([]*Entitlement, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(codes)), ",")
	args := make([]interface{}, 0, len(codes))
	for _, c := range codes {
		args = append(args, c)
	}
	query := "SELECT " + entitlementColumns + " FROM mb_entitlement WHERE code IN (" + placeholders + ")"

	var rows []*Entitlement
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("mb_entitlement ListByCodes: %w", err)
	}
	return rows, nil
}

func (m *defaultEntitlementModel) List(ctx context.Context, enabledOnly bool) ([]*Entitlement, error) {
	query := "SELECT " + entitlementColumns + " FROM mb_entitlement"
	var args []interface{}
	if enabledOnly {
		query += " WHERE enabled = ?"
		args = append(args, 1)
	}
	query += " ORDER BY min_vip_type ASC, entitlement_id ASC LIMIT 500"

	var rows []*Entitlement
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("mb_entitlement List: %w", err)
	}
	return rows, nil
}

func (m *defaultEntitlementModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }
