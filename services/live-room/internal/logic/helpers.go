// 本文件是 logic 包的手写扩展（入参校验、幂等/事件去重接线、状态机裁决、游标与缓存键），
// 不是 goctl 生成产物。
//
// 分工（AGENTS.md §4/§5）：这里只放「不碰 SQL」的可测函数——校验、归一、幂等判定、
// 状态机裁决、投影前的纯判定与缓存键构造。SQL、CAS 与事务边界一律留在 model。

package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 列宽约束（与 deploy/migrations/live-room/0000NN 一致）。
// 校验发生在 logic：model 只保证「不合法就别写」，logic 保证「别把不合法的请求带进事务」。
const (
	// maxDedupIDBytes 对应 live_room_idempotency.dedup_key VARCHAR(64)：
	// request_id / event_id 原样作为该列的值（live_room_state_log.request_id 与之同值，
	// 所以不能在这里再拼 rpc 名前缀，否则两张表的关联字段会对不上）。
	maxDedupIDBytes = 64
	// maxTraceIDBytes 对应各表 trace_id VARCHAR(64)。
	maxTraceIDBytes = 64
	// maxReasonRunes 对应 reason / lift_reason / reject_reason VARCHAR(255)：
	// 留 5 个字符余量给多字节安全边界外的省略号，超长直接拒绝而不是截断（审计必须与库值一致）。
	maxReasonRunes = 250
	// maxCoverRefRunes 对应 live_room.cover VARCHAR(512)。
	maxCoverRefRunes = 512
	// maxRefRunes 对应 stream_id / active_stream_id VARCHAR(64)。
	maxRefRunes = 64
	// maxSnapshotTitleRunes 对应 live_session.title_snapshot 列宽。
	maxSnapshotTitleRunes = 80
)

// 缓存键前缀：本服务自有命名空间，不与他服务共用（AGENTS.md §5 的 key 空间边界）。
const (
	roomCacheKeyPrefix     = "govideo:liveroom:room:"
	areaListCacheKeyPrefix = "govideo:liveroom:areas:"
	// areaCacheNoFilter 表达「不过滤」，与 model.AreaListQuery 的 -1 同语义。
	// 0 是合法取值（一级分区 / 停用），绝不能当缺省丢掉。
	areaCacheNoFilter = -1
)

// --- 通用入参校验 ---

func checkRoomID(id int64) error {
	if id <= 0 {
		return model.ErrInvalidRoomID
	}
	return nil
}

func checkSessionID(id int64) error {
	if id <= 0 {
		return model.ErrInvalidSessionID
	}
	return nil
}

func checkMid(id int64) error {
	if id <= 0 {
		return model.ErrInvalidMid
	}
	return nil
}

// checkOperator 校验运营/系统操作者：没有归因主体的处置不可受理。
func checkOperator(mid int64) error {
	if mid <= 0 {
		return model.ErrOperatorRequired
	}
	return nil
}

// checkRequestID 校验写接口幂等键。空键一律拒绝：没有幂等键的写重放就是两次副作用。
func checkRequestID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ErrRequestIDRequired
	}
	if len(id) > maxDedupIDBytes {
		return fmt.Errorf("%w: request_id %d bytes, max %d", model.ErrDedupIDTooLong, len(id), maxDedupIDBytes)
	}
	return nil
}

// checkEventID 校验上游事件键（live.state.v1 / moderation.result.v1 的 event_id）。
func checkEventID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ErrEventIDRequired
	}
	if len(id) > maxDedupIDBytes {
		return fmt.Errorf("%w: event_id %d bytes, max %d", model.ErrDedupIDTooLong, len(id), maxDedupIDBytes)
	}
	return nil
}

// checkTitle 归一并校验房间标题：去首尾空白后按 rune 计长（列宽是字符数，不是字节数）。
func checkTitle(title string, maxRunes int32) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "", model.ErrTitleInvalid
	}
	if int32(runeLen(t)) > maxRunes {
		return "", fmt.Errorf("%w: %d > %d", model.ErrTitleInvalid, runeLen(t), maxRunes)
	}
	return t, nil
}

