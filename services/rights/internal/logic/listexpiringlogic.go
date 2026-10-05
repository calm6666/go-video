package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListExpiringLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListExpiringLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListExpiringLogic {
	return &ListExpiringLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListExpiring 查询即将过期的窗口（cron 用）。
// within_seconds 默认 3600，ps 上限 100。按 end_time 升序返回。
func (l *ListExpiringLogic) ListExpiring(in *rpc.ListExpiringReq) (*rpc.WindowsReply, error) {
	if in.WithinSeconds <= 0 {
		return nil, model.ErrExpiringWithinInvalid
	}
	if in.Ps <= 0 || in.Ps > 100 {
		return nil, model.ErrPsTooLarge
	}
	rows, total, err := l.svcCtx.Repository.ListExpiring(l.ctx, in.WithinSeconds, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("rights/ListExpiring: within=%d err=%v", in.WithinSeconds, err)
		return nil, err
	}
	out := make([]*rpc.Window, 0, len(rows))
	for _, w := range rows {
		out = append(out, windowToRPC(w))
	}
	return &rpc.WindowsReply{Total: total, Windows: out}, nil
}
