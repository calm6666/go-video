// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	cronrpc "go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CronGetTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个任务定义（含 next_fire_at/last_error 与乐观锁 version）
func NewCronGetTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronGetTaskLogic {
	return &CronGetTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronGetTask 聚合 cron GetTask。
// definition 为空按 found=false 表达「未注册」而不是错误（.api 注释口径），
// 后台据此区分「任务不存在」与「cron 不可用」；version 必须回传，它是后续
// Update/Pause/Resume/Disable 的乐观锁入参。
func (l *CronGetTaskLogic) CronGetTask(req *types.ParamCronGetTask) (resp *types.CronTaskResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := requireNonEmpty("task_key", req.TaskKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.GetTask(l.ctx, &cronrpc.GetTaskReq{TaskKey: req.TaskKey})
	if err != nil {
		l.Errorf("gateway/admin/cronGetTask: task_key=%s err=%v", req.TaskKey, err)
		return nil, err
	}
	def := reply.GetDefinition()
	return &types.CronTaskResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronTaskData{
			Definition: cronTaskToAPI(def),
			Found:      def != nil,
		},
		TTL: 0,
	}, nil
}
