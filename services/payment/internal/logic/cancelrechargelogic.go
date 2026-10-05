package logic

import (
	"context"
	"strings"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CancelRechargeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelRechargeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelRechargeLogic {
	return &CancelRechargeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CancelRecharge 取消未结算充值单。
//
// 判定口径：
//   - reason 必填（台账要知道这单为什么作废），operator 必填；
//   - 只有 PENDING 可取消：钱从未进账，取消不动余额、不写流水；
//   - 已 SUCCESS 的单不能靠取消回滚——那会把「已入账的钱」凭空抹掉，
//     必须走退款/运营调整路径，错误消息里把这条路写清楚；
//   - 已 CANCELLED 重复取消 → duplicated=true（取消天然幂等）；
//   - 状态推进用条件更新（WHERE state = PENDING）完成，不查后改。
func (l *CancelRechargeLogic) CancelRecharge(in *rpc.CancelRechargeReq) (*rpc.CancelRechargeReply, error) {
	if strings.TrimSpace(in.RechargeNo) == "" {
		return nil, model.ErrRechargeNoRequired
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
	recharge, err := m.Recharge.FindOne(l.ctx, in.RechargeNo)
	if err != nil {
		l.Errorf("payment/CancelRecharge: lookup recharge_no=%s err=%v", in.RechargeNo, err)
		return nil, err
	}
	if recharge == nil {
		return nil, model.ErrRechargeNotFound
	}

	switch recharge.State {
	case model.RechargeStateCancelled:
		return &rpc.CancelRechargeReply{Duplicated: true, Recharge: rechargeInfo(recharge)}, nil
	case model.RechargeStateSuccess:
		return nil, model.ErrRechargeAlreadySettled
	case model.RechargeStatePending:
		// 走下面的条件更新。
	default:
		return nil, model.ErrDetail(model.ErrRechargeNotPending,
			"recharge_state="+rpc.RechargeState(recharge.State).String())
	}

	ok, err := m.Recharge.CancelTx(l.ctx, nil, in.RechargeNo, in.Operator, in.Reason)
	if err != nil {
		l.Errorf("payment/CancelRecharge: cancel recharge_no=%s err=%v", in.RechargeNo, err)
		return nil, err
	}
	fresh, err := m.Recharge.FindOne(l.ctx, in.RechargeNo)
	if err != nil {
		l.Errorf("payment/CancelRecharge: re-read recharge_no=%s err=%v", in.RechargeNo, err)
		return nil, err
	}
	if fresh == nil {
		return nil, model.ErrRechargeNotFound
	}
	if !ok {
		// 条件未命中：并发已经推进过这张单，按当前真实状态给结论，不假装取消成功。
		switch fresh.State {
		case model.RechargeStateCancelled:
			return &rpc.CancelRechargeReply{Duplicated: true, Recharge: rechargeInfo(fresh)}, nil
		case model.RechargeStateSuccess:
			return nil, model.ErrRechargeAlreadySettled
		default:
			return nil, model.ErrDetail(model.ErrConcurrentUpdate,
				"recharge_state="+rpc.RechargeState(fresh.State).String())
		}
	}
	return &rpc.CancelRechargeReply{Recharge: rechargeInfo(fresh)}, nil
}
