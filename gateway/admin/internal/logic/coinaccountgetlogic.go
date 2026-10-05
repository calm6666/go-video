// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	coinrpc "go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CoinAccountGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 硬币账户（含今日额度；无账户时 found=false 且余额 0）
func NewCoinAccountGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinAccountGetLogic {
	return &CoinAccountGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinAccountGet 转发 coin GetCoinAccount（硬币账户，含今日额度）。
//
// 只读路由，不挂 AdminPermission；本域与 payment 的现金余额是两套账，这里读到的
// balance 是**硬币**，网关不出现任何与 *_minor 相加或折算的字段（§1）。
//
// found 语义照契约转达，两端都不美化（proto 注释锁定）：
//   - found=false 是「这个人从未有过硬币账户」的真实结论，不是错误，也不是「余额被扣光」；
//   - 读到 error（含 DB 失败）时必须上抛：服务注释写明「折叠成 found=false 会让客户端
//     显示余额 0，用户以为币被扣光了，比报错严重得多」——网关这一层正是那句话的落点；
//   - 本路由刻意不建仓：读一次就凭空送一笔初始币会把发行量搞失控，网关也不提供
//     「顺便初始化账户」的口子（那属 /grant）。
//
// found=false 时服务仍会回一份带三项限额的账户（限额来自生效配置，不是账户行），
// 因此 today_limit/per_target_limit/cancel_window_seconds 依然如实投影，不整行清零。
// 网关只挡 mid 必须为正（服务对 mid<=0 回 ErrInvalidMid，硬币账户没有游客号）。
func (l *CoinAccountGetLogic) CoinAccountGet(req *types.ParamCoinAccountGet) (resp *types.CoinAccountGetResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errCoinServiceNotConfigured
	}
	if req == nil {
		return nil, errCoinRequestMissing
	}
	if err := coinPositive("mid", req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.GetCoinAccount(l.ctx, &coinrpc.GetCoinAccountReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/admin/coinAccountGet: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.CoinAccountGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinAccountGetData{
			Found:   reply.GetFound(),
			Account: coinAccountToAPI(reply.GetAccount()),
		},
		TTL: 0,
	}, nil
}
