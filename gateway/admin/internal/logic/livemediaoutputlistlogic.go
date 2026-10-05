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

type LiveMediaOutputListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 房间当前可分发档位（默认只在线，include_offline 带历史）
func NewLiveMediaOutputListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaOutputListLogic {
	return &LiveMediaOutputListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaOutputList 聚合 live-media ListStreamOutputs。
//
// room_id 是必填主键（proto：「必填：房间维度查询当前档位」），不给时服务无从限定作用域，
// 因此网关直接拒。live_session_id=0 走「只看当前在线档位」分支（服务侧语义 <=0，负数网关先拒），
// include_offline 原样透传：是否把已下线档位带进列表是 live-media 的查询口径，网关不代为补默认值。
func (l *LiveMediaOutputListLogic) LiveMediaOutputList(req *types.ParamLiveMediaOutputList) (resp *types.LiveMediaOutputListResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	page, err := liveMediaPage(req.Pn, req.Ps)
	if err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("live_session_id", req.SessionId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveMedia.ListStreamOutputs(l.ctx, &livemediarpc.ListStreamOutputsReq{
		RoomId:         req.RoomId,
		LiveSessionId:  req.SessionId,
		IncludeOffline: req.IncludeOffline,
		Page:           page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaOutputList: room_id=%d live_session_id=%d include_offline=%v err=%v",
			req.RoomId, req.SessionId, req.IncludeOffline, err)
		return nil, err
	}
	return &types.LiveMediaOutputListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveMediaOutputListData{
			Total: liveMediaPageTotal(reply.GetPage()),
			List:  liveMediaStreamOutputsToAPI(reply.GetOutputs()),
		},
		TTL: 0,
	}, nil
}
