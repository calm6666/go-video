package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SettleSandboxRechargeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSettleSandboxRechargeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SettleSandboxRechargeLogic {
	return &SettleSandboxRechargeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// SettleSandboxRecharge 沙箱结算：把 PENDING 充值单置 SUCCESS 并入账。
//
// 真实渠道下这一步由渠道异步回调驱动；本项目没有回调，所以这一步就是「钱到账」
// 的唯一入口——它只动本地台账，不请求任何第三方支付，不产生真实资金移动。
//
// 判定口径：
//   - 只对 PENDING 生效：状态推进用 CAS（WHERE state = PENDING），0 行即并发冲突，
//     回滚后回读：已 SUCCESS 按重放返回，其他状态报非法迁移；
//   - SUCCESS 单重复调用 → duplicated=true，返回原单据、当前余额与原流水，不重复入账；
//   - CANCELLED/FAILED 单不能结算（钱从未进账也无从作废）→ FailedPrecondition；
//   - 「改单据 + 加余额 + 写流水」同一事务：任何一步失败整体回滚，
//     不允许出现已入账无流水或已加余额无单据；
//   - 流水 (biz_type=RECHARGE, biz_no=recharge_no) 与 request_id 都有唯一索引，
//     是重复入账的第二道防线：即使单据判定被绕过，落账也只能有一次。
func (l *SettleSandboxRechargeLogic) SettleSandboxRecharge(in *rpc.SettleSandboxRechargeReq) (*rpc.SettleSandboxRechargeReply, error) {
	cfg := l.svcCtx.Config.Payment
	if strings.TrimSpace(in.RechargeNo) == "" {
		return nil, model.ErrRechargeNoRequired
	}
	if err := requireRequestID(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := requireMaxLength("reason", in.Reason, maxTextRunes); err != nil {
		return nil, err
	}
	// 沙箱渠道被运营关掉时，账不能再进——这是渠道门禁，不是可选优化。
	if !cfg.AllowsChannel(channelName(rpc.PayChannel_PAY_CHANNEL_SANDBOX)) {
		return nil, model.ErrChannelNotConfigured
	}

	m := l.svcCtx.Models
	recharge, err := m.Recharge.FindOne(l.ctx, in.RechargeNo)
	if err != nil {
		l.Errorf("payment/SettleSandboxRecharge: lookup recharge_no=%s err=%v", in.RechargeNo, err)
		return nil, err
	}
	if recharge == nil {
		return nil, model.ErrRechargeNotFound
	}
	if recharge.State == model.RechargeStateSuccess {
		return l.replay(recharge)
	}
	if recharge.State != model.RechargeStatePending {
		return nil, model.ErrDetail(model.ErrInvalidStateTransition,
			"recharge_state="+rpc.RechargeState(recharge.State).String())
	}

	var dupKeyErr bool
	err = m.Tx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		ok, err := m.Recharge.SettleTx(ctx, session, recharge.RechargeNo, in.Operator, in.Reason, 0)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		wallet, err := m.Wallet.ApplyDeltaTx(ctx, session, recharge.Mid, recharge.Currency, recharge.AmountMinor)
		if err != nil {
			return err
		}
		if _, err := m.Flow.InsertTx(ctx, session, &model.Flow{
			Mid:               recharge.Mid,
			BizType:           model.FlowBizRecharge,
			BizNo:             recharge.RechargeNo,
			DeltaMinor:        recharge.AmountMinor,
			BalanceAfterMinor: wallet.BalanceMinor,
			Currency:          recharge.Currency,
			Remark:            "sandbox recharge settled",
			Operator:          in.Operator,
			RequestId:         in.RequestId,
		}); err != nil {
			if m.Flow.IsDuplicate(err) {
				dupKeyErr = true
			}
			return err
		}
		return nil
	})
	if err != nil {
		switch {
		case dupKeyErr:
			// 事务已整体回滚：该 request_id 已在别的台账行上用掉，必须让调用方换号重试。
			l.Errorf("payment/SettleSandboxRecharge: request_id reused recharge_no=%s request_id=%s", in.RechargeNo, in.RequestId)
			return nil, model.ErrRequestIDReused
		case errors.Is(err, model.ErrConcurrentUpdate):
			fresh, lookupErr := m.Recharge.FindOne(l.ctx, in.RechargeNo)
			if lookupErr != nil {
				l.Errorf("payment/SettleSandboxRecharge: re-read after CAS miss recharge_no=%s err=%v", in.RechargeNo, lookupErr)
				return nil, lookupErr
			}
			if fresh != nil && fresh.State == model.RechargeStateSuccess {
				return l.replay(fresh)
			}
			return nil, model.ErrConcurrentUpdate
		default:
			l.Errorf("payment/SettleSandboxRecharge: settle failed recharge_no=%s err=%v", in.RechargeNo, err)
			return nil, err
		}
	}

	fresh, err := m.Recharge.FindOne(l.ctx, in.RechargeNo)
	if err != nil {
		l.Errorf("payment/SettleSandboxRecharge: re-read settled recharge_no=%s err=%v", in.RechargeNo, err)
		return nil, err
	}
	if fresh == nil {
		// 事务已提交却读不到单据：台账不一致，报错而不是返回空成功。
		l.Errorf("payment/SettleSandboxRecharge: recharge row missing after commit recharge_no=%s", in.RechargeNo)
		return nil, model.ErrRechargeNotFound
	}
	return l.reply(fresh, false)
}

// replay 已入账单据的幂等返回路径。
func (l *SettleSandboxRechargeLogic) replay(recharge *model.Recharge) (*rpc.SettleSandboxRechargeReply, error) {
	return l.reply(recharge, true)
}

// reply 组装结算结果：单据 + 余额快照 + 本次入账流水号（全部回读，不回显未提交数据）。
func (l *SettleSandboxRechargeLogic) reply(recharge *model.Recharge, duplicated bool) (*rpc.SettleSandboxRechargeReply, error) {
	m := l.svcCtx.Models
	wallet, err := m.Wallet.FindOne(l.ctx, recharge.Mid)
	if err != nil {
		l.Errorf("payment/SettleSandboxRecharge: wallet snapshot mid=%d err=%v", recharge.Mid, err)
		return nil, err
	}
	flow, err := m.Flow.FindByBizNo(l.ctx, model.FlowBizRecharge, recharge.RechargeNo)
	if err != nil {
		l.Errorf("payment/SettleSandboxRecharge: flow lookup recharge_no=%s err=%v", recharge.RechargeNo, err)
		return nil, err
	}
	var flowID int64
	if flow != nil {
		flowID = flow.FlowId
	}
	return &rpc.SettleSandboxRechargeReply{
		Duplicated: duplicated,
		Recharge:   rechargeInfo(recharge),
		Wallet:     walletInfo(wallet, recharge.Mid, recharge.Currency),
		FlowId:     flowID,
	}, nil
}
