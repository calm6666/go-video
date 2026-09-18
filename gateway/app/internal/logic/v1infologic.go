// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type V1InfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// v1 老客户端：查询用户基础信息
func NewV1InfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *V1InfoLogic {
	return &V1InfoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// v1 老客户端：查询用户基础信息（Card3 → V1Info 字段转换）。
func (l *V1InfoLogic) V1Info(req *types.ParamMid) (resp *types.V1InfoResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.Card3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/v1Info: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.V1InfoResponse{
		Code:    0,
		Message: "ok",
		Data:    toV1Info(reply.GetCard()),
		TTL:     0,
	}, nil
}
