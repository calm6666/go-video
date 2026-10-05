// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	auditrpc "go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AuditListArchivesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询归档批次（chain_key/state/时间过滤）
func NewAuditListArchivesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditListArchivesLogic {
	return &AuditListArchivesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 归档批次是审计离开热表前后的证据（manifest_hash / last_entry_hash），
// 运营据此判断某条链能否继续增量校验；网关不解读 state，原样透传。
func (l *AuditListArchivesLogic) AuditListArchives(req *types.ParamAuditListArchives) (resp *types.AuditArchivesResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	if err := auditTimeRange(req.StartAt, req.EndAt); err != nil {
		return nil, err
	}
	pn, ps := normalizeAuditPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Audit.ListArchiveBatches(l.ctx, &auditrpc.ListArchiveBatchesReq{
		Ctx:      callCtx,
		ChainKey: req.ChainKey,
		State:    req.State,
		StartAt:  req.StartAt,
		EndAt:    req.EndAt,
		Pn:       pn,
		Ps:       ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditListArchives: operator=%d chain_key=%s state=%s err=%v",
			callCtx.GetOperatorId(), req.ChainKey, req.State, err)
		return nil, err
	}
	return &types.AuditArchivesResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AuditArchivesData{Items: auditArchiveBatchesToAPI(reply.GetItems()), Total: reply.GetTotal()},
		TTL:     0,
	}, nil
}
