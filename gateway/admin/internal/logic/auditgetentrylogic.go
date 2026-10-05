// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	auditrpc "go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AuditGetEntryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按 entry_id 或 event_id 取单条审计（found=false 表示不存在，不报 NotFound）
func NewAuditGetEntryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditGetEntryLogic {
	return &AuditGetEntryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 取单条存证。entry_id 与 event_id 至少给一个（proto 注释「entry_id 优先」由 audit 实现），
// 两者都空时网关先拒绝，避免退化成「无条件取第一条」这类越权读取。
func (l *AuditGetEntryLogic) AuditGetEntry(req *types.ParamAuditGetEntry) (resp *types.AuditEntryResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	if req.EntryId <= 0 && req.EventId == "" {
		return nil, errors.New("gateway/admin: entry_id or event_id required")
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Audit.GetAuditEntry(l.ctx, &auditrpc.GetAuditEntryReq{
		Ctx:     callCtx,
		EntryId: req.EntryId,
		EventId: req.EventId,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditGetEntry: operator=%d entry_id=%d event_id=%s err=%v",
			callCtx.GetOperatorId(), req.EntryId, req.EventId, err)
		return nil, err
	}
	return &types.AuditEntryResponse{
		Code:    0,
		Message: "ok",
		Data: types.AuditEntryData{
			Entry: auditEntryToAPI(reply.GetEntry()),
			Found: reply.GetFound(),
		},
		TTL: 0,
	}, nil
}
