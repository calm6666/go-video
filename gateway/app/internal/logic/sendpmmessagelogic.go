// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SendPmMessageLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发送私信（client_msg_id 幂等键透传）
func NewSendPmMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SendPmMessageLogic {
	return &SendPmMessageLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SendPmMessage 只负责把幂等键与载体原样交给服务侧：
// 门禁顺序（黑名单/接收范围 → 风控 CheckAction → 长度与敏感预检 → 落库 → 送审 → 游标推进）
// 全在 private-message 内实现（AGENTS.md §5/§8），网关不做任何内容判断。
// client_msg_id 是 (sender_mid, client_msg_id) 唯一键的组成部分，空值会让重试无法去重，
// 因此按传输必填字段在此拒绝；replayed 由服务给出，网关不推断。
// 日志只记主键与状态，禁止打印 content（隐私级别 P4）。
func (l *SendPmMessageLogic) SendPmMessage(req *types.ParamPmSend) (resp *types.PmSendResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	if strings.TrimSpace(req.ClientMsgId) == "" {
		return nil, errors.New("client_msg_id 必填：缺少幂等键的重试会产生重复消息")
	}
	reply, err := l.svcCtx.PrivateMessage.SendMessage(l.ctx, &privatemessagerpc.SendMessageReq{
		Mid:            req.Mid,
		ConversationId: req.ConversationId,
		PeerMid:        req.PeerMid,
		MsgType:        privatemessagerpc.MsgType(req.MsgType),
		Content:        req.Content,
		MediaRef:       req.MediaRef,
		ClientMsgId:    req.ClientMsgId,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/sendPmMessage: mid=%d conversation_id=%d peer_mid=%d client_msg_id=%q err=%v",
			req.Mid, req.ConversationId, req.PeerMid, req.ClientMsgId, err)
		return nil, err
	}
	return &types.PmSendResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmSendData{
			MsgId:          reply.GetMsgId(),
			ConversationId: reply.GetConversationId(),
			Seq:            reply.GetSeq(),
			State:          reply.GetState(),
			Ctime:          reply.GetCtime(),
			Replayed:       reply.GetReplayed(),
			AuditTaskId:    reply.GetAuditTaskId(),
			Preview:        reply.GetPreview(),
		},
		TTL: 0,
	}, nil
}
