package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SubmitRetentionTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitRetentionTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitRetentionTaskLogic {
	return &SubmitRetentionTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交回收任务（超期切片/回放产物/残留档位），先登记后执行，保留审计证据
//
// 三步流程（proto line 641-652、AGENTS.md §8）：登记意图 → Worker 执行 → 回报计数。
// 本方法**一个对象都不删**：purge=true 也只是把「允许真删」的授权写进行里，
// 实际删除由 Worker 调 repository 的 Storage 接口完成（README §8.9 本轮仍是显式 stub）。
// 之所以禁止边查边删：一旦删错就必须能回答「谁在什么时候、为什么、动了哪些对象」。
//
// 校验与归一：
//   - target_kind∈{1 切片,2 回放产物,3 残留档位}（ErrInvalidTransition）；
//   - target_id 与 expire_before 至少一个（ErrRetentionTargetRequired：两者都缺等于「回收全世界」）；
//     负数一律拒绝，不接受「-1 表示全部」这种隐式约定；
//   - room_id<=0 是全局批量扫描，此时必须给 expire_before 收敛扫描面；
//     给了 target_id 时房间由对象自身决定（见下），不再要求调用方重复声明；
//   - reason（审计必填）与 operator（归因）都不可缺，长度按列宽拒绝而非截断；
//   - batch_limit<=0 取 LiveMedia.DefaultRetentionBatchLimit，超过 MaxRetentionBatchLimit 夹取（不报错）：
//     它是 Worker 每批删除的限流值，写进行里后不随配置变化改写历史任务。
//
// 存在性与准入（**只对定点回收 target_id>0 生效**；被回收对象必须真实存在，且当前状态允许回收）：
//   - SEGMENT：Segments.FindOne → 不存在 ErrSegmentNotFound；登记空转任务只会让 Worker 白跑；
//   - REPLAY：ReplayRefs.FindOne → ErrReplayRefNotFound；已 Reclaimed 的引用不再排第二次队；
//     未被标记（RetentionState!=Pending）时还要求父任务已进终态，
//     否则「拼接还在进行中的产物」会被删掉，而投影通道随后可能把它标成 COMPLETED；
//   - STREAM_OUTPUT：StreamOutputs.FindOne → ErrStreamOutputNotFound；
//     仍在线的输出直接拒绝——回收在线档位等于掐断正在分发的直播流，必须先 OfflineStreamOutput；
//   - 调用方声明的 room_id 与对象实际归属不一致时拒绝（ErrInvalidRoomID），
//     这是「拿 A 房间的 target_id 去登记回收 B 房间对象」这类越权请求的唯一拦截点。
//   - 批量任务（target_id=0 + expire_before>0）没有可回读的单一对象，因此不走上面的准入：
//     它扫到哪些行、哪一行还不许删，由 Worker 执行时逐行判定并计入 skipped。
//     这类任务的 room_id 就是调用方声明的扫描面（0=全局），不会被对象归属改写。
//
// 幂等与去重（README §2.5 line 89：幂等键 uniq_request_id）：
//  1. FindByRequestID 命中 → 返回既有任务（回复就是既成事实的那一行，不重复排队删除意图）；
//  2. target_id>0 时 FindUnfinishedByTarget 命中 PENDING/RUNNING → 复用该任务
//     （同一对象排两个删除任务必然双删/误删）；批量任务（target_id=0）是集合语义、可重叠，不参与判定；
//  3. InsertTx 撞 uniq_request_id（ErrRequestIdDuplicated）→ 回读既有任务，与 1 同一结论。
//
// 标记联动与事务：target_kind=REPLAY 且定位到引用行时，任务行与
// ReplayRefs.MarkRetentionStateTx(Normal→Pending) 同事务提交（AGENTS.md §5），
// 0 行表示已被并发标记/已回收，按幂等处理；不会出现「任务已登记但引用无标记」。
//
// 事件：本方法不写 Outbox——model/live_media_outbox.go 只登记了
// livemedia.retention.finished（回收结束），没有回收登记类事件类型，
// 且本服务自己的 topic 尚未注册（README §8.4）；这里**不伪造事件类型或 topic**，
// 待执行的回收任务由 Worker 用 ListRetentionTasks/ListByState 拉取。
func (l *SubmitRetentionTaskLogic) SubmitRetentionTask(in *rpc.SubmitRetentionTaskReq) (*rpc.LiveRetentionTaskInfo, error) {
	kind := int32(in.GetTargetKind())
	if err := checkRetentionTarget(kind); err != nil {
		return nil, err
	}
	targetID := in.GetTargetId()
	expireBefore := in.GetExpireBefore()
	roomID := in.GetRoomId()
	if targetID < 0 || expireBefore < 0 || roomID < 0 {
		return nil, fmt.Errorf("live-media: retention target_id=%d expire_before=%d room_id=%d must not be negative: %w",
			targetID, expireBefore, roomID, model.ErrRetentionTargetRequired)
	}
	if targetID == 0 && expireBefore == 0 {
		// 两者都缺就是「回收全世界」：没有对象主键，也没有收敛扫描面的到期时刻。
		return nil, fmt.Errorf("live-media: target_kind=%d needs target_id or expire_before: %w",
			kind, model.ErrRetentionTargetRequired)
	}
	reason, err := checkAuditText("retention reason", in.GetReason(), true, maxReasonRunes)
	if err != nil {
		return nil, err
	}
	operator, err := sanitizeOperator(in.GetOperator())
	if err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	requestID := normalizedRequestID(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())
	batchLimit := retentionBatchLimit(l.svcCtx.Config.LiveMedia, in.GetBatchLimit())
	purge := boolInt32(in.GetPurge())

	// --- 幂等：同一 request_id 的重投不再排第二次删除意图 ---
	if existed, findErr := l.svcCtx.RetentionTasks.FindByRequestID(l.ctx, requestID); findErr != nil {
		return nil, findErr
	} else if existed != nil {
		l.Infof("livemedia/SubmitRetentionTask: request_id=%s reused retention_id=%d state=%d",
			requestID, existed.RetentionId, existed.State)
		return retentionInfo(existed), nil
	}

	// --- 去重：同一对象只允许有一个未完成的回收任务 ---
	if targetID > 0 {
		if active, findErr := l.svcCtx.RetentionTasks.FindUnfinishedByTarget(l.ctx, int64(kind), targetID); findErr != nil {
			return nil, findErr
		} else if active != nil {
			l.Infof("livemedia/SubmitRetentionTask: target_kind=%d target_id=%d already has retention_id=%d state=%d, reused",
				kind, targetID, active.RetentionId, active.State)
			return retentionInfo(active), nil
		}
	}

	// --- 存在性与准入：主连接读，必须在事务前完成 ---
	// 只有定点回收有「一个真实的被回收对象」可回读。批量任务（target_id=0，proto line 644
	// 「0 表示按 expire_before 批量」）没有单一对象，准入由 Worker 在扫描时逐行判定并计入
	// skipped（proto line 662），所以不走 locateTarget —— 否则 FindOne(0) 必然查无此行，
	// 超期清理这类批量登记会全部撞 not-found 而永远登记不出来（缺陷 #6，本轮修）。
	actualRoom := roomID
	var refID int64
	if targetID > 0 {
		room, id, locateErr := l.locateTarget(kind, targetID, roomID)
		if locateErr != nil {
			return nil, locateErr
		}
		actualRoom, refID = room, id
	}

	task := &model.LiveRetentionTask{
		TargetKind:   kind,
		RoomId:       actualRoom,
		TargetId:     targetID,
		ExpireBefore: expireBefore,
		Purge:        purge,
		BatchLimit:   batchLimit,
		State:        model.RetentionStatePending,
		Reason:       reason,
		Operator:     operator,
		Version:      1,
		RequestId:    requestID,
		TraceId:      traceID,
	}

	var retentionID int64
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		id, insErr := l.svcCtx.RetentionTasks.InsertTx(ctx, sess, task)
		if insErr != nil {
			return insErr
		}
		retentionID = id
		if refID <= 0 {
			return nil
		}
		// 引用行进入「待回收」：生命周期由回收流程独占推进，这里只做 Normal→Pending。
		aff, markErr := l.svcCtx.ReplayRefs.MarkRetentionStateTx(ctx, sess, refID,
			model.RefRetentionStateNormal, model.RefRetentionStatePending)
		if markErr != nil {
			return markErr
		}
		if aff == 0 {
			// 值未变化（已被并发的 ApplyReplayContentState/另一次登记标记过）也返回 0 行：幂等。
			l.Infof("livemedia/SubmitRetentionTask: ref id=%d retention marker already set, retention_id=%d", refID, id)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, model.ErrRequestIdDuplicated) {
			// 唯一键是幂等的最终防线：并发重投在这里撞车，回读给出与预读同一结论。
			existed, findErr := l.svcCtx.RetentionTasks.FindByRequestID(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if existed != nil {
				return retentionInfo(existed), nil
			}
			return nil, err
		}
		return nil, err
	}

	created, err := l.svcCtx.RetentionTasks.FindOne(l.ctx, retentionID)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, fmt.Errorf("live-media: retention_id=%d: %w", retentionID, model.ErrRetentionTaskNotFound)
	}
	return retentionInfo(created), nil
}

