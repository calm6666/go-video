// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PrepareLiveLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：开播前置检查（资格与风控由 live-room 经 creator/risk-control RPC 判定）
func NewPrepareLiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrepareLiveLogic {
	return &PrepareLiveLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PrepareLive 只做参数校验与结果透出：主播资格来自 creator、风控来自 risk-control，
// 两项判定都在 live-room 内部完成，网关既不直连它们的库表，也不因为「看起来 ready=true」
// 就跳过服务侧的 PENDING→READY 迁移（AGENTS.md §5/§8）。
// checks 中 degraded=true 表示下游不可用未评估，按未通过原样下发，不美化。
// device_hash / ip_hash 是客户端预哈希摘要，网关绝不用 r.RemoteAddr 合成 ip_hash。
func (l *PrepareLiveLogic) PrepareLive(req *types.ParamLivePrepare) (resp *types.LivePrepareResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：前置检查会推进房间状态，需要幂等键")
	}
	reply, err := l.svcCtx.LiveRoom.PrepareLive(l.ctx, &liveroomrpc.PrepareLiveReq{
		RoomId:     req.RoomId,
		Mid:        req.Mid,
		Platform:   liveroomrpc.Platform(req.Platform),
		DeviceHash: req.DeviceHash,
		IpHash:     req.IpHash,
		RequestId:  req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/prepareLive: room_id=%d mid=%d err=%v", req.RoomId, req.Mid, err)
		return nil, err
	}
	return &types.LivePrepareResponse{
		Code:    0,
		Message: "ok",
		Data: types.LivePrepareData{
			RoomId:            reply.GetRoomId(),
			State:             int32(reply.GetState()),
			Checks:            livePrepareChecksToAPI(reply.GetChecks()),
			Ready:             reply.GetReady(),
			DenyCode:          reply.GetDenyCode(),
			RetryAfterSeconds: reply.GetRetryAfterSeconds(),
			Replayed:          reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
