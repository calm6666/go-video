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

type AuditGetExportLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询导出任务（含短期签名下载地址与到期时间）
func NewAuditGetExportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditGetExportLogic {
	return &AuditGetExportLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 取导出任务与下载地址。task_id / request_id 至少一个，避免无条件列表化。
// 网关以 TTL=0 返回：签名地址是短期凭证（audit 按 url_expire_at 控制有效期），
// 任何一层缓存都会让地址在过期后仍被复用（AGENTS.md §6）。
func (l *AuditGetExportLogic) AuditGetExport(req *types.ParamAuditGetExport) (resp *types.AuditExportDetailResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	if req.TaskId <= 0 && req.RequestId == "" {
		return nil, errors.New("gateway/admin: task_id or request_id required")
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Audit.GetAuditExport(l.ctx, &auditrpc.GetAuditExportReq{
		Ctx:       callCtx,
		TaskId:    req.TaskId,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditGetExport: operator=%d task_id=%d err=%v",
			callCtx.GetOperatorId(), req.TaskId, err)
		return nil, err
	}
	return &types.AuditExportDetailResponse{
		Code:    0,
		Message: "ok",
		Data: types.AuditExportDetailData{
			Task:        auditExportToAPI(reply.GetTask()),
			DownloadUrl: reply.GetDownloadUrl(),
			UrlExpireAt: reply.GetUrlExpireAt(),
		},
		TTL: 0,
	}, nil
}
