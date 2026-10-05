package model

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 走 mb_biz_request 幂等台账的写用例名。
//
// 为什么要单独一张表：mb_grant 的 action 取值被契约钉死在
// GRANT/EXTEND/REVOKE/EXPIRE 四种「时长变更」，SetAutoRenew 与 UpsertEntitlement
// 不动时长，硬塞进台账会造出一个契约里没有的动作。
// 这两类写请求因此用 request_id 唯一索引 + 参数指纹做幂等，
// 语义与 mb_grant.request_id 完全一致（重放回查首次结果、换参数报冲突）。
const (
	// ApiSetAutoRenew 自动续费签约/解约。
	ApiSetAutoRenew = "SetAutoRenew"
	// ApiUpsertEntitlement 权益码新增/开关。
	ApiUpsertEntitlement = "UpsertEntitlement"
)

// bizRequestColumns 是 mb_biz_request 的完整列清单。
const bizRequestColumns = "request_id, api, subject, mid, vip_type, result_id, params_fingerprint, operator, ctime"

// BizRequest 写请求幂等台账。主键就是幂等键本身，因此不需要自增列。
type BizRequest struct {
	RequestID         string `db:"request_id"`         // 幂等键（主键）
	Api               string `db:"api"`                // 用例名，见 Api*
	Subject           string `db:"subject"`            // 作用对象标识："mid:<mid>:vip:<n>" 或权益码
	Mid               int64  `db:"mid"`                // 涉及用户（无则 0）
	VipType           int32  `db:"vip_type"`           // 涉及档位（无则 0）
	ResultID          int64  `db:"result_id"`          // 首次生效的结果主键（entitlement_id 等）
	ParamsFingerprint string `db:"params_fingerprint"` // 关键参数指纹
	Operator          string `db:"operator"`           // 操作者
	Ctime             int64  `db:"ctime"`              // 创建时间（Unix 秒）
}

// BizRequestModel mb_biz_request 表接口。
type BizRequestModel interface {
	// InsertTx 抢占幂等键。冲突返回可被 IsDuplicate 识别的错误，
	// 调用方必须回查首次记录并比对指纹，而不是当作「已经成功」。
	InsertTx(ctx context.Context, session sqlx.Session, r *BizRequest) error
	// FindByRequestIDTx 幂等重放回查（session 非空时在事务内读，能看见本事务的写入）；
	// 不存在返回 (nil, nil)。
	FindByRequestIDTx(ctx context.Context, session sqlx.Session, requestID string) (*BizRequest, error)
	// UpdateResultTx 回填首次结果主键（结果行创建之后才有 ID）。
	UpdateResultTx(ctx context.Context, session sqlx.Session, requestID string, resultID int64) error
	// IsDuplicate 暴露唯一索引冲突判定。
	IsDuplicate(err error) bool
}

type defaultBizRequestModel struct {
	conn sqlx.SqlConn
}

// NewBizRequestModel 创建 BizRequestModel 实现。
func NewBizRequestModel(conn sqlx.SqlConn) BizRequestModel {
	return &defaultBizRequestModel{conn: conn}
}

func (m *defaultBizRequestModel) InsertTx(ctx context.Context, session sqlx.Session, r *BizRequest) error {
	const query = "INSERT INTO mb_biz_request (request_id, api, subject, mid, vip_type, result_id, " +
		"params_fingerprint, operator, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"
	if r.Ctime == 0 {
		r.Ctime = nowUnix()
	}
	if _, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		r.RequestID, r.Api, r.Subject, r.Mid, r.VipType, r.ResultID,
		r.ParamsFingerprint, r.Operator, r.Ctime); err != nil {
		return fmt.Errorf("mb_biz_request Insert: %w", err)
	}
	return nil
}

func (m *defaultBizRequestModel) FindByRequestIDTx(ctx context.Context, session sqlx.Session, requestID string) (*BizRequest, error) {
	var r BizRequest
	query := "SELECT " + bizRequestColumns + " FROM mb_biz_request WHERE request_id = ? LIMIT 1"
	var err error
	if session != nil {
		err = session.QueryRowCtx(ctx, &r, query, requestID)
	} else {
		err = m.conn.QueryRowCtx(ctx, &r, query, requestID)
	}
	if err != nil {
		return noRowsAsNil(&r, err, "mb_biz_request FindByRequestID")
	}
	return &r, nil
}

func (m *defaultBizRequestModel) UpdateResultTx(ctx context.Context, session sqlx.Session, requestID string, resultID int64) error {
	const query = "UPDATE mb_biz_request SET result_id = ? WHERE request_id = ?"
	if _, err := execerOr(session, m.conn).ExecCtx(ctx, query, resultID, requestID); err != nil {
		return fmt.Errorf("mb_biz_request UpdateResult: %w", err)
	}
	return nil
}

func (m *defaultBizRequestModel) IsDuplicate(err error) bool { return isDuplicateErr(err) }
