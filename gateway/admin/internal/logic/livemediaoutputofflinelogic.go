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

type LiveMediaOutputOfflineLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 下线一个档位（断流/到期/人工；只影响观众侧可用性，与回放发布状态无关）
func NewLiveMediaOutputOfflineLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaOutputOfflineLogic {
	return &LiveMediaOutputOfflineLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaOutputOffline 聚合 live-media OfflineStreamOutput。
//
// 这是观众侧可用性动作：下线后该档位不再出现在分发列表里，但它**不**推进任何稿件状态
// （回放是否可看由 video 侧决定），所以权限点是 live:output:offline 而不是挂到回放域。
//
// 寻址二选一：output_id，或 (room_id, bitrate_level, protocol) 三元组 —— 两者都不全时服务
// 无从定位是哪一路（回给后台的是「输出不存在」而不是「你少传了参数」），网关先拒。
// reason 必须是具体下线原因（源流丢失/到期/人工），0=UNSPECIFIED 会让档位「无声消失」。
// 本方法没有 operator 位，谁下的线落在网关日志（缺口见 admin.api 与 README）。
func (l *LiveMediaOutputOfflineLogic) LiveMediaOutputOffline(req *types.ParamLiveMediaOutputOffline) (resp *types.LiveMediaOutputOfflineResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveMediaSessionGate(l.ctx, "liveMediaOutputOffline"); err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveMediaOutputSubjectGate(req.OutputId, req.RoomId, req.BitrateLevel, req.Protocol); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("reason", req.Reason); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.OfflineStreamOutput(l.ctx, &livemediarpc.OfflineStreamOutputReq{
		OutputId:     req.OutputId,
		RoomId:       req.RoomId,
		BitrateLevel: livemediarpc.BitrateLevel(req.BitrateLevel),
		Protocol:     livemediarpc.StreamProtocol(req.Protocol),
		Reason:       livemediarpc.FailureReason(req.Reason),
		RequestId:    req.RequestId,
		TraceId:      req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaOutputOffline: output_id=%d room_id=%d bitrate_level=%d protocol=%d request_id=%s err=%v",
			req.OutputId, req.RoomId, req.BitrateLevel, req.Protocol, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaOutputOfflineResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaStreamOutputToAPI(info),
		TTL:     0,
	}, nil
}
