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

type EndLiveLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：下播（LIVING→READY，场次置为 ENDED）
func NewEndLiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EndLiveLogic {
	return &EndLiveLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// EndLive 主播主动下播的 end_reason 固定为 1（ANCHOR_STOP）：客户端省略时网关补 1，
// 传其它值一律拒绝——禁播（2）、关房（3）、断流超时（4）与补偿（5）只能由服务侧
// 内部方法或 live.state.v1 事件写入，否则终端就能伪造一场直播的终止原因。
func (l *EndLiveLogic) EndLive(req *types.ParamLiveEnd) (resp *types.LiveEndResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：下播重试不能重复计算时长")
	}
	endReason := req.EndReason
	if endReason == 0 {
		endReason = int32(liveroomrpc.EndReason_END_REASON_ANCHOR_STOP)
	}
	if endReason != int32(liveroomrpc.EndReason_END_REASON_ANCHOR_STOP) {
		return nil, errors.New("end_reason 只允许 1（主播主动下播），其余终止原因由服务侧写入")
	}
	reply, err := l.svcCtx.LiveRoom.EndLive(l.ctx, &liveroomrpc.EndLiveReq{
		RoomId:    req.RoomId,
		SessionId: req.SessionId,
		Mid:       req.Mid,
		EndReason: liveroomrpc.EndReason(endReason),
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/endLive: room_id=%d session_id=%d mid=%d err=%v", req.RoomId, req.SessionId, req.Mid, err)
		return nil, err
	}
	return &types.LiveEndResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveEndData{
			SessionId:       reply.GetSessionId(),
			SessionState:    int32(reply.GetSessionState()),
			RoomState:       int32(reply.GetRoomState()),
			DurationSeconds: reply.GetDurationSeconds(),
			Replayed:        reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
