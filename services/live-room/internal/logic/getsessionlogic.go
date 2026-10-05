package logic

import (
	"context"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetSessionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetSessionLogic {
	return &GetSessionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 读场次（按 session_id，或按 room_id 取最近 N 场之一）
//
// 查不到返回 ErrSessionNotFound：回一个零值 SessionInfo 等于告诉客户端「有一场没标题没状态的直播」。
// record_id/record_asset_id/record_aid 只是引用，播放地址由 live-media 签发，本服务不拼 URL。
func (l *GetSessionLogic) GetSession(in *rpc.GetSessionReq) (*rpc.GetSessionReply, error) {
	if in == nil {
		return nil, model.ErrInvalidSessionID
	}
	var (
		session *model.LiveSession
		err     error
	)
	switch {
	case in.GetSessionId() > 0:
		session, err = l.svcCtx.Sessions.FindOne(l.ctx, in.GetSessionId())
	case in.GetRoomId() > 0:
		if in.GetOffset() < 0 {
			return nil, model.ErrOffsetInvalid
		}
		session, err = l.svcCtx.Sessions.FindLatest(l.ctx, in.GetRoomId(), in.GetOffset())
	default:
		return nil, model.ErrInvalidSessionID
	}
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, model.ErrSessionNotFound
	}
	return &rpc.GetSessionReply{Session: sessionInfo(session)}, nil
}
