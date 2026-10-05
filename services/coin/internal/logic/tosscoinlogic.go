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

type TossCoinLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTossCoinLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TossCoinLogic {
	return &TossCoinLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// errTossReplay 是事务内的信号：本次 request_id 已被另一个并发请求写成流水。
// 它不是失败（事务回滚后什么都没丢），由外层按「首次结论 + duplicated=true」返回。
var errTossReplay = errors.New("coin: toss request replayed by a concurrent transaction")

// 投币（扣币 + 记录 + 限额判定，幂等）。
//
// 一个事务内按固定顺序完成（顺序换了就是死锁或双扣，见迁移 SQL「锁风险」段）：
//
//	① cn_account 建仓/行锁   —— 同一 mid 所有写操作的串行点
//	② request_id 复核        —— 幂等最终防线（快路径之后的并发窗口）
//	③ cn_toss 行锁           —— 只锁不改，先定锁序
//	④ cn_daily_toss 条件累加 —— 日额度判定唯一真值
//	⑤ cn_account 条件扣减    —— UPDATE ... WHERE balance >= ?，affected=0 即余额不足
//	⑥ cn_toss 落库           —— 单片累计上限由条件 UPDATE 承担
//	⑦ cn_flow 追加           —— 负 delta，与①~⑥同事务提交
//
// 任何一步不满足都整体回滚：不可能出现「扣了币没记录」或「记了流水没扣币」。
// 四类拒绝都是**业务结论**（accepted=false + reason + reject_detail），
// 只有 mid<=0、request_id 为空这种入参非法才返回 gRPC 错误。
func (l *TossCoinLogic) TossCoin(in *rpc.TossCoinReq) (*rpc.TossCoinReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.RequestId == "" {
		return nil, model.ErrRequestIDRequired
	}
	// proto: count<=0 视为 1，服务侧不猜客户端想要几枚。
	count := int32(in.Count)
	if count <= 0 {
		count = 1
	}
	needCoins := int64(count)
	coin := l.svcCtx.Coin()

	// aid<=0 是结论不是错误：本服务不查稿件是否存在（跨服务越权），
	// 存在性由网关/详情页保证，这里只挡「根本没有目标」这一种畸形请求。
	if in.TargetAid <= 0 {
		return l.reject(in.Mid, rpc.TossRejectReason_TOSS_REJECT_TARGET_INVALID,
			"未指定有效的目标稿件（aid 必须为正整数）")
	}
	// 单次请求就超过单片累计上限时不必开事务：结论恒为拒绝。
	if needCoins > coin.PerTargetLimit {
		return l.reject(in.Mid, rpc.TossRejectReason_TOSS_REJECT_TARGET_LIMIT,
			fmt.Sprintf("同一内容累计最多投 %d 枚，本次请求 %d 枚", coin.PerTargetLimit, count))
	}

	stored, err := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		return l.replay(in, count, stored)
	}

	var snapshot *model.Account // 扣减前的账户，用于给出准确的余额结论
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
			// 建仓送币必须留流水，否则余额比流水多出的部分无法解释（对账不变式）。
			if _, err := l.svcCtx.Flows.InsertTx(ctx, tx, initialGrantFlow(in.Mid, coin.InitialBalance)); err != nil {
				return err
			}
			// 注意：EnsureTx 插入的那一行 balance 已经是 InitialBalance，acc 是它读回来的值，
			// 这里不能再加一次 —— 否则本笔投币流水的 balance_after 会凭空多出初始币，
			// 「流水 balance_after == 落库余额」这条对账口径在新账户的第一笔上就破了。
		}
		snapshot = acc

		// 幂等复核：此刻已持有该 mid 的账户行锁，同 request_id 必然同 mid，
		// 所以并发重试方要么已被前一个事务提交（这里读得到），要么还堵在锁上。
		again, err := l.svcCtx.Flows.FindByRequestID(ctx, tx, in.RequestId)
		if err != nil {
			return err
		}
		if again != nil {
			return errTossReplay
		}

		row, err := l.svcCtx.Tosses.LockByTargetTx(ctx, tx, in.Mid, in.TargetAid)
		if err != nil {
			return err
		}

		day := model.TodayDayNo()
		if err := l.svcCtx.Daily.EnsureTx(ctx, tx, in.Mid, day); err != nil {
			return err
		}
		quotaOK, err := l.svcCtx.Daily.AccumulateTx(ctx, tx, in.Mid, day, count, int32(coin.DailyLimit))
		if err != nil {
			return err
		}
		if !quotaOK {
			return model.ErrDailyLimitExceeded
		}

		deducted, err := l.svcCtx.Accounts.DeductForTossTx(ctx, tx, in.Mid, needCoins, coin.MinBalanceToToss)
		if err != nil {
			return err
		}
		if !deducted {
			return model.ErrInsufficientBalance
		}

		now := model.NowUnix()
		platform := int32(in.Platform)
		traceID := clipID(in.ClientTraceId)
		switch {
		case row == nil:
			if _, err := l.svcCtx.Tosses.InsertTx(ctx, tx, &model.Toss{
				Mid:           in.Mid,
				TargetAid:     in.TargetAid,
				Count:         count,
				State:         model.TossStateActive,
				FirstTossedAt: now,
				LastTossedAt:  now,
				LastRequestID: in.RequestId,
				Platform:      platform,
				TraceID:       traceID,
				LastTossDate:  day,
				Ctime:         now,
				Mtime:         now,
			}); err != nil {
				return err
			}
		case row.State == model.TossStateActive:
			// 单片累计上限的权威判定就在这条条件 UPDATE 里（`count` + delta <= limit）。
			limitOK, err := l.svcCtx.Tosses.AccumulateTx(ctx, tx, row.ID, count,
				int32(coin.PerTargetLimit), now, day, in.RequestId, platform, traceID)
			if err != nil {
				return err
			}
			if !limitOK {
				return model.ErrTargetLimitExceeded
			}
		case row.State == model.TossStateCancelled:
			// 取消过的行重新投币：`count` 被本次枚数**覆盖**而非累加，
			// 上一次已退回的币不能再算成「现在投出去的」。
			reOK, err := l.svcCtx.Tosses.ReactivateTx(ctx, tx, row.ID, count,
				now, day, in.RequestId, platform, traceID)
			if err != nil {
				return err
			}
			if !reOK {
				return model.ErrConcurrentUpdate
			}
		default:
			// 库里出现枚举外的 state：数据被绕过本服务写过，拒绝本次投币并暴露问题。
			return model.ErrConcurrentUpdate
		}

		_, err = l.svcCtx.Flows.InsertTx(ctx, tx, &model.Flow{
			Mid:          in.Mid,
			FlowType:     model.FlowTypeToss,
			Delta:        -needCoins,
			BalanceAfter: acc.Balance - needCoins,
			TargetAid:    in.TargetAid,
			Operator:     "user",
			RequestID:    in.RequestId,
			Remark:       fmt.Sprintf("投币 aid=%d ×%d", in.TargetAid, count),
			TraceID:      traceID,
		})
		return err
	})

	switch {
	case err == nil:
		return l.accepted(in, count)
	case errors.Is(err, model.ErrDailyLimitExceeded):
		return l.rejectWithAccount(snapshot, in.Mid, rpc.TossRejectReason_TOSS_REJECT_DAILY_LIMIT,
			l.dailyLimitDetail(in.Mid, needCoins))
	case errors.Is(err, model.ErrInsufficientBalance):
		balance := int64(0)
		if snapshot != nil {
			balance = snapshot.Balance
		}
		return l.rejectWithAccount(snapshot, in.Mid, rpc.TossRejectReason_TOSS_REJECT_INSUFFICIENT_BALANCE,
			fmt.Sprintf("当前余额 %d 枚，本次需要 %d 枚；硬币可通过投币之外的运营活动或购买硬币包获得", balance, needCoins))
	case errors.Is(err, model.ErrTargetLimitExceeded):
		return l.rejectWithAccount(snapshot, in.Mid, rpc.TossRejectReason_TOSS_REJECT_TARGET_LIMIT,
			fmt.Sprintf("对该稿件最多累计投 %d 枚（每人每片上限），本次还想再投 %d 枚", coin.PerTargetLimit, count))
	case errors.Is(err, errTossReplay):
		again, aerr := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
		if aerr != nil {
			return nil, aerr
		}
		if again == nil {
			// 事务回滚后键反而不见了：只可能是有人在删流水或读到了滞后副本，
			// 宁可让调用方重试，也不能当成成功。
			return nil, fmt.Errorf("%w: request_id vanished after rollback", model.ErrConcurrentUpdate)
		}
		return l.replay(in, count, again)
	default:
		return nil, err
	}
}

