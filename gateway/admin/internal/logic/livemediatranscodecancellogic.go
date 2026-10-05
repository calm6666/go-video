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

type LiveMediaTranscodeCancelLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消转码任务（PENDING|STOPPING→CANCELLED 终态，只能重新登记）
func NewLiveMediaTranscodeCancelLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaTranscodeCancelLogic {
	return &LiveMediaTranscodeCancelLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaTranscodeCancel 聚合 live-media CancelLiveTranscode。
//
// 不可逆：CANCELLED 是终态，proto 明确「其它写操作不得复活终态任务」，要再转码只能重新 Start，
// 因此这个动作与 stop / retry 分开授权（live:transcode:cancel），不能合并成一个「操作转码」点。
//
// 入参门槛与 stop 完全同口径（task_id 必填、expected_version=0 表示不校验版本、reason 必须是
// 具体取消原因、operator 由会话渲染）；哪些状态能被取消（PENDING/STOPPING）由 live-media 判定，
// 网关不查台账，也不把下游的拒绝兜成成功。
func (l *LiveMediaTranscodeCancelLogic) LiveMediaTranscodeCancel(req *types.ParamLiveMediaTranscodeCancel) (resp *types.LiveMediaTranscodeCancelResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveMediaOperator(l.ctx, "liveMediaTranscodeCancel")
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
	if err := liveMediaEnum("reason", req.Reason); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.CancelLiveTranscode(l.ctx, &livemediarpc.CancelLiveTranscodeReq{
		TaskId:          req.TaskId,
		ExpectedVersion: req.ExpectedVersion,
		Reason:          livemediarpc.FailureReason(req.Reason),
		RequestId:       req.RequestId,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaTranscodeCancel: task_id=%d expected_version=%d reason=%d request_id=%s err=%v",
			req.TaskId, req.ExpectedVersion, req.Reason, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaTranscodeCancelResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaTranscodeTaskToAPI(info),
		TTL:     0,
	}, nil
}
