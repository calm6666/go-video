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

type CancelTossLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCancelTossLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CancelTossLogic {
	return &CancelTossLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// errCancelReplay / errAlreadyCancelled 是事务内带回的结论信号，不是失败：
// 事务回滚后由外层翻译成 duplicated=true 的正常响应。
var (
	errCancelReplay     = errors.New("coin: cancel request replayed by a concurrent transaction")
	errAlreadyCancelled = errors.New("coin: toss record already cancelled")
)

// 取消投币（窗口内全额退回）。
//
// 判定口径：
//   - 只在 state=ACTIVE 且 now - last_tossed_at <= Coin.CancelWindowSeconds 时允许，
//     超窗是**结论**（cancelled=false + reason），不能静默成功；
//   - 全额退回该记录的 `count` 枚：余额增加 + 一条 CANCEL_TOSS 正向流水 +
//     cn_toss 置 CANCELLED + cn_daily_toss 回退最后投币日的计数，全在一个事务里；
//   - total_tossed 不回退（proto 锁定：那是「曾经投出去过多少」的历史口径）；
//   - 重复取消（同 request_id 重放，或换 key 再取消已 CANCELLED 的记录）一律
//     duplicated=true 且不再退币。
//
// 契约口径：超窗是 TOSS_REJECT_CANCEL_WINDOW_EXPIRED（不是限额问题），可展示文案走 reject_detail。
func (l *CancelTossLogic) CancelToss(in *rpc.CancelTossReq) (*rpc.CancelTossReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.RequestId == "" {
		return nil, model.ErrRequestIDRequired
	}
	if in.TargetAid <= 0 {
		return nil, model.ErrInvalidTargetAid
	}
	operator := in.Operator
	if operator == "" {
		operator = "user"
	}
	// 代他人取消必须留下可追责的理由，否则运营面就是无凭据的余额改动机。
	if operator != "user" && in.Reason == "" {
		return nil, model.ErrOperatorReasonRequired
	}
	coin := l.svcCtx.Coin()

	row, err := l.svcCtx.Tosses.FindOne(l.ctx, in.Mid, in.TargetAid)
	if err != nil {
		return nil, err
	}
	if row == nil || row.State != model.TossStateActive {
		// 没有可取消的投币：要么从没投过（TARGET_INVALID），要么已取消（重放）。
		if row == nil {
			return l.conclusion(in.Mid, in.TargetAid, false,
				rpc.TossRejectReason_TOSS_REJECT_TARGET_INVALID, false, 0)
		}
		flow, ferr := l.svcCtx.Flows.FindLatestByTarget(l.ctx, in.Mid, in.TargetAid, model.FlowTypeCancelToss)
		if ferr != nil {
			return nil, ferr
		}
		var flowID int64
		if flow != nil {
			flowID = flow.ID
		}
		return l.conclusion(in.Mid, in.TargetAid, true, rpc.TossRejectReason_TOSS_ACCEPTED, true, flowID)
	}
	// 窗口判定先行，省掉一次注定失败的事务。最终判定仍在事务内以锁定行重做一遍。
	if model.NowUnix()-row.LastTossedAt > coin.CancelWindowSeconds {
		return l.reject(in.Mid, in.TargetAid, rpc.TossRejectReason_TOSS_REJECT_CANCEL_WINDOW_EXPIRED,
			fmt.Sprintf("该投币已超过 %d 秒的撤币窗口", coin.CancelWindowSeconds))
	}

	stored, err := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		if err := assertSameCancelRequest(in.Mid, in.TargetAid, stored); err != nil {
			return nil, err
		}
		return l.conclusion(in.Mid, in.TargetAid, true, rpc.TossRejectReason_TOSS_ACCEPTED, true, stored.ID)
	}

	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		// 与投币同一取锁顺序：先账户行锁（同用户串行点），再投币记录，最后动额度与台账。
		acc, err := l.svcCtx.Accounts.LockForUpdateTx(ctx, tx, in.Mid)
		if err != nil {
			return err
		}
		again, err := l.svcCtx.Flows.FindByRequestID(ctx, tx, in.RequestId)
		if err != nil {
			return err
		}
		if again != nil {
			return errCancelReplay
		}
		locked, err := l.svcCtx.Tosses.LockByTargetTx(ctx, tx, in.Mid, in.TargetAid)
		if err != nil {
			return err
		}
		if locked == nil {
			return model.ErrTossNotFound
		}
		if locked.State != model.TossStateActive {
			return errAlreadyCancelled
		}
		if model.NowUnix()-locked.LastTossedAt > coin.CancelWindowSeconds {
			return model.ErrCancelWindowExpired
		}
		refund := int64(locked.Count)
		ok, err := l.svcCtx.Accounts.RefundForCancelTx(ctx, tx, in.Mid, refund)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		cOK, err := l.svcCtx.Tosses.CancelTx(ctx, tx, locked.ID, model.NowUnix())
		if err != nil {
			return err
		}
		if !cOK {
			return model.ErrConcurrentUpdate
		}
		// 只回退最后投币日那天的额度：PerTargetLimit 很小、窗口只有 24 小时，
		// 跨日累加到多个日桶的极端情形由 GREATEST 夹底，宁可少给也不凭空多给。
		if err := l.svcCtx.Daily.RollbackTx(ctx, tx, in.Mid, locked.LastTossDate, locked.Count); err != nil {
			return err
		}
		_, err = l.svcCtx.Flows.InsertTx(ctx, tx, &model.Flow{
			Mid:          in.Mid,
			FlowType:     model.FlowTypeCancelToss,
			Delta:        refund,
			BalanceAfter: acc.Balance + refund,
			TargetAid:    in.TargetAid,
			Operator:     clipID(operator),
			RequestID:    in.RequestId,
			Remark:       clipRemark(fmt.Sprintf("取消投币 aid=%d，退回 %d 枚；%s", in.TargetAid, refund, in.Reason)),
		})
		return err
	})

	switch {
	case err == nil:
		invalidateSummaryCache(l.ctx, l.svcCtx, in.TargetAid)
		flow, ferr := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
		if ferr != nil {
			l.Logger.Errorf("coin: CancelToss committed but flow reread failed, request_id=%s: %v", in.RequestId, ferr)
		}
		var flowID int64
		if flow != nil {
			flowID = flow.ID
		}
		return l.conclusion(in.Mid, in.TargetAid, true, rpc.TossRejectReason_TOSS_ACCEPTED, false, flowID)
	case errors.Is(err, errCancelReplay), errors.Is(err, errAlreadyCancelled):
		flow, ferr := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
		if ferr != nil {
			return nil, ferr
		}
		if flow != nil {
			if err := assertSameCancelRequest(in.Mid, in.TargetAid, flow); err != nil {
				return nil, err
			}
			return l.conclusion(in.Mid, in.TargetAid, true, rpc.TossRejectReason_TOSS_ACCEPTED, true, flow.ID)
		}
		return l.conclusion(in.Mid, in.TargetAid, true, rpc.TossRejectReason_TOSS_ACCEPTED, true, 0)
	case errors.Is(err, model.ErrCancelWindowExpired):
		return l.reject(in.Mid, in.TargetAid, rpc.TossRejectReason_TOSS_REJECT_CANCEL_WINDOW_EXPIRED,
			fmt.Sprintf("锁定行复核：已超过 %d 秒撤币窗口", l.svcCtx.Coin().CancelWindowSeconds))
	case errors.Is(err, model.ErrTossNotFound):
		return l.conclusion(in.Mid, in.TargetAid, false,
			rpc.TossRejectReason_TOSS_REJECT_TARGET_INVALID, false, 0)
	default:
		return nil, err
	}
}

