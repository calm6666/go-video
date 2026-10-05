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

type MutateLiveAnchorLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：绑定或解绑主播/房管（房主房间数上限由服务校验）
func NewMutateLiveAnchorLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MutateLiveAnchorLogic {
	return &MutateLiveAnchorLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MutateLiveAnchor 绑定动作只允许 1（绑定/重新启用）与 2（解绑）：
// 房主转让与角色合法性、单主播房间数上限由 live-room 判定，网关不预览也不改写 bound_count。
// 连麦嘉宾（COHOST）只是房间协作关系，不含任何分成语义（AGENTS.md §2 商业化范围外）。
func (l *MutateLiveAnchorLogic) MutateLiveAnchor(req *types.ParamLiveAnchorMutate) (resp *types.LiveAnchorMutateResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：绑定/解绑重试不能产生两条生效绑定")
	}
	if req.Action != int32(liveroomrpc.AnchorAction_ANCHOR_ACTION_BIND) &&
		req.Action != int32(liveroomrpc.AnchorAction_ANCHOR_ACTION_UNBIND) {
		return nil, errors.New("action 只允许 1（绑定或重新启用）或 2（解绑）")
	}
	reply, err := l.svcCtx.LiveRoom.MutateAnchor(l.ctx, &liveroomrpc.MutateAnchorReq{
		RoomId:      req.RoomId,
		OperatorMid: req.OperatorMid,
		TargetMid:   req.TargetMid,
		Action:      liveroomrpc.AnchorAction(req.Action),
		Role:        liveroomrpc.AnchorRole(req.Role),
		RequestId:   req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/mutateLiveAnchor: room_id=%d operator_mid=%d target_mid=%d action=%d err=%v",
			req.RoomId, req.OperatorMid, req.TargetMid, req.Action, err)
		return nil, err
	}
	return &types.LiveAnchorMutateResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAnchorMutateData{
			RoomId:     reply.GetRoomId(),
			TargetMid:  reply.GetTargetMid(),
			State:      reply.GetState(),
			BoundCount: reply.GetBoundCount(),
			Replayed:   reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
