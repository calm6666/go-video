package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/recommend-rank/internal/repository"
	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertExperimentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertExperimentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertExperimentLogic {
	return &UpsertExperimentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新建/修改实验变体（同 (exp_key,variant_key) 唯一，变更 revision+1）
//
// 新登记的变体一律是 DRAFT：本服务不接受「登记即分流」，
// 分流必须再走一次 SetExperimentState(RUNNING)，这样「谁开始吃流量」才是独立审计事实。
//
// 关键约束：
//   - 落库桶空间固定为 svc.BucketCount，入参区间必须落在它内（调用方不能自带另一套桶数）；
//   - 写入前用 Repository().BucketOverlapConflict 证明同层不重叠（同层互斥是实验结论可信的前提），
//     冲突返回 ErrBucketOverlap 并点名冲突变体；
//   - overrides 走 model.SupportedOverrides() 白名单 + 值域校验，任何商业化字段直接拒绝（AGENTS.md §7）；
//   - 已存在变体的分桶语义只在 DRAFT/PAUSED 可改（UpdateBucketConfig 的 SQL 自带状态保护与 revision CAS），
//     未生效就改盐/改区间等于让线上跳组，因此 RUNNING/STOPPED 返回 ErrExperimentImmutable。
//
// RUNNING 快照缓存按分钟分片（见 repository/cache.go），本写入不需要清 key，下一分钟自然生效。
func (l *UpsertExperimentLogic) UpsertExperiment(in *rpc.UpsertExperimentReq) (*rpc.UpsertExperimentReply, error) {
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
	// layer_key 可留空（model 侧归到 default 层）：留空也要有作用域，否则「同层互斥」校验形同虚设。
	layerKey, err := optionalIdent("layer_key", in.GetLayerKey(), colIdent)
	if err != nil {
		return nil, err
	}
	if layerKey == "" {
		layerKey = defaultLayerKey
	} else if err := checkIdent("layer_key", layerKey); err != nil {
		return nil, err
	}
	// 换盐会让同一主体落到另一个变体（等于重新分流），因此 salt 必填且改盐必须有 reason。
	hashSeed, err := requiredIdent("hash_seed", in.GetHashSeed())
	if err != nil {
		return nil, err
	}
	bucketCount := l.svcCtx.BucketCount
	if bucketCount <= 0 {
		bucketCount = model.DefaultBucketCount
	}
	start, end := in.GetBucketStart(), in.GetBucketEnd()
	if !model.ValidBucketRange(start, end, bucketCount) {
		return nil, fmt.Errorf("%w: [%d,%d) 不在 [0,%d) 内或左端不小于右端", model.ErrInvalidBucketRange, start, end, bucketCount)
	}
	modelKey, err := optionalIdent("model_key", in.GetModelKey(), colIdent)
	if err != nil {
		return nil, err
	}
	modelVersion, err := optionalIdent("model_version", in.GetModelVersion(), colIdent)
	if err != nil {
		return nil, err
	}
	featureVersion, err := optionalIdent("feature_config_version", in.GetFeatureConfigVersion(), colIdent)
	if err != nil {
		return nil, err
	}
	overrides, err := parseOverrides(in.GetOverrides(), l.svcCtx.Config.Rank.MaxOverridesBytes)
	if err != nil {
		return nil, err
	}
	if err := checkTimeRange(in.GetStartAt(), in.GetEndAt()); err != nil {
		return nil, err
	}
	// 绑定了具体版本却查无此行：分流一开就是把流量送到不存在的模型上，登记阶段就得拦住。
	repo := l.svcCtx.Repository
	want := &model.RankExperiment{
		ExpKey:               expKey,
		VariantKey:           variantKey,
		LayerKey:             layerKey,
		HashSeed:             hashSeed,
		BucketCount:          bucketCount,
		BucketStart:          start,
		BucketEnd:            end,
		ModelKey:             repo.ResolveModelKey(modelKey),
		ModelVersion:         modelVersion,
		FeatureConfigVersion: featureVersion,
		Overrides:            overrides.raw,
		State:                model.ExpStateDraft,
		Revision:             1,
		StartAt:              in.GetStartAt(),
		EndAt:                in.GetEndAt(),
		Operator:             operator,
		Note:                 reason,
	}
	// 绑定可用性 + 同层互斥都要在写入前证明：一开分流就是把流量送到不存在的模型或
	// 与同层另一个变体重叠的区间上，这两种都会让实验结论失效。
	if err := checkVariantBindings(l.ctx, repo, want); err != nil {
		return nil, err
	}
	if conflict, found, err := repo.BucketOverlapConflict(l.ctx, want.LayerKey, start, end, variantKey); err != nil {
		return nil, err
	} else if found {
		return nil, fmt.Errorf("%w: %s [%d,%d) 与同层 %s/%s [%d,%d) 相交（同层互斥分流，区间必须不相交）",
			model.ErrBucketOverlap, want.LayerKey, start, end, conflict.ExpKey, conflict.VariantKey,
			conflict.BucketStart, conflict.BucketEnd)
	}

	err = repo.Experiments().Insert(l.ctx, want)
	switch {
	case err == nil:
		l.Infof("recommend-rank: experiment variant registered exp_key=%s variant_key=%s layer_key=%s "+
			"buckets=[%d,%d) seed=%s state=draft overrides=%s operator=%s idempotency_key=%s",
			expKey, variantKey, want.LayerKey, start, end, hashSeed, summarizeOverrides(overrides), operator, idempotencyKey)
		return &rpc.UpsertExperimentReply{Experiment: experimentInfo(want), Deduplicated: false}, nil
	case errors.Is(err, model.ErrExperimentExists):
		return l.replayOrPatch(want, operator, reason, idempotencyKey)
	default:
		return nil, err
	}
}

// checkVariantBindings 校验变体绑定的模型版本与特征清单是否真实可用。
// 空 model_version 表示「沿用该 model_key 的 ACTIVE 版本」（在线解析），不做存在性校验；
// 给了具体版本号就必须查得到，且不能是已下线的版本。
// UpsertExperiment 与 SetExperimentState(→RUNNING) 共用它：
// 绑定关系可能在登记之后被改动（模型下线、特征停用），开始分流前必须再查一次。
func checkVariantBindings(ctx context.Context, repo *repository.Repository, e *model.RankExperiment) error {
	if e == nil {
		return model.ErrExperimentNotFound
	}
	if e.ModelVersion != "" {
		row, err := repo.ModelVersions().FindOne(ctx, repo.ResolveModelKey(e.ModelKey), e.ModelVersion)
		if err != nil {
			return err
		}
		if row.State == model.ModelStateRetired {
			return fmt.Errorf("%w: %s/%s 已下线，不能作为实验变体的绑定版本",
				model.ErrModelImmutable, row.ModelKey, row.Version)
		}
	}
	if e.FeatureConfigVersion != "" {
		cfg, err := repo.FeatureConfigs().FindOne(ctx, e.FeatureConfigVersion)
		if err != nil {
			return err
		}
		if cfg.State != model.FeatureStateEnabled {
			return fmt.Errorf("%w: %s", model.ErrFeatureConfigDisabled, cfg.ConfigVersion)
		}
	}
	return nil
}

// replayOrPatch 处理「同 (exp_key, variant_key) 再次登记」：
// 语义完全一致 → 幂等回 deduplicated；有差异且状态允许（DRAFT/PAUSED）→ 更新并 revision+1；
// RUNNING/STOPPED 有差异 → ErrExperimentImmutable（改配置先 PAUSED，STOPPED 是终态）。
func (l *UpsertExperimentLogic) replayOrPatch(want *model.RankExperiment, operator, reason,
	idempotencyKey string) (*rpc.UpsertExperimentReply, error) {
	repo := l.svcCtx.Repository
	existing, err := repo.Experiments().FindOne(l.ctx, want.ExpKey, want.VariantKey)
	if err != nil {
		return nil, err
	}
	diff := experimentSemanticDiff(existing, want)
	if len(diff) == 0 {
		l.Infof("recommend-rank: experiment variant upsert deduplicated exp_key=%s variant_key=%s revision=%d "+
			"operator=%s idempotency_key=%s", existing.ExpKey, existing.VariantKey, existing.Revision,
			operator, idempotencyKey)
		return &rpc.UpsertExperimentReply{Experiment: experimentInfo(existing), Deduplicated: true}, nil
	}
	if existing.State != model.ExpStateDraft && existing.State != model.ExpStatePaused {
		return nil, fmt.Errorf("%w: %s/%s 处于 %s，分桶语义 %v 不可改；改配置请先 PAUSED（STOPPED 是终态，请另开 exp_key）",
			model.ErrExperimentImmutable, want.ExpKey, want.VariantKey, expStateName(existing.State), diff)
	}
	// CAS 字段：id + 读到的 revision，SQL 里带 state IN (DRAFT,PAUSED) AND revision=? 条件。
	target := *want
	target.ID = existing.ID
	target.Revision = existing.Revision
	if ok, err := repo.Experiments().UpdateBucketConfig(l.ctx, &target, operator, reason); err != nil {
		return nil, err
	} else if !ok {
		return nil, fmt.Errorf("%w: %s/%s 更新未生效（状态被并发推进或 revision 已变），请重读后再改",
			model.ErrExperimentImmutable, want.ExpKey, want.VariantKey)
	}
	fresh, err := repo.Experiments().FindOne(l.ctx, want.ExpKey, want.VariantKey)
	if err != nil {
		return nil, err
	}
	if fresh.HashSeed != existing.HashSeed {
		// 换盐 = 重新分流。已落库的 sticky 分桶仍会保留（读路径优先查表），
		// 只有新主体按新盐分桶，因此必须留下「谁在什么时候换了盐」的证据。
		if already, err := repo.ListAssignments(l.ctx, existing.ExpKey, existing.VariantKey, 0, 1); err == nil && len(already) > 0 {
			l.Errorf("recommend-rank: experiment hash_seed changed exp_key=%s variant_key=%s from=%s to=%s "+
				"with existing sticky assignments; new subjects will be re-bucketed",
				existing.ExpKey, existing.VariantKey, existing.HashSeed, fresh.HashSeed)
		}
	}
	l.Infof("recommend-rank: experiment variant updated exp_key=%s variant_key=%s revision=%d changed=%v "+
		"state=%s operator=%s reason=%s idempotency_key=%s",
		fresh.ExpKey, fresh.VariantKey, fresh.Revision, diff, expStateName(fresh.State), operator, reason, idempotencyKey)
	return &rpc.UpsertExperimentReply{Experiment: experimentInfo(fresh), Deduplicated: false}, nil
}

// defaultLayerKey 与 model.RankExperimentModel.Insert 的兜底值保持一致，
// 否则 logic 里的重叠校验会查错作用域（空串在 model 侧才归一到 default）。
const defaultLayerKey = "default"

// experimentSemanticDiff 列出会改变分流结果的字段差异。
// overrides 比较前已各自过规范化（同一份 parseOverrides），因此可按字符串逐字节比较。
func experimentSemanticDiff(existing, want *model.RankExperiment) []string {
	var diff []string
	add := func(name string, changed bool) {
		if changed {
			diff = append(diff, name)
		}
	}
	add("layer_key", existing.LayerKey != want.LayerKey)
	add("hash_seed", existing.HashSeed != want.HashSeed)
	add("bucket_start", existing.BucketStart != want.BucketStart)
	add("bucket_end", existing.BucketEnd != want.BucketEnd)
	add("model_key", existing.ModelKey != want.ModelKey)
	add("model_version", existing.ModelVersion != want.ModelVersion)
	add("feature_config_version", existing.FeatureConfigVersion != want.FeatureConfigVersion)
	add("overrides", normalizeOverrideJSON(existing.Overrides) != normalizeOverrideJSON(want.Overrides))
	add("start_at", existing.StartAt != want.StartAt)
	add("end_at", existing.EndAt != want.EndAt)
	return diff
}

// normalizeOverrideJSON 把空/空白 overrides 统一成 "{}"，
// 免得「第一次登记留空、第二次显式给 {}」被误判成语义变更。
func normalizeOverrideJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "{}"
	}
	return strings.TrimSpace(raw)
}

// summarizeOverrides 产出日志用的参数摘要（只有 key 计数，不含任何取值主体）。
func summarizeOverrides(set *overrideSet) string {
	if set.isEmpty() {
		return "none"
	}
	return fmt.Sprintf("weights=%d quota=%d gap=%d cap=%d boost=%.3f",
		len(set.scoreWeights), len(set.sourceQuota), set.diversityGap, set.frequencyCap, set.coldStartBoost)
}
