package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveRolloutRuleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveRolloutRuleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveRolloutRuleLogic {
	return &SaveRolloutRuleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新建/更新灰度规则（按 config_id + version + name upsert）
//
// 「写规则」与「开闸」是两步：新建规则一律 state=OFF（model 侧兜底），
// 生效必须再调 SetRolloutRuleState。这条约束的意义在于——一次误填的百分比
// 不会因为保存动作本身就流到线上。
func (l *SaveRolloutRuleLogic) SaveRolloutRule(in *rpc.SaveRolloutRuleReq) (*rpc.SaveRolloutRuleReply, error) {
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
	if in.GetVersion() <= 0 {
		return nil, model.ErrVersionNotFound
	}
	spec := in.GetRule()
	if spec == nil || strings.TrimSpace(spec.GetName()) == "" {
		return nil, model.ErrRuleNameRequired
	}
	// 契约里本请求没有独立的 reason 字段，remark 是唯一的说明位；
	// 它同时充当审计理由（缺口已写进报告）。
	if err := checkReason(spec.GetRemark(), lim); err != nil {
		return nil, err
	}
	m := &l.svcCtx.Models

	item, err := m.ConfigItem.FindOne(l.ctx, strings.TrimSpace(in.GetCfgKey()), scope)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fmt.Errorf("%w: cfg_key=%s scope=%s", model.ErrConfigNotFound, in.GetCfgKey(), scope)
	}

	// 规则只能挂在已发布的版本上：指向不存在的版本等于把不存在的值推给线上。
	ver, err := m.ConfigVersion.FindOne(l.ctx, item.ConfigID, in.GetVersion())
	if err != nil {
		return nil, err
	}
	if ver == nil {
		return nil, fmt.Errorf("%w: cfg_key=%s version=%d", model.ErrVersionNotFound, item.CfgKey, in.GetVersion())
	}

	rule, err := ruleFromSpec(spec, item.ConfigID, in.GetVersion(), in.GetCtx().GetOperatorId(), lim)
	if err != nil {
		return nil, err
	}
	ts := model.NowUnix()
	rule.Ctime = ts

	// 幂等依据是本行的自然键 (config_id, version, name)：同一份入参重放只会覆盖同一行，
	// 因此不需要额外的 request_id 去重表（AGENTS.md §7）；audit 侧仍按 event_id 去重。
	ruleID, created, err := m.RolloutRule.Upsert(l.ctx, rule)
	if err != nil {
		// Upsert 内部跑 ValidateRuleShape：mode 与已填维度不自洽、区间倒挂、时间窗非法都在这里拒。
		// 写侧与读侧共用 model/rollout.go 一套纯函数，展示口径与放量口径因此不会分叉。
		return nil, err
	}
	stored, ferr := m.RolloutRule.FindByID(l.ctx, ruleID)
	if ferr != nil {
		return nil, ferr
	}
	if stored == nil {
		return nil, fmt.Errorf("%w: upsert 后读不到 rule_id=%d", model.ErrRuleNotFound, ruleID)
	}

	// 规则不缓存，但 epoch 必须换代：端上/网关按 epoch 判断「我手里的解析结果属于哪一代」，
	// 不改 epoch 的灰度变更要等各自 TTL 才收敛。
	if _, berr := m.ConfigItem.BumpEpoch(l.ctx, []int64{item.ConfigID}, ts); berr != nil {
		l.Errorf("ops-config/rollout: config_id=%d 的 epoch 换代失败（灰度已保存，收敛延迟一个 TTL）: %v", item.ConfigID, berr)
	}
	cacheDel(l.ctx, l.svcCtx.Cache, lim.configKeysOf(item), l.Logger)

	verb := "update"
	if created {
		verb = "create_rule"
	}
	digest := fmt.Sprintf("mode=%d,percentage=%d,platforms=%q,suffixes=%q,whitelist=%d,priority=%d,window=%d/%d,state=%d",
		rule.Mode, rule.Percentage, rule.Platforms, rule.MidSuffixes,
		len(model.ParseIDList(rule.WhitelistMids)), rule.Priority, rule.StartAt, rule.EndAt, stored.State)
	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), verb,
		"ops_config:rollout_rule", strconv.FormatInt(ruleID, 10),
		"", digest, spec.GetRemark(), ts)

	return &rpc.SaveRolloutRuleReply{Rule: rolloutRuleInfo(stored), AuditEntryId: entryID}, nil
}
