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

type ReportLiveTranscodeProgressLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportLiveTranscodeProgressLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportLiveTranscodeProgressLogic {
	return &ReportLiveTranscodeProgressLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Worker 上报心跳/进度/终态（含超时判定），条件 UPDATE + 版本校验
//
// 三条不变量：
//  1. 终态不可复活：STOPPED/CANCELLED 的行只接受同态重放，迟到的心跳不能把它改回 RUNNING
//     （判定与写入之间没有读-改-写窗口，状态条件在 UPDATE 的 WHERE 里）。
//  2. 乱序/过期进度不倒退状态：目标态必须通过 IsValidTranscodeTransition 的合法边，
//     且 UPDATE 带 fromStates=[当前态]，并发抢先迁移后本次上报 0 行报错而不是写脏。
//  3. 心跳不能被版本卡死：同状态同进度的重投只顺延 heartbeat_at/timeout_at（放弃版本校验、
//     不发事件），否则 Worker 每次心跳重试都要先重新读版本，超时清扫会误杀正常任务。
//
// err_msg 一律先脱敏再截断（列宽 512）：Worker 的错误摘要常带签名拉流地址。
// worker_id 只进日志与事件 payload —— live_transcode_task 没有该列（排障靠 trace_id 串事件流）。
// 进入 STOPPED 后是否接着录制的联动事件由录制侧（ReportLiveRecordProgress）负责，本方法不越界。
func (l *ReportLiveTranscodeProgressLogic) ReportLiveTranscodeProgress(in *rpc.ReportLiveTranscodeProgressReq) (*rpc.LiveTranscodeTaskInfo, error) {
	taskID := in.GetTaskId()
	if err := checkPositive("task_id", taskID, model.ErrTranscodeTaskNotFound); err != nil {
		return nil, err
	}
	target := int32(in.GetState())
	if err := checkTranscodeState(target); err != nil {
		return nil, err
	}
	reason := int32(in.GetReason())
	if err := checkFailureReason(reason); err != nil {
		return nil, err
	}
	// PENDING 是登记态（只由 StartLiveTranscode / RetryLiveTranscode 写入）：
	// Worker 回报 PENDING 既无进度语义也不该被记成一次状态迁移。
	if target == model.TranscodeStatePending {
		return nil, fmt.Errorf("live-media: PENDING is not a reportable transcode state: %w", model.ErrInvalidTransition)
	}
	if target == model.TranscodeStateFailed && reason == model.ReasonUnspecified {
		// 失败必须可归因：超时上报 TIMEOUT、断流上报 SOURCE_LOST、存储失败上报 STORAGE。
		return nil, fmt.Errorf("live-media: FAILED report needs a concrete reason: %w", model.ErrInvalidTransition)
	}
	progress, clamped := clampProgress(in.GetProgress())
	if clamped {
		l.Errorf("livemedia/ReportLiveTranscodeProgress: task_id=%d progress=%d out of 0..100, clamped to %d",
			taskID, in.GetProgress(), progress)
	}
	workerID := sanitizeWorkerID(in.GetWorkerId())
	traceID := sanitizeTraceID(in.GetTraceId())

	cur, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, model.ErrTranscodeTaskNotFound
	}

	if model.IsTranscodeTerminal(cur.State) {
		if cur.State == target {
			// 同值重放：不写心跳、不写事件，直接回原行。
			return transcodeInfo(cur), nil
		}
		l.Errorf("livemedia/ReportLiveTranscodeProgress: reject report on terminal task_id=%d state=%d incoming=%d worker=%s",
			taskID, cur.State, target, workerID)
		return nil, model.ErrTerminalState
	}
	if !model.IsValidTranscodeTransition(cur.State, target) {
		return nil, fmt.Errorf("live-media: transcode task_id=%d state %d->%d: %w",
			taskID, cur.State, target, model.ErrInvalidTransition)
	}

	// timeout_at 顺延仍用登记时刻的「无心跳超时秒数」快照（见 timeoutBudget），
	// 不回读配置：配置改动不该让正在跑的转码任务集体误判超时。
	budget := timeoutBudget(cur.HeartbeatAt, cur.TimeoutAt)
	now := model.NowUnix()

	// --- 纯心跳：状态与进度都与当前行一致 ---
	if transcodeReportReplay(cur.State, target, cur.Progress, progress) {
		aff, updErr := l.svcCtx.TranscodeTasks.UpdateState(l.ctx, taskID, []int32{cur.State}, 0, cur.State,
			model.TranscodePatch{HeartbeatAt: i64p(now), TimeoutAt: i64p(nextTimeoutAt(now, budget))})
		if updErr != nil {
			return nil, updErr
		}
		if aff == 0 {
			latest, findErr := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
			if findErr != nil {
				return nil, findErr
			}
			if latest == nil {
				return nil, model.ErrTranscodeTaskNotFound
			}
			if latest.State != cur.State {
				// 并发改了状态：本次心跳作废，交给调用方按新状态重判。
				return nil, fmt.Errorf("live-media: transcode heartbeat lost race task_id=%d state=%d: %w",
					taskID, latest.State, model.ErrInvalidTransition)
			}
			// 同一秒内的重投：MySQL「匹配但未变化」也返回 0 行，值已等价，按幂等成功处理。
			return transcodeInfo(latest), nil
		}
		latest, findErr := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
		if findErr != nil {
			return nil, findErr
		}
		if latest == nil {
			// 心跳已写进库但回读为空：见 StopLiveTranscode 的同处注释。
			// 上面的 0 行分支已有这道门禁，成功分支漏掉就会让 Worker 拿到一个
			// task_id=0 的空响应，以为任务被删了而停掉正在跑的进程。
			return nil, model.ErrTranscodeTaskNotFound
		}
		return transcodeInfo(latest), nil
	}

	patch := model.TranscodePatch{
		Progress:    i32p(progress),
		HeartbeatAt: i64p(now),
		TimeoutAt:   i64p(nextTimeoutAt(now, budget)),
		Errno:       i32p(in.GetErrno()),
		ErrMsg:      strp(sanitizeErrMsg(in.GetErrMsg())),
		TraceID:     strp(traceID),
	}
	if reason != model.ReasonUnspecified {
		// 只覆盖「带上来的原因」：健康心跳把已记录的原因抹掉会让重试与审计失去判据。
		patch.Reason = i32p(reason)
	}
	if target == model.TranscodeStateRunning && cur.StartedAt == 0 {
		// started_at 只记首次：Worker 重启后的第二次 RUNNING 不能把真实启动点往后推。
		patch.StartedAt = i64p(now)
	}
	if target == model.TranscodeStateStopped || target == model.TranscodeStateFailed {
		patch.StoppedAt = i64p(now)
	}

	stateChanged := cur.State != target
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.TranscodeTasks.UpdateStateTx(ctx, sess, taskID,
			[]int32{cur.State}, in.GetExpectedVersion(), target, patch)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			return classifyTranscodeZeroRow(ctx, l.svcCtx.TranscodeTasks, taskID, in.GetExpectedVersion())
		}
		if !stateChanged {
			// 仅进度变化（RUNNING→RUNNING 且进度不同）不发事件：
			// 转码健康度是高频采样，逐次发事件会刷爆 Outbox。
			return nil
		}
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeTranscodeStateChanged,
			model.AggregateTranscodeTask, taskID, cur.RoomId, map[string]any{
				"task_id":         taskID,
				"room_id":         cur.RoomId,
				"live_session_id": cur.LiveSession,
				"bitrate_level":   cur.BitrateLevel,
				"protocol":        cur.Protocol,
				"prev_state":      cur.State,
				"state":           target,
				"progress":        progress,
				"attempt":         cur.Attempt,
				"reason":          reason,
				"errno":           in.GetErrno(),
				"worker_id":       workerID,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}

	latest, err := l.svcCtx.TranscodeTasks.FindOne(l.ctx, taskID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, model.ErrTranscodeTaskNotFound
	}
	return transcodeInfo(latest), nil
}