// dailyLimitDetail 组装「今日额度不足」的可读结论。回读失败时退化为只报上限，
// 不影响判定本身（判定已经由事务内的条件累加给出）。
func (l *TossCoinLogic) dailyLimitDetail(mid int64, needCoins int64) string {
	coin := l.svcCtx.Coin()
	tossed := int64(-1)
	if d, err := l.svcCtx.Daily.FindOne(l.ctx, mid, model.TodayDayNo()); err == nil && d != nil {
		tossed = int64(d.Tossed)
	}
	if tossed < 0 {
		return fmt.Sprintf("今日投币额度不足，每日上限 %d 枚，本次需要 %d 枚", coin.DailyLimit, needCoins)
	}
	remaining := coin.DailyLimit - tossed
	if remaining < 0 {
		remaining = 0
	}
	return fmt.Sprintf("今日已投 %d 枚，上限 %d 枚，还可投 %d 枚，本次需要 %d 枚",
		tossed, coin.DailyLimit, remaining, needCoins)
}

// accepted 在事务提交后回读账户、投币记录与流水，构造成功结论。
//
// 回读失败时**不**把已经提交的投币报成错误：客户端会当失败重试，
// 虽然 request_id 幂等能挡住二次扣币，但用户看到的是一次莫名失败。
// 这里降级为「按事务内可推导的值回显 + 错误日志」，投币事实本身已经安全落库。
func (l *TossCoinLogic) accepted(in *rpc.TossCoinReq, count int32) (*rpc.TossCoinReply, error) {
	coin := l.svcCtx.Coin()
	acc, err := l.svcCtx.Accounts.FindOne(l.ctx, in.Mid)
	if err != nil {
		l.Logger.Errorf("coin: TossCoin committed but account reread failed, mid=%d: %v", in.Mid, err)
	}
	today, err := l.svcCtx.Daily.FindOne(l.ctx, in.Mid, model.TodayDayNo())
	if err != nil {
		l.Logger.Errorf("coin: TossCoin committed but daily reread failed, mid=%d: %v", in.Mid, err)
	}
	row, err := l.svcCtx.Tosses.FindOne(l.ctx, in.Mid, in.TargetAid)
	if err != nil {
		l.Logger.Errorf("coin: TossCoin committed but toss reread failed, mid=%d aid=%d: %v", in.Mid, in.TargetAid, err)
	}
	flow, err := l.svcCtx.Flows.FindByRequestID(l.ctx, nil, in.RequestId)
	if err != nil {
		l.Logger.Errorf("coin: TossCoin committed but flow reread failed, request_id=%s: %v", in.RequestId, err)
	}

	var todayTossed, flowID int64
	if today != nil {
		todayTossed = int64(today.Tossed)
	}
	if flow != nil {
		flowID = flow.ID
	}
	remaining := coin.DailyLimit - todayTossed
	if remaining < 0 {
		remaining = 0
	}
	// 展示口径的汇总缓存尽力清理；清不掉只是几秒旧计数，不影响余额与额度真值。
	invalidateSummaryCache(l.ctx, l.svcCtx, in.TargetAid)

	reply := &rpc.TossCoinReply{
		Accepted:     true,
		Reason:       rpc.TossRejectReason_TOSS_ACCEPTED,
		Account:      accountInfo(in.Mid, acc, todayTossed, coin),
		Toss:         tossInfo(row),
		FlowId:       flowID,
		RejectDetail: fmt.Sprintf("本次投出 %d 枚，今日还可投 %d 枚", count, remaining),
	}
	if row != nil {
		reply.RejectDetail = fmt.Sprintf("%s，对该稿件累计已投 %d 枚", reply.RejectDetail, row.Count)
	}
	return reply, nil
}

