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

type MarkPmReadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 前移已读游标（幂等，只前进不回退）
func NewMarkPmReadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MarkPmReadLogic {
	return &MarkPmReadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MarkPmRead 已读是「每会话每成员一行游标」而不是逐条回执表，回退请求由服务返回
// 当前游标且 changed=false；网关不本地累加未读数，也不把 changed=false 改写为失败。
func (l *MarkPmReadLogic) MarkPmRead(req *types.ParamPmMarkRead) (resp *types.PmMarkReadResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	reply, err := l.svcCtx.PrivateMessage.MarkRead(l.ctx, &privatemessagerpc.MarkReadReq{
		ConversationId: req.ConversationId,
		Mid:            req.Mid,
		ReadSeq:        req.ReadSeq,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/markPmRead: conversation_id=%d mid=%d read_seq=%d err=%v",
			req.ConversationId, req.Mid, req.ReadSeq, err)
		return nil, err
	}
	return &types.PmMarkReadResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmMarkReadData{
			ReadSeq:     reply.GetReadSeq(),
			Changed:     reply.GetChanged(),
			UnreadCount: reply.GetUnreadCount(),
		},
		TTL: 0,
	}, nil
}
