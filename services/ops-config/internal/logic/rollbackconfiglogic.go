package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RollbackConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRollbackConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RollbackConfigLogic {
	return &RollbackConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 回滚到历史版本（生成新版本，不改写历史）
//
// 为什么「回滚」不是「把指针往回拨」：线上此刻跑的是哪一次改动，必须能由
// ops_config_item.latest_version 直接读出来，且版本序列单调递增。
// 往回拨指针会让「version=7」同时代表两个不同的值（回拨前与回拨后），
// 审计与端上缓存（键里带版本号）会当场失真。所以回滚 = 再发布一次历史值。
func (l *RollbackConfigLogic) RollbackConfig(in *rpc.RollbackConfigReq) (*rpc.RollbackConfigReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	if err := checkCfgKey(in.GetCfgKey(), lim); err != nil {
		return nil, err
	}
	scope := normalizeScope(in.GetScope())
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	if err := checkReason(in.GetReason(), lim); err != nil {
		return nil, err
	}
	if in.GetToVersion() <= 0 {
		return nil, model.ErrVersionNotFound
	}
	m := &l.svcCtx.Models

	// 幂等回放：同一 request_id 的回滚重放不得产生第二个版本。
	if existed, err := m.ConfigVersion.FindByRequestID(l.ctx, in.GetCtx().GetRequestId()); err != nil {
		return nil, err
	} else if existed != nil {
		return l.replay(m, existed)
	}

	item, err := m.ConfigItem.FindOne(l.ctx, strings.TrimSpace(in.GetCfgKey()), scope)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, model.ErrConfigNotFound
	}
	if in.GetToVersion() == item.LatestVersion {
		// 「回滚到当前生效版本」是空操作。回空操作一个成功，等于让运营以为止血生效了。
		return nil, model.ErrRollbackToLatest
	}

	// 历史快照必须存在。注意：允许回滚到「从未生效过的灰度版本」——
	// 那正是「灰度发现没问题、直接全量」的合法路径，值本身是可用的。
	target, err := m.ConfigVersion.FindOne(l.ctx, item.ConfigID, in.GetToVersion())
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("%w: cfg_key=%s to_version=%d", model.ErrVersionNotFound, item.CfgKey, in.GetToVersion())
	}
	if !model.ValidValueType(target.ValueType) {
		// 历史行的值类型已不可解释（例如库被手工改过）：宁可失败，不能猜类型。
		return nil, model.ErrValueTypeUnsupported
	}

	newVersion, err := m.ConfigVersion.MaxVersion(l.ctx, item.ConfigID)
	if err != nil {
		return nil, err
	}
	newVersion++

	ts := model.NowUnix()
	row := &model.ConfigVersion{
		ConfigID:  item.ConfigID,
		Version:   newVersion,
		Value:     target.Value,
		ValueType: target.ValueType,
		// change_type=rollback + rollback_from 指向被复用的那一版：
		// 「这次改了什么」由此可查，无需 diff 两个版本。
		ChangeType:   model.ChangeTypeRollback,
		RollbackFrom: target.Version,
		OperatorID:   in.GetCtx().GetOperatorId(),
		OperatorName: in.GetCtx().GetOperatorName(),
		Reason:       in.GetReason(),
		RequestID:    in.GetCtx().GetRequestId(),
		PublishedAt:  ts,
		Ctime:        ts,
	}

	// 回滚前先取一份「当前开着的规则」：它们是本次要一并关掉的对象，也是审计 before 摘要的内容。
	openRules, _, err := m.RolloutRule.List(l.ctx, model.RolloutRuleFilter{
		ConfigID: item.ConfigID, State: model.StateOn, Pn: 1, Ps: int32(lim.rulesPerConfig),
	})
	if err != nil {
		return nil, err
	}

	err = l.svcCtx.Conn.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, e := m.ConfigVersion.InsertTx(ctx, session, row); e != nil {
			return e
		}
		// 回滚说明这轮放量出了问题：留着 ON 规则等于把一部分人继续导回坏版本。
		// 只关 state=ON 的行；返回 false 表示别人已经关了 —— 目标状态一致，不算冲突。
		for _, r := range openRules {
			if r.State != model.StateOn {
				continue
			}
			if _, e := m.RolloutRule.SetStateTx(ctx, session, r.RuleID, model.StateOff, model.StateOn,
				in.GetCtx().GetOperatorId(), ts); e != nil {
				return e
			}
		}
		ok, e := m.ConfigItem.AdvanceReleaseTx(ctx, session, item.ConfigID, item.LatestVersion, newVersion,
			in.GetCtx().GetOperatorId(), ts)
		if e != nil {
			return e
		}
		if !ok {
			return model.ErrVersionConflict
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, model.ErrVersionExists) {
			// 与发布同一套规矩：撞唯一键要转成可重试的乐观锁冲突，但原始原因留在链上。
			return nil, fmt.Errorf("%w: %w", model.ErrVersionConflict, err)
		}
		return nil, err
	}

	updated, err := m.ConfigItem.FindByID(l.ctx, item.ConfigID)
	if err != nil {
		return nil, err
	}
	cacheDel(l.ctx, l.svcCtx.Cache, lim.configKeysOf(updated), l.Logger)

	before := fmt.Sprintf("latest_version=%d,open_rules=%d", item.LatestVersion, len(openRules))
	after := fmt.Sprintf("latest_version=%d,rollback_from=%d,rules_closed=%d", newVersion, target.Version, len(openRules))
	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "rollback",
		"ops_config:release", strconv.FormatInt(item.ConfigID, 10)+"/"+strconv.FormatInt(newVersion, 10),
		before, after, in.GetReason(), ts)
	if entryID > 0 {
		if ok, serr := m.ConfigVersion.SetAuditEntry(l.ctx, row.VersionID, entryID); serr != nil {
			l.Errorf("ops-config/audit: 回填 version_id=%d 的 audit_entry_id 失败，待补偿: %v", row.VersionID, serr)
		} else if !ok {
			l.Errorf("ops-config/audit: 回填 version_id=%d 未命中（回滚已完成，仅存证待补）", row.VersionID)
		}
	}

	return &rpc.RollbackConfigReply{
		Item:         configItemInfo(updated),
		Version:      configVersionInfo(row),
		AuditEntryId: entryID,
	}, nil
}

// replay 回放上次回滚的既成事实。
// 与发布回放同样的规矩：版本行存在但指针从未推进 ⇒ 上次执行半途而废，
// 必须 ErrReleaseIncomplete，不能把「写了一半」报成成功。
func (l *RollbackConfigLogic) replay(m *svc.Models, existed *model.ConfigVersion) (*rpc.RollbackConfigReply, error) {
	item, err := m.ConfigItem.FindByID(l.ctx, existed.ConfigID)
	if err != nil {
		return nil, err
	}
	if existed.ChangeType != model.ChangeTypeRollback {
		return nil, fmt.Errorf("%w: request_id=%s 已用于 %s 动作，回滚需换新 request_id",
			model.ErrRequestIDRequired, existed.RequestID, existed.ChangeType)
	}
	if item.LatestVersion != existed.Version {
		return nil, fmt.Errorf("%w: cfg_key=%s version=%d", model.ErrReleaseIncomplete, item.CfgKey, existed.Version)
	}
	return &rpc.RollbackConfigReply{
		Item:         configItemInfo(item),
		Version:      configVersionInfo(existed),
		AuditEntryId: existed.AuditEntryID,
	}, nil
}
