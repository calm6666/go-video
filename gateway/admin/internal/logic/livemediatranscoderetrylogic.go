// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveMediaTranscodeRetryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重试失败的转码任务（FAILED→PENDING，attempt+1，受 max_attempts 限制）
func NewLiveMediaTranscodeRetryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaTranscodeRetryLogic {
	return &LiveMediaTranscodeRetryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaTranscodeRetry 聚合 live-media RetryLiveTranscode。
//
// Retry 与 Stop/Cancel 是三种不同后果的动作，各自占权限点：Retry 是把已失败的不可用任务重新放回
// 队列（会再次拉起 FFmpeg、再次产生成本），因此这里的 reason 是**字符串说明**而不是枚举 ——
// 网关只要求它非空（「为什么重开」必须留得下来），内容是否充分由服务与审计判。
//
// 能不能重试（状态是否 FAILED、attempt 是否已到 max_attempts）完全由 live-media 判定，
// 网关不查台账也不复算；同 request_id 重放不会重复 ++attempt，这是契约给的幂等保证。
func (l *LiveMediaTranscodeRetryLogic) LiveMediaTranscodeRetry(req *types.ParamLiveMediaTranscodeRetry) (resp *types.LiveMediaTranscodeRetryResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveMediaOperator(l.ctx, "liveMediaTranscodeRetry")
	if err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("task_id", req.TaskId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.RetryLiveTranscode(l.ctx, &livemediarpc.RetryLiveTranscodeReq{
		TaskId:          req.TaskId,
		ExpectedVersion: req.ExpectedVersion,
		Reason:          req.Reason,
		RequestId:       req.RequestId,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaTranscodeRetry: task_id=%d expected_version=%d request_id=%s err=%v",
			req.TaskId, req.ExpectedVersion, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaTranscodeRetryResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaTranscodeTaskToAPI(info),
		TTL:     0,
	}, nil
}
