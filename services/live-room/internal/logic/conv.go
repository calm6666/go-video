// 本文件是 logic 包的手写扩展（rpc ↔ model 投影），不是 goctl 生成产物。
//
// AGENTS.md §4/§6：领域服务不返回数据库原始对象，投影只搬运事实字段，
// 不在这里做业务判定；枚举一律按 model 常量表回灌 rpc 枚举（两者取值已被 proto 锁死，
// 见 model.TestModelStateConstantsMatchProtoEnums）。

package logic

import (
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// roomInfo 把 live_room 行投影为 rpc.RoomInfo。
func roomInfo(r *model.LiveRoom) *rpc.RoomInfo {
	if r == nil {
		return nil
	}
	return &rpc.RoomInfo{
		RoomId:          r.RoomID,
		OwnerMid:        r.OwnerMid,
		Title:           r.Title,
		Cover:           r.Cover,
		AreaId:          r.AreaID,
		State:           rpc.RoomState(r.State),
		VerifyState:     rpc.VerifyState(r.VerifyState),
		ActiveSessionId: r.ActiveSessionID,
		ActiveStreamId:  r.ActiveStreamID,
		StateVersion:    r.StateVersion,
		RejectReason:    r.RejectReason,
		BanUntil:        r.BanUntil,
		Ctime:           r.Ctime,
		Mtime:           r.Mtime,
	}
}

func roomInfoList(rows []*model.LiveRoom) []*rpc.RoomInfo {
	out := make([]*rpc.RoomInfo, 0, len(rows))
	for _, r := range rows {
		if info := roomInfo(r); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// settingInfo 把 live_room_setting 行投影为 rpc.RoomSetting；
// row 为 nil（房间还没写过配置）时套服务端默认，
// 绝不让客户端把「缺行」误读成「所有功能都关着」。
func settingInfo(row *model.LiveRoomSetting) *rpc.RoomSetting {
	if row == nil {
		row = defaultSettingRow(0)
	}
	return &rpc.RoomSetting{
		RoomId:               row.RoomID,
		DanmakuEnabled:       model.Int32ToBool(row.DanmakuEnabled),
		ReplyEnabled:         model.Int32ToBool(row.ReplyEnabled),
		RecordEnabled:        model.Int32ToBool(row.RecordEnabled),
		LinkmicEnabled:       model.Int32ToBool(row.LinkmicEnabled),
		LiveType:             row.LiveType,
		MinClientVersionCode: row.MinClientVersionCode,
		Mtime:                row.Mtime,
	}
}

// sessionInfo 把 live_session 行投影为 rpc.SessionInfo。
// 回放三列只是引用，播放地址由 live-media 签发，本服务不拼接 URL。
func sessionInfo(s *model.LiveSession) *rpc.SessionInfo {
	if s == nil {
		return nil
	}
	return &rpc.SessionInfo{
		SessionId:        s.SessionID,
		RoomId:           s.RoomID,
		Mid:              s.Mid,
		State:            rpc.SessionState(s.State),
		TitleSnapshot:    s.TitleSnapshot,
		AreaIdSnapshot:   s.AreaIDSnapshot,
		StreamId:         s.StreamID,
		StartedAt:        s.StartedAt,
		EndedAt:          s.EndedAt,
		DurationSeconds:  s.DurationSeconds,
		EndReason:        rpc.EndReason(s.EndReason),
		LastStreamSeq:    s.LastStreamSeq,
		ReplayState:      rpc.ReplayState(s.ReplayState),
		RecordId:         s.RecordID,
		RecordAssetId:    s.RecordAssetID,
		RecordAid:        s.RecordAid,
		ModerationTaskId: s.ModerationTaskID,
		Ctime:            s.Ctime,
		Mtime:            s.Mtime,
	}
}

func sessionInfoList(rows []*model.LiveSession) []*rpc.SessionInfo {
	out := make([]*rpc.SessionInfo, 0, len(rows))
	for _, s := range rows {
		if info := sessionInfo(s); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// banInfo 把 live_room_ban 行投影为 rpc.RoomBanInfo。
// reason/lift_reason 原样给运营侧；可见性与脱敏由 gateway/admin 决定，
// 本服务不做「按角色裁剪字段」的猜测（那是越权风险最高的地方）。
func banInfo(b *model.LiveRoomBan) *rpc.RoomBanInfo {
	if b == nil {
		return nil
	}
	return &rpc.RoomBanInfo{
		BanId:           b.BanID,
		RoomId:          b.RoomID,
		Mid:             b.Mid,
		BanType:         rpc.BanType(b.BanType),
		Reason:          b.Reason,
		StartAt:         b.StartAt,
		EndAt:           b.EndAt,
		State:           b.State,
		OperatorMid:     b.OperatorMid,
		LiftOperatorMid: b.LiftOperatorMid,
		LiftReason:      b.LiftReason,
		LiftedAt:        b.LiftedAt,
		Ctime:           b.Ctime,
	}
}

func banInfoList(rows []*model.LiveRoomBan) []*rpc.RoomBanInfo {
	out := make([]*rpc.RoomBanInfo, 0, len(rows))
	for _, b := range rows {
		if info := banInfo(b); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// areaInfo 把 live_area 行投影为 rpc.AreaInfo。
func areaInfo(a *model.LiveArea) *rpc.AreaInfo {
	if a == nil {
		return nil
	}
	return &rpc.AreaInfo{
		AreaId:       a.AreaID,
		AreaName:     a.AreaName,
		ParentAreaId: a.ParentAreaID,
		Sort:         a.Sort,
		State:        a.State,
		OperatorMid:  a.OperatorMid,
		Ctime:        a.Ctime,
		Mtime:        a.Mtime,
	}
}

func areaInfoList(rows []*model.LiveArea) []*rpc.AreaInfo {
	out := make([]*rpc.AreaInfo, 0, len(rows))
	for _, a := range rows {
		if info := areaInfo(a); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// anchorInfo 把 live_room_anchor 行投影为 rpc.AnchorInfo。
func anchorInfo(a *model.LiveRoomAnchor) *rpc.AnchorInfo {
	if a == nil {
		return nil
	}
	return &rpc.AnchorInfo{
		Id:     a.ID,
		RoomId: a.RoomID,
		Mid:    a.Mid,
		Role:   rpc.AnchorRole(a.Role),
		State:  a.State,
		Ctime:  a.Ctime,
		Mtime:  a.Mtime,
	}
}

func anchorInfoList(rows []*model.LiveRoomAnchor) []*rpc.AnchorInfo {
	out := make([]*rpc.AnchorInfo, 0, len(rows))
	for _, a := range rows {
		if info := anchorInfo(a); info != nil {
			out = append(out, info)
		}
	}
	return out
}

// newCheckItem 构造 PrepareLive 的单项检查结果。
// detail 只放稳定描述，不放下游原始响应、凭据或明文 IP/设备号（AGENTS.md §7）。
func newCheckItem(code string, passed bool, detail string, degraded bool) *rpc.PrepareCheckItem {
	return &rpc.PrepareCheckItem{Code: code, Passed: passed, Detail: detail, Degraded: degraded}
}

// PrepareLive 检查项的稳定 key（顺序即下发顺序，客户端按 code 渲染，不按数组下标）。
const (
	checkAnchorQualification = "anchor_qualification"
	checkRiskControl         = "risk_control"
	checkRoomVerified        = "room_verified"
	checkNotBanned           = "not_banned"
	checkSettingOk           = "setting_ok"
)

// PrepareLive / StartLive 的 deny_code 稳定取值。
// 文案由客户端按 code 渲染，服务端不写死 UI（AGENTS.md §6）。
const (
	denyAnchorQualification = "anchor_not_verified"
	denyRiskControl         = "risk_denied"
	denyRiskChallenge       = "risk_challenge"
	denyRoomNotVerified     = "room_not_verified"
	denyBanned              = "room_banned"
	denySettingMissing      = "setting_missing"
	denyRoomNotReady        = "not_ready"
	denyRoomLiving          = "already_living"
	denyDownstreamDegraded  = "downstream_unavailable"
)

// allChecksPassed 判定检查清单是否全通过。空清单不算通过——
// 「一个检查项都没产出」只可能是实现漏了，不能当成「都过了」。
func allChecksPassed(items []*rpc.PrepareCheckItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		if it == nil || !it.GetPassed() {
			return false
		}
	}
	return true
}

// firstFailedCheck 返回第一个未通过的检查项（供 deny_code / retry_after 归因）。
func firstFailedCheck(items []*rpc.PrepareCheckItem) *rpc.PrepareCheckItem {
	for _, it := range items {
		if it == nil || !it.GetPassed() {
			return it
		}
	}
	return nil
}

// denyCodeForCheck 把检查项 code 映射为稳定 deny_code。
// 未知 code 回落 denyRoomNotReady，绝不回落成「通过」。
func denyCodeForCheck(code string, degraded bool) string {
	if degraded {
		return denyDownstreamDegraded
	}
	switch code {
	case checkAnchorQualification:
		return denyAnchorQualification
	case checkRiskControl:
		return denyRiskControl
	case checkRoomVerified:
		return denyRoomNotVerified
	case checkNotBanned:
		return denyBanned
	case checkSettingOk:
		return denySettingMissing
	default:
		return denyRoomNotReady
	}
}
