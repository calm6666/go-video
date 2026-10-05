// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：live-room RPC → 客户端投影。
//
// 投影原则（AGENTS.md §5/§6）：
//   - 枚举一律降为 int32，客户端拿不到 protobuf 类型；
//   - 服务内部字段不下发终端：SessionInfo.last_stream_seq（流事件乱序守卫）、
//     SessionInfo.record_id（live-media 内部录制记录）、SessionInfo.moderation_task_id（送审关联）、
//     AreaInfo.operator_mid/ctime/mtime（运营审计）；回放对外只给 asset_id/aid 引用；
//   - 列表返回非 nil 切片，端上不必区分 null 与 []；
//   - 网关不推断房间可见性：房间状态、禁播与资料审核结论一律原样透出。

package logic

import (
	"go-video/gateway/app/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"
)

func liveRoomToAPI(p *liveroomrpc.RoomInfo) types.LiveRoomInfo {
	if p == nil {
		return types.LiveRoomInfo{}
	}
	return types.LiveRoomInfo{
		RoomId:          p.GetRoomId(),
		OwnerMid:        p.GetOwnerMid(),
		Title:           p.GetTitle(),
		Cover:           p.GetCover(),
		AreaId:          p.GetAreaId(),
		State:           int32(p.GetState()),
		VerifyState:     int32(p.GetVerifyState()),
		ActiveSessionId: p.GetActiveSessionId(),
		ActiveStreamId:  p.GetActiveStreamId(),
		StateVersion:    p.GetStateVersion(),
		RejectReason:    p.GetRejectReason(),
		BanUntil:        p.GetBanUntil(),
		Ctime:           p.GetCtime(),
		Mtime:           p.GetMtime(),
	}
}

func liveRoomsToAPI(list []*liveroomrpc.RoomInfo) []types.LiveRoomInfo {
	out := make([]types.LiveRoomInfo, 0, len(list))
	for _, r := range list {
		out = append(out, liveRoomToAPI(r))
	}
	return out
}

func liveSettingToAPI(p *liveroomrpc.RoomSetting) types.LiveRoomSetting {
	if p == nil {
		return types.LiveRoomSetting{}
	}
	return types.LiveRoomSetting{
		RoomId:               p.GetRoomId(),
		DanmakuEnabled:       p.GetDanmakuEnabled(),
		ReplyEnabled:         p.GetReplyEnabled(),
		RecordEnabled:        p.GetRecordEnabled(),
		LinkmicEnabled:       p.GetLinkmicEnabled(),
		LiveType:             p.GetLiveType(),
		MinClientVersionCode: p.GetMinClientVersionCode(),
		Mtime:                p.GetMtime(),
	}
}

func liveSessionToAPI(p *liveroomrpc.SessionInfo) types.LiveSessionInfo {
	if p == nil {
		return types.LiveSessionInfo{}
	}
	return types.LiveSessionInfo{
		SessionId:       p.GetSessionId(),
		RoomId:          p.GetRoomId(),
		Mid:             p.GetMid(),
		State:           int32(p.GetState()),
		TitleSnapshot:   p.GetTitleSnapshot(),
		AreaIdSnapshot:  p.GetAreaIdSnapshot(),
		StreamId:        p.GetStreamId(),
		StartedAt:       p.GetStartedAt(),
		EndedAt:         p.GetEndedAt(),
		DurationSeconds: p.GetDurationSeconds(),
		EndReason:       int32(p.GetEndReason()),
		ReplayState:     int32(p.GetReplayState()),
		RecordAssetId:   p.GetRecordAssetId(),
		RecordAid:       p.GetRecordAid(),
		Ctime:           p.GetCtime(),
		Mtime:           p.GetMtime(),
	}
}

func liveSessionsToAPI(list []*liveroomrpc.SessionInfo) []types.LiveSessionInfo {
	out := make([]types.LiveSessionInfo, 0, len(list))
	for _, s := range list {
		out = append(out, liveSessionToAPI(s))
	}
	return out
}

func liveAreasToAPI(list []*liveroomrpc.AreaInfo) []types.LiveArea {
	out := make([]types.LiveArea, 0, len(list))
	for _, a := range list {
		out = append(out, types.LiveArea{
			AreaId:       a.GetAreaId(),
			AreaName:     a.GetAreaName(),
			ParentAreaId: a.GetParentAreaId(),
			Sort:         a.GetSort(),
			State:        a.GetState(),
		})
	}
	return out
}

func liveAnchorsToAPI(list []*liveroomrpc.AnchorInfo) []types.LiveAnchor {
	out := make([]types.LiveAnchor, 0, len(list))
	for _, a := range list {
		out = append(out, types.LiveAnchor{
			RoomId: a.GetRoomId(),
			Mid:    a.GetMid(),
			Role:   int32(a.GetRole()),
			State:  a.GetState(),
			Ctime:  a.GetCtime(),
			Mtime:  a.GetMtime(),
		})
	}
	return out
}

// livePrepareChecksToAPI 逐项透传开播前置检查结论，保持服务返回顺序（契约要求顺序稳定）。
// degraded=true 的项原样下发：网关不得因为「看起来没失败」就把它标成 passed。
func livePrepareChecksToAPI(list []*liveroomrpc.PrepareCheckItem) []types.LivePrepareCheckItem {
	out := make([]types.LivePrepareCheckItem, 0, len(list))
	for _, c := range list {
		out = append(out, types.LivePrepareCheckItem{
			Code:     c.GetCode(),
			Passed:   c.GetPassed(),
			Detail:   c.GetDetail(),
			Degraded: c.GetDegraded(),
		})
	}
	return out
}

// liveRoomDataFromReply 把 GetRoomReply 投影成统一房间视图。
// has_setting / has_active_session 显式回传「服务有没有给这一段」，
// 因为 setting 与 active_session 是可选 protobuf 消息：客户端无法从全 false 的零值里
// 区分「未请求」和「主播真的把所有开关都关了」，也不能从 room.active_session_id=0
// 反推进场次详情（with_active_session 未请求时该字段本就为 0）。
func liveRoomDataFromReply(reply *liveroomrpc.GetRoomReply) types.LiveRoomData {
	if reply == nil {
		return types.LiveRoomData{}
	}
	return types.LiveRoomData{
		Room:             liveRoomToAPI(reply.GetRoom()),
		Setting:          liveSettingToAPI(reply.GetSetting()),
		HasSetting:       reply.GetSetting() != nil,
		ActiveSession:    liveSessionToAPI(reply.GetActiveSession()),
		HasActiveSession: reply.GetActiveSession() != nil,
	}
}

// liveRoomSettingForRPC 把终端平铺的配置字段组装成 protobuf RoomSetting。
// room_id 由服务侧按请求上下文回填，这里保持 0，避免网关伪造主键。
func liveRoomSettingForRPC(danmaku, reply, record, linkmic bool, liveType, minClientVersion int32) *liveroomrpc.RoomSetting {
	return &liveroomrpc.RoomSetting{
		DanmakuEnabled:       danmaku,
		ReplyEnabled:         reply,
		RecordEnabled:        record,
		LinkmicEnabled:       linkmic,
		LiveType:             liveType,
		MinClientVersionCode: minClientVersion,
	}
}
