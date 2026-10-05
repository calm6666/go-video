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

type GetOrCreatePmConversationLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按对方 mid 定位或创建单聊会话（pair_key 幂等）
func NewGetOrCreatePmConversationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetOrCreatePmConversationLogic {
	return &GetOrCreatePmConversationLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetOrCreatePmConversation 幂等性由服务侧 pair_key 唯一索引保证（min:max 规范化排序），
// 网关不生成也不校验会话主键，重复调用返回同一 conversation_id。
func (l *GetOrCreatePmConversationLogic) GetOrCreatePmConversation(req *types.ParamPmConversationGet) (resp *types.PmConversationCreateResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	reply, err := l.svcCtx.PrivateMessage.GetOrCreateConversation(l.ctx, &privatemessagerpc.GetOrCreateConversationReq{
		Mid:     req.Mid,
		PeerMid: req.PeerMid,
		TraceId: req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/getOrCreatePmConversation: mid=%d peer_mid=%d err=%v", req.Mid, req.PeerMid, err)
		return nil, err
	}
	return &types.PmConversationCreateResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmConversationCreateData{
			ConversationId: reply.GetConversationId(),
			Created:        reply.GetCreated(),
			State:          reply.GetState(),
			Ctime:          reply.GetCtime(),
		},
		TTL: 0,
	}, nil
}