// replay 按首次落库的流水重建结论：绝不重复扣币。
//
// 参数不一致（同 request_id 投了不同稿件/不同枚数）必须报冲突而不是任选一份结论 ——
// 那是调用方把幂等键用串了，静默复用会吞掉用户真实的一枚币。
// 账户回显取当前值（首次扣减之后可能又有发放），首次扣减额由返回的 flow_id 指向的流水承载。
func (l *TossCoinLogic) replay(in *rpc.TossCoinReq, count int32, stored *model.Flow) (*rpc.TossCoinReply, error) {
	if err := assertSameTossRequest(in.Mid, in.TargetAid, count, stored); err != nil {
		return nil, err
	}
	coin := l.svcCtx.Coin()
	acc, err := l.svcCtx.Accounts.FindOne(l.ctx, in.Mid)
	if err != nil {
		return nil, err
	}
	today, err := l.svcCtx.Daily.FindOne(l.ctx, in.Mid, model.TodayDayNo())
	if err != nil {
		return nil, err
	}
	row, err := l.svcCtx.Tosses.FindOne(l.ctx, in.Mid, in.TargetAid)
	if err != nil {
		return nil, err
	}
	var todayTossed int64
	if today != nil {
		todayTossed = int64(today.Tossed)
	}
	return &rpc.TossCoinReply{
		Accepted:   true,
		Reason:     rpc.TossRejectReason_TOSS_ACCEPTED,
		Duplicated: true,
		Account:    accountInfo(in.Mid, acc, todayTossed, coin),
		Toss:       tossInfo(row),
		FlowId:     stored.ID,
		RejectDetail: fmt.Sprintf("相同 request_id 的重复请求，返回首次投币结论（首次扣减 %d 枚，flow_id=%d）",
			-stored.Delta, stored.ID),
	}, nil
}

