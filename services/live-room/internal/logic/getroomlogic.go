package logic

import (
	"context"
	"fmt"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRoomLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRoomLogic {
	return &GetRoomLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 读房间（按 room_id 或房主 mid），可附带配置与进行中场次
//
// 不允许「空条件查询」：两个键都为 0 时返回 ErrInvalidRoomID，
// 返回空 reply 等于让 gateway 去猜是「没找到」还是「参数没传」。
// 缓存只在纯房间读时生效（with_setting / with_active_session 必须回源，
// 否则会把「本轮没查」误答成「没有」）。
func (l *GetRoomLogic) GetRoom(in *rpc.GetRoomReq) (*rpc.GetRoomReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	roomID := in.GetRoomId()
	if roomID <= 0 && in.GetOwnerMid() <= 0 {
		return nil, model.ErrInvalidRoomID
	}
	plain := !in.GetWithSetting() && !in.GetWithActiveSession()

	var (
		room *model.LiveRoom
		err  error
	)
	if roomID > 0 {
		if plain {
			if info := cachedRoom(l.ctx, l.svcCtx, roomID); info != nil {
				return &rpc.GetRoomReply{Room: info}, nil
			}
		}
		room, err = l.svcCtx.Rooms.FindOne(l.ctx, roomID)
	} else {
		// 桩注释里的 Anchors.FindOwner 需要 room_id 人参，按 owner_mid 反查只能走
		// live_room.owner_mid 投影（ListByOwner 已排除 FINISHED）。桩注释与契约不符，
		// 已作为契约缺口登记。
		room, err = l.roomByOwner(in.GetOwnerMid())
	}
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, model.ErrRoomNotFound
	}

	reply := &rpc.GetRoomReply{Room: roomInfo(room)}
	if plain {
		cacheRoom(l.ctx, l.svcCtx, reply.GetRoom())
		return reply, nil
	}
	if in.GetWithSetting() {
		row, err := l.svcCtx.Settings.FindOne(l.ctx, room.RoomID)
		if err != nil {
			return nil, err
		}
		// 无配置行必须套服务端默认：客户端把缺行读成「全部功能关闭」是错误结论。
		reply.Setting = settingInfo(row)
	}
	if in.GetWithActiveSession() {
		session, err := l.svcCtx.Sessions.FindActiveByRoom(l.ctx, room.RoomID)
		if err != nil {
			return nil, err
		}
		if session != nil {
			reply.ActiveSession = sessionInfo(session)
		}
	}
	return reply, nil
}

// roomByOwner 按房主取其最新一个生效房间（room_id 倒序首条）。
func (l *GetRoomLogic) roomByOwner(ownerMid int64) (*model.LiveRoom, error) {
	if err := checkMid(ownerMid); err != nil {
		return nil, err
	}
	rows, err := l.svcCtx.Rooms.ListByOwner(l.ctx, ownerMid, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: owner_mid=%d", model.ErrRoomNotFound, ownerMid)
	}
	return rows[0], nil
}
