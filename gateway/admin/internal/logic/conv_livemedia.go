// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// live-media RPC ↔ 管理后台投影 + 写入口门槛。
//
// 职责边界（AGENTS.md §4/§5/§8），与 conv_live.go / conv_live_ingest.go / conv_live_gateway.go
// 同一套口径：
//  1. 网关只做三件事：入参形态门槛（会话身份、幂等键非空、必填主键、数值非负、枚举目标位非
//     UNSPECIFIED、引用成对、时间/序号窗口不倒置）、调用下游、逐字段投影。
//     「任务当前状态能不能停/重试/取消」「attempt 是否用尽」「切片区间是否完整」「档位是否仍被引用」
//     「回收对象是否还在保留期内」全部由 services/live-media 判定；网关不复算，也不把下游错误
//     改写成看起来成功的空结果；
//  2. 本域的操作者字段是 `operator string`（审计字符串，不是用户 mid 空间），凡契约里有这一位的
//     方法（StopLiveTranscode / RetryLiveTranscode / CancelLiveTranscode / StopLiveRecord /
//     SubmitRetentionTask）都由会话 admin_id 渲染成 admin:<admin_id>，表单不得自报身份。
//     其余七个写方法（Start 转码/录制、Upsert/Offline 档位、SubmitReplayTask、BindReplayAsset、
//     ApplyReplayContentState）在 proto 里**没有** operator 位：网关仍然要求会话身份存在
//     （liveMediaOperator 返回的字符串只进日志、不进请求），这样「未登录的点按钮」在出网关前
//     就被拒掉，而「谁点的」只能从网关日志回溯 —— 该缺口逐条记在 admin.api 头部注释与
//     gateway/admin/README.md，本轮不改 services/**；
//  3. 凭据不回显也不代为生成：source_ref / bucket / object_key / cdn_domain / checksum 都只是
//     引用与摘要（proto 注明不含签名与密钥），因此可以整行回后台；反过来网关不会为任何字段
//     伪造默认值（幂等键改动会失去语义，故只判空不改写）；
//  4. ApplyReplayContentState 的 source 由网关固定为 `manual`：后台点出来的投影刷新既不可能
//     来自 content.published.v1 事件，也不可能来自 video.rpc 回调，让表单自选来源会让引用行的
//     投影留痕失真；
//  5. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// errLiveMediaNotConfigured：未配置 LiveMediaRPC 时 live-media 域 23 条路由一律返回它。
// 不退化成伪造空列表/零值任务——那会让后台把「下游没接」读成「这个房间没有在跑的转码、
// 录制与回放任务」，进而误判直播链路健康。
var errLiveMediaNotConfigured = errors.New("live-media service not configured")

// liveMediaContentStateSource：ApplyReplayContentState 的固定来源标记（见文件头第 4 条）。
const liveMediaContentStateSource = "manual"

// liveMediaOperator 是写入口的统一门槛：会话身份必须存在（AdminPermission 判定通过后才会挂上），
// 并由它生成服务侧审计用的 operator 字符串。
//
// 返回值只用于**有 operator 位**的五个方法；没有该位的方法照样调用本函数，把点按钮的账号
// 落进网关日志（logx 里只有路由名与 admin_id：reason 正文、对象存储引用、稿件标题都不进日志，
// AGENTS.md §4）。
func liveMediaOperator(ctx context.Context, route string) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errLiveSessionRequired
	}
	operator := fmt.Sprintf("admin:%d", id.AdminID)
	logx.WithContext(ctx).Infof("gateway/admin/%s: operator=%s", route, operator)
	return operator, nil
}

// liveMediaSessionGate 是**契约里没有 operator 位**的写入口（Start 转码 / Start 录制 /
// Upsert 与 Offline 档位 / SubmitReplayTask / BindReplayAsset / ApplyReplayContentState）的门槛：
// 只要求会话身份存在，并把路由名与 admin_id 落进网关日志。
//
// 这些方法的任务行/引用行看不到操作者（proto 缺口，逐条记在 admin.api 头部与 README），
// 但「未登录就点按钮」必须在出网关之前被拒 —— 身份判定与有 operator 位的五条完全一致，
// 差别只在结论落不到下游行上。
func liveMediaSessionGate(ctx context.Context, route string) error {
	_, err := liveMediaOperator(ctx, route)
	return err
}

// liveMediaIdempotencyGate 拦住空幂等键：幂等键的任何改写（含 TrimSpace 后回写）都会让它失去
// 语义，因此只判空、不改值，原样透传给 live-media 的 request_id。
func liveMediaIdempotencyGate(requestID string) error {
	return requireNonEmpty("request_id", requestID)
}

