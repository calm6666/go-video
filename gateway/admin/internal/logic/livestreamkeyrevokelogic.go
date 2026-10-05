// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveingestrpc "go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveStreamKeyRevokeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 吊销推流密钥（不可逆终态，可按需级联停止进行中的流）
func NewLiveStreamKeyRevokeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamKeyRevokeLogic {
	return &LiveStreamKeyRevokeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamKeyRevoke 聚合 live-ingest RevokeStreamKey（密钥泄露/禁播/风控处置）。
//
// admin 固定 true：只有运营侧能吊销他人密钥，主播侧的自助吊销留在 gateway/app；
// 本路由在 AdminPermission 组里，没有会话身份到不了 logic。
//
// 本路由是密钥泄露的唯一后台处置面（RotateStreamKey 因响应带明文密钥与含明文的推流地址而不开面）：
// 吊销 + 主播端重新签发即可闭环，网关不需要、也不开「把新明文取回来」的口子。
//
// stop_stream 是运营的选择（级联停流会让直播立刻中断），网关不代填 true 也不代填 false；
// 吊销是否被接受、宽限期与轮转链如何处理由 live-ingest 判定。
func (l *LiveStreamKeyRevokeLogic) LiveStreamKeyRevoke(req *types.ParamLiveStreamKeyRevoke) (resp *types.LiveStreamKeyRevokeResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveStreamKeyRevoke", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("key_id", req.KeyId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.RevokeStreamKey(l.ctx, &liveingestrpc.RevokeStreamKeyReq{
		KeyId:       req.KeyId,
		OperatorMid: req.OperatorMid,
		Admin:       true, // 固定：运营侧吊销，不接受表单声明
		StopStream:  req.StopStream,
		Reason:      req.Reason,
		RequestId:   req.RequestId,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamKeyRevoke: key_id=%d operator_mid=%d stop_stream=%t request_id=%s err=%v",
			req.KeyId, req.OperatorMid, req.StopStream, req.RequestId, err)
		return nil, err
	}
	stopped := make([]string, 0, len(reply.GetStoppedStreamIds()))
	stopped = append(stopped, reply.GetStoppedStreamIds()...)
	return &types.LiveStreamKeyRevokeResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamKeyRevokeData{
			State:            int32(reply.GetState()),
			StoppedStreamIds: stopped,
			Replayed:         reply.GetReplayed(),
			Message:          reply.GetMessage(),
		},
		TTL: 0,
	}, nil
}
