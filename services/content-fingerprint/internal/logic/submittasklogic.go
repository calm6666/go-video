package logic

import (
	"context"

	"go-video/services/content-fingerprint/internal/svc"
	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitTaskLogic {
	return &SubmitTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SubmitTask 创建指纹任务（PENDING）。
// 本期占位：不调用真实指纹算法，仅持久化任务记录，由后续 Worker 消费 media.task.v1 事件触发实际计算。
// TODO(后续)：接入 media.task.v1 事件消费者派发指纹任务到 Worker。
func (l *SubmitTaskLogic) SubmitTask(in *rpc.SubmitTaskReq) (*rpc.TaskReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if fpTypeToModel(in.FpType) == 0 {
		return nil, model.ErrInvalidFpType
	}
	task := &model.FingerprintTask{
		AssetID: in.AssetId,
		FpType:  fpTypeToModel(in.FpType),
		State:   model.TaskStatePending,
	}
	saved, err := l.svcCtx.Repository.SubmitTask(l.ctx, task)
	if err != nil {
		l.Errorf("content-fingerprint/SubmitTask: asset_id=%d fp_type=%v err=%v",
			in.AssetId, in.FpType, err)
		return nil, err
	}
	return taskToReply(saved), nil
}