// checkCover 校验封面引用。空串合法（尚未上传封面）。
// 明确拒绝绝对 URL：客户端只能拿到 object key / 站内相对地址，
// 签名地址与 OSS 公共地址不得进库（AGENTS.md §6）。
func checkCover(cover string) (string, error) {
	c := strings.TrimSpace(cover)
	if c == "" {
		return "", nil
	}
	if runeLen(c) > maxCoverRefRunes {
		return "", fmt.Errorf("%w: %d > %d", model.ErrCoverTooLong, runeLen(c), maxCoverRefRunes)
	}
	low := strings.ToLower(c)
	if strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") ||
		strings.Contains(low, "://") {
		return "", fmt.Errorf("%w: 只允许 object key 或站内相对地址", model.ErrCoverTooLong)
	}
	return c, nil
}

// checkRef 校验 stream_id / device_hash / ip_hash 这类短引用：禁止明文 IP、设备号与凭据。
func checkRef(name, v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if runeLen(v) > maxRefRunes {
		return "", fmt.Errorf("%w: %s too long", model.ErrStreamRefMismatch, name)
	}
	if strings.Contains(v, " ") || strings.Contains(v, "://") {
		return "", fmt.Errorf("%w: %s must be an opaque reference", model.ErrStreamRefMismatch, name)
	}
	return v, nil
}

// checkReason 校验审计文本。required=true 时空串即拒绝。
func checkReason(name, reason string, required bool) (string, error) {
	r := strings.TrimSpace(reason)
	if r == "" {
		if required {
			return "", fmt.Errorf("%w: %s required", model.ErrReasonTooLong, name)
		}
		return "", nil
	}
	if runeLen(r) > maxReasonRunes {
		return "", fmt.Errorf("%w: %s %d > %d", model.ErrReasonTooLong, name, runeLen(r), maxReasonRunes)
	}
	return r, nil
}

func checkAreaName(name string, maxRunes int32) (string, error) {
	n := strings.TrimSpace(name)
	if n == "" || int32(runeLen(n)) > maxRunes {
		return "", fmt.Errorf("%w: %d > %d", model.ErrAreaNameInvalid, runeLen(n), maxRunes)
	}
	return n, nil
}

// sanitizeTraceID 裁剪 trace_id 到列宽：trace 是关联句柄而非业务事实，
// 截断尾巴不影响正确性，而因它让整次写入失败反而丢状态。
func sanitizeTraceID(id string) string {
	id = strings.TrimSpace(id)
	if len(id) > maxTraceIDBytes {
		return id[:maxTraceIDBytes]
	}
	return id
}

func runeLen(s string) int { return len([]rune(s)) }

// truncateRunes 按 rune 截断到列宽内。只用于「快照类」文本（开播时的标题快照）：
// 快照的语义本就是「那一刻的标题」，列宽装不下时保留前缀比让整个开播失败更正确。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// 幂等/审计用的 RPC 名常量：与 rpc.LiveRoom 的方法名逐字一致。
// 单独定义常量的原因是 live_room_idempotency.rpc 与 live_room_state_log 靠它归因，
// 散落字面量迟早会写出两个拼写。
const (
	rpcCreateRoom                = "CreateRoom"
	rpcUpdateRoomInfo            = "UpdateRoomInfo"
	rpcGetRoom                   = "GetRoom"
	rpcListRooms                 = "ListRooms"
	rpcPrepareLive               = "PrepareLive"
	rpcStartLive                 = "StartLive"
	rpcEndLive                   = "EndLive"
	rpcCloseRoom                 = "CloseRoom"
	rpcReportStreamState         = "ReportStreamState"
	rpcApplyRoomModerationResult = "ApplyRoomModerationResult"
	rpcBanRoom                   = "BanRoom"
	rpcLiftBan                   = "LiftBan"
	rpcListRoomBans              = "ListRoomBans"
	rpcGetSession                = "GetSession"
	rpcListSessions              = "ListSessions"
	rpcAttachReplay              = "AttachReplay"
	rpcUpdateRoomSetting         = "UpdateRoomSetting"
	rpcMutateAnchor              = "MutateAnchor"
	rpcListAnchors               = "ListAnchors"
	rpcUpsertArea                = "UpsertArea"
	rpcListAreas                 = "ListAreas"
)

