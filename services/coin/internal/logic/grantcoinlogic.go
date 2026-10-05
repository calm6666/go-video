package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type GrantCoinLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGrantCoinLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GrantCoinLogic {
	return &GrantCoinLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// errGrantReplay 事务内发现同 request_id 已被并发写入，由外层翻译成 duplicated=true。
var errGrantReplay = errors.New("coin: grant request replayed by a concurrent transaction")

// 发放/扣回硬币（运营授权或订单履约）—— 除投币外唯一的余额变动入口。
//
// 口径：
//   - flow_type 只接受 ORDER_PACK（trade-order 履约）与 ADMIN_GRANT（运营），
//     TOSS/CANCEL_TOSS 由投币链路自己写，从这儿塞进来就是伪造投币记录；
//     EXPIRE 未开启（本项目没有硬币过期能力），一律拒绝；
//   - operator 恒必填：一条改动余额的台账若看不出是谁发起的，事后无法追责；
//     ADMIN_GRANT 还要 reason，ORDER_PACK 还要 biz_no（对账时要能区分「白送的」和「买来的」）；
//   - |delta| 超 Coin.MaxGrantDelta 直接拒绝，挡住运营少打一个 0；
//   - 负 delta 不得把余额扣成负数：条件更新 UPDATE ... WHERE balance + delta >= 0；
//   - request_id 幂等：重放返回首次结论（flow_id + 当前账户），参数不同报冲突。
//
// 本方法没有「业务结论」通道（GrantCoinReply 无 accepted/reason 字段），
// 所以以上不合法一律以 gRPC 错误上抛，由 gateway 映射成 HTTP 信封 code —— 这是契约决定的，
// 不是实现偷懒（见 README 契约缺口）。
func (l *GrantCoinLogic) GrantCoin(in *rpc.GrantCoinReq) (*rpc.GrantCoinReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.RequestId == "" {
		return nil, model.ErrRequestIDRequired
	}
	if in.Delta == 0 {
		return nil, fmt.Errorf("%w: delta=0 无记账意义", model.ErrGrantDeltaInvalid)
	}
	coin := l.svcCtx.Coin()
	abs := in.Delta
	if abs < 0 {
		abs = -abs
	}
	if abs > coin.MaxGrantDelta {
		return nil, fmt.Errorf("%w: |delta|=%d 超过单次上限 %d", model.ErrGrantDeltaInvalid, abs, coin.MaxGrantDelta)
	}
	if in.FlowType != rpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK &&
		in.FlowType != rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT {
		return nil, model.ErrGrantTypeInvalid
	}
	if in.Operator == "" {
		return nil, model.ErrGrantOperatorRequired
	}
	if in.FlowType == rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT && in.Reason == "" {
		return nil, model.ErrGrantReasonRequired
	}
	if in.FlowType == rpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK && in.BizNo == "" {
		return nil, model.ErrGrantBizNoRequired
	}

	stored, err := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if err := assertSameGrantRequest(in, stored); err != nil {
			return nil, err
		}
		acc, err := l.freshAccount(in.Mid, nil)
		if err != nil {
			return nil, err
		}
		return &rpc.GrantCoinReply{
			Duplicated: true,
			Account:    accountInfo(in.Mid, acc, l.todayTossed(in.Mid), coin),
			FlowId:     stored.ID,
		}, nil
	}

	var (
		flowID  int64
		snap    *model.Account
		inTxAcc *model.Account
	)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		created, err := l.svcCtx.Accounts.EnsureTx(ctx, tx, in.Mid, coin.InitialBalance)
		if err != nil {
			return err
		}
		acc, err := l.svcCtx.Accounts.LockForUpdateTx(ctx, tx, in.Mid)
		if err != nil {
			return err
		}
		if created && coin.InitialBalance > 0 {
			// 建仓送币同样要留流水：GrantCoin 是新账户最常见的诞生场景，
			// 少了这条流水，第一笔发放的 balance_after 就对不上历史。
			if _, err := l.svcCtx.Flows.InsertTx(ctx, tx, initialGrantFlow(in.Mid, coin.InitialBalance)); err != nil {
				return err
			}
			// 注意：EnsureTx 插入的那一行 balance 已经是 InitialBalance，acc 是它读回来的值，
			// 这里不能再加一次 —— 否则本次发放流水的 balance_after 与回显余额都会凭空多出初始币。
		}
		snap = acc
		inTxAcc = acc

		again, err := l.svcCtx.Flows.FindByRequestID(ctx, tx, in.RequestId)
		if err != nil {
			return err
		}
		if again != nil {
			return errGrantReplay
		}

		ok, err := l.svcCtx.Accounts.ApplyGrantTx(ctx, tx, in.Mid, in.Delta)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrGrantBalanceWouldGoNegative
		}
		// 提交后回读失败时的兜底回显必须是「改完之后」的余额：acc 是本次事务改动**前**
		// 读到的行，直接拿它兜底会给运营看一个偏小的数字，比报错更容易被当成「发放没生效」。
		after := *acc
		after.Balance += in.Delta
		snap = &after

		remark := in.Reason
		if remark == "" {
			remark = fmt.Sprintf("%s 履约发放 %d 枚，订单号 %s", in.Operator, in.Delta, in.BizNo)
		}
		id, err := l.svcCtx.Flows.InsertTx(ctx, tx, &model.Flow{
			Mid:          in.Mid,
			FlowType:     int32(in.FlowType),
			Delta:        in.Delta,
			BalanceAfter: acc.Balance + in.Delta,
			BizNo:        clipID(in.BizNo),
			Operator:     clipID(in.Operator),
			RequestID:    in.RequestId,
			Remark:       clipRemark(remark),
		})
		if err != nil {
			return err
		}
		flowID = id
		return nil
	})

	switch {
	case err == nil:
		acc, aerr := l.freshAccount(in.Mid, snap)
		if aerr != nil {
			return nil, aerr
		}
		return &rpc.GrantCoinReply{
			Duplicated: false,
			Account:    accountInfo(in.Mid, acc, l.todayTossed(in.Mid), coin),
			FlowId:     flowID,
		}, nil
	case errors.Is(err, errGrantReplay):
		again, aerr := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
		if aerr != nil {
			return nil, aerr
		}
		if again == nil {
			return nil, fmt.Errorf("%w: request_id vanished after rollback", model.ErrConcurrentUpdate)
		}
		if err := assertSameGrantRequest(in, again); err != nil {
			return nil, err
		}
		acc, aerr := l.freshAccount(in.Mid, snap)
		if aerr != nil {
			return nil, aerr
		}
		return &rpc.GrantCoinReply{
			Duplicated: true,
			Account:    accountInfo(in.Mid, acc, l.todayTossed(in.Mid), coin),
			FlowId:     again.ID,
		}, nil
	case errors.Is(err, model.ErrGrantBalanceWouldGoNegative):
		balance := int64(0)
		if inTxAcc != nil {
			balance = inTxAcc.Balance
		}
		return nil, fmt.Errorf("%w: mid=%d 当前余额 %d 枚，本次扣回 %d 枚",
			model.ErrGrantBalanceWouldGoNegative, in.Mid, balance, -in.Delta)
	default:
		return nil, err
	}
}

