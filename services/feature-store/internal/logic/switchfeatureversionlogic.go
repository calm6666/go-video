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
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SwitchFeatureVersionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSwitchFeatureVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SwitchFeatureVersionLogic {
	return &SwitchFeatureVersionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// switchSnapshot 是切换回执的可回放快照。
type switchSnapshot struct {
	FeatureKey    string `json:"k"`
	ActiveVersion int32  `json:"a"`
	SwitchID      int64  `json:"s"`
	Switched      bool   `json:"t"`
}

// switchOutcome 是事务内真正发生的事后事实，供回执与响应复用。
type switchOutcome struct {
	activeVersion int32
	switchID      int64
	switched      bool
}

// 切换对外生效的版本（乐观校验 + 审计留痕）
//
// 一个事务里按顺序完成：锁指针行（FOR UPDATE，串行化同 key 的并发切换）→ 乐观校验 →
// 目标版本自洽校验 → 追加审计 → CAS 切指针。审计先于指针，因为指针行要记 last_switch_id，
// 而任何一步失败都会整体回滚，不会留下「指针动了但没人知道」。
// 回滚不是删历史：切回 previous_version 时追加一条 switch_type=rollback（引用被撤销的
// 那条 switch_id），「切错了又切回去」在审计里成对出现。
func (l *SwitchFeatureVersionLogic) SwitchFeatureVersion(
	in *rpc.SwitchFeatureVersionReq) (*rpc.SwitchFeatureVersionReply, error) {
	key := strings.TrimSpace(in.GetFeatureKey())
	op := strings.TrimSpace(in.GetOperator())
	if err := checkFeatureKey(key); err != nil {
		return nil, err
	}
	if err := checkVersion(in.GetFromVersion()); err != nil {
		return nil, err
	}
	if err := checkVersion(in.GetToVersion()); err != nil {
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
	if in.GetFromVersion() == in.GetToVersion() {
		return nil, fmt.Errorf("%w: from_version == to_version", model.ErrFeatureVersionRequired)
	}

	spec := receiptSpec{
		requestID:  strings.TrimSpace(in.GetRequestId()),
		opType:     model.ReceiptOpSwitch,
		featureKey: key,
		version:    in.GetToVersion(),
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

	outcome := switchOutcome{}
	if err := l.applyInTx(key, in, spec, op, &outcome); err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	// 值键带 version，天然不冲突，所以只清指针键。
	invalidateCache(l.ctx, l.svcCtx, l.Logger, []string{model.ActiveVersionCacheKey(key)})

	raw := marshalJSON(switchSnapshot{FeatureKey: key, ActiveVersion: outcome.activeVersion,
		SwitchID: outcome.switchID, Switched: outcome.switched})
	if err := finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   raw,
		ResultDigest: resultDigest(raw),
		AffectedRows: boolToRows(outcome.switched),
		DetailKept:   true,
	}); err != nil {
		return nil, err
	}
	l.Infow("feature version switched", logx.Field("feature_key", key),
		logx.Field("to_version", outcome.activeVersion), logx.Field("switch_id", outcome.switchID),
		logx.Field("operator", op))
	return &rpc.SwitchFeatureVersionReply{
		Switched:      outcome.switched,
		ActiveVersion: outcome.activeVersion,
		SwitchId:      outcome.switchID,
	}, nil
}

// applyInTx 是切换的全部副作用，二态输出：要么整体成功，要么整体回滚。
func (l *SwitchFeatureVersionLogic) applyInTx(key string, in *rpc.SwitchFeatureVersionReq,
	spec receiptSpec, op string, out *switchOutcome) error {
	return l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		pointer, err := l.svcCtx.ActiveVersions.LockForUpdate(ctx, tx, key)
		if err != nil {
			return err
		}
		active := pointer.ActiveVersion
		// 乐观校验：expected_from_version=0 表示调用方（回填作业内部）显式放弃这一层，
		// 但契约里 from_version 必填，它同样声明了「调用方看到的当前版本」，两者都要对得上。
		if expected := in.GetExpectedFromVersion(); expected != 0 && expected != active {
			return fmt.Errorf("%w: expected v%d but server serves v%d",
				model.ErrVersionConflict, expected, active)
		}
		if in.GetFromVersion() != active {
			return fmt.Errorf("%w: from_version v%d but server serves v%d",
				model.ErrVersionConflict, in.GetFromVersion(), active)
		}
		if active == in.GetToVersion() {
			// 目标已是生效版本：不追加审计、不改指针，幂等返回现状。
			out.activeVersion, out.switchID, out.switched = active, pointer.LastSwitchID, false
			return nil
		}
		target, err := l.svcCtx.Definitions.FindOne(ctx, key, in.GetToVersion())
		if err != nil {
			return fmt.Errorf("feature-store: target version %s@v%d: %w", key, in.GetToVersion(), err)
		}
		if target.State != model.FeatureStateActive {
			return fmt.Errorf("%w: %s@v%d is not ACTIVE, run UpdateFeatureState first",
				model.ErrFeatureNotActive, key, target.Version)
		}
		if active > 0 {
			current, err := l.svcCtx.Definitions.FindOne(ctx, key, active)
			switch {
			case errors.Is(err, model.ErrFeatureNotFound):
				// 指针指向了不存在的定义：数据不一致，不能靠切换「顺手」把它盖过去。
				return fmt.Errorf("%w: active pointer of %s points at missing v%d",
					model.ErrActiveVersionMissing, key, active)
			case err != nil:
				return err
			}
			if !current.SameImmutableAs(target) {
				// 口径变了必须换新 key：同一个 key 的含义不能在排序侧脚下漂移。
				return fmt.Errorf("%w: %s@v%d and v%d differ on immutable fields",
					model.ErrImmutableFieldMismatch, key, active, target.Version)
			}
		}
		switchType, rollbackID := model.SwitchTypeVersionSwitch, int64(0)
		if active > 0 && in.GetToVersion() == pointer.PreviousVersion && pointer.LastSwitchID > 0 {
			// 切回上一生效版本 = 撤销那一次切换，审计必须标成 rollback 并引用它。
			switchType, rollbackID = model.SwitchTypeRollback, pointer.LastSwitchID
		}
		fromDigest := ""
		if active > 0 {
			if current, err := l.svcCtx.Definitions.FindOne(ctx, key, active); err == nil {
				fromDigest = current.DefinitionDigest
			}
		}
		switchID, err := l.svcCtx.Switches.Append(ctx, tx, &model.VersionSwitch{
			FeatureKey:       key,
			SwitchType:       switchType,
			FromVersion:      active,
			ToVersion:        target.Version,
			FromDigest:       fromDigest,
			ToDigest:         target.DefinitionDigest,
			Operator:         clip(op, maxOperatorLen),
			Reason:           clip(spec.reason, maxReasonLen),
			RequestID:        spec.requestID,
			TraceID:          traceIDOf(ctx),
			RollbackSwitchID: rollbackID,
		})
		if err != nil {
			return err
		}
		ok, err := l.svcCtx.ActiveVersions.Switch(ctx, tx, key, active, target.Version, switchID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: pointer moved after locking (concurrent switch)",
				model.ErrVersionConflict)
		}
		out.activeVersion, out.switchID, out.switched = target.Version, switchID, true
		return nil
	})
}

// replay 回放首次切换的结果：指针是真值，回放按快照里的 switch_id/版本返回。
func (l *SwitchFeatureVersionLogic) replay(receipt *model.WriteReceipt) (*rpc.SwitchFeatureVersionReply, error) {
	var snap switchSnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: switch receipt unreadable: %w", err)
	}
	return &rpc.SwitchFeatureVersionReply{
		Switched:      snap.Switched,
		Reused:        true,
		ActiveVersion: snap.ActiveVersion,
		SwitchId:      snap.SwitchID,
	}, nil
}

// boolToRows 把「是否真的切了」映射成回执的 affected_rows（1=改了一行指针）。
func boolToRows(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
