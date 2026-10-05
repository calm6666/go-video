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

type ReportRetentionResultLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportRetentionResultLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportRetentionResultLogic {
	return &ReportRetentionResultLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Worker 上报回收结果（扫描/删除/跳过计数）
//
// 计数是证据而不是增量（model/live_retention_task.go 的 RetentionPatch 注释锁定了「覆盖写」）：
// 每次上报都代表本次执行的完整结果，累加会让重投把 deleted 翻倍。
// 三条自洽判据（不满足一律拒收，绝不替它改数）：
//   - scanned/deleted/skipped 非负且 deleted+skipped<=scanned（retentionCountsConsistent）；
//   - purge=false 的行只登记标记，deleted 必须为 0：上报非 0 就是「没有删除授权却删了东西」，
//     拒绝并 Errorf 告警（AGENTS.md §8 审计）；
//   - 从 RUNNING 进终态时三个计数都不得回退：删除是不可逆动作，
//     更小的值只可能是另一批/串了 retention_id 的回执，收下就等于抹掉已发生的删除证据。
//
// 状态机（retentionTransitions，PENDING→RUNNING→终态）：
//   - PENDING 不是上报目标态（checkReportedRetentionState 直接拒绝）；
//   - 必须先上报 RUNNING 认领，再上报终态：PENDING→SUCCEEDED 没有边，判 ErrInvalidTransition；
//   - 表里没有自环，因此同状态携带不同计数=非法迁移（只有完全同值才是幂等重放）；
//   - 终态（SUCCEEDED/FAILED/CANCELLED）除同值重放外一律 ErrTerminalState，
//     迟到的上报不能把已完成的任务改回执行中。
//
// 幂等：retentionReportReplay(state+三个计数全等) → 返回当前行，不 ++version、不再发事件；
// 乐观并发：expected_version>0 时由条件 UPDATE 校验，0 行交 classifyRetentionZeroRow 给可读结论。
//
// 归因三列（errno/err_msg/fail_reason）与计数同属「本次回执即事实」的覆盖写，
// 所以成功边不需要额外清痕：SUCCEEDED 回执不带归因，落库就是把上一次 RUNNING 的痕迹擦成 0/""。
// fail_reason 多一条保留规则——非成功边且本次没给原因时不动这一列（见 patch 处注释）。
//
// 副作用（与状态推进同事务，且都幂等）：
//   - REPLAY + SUCCEEDED + purge=true：引用行 Pending→Reclaimed（引用保留、产物已删，proto line 660 语义）；
//   - REPLAY + FAILED/CANCELLED 且 deleted=0：引用行回退 Pending→Normal，
//     标记的语义是「当前有回收意图待执行」，失败且什么都没删就不该把对象永久挂在待回收上；
//     Reclaimed 的行不动（fromStates 条件 UPDATE 天然只命中 Pending）；
//   - REPLAY + SUCCEEDED + purge=false：不推进 Reclaimed，引用留在 Pending——
//     对象存储根本没删（README §8.9：Storage 删除接口本轮未接线），标成已回收就是假证据；
//   - SEGMENT：本地切片行**不删**。model 只有按录制任务粒度的 PurgeByRecordTx(record_id, limit)，
//     没有按单个 segment id 的删除入口，用它会把整场录制的切片全删（越权删除），
//     因此只记告警并留待补 model 能力（见 README 已知缺口）；
//   - STREAM_OUTPUT：无删除入口，且已下线行本身就是审计证据，保留；
//     但结论与现状矛盾时拒绝——行仍在线却被上报删了产物，说明 Worker 串了对象。
//
// 事件：进入终态写 Outbox(livemedia.retention.finished)，payload 带三个计数作为可追溯证据，
// 与状态推进同事务；RUNNING 不发事件（model 里没有登记执行中事件类型，也不伪造）。
func (l *ReportRetentionResultLogic) ReportRetentionResult(in *rpc.ReportRetentionResultReq) (*rpc.LiveRetentionTaskInfo, error) {
	retentionID := in.GetRetentionId()
	if err := checkPositive("retention_id", retentionID, model.ErrRetentionTaskNotFound); err != nil {
		return nil, err
	}
	target := int32(in.GetState())
	if err := checkReportedRetentionState(target); err != nil {
		return nil, err
	}
	failReason := int32(in.GetFailReason())
	if err := checkFailureReason(failReason); err != nil {
		return nil, err
	}
	if in.GetExpectedVersion() < 0 {
		return nil, fmt.Errorf("live-media: expected_version=%d must not be negative: %w",
			in.GetExpectedVersion(), model.ErrVersionConflict)
	}
	if err := retentionCountsConsistent(in.GetScanned(), in.GetDeleted(), in.GetSkipped()); err != nil {
		return nil, err
	}
	if target == model.RetentionStateFailed && failReason == model.ReasonUnspecified {
		return nil, fmt.Errorf("live-media: FAILED report needs a concrete fail_reason: %w",
			model.ErrInvalidTransition)
	}
	workerID := sanitizeWorkerID(in.GetWorkerId())
	traceID := sanitizeTraceID(in.GetTraceId())

	cur, err := l.svcCtx.RetentionTasks.FindOne(l.ctx, retentionID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("live-media: retention_id=%d: %w", retentionID, model.ErrRetentionTaskNotFound)
	}
	if cur.Purge == 0 && in.GetDeleted() > 0 {
		// 只登记了标记却上报删除：授权范围被越过了，收下就等于给未授权的删除补一份合法记录。
		l.Errorf("livemedia/ReportRetentionResult: retention_id=%d purge=false but deleted=%d reported by worker=%s",
			retentionID, in.GetDeleted(), workerID)
		return nil, fmt.Errorf("live-media: retention_id=%d was registered with purge=false, deleted=%d is unauthorized: %w",
			retentionID, in.GetDeleted(), model.ErrInvalidTransition)
	}

	if model.IsRetentionTerminal(cur.State) {
		if cur.State == target &&
			retentionReportReplay(cur.State, target, cur.Scanned, in.GetScanned(),
				cur.Deleted, in.GetDeleted(), cur.Skipped, in.GetSkipped()) {
			// 终态同值重放：回执丢了就重投一次，读到既有结论即可。
			return retentionInfo(cur), nil
		}
		l.Errorf("livemedia/ReportRetentionResult: reject report on terminal retention_id=%d state=%d incoming=%d worker=%s",
			retentionID, cur.State, target, workerID)
		return nil, fmt.Errorf("live-media: retention_id=%d state=%d is terminal, incoming %d: %w",
			retentionID, cur.State, target, model.ErrTerminalState)
	}
	if cur.State == target {
		if retentionReportReplay(cur.State, target, cur.Scanned, in.GetScanned(),
			cur.Deleted, in.GetDeleted(), cur.Skipped, in.GetSkipped()) {
			if in.GetExpectedVersion() > cur.Version {
				return nil, fmt.Errorf("live-media: retention_id=%d expected_version=%d ahead of version=%d: %w",
					retentionID, in.GetExpectedVersion(), cur.Version, model.ErrVersionConflict)
			}
			l.Infof("livemedia/ReportRetentionResult: retention_id=%d state=%d identical report (worker=%s)",
				retentionID, target, workerID)
			return retentionInfo(cur), nil
		}
		return nil, fmt.Errorf("live-media: retention_id=%d has no self transition for state=%d, counts are overwritten on the terminal report: %w",
			retentionID, target, model.ErrInvalidTransition)
	}
	if !model.IsValidRetentionTransition(cur.State, target) {
		return nil, fmt.Errorf("live-media: retention retention_id=%d state %d->%d: %w",
			retentionID, cur.State, target, model.ErrInvalidTransition)
	}
	if cur.State == model.RetentionStateRunning {
		// 已认领的执行中行带着上一次上报的证据，任何一项回退都说明这次回执不是同一批。
		for _, c := range []struct {
			name   string
			before int32
			now    int32
		}{
			{"scanned", cur.Scanned, in.GetScanned()},
			{"deleted", cur.Deleted, in.GetDeleted()},
			{"skipped", cur.Skipped, in.GetSkipped()},
		} {
			if c.now < c.before {
				return nil, fmt.Errorf("live-media: retention_id=%d reported %s=%d shrinks recorded %d: %w",
					retentionID, c.name, c.now, c.before, model.ErrInvalidTransition)
			}
		}
	}
	if cur.TargetKind == model.RetentionTargetStreamOutput && cur.TargetId > 0 && in.GetDeleted() > 0 {
		// 结论与现状矛盾：档位行还在在线态，产物不可能已被安全删除（回收只允许从已下线档位开始）。
		out, outErr := l.svcCtx.StreamOutputs.FindOne(l.ctx, cur.TargetId)
		if outErr != nil {
			return nil, outErr
		}
		if out != nil && out.State != model.StreamOutputStateOffline {
			return nil, fmt.Errorf("live-media: retention_id=%d claims deleted=%d but output_id=%d is still online (state=%d): %w",
				retentionID, in.GetDeleted(), cur.TargetId, out.State, model.ErrInvalidTransition)
		}
	}

	patch := model.RetentionPatch{
		State:   target,
		Scanned: i32p(in.GetScanned()),
		Deleted: i32p(in.GetDeleted()),
		Skipped: i32p(in.GetSkipped()),
		Errno:   i32p(in.GetErrno()),
		ErrMsg:  strp(sanitizeErrMsg(in.GetErrMsg())),
		TraceID: strp(traceID),
	}
	// fail_reason 只在本次真的给出归因时写：PENDING/RUNNING 之间的心跳式回执不该把上一次失败原因
	// 擦成 0（那是这条链路上唯一「迟到了但仍然成立」的事实）。
	// 例外是成功边：见函数头「成功边不保留失败痕迹」——SUCCEEDED 回执不带归因，
	// 三列就按回执的 0/"" 覆盖写（errno/err_msg 在上面已经无条件写，这里补上 fail_reason）。
	// 缺陷 #7（本轮修）：这里曾把 Errno/ErrMsg 置成 nil，而 Patch 的 nil 语义是「本次不更新这一列」，
	// 于是上一次 RUNNING 的失败痕迹被留在了一条 SUCCEEDED 行上，与注释承诺的行为正好相反。
	if failReason != model.ReasonUnspecified || target == model.RetentionStateSucceeded {
		patch.FailReason = i32p(failReason)
	}

	// 生命周期标记的联动判定（都是条件 UPDATE，重投幂等）。
	markReclaimed := cur.TargetKind == model.RetentionTargetReplay && cur.TargetId > 0 &&
		target == model.RetentionStateSucceeded && cur.Purge == 1 && in.GetDeleted() > 0
	markReleased := cur.TargetKind == model.RetentionTargetReplay && cur.TargetId > 0 &&
		(target == model.RetentionStateFailed || target == model.RetentionStateCancelled) &&
		in.GetDeleted() == 0
	if cur.TargetKind == model.RetentionTargetSegment && cur.Purge == 1 && in.GetDeleted() > 0 {
		// 见函数头：model 只有按录制任务粒度的删除入口，单切片回收无法安全落本地行，留证不代删。
		l.Errorf("livemedia/ReportRetentionResult: retention_id=%d segment purge not executable locally (model has no per-segment delete), segment rows kept: deleted=%d",
			retentionID, in.GetDeleted())
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		aff, updErr := l.svcCtx.RetentionTasks.UpdateStateTx(ctx, sess, retentionID,
			[]int32{cur.State}, in.GetExpectedVersion(), patch)
		if updErr != nil {
			return updErr
		}
		if aff == 0 {
			return classifyRetentionZeroRow(ctx, l.svcCtx.RetentionTasks, retentionID, in.GetExpectedVersion())
		}
		if markReclaimed {
			mAff, mErr := l.svcCtx.ReplayRefs.MarkRetentionStateTx(ctx, sess, cur.TargetId,
				model.RefRetentionStatePending, model.RefRetentionStateReclaimed)
			if mErr != nil {
				return mErr
			}
			if mAff == 0 {
				l.Infof("livemedia/ReportRetentionResult: ref id=%d not in PENDING, marker kept, retention_id=%d",
					cur.TargetId, retentionID)
			}
		}
		if markReleased {
			mAff, mErr := l.svcCtx.ReplayRefs.MarkRetentionStateTx(ctx, sess, cur.TargetId,
				model.RefRetentionStatePending, model.RefRetentionStateNormal)
			if mErr != nil {
				return mErr
			}
			if mAff == 0 {
				l.Infof("livemedia/ReportRetentionResult: ref id=%d marker already settled, retention_id=%d",
					cur.TargetId, retentionID)
			}
		}
		if !model.IsRetentionTerminal(target) {
			return nil
		}
		return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeRetentionFinished,
			model.AggregateRetentionTask, retentionID, cur.RoomId, map[string]any{
				"retention_id":  retentionID,
				"target_kind":   cur.TargetKind,
				"target_id":     cur.TargetId,
				"room_id":       cur.RoomId,
				"expire_before": cur.ExpireBefore,
				"purge":         cur.Purge,
				"prev_state":    cur.State,
				"state":         target,
				"scanned":       in.GetScanned(),
				"deleted":       in.GetDeleted(),
				"skipped":       in.GetSkipped(),
				"fail_reason":   failReason,
				"errno":         in.GetErrno(),
				"worker_id":     workerID,
			}, traceID)
	})
	if err != nil {
		return nil, err
	}

	latest, err := l.svcCtx.RetentionTasks.FindOne(l.ctx, retentionID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, fmt.Errorf("live-media: retention_id=%d: %w", retentionID, model.ErrRetentionTaskNotFound)
	}
	return retentionInfo(latest), nil
}
