// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	accountrpc "go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LoginLogLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询用户登录日志
func NewLoginLogLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogLogic {
	return &LoginLogLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询用户登录日志：聚合 account LoginLogs RPC（参考 passport RPC.LoginLogs
// 与 account-interface /x/member/web/login/log）。
func (l *LoginLogLogic) LoginLog(req *types.ParamLoginLog) (resp *types.LoginLogsResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.LoginLogs(l.ctx, &accountrpc.LoginLogsReq{Mid: req.Mid, Limit: req.Limit})
	if err != nil {
		l.Errorf("gateway/admin/loginLog: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	logs := make([]types.LoginLogItem, 0, len(reply.GetLogs()))
	for _, item := range reply.GetLogs() {
		logs = append(logs, types.LoginLogItem{
			Mid:       item.GetMid(),
			IP:        item.GetIp(),
			TS:        item.GetTs(),
			LoginType: item.GetLoginType(),
			Status:    item.GetStatus(),
			Reason:    item.GetReason(),
			Device:    item.GetDevice(),
		})
	}
	return &types.LoginLogsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.LoginLogsData{Logs: logs},
		TTL:     0,
	}, nil
}
