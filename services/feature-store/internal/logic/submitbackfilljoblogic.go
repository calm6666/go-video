package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitBackfillJobLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitBackfillJobLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitBackfillJobLogic {
	return &SubmitBackfillJobLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// backfillSnapshot 是回填提交回执的可回放快照。
type backfillSnapshot struct {
	JobID      int64  `json:"j"`
	FeatureKey string `json:"k"`
	Version    int32  `json:"v"`
	Total      int64  `json:"t"`
}

// 提交回填任务（request_id 幂等）
//
// 回填是「给一个还没对外的版本补历史值」，所以三条前置条件必须在提交时就锁死：
//   - 目标版本必须是 DRAFT：给 ACTIVE 版本边补边读，等于让在线读到半成品；
//   - source 与 entity_scope 必须与定义一致：防止用兴趣口径去补热度特征、把值写到别的主体维度；
//   - auto_switch 必须带 from_version 基线：回填期间有人手工切过版本时，
//     自动切换必须失败而不是悄悄覆盖别人的决策。
//
// 作业本身只落台账：取数来源适配器（internal/featuresource）尚未创建，
// 由 worker + services/cron 认领推进（见 README「已知缺口」）。
func (l *SubmitBackfillJobLogic) SubmitBackfillJob(
	in *rpc.SubmitBackfillJobReq) (*rpc.SubmitBackfillJobReply, error) {
	key := strings.TrimSpace(in.GetFeatureKey())
	requestID := strings.TrimSpace(in.GetRequestId())
	operator := strings.TrimSpace(in.GetOperator())
	reason := strings.TrimSpace(in.GetReason())
	if err := checkFeatureKey(key); err != nil {
		return nil, err
	}
	if err := checkVersion(in.GetVersion()); err != nil {
		return nil, err
	}
	if err := checkRequestID(requestID); err != nil {
		return nil, err
	}
	if err := checkOperator(operator); err != nil {
		return nil, err
	}
	if err := checkReason(reason); err != nil {
		return nil, err
	}
	scope := int32(in.GetEntityScope())
	if !model.ValidEntityScope(scope) {
		return nil, model.ErrEntityScopeRequired
	}
	source := int32(in.GetSource())
	if !model.ValidSource(source) {
		return nil, model.ErrSourceRequired
	}

	spec := receiptSpec{
		requestID:   requestID,
		opType:      model.ReceiptOpBackfill,
		featureKey:  key,
		version:     in.GetVersion(),
		entityScope: scope,
		rowCount:    1,
		operator:    operator,
		reason:      reason,
	}
	res, owner, err := beginReceipt(l.ctx, l.svcCtx, l.Logger, spec)
	if err != nil {
		return nil, err
	}
	if !res.Execute {
		return l.replay(res.Receipt)
	}

	job, err := l.buildJob(in, key, scope, source, operator, reason, requestID)
	if err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	if err := model.ValidateBackfillJob(job); err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	if err := l.checkAutoSwitch(job); err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}

	jobID, err := l.svcCtx.Backfills.Insert(l.ctx, job)
	if err != nil {
		if errors.Is(err, model.ErrJobExists) {
			// 台账已有同一 request_id：说明首次提交已落库但在收尾前中断，
			// 回查现值按 reused 返回，绝不插入第二条作业。
			existing, findErr := l.svcCtx.Backfills.FindByRequestID(l.ctx, requestID)
			if findErr != nil {
				failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, findErr)
				return nil, findErr
			}
			if existing == nil {
				e := fmt.Errorf("%w: receipt-less job row for the same request_id", model.ErrJobExists)
				failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, e)
				return nil, e
			}
			return l.reply(spec, owner, existing, true)
		}
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	job.JobID = jobID
	if !l.svcCtx.Config.Backfill.WorkerEnabled {
		// 本实例不认领作业：作业留在 PENDING，由开启 worker 的实例或 services/cron 接管。
		l.Infow("backfill job queued", logx.Field("job_id", jobID),
			logx.Field("worker_enabled", false))
	}
	l.Infow("backfill job submitted", logx.Field("job_id", jobID), logx.Field("feature_key", key),
		logx.Field("version", job.Version), logx.Field("entities_total", job.EntitiesTotal),
		logx.Field("operator", operator))
	return l.reply(spec, owner, job, false)
}

