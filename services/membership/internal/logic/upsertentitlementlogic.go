package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpsertEntitlementLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertEntitlementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertEntitlementLogic {
	return &UpsertEntitlementLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UpsertEntitlement 运营面权益码新增/开关。
//
// 口径：
//  1. code 一旦创建不可改（跨服务稳定引用），要换口径就新增码并把旧码 enabled=0；
//  2. enabled=0 不删行：判定要能区分 CODE_DISABLED（本地关掉）与 CODE_UNKNOWN（传错），
//     删行会让两者混成一类；
//  3. expected_version 是 CAS 位（新建传 0，更新必须给当前版本）；
//  4. request_id 幂等：不动时长，所以不进 mb_grant（action 取值被契约钉死），
//     而是走 mb_biz_request 的唯一索引 + 参数指纹，重放回查首次结果、换参数报冲突。
func (l *UpsertEntitlementLogic) UpsertEntitlement(in *rpc.UpsertEntitlementReq) (*rpc.EntitlementInfo, error) {
	code := strings.TrimSpace(in.Code)
	if code == "" {
		return nil, model.ErrEntitlementCodeRequired
	}
	if err := checkLen("code", code, model.MaxEntitlementCodeLength); err != nil {
		return nil, err
	}
	if !validEntitlementCode(code) {
		return nil, fmt.Errorf("%w: code must match [A-Za-z0-9._-]+", model.ErrEntitlementCodeRequired)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: entitlement name required", model.ErrEntitlementCodeRequired)
	}
	if err := checkLen("name", name, model.MaxEntitlementNameLength); err != nil {
		return nil, err
	}
	desc := strings.TrimSpace(in.Description)
	if err := checkLen("description", desc, model.MaxEntitlementDescLength); err != nil {
		return nil, err
	}
	if err := requireVipType(in.MinVipType); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := requireRequestId(in.RequestId); err != nil {
		return nil, err
	}
	operator := strings.TrimSpace(in.Operator)
	minVip := int32(in.MinVipType)
	enabled := int32FromBool(in.Enabled)

	existing, err := l.svcCtx.Entitlement.FindByCode(l.ctx, code)
	if err != nil {
		l.Errorf("membership/UpsertEntitlement: read code=%s err=%v", code, err)
		return nil, err
	}
	fp := model.Fingerprint("UpsertEntitlement", code, name, desc,
		fmt.Sprintf("%d", minVip), fmt.Sprintf("%d", enabled), fmt.Sprintf("%d", in.ExpectedVersion))

	// 幂等重放优先：命中同一 request_id 时把首次结果还给调用方，不二次改动目录。
	if req, ferr := l.svcCtx.Request.FindByRequestIDTx(l.ctx, nil, in.RequestId); ferr != nil {
		l.Errorf("membership/UpsertEntitlement: idempotency lookup request_id=%s err=%v", in.RequestId, ferr)
		return nil, ferr
	} else if req != nil {
		return l.replay(req, fp, code, in.RequestId)
	}

	if existing != nil && in.ExpectedVersion != existing.Version {
		// 先给一次明确的冲突，省下一次注定回滚的事务。
		// 放在幂等预读之后：超时重试带着首建时的 expected_version 回来，
		// 那一版此刻必然已经过期，若先校验就会把「我这次的写」误判成别人的并发写。
		return nil, fmt.Errorf("%w: expected_version=%d current=%d", model.ErrConcurrentUpdate, in.ExpectedVersion, existing.Version)
	}

	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		if err := l.svcCtx.Request.InsertTx(ctx, session, &model.BizRequest{
			RequestID:         in.RequestId,
			Api:               model.ApiUpsertEntitlement,
			Subject:           code,
			ParamsFingerprint: fp,
			Operator:          operator,
		}); err != nil {
			return err
		}
		if existing == nil {
			row := &model.Entitlement{
				Code:        code,
				Name:        name,
				Description: desc,
				MinVipType:  minVip,
				Enabled:     enabled,
				UpdatedBy:   operator,
			}
			id, err := l.svcCtx.Entitlement.InsertTx(ctx, session, row)
			if err != nil {
				return err
			}
			return l.svcCtx.Request.UpdateResultTx(ctx, session, in.RequestId, id)
		}

		next := *existing
		next.Name = name
		next.Description = desc
		next.MinVipType = minVip
		next.Enabled = enabled
		next.UpdatedBy = operator
		ok, err := l.svcCtx.Entitlement.UpdateTx(ctx, session, &next, existing.Version)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		return l.svcCtx.Request.UpdateResultTx(ctx, session, in.RequestId, existing.EntitlementID)
	})
	if err != nil {
		// 一个 1062 有两个来源（mb_biz_request 主键 / mb_entitlement.uniq_code），
		// 错误文本分不出是哪个，只能靠「幂等键到底记上没有」来判：
		// 记上了就是重放，没记上就是并发建了同码。
		if l.svcCtx.Request.IsDuplicate(err) {
			req, ferr := l.svcCtx.Request.FindByRequestIDTx(l.ctx, nil, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			if req != nil {
				return l.replay(req, fp, code, in.RequestId)
			}
		}
		if l.svcCtx.Entitlement.IsDuplicate(err) {
			// 并发有人在同一瞬间建了同码：本次已整体回滚，调用方重读后按更新重试。
			l.Infof("membership/UpsertEntitlement: code=%s created concurrently, request_id=%s", code, in.RequestId)
			return nil, model.ErrConcurrentUpdate
		}
		l.Errorf("membership/UpsertEntitlement: apply failed code=%s request_id=%s err=%v", code, in.RequestId, err)
		return nil, err
	}

	fresh, err := l.svcCtx.Entitlement.FindByCode(l.ctx, code)
	if err != nil {
		l.Errorf("membership/UpsertEntitlement: reread code=%s err=%v", code, err)
		return nil, err
	}
	return entitlementToRPC(fresh), nil
}

// replay 命中同一 request_id：指纹必须一致，否则是借幂等键改口径。
func (l *UpsertEntitlementLogic) replay(req *model.BizRequest, fp, code, requestID string) (*rpc.EntitlementInfo, error) {
	if req.ParamsFingerprint != fp {
		l.Errorf("membership/UpsertEntitlement: request_id=%s reused with different parameters", requestID)
		return nil, model.ErrRequestIdReused
	}
	row, err := l.svcCtx.Entitlement.FindByCode(l.ctx, code)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, model.ErrEntitlementNotFound
	}
	return entitlementToRPC(row), nil
}

// validEntitlementCode 权益码字符集：它是跨服务引用（playback 等会硬编码常量），
// 允许空格或中文会让「看起来相同」的两个码判定成两个结论。
func validEntitlementCode(code string) bool {
	for _, r := range code {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}
