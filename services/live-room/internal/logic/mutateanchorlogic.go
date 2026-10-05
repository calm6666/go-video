package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MutateAnchorLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMutateAnchorLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MutateAnchorLogic {
	return &MutateAnchorLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 绑定或解绑主播（房主/联合主播/房管），含单主播房间数上限校验
//
// 房主行永不在此解绑，BIND(role=OWNER) 也一律拒绝：换房主必须走 model.TransferOwner
// （同事务先释放占位再绑定，杜绝无房主窗口），而 rpc.MutateAnchor 里没有这个动作位，
// 已作为契约缺口登记。
// 房间在播时禁止解绑当前开播主播：否则 active_session_id 指向一个不再是主播的人，
// EndLive 的归属校验会永久失败。
func (l *MutateAnchorLogic) MutateAnchor(in *rpc.MutateAnchorReq) (*rpc.MutateAnchorReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkOperator(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	if err := checkMid(in.GetTargetMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	action := int32(in.GetAction())
	if !validAnchorAction(action) {
		return nil, fmt.Errorf("%w: action=%d", model.ErrAnchorActionInvalid, action)
	}
	room, err := l.svcCtx.Rooms.FindOne(l.ctx, in.GetRoomId())
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, model.ErrRoomNotFound
	}
	if model.RoomStateIsTerminal(room.State) {
		return nil, model.ErrRoomFinished
	}
	// 契约里没有 admin 位：成员变更只能由生效房主发起（缺口见 README）。
	owner, err := l.svcCtx.Anchors.FindOwner(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	if owner == nil || owner.Mid != in.GetOperatorMid() {
		return nil, fmt.Errorf("%w: operator_mid=%d 不是该房间生效房主", model.ErrAnchorForbidden, in.GetOperatorMid())
	}
	role, err := anchorRoleForAction(in.GetRole(), action)
	if err != nil {
		return nil, err
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcMutateAnchor, reqID, model.IdempotencyKindRequest,
		room.RoomID, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcMutateAnchor, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.MutateAnchorReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	bound := model.BindStateEnabled
	if action == int32(rpc.AnchorAction_ANCHOR_ACTION_UNBIND) {
		if err := l.guardLivingAnchor(room, in.GetTargetMid()); err != nil {
			return nil, err
		}
		if _, err := l.svcCtx.Anchors.Unbind(l.ctx, room.RoomID, in.GetTargetMid(), role); err != nil {
			return nil, err
		}
		bound = model.BindStateDisabled
	} else {
		if err := l.checkBindQuota(room.RoomID, in.GetTargetMid(), role); err != nil {
			return nil, err
		}
		if _, err := l.svcCtx.Anchors.Bind(l.ctx, room.RoomID, in.GetTargetMid(), role); err != nil {
			// ErrDuplicateOwner 说明房主占位被别人持有：并发下已有新房主，直接透出不覆盖。
			return nil, err
		}
	}

	// bound_count 回的是「该主播当前生效绑定数」，客户端据此展示上限占用。
	// role 为 0（UNBIND 全部非房主）时 model 按「全部角色」统计，与解绑范围一致。
	cnt, err := l.svcCtx.Anchors.CountActiveRoomsByMid(l.ctx, in.GetTargetMid(), role)
	if err != nil {
		return nil, err
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	reply := &rpc.MutateAnchorReply{
		RoomId:     room.RoomID,
		TargetMid:  in.GetTargetMid(),
		State:      bound,
		BoundCount: int32(cnt),
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// validAnchorAction 只接受 BIND/UNBIND：UNSPECIFIED 与契约之外的取值一律拒绝。
func validAnchorAction(action int32) bool {
	return action == int32(rpc.AnchorAction_ANCHOR_ACTION_BIND) ||
		action == int32(rpc.AnchorAction_ANCHOR_ACTION_UNBIND)
}

// anchorRoleForAction 按动作归一角色：BIND 必须给出具体角色且不得是房主；
// UNBIND 允许 0（解绑该 mid 全部非房主角色），但显式 OWNER 一律拒绝。
func anchorRoleForAction(r rpc.AnchorRole, action int32) (int32, error) {
	role := int32(r)
	if action == int32(rpc.AnchorAction_ANCHOR_ACTION_UNBIND) {
		if role == model.AnchorRoleOwner {
			return 0, model.ErrCannotUnbindOwner
		}
		if role != model.AnchorRoleUnspecified && !model.ValidAnchorRole(role) {
			return 0, fmt.Errorf("%w: role=%d", model.ErrAnchorRoleInvalid, role)
		}
		return role, nil
	}
	if !model.ValidAnchorRole(role) {
		return 0, fmt.Errorf("%w: role=%d", model.ErrAnchorRoleInvalid, role)
	}
	if role == model.AnchorRoleOwner {
		return 0, fmt.Errorf("%w: 房主绑定由 CreateRoom 建立，移交需 TransferOwner（本 RPC 不支持）",
			model.ErrAnchorRoleInvalid)
	}
	return role, nil
}

// checkBindQuota 房间维度与主播维度两道上限。
func (l *MutateAnchorLogic) checkBindQuota(roomID, targetMid int64, role int32) error {
	maxPerRoom := l.svcCtx.Config.LiveRoom.MaxCohostPerRoom
	if role == model.AnchorRoleManager {
		maxPerRoom = l.svcCtx.Config.LiveRoom.MaxManagerPerRoom
	}
	if maxPerRoom > 0 {
		cnt, err := l.svcCtx.Anchors.Count(l.ctx, model.AnchorListQuery{
			RoomID: roomID, Role: role, OnlyEnabled: true,
		})
		if err != nil {
			return err
		}
		// 已生效的重复绑定按幂等处理（Bind 会把该行置回生效），不占用新额度。
		cur, err := l.svcCtx.Anchors.Find(l.ctx, roomID, targetMid, role)
		if err != nil {
			return err
		}
		if cur == nil || cur.State != model.BindStateEnabled {
			if cnt >= int64(maxPerRoom) {
				return fmt.Errorf("%w: room %d role %d 已有 %d 个生效绑定，上限 %d",
					model.ErrAnchorLimitExceeded, roomID, role, cnt, maxPerRoom)
			}
		}
	}
	maxRooms := l.svcCtx.Config.LiveRoom.MaxOwnerBindingsPerMid
	if maxRooms > 0 {
		cnt, err := l.svcCtx.Anchors.CountActiveRoomsByMid(l.ctx, targetMid, model.AnchorRoleUnspecified)
		if err != nil {
			return err
		}
		rooms, err := l.svcCtx.Anchors.ListRoomsByMid(l.ctx, targetMid, model.AnchorRoleUnspecified, maxRooms+1)
		if err != nil {
			return err
		}
		alreadyIn := false
		for _, id := range rooms {
			if id == roomID {
				alreadyIn = true
				break
			}
		}
		if !alreadyIn && cnt >= int64(maxRooms) {
			return fmt.Errorf("%w: mid=%d 已绑定 %d 个房间，上限 %d",
				model.ErrAnchorLimitExceeded, targetMid, cnt, maxRooms)
		}
	}
	return nil
}

// guardLivingAnchor 在播场次的主播不可被解绑。
func (l *MutateAnchorLogic) guardLivingAnchor(room *model.LiveRoom, targetMid int64) error {
	if room.State != model.RoomStateLiving || room.ActiveSessionID <= 0 {
		return nil
	}
	session, err := l.svcCtx.Sessions.FindOne(l.ctx, room.ActiveSessionID)
	if err != nil {
		return err
	}
	if session != nil && session.Mid == targetMid && !model.SessionStateIsTerminal(session.State) {
		return fmt.Errorf("%w: 主播 %d 正在场次 %d 中开播", model.ErrAnchorForbidden, targetMid, session.SessionID)
	}
	return nil
}
