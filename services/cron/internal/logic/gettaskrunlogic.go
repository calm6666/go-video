package logic

import (
	"context"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTaskRunLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTaskRunLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTaskRunLogic {
	return &GetTaskRunLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单条执行记录。
func (l *GetTaskRunLogic) GetTaskRun(in *rpc.GetTaskRunReq) (*rpc.GetTaskRunReply, error) {
	if in == nil || in.RunId <= 0 {
		return nil, model.ErrRunNotFound
	}
	r, err := l.svcCtx.Runs.FindOne(l.ctx, in.RunId)
	if err != nil {
		l.Errorf("GetTaskRun failed, run_id=%d", in.RunId)
		return nil, err
	}
	if r == nil {
		return nil, model.ErrRunNotFound
	}
	// lease_owner/fence_token/next_retry_at 一并透出：运维要能看清「谁在跑、还能跑多久」。
	return &rpc.GetTaskRunReply{Run: runRecord(r)}, nil
}
