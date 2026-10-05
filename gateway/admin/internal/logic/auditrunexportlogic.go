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

type AuditRunExportLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 手动推进一个导出任务（正常由 services/cron 驱动，这里是运营兜底）
func NewAuditRunExportLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AuditRunExportLogic {
	return &AuditRunExportLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 推进一个批次：audit 不内置 worker，本方法只把任务往前推一段并回传 finished。
// batch_rows<=0 由 audit 按 AuditExport.BatchRows 默认值兜底，网关不放大批量；
// request_id 必填，让「重复点推进」在 audit 侧可归因到同一次操作。
func (l *AuditRunExportLogic) AuditRunExport(req *types.ParamAuditRunExport) (resp *types.AuditExportRunResponse, err error) {
	if l.svcCtx.Audit == nil {
		return nil, errAuditServiceNotConfigured
	}
	if req.TaskId <= 0 {
		return nil, errors.New("gateway/admin: task_id required")
	}
	callCtx, err := auditCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Audit.RunAuditExportTask(l.ctx, &auditrpc.RunAuditExportTaskReq{
		Ctx:       callCtx,
		TaskId:    req.TaskId,
		BatchRows: req.BatchRows,
	})
	if err != nil {
		l.Errorf("gateway/admin/auditRunExport: operator=%d task_id=%d err=%v",
			callCtx.GetOperatorId(), req.TaskId, err)
		return nil, err
	}
	return &types.AuditExportRunResponse{
		Code:    0,
		Message: "ok",
		Data: types.AuditExportRunData{
			Task:         auditExportToAPI(reply.GetTask()),
			ExportedRows: reply.GetExportedRows(),
			Finished:     reply.GetFinished(),
		},
		TTL: 0,
	}, nil
}
