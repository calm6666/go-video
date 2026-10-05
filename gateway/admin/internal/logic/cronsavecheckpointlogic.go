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

type CronSaveCheckpointLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 独立推进增量游标（expected_version CAS；值单调性由处理器保证）
func NewCronSaveCheckpointLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CronSaveCheckpointLogic {
	return &CronSaveCheckpointLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CronSaveCheckpoint 聚合 cron SaveCheckpoint。
// expected_version 是唯一的并发保护：0 表示「要求尚不存在」，>0 走 CAS，冲突由 cron 判定；
// checkpoint.version 与 operator 由服务端维护，客户端声明即留痕并覆盖。
// 游标值的单调性（是否倒退）属处理器与 cron 的规则，网关不复算。
func (l *CronSaveCheckpointLogic) CronSaveCheckpoint(req *types.ParamCronSaveCheckpoint) (resp *types.CronSaveCheckpointResponse, err error) {
	if l.svcCtx.Cron == nil {
		return nil, errCronServiceNotConfigured
	}
	if req == nil {
		return nil, errCronRequestMissing
	}
	operator, err := cronOperator(l.ctx)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := cronVersion(req.ExpectedVersion); err != nil {
		return nil, err
	}
	checkpoint, err := cronCheckpointForSave(l.ctx, req.Checkpoint, operator)
	if err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Cron.SaveCheckpoint(l.ctx, &cronrpc.SaveCheckpointReq{
		Checkpoint:      checkpoint,
		ExpectedVersion: req.ExpectedVersion,
		IdempotencyKey:  req.IdempotencyKey,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/cronSaveCheckpoint: task_key=%s scope_key=%s expected_version=%d idempotency_key=%s operator=%s err=%v",
			checkpoint.GetTaskKey(), checkpoint.GetScopeKey(), req.ExpectedVersion, req.IdempotencyKey, operator, err)
		return nil, err
	}
	return &types.CronSaveCheckpointResponse{
		Code:    0,
		Message: "ok",
		Data: types.CronSaveCheckpointData{
			Checkpoint: cronCheckpointToAPI(reply.GetCheckpoint()),
			Advanced:   reply.GetAdvanced(),
		},
		TTL: 0,
	}, nil
}