// buildJob 组装作业台账行：定义自洽校验 + 主体列表规范化 + 窗口与进度分母。
func (l *SubmitBackfillJobLogic) buildJob(in *rpc.SubmitBackfillJobReq, key string,
	scope, source int32, operator, reason, requestID string) (*model.BackfillJob, error) {
	def, err := l.svcCtx.Definitions.FindOne(l.ctx, key, in.GetVersion())
	if err != nil {
		return nil, err
	}
	// 回填只面向尚未对外的版本。RETIRED 同理拒绝：给已下线版本补数没有任何读方能看见。
	if def.State != model.FeatureStateDraft {
		return nil, fmt.Errorf("%w: %s@v%d is not DRAFT", model.ErrBackfillTargetNotDraft,
			def.FeatureKey, def.Version)
	}
	if def.EntityScope != scope {
		return nil, fmt.Errorf("%w: request scope %d != definition scope %d",
			model.ErrEntityScopeMismatch, scope, def.EntityScope)
	}
	if def.Source != source {
		return nil, fmt.Errorf("%w: request source %d != definition source %d",
			model.ErrBackfillSourceMismatch, source, def.Source)
	}
	// FormatEntityIDs 会去重、升序、复验形态并检查条数与字节上限：
	// 超限直接报错，不静默截断——截断等于「补了一半的历史」却对外声称成功。
	entityIDs, err := model.FormatEntityIDs(scope, in.GetEntityIds())
	if err != nil {
		return nil, err
	}
	var total int64
	if entityIDs != "" {
		total = int64(strings.Count(entityIDs, ",") + 1)
	}
	// 全量扫描时不预估总数：本服务没有跨域的「主体全集」可读，
	// 猜一个分母只会让进度百分比看起来像真话（字段注释即「扫描前为 0」）。
	windowTo := in.GetWindowTo()
	if windowTo == 0 {
		windowTo = model.NowUnix()
	}
	return &model.BackfillJob{
		FeatureKey:  def.FeatureKey,
		Version:     def.Version,
		EntityScope: def.EntityScope,
		Source:      def.Source,
		State:       model.BackfillStatePending,
		WindowFrom:  in.GetWindowFrom(),
		WindowTo:    windowTo,
		EntityIDs:   entityIDs,
		// 提交路径要么收下完整列表、要么报错，因此不存在被截断的作业；
		// 这一列由 worker 在自身批次上限处诚实置 1（作业必须 FAILED）。
		EntitiesTruncated: 0,
		EntitiesTotal:     total,
		AutoSwitch:        in.GetAutoSwitch(),
		FromVersion:       in.GetFromVersion(),
		RequestID:         requestID,
		Operator:          clip(operator, maxOperatorLen),
		Reason:            clip(reason, maxReasonLen),
		TraceID:           traceIDOf(l.ctx),
	}, nil
}

// checkAutoSwitch 核对 from_version 基线：auto_switch 是一次「延迟到作业结束时执行的版本切换」，
// 因此它的基线必须在提交时就与指针现值一致，而不是等切换那一刻再猜。
// from_version=0 的语义是「从无到有」，所以它只允许在没有 ACTIVE 版本时通过。
func (l *SubmitBackfillJobLogic) checkAutoSwitch(job *model.BackfillJob) error {
	if !job.AutoSwitch {
		// 未开自动切换时 from_version 不参与任何判定，落 0 以免后来者误读成「已校验过」。
		job.FromVersion = 0
		return nil
	}
	pointer, err := l.svcCtx.ActiveVersions.FindOne(l.ctx, job.FeatureKey)
	if err != nil {
		return err
	}
	var active int32
	if pointer.HasActive() {
		active = pointer.ActiveVersion
	}
	if job.FromVersion != active {
		return fmt.Errorf("%w: auto_switch baseline is %d but %s currently serves %d",
			model.ErrVersionConflict, job.FromVersion, job.FeatureKey, active)
	}
	return nil
}

func (l *SubmitBackfillJobLogic) reply(spec receiptSpec, owner string,
	job *model.BackfillJob, reused bool) (*rpc.SubmitBackfillJobReply, error) {
	raw := marshalJSON(backfillSnapshot{JobID: job.JobID, FeatureKey: job.FeatureKey,
		Version: job.Version, Total: job.EntitiesTotal})
	if err := finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   raw,
		ResultDigest: resultDigest(raw),
		AffectedRows: 1,
		DetailKept:   true,
	}); err != nil {
		return nil, err
	}
	return &rpc.SubmitBackfillJobReply{JobId: job.JobID, Reused: reused, Job: backfillToProto(job)}, nil
}

// replay 按首次回执回答重复提交：回执只存 job_id，现值一律回查台账表。
// 回查而非用快照里的旧计数，是因为作业进度本来就在推进，回放必须给出「现在到哪了」。
func (l *SubmitBackfillJobLogic) replay(receipt *model.WriteReceipt) (*rpc.SubmitBackfillJobReply, error) {
	var snap backfillSnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: backfill receipt unreadable: %w", err)
	}
	if snap.JobID <= 0 {
		return nil, fmt.Errorf("%w: backfill receipt has no job id", model.ErrReceiptStateInvalid)
	}
	job, err := l.svcCtx.Backfills.FindOne(l.ctx, snap.JobID)
	if err != nil {
		return nil, err
	}
	return &rpc.SubmitBackfillJobReply{JobId: job.JobID, Reused: true, Job: backfillToProto(job)}, nil
}
