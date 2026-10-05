package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpsertRevenueRuleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertRevenueRuleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertRevenueRuleLogic {
	return &UpsertRevenueRuleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营面：新增/修改规则草稿
//
// 语义要点（详见 rulereplay.go 的注释）：
//   - 本方法只产生/修改 DRAFT，永不改 ACTIVE：改 ACTIVE 单价等于事后改写别人已算出的钱；
//   - 主表写与 cr_rule_change_log 同事务，二者要么都在要么都不在；
//   - 幂等由 cr_rule_change_log.uniq_request_id 兜住：同一 request_id 重放回首次结果，
//     复用到别的规则（或其后已有新变更）回 ErrRequestIDConflict，绝不静默二次改价。
func (l *UpsertRevenueRuleLogic) UpsertRevenueRule(in *rpc.UpsertRevenueRuleReq) (*rpc.RevenueRuleInfo, error) {
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	d, err := validateRuleUpsert(l.svcCtx.Config, in)
	if err != nil {
		return nil, err
	}

	var ruleID int64
	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		id, _, err := applyRuleDraftInTx(ctx, tx, d)
		ruleID = id
		return err
	})
	if err != nil {
		replay, rerr := l.handleRuleWriteErr(err, d)
		if rerr != nil {
			return nil, rerr
		}
		return replay, nil
	}

	row, err := l.svcCtx.Rules.FindOne(l.ctx, ruleID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		// 事务已提交却读不到自己刚写的行：只可能是主从延迟/库被清，
		// 回错误而不是回一个零值规则冒充成功。
		return nil, fmt.Errorf("%w: rule_id=%d 提交后读不到", model.ErrRuleNotFound, ruleID)
	}
	return ruleInfo(row), nil
}

// handleRuleWriteErr 在写事务失败后判定「幂等重放」还是「真失败」。
// 事务此时已回滚，下面全部是只读操作，不会再改任何数据。
func (l *UpsertRevenueRuleLogic) handleRuleWriteErr(
	err error, d *ruleDraft,
) (*rpc.RevenueRuleInfo, error) {
	takenByCode := errors.Is(err, errRuleCodeTaken)
	if !takenByCode && !model.IsDuplicateErr(err) {
		l.Errorf("upsert revenue rule rule_code=%s: %v", d.RuleCode, err)
		return nil, err
	}

	info, rerr := resolveRuleReplay(l.ctx, l.svcCtx.Rules, l.svcCtx.RuleChanges, d.RuleCode, d.RequestId)
	if rerr == nil {
		return info, nil
	}
	if takenByCode {
		// request_id 对不上任何本规则的变更：rule_code 被别人占了。
		return nil, fmt.Errorf("%w: rule_code=%s 已存在；改价请带 rule_id 与 expected_version 更新草稿（%v）",
			model.ErrRuleCodeConflict, d.RuleCode, rerr)
	}
	return nil, rerr
}
