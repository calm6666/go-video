package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ClearUnreadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewClearUnreadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ClearUnreadLogic {
	return &ClearUnreadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ClearUnread 清零用户未读动态数。
// Redis + DB 双写。
func (l *ClearUnreadLogic) ClearUnread(in *rpc.MidReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if err := l.svcCtx.Repository.ClearUnread(l.ctx, in.Mid); err != nil {
		l.Errorf("feed/ClearUnread: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
