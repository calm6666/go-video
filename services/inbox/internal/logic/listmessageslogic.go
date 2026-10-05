package logic

import (
	"context"

	"go-video/services/inbox/internal/svc"
	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListMessagesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListMessagesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListMessagesLogic {
	return &ListMessagesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListMessages 按分类游标分页拉取收件箱。
// 游标不透明（内部为 (ctime,id)），客户端只回传 next_cursor；
// unread_total 走未读快照，不随分页重复 COUNT(*)。
func (l *ListMessagesLogic) ListMessages(in *rpc.ListMessagesReq) (*rpc.ListMessagesReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	rows, next, hasMore, err := l.svcCtx.Repository.ListMessages(
		l.ctx, in.Mid, convertCategory(in.Category), in.Cursor, in.Ps, in.UnreadOnly)
	if err != nil {
		return nil, err
	}
	var unreadTotal int64
	if unread, err := l.svcCtx.Repository.GetUnread(l.ctx, in.Mid, false); err != nil {
		// 未读计数是列表的附加信息：Redis/快照异常时不能拖垮收件箱读取，
		// 记日志并按 0 返回，由客户端重试。
		l.Errorf("inbox/ListMessages: mid=%d 读取未读计数失败 err=%v", in.Mid, err)
	} else {
		unreadTotal = unread.Total
		if category := convertCategory(in.Category); category != model.CategoryAll {
			unreadTotal = unread.ByCategory[category]
		}
	}
	return &rpc.ListMessagesReply{
		List:        toRPCMessages(rows),
		NextCursor:  next,
		HasMore:     hasMore,
		UnreadTotal: unreadTotal,
	}, nil
}
