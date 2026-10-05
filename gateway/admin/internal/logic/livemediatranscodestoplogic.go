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

type LiveMediaTranscodeStopLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 请求停止转码（RUNNING→STOPPING，Worker 收尾后 STOPPED）
func NewLiveMediaTranscodeStopLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaTranscodeStopLogic {
	return &LiveMediaTranscodeStopLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaTranscodeStop 聚合 live-media StopLiveTranscode。
//
// 与 Cancel 分开授权：stop 只是让运行中的任务收尾（RUNNING→STOPPING→STOPPED，Worker 仍是执行体），
// cancel 是放弃任务且不可复活，两者的运营后果不同（见 routePermissions 的 live:transcode:stop /
// :cancel 两点）。
//
// expected_version=0 是「不校验版本」的合法哨兵（仍受状态机约束），网关不代为读取当前版本、
// 也不复算冲突；reason 必须是具体停止原因（人工停止是 7 MANUAL），0=UNSPECIFIED 会让台账里出现
// 「无原因停止」，事后无法解释，因此在枚举位上直接拒。operator 由会话渲染成 admin:<admin_id>，
// 表单不声明；能不能停（当前状态是否 RUNNING）由 live-media 判定，网关原样转达下游错误。
func (l *LiveMediaTranscodeStopLogic) LiveMediaTranscodeStop(req *types.ParamLiveMediaTranscodeStop) (resp *types.LiveMediaTranscodeStopResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveMediaOperator(l.ctx, "liveMediaTranscodeStop")
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
	info, err := l.svcCtx.LiveMedia.StopLiveTranscode(l.ctx, &livemediarpc.StopLiveTranscodeReq{
		TaskId:          req.TaskId,
		ExpectedVersion: req.ExpectedVersion,
		Reason:          livemediarpc.FailureReason(req.Reason),
		RequestId:       req.RequestId,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaTranscodeStop: task_id=%d expected_version=%d reason=%d request_id=%s err=%v",
			req.TaskId, req.ExpectedVersion, req.Reason, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaTranscodeStopResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaTranscodeTaskToAPI(info),
		TTL:     0,
	}, nil
}
