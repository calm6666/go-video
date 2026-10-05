package logic

import (
	"context"
	"fmt"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpsertStreamOutputLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertStreamOutputLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertStreamOutputLogic {
	return &UpsertStreamOutputLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登记或刷新一个码率档位的分发输出（(room,session,level,protocol) 唯一）
//
// live_stream_output 属「直播实时链路」，与回放发布状态完全无关（rpc 头部硬约束 2）。
// 幂等靠天然键 uniq_output_natural：重复登记不新增行，只刷新产物引用与参数快照，
// 并把 offline_at/offline_reason 复位（重新在线），ctime 保持首次登记值（审计事实）。
// request_id 是调用方归因字段而非唯一索引 —— 同一天然键会反复上下线，每次都刷新它。
//
// 只存引用：bucket + 相对 object_key + CDN 域名。签名地址、临时凭据一律拒绝（AGENTS.md §6），
// width/height/bitrate_kbps/fps 是下发时刻的参数快照，transcode 模板变更不回写历史行。
// online_expire_at>0 的到点下线由 MarkExpiredOffline 清扫（ReasonTimeout），本方法不承担定时职责。
func (l *UpsertStreamOutputLogic) UpsertStreamOutput(in *rpc.UpsertStreamOutputReq) (*rpc.StreamOutputInfo, error) {
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkSessionID(in.GetLiveSessionId()); err != nil {
		return nil, err
	}
	level := int32(in.GetBitrateLevel())
	protocol := int32(in.GetProtocol())
	if err := checkBitrateLevel(level); err != nil {
		return nil, err
	}
	if err := checkProtocol(protocol); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	requestID := normalizedRequestID(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())
	// 分发产物必须有可播的相对路径（m3u8 / 流路径）；签名参数一旦进这一列就是长期泄漏。
	bucket, objectKey, err := checkObjectRef(in.GetBucket(), in.GetObjectKey(), true, maxObjectKeyRunes)
	if err != nil {
		return nil, err
	}
	cdnDomain, err := checkCdnDomain(in.GetCdnDomain())
	if err != nil {
		return nil, err
	}
	for name, v := range map[string]int32{"width": in.GetWidth(), "height": in.GetHeight(),
		"bitrate_kbps": in.GetBitrateKbps(), "fps": in.GetFps()} {
		if v < 0 {
			return nil, fmt.Errorf("live-media: %s=%d must not be negative: %w", name, v, model.ErrInvalidBucketRef)
		}
	}
	if in.GetOnlineExpireAt() < 0 {
		return nil, fmt.Errorf("live-media: online_expire_at=%d must be 0 (offline with the stream) or positive: %w",
			in.GetOnlineExpireAt(), model.ErrInvalidTransition)
	}
	if in.GetTaskId() < 0 {
		return nil, fmt.Errorf("live-media: task_id=%d must be 0 (source stream direct output) or positive: %w",
			in.GetTaskId(), model.ErrTranscodeTaskNotFound)
	}
	// task_id>0 时只校验存在性与归属，不复制其主数据（模板/源地址的事实源在转码侧）。
	if taskID := in.GetTaskId(); taskID > 0 {
		task, findErr := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
		if findErr != nil {
			return nil, findErr
		}
		if task == nil {
			return nil, fmt.Errorf("live-media: task_id=%d: %w", taskID, model.ErrTranscodeTaskNotFound)
		}
		if task.RoomId != in.GetRoomId() {
			return nil, fmt.Errorf("live-media: task_id=%d belongs to room %d, not %d: %w",
				taskID, task.RoomId, in.GetRoomId(), model.ErrInvalidRoomID)
		}
	}
	if in.GetOnlineExpireAt() > 0 && in.GetOnlineExpireAt() <= model.NowUnix() {
		// 登记即过期的档位会被清扫器立刻下线：不拒绝（Worker 可能在重放旧指令），但必须留痕。
		l.Errorf("livemedia/UpsertStreamOutput: room=%d session=%d level=%d online_expire_at=%d already expired",
			in.GetRoomId(), in.GetLiveSessionId(), level, in.GetOnlineExpireAt())
	}

	now := model.NowUnix()
	output := &model.LiveStreamOutput{
		RoomId:       in.GetRoomId(),
		LiveSession:  in.GetLiveSessionId(),
		TaskId:       in.GetTaskId(),
		BitrateLevel: level,
		Protocol:     protocol,
		Bucket:       bucket,
		ObjectKey:    objectKey,
		CdnDomain:    cdnDomain,
		Width:        in.GetWidth(),
		Height:       in.GetHeight(),
		BitrateKbps:  in.GetBitrateKbps(),
		Fps:          in.GetFps(),
		State:        model.StreamOutputStateOnline,
		OnlineAt:     now,
		OnlineExpire: in.GetOnlineExpireAt(),
		RequestId:    requestID,
		TraceId:      traceID,
	}

	var outputID int64
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		id, upsertErr := l.svcCtx.StreamOutputs.UpsertTx(ctx, sess, output)
		if upsertErr != nil {
			return upsertErr
		}
		outputID = id
		// payload 只放标识与档位：bucket/object_key/cdn 由读接口给，事件里不带可播路径。
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeStreamOutputOnline,
			model.AggregateStreamOutput, outputID, output.RoomId, map[string]any{
				"output_id":       outputID,
				"room_id":         output.RoomId,
				"live_session_id": output.LiveSession,
				"bitrate_level":   output.BitrateLevel,
				"protocol":        output.Protocol,
				"task_id":         output.TaskId,
				"online_at":       output.OnlineAt,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}
	// 档位列表缓存失效：代际号自增让该房间此前的列表键在 TTL 内自然作废（无需 SCAN）。
	bumpOutputGen(l.ctx, l.svcCtx, output.RoomId)

	created, err := l.svcCtx.StreamOutputs.FindOne(l.ctx, outputID)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, model.ErrStreamOutputNotFound
	}
	return outputInfo(created), nil
}
