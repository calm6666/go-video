// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// live-room RPC ↔ 管理后台投影 + 写入口门槛。
//
// 职责边界（AGENTS.md §4/§5/§8），与 conv_cron.go / conv_audit.go 同一套口径：
//  1. 网关只做三件事：入参形态门槛（主体存在、幂等键非空、数值非负、游标长度）、调用下游、
//     逐字段投影。
//     房间状态机合法性、禁播时长与到期时间、分区名称唯一性与停用占用、配置取值范围、
//     「operator_mid 是否是该房间生效房主」全部由 services/live-room 判定，网关不复算，
//     也不把下游错误改写成看起来成功的空结果；
//  2. 后台面下发**全字段**投影（gateway/app 的终端面裁掉的 last_stream_seq、record_id、
//     moderation_task_id、ban reason、area operator_mid 在这里都要保留）：这些正是运营排障
//     与审计需要看的证据，裁掉就等于让后台只能靠猜。反向的边界是 room.cover 与
//     session.stream_id ——  proto 已保证它们只是对象/流标识引用，不含签名地址，
//     网关也不代为换取任何可播放地址；
//  3. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// errLiveServiceNotConfigured：未配置 LiveRoomRPC 时 live 域路由一律返回它。
// 不退化成伪造空列表——那会让后台把「下游没接」读成「没有人开播」，
// 进而误判「直播间全都正常」。
var errLiveServiceNotConfigured = errors.New("live-room service not configured")

// errLiveRequestMissing：请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景，出现即说明调用方漏装了参数。
var errLiveRequestMissing = errors.New("gateway/admin: request body required")

// errLiveSessionRequired：受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没有被中间件保护（权限表/挂载漂移），一律 fail-closed。
var errLiveSessionRequired = errors.New("gateway/admin: admin session identity required")

// errLiveRoomSubjectRequired GetRoomReq 的 room_id / owner_mid 二选一：
// 两个都是 0 时下游会去查 room_id=0 的房间，返回的是「房间不存在」而不是「你少传了参数」。
var errLiveRoomSubjectRequired = errors.New("gateway/admin: room_id or owner_mid required")

// errLiveSessionSubjectRequired 同上：GetSessionReq 至少要给 session_id 或 room_id。
var errLiveSessionSubjectRequired = errors.New("gateway/admin: session_id or room_id required")

// liveOperatorGate 是写入口的统一门槛：
//   - operator_mid 必须 > 0（live-room 以此作为台账主体，无主体的处置不允许发生）；
//   - 会话身份必须存在（AdminPermission 判定通过后才会挂上）。
//
// 网关**不用** admin_id 覆盖 operator_mid：admin_id 是 op_admin_user 主键、operator_mid 是
// 用户 mid，两者不是同一编号空间，覆盖等于把处置记到无关用户头上（缺口见 admin.api 注释
// 与 gateway/admin/README.md）。两条主体都写进日志，事后既能追「谁点的按钮」也能对上台账。
func liveOperatorGate(ctx context.Context, route string, operatorMid int64) error {
	if err := requireOperator("operator_mid", operatorMid); err != nil {
		return err
	}
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return errLiveSessionRequired
	}
	// 只打路由与两个 ID：token、房主资料、reason 正文都不进日志（AGENTS.md §4）。
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d operator_mid=%d", route, id.AdminID, operatorMid)
	return nil
}

// liveIdempotencyGate 拦住空幂等键：幂等键的任何改写（含 TrimSpace 后回写）都会让它失去语义，
// 所以这里只判空、不改值，原样透传给 live-room 的 request_id。
func liveIdempotencyGate(requestID string) error {
	return requireNonEmpty("request_id", requestID)
}

// liveNonNeg 传输层门槛：0 在 live-room 里普遍是「不过滤/不限制」的合法哨兵，
// 负数没有任何对应语义，透传只会换来一次无意义的往返。
func liveNonNeg(field string, v int64) error {
	if v < 0 {
		return errors.New("gateway/admin: " + field + " must be >= 0")
	}
	return nil
}

// liveRequiredID 拦住「必填主键缺失」：live-room 的 ListSessions/ListAnchors 明确要求 room_id>0，
// 传 0 只会落到一个不存在的主键上，回给后台的是空列表而不是「你少传了参数」。
func liveRequiredID(field string, v int64) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required")
	}
	return nil
}

// liveSentinelGE 给 -1 型哨兵参数（ListAreasReq.parent_area_id / state）设下界：
// 小于「不过滤」的取值没有任何下游语义。上界属 live-room 的判定域（哪些 state 合法由它说），
// 网关不复算。
func liveSentinelGE(field string, v int64) error {
	if v < -1 {
		return errors.New("gateway/admin: " + field + " must be >= -1 (-1 = no filter)")
	}
	return nil
}

// liveCursorMaxLen 是场次游标字符串的传输层长度上限（与 conv_cron.go 的 cronMaxCursorLen 同口径）：
// 游标内容（live-room 侧是 session_id 的编码）由 decodeIDCursor 解释并回 ErrCursorInvalid，
// 网关不复算格式，只挡住明显异常的长度，避免把任意大的 blob 喂进下游的查询条件。
const liveCursorMaxLen = 128

