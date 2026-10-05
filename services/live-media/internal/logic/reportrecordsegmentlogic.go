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

type ReportRecordSegmentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportRecordSegmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportRecordSegmentLogic {
	return &ReportRecordSegmentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 逐片登记切片（(record_id,seq) 幂等，缺口必须显式登记 MISSING）
//
// 幂等与「不倒退」：
//   - 首登记靠 uniq_record_seq 的 INSERT IGNORE：返回 1 是新行，0 是序号已登记（不报重复错误）；
//   - 重放推进用 UpdateStateTx，其 fromStates 由状态机表反推（segmentFromStates），
//     因此 VERIFIED→UPLOADED 这类回退天然 0 行；0 行后回读 FindBySeq 区分
//     「已是目标态」（幂等成功）与「非法回退」（ErrInvalidTransition）；
//   - MySQL 在「行存在但值未变」时也返回 0 行，故 0 行不等于失败，必须回读。
//
// 缺口：seq 跳跃时把中间空洞补成 MISSING 行（时间轴按 segment_seconds 与任务起点反推）。
// 无法反推（缺时长/锚点）或跳跃超过 maxGapFillRows 时不补洞，只记 warn 并发缺口事件——
// 静默跳过会让回放拼出一条「看起来完整」实则断档的回放。
//
// 水位与计数：由 RefreshStatsTx 从切片表重算（含 last_seq=GREATEST(现值, MAX(seq))），
// 不在此单独走 UpdateState(patch{LastSeq})：那会让每片登记都给任务行 ++version，
// 反过来把 Worker 自己的进度上报撞成版本冲突。
//
// 父任务终态拒绝新切片登记（避免无人认领的孤儿对象）；判定在事务外，极端并发下
// 宁可收下引用交给回收任务清理，也不丢已存在的对象。
func (l *ReportRecordSegmentLogic) ReportRecordSegment(in *rpc.ReportRecordSegmentReq) (*rpc.RecordSegmentInfo, error) {
	recordID := in.GetRecordId()
	if err := checkPositive("record_id", recordID, model.ErrRecordTaskNotFound); err != nil {
		return nil, err
	}
	if in.GetSeq() <= 0 {
		return nil, fmt.Errorf("live-media: seq=%d: %w", in.GetSeq(), model.ErrInvalidSeq)
	}
	target := int32(in.GetState())
	if err := checkSegmentState(target); err != nil {
		return nil, err
	}
	if in.GetStartAt() <= 0 || in.GetEndAt() <= in.GetStartAt() {
		return nil, fmt.Errorf("live-media: seq=%d needs start_at<end_at (got %d,%d): %w",
			in.GetSeq(), in.GetStartAt(), in.GetEndAt(), model.ErrInvalidSegmentRange)
	}
	if in.GetDurationMs() <= 0 {
		return nil, fmt.Errorf("live-media: seq=%d duration_ms=%d must be positive: %w",
			in.GetSeq(), in.GetDurationMs(), model.ErrInvalidSegmentRange)
	}
	if in.GetSizeBytes() < 0 {
		return nil, fmt.Errorf("live-media: seq=%d size_bytes=%d must not be negative: %w",
			in.GetSeq(), in.GetSizeBytes(), model.ErrInvalidSegmentRange)
	}
	// 只有正常链路（UPLOADING/UPLOADED/VERIFIED）必须有引用；MISSING/CORRUPT 允许无引用，
	// 缺口行写了空引用会让回收误判为「可删对象」。
	keyRequired := target == model.SegmentStateUploading || target == model.SegmentStateUploaded ||
		target == model.SegmentStateVerified
	bucket, objectKey, err := checkObjectRef(in.GetBucket(), in.GetObjectKey(), keyRequired, maxObjectKeyRunes)
	if err != nil {
		return nil, err
	}
	checksum, err := checkChecksum(in.GetChecksum())
	if err != nil {
		return nil, err
	}
	workerID := sanitizeWorkerID(in.GetWorkerId())
	traceID := sanitizeTraceID(in.GetTraceId())

	task, err := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, model.ErrRecordTaskNotFound
	}
	if model.IsRecordTerminal(task.State) {
		l.Errorf("livemedia/ReportRecordSegment: reject seq=%d on terminal record_id=%d state=%d worker=%s",
			in.GetSeq(), recordID, task.State, workerID)
		return nil, model.ErrTerminalState
	}
	if !keyRequired && bucket == "" {
		// MISSING/CORRUPT 不带引用是合法形态：清空入参，避免把上一条切片的 key 串到缺口行。
		objectKey = ""
	}
	// 时长与时间轴的一致性只告警：Worker 才是切片事实源，服务端不臆改它的上报值。
	if tol := durationToleranceMs(task.SegmentSeconds); tol > 0 {
		delta := in.GetDurationMs() - (in.GetEndAt()-in.GetStartAt())*1000
		if delta < 0 {
			delta = -delta
		}
		if delta > tol {
			l.Errorf("livemedia/ReportRecordSegment: record_id=%d seq=%d duration_ms=%d diverges from timeline %ds by %dms",
				recordID, in.GetSeq(), in.GetDurationMs(), in.GetEndAt()-in.GetStartAt(), delta)
		}
	}

	// 补洞计划：只在 seq 真跳号时生成 MISSING 行。
	var plan []*model.LiveRecordSegment
	var unfillable int64
	if in.GetSeq() > task.LastSeq+1 {
		plan = planMissingRows(task, task.LastSeq+1, in.GetSeq(), workerID, traceID)
		if len(plan) == 0 {
			unfillable = in.GetSeq() - task.LastSeq - 1
			l.Errorf("livemedia/ReportRecordSegment: record_id=%d cannot materialise %d gap rows (seq %d..%d): "+
				"check segment_seconds/start_at or worker seq numbering",
				recordID, unfillable, task.LastSeq+1, in.GetSeq()-1)
		}
	}

	newMissing := int64(0)
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, sess sqlx.Session) error {
		if len(plan) > 0 {
			aff, insErr := l.svcCtx.Segments.InsertIgnoreMissingTx(ctx, sess, plan)
			if insErr != nil {
				return insErr
			}
			newMissing += aff
		}

		row := &model.LiveRecordSegment{
			RecordId:    recordID,
			RoomId:      task.RoomId,
			LiveSession: task.LiveSession,
			Seq:         in.GetSeq(),
			StartAt:     in.GetStartAt(),
			EndAt:       in.GetEndAt(),
			DurationMs:  in.GetDurationMs(),
			State:       target,
			Bucket:      bucket,
			ObjectKey:   objectKey,
			SizeBytes:   in.GetSizeBytes(),
			Checksum:    checksum,
			WorkerId:    workerID,
			TraceId:     traceID,
		}
		aff, insErr := l.svcCtx.Segments.InsertIgnoreTx(ctx, sess, row)
		if insErr != nil {
			return insErr
		}
		switch aff {
		case 1:
			if target == model.SegmentStateMissing {
				newMissing++
			}
		default:
			// 序号已登记：推进校验信息与状态，不新增行。
			patch := model.SegmentPatch{
				StartAt:    i64p(in.GetStartAt()),
				EndAt:      i64p(in.GetEndAt()),
				DurationMs: i64p(in.GetDurationMs()),
				SizeBytes:  i64p(in.GetSizeBytes()),
				WorkerID:   strp(workerID),
				TraceID:    strp(traceID),
			}
			if bucket != "" {
				patch.Bucket = strp(bucket)
			}
			if objectKey != "" {
				patch.ObjectKey = strp(objectKey)
			}
			if checksum != "" {
				patch.Checksum = strp(checksum)
			}
			updAff, updErr := l.svcCtx.Segments.UpdateStateTx(ctx, sess, recordID, in.GetSeq(), target, patch)
			if updErr != nil {
				return updErr
			}
			if updAff == 0 {
				// 0 行有两种可能：值未变（幂等成功）或状态机不允许（非法回退）。回读判定。
				existed, findErr := l.svcCtx.Segments.FindBySeq(ctx, recordID, in.GetSeq())
				if findErr != nil {
					return findErr
				}
				if existed == nil {
					return fmt.Errorf("live-media: record_id=%d seq=%d: %w", recordID, in.GetSeq(), model.ErrSegmentNotFound)
				}
				if existed.State != target {
					return fmt.Errorf("live-media: record_id=%d seq=%d segment state %d->%d: %w",
						recordID, in.GetSeq(), existed.State, target, model.ErrInvalidTransition)
				}
			}
		}

		if _, refErr := l.svcCtx.RecordTasks.RefreshStatsTx(ctx, sess, recordID); refErr != nil {
			return refErr
		}

		if newMissing > 0 || unfillable > 0 {
			gapFrom := task.LastSeq + 1
			return appendOutboxEvent(ctx, l.svcCtx, sess, model.EventTypeRecordGapDetected,
				model.AggregateRecordTask, recordID, task.RoomId, map[string]any{
					"record_id":        recordID,
					"room_id":          task.RoomId,
					"live_session_id":  task.LiveSession,
					"from_seq":         gapFrom,
					"to_seq":           in.GetSeq(),
					"missing_recorded": newMissing,
					"missing_untraced": unfillable,
					"worker_id":        workerID,
				}, traceID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 提交后回读：事务内用主连接读不到本事务的写入，返回值必须是提交后的行。
	seg, err := l.svcCtx.Segments.FindBySeq(l.ctx, recordID, in.GetSeq())
	if err != nil {
		return nil, err
	}
	if seg == nil {
		return nil, fmt.Errorf("live-media: record_id=%d seq=%d: %w", recordID, in.GetSeq(), model.ErrSegmentNotFound)
	}
	return segmentInfo(seg), nil
}
