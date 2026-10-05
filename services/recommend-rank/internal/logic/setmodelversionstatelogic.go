package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetModelVersionStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetModelVersionStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetModelVersionStateLogic {
	return &SetModelVersionStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 切换模型版本状态（READY/ACTIVE/RETIRED；激活是回滚开关）
//
// 只接受 READY/ACTIVE/RETIRED 三个目标态，operator/reason/idempotency_key 必填
// （激活/回滚是审计事件，没有归因主体的切换不受理）。
// 激活走 Repository().ActivateModel：同一事务内「旧 ACTIVE 置 RETIRED + 新版本置 ACTIVE」，
// CAS 未命中返回 ErrModelStateTransition，因此不可能出现同一 model_key 两个 ACTIVE 版本；
// 其余状态走 ModelVersions().UpdateState（条件 UPDATE + RowsAffected）。
// 状态迁移一律经 model.CanTransitionModelState 判定，RETIRED 不允许复活。
// event_id 本期恒空：MQ 未接（见 README 已知缺口），不允许伪造一个 event_id 返回。
func (l *SetModelVersionStateLogic) SetModelVersionState(in *rpc.SetModelVersionStateReq) (*rpc.SetModelVersionStateReply, error) {
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
	target, err := modelStateFromRPC(in.GetTargetState())
	if err != nil {
		return nil, err
	}

	repo := l.svcCtx.Repository
	row, err := repo.ModelVersions().FindOne(l.ctx, modelKey, version)
	if err != nil {
		return nil, err
	}
	// 目标态即当前态：这是重放，不是「第二次切换」，回 deduplicated 且不推进状态。
	if row.State == target {
		l.Infof("recommend-rank: model version state change deduplicated model_key=%s version=%s state=%s "+
			"operator=%s idempotency_key=%s", modelKey, version, modelStateName(target), operator, idempotencyKey)
		return setModelStateReply(false, target, row.PreviousActive, true), nil
	}
	if !model.CanTransitionModelState(row.State, target) {
		return nil, fmt.Errorf("%w: %s/%s %s -> %s 非法（只允许 DRAFT→READY、READY→ACTIVE、DRAFT/READY/ACTIVE→RETIRED）",
			model.ErrModelStateTransition, modelKey, version, modelStateName(row.State), modelStateName(target))
	}
	// previous_active_version 的口径是「切换前该 model_key 的 ACTIVE 版本」，
	// 必须在切换动作之前读出来：切换完再查就查不到旧值了。
	previous, err := l.activeVersionBeforeSwitch(row)
	if err != nil {
		return nil, err
	}
	if target == model.ModelStateActive {
		return l.activate(row, operator, reason, idempotencyKey)
	}
	l.warnIfRetiringActive(row, target)
	ok, err := repo.ModelVersions().UpdateState(l.ctx, row.ID, row.State, target, operator, reason)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 条件 UPDATE 未命中：状态已被并发切换，必须让调用方重读而不是当成成功。
		return nil, fmt.Errorf("%w: %s/%s %s -> %s 未生效（并发切换）",
			model.ErrModelStateTransition, modelKey, version, modelStateName(row.State), modelStateName(target))
	}
	repo.Cache().InvalidateModelConfig(l.ctx, modelKey)
	l.Infof("recommend-rank: model version state changed model_key=%s version=%s from=%s to=%s "+
		"previous_active=%s operator=%s reason=%s idempotency_key=%s",
		modelKey, version, modelStateName(row.State), modelStateName(target), previous, operator, reason, idempotencyKey)
	return setModelStateReply(true, target, previous, false), nil
}