// liveMediaPage 组装 PageParam。pn/ps 的取值合法性（ps 上限 50、pn 下界）由 live-media 夹取，
// 网关只做非负门槛，不复算分页。
func liveMediaPage(pn, ps int32) (*livemediarpc.PageParam, error) {
	if err := liveNonNeg32("pn", pn); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("ps", ps); err != nil {
		return nil, err
	}
	return &livemediarpc.PageParam{Pn: pn, Ps: ps}, nil
}

// liveMediaEnum 挡住「必填枚举位取 UNSPECIFIED」。本域的 reason / review_state / target_kind /
// bitrate_level / protocol 在 proto 里都有 0=UNSPECIFIED，但作为**写目标**时 0 没有语义：
// 停止/取消不下原因、投影刷新不落审核结论、档位不知道是哪一路，服务只会按各自的默认分支处理，
// 后台看到的是一次「成功但什么都没发生」的处置。上界（哪些取值合法、状态机允许哪条迁移）
// 仍是 live-media 的判定域，网关不复算。
func liveMediaEnum(field string, v int32) error {
	if v <= 0 {
		return fmt.Errorf("gateway/admin: %s required (0 = *_UNSPECIFIED)", field)
	}
	return nil
}

// liveMediaWindow 时间窗口门槛：两端各自非负由调用方先挡，这里只挡倒置。
// start_at/end_at、from_seq/to_seq 都是「0 = 不限制这一端」的哨兵，只有两端都给值时才可比。
func liveMediaWindow(fieldFrom, fieldTo string, from, to int64) error {
	if from > 0 && to > 0 && from > to {
		return fmt.Errorf("gateway/admin: %s must be <= %s", fieldFrom, fieldTo)
	}
	return nil
}

// liveMediaRefPair 引用成对门槛：bucket 与 object_key（或 output_bucket / output_prefix、
// cdn_domain 之外的路径字段）只给一半时这一条引用是不可用的 —— 服务无从校验对象是否存在，
// 后台却会看到一个「有桶没键」的行，排障时反而更绕。
func liveMediaRefPair(nameFrom, nameTo string, from, to string) error {
	if (from == "") != (to == "") {
		return fmt.Errorf("gateway/admin: %s and %s must be given together", nameFrom, nameTo)
	}
	return nil
}

// liveMediaOutputSubjectGate 下线档位的寻址门槛：output_id，或 (room_id, bitrate_level, protocol)
// 三元组。两者都不全时服务无从定位是哪一路（回给后台的是「输出不存在」而不是「你少传了参数」）。
func liveMediaOutputSubjectGate(outputID, roomID int64, level, protocol int32) error {
	if outputID > 0 {
		return liveNonNeg("room_id", roomID)
	}
	if roomID > 0 && level > 0 && protocol > 0 {
		return nil
	}
	return errors.New("gateway/admin: output_id or (room_id, bitrate_level, protocol) required")
}

// liveMediaReplaySubjectGate 投影刷新的寻址门槛：replay_id 与 asset_id 至少一个。
// 都不给时服务无从定位引用行，与 offline 同一口径先拒。
func liveMediaReplaySubjectGate(replayID, assetID int64) error {
	if replayID <= 0 && assetID <= 0 {
		return errors.New("gateway/admin: replay_id or asset_id required")
	}
	if err := liveNonNeg("replay_id", replayID); err != nil {
		return err
	}
	return liveNonNeg("asset_id", assetID)
}

// liveMediaAssetRefGate 回填引用的主体门槛：asset_id 与 aid 至少一个。
// 两者都为空时这一行没有任何外部引用可存（BindReplayAsset 是「只写引用」的表，
// 空引用行既不指向媒资也不指向稿件，纯属噪声）。
func liveMediaAssetRefGate(assetID, aid int64) error {
	if assetID <= 0 && aid <= 0 {
		return errors.New("gateway/admin: asset_id or aid required")
	}
	if err := liveNonNeg("asset_id", assetID); err != nil {
		return err
	}
	return liveNonNeg("aid", aid)
}

// --- rpc → 后台 types 投影 ---

