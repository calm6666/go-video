package logic

import (
	"context"
	"strings"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetRevenueRuleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRevenueRuleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRevenueRuleLogic {
	return &GetRevenueRuleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 规则读取（支持按历史版本复核）。
//
// 判定口径：
//   - 定位方式二选一：rule_id 优先，其次 rule_code；都没有 → ErrRuleTargetRequired；
//   - version == 0 读当前行；version > 0 复核历史版本：
//     主表 rule_code 唯一且就地更新，所以历史「单价 + 状态」只能从 cr_rule_change_log
//     的 from_ 列还原（取 from_version >= version 的首条，即该版本生效期间的取值）。
//     其余字段（名称/说明/门槛/封顶）不入变更台账 —— 因为 cr_metric 每行都锁定了
//     rule_version 与算出的 amount/capped_amount，历史金额的可复核性由台账行本身承担，
//     不需要靠规则表反推；这条口径写进 README，避免误以为能整行回放。
//   - 请求的版本大于当前版本 → found=false（该版本还不存在），不报错也不回现值；
//   - 查询失败上抛错误，绝不回 found=false 冒充「确实没有这版规则」。
func (l *GetRevenueRuleLogic) GetRevenueRule(in *rpc.GetRevenueRuleReq) (*rpc.GetRevenueRuleReply, error) {
	if in == nil {
		in = &rpc.GetRevenueRuleReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}

	var (
		cur *model.RevenueRule
		err error
	)
	switch {
	case in.RuleId > 0:
		cur, err = l.svcCtx.Rules.FindOne(l.ctx, in.RuleId)
	case strings.TrimSpace(in.RuleCode) != "":
		cur, err = l.svcCtx.Rules.FindByCode(l.ctx, strings.TrimSpace(in.RuleCode))
	default:
		return nil, model.ErrRuleTargetRequired
	}
	if err != nil {
		l.Errorf("GetRevenueRule failed, rule_id=%d rule_code=%s: %v", in.RuleId, in.RuleCode, err)
		return nil, err
	}
	if cur == nil {
		return &rpc.GetRevenueRuleReply{Found: false}, nil
	}
	if in.Version == 0 || in.Version == cur.Version {
		return &rpc.GetRevenueRuleReply{Found: true, Rule: ruleInfo(cur)}, nil
	}
	if in.Version > cur.Version || in.Version < 0 {
		// 还不存在的版本：不能拿现值冒充历史值。
		return &rpc.GetRevenueRuleReply{Found: false}, nil
	}

	logRow, err := l.svcCtx.RuleChanges.FirstChangeFrom(l.ctx, cur.RuleCode, in.Version)
	if err != nil {
		l.Errorf("GetRevenueRule history failed, rule_code=%s version=%d: %v", cur.RuleCode, in.Version, err)
		return nil, err
	}
	if logRow == nil {
		// 变更台账缺失（例如数据迁移前的存量行）：宁可报 not found，
		// 也不能把「现单价」当成「历史单价」返回给争议复核。
		l.Errorf("GetRevenueRule: rule_code=%s 无 version<=%d 的变更台账，无法还原历史单价口径", cur.RuleCode, in.Version)
		return &rpc.GetRevenueRuleReply{Found: false}, nil
	}
	replay := *cur // 复制后再覆盖单价/状态/版本，其余字段沿用现值（见函数头说明）
	replay.UnitPricePer1000 = logRow.FromUnitPrice
	replay.State = logRow.FromState
	replay.Version = in.Version
	return &rpc.GetRevenueRuleReply{Found: true, Rule: ruleInfo(&replay)}, nil
}
