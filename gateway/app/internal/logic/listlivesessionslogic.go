// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLiveSessionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 观众面：历史场次 cursor 分页
func NewListLiveSessionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLiveSessionsLogic {
	return &ListLiveSessionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListLiveSessions 游标由服务生成（session_id 倒序位点），网关不解析也不自造 next_cursor；
// 空 next_cursor 表示到底。回放状态是 live-media 推进的投影，网关只透出 replay_state。
func (l *ListLiveSessionsLogic) ListLiveSessions(req *types.ParamLiveSessions) (resp *types.LiveSessionsResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	reply, err := l.svcCtx.LiveRoom.ListSessions(l.ctx, &liveroomrpc.ListSessionsReq{
		RoomId:   req.RoomId,
		Mid:      req.Mid,
		State:    liveroomrpc.SessionState(req.State),
		Cursor:   req.Cursor,
		PageSize: req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/app/listLiveSessions: room_id=%d cursor=%q err=%v", req.RoomId, req.Cursor, err)
		return nil, err
	}
	return &types.LiveSessionsResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveSessionsData{
			Sessions:   liveSessionsToAPI(reply.GetSessions()),
			NextCursor: reply.GetNextCursor(),
		},
		TTL: 0,
	}, nil
}
