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

type LiveMediaRecordStopLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 停止录制（RECORDING→STOPPING，最后一片落库后 STOPPED 才能拼回放）
func NewLiveMediaRecordStopLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRecordStopLogic {
	return &LiveMediaRecordStopLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRecordStop 聚合 live-media StopLiveRecord。
//
// 停录制与停转码是两条独立的状态机（record 与 transcode 各自的 version 与台账），因此权限点分开
// （live:record:stop 与 live:transcode:stop）。
//
// end_at=0 表示立即停止、>0 表示录到该时刻，两者都是合法哨兵，网关只拒负数。
// 期望区间在 Start 时已落在台账上，这里网关**不**跨请求复算区间（那是 live-media 的数据），
// 只挡住自身形态。reason 必须是具体原因（人工停止 7 MANUAL），能不能停（是否 RECORDING、
// 最后一片是否落库）由服务判定；operator 由会话渲染成 admin:<admin_id>。
func (l *LiveMediaRecordStopLogic) LiveMediaRecordStop(req *types.ParamLiveMediaRecordStop) (resp *types.LiveMediaRecordStopResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	operator, err := liveMediaOperator(l.ctx, "liveMediaRecordStop")
	if err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("record_id", req.RecordId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	if err := liveNonNeg("end_at", req.EndAt); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("reason", req.Reason); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.StopLiveRecord(l.ctx, &livemediarpc.StopLiveRecordReq{
		RecordId:        req.RecordId,
		ExpectedVersion: req.ExpectedVersion,
		EndAt:           req.EndAt,
		Reason:          livemediarpc.FailureReason(req.Reason),
		RequestId:       req.RequestId,
		Operator:        operator,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRecordStop: record_id=%d expected_version=%d end_at=%d reason=%d request_id=%s err=%v",
			req.RecordId, req.ExpectedVersion, req.EndAt, req.Reason, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaRecordStopResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaRecordTaskToAPI(info),
		TTL:     0,
	}, nil
}
