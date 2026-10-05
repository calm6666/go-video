package logic

import (
	"context"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"

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

// GetUnreadCount 返回分类未读数。
// 读链路：Redis 快照 -> inbox_unread_stat -> inbox_user_message 重算。
// force_recompute=true 时直接从明细重算，用于客户端发现计数异常时的自助修复。
func (l *GetUnreadCountLogic) GetUnreadCount(in *rpc.GetUnreadCountReq) (*rpc.GetUnreadCountReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	snapshot, err := l.svcCtx.Repository.GetUnread(l.ctx, in.Mid, in.ForceRecompute)
	if err != nil {
		return nil, err
	}
	return &rpc.GetUnreadCountReply{
		Total:      snapshot.Total,
		ByCategory: toRPCUnread(snapshot.ByCategory),
		Mtime:      snapshot.Mtime,
	}, nil
}
