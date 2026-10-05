package logic

import (
	"context"
	"strings"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetCheckpointLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetCheckpointLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetCheckpointLogic {
	return &GetCheckpointLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个游标。
func (l *GetCheckpointLogic) GetCheckpoint(in *rpc.GetCheckpointReq) (*rpc.GetCheckpointReply, error) {
	if in == nil {
		return nil, model.ErrTaskKeyEmpty
	}
	taskKey := strings.TrimSpace(in.TaskKey)
	if taskKey == "" {
		return nil, model.ErrTaskKeyEmpty
	}
	c, err := l.svcCtx.Checkpoints.FindOne(l.ctx, taskKey, in.ScopeKey)
	if err != nil {
		l.Errorf("GetCheckpoint failed, task_key=%s scope_key=%s", taskKey, in.ScopeKey)
		return nil, err
	}
	if c == nil {
		// 「从未推进过游标」是增量任务的正常起点，不是错误：用 found=false 表达。
		return &rpc.GetCheckpointReply{Found: false}, nil
	}
	return &rpc.GetCheckpointReply{Checkpoint: checkpointInfo(c), Found: true}, nil
}