// reject / rejectWithAccount 构造拒绝结论。
// rejectWithAccount 复用事务内已读到的账户快照，省掉一次回读；
// 账户读取失败一律上抛 —— 不用「余额 0」的假账户冒充结论。
func (l *TossCoinLogic) reject(mid int64, reason rpc.TossRejectReason, detail string) (*rpc.TossCoinReply, error) {
	return l.rejectWithAccount(nil, mid, reason, detail)
}

func (l *TossCoinLogic) rejectWithAccount(acc *model.Account, mid int64, reason rpc.TossRejectReason,
	detail string) (*rpc.TossCoinReply, error) {
	coin := l.svcCtx.Coin()
	if acc == nil {
		found, err := l.svcCtx.Accounts.FindOne(l.ctx, mid)
		if err != nil {
			return nil, err
		}
		acc = found
	}
	today, err := l.svcCtx.Daily.FindOne(l.ctx, mid, model.TodayDayNo())
	if err != nil {
		return nil, err
	}
	var todayTossed int64
	if today != nil {
		todayTossed = int64(today.Tossed)
	}
	return &rpc.TossCoinReply{
		Accepted:     false,
		Reason:       reason,
		Account:      accountInfo(mid, acc, todayTossed, coin),
		RejectDetail: detail,
	}, nil
}

// assertSameTossRequest 校验「同 request_id 必须是同一笔投币」。
// count 必须是归一化（<=0 视为 1）之后的值，否则首次以 count=0 落库、
// 重放时按 0 比较会误判成冲突。
func assertSameTossRequest(mid, targetAid int64, count int32, stored *model.Flow) error {
	if stored.FlowType == model.FlowTypeToss &&
		stored.Mid == mid &&
		stored.TargetAid == targetAid &&
		stored.Delta == -int64(count) {
		return nil
	}
	return fmt.Errorf("%w: request_id=%s 已对应 flow(mid=%d,type=%d,delta=%d,aid=%d)，本次请求(mid=%d,count=%d,aid=%d)",
		model.ErrIdempotencyConflict, stored.RequestID,
		stored.Mid, stored.FlowType, stored.Delta, stored.TargetAid, mid, count, targetAid)
}
