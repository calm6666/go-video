package logic

import (
	"context"

	"go-video/services/moderation-orchestrator/internal/svc"
	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetResultLogic {
	return &GetResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询审核结论。
func (l *GetResultLogic) GetResult(in *rpc.ResultReq) (*rpc.ResultReply, error) {
	if in.TaskId <= 0 {
		return nil, model.ErrInvalidTaskID
	}
	res, err := l.svcCtx.Repository.GetResult(l.ctx, in.TaskId)
	if err != nil {
		l.Errorf("moderation/GetResult: task=%d err=%v", in.TaskId, err)
		return nil, err
	}
	return &rpc.ResultReply{Result: resultToRPC(res)}, nil
}