// conclusion 组装取消响应：账户与投币记录一律回读当前真值，
// 读失败直接上抛（不返回半截响应让客户端以为「取消没生效」）。
func (l *CancelTossLogic) conclusion(mid, targetAid int64, cancelled bool,
	reason rpc.TossRejectReason, duplicated bool, flowID int64) (*rpc.CancelTossReply, error) {
	coin := l.svcCtx.Coin()
	acc, err := l.svcCtx.Accounts.FindOne(l.ctx, mid)
	if err != nil {
		return nil, err
	}
	today, err := l.svcCtx.Daily.FindOne(l.ctx, mid, model.TodayDayNo())
	if err != nil {
		return nil, err
	}
	row, err := l.svcCtx.Tosses.FindOne(l.ctx, mid, targetAid)
	if err != nil {
		return nil, err
	}
	var todayTossed int64
	if today != nil {
		todayTossed = int64(today.Tossed)
	}
	return &rpc.CancelTossReply{
		Cancelled:  cancelled,
		Reason:     reason,
		Duplicated: duplicated,
		Account:    accountInfo(mid, acc, todayTossed, coin),
		Toss:       tossInfo(row),
		FlowId:     flowID,
	}, nil
}

// reject 是「拒绝结论 + 可读文案」的组装口：reason 枚举只表达类别（客户端据此选文案模板），
// reject_detail 给出可直接展示的补充说明。仍回账户与投币行快照，客户端不需要再补一次查询。
func (l *CancelTossLogic) reject(mid, targetAid int64, reason rpc.TossRejectReason,
	detail string) (*rpc.CancelTossReply, error) {
	reply, err := l.conclusion(mid, targetAid, false, reason, false, 0)
	if reply != nil {
		reply.RejectDetail = detail
	}
	return reply, err
}

// assertSameCancelRequest 校验同 request_id 的取消重放必须指向同一目标。
func assertSameCancelRequest(mid, targetAid int64, stored *model.Flow) error {
	if stored.FlowType == model.FlowTypeCancelToss &&
		stored.Mid == mid &&
		stored.TargetAid == targetAid {
		return nil
	}
	return fmt.Errorf("%w: request_id=%s 已对应 flow(mid=%d,type=%d,delta=%d,aid=%d)，本次取消请求(mid=%d,aid=%d)",
		model.ErrIdempotencyConflict, stored.RequestID,
		stored.Mid, stored.FlowType, stored.Delta, stored.TargetAid, mid, targetAid)
}
