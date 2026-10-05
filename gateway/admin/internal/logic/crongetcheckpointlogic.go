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

type CronGetCheckpointLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个游标（未推进过返回 found=false 而不是错误）
func NewCronGetCheckpointLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronGetCheckpointLogic {
	return &CronGetCheckpointLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronGetCheckpoint 聚合 cron GetCheckpoint。
// scope_key 空串表示默认游标，是合法值，不能当缺参拦截；
// found=false 表示从未推进过水位，此时 checkpoint 字段无效（网关投影成零值而非报错）。
func (l *CronGetCheckpointLogic) CronGetCheckpoint(req *types.ParamCronGetCheckpoint) (resp *types.CronCheckpointResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	if err := requireNonEmpty("task_key", req.TaskKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.GetCheckpoint(l.ctx, &cronrpc.GetCheckpointReq{
		TaskKey:  req.TaskKey,
		ScopeKey: req.ScopeKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronGetCheckpoint: task_key=%s scope_key=%s err=%v", req.TaskKey, req.ScopeKey, err)
		return nil, err
	}
	return &types.CronCheckpointResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronCheckpointData{
			Checkpoint: cronCheckpointToAPI(reply.GetCheckpoint()),
			Found:      reply.GetFound(),
		},
		TTL: 0,
	}, nil
}
