package logic

import (
	"context"
	"errors"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type AdjustBalanceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdjustBalanceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdjustBalanceLogic {
	return &AdjustBalanceLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdjustBalance 运营调整余额（正负皆可，只留台账、不留单据）。
//
// 与充值的区别是审计口径不同：充值必须有充值单可查（OpenRecharge → Settle），
// 调整只有流水与理由，两者不能混成一条路径。
//
// 判定口径：
//   - delta_minor = 0 拒绝（无意义流水会污染对账）；绝对值不得超过 Payment.MaxAdjustMinor；
//   - operator、reason、request_id 必填；
//   - 结果余额不得为负：负向调整走条件扣减（WHERE balance_minor >= ?），
//     0 行即余额不足 FailedPrecondition，且此时不写任何流水；
//   - 「改余额 + 写 ADMIN_ADJUST 流水」同一事务，任何一步失败整体回滚；
//   - 幂等：pm_flow.uniq_request_id 命中时返回原流水并置 duplicated=true；
//     同一 request_id 落在别的业务类型上（说明号被复用）返回冲突错误，不重复调整。
func (l *AdjustBalanceLogic) AdjustBalance(in *rpc.AdjustBalanceReq) (*rpc.AdjustBalanceReply, error) {
	cfg := l.svcCtx.Config.Payment
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.DeltaMinor == 0 {
		return nil, model.ErrAdjustDeltaZero
	}
	if !cfg.AdjustAmountAllowed(in.DeltaMinor) {
		return nil, model.ErrAdjustAmountOutOfRange
	}
	currency, err := resolveCurrency(cfg, in.Currency)
	if err != nil {
		return nil, err
	}
	if err := requireRequestID(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}

	m := l.svcCtx.Models
	// 幂等重放：先按 request_id 找已有流水。
	existing, err := m.Flow.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("payment/AdjustBalance: replay lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if existing != nil {
		if existing.BizType != model.FlowBizAdminAdjust || existing.Mid != in.Mid {
			// 请求号被别的业务动作用掉了：不能把它当本次调整的重放。
			return nil, model.ErrRequestIDReused
		}
		return l.reply(existing.Mid, currency, existing.FlowId, true)
	}

	adjustNo, err := newDocumentNo(prefixAdjust)
	if err != nil {
		return nil, err
	}
	var flowID int64
	var dupKeyErr bool
	err = m.Tx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		// 条件更新保证结果余额非负；余额不足时这里报错，流水不会落库。
		wallet, err := m.Wallet.ApplyDeltaTx(ctx, session, in.Mid, currency, in.DeltaMinor)
		if err != nil {
			return err
		}
		flowID, err = m.Flow.InsertTx(ctx, session, &model.Flow{
			Mid:               in.Mid,
			BizType:           model.FlowBizAdminAdjust,
			BizNo:             adjustNo,
			DeltaMinor:        in.DeltaMinor,
			BalanceAfterMinor: wallet.BalanceMinor,
			Currency:          currency,
			Remark:            in.Reason,
			Operator:          in.Operator,
			RequestId:         in.RequestId,
		})
		return err
	})
	if err != nil {
		switch {
		case m.Flow.IsDuplicate(err):
			dupKeyErr = true
		case errors.Is(err, model.ErrInsufficientBalance), errors.Is(err, model.ErrUnsupportedCurrency):
			l.Infof("payment/AdjustBalance: rejected mid=%d delta_minor=%d reason=%s", in.Mid, in.DeltaMinor, err.Error())
			return nil, err
		default:
			l.Errorf("payment/AdjustBalance: adjust failed mid=%d delta_minor=%d request_id=%s err=%v",
				in.Mid, in.DeltaMinor, in.RequestId, err)
			return nil, err
		}
	}
	if dupKeyErr {
		// 事务已回滚：回读确认是不是同一请求号的重放，查不到就说明号被复用。
		concurrent, lookupErr := m.Flow.FindByRequestID(l.ctx, in.RequestId)
		if lookupErr == nil && concurrent != nil &&
			concurrent.BizType == model.FlowBizAdminAdjust && concurrent.Mid == in.Mid {
			return l.reply(in.Mid, currency, concurrent.FlowId, true)
		}
		l.Errorf("payment/AdjustBalance: request_id reused mid=%d request_id=%s", in.Mid, in.RequestId)
		return nil, model.ErrRequestIDReused
	}
	return l.reply(in.Mid, currency, flowID, false)
}

// reply 回读余额快照组装响应；流水缺失（不应该发生）时报错而不是返回 0 成功。
func (l *AdjustBalanceLogic) reply(mid int64, currency string, flowID int64, duplicated bool) (*rpc.AdjustBalanceReply, error) {
	wallet, err := l.svcCtx.Models.Wallet.FindOne(l.ctx, mid)
	if err != nil {
		l.Errorf("payment/AdjustBalance: wallet snapshot mid=%d err=%v", mid, err)
		return nil, err
	}
	if flowID == 0 {
		l.Errorf("payment/AdjustBalance: flow id missing after commit mid=%d", mid)
		return nil, model.ErrConcurrentUpdate
	}
	return &rpc.AdjustBalanceReply{
		Duplicated: duplicated,
		Wallet:     walletInfo(wallet, mid, currency),
		FlowId:     flowID,
	}, nil
}
