// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveSessionListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 场次 cursor 分页（session_id 倒序，next_cursor 空表示到底）
func NewLiveSessionListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveSessionListLogic {
	return &LiveSessionListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveSessionList 聚合 live-room ListSessions。
// room_id 必填（proto 语义：场次只在房间内有序）；游标格式与解析失败一律由 live-room 判定，
// 网关只挡长度，绝不把非法游标「当作第一页」重查——那会让运营看到重复页却以为翻页正常。
// next_cursor 原样回传：它是不透明串，后台只负责带回，不负责解释。
func (l *LiveSessionListLogic) LiveSessionList(req *types.ParamLiveSessionList) (resp *types.LiveSessionListResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
	}
	if err := liveCursor(req.Cursor); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.ListSessions(l.ctx, &liveroomrpc.ListSessionsReq{
		RoomId:   req.RoomId,
		Mid:      req.Mid,
		State:    liveroomrpc.SessionState(req.State),
		Cursor:   req.Cursor,
		PageSize: req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveSessionList: room_id=%d mid=%d state=%d err=%v",
			req.RoomId, req.Mid, req.State, err)
		return nil, err
	}
	return &types.LiveSessionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveSessionListData{
			List:       liveSessionsToAPI(reply.GetSessions()),
			NextCursor: reply.GetNextCursor(),
		},
		TTL: 0,
	}, nil
}
