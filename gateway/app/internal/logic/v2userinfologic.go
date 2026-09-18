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

type V2UserInfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// v2 查询我的资料（userinfo 别名）
func NewV2UserInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *V2UserInfoLogic {
	return &V2UserInfoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// v2 查询我的资料（userinfo 别名，与 /myinfo 行为一致）。
func (l *V2UserInfoLogic) V2UserInfo(req *types.ParamMid) (resp *types.V2MyInfoResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.ProfileWithStat3(l.ctx, &accountrpc.MidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/v2UserInfo: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.V2MyInfoResponse{
		Code:    0,
		Message: "ok",
		Data:    toV2MyInfo(reply),
		TTL:     0,
	}, nil
}
