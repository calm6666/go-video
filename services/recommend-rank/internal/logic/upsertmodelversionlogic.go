package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertModelVersionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertModelVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertModelVersionLogic {
	return &UpsertModelVersionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 登记模型版本元数据（版本不可变，元数据变更 revision+1）
//
// 登记出来的行永远是 DRAFT：本服务不接受「登记即上线」——
// 上线必须再走一次 SetModelVersionState(READY) → (ACTIVE)，
// 这样「谁在什么时候把哪个版本推上线」才是两条可审计的事实（AGENTS.md §8 的同一套纪律）。
//
// 语义字段（feature_config_version、objective_weights）在登记后不可改：
// 同版本号必须同语义，否则历史 rank_decision_log 里的 model_version 无法解释。
// 非语义字段（artifact_ref、offline_metrics）只在 DRAFT/READY 下可更新（model.UpdateMetadata
// 的 SQL 自带 state<>ACTIVE 保护），ACTIVE 后要换工件就先登记新版本再切换。
func (l *UpsertModelVersionLogic) UpsertModelVersion(in *rpc.UpsertModelVersionReq) (*rpc.UpsertModelVersionReply, error) {
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
	modelKey, err := requiredIdent("model_key", in.GetModelKey())
	if err != nil {
		return nil, err
	}
	version, err := requiredIdent("version", in.GetVersion())
	if err != nil {
		return nil, err
	}
	featureVersion, err := requiredIdent("feature_config_version", in.GetFeatureConfigVersion())
	if err != nil {
		return nil, err
	}
	weights := l.svcCtx.Config.Rank.MaxWeightSum
	if weights <= 0 {
		weights = 10
	}
	_, weightsJSON, err := objectiveWeightsFromRPC(in.GetObjectiveWeights(), weights)
	if err != nil {
		return nil, err
	}
	artifactRef, err := checkArtifactRef(in.GetArtifactRef(), l.svcCtx.Config.Rank.MaxArtifactRefBytes)
	if err != nil {
		return nil, err
	}
	offlineMetrics, err := checkOfflineMetrics(in.GetOfflineMetrics())
	if err != nil {
		return nil, err
	}
	// 绑定的特征清单必须已登记且处于启用态：绑到没登记的版本上，激活后就是「无特征可取」，
	// 这类自伤必须在登记阶段拦住，而不是等在线排序持续降级才发现。
	cfg, err := l.svcCtx.Repository.FeatureConfigs().FindOne(l.ctx, featureVersion)
	if err != nil {
		return nil, err
	}
	if cfg.State != model.FeatureStateEnabled {
		return nil, fmt.Errorf("%w: %s", model.ErrFeatureConfigDisabled, featureVersion)
	}

	repo := l.svcCtx.Repository
	row := &model.RankModelVersion{
		ModelKey:             modelKey,
		Version:              version,
		FeatureConfigVersion: cfg.ConfigVersion,
		ObjectiveWeights:     weightsJSON,
		ArtifactRef:          artifactRef,
		OfflineMetrics:       offlineMetrics,
		State:                model.ModelStateDraft,
		Revision:             1,
		Operator:             operator,
		Note:                 reason,
	}
	err = repo.ModelVersions().Insert(l.ctx, row)
	switch {
	case err == nil:
		l.Infof("recommend-rank: model version registered model_key=%s version=%s state=draft weights=%s "+
			"feature_config=%s operator=%s idempotency_key=%s",
			modelKey, version, weightsJSON, cfg.ConfigVersion, operator, idempotencyKey)
		return modelVersionReply(row, false), nil
	case errors.Is(err, model.ErrModelVersionExists):
		return l.replayOrPatch(row, operator, idempotencyKey)
	default:
		return nil, err
	}
}

// replayOrPatch 处理「同 (model_key, version) 再登记」：
// 语义一致才幂等，语义不一致返回 ErrModelImmutable；
// 语义一致但工件/离线指标变了，则按非语义元数据更新（revision+1）。
func (l *UpsertModelVersionLogic) replayOrPatch(want *model.RankModelVersion, operator, idempotencyKey string) (
	*rpc.UpsertModelVersionReply, error) {
	repo := l.svcCtx.Repository
	existing, err := repo.ModelVersions().FindOne(l.ctx, want.ModelKey, want.Version)
	if err != nil {
		return nil, err
	}
	if diff := modelVersionSemanticDiff(existing, want); len(diff) > 0 {
		return nil, fmt.Errorf("%w: %s/%s 已登记，语义字段 %v 不可改；要改语义请登记新版本（落库权重按规范化 JSON 逐字节比较）",
			model.ErrModelImmutable, want.ModelKey, want.Version, diff)
	}
	metadataDrift := existing.ArtifactRef != want.ArtifactRef || existing.OfflineMetrics != want.OfflineMetrics
	if metadataDrift && existing.State == model.ModelStateActive {
		// ACTIVE 版本的元数据不可热改（model.UpdateMetadata 的 SQL 也拒），
		// 否则同一次在线排序前后会指向两个不同的工件。
		return nil, fmt.Errorf("%w: ACTIVE 版本 %s/%s 禁止改工件引用与离线指标，请登记新版本后再切换",
			model.ErrModelImmutable, want.ModelKey, want.Version)
	}
	if metadataDrift {
		ok, err := repo.ModelVersions().UpdateMetadata(l.ctx, existing.ID, want.ArtifactRef, want.OfflineMetrics, operator, want.Note)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: 元数据更新未生效（版本状态已被并发切换）", model.ErrModelStateTransition)
		}
		fresh, err := repo.ModelVersions().FindOne(l.ctx, want.ModelKey, want.Version)
		if err != nil {
			return nil, err
		}
		existing = fresh
		repo.Cache().InvalidateModelConfig(l.ctx, existing.ModelKey)
		l.Infof("recommend-rank: model version metadata updated model_key=%s version=%s revision=%d "+
			"operator=%s idempotency_key=%s", existing.ModelKey, existing.Version, existing.Revision, operator, idempotencyKey)
	}
	return modelVersionReply(existing, true), nil
}