// liveMediaTranscodeTaskToAPI 转码任务整行投影。
// version / heartbeat_at / timeout_at / attempt / max_attempts 一个都不裁：
// 它们是运营判断「要不要停、能不能重试」的唯一依据，也是回传 expected_version 的来源。
// errno/err_msg 在契约层已脱敏（proto 注明不含密钥与完整 URL）。
func liveMediaTranscodeTaskToAPI(p *livemediarpc.LiveTranscodeTaskInfo) types.LiveMediaTranscodeTaskInfo {
	if p == nil {
		return types.LiveMediaTranscodeTaskInfo{}
	}
	return types.LiveMediaTranscodeTaskInfo{
		TaskId:        p.GetTaskId(),
		RoomId:        p.GetRoomId(),
		LiveSessionId: p.GetLiveSessionId(),
		TemplateId:    p.GetTemplateId(),
		BitrateLevel:  int32(p.GetBitrateLevel()),
		Protocol:      int32(p.GetProtocol()),
		SourceRef:     p.GetSourceRef(),
		AnchorMid:     p.GetAnchorMid(),
		State:         int32(p.GetState()),
		Progress:      p.GetProgress(),
		Attempt:       p.GetAttempt(),
		MaxAttempts:   p.GetMaxAttempts(),
		StartedAt:     p.GetStartedAt(),
		StoppedAt:     p.GetStoppedAt(),
		HeartbeatAt:   p.GetHeartbeatAt(),
		TimeoutAt:     p.GetTimeoutAt(),
		Version:       p.GetVersion(),
		Reason:        int32(p.GetReason()),
		Errno:         p.GetErrno(),
		ErrMsg:        p.GetErrMsg(),
		RequestId:     p.GetRequestId(),
		TraceId:       p.GetTraceId(),
		Ctime:         p.GetCtime(),
		Mtime:         p.GetMtime(),
	}
}

func liveMediaTranscodeTasksToAPI(list []*livemediarpc.LiveTranscodeTaskInfo) []types.LiveMediaTranscodeTaskInfo {
	out := make([]types.LiveMediaTranscodeTaskInfo, 0, len(list))
	for _, item := range list {
		out = append(out, liveMediaTranscodeTaskToAPI(item))
	}
	return out
}

// liveMediaStreamOutputToAPI 分发档位投影。state 在 proto 里就是 int32（1 在线、2 已下线），
// 不是枚举，网关按值透传；reason 是下线原因（state=2 时才有意义）。
func liveMediaStreamOutputToAPI(p *livemediarpc.StreamOutputInfo) types.LiveMediaStreamOutputInfo {
	if p == nil {
		return types.LiveMediaStreamOutputInfo{}
	}
	return types.LiveMediaStreamOutputInfo{
		OutputId:       p.GetOutputId(),
		RoomId:         p.GetRoomId(),
		LiveSessionId:  p.GetLiveSessionId(),
		TaskId:         p.GetTaskId(),
		BitrateLevel:   int32(p.GetBitrateLevel()),
		Protocol:       int32(p.GetProtocol()),
		Bucket:         p.GetBucket(),
		ObjectKey:      p.GetObjectKey(),
		CdnDomain:      p.GetCdnDomain(),
		Width:          p.GetWidth(),
		Height:         p.GetHeight(),
		BitrateKbps:    p.GetBitrateKbps(),
		Fps:            p.GetFps(),
		State:          p.GetState(),
		OnlineAt:       p.GetOnlineAt(),
		OfflineAt:      p.GetOfflineAt(),
		OnlineExpireAt: p.GetOnlineExpireAt(),
		RequestId:      p.GetRequestId(),
		Ctime:          p.GetCtime(),
		Mtime:          p.GetMtime(),
		Reason:         int32(p.GetReason()),
	}
}

func liveMediaStreamOutputsToAPI(list []*livemediarpc.StreamOutputInfo) []types.LiveMediaStreamOutputInfo {
	out := make([]types.LiveMediaStreamOutputInfo, 0, len(list))
	for _, item := range list {
		out = append(out, liveMediaStreamOutputToAPI(item))
	}
	return out
}

// liveMediaRecordTaskToAPI 录制任务整行。last_seq / segment_count / gap_count /
// recorded_duration_ms 是「断点续录从哪继续」与「回放有没有洞」的读数，全部保留。
func liveMediaRecordTaskToAPI(p *livemediarpc.LiveRecordTaskInfo) types.LiveMediaRecordTaskInfo {
	if p == nil {
		return types.LiveMediaRecordTaskInfo{}
	}
	return types.LiveMediaRecordTaskInfo{
		RecordId:           p.GetRecordId(),
		RoomId:             p.GetRoomId(),
		LiveSessionId:      p.GetLiveSessionId(),
		SourceTaskId:       p.GetSourceTaskId(),
		State:              int32(p.GetState()),
		StartAt:            p.GetStartAt(),
		EndAt:              p.GetEndAt(),
		RecordStartAt:      p.GetRecordStartAt(),
		RecordEndAt:        p.GetRecordEndAt(),
		SegmentSeconds:     p.GetSegmentSeconds(),
		LastSeq:            p.GetLastSeq(),
		SegmentCount:       p.GetSegmentCount(),
		GapCount:           p.GetGapCount(),
		RecordedDurationMs: p.GetRecordedDurationMs(),
		OutputBucket:       p.GetOutputBucket(),
		OutputPrefix:       p.GetOutputPrefix(),
		HeartbeatAt:        p.GetHeartbeatAt(),
		TimeoutAt:          p.GetTimeoutAt(),
		Version:            p.GetVersion(),
		Reason:             int32(p.GetReason()),
		Errno:              p.GetErrno(),
		ErrMsg:             p.GetErrMsg(),
		RequestId:          p.GetRequestId(),
		TraceId:            p.GetTraceId(),
		Ctime:              p.GetCtime(),
		Mtime:              p.GetMtime(),
	}
}

