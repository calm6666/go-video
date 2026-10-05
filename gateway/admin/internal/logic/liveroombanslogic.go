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

type LiveRoomBansLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 禁播台账（含运营内部 reason 与解除留痕）
func NewLiveRoomBansLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomBansLogic {
	return &LiveRoomBansLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomBans 聚合 live-room ListRoomBans。
// 本路由在免 AdminPermission 的只读组里，但 operator_mid 仍是硬门槛：禁播台账带 reason 与
// lift_reason（proto 明确不下发终端的运营内部说明），live-room 拒掉无主体的读取（见
// listroombanslogic.go）。主体由后台表单声明、由 live-room 记进查询日志，网关不伪造。
// state 的 0 是「不过滤」哨兵，取值合法性归 live-room。
func (l *LiveRoomBansLogic) LiveRoomBans(req *types.ParamLiveRoomBans) (resp *types.LiveRoomBansResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := requireOperator("operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveNonNeg("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page", req.Page); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.ListRoomBans(l.ctx, &liveroomrpc.ListRoomBansReq{
		RoomId:      req.RoomId,
		Mid:         req.Mid,
		State:       req.State,
		Page:        req.Page,
		PageSize:    req.PageSize,
		OperatorMid: req.OperatorMid,
	})
	if err != nil {
		// 只打主键与主体：reason 正文属运营内部说明，不进日志（AGENTS.md §4）。
		l.Errorf("gateway/admin/liveRoomBans: room_id=%d mid=%d state=%d operator_mid=%d err=%v",
			req.RoomId, req.Mid, req.State, req.OperatorMid, err)
		return nil, err
	}
	return &types.LiveRoomBansResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomBansData{
			List:     liveBansToAPI(reply.GetBans()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetPageSize(),
		},
		TTL: 0,
	}, nil
}
