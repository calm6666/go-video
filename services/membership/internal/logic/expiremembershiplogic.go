package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// expireLedgerSource 到期台账的来源位。
// rpc.GrantSource 的 1..5 全部表示「某种授予渠道」，到期不是授予，
// 因此记 GRANT_SOURCE_UNSPECIFIED(0)：填一个不存在的枚举值才是造假。
const expireLedgerSource int32 = 0

// expireLedgerReason 生成 EXPIRE 台账的理由：cron 填了就用自己的（能说明是哪次任务），
// 留空回落到机器可读摘要——台账不允许空串，否则审计页分不清「无理由」与「忘了写」。
func expireLedgerReason(reason string, expireAt int64, operator string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return r
	}
	return fmt.Sprintf("membership expired at %d, detected by %s", expireAt, operator)
}

type ExpireMembershipLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewExpireMembershipLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpireMembershipLogic {
	return &ExpireMembershipLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ExpireMembership cron：幂等置过期。
//
// 口径（本方法是「判定口径 = expire_at 即状态」这条设计的直接体现）：
//  1. mb_membership 没有 state 列，expire_at <= now 就是已过期，因此本接口
//     不改 mb_membership 的任何字段——改写 expire_at 会抹掉「到期于何时」的事实，
//     顺延续费（NextExpireAt）也就失去基准；它只补一条 EXPIRE 台账，
//     让「权益何时终止」在只增不删的台账里可查；
//  2. skipped=true 的三种情形：未到期、同一到期时间已记过 EXPIRE、
//     （重放同一 request_id 时也是已处理过）。到期是幂等事件，不该记成两次；
//  3. request_id 唯一索引挡住同一次调用的重试，FindAction(mid,vip,EXPIRE,expire_at)
//     挡住 cron 换 request_id 对同一到期事件的重复扫描；
//  4. 身份行不存在返回 ErrMembershipNotFound 而不是 skipped：
//     调用方（services/cron 的任务表）扫到不存在的用户属于任务与数据不一致，
//     静默 skip 会把这种漂移藏起来；
//  5. 不发 MQ、不写 outbox、不外呼：本轮没有事件总线契约，权益的失效由
//     CheckEntitlement 每次读库自然达成（见 README「已知缺口」）；
//  6. reason 由调用方（cron 任务）填写，留空时回落成机器可读摘要，
//     这样台账里既能看到「哪次任务判的到期」，也不会出现空理由。
func (l *ExpireMembershipLogic) ExpireMembership(in *rpc.ExpireMembershipReq) (*rpc.ExpireMembershipReply, error) {
	if err := requireMid(in.Mid); err != nil {
		return nil, err
	}
	if err := requireVipType(in.VipType); err != nil {
		return nil, err
	}
	if err := requireRequestId(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireOperator(in.Operator); err != nil {
		return nil, err
	}
	if err := optionalReason(in.Reason); err != nil {
		return nil, err
	}

	vipType := int32(in.VipType)
	operator := strings.TrimSpace(in.Operator)
	now := model.NowUnix()

	// 同一次调用的重试：request_id 已记过账，直接把「已处理」还回去。
	first, err := l.svcCtx.Grant.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("membership/ExpireMembership: idempotency lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if first != nil {
		return l.replay(first, in)
	}

	var (
		skipped  bool
		grantID  int64
		applied  *model.Membership
		expireAt int64
	)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		cur, err := l.svcCtx.Membership.FindOneTx(ctx, session, in.Mid, vipType)
		if err != nil {
			return err
		}
		if cur == nil {
			return model.ErrMembershipNotFound
		}
		applied = cur
		if cur.ExpireAt > now {
			// 还没到期：不记台账，也不消耗 request_id，到期后同一次任务可以重试成功。
			skipped = true
			return nil
		}
		// 换 request_id 的重复扫描：同一 expire_at 的 EXPIRE 只该有一条。
		seen, err := l.svcCtx.Grant.FindAction(ctx, in.Mid, vipType, model.ActionExpire, cur.ExpireAt)
		if err != nil {
			return err
		}
		if seen != nil {
			skipped = true
			grantID = seen.GrantID
			return nil
		}

		g := &model.Grant{
			Mid:            in.Mid,
			VipType:        vipType,
			Action:         model.ActionExpire,
			DeltaDays:      0, // 到期不扣时长：expire_at 本来就是过去时刻
			Source:         expireLedgerSource,
			BeforeExpireAt: cur.ExpireAt,
			AfterExpireAt:  cur.ExpireAt,
			Operator:       operator,
			RequestID:      in.RequestId,
			Reason:         expireLedgerReason(in.Reason, cur.ExpireAt, operator),
			Ctime:          now,
		}
		id, err := l.svcCtx.Grant.InsertTx(ctx, session, g)
		if err != nil {
			return err
		}
		grantID = id
		expireAt = cur.ExpireAt
		return nil
	})
	if err != nil {
		if l.svcCtx.Grant.IsDuplicate(err) {
			rival, ferr := l.svcCtx.Grant.FindByRequestID(l.ctx, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			if rival != nil {
				return l.replay(rival, in)
			}
			return nil, err
		}
		if errors.Is(err, model.ErrMembershipNotFound) {
			l.Infof("membership/ExpireMembership: no identity row mid=%d vip_type=%d request_id=%s", in.Mid, vipType, in.RequestId)
			return nil, err
		}
		l.Errorf("membership/ExpireMembership: apply failed mid=%d vip_type=%d request_id=%s err=%v",
			in.Mid, vipType, in.RequestId, err)
		return nil, err
	}

	if skipped {
		l.Infof("membership/ExpireMembership: skipped mid=%d vip_type=%d grant_id=%d request_id=%s",
			in.Mid, vipType, grantID, in.RequestId)
	} else {
		l.Infof("membership/ExpireMembership: marked expired mid=%d vip_type=%d expire_at=%d grant_id=%d",
			in.Mid, vipType, expireAt, grantID)
	}
	return &rpc.ExpireMembershipReply{
		Skipped:    skipped,
		GrantId:    grantID,
		Membership: membershipToRPC(applied),
	}, nil
}

// replay 命中同一 request_id：EXPIRE 台账只该有一条，回查后按「已处理」返回。
func (l *ExpireMembershipLogic) replay(first *model.Grant, in *rpc.ExpireMembershipReq) (*rpc.ExpireMembershipReply, error) {
	if !grantMatchesRequest(first, in.Mid, int32(in.VipType), model.ActionExpire, 0,
		0, expireLedgerSource, "", "") {
		l.Errorf("membership/ExpireMembership: request_id=%s reused with different parameters (granted mid=%d vip_type=%d action=%s)",
			in.RequestId, first.Mid, first.VipType, first.Action)
		return nil, model.ErrRequestIdReused
	}
	row, err := l.svcCtx.Membership.FindOne(l.ctx, first.Mid, first.VipType)
	if err != nil {
		l.Errorf("membership/ExpireMembership: replay reread mid=%d vip_type=%d err=%v", first.Mid, first.VipType, err)
		return nil, err
	}
	if row == nil {
		l.Errorf("membership/ExpireMembership: grant_id=%d has no membership row, mid=%d vip_type=%d",
			first.GrantID, first.Mid, first.VipType)
		return nil, model.ErrMembershipNotFound
	}
	l.Infof("membership/ExpireMembership: replay request_id=%s mid=%d grant_id=%d", in.RequestId, in.Mid, first.GrantID)
	return &rpc.ExpireMembershipReply{
		Skipped:    true, // 契约里 skipped 含「已处理过」
		GrantId:    first.GrantID,
		Membership: membershipToRPC(row),
	}, nil
}