func liveMediaRecordTasksToAPI(list []*livemediarpc.LiveRecordTaskInfo) []types.LiveMediaRecordTaskInfo {
	out := make([]types.LiveMediaRecordTaskInfo, 0, len(list))
	for _, item := range list {
		out = append(out, liveMediaRecordTaskToAPI(item))
	}
	return out
}

// liveMediaRecordSegmentToAPI 切片投影：MISSING(4)/CORRUPT(5) 是时间轴上的洞与坏片，
// 后台必须看得见，否则一段少了十分钟的回放看起来是完整的。
func liveMediaRecordSegmentToAPI(p *livemediarpc.RecordSegmentInfo) types.LiveMediaRecordSegmentInfo {
	if p == nil {
		return types.LiveMediaRecordSegmentInfo{}
	}
	return types.LiveMediaRecordSegmentInfo{
		Id:            p.GetId(),
		RecordId:      p.GetRecordId(),
		RoomId:        p.GetRoomId(),
		LiveSessionId: p.GetLiveSessionId(),
		Seq:           p.GetSeq(),
		StartAt:       p.GetStartAt(),
		EndAt:         p.GetEndAt(),
		DurationMs:    p.GetDurationMs(),
		State:         int32(p.GetState()),
		Bucket:        p.GetBucket(),
		ObjectKey:     p.GetObjectKey(),
		SizeBytes:     p.GetSizeBytes(),
		Checksum:      p.GetChecksum(),
		WorkerId:      p.GetWorkerId(),
		RegisteredAt:  p.GetRegisteredAt(),
		Mtime:         p.GetMtime(),
	}
}

func liveMediaRecordSegmentsToAPI(list []*livemediarpc.RecordSegmentInfo) []types.LiveMediaRecordSegmentInfo {
	out := make([]types.LiveMediaRecordSegmentInfo, 0, len(list))
	for _, item := range list {
		out = append(out, liveMediaRecordSegmentToAPI(item))
	}
	return out
}

// liveMediaReplayTaskToAPI 回放任务整行。asset_id / aid / bvid 只是引用，
// bvid 是冗余展示字段（稿件事实源在 video），网关不在这里跨服务查标题。
func liveMediaReplayTaskToAPI(p *livemediarpc.LiveReplayTaskInfo) types.LiveMediaReplayTaskInfo {
	if p == nil {
		return types.LiveMediaReplayTaskInfo{}
	}
	return types.LiveMediaReplayTaskInfo{
		ReplayId:      p.GetReplayId(),
		RoomId:        p.GetRoomId(),
		LiveSessionId: p.GetLiveSessionId(),
		RecordId:      p.GetRecordId(),
		State:         int32(p.GetState()),
		FromSeq:       p.GetFromSeq(),
		ToSeq:         p.GetToSeq(),
		SegmentCount:  p.GetSegmentCount(),
		GapCount:      p.GetGapCount(),
		StartAt:       p.GetStartAt(),
		EndAt:         p.GetEndAt(),
		DurationMs:    p.GetDurationMs(),
		AllowGaps:     p.GetAllowGaps(),
		OutputBucket:  p.GetOutputBucket(),
		OutputKey:     p.GetOutputKey(),
		AssetId:       p.GetAssetId(),
		Aid:           p.GetAid(),
		Bvid:          p.GetBvid(),
		AnchorMid:     p.GetAnchorMid(),
		Title:         p.GetTitle(),
		Version:       p.GetVersion(),
		Reason:        int32(p.GetReason()),
		Errno:         p.GetErrno(),
		ErrMsg:        p.GetErrMsg(),
		RequestId:     p.GetRequestId(),
		TraceId:       p.GetTraceId(),
		Ctime:         p.GetCtime(),
		Mtime:         p.GetMtime(),
	}
}

