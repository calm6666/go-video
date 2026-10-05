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

type LiveRoomSettingUpdateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 改直播配置（整段覆盖；live-room 无运营主体位，只认生效房主，见类型注释）
func NewLiveRoomSettingUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomSettingUpdateLogic {
	return &LiveRoomSettingUpdateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomSettingUpdate 聚合 live-room UpdateRoomSetting。
// 诚实转发，不伪造房主：UpdateRoomSettingReq 里没有运营主体位，live-room 只认「生效房主」
// （updateroomsettinglogic.go 比对 Anchors.FindOwner），因此本路由实际只在 operator_mid
// 恰为该房间生效房主时成功，否则原样上抛 ErrAnchorNotOwner。
// 网关既不把会话 admin_id 塞进 operator_mid（编号空间不同，那是一次彻底的错误归因），
// 也不代主播签发身份；契约补齐（增加 admin 位）之前，这条路由的可用性以 live-room 的判定为准。
// setting 为整段覆盖：go-zero 的必填 nested struct 保证「漏传 setting」在参数解析阶段就被拒，
// 不会出现一次漏发把房间功能全关成不可用。
func (l *LiveRoomSettingUpdateLogic) LiveRoomSettingUpdate(req *types.ParamLiveRoomSettingUpdate) (resp *types.LiveRoomSettingUpdateResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveRoomSettingUpdate", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("live_type", req.Setting.LiveType); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("min_client_version_code", req.Setting.MinClientVersionCode); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.UpdateRoomSetting(l.ctx, &liveroomrpc.UpdateRoomSettingReq{
		RoomId:      req.RoomId,
		OperatorMid: req.OperatorMid,
		Setting:     liveSettingForRPC(req.Setting),
		RequestId:   req.RequestId,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveRoomSettingUpdate: room_id=%d operator_mid=%d live_type=%d request_id=%s err=%v",
			req.RoomId, req.OperatorMid, req.Setting.LiveType, req.RequestId, err)
		return nil, err
	}
	return &types.LiveRoomSettingUpdateResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomSettingUpdateData{
			Setting:  liveSettingToAPI(reply.GetSetting()),
			Replayed: reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
