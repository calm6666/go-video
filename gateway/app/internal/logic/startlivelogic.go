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

type StartLiveLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：开播（READY→LIVING 并新建场次，只登记 stream_id 引用）
func NewStartLiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartLiveLogic {
	return &StartLiveLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// StartLive 只推进房间状态机并登记 live-ingest 分配的 stream_id 引用；
// 推流密钥、流健康度与接入节点归 live-ingest，本入口不接收也不签发（AGENTS.md §6/§8）。
// 未通过前置检查的房间由服务拒绝（合法迁移之外不写入），网关不做「先Prepare再Start」的
// 隐式串联——那会把两次幂等语义搅在一起，也让 deny_code 失去可解释性。
func (l *StartLiveLogic) StartLive(req *types.ParamLiveStart) (resp *types.LiveStartResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：开播重试必须回放到同一 session_id")
	}
	reply, err := l.svcCtx.LiveRoom.StartLive(l.ctx, &liveroomrpc.StartLiveReq{
		RoomId:    req.RoomId,
		Mid:       req.Mid,
		StreamId:  req.StreamId,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/startLive: room_id=%d mid=%d request_id=%q err=%v",
			req.RoomId, req.Mid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveStartResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStartData{
			SessionId:    reply.GetSessionId(),
			State:        int32(reply.GetState()),
			StartedAt:    reply.GetStartedAt(),
			StateVersion: reply.GetStateVersion(),
			Replayed:     reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
