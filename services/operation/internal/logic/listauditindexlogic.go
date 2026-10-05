package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAuditIndexLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAuditIndexLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAuditIndexLogic {
	return &ListAuditIndexLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询管理操作审计索引（正文证据在下游服务/audit）
func (l *ListAuditIndexLogic) ListAuditIndex(in *rpc.ListAuditIndexReq) (*rpc.ListAuditIndexReply, error) {
	// 审计查询本身也必须留痕主体：没有可信 OpContext 的调用一律拒绝。
	if _, err := actorFrom(in.Ctx); err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.Repository.ListAuditIndex(l.ctx, repository.AuditQuery{
		AdminID:      in.AdminId,
		Action:       in.Action,
		ResourceType: in.ResourceType,
		ResourceID:   in.ResourceId,
		StartAt:      in.StartAt,
		EndAt:        in.EndAt,
		Pn:           in.Pn,
		Ps:           in.Ps,
	})
	if err != nil {
		l.Errorf("operation/ListAuditIndex: admin=%d action=%q err=%v", in.AdminId, in.Action, err)
		return nil, err
	}
	return &rpc.ListAuditIndexReply{Items: auditItems(rows), Total: total}, nil
}