func liveMediaReplayTasksToAPI(list []*livemediarpc.LiveReplayTaskInfo) []types.LiveMediaReplayTaskInfo {
	out := make([]types.LiveMediaReplayTaskInfo, 0, len(list))
	for _, item := range list {
		out = append(out, liveMediaReplayTaskToAPI(item))
	}
	return out
}

// liveMediaReplayAssetRefToAPI 引用行投影。review_state / review_state_at / published_at 是
// video 侧的**只读投影**（事实源不是 live-media），retention_state 是引用行的生命周期标记，
// 四者都按值透传，网关不在这里推断「回放到底能不能看」。
func liveMediaReplayAssetRefToAPI(p *livemediarpc.ReplayAssetRefInfo) types.LiveMediaReplayAssetRefInfo {
	if p == nil {
		return types.LiveMediaReplayAssetRefInfo{}
	}
	return types.LiveMediaReplayAssetRefInfo{
		Id:             p.GetId(),
		RoomId:         p.GetRoomId(),
		LiveSessionId:  p.GetLiveSessionId(),
		ReplayId:       p.GetReplayId(),
		RecordId:       p.GetRecordId(),
		AssetId:        p.GetAssetId(),
		Aid:            p.GetAid(),
		Bvid:           p.GetBvid(),
		AnchorMid:      p.GetAnchorMid(),
		Bucket:         p.GetBucket(),
		ObjectKey:      p.GetObjectKey(),
		DurationMs:     p.GetDurationMs(),
		SegmentFromSeq: p.GetSegmentFromSeq(),
		SegmentToSeq:   p.GetSegmentToSeq(),
		GapCount:       p.GetGapCount(),
		ReviewState:    int32(p.GetReviewState()),
		ReviewStateAt:  p.GetReviewStateAt(),
		RetentionState: p.GetRetentionState(),
		PublishedAt:    p.GetPublishedAt(),
		Ctime:          p.GetCtime(),
		Mtime:          p.GetMtime(),
	}
}

func liveMediaReplayAssetRefsToAPI(list []*livemediarpc.ReplayAssetRefInfo) []types.LiveMediaReplayAssetRefInfo {
	out := make([]types.LiveMediaReplayAssetRefInfo, 0, len(list))
	for _, item := range list {
		out = append(out, liveMediaReplayAssetRefToAPI(item))
	}
	return out
}

// liveMediaRetentionTaskToAPI 回收任务整行。scanned/deleted/skipped 是「先登记后执行」的凭证，
// purge=false 时 deleted 恒为 0 —— 这三列是运营核对「有没有误删」的唯一依据。
func liveMediaRetentionTaskToAPI(p *livemediarpc.LiveRetentionTaskInfo) types.LiveMediaRetentionTaskInfo {
	if p == nil {
		return types.LiveMediaRetentionTaskInfo{}
	}
	return types.LiveMediaRetentionTaskInfo{
		RetentionId:  p.GetRetentionId(),
		TargetKind:   int32(p.GetTargetKind()),
		RoomId:       p.GetRoomId(),
		TargetId:     p.GetTargetId(),
		ExpireBefore: p.GetExpireBefore(),
		Purge:        p.GetPurge(),
		BatchLimit:   p.GetBatchLimit(),
		State:        int32(p.GetState()),
		Scanned:      p.GetScanned(),
		Deleted:      p.GetDeleted(),
		Skipped:      p.GetSkipped(),
		Reason:       p.GetReason(),
		Operator:     p.GetOperator(),
		Version:      p.GetVersion(),
		FailReason:   int32(p.GetFailReason()),
		Errno:        p.GetErrno(),
		ErrMsg:       p.GetErrMsg(),
		RequestId:    p.GetRequestId(),
		TraceId:      p.GetTraceId(),
		Ctime:        p.GetCtime(),
		Mtime:        p.GetMtime(),
	}
}

func liveMediaRetentionTasksToAPI(list []*livemediarpc.LiveRetentionTaskInfo) []types.LiveMediaRetentionTaskInfo {
	out := make([]types.LiveMediaRetentionTaskInfo, 0, len(list))
	for _, item := range list {
		out = append(out, liveMediaRetentionTaskToAPI(item))
	}
	return out
}

// liveMediaPageTotal 回读 PageResult.total。服务未回 page 时返回 0 —— 这与「total 真的是 0」
// 在 HTTP 层无法区分，因此 total 只做展示，计数缺失由 live-media 侧决定，网关不猜。
// （切片列表的 total 是 int64 的独立字段，不走这里。）
func liveMediaPageTotal(page *livemediarpc.PageResult) int32 {
	return page.GetTotal()
}
