package logic

import (
	"context"

	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunImageLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRunImageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunImageLogic {
	return &RunImageLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RunImage 执行图像识别。
// 本期占位实现：仅持久化任务并返回空结果，不调用真实算法。
// TODO: 接入图像识别算法（如色情/暴恐/违禁品检测），返回命中标签与置信度。
func (l *RunImageLogic) RunImage(in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
	params, err := prepareRun(l.svcCtx, in, rpc.CapabilityType_CAPABILITY_IMAGE)
	if err != nil {
		l.Errorf("moderation-worker/RunImage: task_id=%s err=%v", in.GetTaskId(), err)
		return nil, err
	}
	return finishRun(l.svcCtx, l.ctx, params, rpc.CapabilityType_CAPABILITY_IMAGE)
}