// modelVersionSemanticDiff 返回语义字段差异名。
// 权重比较用规范化 JSON 逐字节比：map 遍历序随机，不规范化就会把同一版本判成两次不同登记。
func modelVersionSemanticDiff(existing, want *model.RankModelVersion) []string {
	var diff []string
	if existing.FeatureConfigVersion != want.FeatureConfigVersion {
		diff = append(diff, "feature_config_version")
	}
	if existing.ObjectiveWeights != want.ObjectiveWeights {
		diff = append(diff, "objective_weights")
	}
	return diff
}

func modelVersionReply(row *model.RankModelVersion, deduplicated bool) *rpc.UpsertModelVersionReply {
	return &rpc.UpsertModelVersionReply{
		ModelKey:     row.ModelKey,
		Version:      row.Version,
		State:        toRPCModelState(row.State),
		Revision:     row.Revision,
		Deduplicated: deduplicated,
	}
}

// checkArtifactRef 校验模型工件引用：只允许对象存储 key，禁止完整 URL 与内联凭据。
// 长度上限取 min(config.Rank.MaxArtifactRefBytes, 列宽 512)——配置只能收紧不能放宽。
func checkArtifactRef(raw string, maxBytes int) (string, error) {
	ref := strings.TrimSpace(raw)
	if maxBytes <= 0 || maxBytes > colArtifactRef {
		maxBytes = colArtifactRef
	}
	if ref == "" {
		return "", nil // 工件尚未产出时允许先登记；SetModelVersionState(ACTIVE) 会拒绝空工件
	}
	if len(ref) > maxBytes {
		return "", fmt.Errorf("%w: %d > %d", model.ErrArtifactRefInvalid, len(ref), maxBytes)
	}
	if strings.Contains(ref, "://") || strings.ContainsAny(ref, " \t\n") {
		return "", fmt.Errorf("%w: 只允许 object key，禁止带 scheme 的完整地址与空白", model.ErrArtifactRefInvalid)
	}
	return ref, nil
}

// checkOfflineMetrics 校验离线指标：允许留空；给出时必须是 JSON 对象且落在列宽内。
// 它只供人工核对，不参与在线决策（见 000001 迁移说明），因此不做字段白名单，
// 但坚持「是对象」——一串数字或数组无法承载「哪个指标是多少」的语义。
func checkOfflineMetrics(raw string) (string, error) {
	metrics := strings.TrimSpace(raw)
	if metrics == "" {
		return "", nil
	}
	if len(metrics) > colMetrics {
		return "", fmt.Errorf("%w: %d > %d", model.ErrOfflineMetricsInvalid, len(metrics), colMetrics)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metrics), &parsed); err != nil {
		return "", fmt.Errorf("%w: must be a JSON object: %v", model.ErrOfflineMetricsInvalid, err)
	}
	return metrics, nil
}