// --- 平台口径（AGENTS.md §6：只有 Android/iOS/HarmonyOS/桌面端，不支持小程序）---

// normalizePlatform 归一创建端：UNSPECIFIED 按契约记 android；
// 任何未定义取值（含小程序类）直接拒绝，不做「未知即放行」。
func normalizePlatform(p rpc.Platform) (int32, error) {
	switch p {
	case rpc.Platform_PLATFORM_UNSPECIFIED:
		return model.PlatformAndroid, nil
	case rpc.Platform_PLATFORM_ANDROID:
		return model.PlatformAndroid, nil
	case rpc.Platform_PLATFORM_IOS:
		return model.PlatformIOS, nil
	case rpc.Platform_PLATFORM_HARMONY:
		return model.PlatformHarmony, nil
	case rpc.Platform_PLATFORM_DESKTOP:
		return model.PlatformDesktop, nil
	default:
		return 0, fmt.Errorf("%w: %d", model.ErrPlatformInvalid, int32(p))
	}
}

// platformName 是给 risk-control 的稳定字符串口径（其 platform 列为 VARCHAR）。
func platformName(platform int32) string {
	switch platform {
	case model.PlatformIOS:
		return "ios"
	case model.PlatformHarmony:
		return "harmony"
	case model.PlatformDesktop:
		return "desktop"
	case model.PlatformAndroid:
		return "android"
	default:
		return ""
	}
}

// --- 分页与游标 ---

// clampPage 归一页码：0 视为第一页（客户端省略分页参数的常见形态），负数拒绝。
func clampPage(page int32) (int, error) {
	if page < 0 {
		return 0, fmt.Errorf("%w: page=%d", model.ErrInvalidPage, page)
	}
	if page == 0 {
		return 1, nil
	}
	return int(page), nil
}

// pageOffset 把「页码 + 页大小」换成 SQL OFFSET；页大小已由 svc.PageSize 夹过上限。
func pageOffset(page, size int) int32 {
	if page <= 1 || size <= 0 {
		return 0
	}
	return int32((page - 1) * size)
}

// decodeIDCursor 解析 session_id 型游标。空串表示第一页；
// 不可解析或非正数一律报错——退化成「当作第一页」会让客户端以为自己翻到了开头。
func decodeIDCursor(cursor string) (int64, error) {
	trimmed := strings.TrimSpace(cursor)
	if trimmed == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", model.ErrCursorInvalid, cursor)
	}
	if id <= 0 {
		return 0, fmt.Errorf("%w: %q must be a positive id", model.ErrCursorInvalid, cursor)
	}
	return id, nil
}

