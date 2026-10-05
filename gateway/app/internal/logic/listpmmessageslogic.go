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

type ListPmMessagesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 会话内消息分页（seq 游标倒序）
func NewListPmMessagesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPmMessagesLogic {
	return &ListPmMessagesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListPmMessages 走 (conversation_id, cursor_seq) 游标，禁止 offset 深翻页（docs/api-and-events.md §2）。
// mid 是查看者身份：明文解密授权与黑名单/风控可见性过滤在服务侧按该 mid 判定，
// 网关不下发任何「已过滤」结论；日志只记主键，绝不打印正文（隐私级别 P4）。
func (l *ListPmMessagesLogic) ListPmMessages(req *types.ParamPmMessages) (resp *types.PmMessagesResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	reply, err := l.svcCtx.PrivateMessage.ListMessages(l.ctx, &privatemessagerpc.ListMessagesReq{
		ConversationId: req.ConversationId,
		Mid:            req.Mid,
		CursorSeq:      req.CursorSeq,
		Ps:             req.Ps,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/listPmMessages: conversation_id=%d mid=%d cursor_seq=%d err=%v",
			req.ConversationId, req.Mid, req.CursorSeq, err)
		return nil, err
	}
	return &types.PmMessagesResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmMessagesData{
			List:          pmMessagesToAPI(reply.GetList()),
			NextCursorSeq: reply.GetNextCursorSeq(),
			HasMore:       reply.GetHasMore(),
			ReadSeq:       reply.GetReadSeq(),
		},
		TTL: 0,
	}, nil
}
