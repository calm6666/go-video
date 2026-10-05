package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go-video/services/recommend-rank/internal/config"
	"go-video/services/recommend-rank/internal/repository"
	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRankRuntimeConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRankRuntimeConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRankRuntimeConfigLogic {
	return &GetRankRuntimeConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// runtimeSnapshot 是「当前生效配置」的可缓存快照。
//
// 为什么不直接缓存 rpc 回包：现在的 pb.go 里含 unexported 字段，json.Marshal 会成功但还原不回消息，
// 缓存-还原这条路必须完全可逆，所以缓存一个纯数据结构，回包每次由它投影。
type runtimeSnapshot struct {
	ModelKey             string                  `json:"model_key"`
	ActiveModelVersion   string                  `json:"active_model_version"`
	FeatureConfigVersion string                  `json:"feature_config_version"`
	Objectives           []string                `json:"objectives"`
	Running              []*model.RankExperiment `json:"running"`
	OpsConfigRevision    string                  `json:"ops_config_revision"`
	ConfigRevision       string                  `json:"config_revision"`
	// NoActiveModel 记录「这次读到的确实是没有 ACTIVE 模型」而不是「读失败后留空」。
	NoActiveModel bool `json:"no_active_model"`
}

// 下发在线排序参数与当前生效的模型/特征/实验状态
//
// 这是「灰度核对」的读端：调用方拿 config_revision 与本地期望比对，
// 就能知道「这台实例现在跑的到底是哪套配置」，不需要猜。
//
// 语义边界（不变量：不把故障伪装成正常）：
//   - 无 ACTIVE 模型时 active_model_version 回空串 + config_revision 照算，
//     让调用方提前知道「现在排序必然降级」，而不是等 RankCandidates 报错；
//     degrade_enabled 回的是配置真实值，绝不因为「没模型」而顺手置成 false/true；
//   - 模型绑定的特征清单查不到、或权重解析失败 → 直接返回错误：
//     这是配置坏了，回一份「看起来正常」的参数会让灰度核对得出错误结论；
//   - 运营干预（ops-config）启用但下游未接线 → model.ErrOpsConfigNotConfigured（fail closed）；
//     读取失败时保持默认参数、ops_config_revision 留空并记错误日志（本接口是只读核对面，
//     不是一次在线排序，不能因为运营面抖动就否认模型状态）。
//
// 快照走 Cache().GetSnapshot/SetSnapshot + RuntimeConfigKey，TTL 由 Rank.RuntimeConfigCacheTTLSeconds 控制，
// <=0 表示不缓存；缓存读写失败都不影响本次回包（缓存是加速器，不是事实源）。
func (l *GetRankRuntimeConfigLogic) GetRankRuntimeConfig(in *rpc.GetRankRuntimeConfigReq) (*rpc.GetRankRuntimeConfigReply, error) {
	if l.svcCtx == nil || l.svcCtx.Repository == nil {
		return nil, model.ErrRepositoryNotConfigured
	}
	repo := l.svcCtx.Repository
	conf := l.svcCtx.Config.Rank

	var modelKeyRaw, scene string
	if in != nil {
		var err error
		if modelKeyRaw, err = optionalIdent("model_key", in.GetModelKey(), colIdent); err != nil {
			return nil, err
		}
		if scene, err = optionalIdent("scene", in.GetScene(), colIdent); err != nil {
			return nil, err
		}
	}
	modelKey := repo.ResolveModelKey(modelKeyRaw)
	if modelKey == "" {
		return nil, fmt.Errorf("%w: 未配置 Rank.DefaultModelKey，无法确定生效模型", model.ErrNoActiveModel)
	}

	snap, err := l.loadSnapshot(modelKey, scene)
	if err != nil {
		return nil, err
	}
	return l.project(snap, conf), nil
}

// loadSnapshot 先读分钟级快照，未命中或不可用时回到 MySQL 读事实再写回快照。
func (l *GetRankRuntimeConfigLogic) loadSnapshot(modelKey, scene string) (*runtimeSnapshot, error) {
	repo := l.svcCtx.Repository
	cache := repo.Cache()
	ttl := int64(l.svcCtx.Config.Rank.RuntimeConfigCacheTTLSeconds)
	key := repository.RuntimeConfigKey(modelKey)
	if ttl > 0 {
		var hit runtimeSnapshot
		if cache.GetSnapshot(l.ctx, key, &hit) && hit.ModelKey == modelKey {
			return &hit, nil
		}
	}
	snap, err := l.buildSnapshot(modelKey, scene)
	if err != nil {
		return nil, err
	}
	if ttl > 0 {
		cache.SetSnapshot(l.ctx, key, snap, ttl)
	}
	return snap, nil
}

// buildSnapshot 读事实：ACTIVE 模型 + 绑定特征清单 + 生效 RUNNING 变体 + 运营干预代次。
func (l *GetRankRuntimeConfigLogic) buildSnapshot(modelKey, scene string) (*runtimeSnapshot, error) {
	repo := l.svcCtx.Repository
	snap := &runtimeSnapshot{ModelKey: modelKey}

	var (
		activeVersion   string
		activeRevision  int32
		featureVersion  string
		featureRevision int32
		weights         map[string]float64
	)
	active, err := repo.ModelVersions().FindActive(l.ctx, modelKey)
	switch {
	case err == nil:
		activeVersion, activeRevision, featureVersion = active.Version, active.Revision, active.FeatureConfigVersion
		snap.ActiveModelVersion = active.Version
		snap.FeatureConfigVersion = featureVersion
		weights, err = parseObjectiveWeights(active.ObjectiveWeights)
		if err != nil {
			return nil, err
		}
		snap.Objectives = sortedObjectives(weights)
	case errors.Is(err, model.ErrNoActiveModel):
		// 没有 ACTIVE 版本是「可服务但必然降级」的事实，不是读取故障：显式记下来。
		snap.NoActiveModel = true
		l.Errorf("recommend-rank: runtime config has no ACTIVE model version model_key=%s, "+
			"every RankCandidates on this model_key degrades to recall order", modelKey)
	default:
		return nil, err
	}
	// 特征清单必须仍在且处于启用态：清单被停用/被删掉时这里就报错，
	// 而不是等到在线排序持续降级才被察觉。
	if featureVersion != "" {
		cfg, err := repo.FeatureConfigs().FindOne(l.ctx, featureVersion)
		if err != nil {
			return nil, err
		}
		if cfg.State != model.FeatureStateEnabled {
			return nil, fmt.Errorf("%w: %s（被 %s/%s 引用）",
				model.ErrFeatureConfigDisabled, cfg.ConfigVersion, modelKey, activeVersion)
		}
		featureRevision = cfg.Revision
	}

	now := model.NowUnix()
	rows, err := repo.RunningExperiments(l.ctx, now)
	if err != nil {
		return nil, err
	}
	snap.Running = l.variantsForModel(rows, modelKey)
	if limit := positiveInt(repo.Options().RunningExperimentScanLimit, 200); len(rows) >= limit {
		// 扫描上限命中说明 RUNNING 变体数量已经异常，本次视图可能不完整；
		// 宁可回一份快照 + 错误日志，也不静默少给变体（少给会误判灰度覆盖率）。
		l.Errorf("recommend-rank: running variant scan hit limit %d, runtime config view may be truncated", limit)
	}

	opsRevision, err := l.opsConfigRevision(scene)
	if err != nil {
		return nil, err
	}
	snap.OpsConfigRevision = opsRevision
	snap.ConfigRevision = configRevisionOf(modelKey, activeVersion, activeRevision,
		featureVersion, featureRevision, snap.Running, opsRevision)
	// 权重表随行返回：projection 只需要 key 列表，但把长度放进日志便于核对。
	l.Debugf("recommend-rank: runtime config snapshot built model_key=%s active=%s feature=%s objectives=%d "+
		"running_variants=%d ops_revision=%s config_revision=%s", modelKey, activeVersion, featureVersion,
		len(weights), len(snap.Running), opsRevision, snap.ConfigRevision)
	return snap, nil
}

// variantsForModel 过滤出绑定到本 model_key 的生效变体（按 exp_key、bucket_start 稳定排序）。
// 空 ModelKey 的历史行按「默认模型」解释，与 repository.ResolveModelKey 的口径一致。
func (l *GetRankRuntimeConfigLogic) variantsForModel(rows []*model.RankExperiment, modelKey string) []*model.RankExperiment {
	repo := l.svcCtx.Repository
	out := make([]*model.RankExperiment, 0, len(rows))
	for _, r := range rows {
		if repo.ResolveModelKey(r.ModelKey) != modelKey {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ExpKey != out[j].ExpKey {
			return out[i].ExpKey < out[j].ExpKey
		}
		if out[i].BucketStart != out[j].BucketStart {
			return out[i].BucketStart < out[j].BucketStart
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// opsConfigRevision 读取运营干预代次（只取 revision 用于指纹）。
// 未启用时返回空串（等价「运营面不参与本次配置」），启用但下游是 stub 时 fail closed。
func (l *GetRankRuntimeConfigLogic) opsConfigRevision(scene string) (string, error) {
	conf := l.svcCtx.Config.Rank
	reader, err := opsConfigReader(l.svcCtx.Repository.Downstream(), conf.OpsConfigEnabled)
	if err != nil {
		return "", err
	}
	if reader == nil {
		return "", nil
	}
	iv, err := reader.Resolve(l.ctx, scene)
	if err != nil {
		if isUnwired(err) {
			return "", fmt.Errorf("%w: %s", model.ErrOpsConfigNotConfigured, err)
		}
		l.Errorf("recommend-rank: ops config read failed scene=%s err=%v, keep default params", scene, err)
		return "", nil
	}
	return iv.ConfigRevision, nil
}

// project 把快照与本机配置投影成 rpc 回包。
// max_candidates/max_return/score_budget_ms/ttl_seconds 是本机配置事实，
// 不入快照：换实例配置不同就该给出不同答案，缓存别人的配置值会掩盖配置漂移。
func (l *GetRankRuntimeConfigLogic) project(snap *runtimeSnapshot, conf config.RankConf) *rpc.GetRankRuntimeConfigReply {
	objectives := make([]string, len(snap.Objectives))
	copy(objectives, snap.Objectives)
	return &rpc.GetRankRuntimeConfigReply{
		ModelKey:             snap.ModelKey,
		ActiveModelVersion:   snap.ActiveModelVersion,
		FeatureConfigVersion: snap.FeatureConfigVersion,
		MaxCandidates:        positiveInt32(conf.MaxCandidates, 600),
		MaxReturn:            positiveInt32(conf.MaxReturn, 100),
		Objectives:           objectives,
		DegradeEnabled:       conf.DegradeEnabled,
		Fallback:             fallbackFromKey(configuredFallback(conf.DefaultFallback)),
		ScoreBudgetMs:        positiveInt64(conf.ScoreBudgetMs, 80),
		TtlSeconds:           positiveInt64(conf.TtlSeconds, 30),
		RunningExperiments:   experimentInfos(snap.Running),
		ConfigRevision:       snap.ConfigRevision,
	}
}
