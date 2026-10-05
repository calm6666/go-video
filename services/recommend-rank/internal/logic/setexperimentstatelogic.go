package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetExperimentStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetExperimentStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetExperimentStateLogic {
	return &SetExperimentStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 实验状态迁移（RUNNING/PAUSED/STOPPED）
//
// 迁移路径固定为 DRAFT→RUNNING⇄PAUSED→STOPPED，STOPPED 是终态（model.CanTransitionExpState 判定），
// 想再跑同一套变体就另开 exp_key——否则历史决策摘要里的 (exp_key,variant_key,bucket_no)
// 会在两个不相干的实验期之间被共用，实验结论无法复现。
//
// 置 RUNNING 前的三道闸门：
//  1. 绑定可用：model_version 指向的行存在且非 RETIRED、feature_config_version 处于启用态；
//  2. 时间窗有效：已过 end_at 的实验不能重新分流（改时间窗要在 DRAFT/PAUSED 下走 UpsertExperiment）；
//  3. 同层互斥：两个 DRAFT 变体登记时互相看不见（重叠校验只查 RUNNING/PAUSED），
//     因此在开始分流前必须再证明一次区间不与同层其它生效变体重叠。
//
// 写路径走 Experiments().UpdateState 的条件 UPDATE（SQL 自带 state=fromState 与状态机校验）：
// 未生效时重读真实状态回给调用方，绝不把「没改成」报成成功。
// RUNNING 快照缓存按分钟分片（repository/cache.go），因此本迁移无需清 key，下一分钟自然生效。
func (l *SetExperimentStateLogic) SetExperimentState(in *rpc.SetExperimentStateReq) (*rpc.SetExperimentStateReply, error) {
	if l.svcCtx == nil || l.svcCtx.Repository == nil {
		return nil, model.ErrRepositoryNotConfigured
	}
	if in == nil {
		return nil, model.ErrOperatorRequired
	}
	operator, err := requireOperator(in.GetOperator())
	if err != nil {
		return nil, err
	}
	reason, err := requireReason("reason", in.GetReason())
	if err != nil {
		return nil, err
	}
	idempotencyKey, err := requireIdempotencyKey(in.GetIdempotencyKey())
	if err != nil {
		return nil, err
	}
	expKey, err := requiredIdent("exp_key", in.GetExpKey())
	if err != nil {
		return nil, err
	}
	variantKey, err := requiredIdent("variant_key", in.GetVariantKey())
	if err != nil {
		return nil, err
	}
	target, err := expStateFromRPC(in.GetTargetState())
	if err != nil {
		return nil, err
	}

	repo := l.svcCtx.Repository
	row, err := repo.Experiments().FindOne(l.ctx, expKey, variantKey)
	if err != nil {
		return nil, err
	}
	// 目标态即当前态：重放，不推进状态、不加 revision。
	if row.State == target {
		l.Infof("recommend-rank: experiment state change deduplicated exp_key=%s variant_key=%s state=%s "+
			"operator=%s idempotency_key=%s", expKey, variantKey, expStateName(target), operator, idempotencyKey)
		return expStateReply(false, target, true), nil
	}
	if !model.CanTransitionExpState(row.State, target) {
		return nil, fmt.Errorf("%w: %s/%s %s -> %s 非法（只允许 DRAFT→RUNNING、RUNNING⇄PAUSED、DRAFT/RUNNING/PAUSED→STOPPED）",
			model.ErrExpStateTransition, expKey, variantKey, expStateName(row.State), expStateName(target))
	}
	if target == model.ExpStateRunning {
		if err := l.checkRunnable(row); err != nil {
			return nil, err
		}
	}
	ok, err := repo.Experiments().UpdateState(l.ctx, expKey, variantKey, row.State, target, operator, reason)
	if err != nil {
		return nil, err
	}
	if !ok {
		return l.reportLostRace(expKey, variantKey, row, target, operator, idempotencyKey)
	}
	l.Infof("recommend-rank: experiment state changed exp_key=%s variant_key=%s from=%s to=%s layer_key=%s "+
		"buckets=[%d,%d) operator=%s reason=%s idempotency_key=%s",
		expKey, variantKey, expStateName(row.State), expStateName(target), row.LayerKey,
		row.BucketStart, row.BucketEnd, operator, reason, idempotencyKey)
	return expStateReply(true, target, false), nil
}

// checkRunnable 是「开始吃流量」前的完整性闸门。
func (l *SetExperimentStateLogic) checkRunnable(row *model.RankExperiment) error {
	repo := l.svcCtx.Repository
	if err := checkVariantBindings(l.ctx, repo, row); err != nil {
		return err
	}
	now := model.NowUnix()
	if row.StartAt > now {
		return fmt.Errorf("%w: %s/%s 的 start_at=%d 还没到（现在 %d），到点再置 RUNNING",
			model.ErrInvalidTimeRange, row.ExpKey, row.VariantKey, row.StartAt, now)
	}
	if row.EndAt != 0 && row.EndAt <= now {
		return fmt.Errorf("%w: %s/%s 的 end_at=%d 已过（现在 %d），要续期请在 DRAFT/PAUSED 下改时间窗",
			model.ErrInvalidTimeRange, row.ExpKey, row.VariantKey, row.EndAt, now)
	}
	bucketCount := row.BucketCount
	if bucketCount <= 0 {
		bucketCount = repo.BucketCount()
	}
	if !model.ValidBucketRange(row.BucketStart, row.BucketEnd, bucketCount) {
		return fmt.Errorf("%w: %s/%s 的落库区间 [%d,%d) 与当前桶空间 %d 不匹配，需要人工核对",
			model.ErrInvalidBucketRange, row.ExpKey, row.VariantKey, row.BucketStart, row.BucketEnd, bucketCount)
	}
	// DRAFT 变体登记时互相看不见，这里补一次同层重叠证明，避免两个变体同时开始吃同一段桶。
	if conflict, found, err := repo.BucketOverlapConflict(l.ctx, row.LayerKey, row.BucketStart, row.BucketEnd,
		row.VariantKey); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: %s/%s 的 [%d,%d) 与同层已生效的 %s/%s 相交，不能同时开始分流",
			model.ErrBucketOverlap, row.ExpKey, row.VariantKey, row.BucketStart, row.BucketEnd,
			conflict.ExpKey, conflict.VariantKey)
	}
	return nil
}

