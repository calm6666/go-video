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

type V1CardLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// v1 老客户端：查询用户名片
func NewV1CardLogic(ctx context.Context, svcCtx *svc.ServiceContext) *V1CardLogic {
	return &V1CardLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// v1 老客户端：查询用户名片（ProfileWithStat3 → V1Card 字段转换）。
func (l *V1CardLogic) V1Card(req *types.ParamMid) (resp *types.V1CardResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.ProfileWithStat3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/v1Card: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.V1CardResponse{
		Code:    0,
		Message: "ok",
		Data:    toV1Card(reply),
		TTL:     0,
	}, nil
}
