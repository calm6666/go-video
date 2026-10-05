package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go-video/services/recommend-rank/internal/config"
	"go-video/services/recommend-rank/internal/repository"
	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RankCandidatesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRankCandidatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RankCandidatesLogic {
	return &RankCandidatesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// reasonCodeReplay 标明「这一位来自幂等回放，分数与目标值未落库所以无法复现」。
// 模型顺序（rk.because.<source>）与兜底顺序（rk.because.<fallback>）都不该冒充它。
const reasonCodeReplay = "rk.because.replay"

// rankRequest 是一次排序请求里已经校验归一好的部分，避免各步骤重复解析与参数长串。
type rankRequest struct {
	requestID    string
	generatedID  bool // request_id 由服务端生成时不具备重放语义，日志要能区分
	mid          int64
	deviceHash   string
	scene        string
	platform     int32
	appVersion   string
	region       string
	traceID      string
	snapshotID   string
	idempotency  string
	limit        int
	allowDegrade bool

	cands       []rankedCandidate
	allowed     map[int64]struct{}
	inputDigest string
	started     time.Time
	conf        config.RankConf
}

// rankPlan 是本次排序「实际用了哪套配置」的事实，也是决策摘要要落的字段。
type rankPlan struct {
	modelKey             string
	modelVersion         string
	featureConfigVersion string
	weights              map[string]float64
	missingPolicy        string
	overrides            *overrideSet
	// servable=false 表示没有可用的模型+特征组合：本次只能按兜底顺序出，且必须已声明降级。
	servable bool
	// pinnedModel/pinnedFeature 是实验变体钉住的版本与特征清单（优先于 ACTIVE 行的绑定）。
	pinnedModel   string
	pinnedFeature string

	subjectType int32
	subjectID   string
	bucketNo    int32
	expKey      string
	variantKey  string
	expRevision int32
	// stickyUsed 标记本次分组来自已落库的 sticky 行而非现算（日志与核对用）。
	stickyUsed bool
}

// 多目标排序：入参候选子集内排序，输出可审计结果摘要
//
// 实现顺序（每一步的故障都必须落成显式降级，不允许「看起来正常」）：
//  1. 入参校验：条数 <= MaxCandidates（超出 ErrTooManyCandidates，不静默裁剪）、limit <= MaxReturn、
//     aid 为正、source 在枚举内、device_id_hash 必须是 sha256（明文设备号一律拒绝，AGENTS.md §2/§7）；
//  2. 幂等回放：request_id 命中既有 decision 就按既有事实回包，并比对 input_digest——
//     同一 request_id 换了输入报 ErrRequestIDReused，绝不拿旧结果冒充；
//  3. 取配置：ACTIVE 模型 + 绑定特征清单 + RUNNING 变体，任一不可用记 model.DegradeReason*，
//     allow_degrade=false 时直接返回错误（压测/回放环境不接受半截结果）；
//  4. 分桶：model.BucketOf 的纯函数结果，命中 RUNNING 且生效的变体（未命中为 control）；
//     已落库的 sticky 分桶优先，保证与 GetExperimentAssignment 给出同一个变体；
//     排序本身不写分桶行（写责任归 GetExperimentAssignment，热路径不产生写放大）；
//  5. 过滤：去重 -> 内容安全复核（读不到结论按不通过处理并声明，不默认放行）；
//  6. 打分：按变体 overrides 覆盖后的权重逐条合成，缺失值按特征清单登记的 missing_policy 处理，
//     超出 Rank.ScoreBudgetMs 就停止打分并裁剪未打分候选；
//  7. 不变量：出参 aid 必须都在入参集合内，破坏即放弃本次结果回退召回原序；
//  8. 落库：写 rank_decision_log（两个 digest、top_aids、过滤计数、降级标记与明细），
//     写失败只记错误日志、不翻转已产出的排序结果；降级结果 ttl_seconds 固定 0。
func (l *RankCandidatesLogic) RankCandidates(in *rpc.RankCandidatesReq) (*rpc.RankCandidatesReply, error) {
	if l.svcCtx == nil || l.svcCtx.Repository == nil {
		return nil, model.ErrRepositoryNotConfigured
	}
	if in == nil {
		return nil, model.ErrEmptyCandidates
	}
	req, err := l.parseRequest(in)
	if err != nil {
		return nil, err
	}
	deg := &degradation{}
	repo := l.svcCtx.Repository

	// --- 2. 幂等回放 ---
	if row, err := repo.DecisionLogs().FindByRequestID(l.ctx, req.requestID); err == nil {
		return l.replay(row, req)
	} else if !errors.Is(err, model.ErrDecisionNotFound) {
		return nil, err
	}

	// --- 3~4. 配置解析与分桶命中 ---
	plan, err := l.buildPlan(req, deg)
	if err != nil {
		return nil, err
	}

	// --- 5. 去重与安全复核 ---
	cands, deduped := dedupe(req.cands)
	visible, visErr := l.checkVisibility(cands, req.conf)
	switch {
	case visErr == nil:
		var dropped int32
		cands, dropped = keepVisible(cands, visible)
		deg.addDetail(fmt.Sprintf("safety filtered=%d", dropped))
	case errors.Is(visErr, model.ErrSafetyGateNotConfigured):
		// 配置声明启用却没有接线：这是部署不一致，必须硬报错而不是降级继续。
		return nil, visErr
	case !req.allowDegrade:
		deg.mark(model.DegradeReasonSafetyUnavailable, "", visErr.Error())
		return nil, degradeError(deg)
	default:
		// 结论读不到 = 一条都不放行（宁可少给也不给未复核的内容），并声明降级。
		deg.mark(model.DegradeReasonSafetyUnavailable, "", visErr.Error())
		cands, _ = keepVisible(cands, map[int64]bool{})
	}
	safetyFiltered := int32(len(req.cands)) - int32(len(cands)) - deduped

	allowed := make(map[int64]struct{}, len(cands))
	for _, c := range cands {
		allowed[c.aid] = struct{}{}
	}

	// --- 6~7. 打分与不变量校验 ---
	items, scored, truncated := l.rank(cands, plan, req, deg)
	if !assertSubset(items, allowed) {
		l.Errorf("recommend-rank: candidate subset invariant broken request_id=%s model=%s/%s",
			req.requestID, plan.modelKey, plan.modelVersion)
		deg.mark(model.DegradeReasonModelUnavailable, model.FallbackRecallOrder, model.ErrCandidateSubsetBroken.Error())
		if !req.allowDegrade {
			return nil, degradeError(deg)
		}
		items, cut := l.fallbackOrder(cands, req.limit, model.FallbackRecallOrder)
		items, truncated = markReasonCode(items, model.FallbackRecallOrder), cut
		scored = 0
	}
	if deg.occurred && !req.allowDegrade {
		return nil, degradeError(deg)
	}

	// --- 8. 落库与回包 ---
	return l.commit(req, plan, deg, items, scored, truncated, safetyFiltered, deduped)
}

// parseRequest 只做「请求规模与隐私」校验：所有超限都是报错，不静默裁剪。
func (l *RankCandidatesLogic) parseRequest(in *rpc.RankCandidatesReq) (*rankRequest, error) {
	conf := l.svcCtx.Config.Rank
	rc := in.GetContext()
	req := &rankRequest{
		mid:          rc.GetMid(),
		started:      time.Now(),
		allowDegrade: in.GetAllowDegrade(),
		conf:         conf,
	}
	if rc.GetRequestId() != "" {
		id, err := requiredIdent("request_id", rc.GetRequestId())
		if err != nil {
			return nil, err
		}
		req.requestID = id
	} else {
		// 契约允许 request_id「回显或服务端生成」；生成值无法被调用方重放，日志要能看出来。
		req.requestID = "gen" + model.NewDecisionID(rc.GetTraceId(), model.NowUnix())[:29]
		req.generatedID = true
	}
	req.deviceHash = strings.TrimSpace(rc.GetDeviceIdHash())
	if err := checkDeviceHash(req.deviceHash); err != nil {
		return nil, err
	}
	req.platform = int32(rc.GetPlatform())
	if !model.ValidPlatform(req.platform) {
		return nil, fmt.Errorf("%w: platform=%d", model.ErrInvalidSubjectType, req.platform)
	}
	var err error
	if req.appVersion, err = optionalIdent("app_version", rc.GetAppVersion(), colAppVersion); err != nil {
		return nil, err
	}
	if req.region, err = optionalIdent("region", rc.GetRegion(), colRegion); err != nil {
		return nil, err
	}
	if req.scene, err = optionalIdent("scene", rc.GetScene(), colIdent); err != nil {
		return nil, err
	}
	if req.traceID, err = optionalIdent("trace_id", rc.GetTraceId(), colIdent); err != nil {
		return nil, err
	}
	if req.snapshotID, err = optionalIdent("snapshot_id", in.GetSnapshotId(), colIdent); err != nil {
		return nil, err
	}
	if req.idempotency, err = optionalIdent("idempotency_key", in.GetIdempotencyKey(), colIdent); err != nil {
		return nil, err
	}
	if req.idempotency == "" {
		req.idempotency = req.requestID // 契约：幂等键为空时用 request_id 兜底
	}
	if req.limit, err = l.resultLimit(in.GetLimit()); err != nil {
		return nil, err
	}
	maxCandidates := int(positiveInt32(l.svcCtx.MaxCandidates, 600))
	if len(in.GetCandidates()) == 0 {
		// 没有候选就没有决策可记：报错而不是写一行「零输入零输出」的摘要污染审计表。
		return nil, model.ErrEmptyCandidates
	}
	if req.cands, err = normalizeCandidates(in.GetCandidates(), maxCandidates); err != nil {
		return nil, err
	}
	// input_digest 按原始入参序列计算（含重复项）：它是「这两次请求输入相同」的唯一证据，
	// 去重之后的序列会把不同入参压成同一个摘要。
	req.inputDigest = model.DigestAids(aidSequence(req.cands))
	return req, nil
}

// resultLimit 校验出参条数：0 表示「按本机上限给」，负数与超限都拒绝。
func (l *RankCandidatesLogic) resultLimit(limit int32) (int, error) {
	maxReturn := positiveInt32(l.svcCtx.MaxReturn, 100)
	if limit < 0 {
		return 0, fmt.Errorf("%w: limit=%d", model.ErrLimitTooLarge, limit)
	}
	if limit == 0 {
		return int(maxReturn), nil
	}
	if limit > maxReturn {
		return 0, fmt.Errorf("%w: limit=%d > %d", model.ErrLimitTooLarge, limit, maxReturn)
	}
	return int(limit), nil
}

// buildPlan 解析「实验变体 -> 模型版本 -> 特征清单」这条绑定链。
//
// 任何一环不可用都会：记降级 -> （配置了 previous_model 且有上一 ACTIVE 版本时）换版本重试 ->
// 仍不可用则 plan.servable=false，由 rank 走兜底顺序。
func (l *RankCandidatesLogic) buildPlan(req *rankRequest, deg *degradation) (*rankPlan, error) {
	repo := l.svcCtx.Repository
	plan := &rankPlan{
		modelKey:   repo.ResolveModelKey(""),
		variantKey: model.ControlVariant,
		overrides:  &overrideSet{scoreWeights: map[string]float64{}, sourceQuota: map[int32]float64{}},
	}
	if plan.modelKey == "" {
		return nil, fmt.Errorf("%w: 未配置 Rank.DefaultModelKey，无法确定服务模型", model.ErrNoActiveModel)
	}
	plan.subjectType, plan.subjectID = subjectOf(req.mid, req.deviceHash)
	if err := l.resolveExperiment(req, plan, deg); err != nil {
		return nil, err
	}
	return l.resolveModel(req, plan, deg)
}

// resolveExperiment 计算桶号并命中变体。
//
// 桶号是 model.BucketOf 的纯函数（不看时间、不看实例），所以任何一台机器对同一主体
// 都给出同一变体；已落库的 sticky 行优先，保证 PAUSED/改区间之后不跳组。
func (l *RankCandidatesLogic) resolveExperiment(req *rankRequest, plan *rankPlan, deg *degradation) error {
	repo := l.svcCtx.Repository
	now := model.NowUnix()
	variants, err := repo.RunningExperiments(l.ctx, now)
	if err != nil {
		// 实验配置读不到不致命：退回「无实验」路径用默认模型，但必须声明（实验期结论会缺一角）。
		deg.mark(model.DegradeReasonExperimentUnavailable, "", "读取 RUNNING 变体失败: "+err.Error())
		if !req.allowDegrade {
			return degradeError(deg)
		}
		return nil
	}
	if plan.subjectType == 0 || plan.subjectID == "" {
		deg.addDetail("assignment skipped: no mid and no device hash")
		return nil
	}
	bucketCount := positiveInt32(repo.BucketCount(), model.DefaultBucketCount)
	hit := firstLayerHit(variants, plan.subjectType, plan.subjectID, bucketCount, now, deg)
	if hit == nil {
		return nil
	}
	// 与 GetExperimentAssignment 同源：有 sticky 行就以它为准（行本身不可变，因此读一次就够）。
	sticky, variant := l.stickyVariant(plan, hit, variants)
	if variant == nil {
		if sticky != nil {
			// sticky 指向的变体已不在生效集合里（实验已收尾）：用现算结果，并记下这条不一致。
			deg.addDetail(fmt.Sprintf("assignment %s ignored: variant not running", sticky.VariantKey))
		}
		variant = hit
	} else {
		plan.stickyUsed = true
	}
	plan.bucketNo = bucketOf(variant, plan.subjectType, plan.subjectID, bucketCount)
	plan.expKey, plan.variantKey, plan.expRevision = variant.ExpKey, variant.VariantKey, variant.Revision
	plan.modelKey = repo.ResolveModelKey(variant.ModelKey)
	overrides, err := parseOverrides(variant.Overrides, req.conf.MaxOverridesBytes)
	if err != nil {
		// 变体参数坏了就不能让它参与决策：整体退回 control + 默认模型。
		deg.mark(model.DegradeReasonExperimentUnavailable, "", variant.ExpKey+"/"+variant.VariantKey+" overrides 非法: "+err.Error())
		if !req.allowDegrade {
			return degradeError(deg)
		}
		plan.expKey, plan.variantKey, plan.expRevision = "", model.ControlVariant, 0
		plan.modelKey = repo.ResolveModelKey("")
		plan.overrides = &overrideSet{scoreWeights: map[string]float64{}, sourceQuota: map[int32]float64{}}
		return nil
	}
	plan.overrides = overrides
	if variant.ModelVersion != "" {
		// 变体钉住的版本优先于 ACTIVE（实验的意义就是「这组流量跑这个版本」）。
		plan.pinnedModel = variant.ModelVersion
	}
	if variant.FeatureConfigVersion != "" {
		plan.pinnedFeature = variant.FeatureConfigVersion
	}
	return nil
}

// resolveModel 解析「实际生效的模型版本 + 特征清单」。
//
// 候选版本顺序：变体钉住的版本 -> 当前 ACTIVE -> （配置 previous_model 兜底且 ACTIVE 行记有
// 上一版本时）那个上一版本。逐个尝试第一个「权重可解析且特征清单在生效」的组合；
// 全部不可用则 plan.servable=false，由 rank 走兜底顺序（此时降级原因已经记好）。
func (l *RankCandidatesLogic) resolveModel(req *rankRequest, plan *rankPlan, deg *degradation) (*rankPlan, error) {
	repo := l.svcCtx.Repository
	fallback := configuredFallback(req.conf.DefaultFallback)
	tried := make(map[string]struct{}, 3)

	var rows []*model.RankModelVersion
	if plan.pinnedModel != "" {
		row, err := repo.ModelVersions().FindOne(l.ctx, plan.modelKey, plan.pinnedModel)
		switch {
		case err == nil && (row.State == model.ModelStateActive || row.State == model.ModelStateReady):
			rows = append(rows, row)
		case err != nil && !errors.Is(err, model.ErrModelNotFound):
			if err := l.noteReadFailure(req, deg, model.DegradeReasonStoreUnavailable, err.Error()); err != nil {
				return nil, err
			}
		default:
			why := "未登记"
			if err == nil {
				why = "状态为 " + modelStateName(row.State)
			}
			if err := l.noteReadFailure(req, deg, model.DegradeReasonModelUnavailable,
				fmt.Sprintf("变体钉住的 %s/%s %s，改用 ACTIVE", plan.modelKey, plan.pinnedModel, why)); err != nil {
				return nil, err
			}
		}
	}
	active, err := repo.ModelVersions().FindActive(l.ctx, plan.modelKey)
	switch {
	case err == nil:
		rows = append(rows, active)
		// previous_model 兜底不是「换个说法继续降级」：ACTIVE 行的 previous_active 是激活事务里
		// 读到的真实上一版本，它仍在库里，权重可解析时确实能恢复一次正常打分。
		if fallback == model.FallbackPreviousModel && active.PreviousActive != "" {
			prev, err := repo.ModelVersions().FindOne(l.ctx, plan.modelKey, active.PreviousActive)
			switch {
			case err == nil:
				rows = append(rows, prev)
			case !errors.Is(err, model.ErrModelNotFound):
				if err := l.noteReadFailure(req, deg, model.DegradeReasonStoreUnavailable, err.Error()); err != nil {
					return nil, err
				}
			}
		}
	case errors.Is(err, model.ErrNoActiveModel):
		if err := l.noteReadFailure(req, deg, model.DegradeReasonModelUnavailable,
			plan.modelKey+" 无 ACTIVE 版本"); err != nil {
			return nil, err
		}
	default:
		if err := l.noteReadFailure(req, deg, model.DegradeReasonStoreUnavailable, err.Error()); err != nil {
			return nil, err
		}
	}

	for _, row := range rows {
		if _, dup := tried[row.Version]; dup {
			continue
		}
		tried[row.Version] = struct{}{}
		if err := l.bindModel(req, plan, row, deg); err != nil {
			return nil, err
		}
		if plan.servable {
			return plan, nil
		}
		if !req.allowDegrade {
			return nil, degradeError(deg)
		}
	}
	return plan, nil
}

// bindModel 把某个模型版本变成「可用的一套权重 + 特征清单」。
// 权重不可解析、清单未登记或已停用都会让 servable=false 并记下降级原因：
// 这三类都是「配置坏了」，绝不能拿空权重或停用清单去产出一份看起来正常的排序。
func (l *RankCandidatesLogic) bindModel(req *rankRequest, plan *rankPlan, row *model.RankModelVersion,
	deg *degradation) error {
	weights, err := parseObjectiveWeights(row.ObjectiveWeights)
	if err != nil {
		return l.noteReadFailure(req, deg, model.DegradeReasonModelUnavailable,
			row.ModelKey+"/"+row.Version+" 权重不可解析: "+err.Error())
	}
	if len(weights) == 0 {
		return l.noteReadFailure(req, deg, model.DegradeReasonModelUnavailable,
			row.ModelKey+"/"+row.Version+" 未登记优化目标")
	}
	configVersion := row.FeatureConfigVersion
	if plan.pinnedFeature != "" && row.Version == plan.pinnedModel {
		configVersion = plan.pinnedFeature
	}
	cfg, err := l.svcCtx.Repository.FeatureConfigs().FindOne(l.ctx, configVersion)
	switch {
	case err == nil && cfg.State != model.FeatureStateEnabled:
		return l.noteReadFailure(req, deg, model.DegradeReasonFeatureUnavailable,
			"特征清单 "+cfg.ConfigVersion+" 已停用")
	case err != nil && !errors.Is(err, model.ErrFeatureConfigNotFound):
		return l.noteReadFailure(req, deg, model.DegradeReasonStoreUnavailable, err.Error())
	case err != nil:
		return l.noteReadFailure(req, deg, model.DegradeReasonFeatureUnavailable,
			"特征清单 "+configVersion+" 未登记")
	}
	plan.modelVersion = row.Version
	plan.featureConfigVersion = cfg.ConfigVersion
	plan.weights = weights
	plan.missingPolicy = cfg.MissingPolicy
	plan.servable = true
	return nil
}

// noteReadFailure 记录一次配置读取故障：允许降级时返回 nil（调用方继续走兜底顺序），
// 否则把「已降级」如实转成错误——回放/压测环境不接受降级结果。
func (l *RankCandidatesLogic) noteReadFailure(req *rankRequest, deg *degradation, reason, detail string) error {
	deg.mark(reason, configuredFallback(req.conf.DefaultFallback), detail)
	if req.allowDegrade {
		return nil
	}
	return degradeError(deg)
}

// firstLayerHit 在每个互斥层内找命中变体，再按 (layer_key, exp_key, variant_key) 取第一个。
//
// 契约只能回一个变体，多层正交同时命中时无法全部表达：取字典序第一个（确定），
// 其余写进降级明细（README 已知缺口）。同层出现两个命中说明互斥被破坏，直接声明不可用。
func firstLayerHit(variants []*model.RankExperiment, subjectType int32, subjectID string,
	bucketCount int32, now int64, deg *degradation) *model.RankExperiment {
	byLayer := make(map[string][]*model.RankExperiment, 4)
	for _, v := range variants {
		if !effectiveNow(v, now) {
			continue
		}
		bucket := bucketOf(v, subjectType, subjectID, bucketCount)
		if bucket >= v.BucketStart && bucket < v.BucketEnd {
			byLayer[v.LayerKey] = append(byLayer[v.LayerKey], v)
		}
	}
	if len(byLayer) == 0 {
		return nil
	}
	layers := make([]string, 0, len(byLayer))
	for layer := range byLayer {
		layers = append(layers, layer)
	}
	sort.Strings(layers)
	for _, layer := range layers {
		got := byLayer[layer]
		if len(got) > 1 {
			deg.addDetail(fmt.Sprintf("layer %s has %d overlapping running variants", layer, len(got)))
		}
		sort.SliceStable(got, func(i, j int) bool {
			if got[i].ExpKey != got[j].ExpKey {
				return got[i].ExpKey < got[j].ExpKey
			}
			return got[i].VariantKey < got[j].VariantKey
		})
		if len(layers) > 1 {
			deg.addDetail(fmt.Sprintf("multi-layer hit: %d layers, using %s", len(layers), layers[0]))
		}
		return got[0]
	}
	return nil
}

// bucketOf 用变体自己登记的盐与桶空间算桶号（与 model.BucketOf 同一口径）。
func bucketOf(v *model.RankExperiment, subjectType int32, subjectID string, bucketCount int32) int32 {
	space := v.BucketCount
	if space <= 0 {
		space = bucketCount
	}
	return model.BucketOf(subjectType, subjectID, v.ExpKey, v.HashSeed, space)
}

// effectiveNow 判定变体当前是否在生效时间窗内（end_at=0 表示长期）。
func effectiveNow(v *model.RankExperiment, now int64) bool {
	if v.State != model.ExpStateRunning {
		return false
	}
	return v.StartAt <= now && (v.EndAt == 0 || v.EndAt > now)
}

// stickyVariant 读该主体在该实验当前盐下的分组行。
// 返回 (行, 变体)：行为 nil 表示没有 sticky 记录（用现算结果）；
// 行存在但变体 nil 表示记录指向一个已不生效的变体（同样回落到现算结果，但要留痕）。
func (l *RankCandidatesLogic) stickyVariant(plan *rankPlan, hit *model.RankExperiment,
	variants []*model.RankExperiment) (*model.RankExperimentAssignment, *model.RankExperiment) {
	repo := l.svcCtx.Repository
	row, err := repo.Assignments().FindOne(l.ctx, hit.ExpKey, plan.subjectType, plan.subjectID, hit.HashSeed)
	if err != nil {
		if !errors.Is(err, model.ErrAssignmentNotFound) {
			l.Errorf("recommend-rank: assignment read failed exp_key=%s err=%v", hit.ExpKey, err)
		}
		return nil, nil
	}
	if row.BucketNo < 0 || row.BucketNo >= repo.BucketCount() {
		l.Errorf("recommend-rank: assignment bucket_no %d out of space %d exp_key=%s",
			row.BucketNo, repo.BucketCount(), hit.ExpKey)
		return row, nil
	}
	// 命中变体所在层与分组行的层是同一层才有意义（同层互斥、异层正交）。
	for _, v := range variants {
		if v.ExpKey == row.ExpKey && v.VariantKey == row.VariantKey && effectiveNow(v, model.NowUnix()) {
			return row, v
		}
	}
	return row, nil
}

// commit 组装决策摘要行、落库并回包。
func (l *RankCandidatesLogic) commit(req *rankRequest, plan *rankPlan, deg *degradation,
	items []rankedCandidate, scored, truncated, safetyFiltered, deduped int32) (*rpc.RankCandidatesReply, error) {
	repo := l.svcCtx.Repository
	now := req.started.Unix()
	row := &model.RankDecisionLog{
		DecisionID:           model.NewDecisionID(req.requestID, now),
		RequestID:            req.requestID,
		IdempotencyKey:       req.idempotency,
		TraceID:              req.traceID,
		SnapshotID:           req.snapshotID,
		Mid:                  req.mid,
		SubjectType:          plan.subjectType,
		SubjectID:            plan.subjectID,
		Scene:                req.scene,
		Platform:             req.platform,
		AppVersion:           req.appVersion,
		Region:               req.region,
		ExpKey:               plan.expKey,
		VariantKey:           plan.variantKey,
		BucketNo:             plan.bucketNo,
		ExpRevision:          plan.expRevision,
		ModelKey:             plan.modelKey,
		ModelVersion:         plan.modelVersion,
		FeatureConfigVersion: plan.featureConfigVersion,
		InputCount:           int32(len(req.cands)),
		ReturnedCount:        int32(len(items)),
		ScoredCount:          scored,
		SourceSummary:        sourceSummary(items),
		InputDigest:          req.inputDigest,
		ResultDigest:         model.DigestAids(aidSequence(items)),
		TopAids:              model.JoinAids(headAids(items, repo.MaxDigestAids())),
		SafetyFiltered:       safetyFiltered,
		DedupFiltered:        deduped,
		Truncated:            truncated,
		CostMs:               int32Clamp(time.Since(req.started).Milliseconds()),
	}
	// 召回池版本是「这一屏结果出自哪一版候选」的唯一落库证据（batch_id 无对应列，只进明细）。
	poolVersion, mixedPool, batchText := poolProvenance(req.cands)
	row.PoolVersion = poolVersion
	if mixedPool {
		deg.addDetail("recall pool_version mixed across candidates")
	}
	if batchText != "" {
		deg.addDetail(batchText)
	}
	if plan.stickyUsed {
		deg.addDetail("assignment from sticky row")
	}
	deg.fillRow(row)

	if err := repo.DecisionLogs().Insert(l.ctx, row); err != nil {
		if errors.Is(err, model.ErrDecisionExists) {
			// 并发下另一实例已写入同 request_id：以它的行为准回包，不产生第二条决策事实。
			if existing, findErr := repo.DecisionLogs().FindByRequestID(l.ctx, req.requestID); findErr == nil {
				return l.replay(existing, req)
			}
		}
		// 摘要写失败不翻转已产出的排序结果（可用性优先），但错误必须留痕以便告警补数。
		l.Errorf("recommend-rank: decision log insert failed request_id=%s decision_id=%s generated_id=%t err=%v",
			req.requestID, row.DecisionID, req.generatedID, err)
	} else {
		repo.Cache().SetDecisionPointer(l.ctx, req.requestID, row.DecisionID, req.conf.DecisionReplayCacheTTLSeconds)
	}
	l.Infof("recommend-rank: ranked request_id=%s generated_id=%t scene=%s model=%s/%s feature=%s "+
		"exp=%s/%s bucket=%d sticky=%t input=%d returned=%d scored=%d degraded=%t reason=%s fallback=%s cost_ms=%d",
		req.requestID, req.generatedID, req.scene, row.ModelKey, row.ModelVersion, row.FeatureConfigVersion,
		row.ExpKey, row.VariantKey, row.BucketNo, plan.stickyUsed, row.InputCount, row.ReturnedCount,
		row.ScoredCount, deg.occurred, deg.reasonKey(), deg.fallbackKey(), row.CostMs)

	return &rpc.RankCandidatesReply{
		DecisionId:           row.DecisionID,
		RequestId:            req.requestID,
		Items:                rankedItems(items),
		ResultDigest:         row.ResultDigest,
		ModelKey:             row.ModelKey,
		ModelVersion:         row.ModelVersion,
		FeatureConfigVersion: row.FeatureConfigVersion,
		ExpKey:               row.ExpKey,
		VariantKey:           row.VariantKey,
		BucketNo:             row.BucketNo,
		RecallSnapshotId:     req.snapshotID,
		InputCount:           row.InputCount,
		ReturnedCount:        row.ReturnedCount,
		Filters: &rpc.FilterStat{
			SafetyFiltered: safetyFiltered,
			DedupFiltered:  deduped,
			Truncated:      truncated,
		},
		Degradation: deg.info(scored, row.CostMs),
		TtlSeconds:  recommendedTTL(deg, req.conf),
	}, nil
}

// rank 取特征并打分；不可打分时按配置的兜底策略排序（两条路都必须有显式标记）。
func (l *RankCandidatesLogic) rank(cands []rankedCandidate, plan *rankPlan, req *rankRequest,
	deg *degradation) ([]rankedCandidate, int32, int32) {
	fallback := configuredFallback(req.conf.DefaultFallback)
	if !plan.servable || len(plan.weights) == 0 {
		// 没有可用模型/权重：buildPlan 已经记过原因，这里只负责给出兜底顺序。
		items, truncated := l.fallbackOrder(cands, req.limit, fallback)
		return markReasonCode(items, fallback), 0, truncated
	}
	itemFeats, priorFeats, featErr := l.fetchFeatures(cands, plan, req)
	if featErr != nil {
		deg.mark(model.DegradeReasonFeatureUnavailable, fallback, featErr.Error())
		if !req.allowDegrade {
			return nil, 0, 0
		}
		items, truncated := l.fallbackOrder(cands, req.limit, fallback)
		return markReasonCode(items, fallback), 0, truncated
	}
	if len(itemFeats) == 0 {
		// 一条特征都没有：拿「全 0 分」排序就是给降级结果穿上正常外衣，明确禁止。
		deg.mark(model.DegradeReasonFeatureUnavailable, fallback, "内容特征为空")
		if !req.allowDegrade {
			return nil, 0, 0
		}
		items, truncated := l.fallbackOrder(cands, req.limit, fallback)
		return markReasonCode(items, fallback), 0, truncated
	}

	budget := positiveInt64(req.conf.ScoreBudgetMs, 80)
	out := scoreCandidates(scoreRequest{
		cands:           cands,
		weights:         plan.weights,
		itemFeats:       itemFeats,
		priors:          priorFeats,
		missingPolicy:   plan.missingPolicy,
		overrides:       plan.overrides,
		budgetExhausted: func() bool { return time.Since(req.started) >= time.Duration(budget)*time.Millisecond },
	})
	if out.rejected {
		deg.mark(model.DegradeReasonFeatureUnavailable, fallback, "missing_policy=reject 命中缺失特征")
		if !req.allowDegrade {
			return nil, 0, 0
		}
		items, truncated := l.fallbackOrder(cands, req.limit, fallback)
		return markReasonCode(items, fallback), 0, truncated
	}
	if len(out.droppedSources) > 0 {
		// drop_source 是「整路不要」而不是截断，FilterStat 没有对应位，只能进明细（README 已记）。
		deg.addDetail(fmt.Sprintf("drop_source removed sources=%v items=%d", out.droppedSources, len(out.dropped)))
	}
	ordered := orderScored(out.scored)
	truncated := int32(0)
	if len(out.unscored) > 0 {
		deg.mark(model.DegradeReasonBudgetExhausted, fallback,
			fmt.Sprintf("budget %dms exhausted, dropped unscored=%d", budget, len(out.unscored)))
		truncated += int32(len(out.unscored))
	}
	if !plan.overrides.isEmpty() && len(plan.overrides.sourceQuota) > 0 {
		var quotaDropped int32
		ordered, quotaDropped = applyQuota(ordered, plan.overrides.sourceQuota, req.limit)
		truncated += quotaDropped
		if quotaDropped > 0 {
			deg.addDetail(fmt.Sprintf("source_quota dropped=%d", quotaDropped))
		}
	}
	ordered, cut := truncate(ordered, req.limit)
	truncated += cut
	if deg.occurred && !req.allowDegrade {
		return nil, 0, 0
	}
	// 只有真正按模型分排出来的顺序才用来源码；一旦走兜底顺序就用兜底码。
	code := ""
	if deg.occurred {
		code = deg.fallbackKey()
	}
	return markReasonCode(ordered, code), int32(len(ordered)), truncated
}

// fallbackOrder 按配置的兜底策略排序（不打分）。
func (l *RankCandidatesLogic) fallbackOrder(cands []rankedCandidate, limit int, fallback string) ([]rankedCandidate, int32) {
	var ordered []rankedCandidate
	switch fallback {
	case model.FallbackSafetyOnly:
		ordered = orderInputOrder(cands)
	case model.FallbackPreviousModel:
		// 没有可用的上一模型版本时才落到这里：召回原序是唯一不依赖外部状态的选择。
		ordered = orderRecallOrder(cands)
	default:
		ordered = orderRecallOrder(cands)
	}
	return truncate(ordered, limit)
}

// fetchFeatures 取内容特征与 spm 质量先验（按 FeatureBatchSize 分批，整体受 DownstreamTimeoutMs 约束）。
// 未启用特征读取时返回 error，让调用方按「特征不可用」降级——不能拿空特征去打分冒充正常结果。
//
// 用户侧特征（FeatureSource.UserFeatures）本期不取：打分只按 aid 维度查表，
// 用户特征要生效必须先落到「目标同名特征」里，这条口径写进了 README 待接线清单。
func (l *RankCandidatesLogic) fetchFeatures(cands []rankedCandidate, plan *rankPlan,
	req *rankRequest) (map[int64]repository.FeatureVector, map[int64]repository.FeatureVector, error) {
	down := l.svcCtx.Repository.Downstream()
	src, err := featureSource(down, req.conf.FeatureFetchEnabled)
	if err != nil {
		return nil, nil, err
	}
	if src == nil {
		return nil, nil, fmt.Errorf("%w: Rank.FeatureFetchEnabled=false，模型预估不可得", model.ErrFeatureSourceNotConfigured)
	}
	beh, err := behaviorSource(down, req.conf.BehaviorFetchEnabled)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := l.downstreamContext()
	defer cancel()

	aids := aidList(cands)
	batch := positiveInt(req.conf.FeatureBatchSize, 128)

	itemFeats := make(map[int64]repository.FeatureVector, len(aids))
	if err := eachBatch(aids, batch, func(part []int64) error {
		got, err := src.ItemFeatures(ctx, req.scene, plan.featureConfigVersion, part)
		if err != nil {
			return err
		}
		mergeFeatures(itemFeats, got)
		return nil
	}); err != nil {
		return nil, nil, err
	}

	priors := make(map[int64]repository.FeatureVector, len(aids))
	if beh != nil {
		if err := eachBatch(aids, batch, func(part []int64) error {
			got, err := beh.ContentQuality(ctx, part)
			if err != nil {
				return err
			}
			mergeFeatures(priors, got)
			return nil
		}); err != nil {
			// 先验只是缺失兜底，取不到不致命，但会让「缺失按默认值」的比例上升，必须留痕。
			l.Errorf("recommend-rank: spm content quality read failed err=%v", err)
			priors = map[int64]repository.FeatureVector{}
		}
	}
	return itemFeats, priors, nil
}

// mergeFeatures 合并一批特征（后到的同 aid 向量不覆盖已取到的：下游分批不会返回重复 aid，
// 真出现重复说明下游实现有问题，保留先到的那份让结果仍可解释）。
func mergeFeatures(dst, src map[int64]repository.FeatureVector) {
	for aid, fv := range src {
		if _, ok := dst[aid]; ok {
			continue
		}
		dst[aid] = fv
	}
}

// eachBatch 切分 aid 列表逐批下发（下游接口都有「单请求条数不超过 FeatureBatchSize」的约定）。
// 任一批失败即中止并把错误上抛：半批特征拼出来的打分比不打分更危险。
func eachBatch(aids []int64, size int, fn func(part []int64) error) error {
	if len(aids) == 0 {
		return nil
	}
	if size <= 0 || size >= len(aids) {
		return fn(aids)
	}
	for start := 0; start < len(aids); start += size {
		end := start + size
		if end > len(aids) {
			end = len(aids)
		}
		if err := fn(aids[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// downstreamContext 给下游读取套上超时：降级要来得及发生，才不会把网关超时当成本服务故障。
func (l *RankCandidatesLogic) downstreamContext() (context.Context, context.CancelFunc) {
	ms := positiveInt64(l.svcCtx.Config.Rank.DownstreamTimeoutMs, 30)
	return context.WithTimeout(l.ctx, time.Duration(ms)*time.Millisecond)
}

// checkVisibility 取内容安全复核结论。
// 返回 ErrSafetyGateNotConfigured 表示「配置声明启用但没接线」（部署不一致），调用方须硬报错；
// 其它 error 表示一次读取故障，调用方按不通过处理并声明降级。
func (l *RankCandidatesLogic) checkVisibility(cands []rankedCandidate, conf config.RankConf) (map[int64]bool, error) {
	gate, err := safetyGate(l.svcCtx.Repository.Downstream(), conf.SafetyCheckEnabled)
	if err != nil {
		return nil, err
	}
	if gate == nil {
		return nil, fmt.Errorf("%w: Rank.SafetyCheckEnabled=false，未复核可见性", model.ErrSafetyGateNotConfigured)
	}
	ctx, cancel := context.WithTimeout(l.ctx, time.Duration(positiveInt64(conf.DownstreamTimeoutMs, 30))*time.Millisecond)
	defer cancel()
	aids := aidList(cands)
	batch := positiveInt(conf.FeatureBatchSize, 128)
	visible := make(map[int64]bool, len(aids))
	if err := eachBatch(aids, batch, func(part []int64) error {
		got, err := gate.VisibleAids(ctx, part)
		if err != nil {
			return err
		}
		if got == nil {
			return fmt.Errorf("recommend-rank: safety gate returned nil visibility for %d aids", len(part))
		}
		for aid, ok := range got {
			// 同一 aid 被两批都回答时取「与」：任何一次说不可见就不可见（失败关闭）。
			prev, seen := visible[aid]
			visible[aid] = ok && (!seen || prev)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return visible, nil
}

// replay 用既有决策行回包（幂等重放，不重复打分）。
//
// 能复现的事实：request_id/decision_id、模型三元组、实验分组、降级标记、过滤计数、result_digest、
// 前 MaxDigestAids 个 aid；不能复现的是整页分数与目标值（表里没有这些列），
// 因此重建出的 item 分数留空（0）、objectives 为空，并把 reason_code 标成 rk.because.replay，
// 绝不伪造一份看起来正常的分数（README 已知缺口）。
func (l *RankCandidatesLogic) replay(row *model.RankDecisionLog, req *rankRequest) (*rpc.RankCandidatesReply, error) {
	if row.InputDigest != req.inputDigest {
		return nil, fmt.Errorf("%w: request_id=%s 的入参序列已变（digest %s != %s）",
			model.ErrRequestIDReused, req.requestID, shortDigest(row.InputDigest), shortDigest(req.inputDigest))
	}
	byAid := make(map[int64]rankedCandidate, len(req.cands))
	for _, c := range req.cands {
		byAid[c.aid] = c
	}
	items := make([]rankedCandidate, 0, len(row.TopAids))
	for _, aid := range model.SplitAids(row.TopAids) {
		c, ok := byAid[aid]
		if !ok {
			// 原结果的这一位不在本次入参里：说明两次入参不同，上面 digest 已拦住，这里不该发生。
			l.Errorf("recommend-rank: replay aid %d missing from candidates request_id=%s", aid, req.requestID)
			continue
		}
		c.score, c.objectives, c.scored = 0, nil, false
		c.reasonCode = reasonCodeReplay
		items = append(items, c)
	}
	deg := &degradation{occurred: row.Degraded != 0, reason: row.DegradeReason, fallback: row.FallbackStrategy}
	if text := strings.TrimSpace(row.DegradeDetail); text != "" {
		deg.appendDetail(text)
	}
	deg.appendDetail(fmt.Sprintf("replayed from rank_decision_log decision_id=%s items=%d/%d (top_aids only)",
		row.DecisionID, len(items), row.ReturnedCount))
	l.Infof("recommend-rank: rank request replayed request_id=%s decision_id=%s items=%d returned=%d",
		req.requestID, row.DecisionID, len(items), row.ReturnedCount)
	return &rpc.RankCandidatesReply{
		DecisionId:           row.DecisionID,
		RequestId:            row.RequestID,
		Items:                rankedItems(items),
		ResultDigest:         row.ResultDigest,
		ModelKey:             row.ModelKey,
		ModelVersion:         row.ModelVersion,
		FeatureConfigVersion: row.FeatureConfigVersion,
		ExpKey:               row.ExpKey,
		VariantKey:           row.VariantKey,
		BucketNo:             row.BucketNo,
		RecallSnapshotId:     row.SnapshotID,
		InputCount:           row.InputCount,
		ReturnedCount:        int32(len(items)),
		Filters:              filterStat(row),
		Degradation:          deg.info(row.ScoredCount, row.CostMs),
		TtlSeconds:           0, // 回放结果不再被缓存：它不含分数，缓存会把「未知」扩散出去
	}, nil
}

// recommendedTTL 只有完全正常的结果才给缓存秒数：
// 降级结果固定 0，否则一次故障期的兜底顺序会被网关缓存成「一段时间所有人同一顺序」。
func recommendedTTL(deg *degradation, conf config.RankConf) int64 {
	if deg.occurred {
		return 0
	}
	return positiveInt64(conf.TtlSeconds, 30)
}

// headAids 取前 n 个 aid（top_aids 列宽受配置 MaxDigestAids 约束）。
func headAids(in []rankedCandidate, n int) []int64 {
	if n <= 0 {
		return nil
	}
	aids := aidSequence(in)
	if len(aids) > n {
		return aids[:n]
	}
	return aids
}

// shortDigest 用于错误文案里的摘要前缀（完整 digest 由审计行提供，日志不需要整串）。
func shortDigest(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
