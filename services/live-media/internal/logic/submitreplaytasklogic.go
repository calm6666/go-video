package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitReplayTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitReplayTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitReplayTaskLogic {
	return &SubmitReplayTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交回放拼接任务（只登记与校验切片区间，不拼接、不发布）
//
// 本方法是「素材可拼接性」的裁决点，不含任何发布语义（proto line 489-491、README §1 边界 1）：
// 不调 asset、不建稿件、不写 asset_id/aid，登记的行恒为 REPLAY_STATE_PENDING。
// 之后的链路一律由 Worker 驱动：ReportReplayProgress(MERGING→UPLOADING→REGISTERED) →
// 下游 asset/video/moderation（Worker 侧）→ BindReplayAsset → ApplyReplayContentState 投影结果。
//
// 只有「可回看的录制结论」才允许建回放：live_record_task 必须 STOPPED。
// FAILED 虽可断点续录（recordTransitions: FAILED→PENDING/RECORDING），但此刻尾部切片还可能在补录，
// 按它拼接会产出永远缺尾巴的回放；RECORDING/STOPPING 同理。
//
// 幂等三层（与 StartLiveRecord 同口径）：
//  1. uniq_request_id 命中既有行 → 原样返回（同一次提交的重放绝不产出第二个任务）；
//  2. 同 (record_id, from_seq, to_seq) 已有未终态任务 → 复用（同区间双开会产出两份稿件，
//     FindByRecordAndRange 只匹配非 COMPLETED/FAILED/CANCELLED，失败区间允许重投新任务）；
//  3. Insert 撞唯一索引（ErrRequestIdDuplicated）→ 回读 request_id 返回既有行，不当失败。
//
// 区间与缺口：from_seq<=0 → 1，to_seq<=0 → 该录制已登记的最大序号；越界即 ErrInvalidSegmentRange。
// 缺口判据来自 Segments.StatsInRange 的单表条件聚合（隐式空洞 + MISSING + CORRUPT），
// 允许量由 maxGapSegments 给出（allow_gaps=false 恒为 0；配置 MaxReplayGapSegments 默认 0）——
// 宁可拒绝提交，也不产出一条时间轴断裂却「看起来完整」的回放。
//
// 事件：model 的事件词表里没有「回放任务已登记」这一类（只有 replay_review_submitted /
// replay_content_state_changed，见 model/live_media_outbox.go），而本服务自有 topic 尚未登记
// （README §8.4）。按「不伪造 topic/event_type」的既定口径，此处不写 Outbox 行；
// 下游真正的触发点仍是录制侧的 livemedia.record.stopped 与后续的 review_submitted。
// 因此本方法只有一次 INSERT，无需事务。
func (l *SubmitReplayTaskLogic) SubmitReplayTask(in *rpc.SubmitReplayTaskReq) (*rpc.LiveReplayTaskInfo, error) {
	recordID := in.GetRecordId()
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkSessionID(in.GetLiveSessionId()); err != nil {
		return nil, err
	}
	if err := checkPositive("record_id", recordID, model.ErrRecordTaskNotFound); err != nil {
		return nil, err
	}
	// anchor_mid 是回放稿件的归属人（video.CreateSubmission 的投稿人）：没有它产物无法落到人名下。
	if err := checkPositive("anchor_mid", in.GetAnchorMid(), model.ErrInvalidAid); err != nil {
		return nil, err
	}
	cfg := l.svcCtx.Config.LiveMedia
	title, err := checkReplayTitle(cfg, in.GetTitle())
	if err != nil {
		return nil, err
	}
	description := clampAuditText("description", in.GetDescription(), maxDescRunes, l.Logger)
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	requestID := normalizedRequestID(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	// 时间轴入参先自洽：start_at/end_at 是「回放覆盖区间」，与序号区间是两套事实，打架必须拒绝。
	startAt, endAt := in.GetStartAt(), in.GetEndAt()
	if startAt < 0 || endAt < 0 {
		return nil, fmt.Errorf("live-media: start_at=%d end_at=%d must not be negative: %w",
			startAt, endAt, model.ErrInvalidSegmentRange)
	}
	if startAt > 0 && endAt > 0 && endAt <= startAt {
		return nil, fmt.Errorf("live-media: end_at=%d <= start_at=%d: %w", endAt, startAt, model.ErrInvalidSegmentRange)
	}

	task, err := l.svcCtx.RecordTasks.FindOne(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("live-media: record_id=%d: %w", recordID, model.ErrRecordTaskNotFound)
	}
	if task.RoomId != in.GetRoomId() || task.LiveSession != in.GetLiveSessionId() {
		// 归属不一致说明调用方拿错主键（或场次 ID 没透传，README §8.10）：
		// 让它拼出别人的回放比拒绝严重得多。
		return nil, fmt.Errorf("live-media: record_id=%d belongs to room %d session %d, not %d/%d: %w",
			recordID, task.RoomId, task.LiveSession, in.GetRoomId(), in.GetLiveSessionId(), model.ErrInvalidRoomID)
	}
	if task.State != model.RecordStateStopped {
		return nil, fmt.Errorf("live-media: record_id=%d state=%d is not STOPPED, replay needs a finished recording: %w",
			recordID, task.State, model.ErrInvalidTransition)
	}

	// 水位取切片表真值（record_task.last_seq 是派生列，极端情况下会落后）。
	lastSeq, err := l.svcCtx.Segments.LastSeq(l.ctx, recordID)
	if err != nil {
		return nil, err
	}
	fromSeq, toSeq, err := normalizeReplayRange(in.GetFromSeq(), in.GetToSeq(), lastSeq)
	if err != nil {
		return nil, err
	}
	if startAt == 0 {
		startAt = firstPositive(task.RecordStartAt, task.StartAt)
	}
	if endAt == 0 {
		endAt = firstPositive(task.RecordEndAt, task.EndAt)
	}
	if startAt > 0 && task.RecordStartAt > 0 && startAt < task.RecordStartAt {
		return nil, fmt.Errorf("live-media: start_at=%d precedes record_start_at=%d: %w",
			startAt, task.RecordStartAt, model.ErrInvalidSegmentRange)
	}
	if endAt > 0 && task.RecordEndAt > 0 && endAt > task.RecordEndAt {
		return nil, fmt.Errorf("live-media: end_at=%d exceeds record_end_at=%d: %w",
			endAt, task.RecordEndAt, model.ErrInvalidSegmentRange)
	}
	if startAt > 0 && endAt > 0 && endAt <= startAt {
		return nil, fmt.Errorf("live-media: end_at=%d <= start_at=%d after fallback: %w",
			endAt, startAt, model.ErrInvalidSegmentRange)
	}

	stats, err := l.svcCtx.Segments.StatsInRange(l.ctx, recordID, fromSeq, toSeq)
	if err != nil {
		return nil, err
	}
	gaps := stats.Gaps(toSeq - fromSeq + 1)
	if stats.Verified <= 0 {
		// 区间内一片可拼接的都没有：登记出来也是必然失败的任务，直接拒绝并说明原因。
		return nil, fmt.Errorf("live-media: record_id=%d seq [%d,%d] has no VERIFIED segment: %w",
			recordID, fromSeq, toSeq, model.ErrSegmentRangeNotRecorded)
	}
	if maxGaps := maxGapSegments(cfg, in.GetAllowGaps()); gaps > maxGaps {
		return nil, fmt.Errorf("live-media: record_id=%d seq [%d,%d] has %d gaps, allow_gaps=%s allows %d: %w",
			recordID, fromSeq, toSeq, gaps, boolText(in.GetAllowGaps()), maxGaps, model.ErrReplayGapNotAllowed)
	}

	// 1. 幂等回放。
	if existed, findErr := l.svcCtx.ReplayTasks.FindByRequestID(l.ctx, requestID); findErr != nil {
		return nil, findErr
	} else if existed != nil {
		return replayInfo(existed), nil
	}
	// 2. 同区间复用（避免一份回放产出两份稿件）。
	if same, findErr := l.svcCtx.ReplayTasks.FindByRecordAndRange(l.ctx, recordID, fromSeq, toSeq); findErr != nil {
		return nil, findErr
	} else if same != nil {
		l.Infof("livemedia/SubmitReplayTask: reuse replay_id=%d state=%d record_id=%d seq [%d,%d] request_id=%s",
			same.ReplayId, same.State, recordID, fromSeq, toSeq, requestID)
		return replayInfo(same), nil
	}

	row := &model.LiveReplayTask{
		RoomId:      in.GetRoomId(),
		LiveSession: in.GetLiveSessionId(),
		RecordId:    recordID,
		State:       model.ReplayStatePending,
		FromSeq:     fromSeq,
		ToSeq:       toSeq,
		// segment_count 先落「可用素材数」，Worker 上报时以实际参与拼接的值覆盖（只增不减）。
		SegmentCount: stats.Verified,
		GapCount:     gaps,
		StartAt:      startAt,
		EndAt:        endAt,
		// duration_ms 留空：拼接产物时长是产物事实，只有 Worker 知道；
		// 素材时长（stats.DurationMs）只用于日志，避免 ReportReplayProgress 把它当产物时长锁死。
		AllowGaps:   boolInt32(in.GetAllowGaps()),
		AnchorMid:   in.GetAnchorMid(),
		Title:       title,
		Description: description,
		Version:     1,
		RequestId:   requestID,
		TraceId:     traceID,
	}
	l.Infof("livemedia/SubmitReplayTask: record_id=%d seq [%d,%d] verified=%d gaps=%d materialMs=%d request_id=%s",
		recordID, fromSeq, toSeq, stats.Verified, gaps, stats.DurationMs, requestID)

	id, err := l.svcCtx.ReplayTasks.Insert(l.ctx, row)
	if err != nil {
		if errors.Is(err, model.ErrRequestIdDuplicated) {
			existed, findErr := l.svcCtx.ReplayTasks.FindByRequestID(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if existed != nil {
				return replayInfo(existed), nil
			}
			return nil, err
		}
		return nil, err
	}
	row.ReplayId = id

	// 提交后回读：返回值必须是已提交的行（model 会补 ctime/mtime/version 默认值）。
	created, err := l.svcCtx.ReplayTasks.FindOne(l.ctx, id)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, fmt.Errorf("live-media: replay_id=%d: %w", id, model.ErrReplayTaskNotFound)
	}
	return replayInfo(created), nil
}
