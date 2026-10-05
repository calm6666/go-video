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

type PublishConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPublishConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PublishConfigLogic {
	return &PublishConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 发布新版本（乐观锁 + 幂等 + 可选一并挂灰度规则）
//
// 版本指针语义（本服务的地基，务必读）：
//   - ops_config_version 行**不可变**：一次发布 = 新增一行快照，历史永不改写；
//   - ops_config_item.latest_version 是「当前生效指针」，只能由本函数（以及回滚、
//     灰度收口）通过带 expect_latest_version 条件的 UPDATE 单向推进；
//   - 因此「发布」与「回滚」是两个动作但共用同一套写入骨架：都是追加一个新版本，
//     指针只会变大，不会倒退。
func (l *PublishConfigLogic) PublishConfig(in *rpc.PublishConfigReq) (*rpc.PublishConfigReply, error) {
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
	// 负的 expect_version 不是「未填」，是把乐观锁写反了；放过去等于放弃乐观锁。
	if in.GetExpectVersion() < 0 {
		return nil, model.ErrVersionConflict
	}
	m := &l.svcCtx.Models

	// 1) 幂等回放：request_id 上有唯一键，同一次请求重放绝不产生第二个版本。
	if existed, err := m.ConfigVersion.FindByRequestID(l.ctx, in.GetCtx().GetRequestId()); err != nil {
		return nil, err
	} else if existed != nil {
		return l.replay(m, existed, lim)
	}

	cfgKey := strings.TrimSpace(in.GetCfgKey())
	valueType := int32(in.GetValueType())

	// 2) 取配置项，决定「首发新建」还是「在已有键上发布下一版」。
	//    值的形态校验必须在 valueType 定下来之后做：请求省略 value_type 时类型要
	//    从库里继承（proto:「更新时 0 表示沿用」），提前按请求里的 0 去校验
	//    会把「沿用」判成 unsupported value type；请求改类型时也要先报
	//    ErrValueTypeImmutable（类型是解释这段值的契约，比值的形状更根本）。
	item, err := m.ConfigItem.FindOne(l.ctx, cfgKey, scope)
	if err != nil {
		return nil, err
	}
	creating := item == nil
	if creating {
		// 新建必须显式声明值类型：契约里没有 title/description 之外的兜底类型来源，
		// 猜一个类型会让后续所有版本的 value_type 判定都从错误基线出发。
		if in.GetExpectVersion() != 0 {
			return nil, model.ErrConfigNotFound
		}
		if valueType == 0 {
			return nil, model.ErrValueTypeUnsupported
		}
	} else {
		if in.GetExpectVersion() == 0 && item.LatestVersion > 0 {
			// 期望「新建」却发现键已存在：这是调用方状态过期，不是可以顺手覆盖的事。
			return nil, model.ErrConfigExists
		}
		if in.GetExpectVersion() != item.LatestVersion {
			return nil, fmt.Errorf("%w: cfg_key=%s scope=%s expect=%d latest=%d",
				model.ErrVersionConflict, cfgKey, scope, in.GetExpectVersion(), item.LatestVersion)
		}
		if valueType == 0 {
			valueType = item.ValueType
		} else if valueType != item.ValueType {
			return nil, model.ErrValueTypeImmutable
		}
	}
	if err := validateValueText(valueType, in.GetValue(), lim); err != nil {
		return nil, err
	}

	// 3) 灰度规则先做形态预检：宁可整次请求被拒，也不要「版本写进去了、规则写不进去」
	//    这种半条命令（事务里也会再判一次，但预检能把错误定位到具体第几条规则）。
	specs := in.GetRollout()
	rules := make([]*model.RolloutRule, 0, len(specs))
	for i, spec := range specs {
		rule, err := ruleFromSpec(spec, 0, 0, in.GetCtx().GetOperatorId(), lim)
		if err != nil {
			return nil, fmt.Errorf("rollout[%d]: %w", i, err)
		}
		// 形态自洽性（mode 与已填维度是否吻合）在这里就判掉：ruleFromSpec 只做归一化，
		// 少了这一步，非法规则要等到事务里 UpsertTx 才被发现，错误既没有 rollout[i]
		// 下标、又白跑了一次版本行写入。ValidateRuleShape 先判 version<=0，
		// 而版本号要到 §5 才知道，因此拿一份副本填上占位版本只判形态。
		shapeProbe := *rule
		shapeProbe.Version = 1
		if serr := model.ValidateRuleShape(&shapeProbe, lim.whitelistMax); serr != nil {
			return nil, fmt.Errorf("rollout[%d]: %w", i, serr)
		}
		rules = append(rules, rule)
	}

	ts := model.NowUnix()

	// 4) 首次发布：先落主记录。Insert 走「空更新 + RowsAffected==0」判唯一键冲突，
	//    冲突即 ErrConfigExists —— 绝不去接管别人刚建好的键（那等于覆盖别人的指针）。
	if creating {
		configID, ierr := m.ConfigItem.Insert(l.ctx, &model.ConfigItem{
			CfgKey:        cfgKey,
			Scope:         scope,
			ValueType:     valueType,
			State:         model.StateOn,
			LatestVersion: 0,
			OperatorID:    in.GetCtx().GetOperatorId(),
			Ctime:         ts,
		})
		if ierr != nil {
			if errors.Is(ierr, model.ErrConfigExists) {
				// 并发首发：两边都以为自己是第一个。这里必须失败，让调用方带新 expect_version 重试。
				return nil, model.ErrConfigExists
			}
			return nil, ierr
		}
		item, err = m.ConfigItem.FindByID(l.ctx, configID)
		if err != nil {
			return nil, err
		}
		if item == nil {
			return nil, model.ErrConfigNotFound
		}
	}

	// 5) 新版本号 = MAX(version)+1，不是 count+1（并发下 count 会撞 uniq_config_version）。
	newVersion, err := m.ConfigVersion.MaxVersion(l.ctx, item.ConfigID)
	if err != nil {
		return nil, err
	}
	newVersion++
	for _, r := range rules {
		r.ConfigID = item.ConfigID
		r.Version = newVersion
	}

	row := &model.ConfigVersion{
		ConfigID:     item.ConfigID,
		Version:      newVersion,
		Value:        in.GetValue(),
		ValueType:    valueType,
		ChangeType:   l.changeTypeOf(creating),
		OperatorID:   in.GetCtx().GetOperatorId(),
		OperatorName: in.GetCtx().GetOperatorName(),
		Reason:       in.GetReason(),
		RequestID:    in.GetCtx().GetRequestId(),
		PublishedAt:  ts,
		Ctime:        ts,
	}

	// 6) 挂了灰度规则的发布 = 「放量中」，指针不能推进：端上此刻仍读旧版本，
	//    新只有规则命中的人读得到。收口由 SetRolloutRuleState 或下一次全量发布完成。
	rolloutOnly := len(rules) > 0

	err = l.svcCtx.Conn.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		if _, e := m.ConfigVersion.InsertTx(ctx, session, row); e != nil {
			return e
		}
		for _, r := range rules {
			if _, _, e := m.RolloutRule.UpsertTx(ctx, session, r); e != nil {
				return e
			}
		}
		if rolloutOnly {
			return nil
		}
		ok, e := m.ConfigItem.AdvanceReleaseTx(ctx, session, item.ConfigID, item.LatestVersion, newVersion,
			in.GetCtx().GetOperatorId(), ts)
		if e != nil {
			return e
		}
		if !ok {
			// 条件更新没命中 = 指针已被别人推进：整事务回滚，版本快照也不留下，
			// 否则会留下一条「存在但从未生效」的幽灵版本。
			return model.ErrVersionConflict
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, model.ErrVersionExists) {
			// 并发下 MAX(version)+1 撞了 uniq_config_version：转成可重试的乐观锁冲突，
			// 但原始原因必须留在错误链里（排障要能看出是撞了唯一键而不是 expect 写错）。
			return nil, fmt.Errorf("%w: %w", model.ErrVersionConflict, err)
		}
		return nil, err
	}

	updated, err := m.ConfigItem.FindByID(l.ctx, item.ConfigID)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, model.ErrConfigNotFound
	}

	// 7) 投影失效：指针键必须删（值快照不可变、无需失效；规则不缓存）。
	cacheDel(l.ctx, l.svcCtx.Cache, lim.configKeysOf(updated), l.Logger)

	// 8) 审计存证：event_id 含 request_id ⇒ 重放不产生第二条。失败只留缺口，不回滚发布。
	before := "latest_version=" + strconv.FormatInt(item.LatestVersion, 10)
	after := fmt.Sprintf("version=%d,value_bytes=%d,value_type=%d,rollout=%d,pointer_advanced=%t",
		newVersion, len(in.GetValue()), valueType, len(rules), !rolloutOnly)
	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "publish",
		"ops_config:release", strconv.FormatInt(item.ConfigID, 10)+"/"+strconv.FormatInt(newVersion, 10),
		before, after, in.GetReason(), ts)
	if entryID > 0 {
		if ok, serr := m.ConfigVersion.SetAuditEntry(l.ctx, row.VersionID, entryID); serr != nil {
			l.Errorf("ops-config/audit: 回填 version_id=%d 的 audit_entry_id 失败，待补偿: %v", row.VersionID, serr)
		} else if !ok {
			l.Errorf("ops-config/audit: 回填 version_id=%d 未命中（已发布成功，仅存证待补）", row.VersionID)
		}
	}

	rulesAfter, _, err := m.RolloutRule.List(l.ctx, model.RolloutRuleFilter{
		ConfigID: item.ConfigID, Version: newVersion, Pn: 1, Ps: int32(lim.rulesPerConfig),
	})
	if err != nil {
		return nil, err
	}

	return &rpc.PublishConfigReply{
		Item:         configItemInfo(updated),
		Version:      configVersionInfo(row),
		Rules:        rolloutRuleList(rulesAfter),
		AuditEntryId: entryID,
		Reused:       false,
	}, nil
}

