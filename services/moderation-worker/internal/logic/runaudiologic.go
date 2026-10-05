package logic

import (
	"context"

	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunAudioLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRunAudioLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunAudioLogic {
	return &RunAudioLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RunAudio 执行音频识别。
// 本期占位实现：仅持久化任务并返回空结果，不调用真实算法。
// TODO: 接入音频识别算法（如声纹/违禁语音/ASR 风险片段），返回命中标签与时间片段。
func (l *RunAudioLogic) RunAudio(in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
	params, err := prepareRun(l.svcCtx, in, rpc.CapabilityType_CAPABILITY_AUDIO)
	if err != nil {
		l.Errorf("moderation-worker/RunAudio: task_id=%s err=%v", in.GetTaskId(), err)
		return nil, err
	}
	return finishRun(l.svcCtx, l.ctx, params, rpc.CapabilityType_CAPABILITY_AUDIO)
}