// locateTarget 回读被回收对象：确认存在、校验房间归属，并给出回收准入结论。
// 返回 (对象实际归属房间, 需要联动标记的引用行主键, error)。
// 未通过准入一律失败关闭（不登记空转任务），错误里带上该怎么做的下一步。
func (l *SubmitRetentionTaskLogic) locateTarget(kind int32, targetID, roomID int64) (int64, int64, error) {
	switch kind {
	case model.RetentionTargetSegment:
		seg, err := l.svcCtx.Segments.FindOne(l.ctx, targetID)
		if err != nil {
			return 0, 0, err
		}
		if seg == nil {
			return 0, 0, fmt.Errorf("live-media: segment id=%d: %w", targetID, model.ErrSegmentNotFound)
		}
		if err := sameRoom("segment", roomID, seg.RoomId, targetID); err != nil {
			return 0, 0, err
		}
		// 切片是否仍被某个未完成的回放区间引用，model 没有提供按 seq 反查的能力（见报告「缺口」）：
		// 这一层由 Worker 在执行时判定并计入 skipped（proto line 662「跳过行数（仍被引用/状态不允许）」）。
		return seg.RoomId, 0, nil

	case model.RetentionTargetReplay:
		ref, err := l.svcCtx.ReplayRefs.FindOne(l.ctx, targetID)
		if err != nil {
			return 0, 0, err
		}
		if ref == nil {
			return 0, 0, fmt.Errorf("live-media: replay asset ref id=%d: %w", targetID, model.ErrReplayRefNotFound)
		}
		if err := sameRoom("replay ref", roomID, ref.RoomId, targetID); err != nil {
			return 0, 0, err
		}
		if ref.RetentionState == model.RefRetentionStateReclaimed {
			return 0, 0, fmt.Errorf("live-media: replay ref id=%d product already reclaimed: %w",
				targetID, model.ErrInvalidTransition)
		}
		if ref.RetentionState != model.RefRetentionStatePending {
			// 没有投影侧（DELETED）标记，就得由任务终态来证明这份产物已经不再产出。
			task, taskErr := l.svcCtx.ReplayTasks.FindOne(l.ctx, ref.ReplayId)
			if taskErr != nil {
				return 0, 0, taskErr
			}
			if task == nil {
				return 0, 0, fmt.Errorf("live-media: replay_id=%d: %w", ref.ReplayId, model.ErrReplayTaskNotFound)
			}
			if !model.IsReplayTerminal(task.State) {
				return 0, 0, fmt.Errorf("live-media: replay ref id=%d product of replay_id=%d is still in flight (state=%d): %w",
					targetID, ref.ReplayId, task.State, model.ErrInvalidTransition)
			}
		}
		return ref.RoomId, ref.Id, nil

	case model.RetentionTargetStreamOutput:
		out, err := l.svcCtx.StreamOutputs.FindOne(l.ctx, targetID)
		if err != nil {
			return 0, 0, err
		}
		if out == nil {
			return 0, 0, fmt.Errorf("live-media: output_id=%d: %w", targetID, model.ErrStreamOutputNotFound)
		}
		if err := sameRoom("stream output", roomID, out.RoomId, targetID); err != nil {
			return 0, 0, err
		}
		if out.State != model.StreamOutputStateOffline {
			// 「残留档位」的定义就是已下线的输出；在线档位被删等于掐断正在分发的流。
			return 0, 0, fmt.Errorf("live-media: output_id=%d is still online (state=%d), offline it via OfflineStreamOutput first: %w",
				targetID, out.State, model.ErrInvalidTransition)
		}
		return out.RoomId, 0, nil
	}
	return 0, 0, fmt.Errorf("live-media: target_kind=%d is not supported: %w", kind, model.ErrInvalidTransition)
}

// sameRoom 校验调用方声明的房间与对象实际归属一致；未声明（<=0）时以对象归属为准。
func sameRoom(what string, claimed, actual, targetID int64) error {
	if claimed > 0 && claimed != actual {
		return fmt.Errorf("live-media: %s id=%d belongs to room_id=%d but request declares room_id=%d: %w",
			what, targetID, actual, claimed, model.ErrInvalidRoomID)
	}
	return nil
}
