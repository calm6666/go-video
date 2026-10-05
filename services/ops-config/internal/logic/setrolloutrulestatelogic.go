package logic

import (
	"context"
	"fmt"
	"strconv"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetRolloutRuleStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetRolloutRuleStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetRolloutRuleStateLogic {
	return &SetRolloutRuleStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 开/关灰度规则（只改 state 一列）
//
// 本方法刻意不碰 priority/percentage/platforms：那是 SaveRolloutRule 的覆盖语义。
// 混在一起会让「关闸」这个止血动作附带改写规则的风险，而止血时最需要的是可预期。
func (l *SetRolloutRuleStateLogic) SetRolloutRuleState(in *rpc.SetRolloutRuleStateReq) (*rpc.SetRolloutRuleStateReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	// 停闸是唯一能瞬间改变全端曝光面的手工动作，没理由就不能按。
	if err := checkReason(in.GetReason(), lim); err != nil {
		return nil, err
	}
	if err := checkState(in.GetState()); err != nil {
		return nil, err
	}
	if in.GetRuleId() <= 0 {
		return nil, model.ErrRuleNotFound
	}
	m := &l.svcCtx.Models

	cur, err := m.RolloutRule.FindByID(l.ctx, in.GetRuleId())
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("%w: rule_id=%d", model.ErrRuleNotFound, in.GetRuleId())
	}
	if cur.State == in.GetState() {
		// 重复点击要报错，而不是回一个「成功」：否则审计里会出现两条「看起来改了东西」的记录，
		// 事后无法分辨哪一次是真的止血动作。
		return nil, fmt.Errorf("%w: rule_id=%d state=%d", model.ErrRuleStateUnchanged, cur.RuleID, cur.State)
	}

	ts := model.NowUnix()
	ok, err := m.RolloutRule.SetState(l.ctx, cur.RuleID, in.GetState(), cur.State, in.GetCtx().GetOperatorId(), ts)
	if err != nil {
		return nil, err
	}
	if !ok {
		// expectState 就是并发基线（语义同乐观锁）：没命中说明别人先改了，让调用方重读再决定。
		return nil, fmt.Errorf("%w: rule_id=%d expect_state=%d", model.ErrVersionConflict, cur.RuleID, cur.State)
	}

	stored, ferr := m.RolloutRule.FindByID(l.ctx, cur.RuleID)
	if ferr != nil {
		return nil, ferr
	}
	if stored == nil {
		return nil, model.ErrRuleNotFound
	}

	// 先 bump epoch 再删读缓存：顺序反了会出现「键删了、旧代次又被回填」的空窗。
	if _, berr := m.ConfigItem.BumpEpoch(l.ctx, []int64{cur.ConfigID}, ts); berr != nil {
		l.Errorf("ops-config/rollout: 关闸后 config_id=%d 的 epoch 换代失败，旧放量可能多命中一个 TTL: %v", cur.ConfigID, berr)
	}
	if item, ierr := m.ConfigItem.FindByID(l.ctx, cur.ConfigID); ierr == nil {
		cacheDel(l.ctx, l.svcCtx.Cache, lim.configKeysOf(item), l.Logger)
	} else {
		l.Errorf("ops-config/rollout: 状态已改，但读缓存未能删除（config_id=%d 读不到）: %v", cur.ConfigID, ierr)
	}

	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "set_rollout_state",
		"ops_config:rollout_rule", strconv.FormatInt(cur.RuleID, 10),
		fmt.Sprintf("state=%d,version=%d", cur.State, cur.Version),
		fmt.Sprintf("state=%d,version=%d", stored.State, stored.Version),
		in.GetReason(), ts)

	return &rpc.SetRolloutRuleStateReply{Rule: rolloutRuleInfo(stored), AuditEntryId: entryID}, nil
}
