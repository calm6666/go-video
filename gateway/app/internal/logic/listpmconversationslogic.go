// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPmConversationsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 会话列表（cursor 分页，黑名单/风控/隐藏会话在服务侧过滤）
func NewListPmConversationsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPmConversationsLogic {
	return &ListPmConversationsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListPmConversations 透传 (mid, cursor, ps) 游标口径；ps=0 交给服务按配置决定页大小，
// 网关不写死服务侧策略。服务返回错误（含未实现）一律上抛，不返回空页伪装成功。
func (l *ListPmConversationsLogic) ListPmConversations(req *types.ParamPmConversations) (resp *types.PmConversationsResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	reply, err := l.svcCtx.PrivateMessage.ListConversations(l.ctx, &privatemessagerpc.ListConversationsReq{
		Mid:           req.Mid,
		Cursor:        req.Cursor,
		Ps:            req.Ps,
		OnlyUnread:    req.OnlyUnread,
		IncludeHidden: req.IncludeHidden,
		TraceId:       req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/listPmConversations: mid=%d cursor=%q err=%v", req.Mid, req.Cursor, err)
		return nil, err
	}
	return &types.PmConversationsResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmConversationsData{
			List:        pmConversationsToAPI(reply.GetList()),
			NextCursor:  reply.GetNextCursor(),
			HasMore:     reply.GetHasMore(),
			UnreadTotal: reply.GetUnreadTotal(),
		},
		TTL: 0,
	}, nil
}
