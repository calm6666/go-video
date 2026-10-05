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

type CheckHistoryPasswordLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 历史密码重复校验
func NewCheckHistoryPasswordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckHistoryPasswordLogic {
	return &CheckHistoryPasswordLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CheckHistoryPassword 聚合 account CheckHistoryPassword RPC：待校验密文可逗号分隔多个，
// result 是与入参一一对应的 0/1 命中序列；历史库比对在 account 侧完成，网关不缓存结果。
func (l *CheckHistoryPasswordLogic) CheckHistoryPassword(req *types.ParamCheckHistoryPassword) (resp *types.PassportCheckHistoryResponse, err error) {
	if l.svcCtx.Account == nil {
		return nil, errors.New("account service not configured")
	}
	reply, err := l.svcCtx.Account.CheckHistoryPassword(l.ctx, &accountrpc.CheckHistoryPwdReq{
		Mid:      req.Mid,
		Password: req.Password,
	})
	if err != nil {
		l.Errorf("gateway/app/checkHistoryPassword: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.PassportCheckHistoryResponse{
		Code:    0,
		Message: "ok",
		Data:    types.PassportCheckHistoryData{Result: reply.GetResult()},
		TTL:     0,
	}, nil
}
