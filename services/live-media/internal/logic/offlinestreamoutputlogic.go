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

type OfflineStreamOutputLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOfflineStreamOutputLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OfflineStreamOutputLogic {
	return &OfflineStreamOutputLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 下线一个档位（断流/到期/人工），与回放发布状态无关
//
// 档位机只有两态（rpc 头部硬约束 2、model.StreamOutputState*）：在线→已下线。
// 「已下线」是**可再上线**的收敛态而非绝对终态 —— 同一自然键重新 UpsertStreamOutput 会把
// offline_at/offline_reason 复位（model.UpsertTx 显式写 offline_at=0），所以下线不是幂等
// 覆盖问题而是「一次下线结论只记一次」的问题：
//   - 对已下线的行重复下线：返回既有行（首次的 offline_at/offline_reason 就是审计事实），
//     不覆写、不再发事件，也不当错误（proto line 329「重复下线同一输出返回同一结果」）；
//   - 下线之后再上线、再下线：那是新的一次下线，写新的 offline_at/offline_reason。
//
// 影响观众侧，必须留审计（AGENTS.md §8）：本方法没有 operator 列（proto 未给），
// 归因由 request_id + offline_reason + trace_id 承担，因此 reason 必填且必须是具体原因
// （UNSPECIFIED 表示「没人认领这次下线」，直接拒绝；到期下线由 MarkExpiredOffline 自己写 TIMEOUT）。
//
// 定位二选一：output_id 优先（精确），否则 (room_id, bitrate_level, protocol) 找当前在线档位；
// 自然键路径不带到场的 live_session_id，因此同档位多场在线属数据异常，
// FindOnlineByLevel 以 ErrStreamOutputAmbiguous 拒绝而不是任选一行（猜错会摘掉还在分发的档位）。
//
// 事务边界：MarkOfflineTx + Outbox(livemedia.stream.output.offline) 同事务提交（AGENTS.md §5）。
// 只改在线态：不删对象、不碰 live_replay_*；残留产物由 SubmitRetentionTask
// （target_kind=STREAM_OUTPUT）显式登记后由 Worker 执行。
func (l *OfflineStreamOutputLogic) OfflineStreamOutput(in *rpc.OfflineStreamOutputReq) (*rpc.StreamOutputInfo, error) {
	outputID := in.GetOutputId()
	if outputID < 0 {
		return nil, fmt.Errorf("live-media: output_id=%d must be 0 (locate by natural key) or positive: %w",
			outputID, model.ErrStreamOutputNotFound)
	}
	reason := int32(in.GetReason())
	if err := checkFailureReason(reason); err != nil {
		return nil, err
	}
	if reason == model.ReasonUnspecified {
		return nil, fmt.Errorf("live-media: offline needs a concrete reason (SOURCE_LOST/TIMEOUT/MANUAL): %w",
			model.ErrInvalidTransition)
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	requestID := normalizedRequestID(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	cur, err := l.locateOutput(in, outputID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, model.ErrStreamOutputNotFound
	}
	if cur.State != model.StreamOutputStateOnline {
		if cur.State == model.StreamOutputStateOffline {
			// 幂等重放：回首次下线的结论，不覆写 offline_at/offline_reason、不再发事件。
			l.Infof("livemedia/OfflineStreamOutput: output_id=%d already offline since %d reason=%d request_id=%s",
				cur.OutputId, cur.OfflineAt, cur.OfflineReason, requestID)
			return outputInfo(cur), nil
		}
		return nil, fmt.Errorf("live-media: output_id=%d state=%d is not a known stream output state: %w",
			cur.OutputId, cur.State, model.ErrInvalidTransition)
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.StreamOutputs.MarkOfflineTx(ctx, sess, cur.OutputId, reason, traceID)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			// 0 行只可能是「并发请求已把同一档位下线」：state 本身就是 CAS 条件（本表无 version 列）。
			// 回读确认后不报错、也不补第二条事件——同一次下线只留一份证据。
			latest, findErr := l.svcCtx.StreamOutputs.FindOne(ctx, cur.OutputId)
			if findErr != nil {
				return findErr
			}
			if latest == nil {
				return model.ErrStreamOutputNotFound
			}
			if latest.State != model.StreamOutputStateOffline {
				return fmt.Errorf("live-media: output_id=%d offline lost race, state=%d: %w",
					cur.OutputId, latest.State, model.ErrInvalidTransition)
			}
			return nil
		}
		// payload 不带 offline_at：该列由 MarkOfflineTx 内部取时钟，行本身才是事实源，
		// 事件的时间维度用信封的 occurred_at（避免同一秒两处时间戳互相打脸）。
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeStreamOutputOffline,
			model.AggregateStreamOutput, cur.OutputId, cur.RoomId, map[string]any{
				"output_id":       cur.OutputId,
				"room_id":         cur.RoomId,
				"live_session_id": cur.LiveSession,
				"bitrate_level":   cur.BitrateLevel,
				"protocol":        cur.Protocol,
				"task_id":         cur.TaskId,
				"prev_state":      cur.State,
				"state":           model.StreamOutputStateOffline,
				"offline_reason":  reason,
				"request_id":      requestID,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}
	// 观众侧立刻看不见这个档位：代际号自增让该房间此前的列表键在 TTL 内作废。
	bumpOutputGen(l.ctx, l.svcCtx, cur.RoomId)

	latest, err := l.svcCtx.StreamOutputs.FindOne(l.ctx, cur.OutputId)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, model.ErrStreamOutputNotFound
	}
	return outputInfo(latest), nil
}

// locateOutput 按「output_id 优先，其次 (room, level, protocol) 的当前在线档位」定位下线目标。
// 两个定位入口都给时以 output_id 为准，但校验房间归属：跨房间摘档位一定是调用方拿错 ID。
// 只走自然键时不给 room 就是「全房间下线」的口子，必须拒绝（proto line 324 的「二选一」不含该语义）。
func (l *OfflineStreamOutputLogic) locateOutput(in *rpc.OfflineStreamOutputReq,
	outputID int64) (*model.LiveStreamOutput, error) {
	if outputID > 0 {
		row, err := l.svcCtx.StreamOutputs.FindOne(l.ctx, outputID)
		if err != nil {
			return nil, err
		}
		if row != nil && in.GetRoomId() > 0 && row.RoomId != in.GetRoomId() {
			return nil, fmt.Errorf("live-media: output_id=%d belongs to room %d, not %d: %w",
				outputID, row.RoomId, in.GetRoomId(), model.ErrInvalidRoomID)
		}
		return row, nil
	}
	level := int32(in.GetBitrateLevel())
	protocol := int32(in.GetProtocol())
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkBitrateLevel(level); err != nil {
		return nil, err
	}
	if err := checkProtocol(protocol); err != nil {
		return nil, err
	}
	return l.svcCtx.StreamOutputs.FindOnlineByLevel(l.ctx, in.GetRoomId(), level, protocol)
}