func encodeIDCursor(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// nextCursor 只在「这一页恰好被填满」时给出游标，否则空串表示到底。
func nextCursor(rows []*model.LiveSession, limit int) string {
	if len(rows) == 0 || limit <= 0 || len(rows) < limit {
		return ""
	}
	last := rows[len(rows)-1]
	if last == nil {
		return ""
	}
	return encodeIDCursor(last.SessionID)
}

// --- 幂等 / 事件去重接线 ---

// claimDedup 抢占去重键（request_id 或 event_id）。
// first=false 表示重投/重试，调用方必须回查结果后返回，不得再次产生副作用。
func claimDedup(ctx context.Context, s *svc.ServiceContext, rpcName, dedupID string, kind int32,
	roomID, sessionID int64, traceID string) (bool, error) {
	switch kind {
	case model.IdempotencyKindRequest, model.IdempotencyKindEvent:
	default:
		return false, fmt.Errorf("%w: kind=%d", model.ErrIdempotencyKindInvalid, kind)
	}
	return s.Idempotency.Claim(ctx, &model.LiveRoomIdempotency{
		DedupKey:  dedupID,
		Kind:      kind,
		Rpc:       rpcName,
		RoomID:    roomID,
		SessionID: sessionID,
		TraceID:   sanitizeTraceID(traceID),
	})
}

// dedupRecord 回查去重记录，并按 RPC 名判定「同一个键被两个方法用过」。
// dedup_key 是全局唯一键（迁移 000008 的 uniq_dedup_key），契约里没有按方法分域的字段，
// 因此这里只能显式报错，不能把别人的结果当自己的回放出去。
func dedupRecord(ctx context.Context, s *svc.ServiceContext, rpcName, dedupID string) (*model.LiveRoomIdempotency, error) {
	rec, err := s.Idempotency.Find(ctx, dedupID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		// 记录不存在但 Claim 说不是首次：清理任务刚删掉它（保留窗口被压短）。
		return nil, fmt.Errorf("%w: %s not found after claim", model.ErrIdempotencyResultMissing, dedupID)
	}
	if rec.Rpc != "" && rec.Rpc != rpcName {
		return nil, fmt.Errorf("%w: used by %s", model.ErrRequestIDReused, rec.Rpc)
	}
	return rec, nil
}

// unmarshalResult 把结果快照反序列化回 reply；raw 为空表示首次执行还没回填完。
func unmarshalResult(raw string, into proto.Message) error {
	if strings.TrimSpace(raw) == "" {
		return model.ErrIdempotencyResultMissing
	}
	if err := json.Unmarshal([]byte(raw), into); err != nil {
		return fmt.Errorf("%w: unmarshal idempotent result: %v", model.ErrIdempotencyResultMissing, err)
	}
	return nil
}

// saveDedupResult 回填首次执行的响应快照。失败只记日志：副作用已经产生，
// 不能因为「无法记下结果」再对外报错，否则调用方会以为可以安全重试。
func saveDedupResult(ctx context.Context, s *svc.ServiceContext, dedupID string, msg proto.Message, logger logx.Logger) {
	raw, err := json.Marshal(msg)
	if err != nil {
		logx.Errorf("liveroom/logic: marshal idempotent result for %s: %v", dedupID, err)
		return
	}
	if _, err := s.Idempotency.SaveResult(ctx, dedupID, string(raw)); err != nil {
		if logger != nil {
			logger.Errorf("liveroom/logic: save idempotent result for %s: %v", dedupID, err)
			return
		}
		logx.Errorf("liveroom/logic: save idempotent result for %s: %v", dedupID, err)
	}
}

// --- 状态机裁决（纯函数，供 logic 与单测共用）---

// roomStateAfterBanLift 是解禁落点：审核通过回 READY，否则回 PENDING。
// 与 model.roomTransitions 里 BANNED 的两条出边一一对应，禁止「无脑回 READY」。
func roomStateAfterBanLift(verifyState int32) int32 {
	if verifyState == model.VerifyStatePassed {
		return model.RoomStateReady
	}
	return model.RoomStatePending
}

// verifyTargetForVerdict 把 moderation 结论翻译成资料审核状态目标。
// 返回 changed=false 表示无需迁移（REVIEW 落在已经是 REVIEWING 的房间上）。
func verifyTargetForVerdict(cur int32, verdict rpc.ModerationVerdict) (target int32, changed bool, err error) {
	switch verdict {
	case rpc.ModerationVerdict_VERDICT_PASS:
		target = model.VerifyStatePassed
	case rpc.ModerationVerdict_VERDICT_REJECT:
		target = model.VerifyStateRejected
	case rpc.ModerationVerdict_VERDICT_REVIEW:
		target = model.VerifyStateReviewing
	default:
		return 0, false, fmt.Errorf("%w: %d", model.ErrVerdictInvalid, int32(verdict))
	}
	if cur == target {
		return target, false, nil
	}
	if !model.CanVerifyTransition(cur, target) {
		return target, false, fmt.Errorf("%w: %d->%d", model.ErrInvalidVerifyTransition, cur, target)
	}
	return target, true, nil
}

// roomStateForVerifyResult 给出「资料结论落定后房间该去哪」。
// 只有两条合法边：Pending->Ready（结论通过）、Ready->Pending（结论驳回）。
// 返回 ok=false 表示当前房间状态不允许降级/升级（例如仍在 LIVING），
// 调用方必须只回写审核结论、不得伪造状态迁移。
// BANNED 房间永远不因资料通过而解封（只有 LiftBan 能改）。
func roomStateForVerifyResult(verifyTarget, roomState int32) (to int32, ok bool) {
	switch {
	case verifyTarget == model.VerifyStatePassed && roomState == model.RoomStatePending:
		return model.RoomStateReady, true
	case verifyTarget == model.VerifyStateRejected && roomState == model.RoomStateReady:
		return model.RoomStatePending, true
	default:
		return 0, false
	}
}

// verifyTargetAfterResubmit 是改资料后重审的落点：合法则置 REVIEWING，
// 否则保持原值（保持原值仍然会清掉旧驳回原因，见 model.UpdateProfile）。
func verifyTargetAfterResubmit(cur int32) int32 {
	if model.CanVerifyTransition(cur, model.VerifyStateReviewing) {
		return model.VerifyStateReviewing
	}
	return cur
}

// allowEndReasonForEndLive 锁定 EndLive 入口可写的终止原因：
// BANNED / ROOM_CLOSED / STREAM_TIMEOUT 分别由 BanRoom / CloseRoom / ReportStreamState 写，
// 让客户端自选就等于允许伪造「被禁播」。
func allowEndReasonForEndLive(reason int32) bool {
	return reason == model.EndReasonAnchorStop || reason == model.EndReasonStreamReplay
}

// allowReplayTarget 锁定 AttachReplay 的目标回放状态。
// NONE 只能由场次建档产生；UNSPECIFIED 是非法入参。
func allowReplayTarget(target int32) bool {
	switch target {
	case model.ReplayStateProcessing, model.ReplayStateAvailable, model.ReplayStateRemoved:
		return true
	default:
		return false
	}
}

// roomInfoEditableStates 是 UpdateProfile 的 allowStates 口径：
// 改分区会改变房间的发现归属，因此只允许非直播态；改标题/封面在直播中也允许
// （已落库的场次快照不受影响，见 model.LiveSession.TitleSnapshot 注释）。
func roomInfoEditableStates(changingArea bool) []int32 {
	if changingArea {
		return []int32{model.RoomStatePending, model.RoomStateReady}
	}
	return []int32{model.RoomStatePending, model.RoomStateReady, model.RoomStateLiving}
}

// canEditRoomInfo 判定绑定角色是否可改房间资料：房主与生效联合主播可以，房管不行。
func canEditRoomInfo(role int32) bool {
	return role == model.AnchorRoleOwner || role == model.AnchorRoleCohost
}

// ownerLimitStates 是 CreateRoom 上限校验计入的房间状态（含终态 FINISHED：
// 关闭房间不返还额度，否则「关掉再建」可以无限刷新 room_id）。
func ownerLimitStates() []int32 {
	return []int32{
		model.RoomStatePending, model.RoomStateReady, model.RoomStateLiving,
		model.RoomStateFinished, model.RoomStateBanned, model.RoomStateDisabled,
	}
}

// areaOccupancyStates 是「分区被占用」判定的房间状态集合（未关闭的房间）。
func areaOccupancyStates() []int32 {
	return []int32{
		model.RoomStatePending, model.RoomStateReady, model.RoomStateLiving,
		model.RoomStateBanned, model.RoomStateDisabled,
	}
}

// banEndAt 计算禁播到期时间：永久禁播恒为 0（只能 LiftBan 解除），
// 临时禁播必须正数时长并做溢出保护。
func banEndAt(banType int32, duration, start int64) (int64, error) {
	switch banType {
	case model.BanTypePermanent:
		return 0, nil
	case model.BanTypeTemporary:
		if duration <= 0 {
			return 0, model.ErrBanDurationRequired
		}
		if start <= 0 {
			start = 0
		}
		if duration > int64(^uint64(0)>>1)-start {
			return 0, fmt.Errorf("%w: duration overflow", model.ErrBanDurationRequired)
		}
		return start + duration, nil
	default:
		return 0, model.ErrBanTypeInvalid
	}
}

// streamPlan 是 ReportStreamState 对单个 live.state.v1 事件的裁决结果。
type streamPlan struct {
	// sessionTo 是场次目标状态；0 表示不改场次状态。
	sessionTo int32
	// endReason 是进入终态时的终止原因（由本服务裁决，不接受调用方指定）。
	endReason int32
	// roomTo 是房间目标状态；0 表示不改房间状态。
	roomTo int32
	// bumpOnly 表示「观测型事件」：只推进 last_stream_seq，不产生状态迁移。
	bumpOnly bool
	// noChange 表示连 seq 都不推进（Idle：既非开播也非停止，记录它没有乱序守卫价值）。
	noChange bool
	// result 是 ReportStreamStateReply.result。
	result int32
	// message 是给消费方的说明（不含下游原始响应与凭据）。
	message string
}

// streamEventPlan 把 live-ingest 的流状态映射成本服务的状态迁移。
//
// 边界（README §1）：本服务不拥有流状态，只按此映射推进房间/场次；
// 断流超时不是「收到 INTERRUPTED 就终止」，而是中断累计秒数达到宽限期才终止，
// 否则每次网络抖动都会把在播场次判死。
func streamEventPlan(streamState, sessionState int32, interruptedSeconds int64, graceSeconds int32) (streamPlan, error) {
	if !model.ValidStreamState(streamState) {
		return streamPlan{}, fmt.Errorf("%w: %d", model.ErrStreamStateInvalid, streamState)
	}
	if !model.ValidSessionState(sessionState) {
		return streamPlan{}, fmt.Errorf("%w: %d", model.ErrInvalidSessionTransition, sessionState)
	}
	switch streamState {
	case model.StreamStatePublishing:
		if model.SessionStateIsTerminal(sessionState) {
			return streamPlan{noChange: true, result: model.StreamResultIllegalTransition,
				message: "场次已终态，推流事件不再改变状态"}, nil
		}
		if sessionState == model.SessionStateLiving {
			// 心跳：状态已在位，只需推进 seq，保持乱序守卫有效。
			return streamPlan{bumpOnly: true, result: model.StreamResultApplied,
				message: "推流心跳，无状态迁移"}, nil
		}
		return streamPlan{sessionTo: model.SessionStateLiving, roomTo: model.RoomStateLiving,
			result: model.StreamResultApplied, message: "推流到达，场次进入直播中"}, nil

	case model.StreamStateInterrupted:
		if model.SessionStateIsTerminal(sessionState) {
			return streamPlan{noChange: true, result: model.StreamResultIllegalTransition,
				message: "场次已终态，中断事件不再改变状态"}, nil
		}
		if graceSeconds > 0 && interruptedSeconds >= int64(graceSeconds) {
			return streamPlan{sessionTo: model.SessionStateTerminated,
				endReason: model.EndReasonStreamTimeout, roomTo: model.RoomStateReady,
				result: model.StreamResultApplied, message: "断流超过宽限期，场次终止"}, nil
		}
		return streamPlan{bumpOnly: true, result: model.StreamResultApplied,
			message: "断流宽限期内，仅记录观测"}, nil

	case model.StreamStateStopped:
		if model.SessionStateIsTerminal(sessionState) {
			return streamPlan{noChange: true, result: model.StreamResultIllegalTransition,
				message: "场次已终态，停止事件不再回拨状态"}, nil
		}
		return streamPlan{sessionTo: model.SessionStateTerminated,
			endReason: model.EndReasonStreamReplay, roomTo: model.RoomStateReady,
			result: model.StreamResultApplied, message: "推流停止，场次终止并释放房间直播态"}, nil

	case model.StreamStateIdle:
		// 契约里 Idle 表示「流空闲」，既不是开播也不是结束：不产生迁移，也不推进 seq。
		return streamPlan{noChange: true, result: model.StreamResultIllegalTransition,
			message: "IDLE 不产生状态迁移"}, nil
	default:
		return streamPlan{noChange: true, result: model.StreamResultIllegalTransition,
			message: "未定义的流状态映射"}, nil
	}
}

// staleStreamResult 在 CAS 未命中后区分两种失败：seq 陈旧 还是 状态已被别人推进。
// 事件入口不能靠猜——回错 result 会让消费方决定「重投」还是「丢弃」，直接影响收敛。
func staleStreamResult(session *model.LiveSession, seq int64) (int32, string) {
	if session == nil {
		return model.StreamResultMismatch, "场次不存在"
	}
	if seq <= session.LastStreamSeq {
		return model.StreamResultStale, "事件序号不高于已应用序号，按乱序丢弃"
	}
	return model.StreamResultIllegalTransition, "场次状态已被并发推进，本次迁移未生效"
}

// --- 配置对象 ---

// defaultSettingRow 是服务端默认配置：弹幕/回复开启，录制与连麦默认关闭
// （回放是显式 opt-in 能力，缺配就把用户直播内容送去录制属于越权）。
func defaultSettingRow(roomID int64) *model.LiveRoomSetting {
	return &model.LiveRoomSetting{
		RoomID:               roomID,
		DanmakuEnabled:       model.BoolToInt32(true),
		ReplyEnabled:         model.BoolToInt32(true),
		RecordEnabled:        model.BoolToInt32(false),
		LinkmicEnabled:       model.BoolToInt32(false),
		LiveType:             model.LiveTypeVideo,
		MinClientVersionCode: 0,
	}
}

// settingRowFromRequest 把 rpc.RoomSetting 落成 model 行。
// in==nil 表示「按服务端默认」；live_type=0（客户端未填）归一为视频直播，
// 其余非 0 的未定义取值拒绝——把 0 当非法会让「只改弹幕开关」的调用整体失败。
func settingRowFromRequest(roomID int64, in *rpc.RoomSetting) (*model.LiveRoomSetting, error) {
	row := defaultSettingRow(roomID)
	if in == nil {
		return row, nil
	}
	liveType := in.GetLiveType()
	if liveType == 0 {
		liveType = model.LiveTypeVideo
	}
	if !model.ValidLiveType(liveType) {
		return nil, fmt.Errorf("%w: live_type=%d", model.ErrSettingInvalid, in.GetLiveType())
	}
	if in.GetMinClientVersionCode() < 0 {
		return nil, fmt.Errorf("%w: min_client_version_code=%d", model.ErrSettingInvalid, in.GetMinClientVersionCode())
	}
	row.DanmakuEnabled = model.BoolToInt32(in.GetDanmakuEnabled())
	row.ReplyEnabled = model.BoolToInt32(in.GetReplyEnabled())
	row.RecordEnabled = model.BoolToInt32(in.GetRecordEnabled())
	row.LinkmicEnabled = model.BoolToInt32(in.GetLinkmicEnabled())
	row.LiveType = liveType
	row.MinClientVersionCode = in.GetMinClientVersionCode()
	return row, nil
}

// --- RoomPatch 组装辅助 ---

func i32Ptr(v int32) *int32   { return &v }
func i64Ptr(v int64) *int64   { return &v }
func strPtr(v string) *string { return &v }

// clearActiveSessionPatch 把「清当前场次投影」并进状态迁移那条 UPDATE，
// 避免出现「状态已 READY 但 active_session_id 还指着老场次」的中间态。
func clearActiveSessionPatch() model.RoomPatch {
	return model.RoomPatch{ActiveSessionID: i64Ptr(0), ActiveStreamID: strPtr("")}
}

// --- 缓存（只加速，真值恒在 MySQL）---

func roomCacheKey(roomID int64) string { return roomCacheKeyPrefix + strconv.FormatInt(roomID, 10) }

// areaListCacheKey 覆盖 parent_area_id / state / 分页三元组。
// state 与 parent 用 -1 表达「不过滤」，因此不能把 0 当缺省省掉。
func areaListCacheKey(parentAreaID int64, state, page, size int32) string {
	return fmt.Sprintf("%s%d|%d|%d|%d", areaListCacheKeyPrefix, parentAreaID, state, page, size)
}

// cachedRoom 读房间详情缓存；任何异常（未配置、未命中、脏数据）都返回 nil 回源。
func cachedRoom(ctx context.Context, s *svc.ServiceContext, roomID int64) *rpc.RoomInfo {
	ttl := s.Config.LiveRoom.RoomCacheTTLSeconds
	if s.Cache == nil || ttl <= 0 || roomID <= 0 {
		return nil
	}
	raw, err := s.Cache.GetCtx(ctx, roomCacheKey(roomID))
	if err != nil || raw == "" {
		return nil
	}
	info := &rpc.RoomInfo{}
	if err := json.Unmarshal([]byte(raw), info); err != nil {
		logx.Errorf("liveroom/logic: bad room cache for %d: %v", roomID, err)
		return nil
	}
	if info.GetRoomId() != roomID {
		// 键值不一致说明缓存被别的口径写过，宁缺不滥。
		return nil
	}
	return info
}

func cacheRoom(ctx context.Context, s *svc.ServiceContext, info *rpc.RoomInfo) {
	ttl := s.Config.LiveRoom.RoomCacheTTLSeconds
	if s.Cache == nil || ttl <= 0 || info == nil || info.GetRoomId() <= 0 {
		return
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return
	}
	if err := s.Cache.SetexCtx(ctx, roomCacheKey(info.GetRoomId()), string(raw), ttl); err != nil {
		logx.Errorf("liveroom/logic: set room cache %d: %v", info.GetRoomId(), err)
	}
}

// invalidateRoomCache 在所有写路径末尾调用；缓存缺失只回源，不影响正确性。
func invalidateRoomCache(ctx context.Context, s *svc.ServiceContext, roomID int64) {
	if s.Cache == nil || roomID <= 0 {
		return
	}
	if _, err := s.Cache.DelCtx(ctx, roomCacheKey(roomID)); err != nil {
		logx.Errorf("liveroom/logic: del room cache %d: %v", roomID, err)
	}
}

func invalidateAreaListCache(ctx context.Context, s *svc.ServiceContext) {
	if s.Cache == nil {
		return
	}
	// 分区列表的键带了查询三元组，无法枚举；用 SCAN 成本高且会漂移，
	// 因此这里的失效窗口就是 AreaListCacheTTLSeconds（运营改分区是低频动作）。
	// 真正的即时失效需要按前缀维护索引键，属后续工作，已在 README 记为已知缺口。
	_ = ctx
}

// --- 错误聚合 ---

// firstErr 返回第一个非 nil 错误，用于「清理动作不掩盖主错误」的场合。
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil && !errors.Is(e, errNoOp) {
			return e
		}
	}
	return nil
}

// errNoOp 是内部控制流标记：该分支无事可做，不算错误也不改变状态。
var errNoOp = errors.New("liveroom/logic: no-op")
