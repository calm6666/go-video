package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ExpireWindowLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewExpireWindowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpireWindowLogic {
	return &ExpireWindowLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ExpireWindow 手动过期窗口（运营/cron）。
// 校验：window_id 有效且当前为 active；不可重复过期。
// 成功后失效对应 (content_id, content_type, region) 的 CheckPlayable 缓存。
func (l *ExpireWindowLogic) ExpireWindow(in *rpc.WindowReq) (*rpc.WindowReply, error) {
	if in.WindowId <= 0 {
		return nil, model.ErrInvalidWindowID
	}
	w, err := l.svcCtx.Repository.ExpireWindow(l.ctx, in.WindowId)
	if err != nil {
		l.Errorf("rights/ExpireWindow: window_id=%d err=%v", in.WindowId, err)
		return nil, err
	}
	return &rpc.WindowReply{Window: windowToRPC(w)}, nil
}
