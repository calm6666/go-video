package logic

import (
	"context"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLeasesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListLeasesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLeasesLogic {
	return &ListLeasesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询租约（含已过期可抢占项）。
//
// now=0 时取服务端时钟，避免各副本时钟漂移导致「谁能抢占」判定不一致；
// only_expired=true 用于排障：这些租约的持有实例已经失联。
func (l *ListLeasesLogic) ListLeases(in *rpc.ListLeasesReq) (*rpc.ListLeasesReply, error) {
	if in == nil {
		in = &rpc.ListLeasesReq{}
	}
	limit, err := l.svcCtx.PageSize(in.PageSize)
	if err != nil {
		return nil, err
	}
	cursorID, err := decodeIDCursor(in.Cursor)
	if err != nil {
		return nil, err
	}
	now := in.Now
	if now <= 0 {
		now = l.svcCtx.ServerTime()
	}

	rows, next, err := l.svcCtx.Leases.ListByCursor(l.ctx, in.TaskKey, in.OnlyExpired, now, cursorID, limit)
	if err != nil {
		l.Errorf("ListLeases failed, task_key=%s only_expired=%t", in.TaskKey, in.OnlyExpired)
		return nil, err
	}
	total, err := l.svcCtx.Leases.CountByFilter(l.ctx, in.TaskKey, in.OnlyExpired, now)
	if err != nil {
		l.Errorf("ListLeases count failed, task_key=%s only_expired=%t", in.TaskKey, in.OnlyExpired)
		return nil, err
	}
	return &rpc.ListLeasesReply{
		List:       leaseInfoList(rows),
		NextCursor: encodeIDCursor(next),
		HasMore:    next > 0,
		Total:      total,
	}, nil
}
