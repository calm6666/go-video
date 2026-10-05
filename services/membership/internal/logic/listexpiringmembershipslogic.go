package logic

import (
	"context"
	"fmt"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListExpiringMembershipsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListExpiringMembershipsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListExpiringMembershipsLogic {
	return &ListExpiringMembershipsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListExpiringMemberships cron 扫描到期区间（闭区间 [from_expire_at, to_expire_at]）。
//
// 口径：
//  1. limit 超过 ExpireScanMaxLimit 由本服务裁剪而不是报错（批处理调用方不该被参数细节卡住）；
//  2. 按 (expire_at, membership_id) 升序返回，next_expire_at_cursor 是本批最后一条的 expire_at；
//     返回条数不足 limit 即代表区间扫完，游标回 0；
//  3. 游标只有秒级精度，同一秒内跨批时会重复投喂几条——这是刻意的：
//     ExpireMembership 的 request_id 幂等 + EXPIRE 台账去重保证重复投喂不会重复记账，
//     反过来若跳过同秒未处理的行就会漏掉真实到期。宁重不漏。
//  4. 只读，不改状态：是否真的过期由 ExpireMembership 用服务端时钟再判一次。
func (l *ListExpiringMembershipsLogic) ListExpiringMemberships(in *rpc.ListExpiringMembershipsReq) (*rpc.ListExpiringMembershipsReply, error) {
	cfg := l.svcCtx.Config.Membership
	if in.ToExpireAt <= 0 {
		return nil, fmt.Errorf("%w: to_expire_at=%d", model.ErrInvalidExpireRange, in.ToExpireAt)
	}
	from := in.FromExpireAt
	if from <= 0 {
		from = 1 // 覆盖「从未记过到期时间」的历史行，仍走 idx_expire_at
	}
	if from > in.ToExpireAt {
		return nil, fmt.Errorf("%w: from=%d > to=%d", model.ErrInvalidExpireRange, from, in.ToExpireAt)
	}

	limit := clampLimit(cfg, in.Limit)
	rows, err := l.svcCtx.Membership.ListExpiring(l.ctx, from, in.ToExpireAt, in.AutoRenewOnly, limit)
	if err != nil {
		l.Errorf("membership/ListExpiringMemberships: scan failed from=%d to=%d limit=%d err=%v", from, in.ToExpireAt, limit, err)
		return nil, err
	}

	out := &rpc.ListExpiringMembershipsReply{Memberships: membershipListToRPC(rows)}
	if int64(len(rows)) >= limit && len(rows) > 0 {
		out.NextExpireAtCursor = rows[len(rows)-1].ExpireAt
	}
	return out, nil
}