// reportLostRace 条件 UPDATE 未命中：状态已被并发推进。
// 这里重读真实状态回给调用方（changed=false），绝不把「没改成」写成成功；
// 只有当真实状态恰好已是目标态时才算幂等命中。
func (l *SetExperimentStateLogic) reportLostRace(expKey, variantKey string, from *model.RankExperiment,
	target int32, operator, idempotencyKey string) (*rpc.SetExperimentStateReply, error) {
	fresh, err := l.svcCtx.Repository.Experiments().FindOne(l.ctx, expKey, variantKey)
	if err != nil {
		if errors.Is(err, model.ErrExperimentNotFound) {
			return nil, fmt.Errorf("%w: %s/%s 状态推进未生效且行已不存在，请重读实验配置",
				model.ErrExpStateTransition, expKey, variantKey)
		}
		return nil, err
	}
	l.Errorf("recommend-rank: experiment state change lost race exp_key=%s variant_key=%s expected_from=%s "+
		"actual=%s wanted_to=%s operator=%s idempotency_key=%s",
		expKey, variantKey, expStateName(from.State), expStateName(fresh.State), expStateName(target),
		operator, idempotencyKey)
	return expStateReply(false, fresh.State, fresh.State == target), nil
}

func expStateReply(changed bool, state int32, deduplicated bool) *rpc.SetExperimentStateReply {
	return &rpc.SetExperimentStateReply{
		Changed:      changed,
		State:        toRPCExpState(state),
		Deduplicated: deduplicated,
	}
}