// replay 处理「同 request_id 再进来一次」：回第一次的既成事实，不写任何东西。
//
// 但「写了版本行」与「发布成功」并不等价：带灰度规则的发布刻意不推进指针。
// 因此这里要分辨三件事：
//   - 指针已指向该版本 ⇒ 回放成功；
//   - 该版本挂着规则（非全量放量中）⇒ 同样是合法结果，回放成功；
//   - 两者都不满足 ⇒ 上一次执行半途而废，返回 ErrReleaseIncomplete 让人去查，
//     而不是伪装成成功（AGENTS.md §10）。
func (l *PublishConfigLogic) replay(m *svc.Models, existed *model.ConfigVersion, lim limits) (*rpc.PublishConfigReply, error) {
	item, err := m.ConfigItem.FindByID(l.ctx, existed.ConfigID)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, model.ErrConfigNotFound
	}
	if item.LatestVersion != existed.Version {
		ruleCount, cerr := m.RolloutRule.CountByVersion(l.ctx, existed.ConfigID, existed.Version, 0)
		if cerr != nil {
			return nil, cerr
		}
		if ruleCount == 0 {
			return nil, fmt.Errorf("%w: cfg_key=%s version=%d", model.ErrReleaseIncomplete,
				item.CfgKey, existed.Version)
		}
	}
	rules, _, err := m.RolloutRule.List(l.ctx, model.RolloutRuleFilter{
		ConfigID: existed.ConfigID, Version: existed.Version, Pn: 1, Ps: int32(lim.rulesPerConfig),
	})
	if err != nil {
		return nil, err
	}
	return &rpc.PublishConfigReply{
		Item:         configItemInfo(item),
		Version:      configVersionInfo(existed),
		Rules:        rolloutRuleList(rules),
		AuditEntryId: existed.AuditEntryID,
		Reused:       true,
	}, nil
}

func (l *PublishConfigLogic) changeTypeOf(creating bool) string {
	if creating {
		return model.ChangeTypeCreate
	}
	return model.ChangeTypePublish
}