// activate 走仓储层事务激活：SQL 自带「当前 state=READY」条件，
// 并发下两个实例同时激活同一版本只有一个拿到 changed=true，另一个拿到 ErrModelStateTransition。
func (l *SetModelVersionStateLogic) activate(row *model.RankModelVersion, operator, reason,
	idempotencyKey string) (*rpc.SetModelVersionStateReply, error) {
	repo := l.svcCtx.Repository
	// 激活是「这个版本开始承接线上流量」的唯一闸门，完整性在此逐项校验：
	// READY 只代表登记完成，绑定关系与工件在 READY 之后仍可能被改动。
	if strings.TrimSpace(row.ArtifactRef) == "" {
		return nil, fmt.Errorf("%w: %s/%s 缺 artifact_ref，激活前必须补齐工件引用",
			model.ErrArtifactRefInvalid, row.ModelKey, row.Version)
	}
	weights, err := parseObjectiveWeights(row.ObjectiveWeights)
	if err != nil {
		return nil, err
	}
	if len(weights) == 0 {
		return nil, fmt.Errorf("%w: %s/%s 未登记优化目标，激活后无法合成分",
			model.ErrInvalidWeight, row.ModelKey, row.Version)
	}
	cfg, err := repo.FeatureConfigs().FindOne(l.ctx, row.FeatureConfigVersion)
	if err != nil {
		return nil, err
	}
	if cfg.State != model.FeatureStateEnabled {
		return nil, fmt.Errorf("%w: %s（激活 %s/%s 要求它绑定的特征清单在生效）",
			model.ErrFeatureConfigDisabled, cfg.ConfigVersion, row.ModelKey, row.Version)
	}

	previousActive, changed, err := repo.ActivateModel(l.ctx, row.ID, row.ModelKey, operator, reason)
	if err != nil {
		return nil, err
	}
	if !changed {
		// 事务内发现目标行已是 ACTIVE：幂等命中，不重复推进审计时间线。
		l.Infof("recommend-rank: model version activation deduplicated model_key=%s version=%s "+
			"previous_active=%s operator=%s idempotency_key=%s",
			row.ModelKey, row.Version, previousActive, operator, idempotencyKey)
		return setModelStateReply(false, model.ModelStateActive, previousActive, true), nil
	}
	l.Infof("recommend-rank: model version activated model_key=%s version=%s previous_active=%s weights=%d "+
		"feature_config=%s operator=%s reason=%s idempotency_key=%s",
		row.ModelKey, row.Version, previousActive, len(weights), cfg.ConfigVersion, operator, reason, idempotencyKey)
	return setModelStateReply(true, model.ModelStateActive, previousActive, false), nil
}

// activeVersionBeforeSwitch 读出切换前该 model_key 的 ACTIVE 版本（无 ACTIVE 时回空串）。
func (l *SetModelVersionStateLogic) activeVersionBeforeSwitch(row *model.RankModelVersion) (string, error) {
	if row.State == model.ModelStateActive {
		// 下线当前 ACTIVE 版本时，「切换前的 ACTIVE」就是它自己。
		return row.Version, nil
	}
	active, err := l.svcCtx.Repository.ModelVersions().FindActive(l.ctx, row.ModelKey)
	if err != nil {
		if errors.Is(err, model.ErrNoActiveModel) {
			return "", nil
		}
		return "", err
	}
	return active.Version, nil
}

// warnIfRetiringActive 记录「本次切换会让该 model_key 暂时没有 ACTIVE 版本」：
// 在线排序会全部落到兜底顺序，这是允许的操作（真要下线就得接受），但不能无人知晓。
func (l *SetModelVersionStateLogic) warnIfRetiringActive(row *model.RankModelVersion, target int32) {
	if row.State != model.ModelStateActive || target != model.ModelStateRetired {
		return
	}
	l.Errorf("recommend-rank: retiring the only ACTIVE model version model_key=%s version=%s; "+
		"until a new version is activated every RankCandidates on this model_key degrades to recall order",
		row.ModelKey, row.Version)
}

func setModelStateReply(changed bool, state int32, previousActive string, deduplicated bool) *rpc.SetModelVersionStateReply {
	return &rpc.SetModelVersionStateReply{
		Changed:               changed,
		State:                 toRPCModelState(state),
		PreviousActiveVersion: previousActive,
		Deduplicated:          deduplicated,
		EventId:               "", // MQ 未接：留空而不是伪造（README 已知缺口）
	}
}
