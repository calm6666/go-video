package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpdateFeatureStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateFeatureStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateFeatureStateLogic {
	return &UpdateFeatureStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// stateSnapshot 是状态变更回执的可回放快照。
type stateSnapshot struct {
	FeatureKey string `json:"k"`
	Version    int32  `json:"v"`
	State      int32  `json:"s"`
}

// 变更特征状态（DRAFT/ACTIVE/RETIRED）
//
// 状态机迁移 + 指针变更 + 审计必须同生共死：所以三者在一个事务里，
// 且 ACTIVE 唯一性由 feature_active_version 的一行指针 + 事务内条件更新保证，
// 不靠「扫描 state 列有几个 ACTIVE」。
// 升到 ACTIVE 的前置条件（缺一不可，全部以 ErrFeatureStateTransition 显式失败）：
//  1. 该版本没有 PENDING/RUNNING 的回填作业（边回填边对外读 = 线上值在无人复核时持续变化）；
//  2. 该版本已有值，或同 key 存在上一生效版本可作 PREVIOUS_VERSION 降级来源；
//  3. 该 key 当前没有别的生效版本（要替换请用 SwitchFeatureVersion，它有乐观校验）。
func (l *UpdateFeatureStateLogic) UpdateFeatureState(
	in *rpc.UpdateFeatureStateReq) (*rpc.UpdateFeatureStateReply, error) {
	key := strings.TrimSpace(in.GetFeatureKey())
	op := strings.TrimSpace(in.GetOperator())
	if err := checkFeatureKey(key); err != nil {
		return nil, err
	}
	if err := checkVersion(in.GetVersion()); err != nil {
		return nil, err
	}
	if err := checkOperator(op); err != nil {
		return nil, err
	}
	if err := checkReason(in.GetReason()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	to := int32(in.GetState())
	if !model.ValidFeatureState(to) {
		return nil, fmt.Errorf("%w: state %d is not a declared value", model.ErrFeatureStateTransition, to)
	}

	spec := receiptSpec{
		requestID:  strings.TrimSpace(in.GetRequestId()),
		opType:     model.ReceiptOpStateChange,
		featureKey: key,
		version:    in.GetVersion(),
		rowCount:   1,
		operator:   op,
		reason:     strings.TrimSpace(in.GetReason()),
	}
	res, owner, err := beginReceipt(l.ctx, l.svcCtx, l.Logger, spec)
	if err != nil {
		return nil, err
	}
	if !res.Execute {
		return l.replay(res.Receipt)
	}

	def, err := l.svcCtx.Definitions.FindOne(l.ctx, key, in.GetVersion())
	if err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	if def.State == to {
		// 重复提交同一目标状态：幂等成功，不再追加一条 from==to 的噪声审计。
		return l.finishAndReply(spec, owner, def, true)
	}
	if !model.ValidFeatureStateTransition(def.State, to) {
		err := fmt.Errorf("%w: %s@v%d %d -> %d", model.ErrFeatureStateTransition,
			key, def.Version, def.State, to)
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}

	pointer, err := l.svcCtx.ActiveVersions.FindOne(l.ctx, key)
	if err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	if to == model.FeatureStateActive {
		if err := l.checkActivationPreconditions(def, pointer); err != nil {
			failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
			return nil, err
		}
	}

	if err := l.applyInTx(def, pointer, to, spec, op); err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	// 指针变了就必须让指针缓存失效：陈旧指针等于「切了状态在线还按旧版解释」。
	invalidateCache(l.ctx, l.svcCtx, l.Logger, []string{model.ActiveVersionCacheKey(key)})

	stored, err := l.svcCtx.Definitions.FindOne(l.ctx, key, def.Version)
	if err != nil {
		return nil, err
	}
	return l.finishAndReply(spec, owner, stored, false)
}

// checkActivationPreconditions 是「升 ACTIVE」的三道前置检查。
func (l *UpdateFeatureStateLogic) checkActivationPreconditions(def *model.FeatureDefinition,
	pointer *model.ActiveVersion) error {
	unfinished, err := l.svcCtx.Backfills.CountUnfinished(l.ctx, def.FeatureKey, def.Version)
	if err != nil {
		return err
	}
	if unfinished > 0 {
		return fmt.Errorf("%w: %d unfinished backfill job(s) on %s@v%d",
			model.ErrFeatureStateTransition, unfinished, def.FeatureKey, def.Version)
	}
	if pointer.HasActive() && pointer.ActiveVersion != def.Version {
		return fmt.Errorf("%w: %s already serves v%d, use SwitchFeatureVersion to move the pointer",
			model.ErrMultipleActiveVersions, def.FeatureKey, pointer.ActiveVersion)
	}
	values, err := l.svcCtx.Values.CountByVersion(l.ctx, def.FeatureKey, def.Version)
	if err != nil {
		return err
	}
	if values == 0 && pointer.PreviousVersion < 1 {
		return fmt.Errorf("%w: %s@v%d has no values and no previous version to degrade to",
			model.ErrFeatureStateTransition, def.FeatureKey, def.Version)
	}
	return nil
}

// applyInTx 在一个事务里完成「状态 CAS + 指针变更 + 审计追加」。
// 审计先写、指针后切：switch_id 要落到指针行的 last_switch_id 上，
// 而审计插入失败会让整个事务回滚，不会留下「指针动了但没人知道」的状态。
func (l *UpdateFeatureStateLogic) applyInTx(def *model.FeatureDefinition, pointer *model.ActiveVersion,
	to int32, spec receiptSpec, op string) error {
	return l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		ok, err := l.svcCtx.Definitions.UpdateState(ctx, def.FeatureKey, def.Version, def.State, to)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s@v%d changed concurrently",
				model.ErrFeatureStateTransition, def.FeatureKey, def.Version)
		}
		if err := l.svcCtx.ActiveVersions.Ensure(ctx, tx, def.FeatureKey); err != nil {
			return err
		}
		switchID, err := l.svcCtx.Switches.Append(ctx, tx, &model.VersionSwitch{
			FeatureKey:  def.FeatureKey,
			SwitchType:  model.SwitchTypeStateChange,
			FromVersion: def.Version,
			ToVersion:   def.Version,
			FromValue:   strconv.FormatInt(int64(def.State), 10),
			ToValue:     strconv.FormatInt(int64(to), 10),
			FromDigest:  def.DefinitionDigest,
			ToDigest:    def.DefinitionDigest,
			Operator:    clip(op, maxOperatorLen),
			Reason:      clip(spec.reason, maxReasonLen),
			RequestID:   spec.requestID,
			TraceID:     traceIDOf(ctx),
		})
		if err != nil {
			return err
		}
		switch {
		case to == model.FeatureStateActive:
			// 前置检查已排除「已有别的生效版本」，这里 expectFrom 只会是 0（首次上线）。
			ok, err = l.svcCtx.ActiveVersions.Switch(ctx, tx, def.FeatureKey,
				model.NoActiveVersion, def.Version, switchID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: active pointer moved concurrently", model.ErrVersionConflict)
			}
		case to == model.FeatureStateRetired && pointer.HasActive() && pointer.ActiveVersion == def.Version:
			// 下线的正是当前生效版本：指针回到「无生效版本」，previous_version 记下被下线的版本，
			// 读侧仍能按 PREVIOUS_VERSION → DEFAULT_VALUE 的顺序降级而不是直接报错。
			ok, err = l.svcCtx.ActiveVersions.RetireActive(ctx, tx, def.FeatureKey, def.Version, switchID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: active pointer moved concurrently", model.ErrVersionConflict)
			}
		}
		return nil
	})
}

// finishAndReply 落回执并回响应。
func (l *UpdateFeatureStateLogic) finishAndReply(spec receiptSpec, owner string,
	def *model.FeatureDefinition, reused bool) (*rpc.UpdateFeatureStateReply, error) {
	raw := marshalJSON(stateSnapshot{FeatureKey: def.FeatureKey, Version: def.Version, State: def.State})
	err := finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   raw,
		ResultDigest: resultDigest(raw),
		AffectedRows: 1,
		DetailKept:   true,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.UpdateFeatureStateReply{Definition: defToProto(def), Reused: reused}, nil
}

// replay 回放首次状态变更的结果（定义取库里当前行，状态变更不可原地改写值语义）。
func (l *UpdateFeatureStateLogic) replay(receipt *model.WriteReceipt) (*rpc.UpdateFeatureStateReply, error) {
	var snap stateSnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: state receipt unreadable: %w", err)
	}
	def, err := l.svcCtx.Definitions.FindOne(l.ctx, snap.FeatureKey, snap.Version)
	if err != nil {
		return nil, err
	}
	return &rpc.UpdateFeatureStateReply{Definition: defToProto(def), Reused: true}, nil
}
