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

type SetPasswordLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 设置/修改密码（RSA 密文传输）
func NewSetPasswordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetPasswordLogic {
	return &SetPasswordLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SetPassword 聚合 account SetPassword RPC：RSA 密文原样透传，首次设置时旧密码留空；
// 解密、强度与历史密码策略全部在 account 服务判定，网关日志不得出现口令字段。
func (l *SetPasswordLogic) SetPassword(req *types.ParamSetPassword) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	if _, err = l.svcCtx.Account.SetPassword(l.ctx, &accountrpc.SetPasswordReq{
		Mid:         req.Mid,
		OldPassword: req.OldPassword,
		NewPassword: req.NewPassword,
		Ip:          req.IP,
	}); err != nil {
		l.Errorf("gateway/app/setPassword: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
