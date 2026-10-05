package logic

import (
	"context"

	"go-video/services/feed/internal/svc"
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetUnreadCountLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetUnreadCountLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUnreadCountLogic {
	return &GetUnreadCountLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetUnreadCount 查询用户未读动态数。
// Redis 计数器优先，缓存缺失回查 DB。
func (l *GetUnreadCountLogic) GetUnreadCount(in *rpc.MidReq) (*rpc.UnreadReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	n, err := l.svcCtx.Repository.GetUnreadCount(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("feed/GetUnreadCount: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.UnreadReply{Unread: n}, nil
}