// liveCursor 只做长度门槛，空串是合法的「第一页」。
func liveCursor(cursor string) error {
	if len(cursor) > liveCursorMaxLen {
		return fmt.Errorf("gateway/admin: cursor too long (%d > %d)", len(cursor), liveCursorMaxLen)
	}
	return nil
}

// liveNonNeg32 是 int32 字段的同名版本（state/role/sort/page_size 等）。
func liveNonNeg32(field string, v int32) error {
	if v < 0 {
		return errors.New("gateway/admin: " + field + " must be >= 0")
	}
	return nil
}

// --- rpc → 后台 types 投影 ---

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
		SessionId:        p.GetSessionId(),
		RoomId:           p.GetRoomId(),
		Mid:              p.GetMid(),
		State:            int32(p.GetState()),
		TitleSnapshot:    p.GetTitleSnapshot(),
		AreaIdSnapshot:   p.GetAreaIdSnapshot(),
		StreamId:         p.GetStreamId(),
		StartedAt:        p.GetStartedAt(),
		EndedAt:          p.GetEndedAt(),
		DurationSeconds:  p.GetDurationSeconds(),
		EndReason:        int32(p.GetEndReason()),
		LastStreamSeq:    p.GetLastStreamSeq(),
		ReplayState:      int32(p.GetReplayState()),
		RecordId:         p.GetRecordId(),
		RecordAssetId:    p.GetRecordAssetId(),
		RecordAid:        p.GetRecordAid(),
		ModerationTaskId: p.GetModerationTaskId(),
		Ctime:            p.GetCtime(),
		Mtime:            p.GetMtime(),
	}
}

func liveSessionsToAPI(list []*liveroomrpc.SessionInfo) []types.LiveSessionInfo {
	out := make([]types.LiveSessionInfo, 0, len(list))
	for _, s := range list {
		out = append(out, liveSessionToAPI(s))
	}
	return out
}

func liveBansToAPI(list []*liveroomrpc.RoomBanInfo) []types.LiveRoomBanInfo {
	out := make([]types.LiveRoomBanInfo, 0, len(list))
	for _, b := range list {
		out = append(out, types.LiveRoomBanInfo{
			BanId:        b.GetBanId(),
			RoomId:       b.GetRoomId(),
			Mid:          b.GetMid(),
			BanType:      int32(b.GetBanType()),
			Reason:       b.GetReason(),
			StartAt:      b.GetStartAt(),
			EndAt:        b.GetEndAt(),
			State:        b.GetState(),
			OperatorMid:  b.GetOperatorMid(),
			LiftOperator: b.GetLiftOperatorMid(),
			LiftReason:   b.GetLiftReason(),
			LiftedAt:     b.GetLiftedAt(),
			Ctime:        b.GetCtime(),
		})
	}
	return out
}

func liveAreasToAPI(list []*liveroomrpc.AreaInfo) []types.LiveAreaInfo {
	out := make([]types.LiveAreaInfo, 0, len(list))
	for _, a := range list {
		out = append(out, types.LiveAreaInfo{
			AreaId:       a.GetAreaId(),
			AreaName:     a.GetAreaName(),
			ParentAreaId: a.GetParentAreaId(),
			Sort:         a.GetSort(),
			State:        a.GetState(),
			OperatorMid:  a.GetOperatorMid(),
			Ctime:        a.GetCtime(),
			Mtime:        a.GetMtime(),
		})
	}
	return out
}

func liveAnchorsToAPI(list []*liveroomrpc.AnchorInfo) []types.LiveAnchorInfo {
	out := make([]types.LiveAnchorInfo, 0, len(list))
	for _, a := range list {
		out = append(out, types.LiveAnchorInfo{
			Id:     a.GetId(),
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

// liveRoomDetailFromReply 把 GetRoomReply 投影成运营详情页视图。
// has_setting / has_active_session 显式回传「服务有没有给这一段」：两个字段都是可选
// protobuf 消息，客户端无法从全 false 的零值里区分「没请求附带」和「主播真的把所有开关都关了」。
func liveRoomDetailFromReply(reply *liveroomrpc.GetRoomReply) types.LiveRoomDetailData {
	if reply == nil {
		return types.LiveRoomDetailData{}
	}
	return types.LiveRoomDetailData{
		Room:             liveRoomToAPI(reply.GetRoom()),
		Setting:          liveSettingToAPI(reply.GetSetting()),
		HasSetting:       reply.GetSetting() != nil,
		ActiveSession:    liveSessionToAPI(reply.GetActiveSession()),
		HasActiveSession: reply.GetActiveSession() != nil,
	}
}

// liveSettingForRPC 把后台平铺的配置入参组装成 protobuf RoomSetting。
// room_id 与 mtime 由 live-room 按请求上下文与维护时钟回填（整段覆盖语义要求 setting 必传，
// 所以 types 里刻意不暴露这两个服务端字段，避免后台伪造配置变更时间）。
func liveSettingForRPC(in types.LiveRoomSettingInput) *liveroomrpc.RoomSetting {
	return &liveroomrpc.RoomSetting{
		DanmakuEnabled:       in.DanmakuEnabled,
		ReplyEnabled:         in.ReplyEnabled,
		RecordEnabled:        in.RecordEnabled,
		LinkmicEnabled:       in.LinkmicEnabled,
		LiveType:             in.LiveType,
		MinClientVersionCode: in.MinClientVersionCode,
	}
}
