package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	creatorrpc "go-video/services/creator/rpc"
	riskrpc "go-video/services/risk-control/rpc"
)

type PrepareLiveLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPrepareLiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrepareLiveLogic {
	return &PrepareLiveLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 开播前置检查：主播资格(creator) + 风控(risk-control) + 资料审核 + 未禁播，全通过才 PENDING→READY
//
// checks 顺序固定为 anchor_qualification / risk_control / room_verified / not_banned / setting_ok，
// 客户端可以按同一顺序渲染。任何「无法评估」都记成 passed=false + degraded=true：
// 静默放行等于把风控当可选件（AGENTS.md §9）。
func (l *PrepareLiveLogic) PrepareLive(in *rpc.PrepareLiveReq) (*rpc.PrepareLiveReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkMid(in.GetMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	platform, err := normalizePlatform(in.GetPlatform())
	if err != nil {
		return nil, err
	}
	deviceHash, err := checkRef("device_hash", in.GetDeviceHash())
	if err != nil {
		return nil, err
	}
	ipHash, err := checkRef("ip_hash", in.GetIpHash())
	if err != nil {
		return nil, err
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
	bound, _, err := l.svcCtx.Anchors.IsEnabled(l.ctx, room.RoomID, in.GetMid())
	if err != nil {
		return nil, err
	}
	if !bound {
		return nil, model.ErrAnchorForbidden
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcPrepareLive, reqID, model.IdempotencyKindRequest,
		room.RoomID, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcPrepareLive, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.PrepareLiveReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	now := model.NowUnix()
	checks := make([]*rpc.PrepareCheckItem, 0, 5)
	checks = append(checks, l.checkAnchorQualification(in.GetMid(), platform))

	riskCheck, retryAfter := l.checkRiskControl(l.ctx, in.GetMid(), platform, deviceHash, ipHash, reqID, traceID)
	checks = append(checks, riskCheck)

	checks = append(checks, verifyStateCheck(room))

	bannedCheck, err := l.checkNotBanned(l.ctx, room, in.GetMid(), now)
	if err != nil {
		return nil, err
	}
	checks = append(checks, bannedCheck)

	settingCheck, err := l.checkSettingOk(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	checks = append(checks, settingCheck)

	reply := &rpc.PrepareLiveReply{RoomId: room.RoomID, Checks: checks, State: rpc.RoomState(room.State)}
	finalState := room.State
	switch {
	case !allChecksPassed(checks):
		failed := firstFailedCheck(checks)
		reply.DenyCode = denyCodeForCheck(failed.GetCode(), failed.GetDegraded())
		reply.RetryAfterSeconds = retryAfter
	case room.State == model.RoomStateLiving:
		// 矩阵里没有 Living->Ready 的「重入」边：已在播就是另一件事，不在此处收敛状态。
		reply.DenyCode = denyRoomLiving
	case room.State == model.RoomStatePending:
		ok, err := l.svcCtx.Rooms.Transition(l.ctx, room.RoomID, model.RoomStatePending,
			model.RoomStateReady, room.StateVersion, model.RoomPatch{})
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, model.ErrConcurrentUpdate
		}
		finalState = model.RoomStateReady
		reply.Ready = true
		if _, err := l.svcCtx.StateLogs.Insert(l.ctx, &model.LiveRoomStateLog{
			RoomID: room.RoomID, StateType: model.LogTypeRoomState,
			FromState: model.RoomStatePending, ToState: model.RoomStateReady,
			OperatorMid: in.GetMid(), Source: model.SourceRPCClient,
			RequestID: reqID, TraceID: traceID, Reason: "prepare_passed",
		}); err != nil {
			l.Errorf("liveroom: room %d PENDING→READY 审计日志写入失败: %v", room.RoomID, err)
		}
	default:
		// READY 保持 READY（幂等复检）；BANNED 已被 not_banned 拦在下面分支，不会走到这里。
		reply.Ready = true
	}
	reply.State = rpc.RoomState(finalState)
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// checkAnchorQualification 读 creator 侧的直播资格。
// creator 的 UpAttr(from=2 直播 UP) 是本仓库现有的唯一「直播资格」读出口，
// 没有专门的 live-qualification RPC，属契约缺口（见 README）。
func (l *PrepareLiveLogic) checkAnchorQualification(mid int64, platform int32) *rpc.PrepareCheckItem {
	if l.svcCtx.Creator == nil {
		return newCheckItem(checkAnchorQualification, false, model.ErrCreatorNotConfigured.Error(), true)
	}
	resp, err := l.svcCtx.Creator.UpAttr(l.ctx, &creatorrpc.UpAttrReq{Mid: mid, From: creatorFromLiveUP})
	if err != nil {
		l.Errorf("liveroom: creator UpAttr mid=%d platform=%d: %v", mid, platform, err)
		return newCheckItem(checkAnchorQualification, false, model.ErrDownstreamUnavailable.Error(), true)
	}
	if resp.GetIsAuthor() != model.BoolToInt32(true) {
		return newCheckItem(checkAnchorQualification, false, "anchor has no live qualification", false)
	}
	return newCheckItem(checkAnchorQualification, true, "", false)
}

// checkRiskControl 开播风控判定；返回检查项与建议重试秒数。
// 入参只带摘要（device_hash/ip_hash），明文 IP 与设备号不离开本服务。
func (l *PrepareLiveLogic) checkRiskControl(ctx context.Context, mid int64, platform int32,
	deviceHash, ipHash, reqID, traceID string) (*rpc.PrepareCheckItem, int64) {
	if l.svcCtx.RiskControl == nil {
		return newCheckItem(checkRiskControl, false, model.ErrRiskControlNotConfigured.Error(), true), 0
	}
	resp, err := l.svcCtx.RiskControl.CheckAction(ctx, &riskrpc.CheckActionReq{
		Mid:       mid,
		Action:    riskrpc.GuardedAction_ACTION_LIVE_START,
		DeviceId:  deviceHash,
		IpHash:    ipHash,
		Platform:  platformName(platform),
		RequestId: reqID,
		TraceId:   traceID,
	})
	if err != nil {
		l.Errorf("liveroom: risk CheckAction mid=%d: %v", mid, err)
		return newCheckItem(checkRiskControl, false, model.ErrDownstreamUnavailable.Error(), true), 0
	}
	return riskCheckItem(resp), riskRetryAfter(resp)
}

// riskCheckItem 把风控裁决翻译成检查项（纯函数，供单测覆盖全部裁决分支）。
// CHALLENGE/BLOCK 都是「本次不可开播」，只有 ALLOW/REVIEW 放行；
// degraded=true 时即使 decision=ALLOW 也按未通过处理——降级下的 ALLOW 不代表评估过。
func riskCheckItem(resp *riskrpc.CheckActionReply) *rpc.PrepareCheckItem {
	if resp == nil {
		return newCheckItem(checkRiskControl, false, model.ErrDownstreamUnavailable.Error(), true)
	}
	if resp.GetDegraded() {
		return newCheckItem(checkRiskControl, false, "risk control degraded: "+resp.GetBasis(), true)
	}
	switch resp.GetDecision() {
	case riskrpc.Decision_DECISION_ALLOW, riskrpc.Decision_DECISION_REVIEW:
		return newCheckItem(checkRiskControl, true, "", false)
	case riskrpc.Decision_DECISION_CHALLENGE:
		return newCheckItem(checkRiskControl, false, "risk control requires challenge: "+resp.GetActionCode(), false)
	default:
		reason := resp.GetActionCode()
		if pc := resp.GetPunishment().GetReasonCode(); pc != "" {
			reason = pc
		}
		return newCheckItem(checkRiskControl, false, "risk control denied: "+reason, false)
	}
}

// riskRetryAfter 只有 CHALLENGE 给出有效期，其余裁决不需要定时重试。
func riskRetryAfter(resp *riskrpc.CheckActionReply) int64 {
	if resp == nil || resp.GetDecision() != riskrpc.Decision_DECISION_CHALLENGE {
		return 0
	}
	if ttl := resp.GetChallengeTtlSeconds(); ttl > 0 {
		return ttl
	}
	return defaultChallengeRetrySeconds
}

// verifyStateCheck 资料审核结论：只有 PASSED 且房间处于可开播态族才算通过。
func verifyStateCheck(room *model.LiveRoom) *rpc.PrepareCheckItem {
	if room.VerifyState != model.VerifyStatePassed {
		return newCheckItem(checkRoomVerified, false,
			fmt.Sprintf("verify_state=%d", room.VerifyState), false)
	}
	switch room.State {
	case model.RoomStatePending, model.RoomStateReady, model.RoomStateBanned:
		return newCheckItem(checkRoomVerified, true, "", false)
	default:
		return newCheckItem(checkRoomVerified, false,
			fmt.Sprintf("room state=%d 不在可开播态族", room.State), false)
	}
}

// checkNotBanned 主播维度与房间维度用同一个 now，避免两次取时间造成「一边过期一边生效」的抖动。
func (l *PrepareLiveLogic) checkNotBanned(ctx context.Context, room *model.LiveRoom, mid, now int64) (*rpc.PrepareCheckItem, error) {
	if room.State == model.RoomStateBanned {
		return newCheckItem(checkNotBanned, false, "room is banned", false), nil
	}
	byMid, err := l.svcCtx.Bans.HasActiveByMid(ctx, mid, now)
	if err != nil {
		return nil, err
	}
	if byMid {
		return newCheckItem(checkNotBanned, false, "anchor has an active ban", false), nil
	}
	ban, err := l.svcCtx.Bans.FindActiveByRoom(ctx, room.RoomID, now)
	if err != nil {
		return nil, err
	}
	if ban != nil {
		return newCheckItem(checkNotBanned, false, "room has an active ban", false), nil
	}
	return newCheckItem(checkNotBanned, true, "", false), nil
}

// checkSettingOk 配置行存在性：CreateRoom 必然写入配置，缺行说明数据被外部改坏，
// 此时不能假定「按默认值开播」——那是用缺数据冒充已配置。
func (l *PrepareLiveLogic) checkSettingOk(ctx context.Context, roomID int64) (*rpc.PrepareCheckItem, error) {
	row, err := l.svcCtx.Settings.FindOne(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return newCheckItem(checkSettingOk, false, model.ErrNoSettingRow.Error(), false), nil
	}
	if !model.ValidLiveType(row.LiveType) {
		return newCheckItem(checkSettingOk, false, fmt.Sprintf("live_type=%d 非法", row.LiveType), false), nil
	}
	return newCheckItem(checkSettingOk, true, "", false), nil
}

const (
	// creatorFromLiveUP creator.UpAttrReq.from 的「直播 UP」取值。
	creatorFromLiveUP = 2
	// defaultChallengeRetrySeconds 风控未给出有效期时的兜底重试间隔。
	defaultChallengeRetrySeconds = 30
)
