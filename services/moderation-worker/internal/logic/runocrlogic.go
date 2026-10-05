package logic

import (
	"context"

	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunOCRLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRunOCRLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunOCRLogic {
	return &RunOCRLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RunOCR 执行 OCR 文本识别。
// 本期占位实现：仅持久化任务并返回空结果，不调用真实算法。
// TODO: 接入 OCR 算法服务/SDK，返回文本片段（含 bbox、置信度）。
func (l *RunOCRLogic) RunOCR(in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
	params, err := prepareRun(l.svcCtx, in, rpc.CapabilityType_CAPABILITY_OCR)
	if err != nil {
		l.Errorf("moderation-worker/RunOCR: task_id=%s err=%v", in.GetTaskId(), err)
		return nil, err
	}
	return finishRun(l.svcCtx, l.ctx, params, rpc.CapabilityType_CAPABILITY_OCR)
}
