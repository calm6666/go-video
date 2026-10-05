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

type LoginLogsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 本人登录记录
func NewLoginLogsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogsLogic {
	return &LoginLogsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LoginLogs 聚合 account LoginLogs RPC：只返回请求 mid 本人的记录，条数上限由 account 截断；
// 结果含来源 IP 与设备标识，属隐私数据，网关固定 ttl=0 不允许客户端缓存（AGENTS.md §5）。
func (l *LoginLogsLogic) LoginLogs(req *types.ParamLoginLogs) (resp *types.PassportLoginLogsResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.LoginLogs(l.ctx, &accountrpc.LoginLogsReq{
		Mid:   req.Mid,
		Limit: req.Limit,
	})
	if err != nil {
		l.Errorf("gateway/app/loginLogs: mid=%d limit=%d err=%v", req.Mid, req.Limit, err)
		return nil, err
	}
	return &types.PassportLoginLogsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.PassportLoginLogsData{Logs: passportLoginLogsToAPI(reply.GetLogs())},
		TTL:     0,
	}, nil
}
