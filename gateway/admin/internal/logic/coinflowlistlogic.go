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

type CoinFlowListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 硬币流水台账分页（区分投币/撤币/硬币包/运营发放，正入负出）
func NewCoinFlowListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinFlowListLogic {
	return &CoinFlowListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinFlowList 转发 coin ListCoinFlows（硬币流水台账分页）。
//
// cn_flow 是 append-only 台账、余额变动的唯一可复算证据，本路由只有读。
// 需要「修正」时唯一口径是再记一条反向流水（/grant），网关不得提供改写历史行的口子，
// 也不把 delta 取绝对值后「汇总」成余额（§5 余额归 coin）。
//
// 网关只挡形状：mid/flow_type/page/size/from_ts/to_ts 非负、窗口不倒着给。
// 有界性与取值合法性全在服务判，一条都不接管：
//   - 跨用户（mid=0）既没给时间窗也没给 biz_no → ErrUnboundedLedgerQuery。
//     那句拒绝必须原样透出，**不能折叠成空台账**：服务注释写明「空列表在调用方眼里等于
//     这个人没有过任何硬币变动，把故障伪装成业务事实是对账里最坏的错误」；
//   - flow_type 非 0 时是否在合法枚举内 → validFlowType/ErrInvalidFlowType（同样报错
//     而不是空结果）；flow_type=0 是「五类都要」的合法哨兵，网关不代填也不剔除 EXPIRE
//     ——「本项目恒不出现 5」是 coin 的事实，从台账里筛掉它会让后台看不见未来写入的行；
//   - page/size=0 用服务默认页；size 越 MaxPageSize 是拒绝（ErrPageSizeTooLarge）不是裁剪。
//
// total/page/size 照抄服务回显（page 被服务归一成 1 起算，网关不复算）。
func (l *CoinFlowListLogic) CoinFlowList(req *types.ParamCoinFlowList) (resp *types.CoinFlowListResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errCoinServiceNotConfigured
	}
	if req == nil {
		return nil, errCoinRequestMissing
	}
	if err := coinNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := coinNonNeg("flow_type", int64(req.FlowType)); err != nil {
		return nil, err
	}
	if err := coinTimeWindow(req.FromTs, req.ToTs); err != nil {
		return nil, err
	}
	if err := coinNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := coinNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.ListCoinFlows(l.ctx, &coinrpc.ListCoinFlowsReq{
		Mid:      req.Mid,
		FlowType: coinrpc.CoinFlowType(req.FlowType),
		BizNo:    req.BizNo,
		FromTs:   req.FromTs,
		ToTs:     req.ToTs,
		Page:     req.Page,
		Size:     req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/coinFlowList: mid=%d flow_type=%d biz_no=%q from_ts=%d to_ts=%d page=%d size=%d err=%v",
			req.Mid, req.FlowType, req.BizNo, req.FromTs, req.ToTs, req.Page, req.Size, err)
		return nil, err
	}
	return &types.CoinFlowListResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinFlowListData{
			List:  coinFlowsToAPI(reply.GetFlows()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
