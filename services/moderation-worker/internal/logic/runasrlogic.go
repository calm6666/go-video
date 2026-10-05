package logic

import (
	"context"

	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunASRLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRunASRLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunASRLogic {
	return &RunASRLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RunASR 执行 ASR 语音识别。
// 本期占位实现：仅持久化任务并返回空结果，不调用真实算法。
// TODO: 接入 ASR 算法服务，返回带时间戳的转写文本片段。
func (l *RunASRLogic) RunASR(in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
	params, err := prepareRun(l.svcCtx, in, rpc.CapabilityType_CAPABILITY_ASR)
	if err != nil {
		l.Errorf("moderation-worker/RunASR: task_id=%s err=%v", in.GetTaskId(), err)
		return nil, err
	}
	return finishRun(l.svcCtx, l.ctx, params, rpc.CapabilityType_CAPABILITY_ASR)
}