// freshAccount 回读账户；读失败或从未建过时用事务内快照兜底，
// 避免「改动已经成功却回一个空账户」。
func (l *GrantCoinLogic) freshAccount(mid int64, fallback *model.Account) (*model.Account, error) {
	acc, err := l.svcCtx.Accounts.FindOne(l.ctx, mid)
	if err != nil {
		if fallback != nil {
			return fallback, nil
		}
		return nil, err
	}
	if acc == nil {
		return fallback, nil
	}
	return acc, nil
}

// todayTossed 读当日已投枚数用于账户回显；失败按 0 处理并记日志
// （今日额度不是发放的判定条件，读它只是为了让响应里的账户完整）。
func (l *GrantCoinLogic) todayTossed(mid int64) int64 {
	d, err := l.svcCtx.Daily.FindOne(l.ctx, mid, model.TodayDayNo())
	if err != nil {
		l.Logger.Errorf("coin: GrantCoin daily reread failed, mid=%d: %v", mid, err)
		return 0
	}
	if d == nil {
		return 0
	}
	return int64(d.Tossed)
}

// assertSameGrantRequest 校验同 request_id 的发放重放必须与首次参数一致。
// 比较 biz_no：订单重放时若被拿去给别的订单记账，就是账实不符。
// 比的是 clipID 后的值 —— 落库时同样按列宽收敛（cn_flow.biz_no VARCHAR(64)），
// 用原始入参去比会让超长订单号的合法重放被误判成串号冲突。
func assertSameGrantRequest(in *rpc.GrantCoinReq, stored *model.Flow) error {
	if int32(in.FlowType) == stored.FlowType &&
		in.Mid == stored.Mid &&
		in.Delta == stored.Delta &&
		clipID(in.BizNo) == stored.BizNo &&
		stored.TargetAid == 0 {
		return nil
	}
	return fmt.Errorf("%w: request_id=%s 已对应 flow(mid=%d,type=%d,delta=%d,biz_no=%s,aid=%d)，本次请求(mid=%d,delta=%d,type=%s,biz_no=%s)",
		model.ErrIdempotencyConflict, in.RequestId,
		stored.Mid, stored.FlowType, stored.Delta, stored.BizNo, stored.TargetAid,
		in.Mid, in.Delta, in.FlowType, in.BizNo)
}
