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

type CoinTossConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 生效投币参数（日限/单片上限/取消窗口/初始余额；只读，后台不改这套规则）
func NewCoinTossConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinTossConfigLogic {
	return &CoinTossConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinTossConfig 转发 coin GetTossConfig（生效投币参数自述）。
//
// 与 payment 的 DescribeChannels 同一条防线：这条路由存在的唯一理由是
// 「让当前真正生效的投币规则可查询」（AGENTS.md §6 客户端不写死），所以它的价值
// 全在**逐字转达服务说了什么**：
//   - 网关不硬编码任何一位（daily_limit=5、initial_balance=5 之类），也不替缺失位补默认值：
//     一旦 payment/coin 被换成别的配置，写死的数字会让运营页永远读到旧上限，
//     「改了配置但页面还在按旧规则提示」就成了常态故障；
//   - initial_balance 原样转达：它是**沙箱便利**（新建账户送币），不是赠送规则，
//     网关不把它解释成「运营可以免费送的额度」，也不因为看起来像「白送」就报错误；
//   - 未配客户端时一律 errCoinServiceNotConfigured，**绝不**回「参数全 0」：
//     全 0 在后台会被读成「投币被关掉了」（daily_limit=0 谁也投不了），正是 §1 禁止的假成功。
//
// 本路由刻意只读：投币规则属运营配置（ops-config）域，coin 侧没有写这套参数的 RPC，
// admin.api 也因此没有配套的写路由（见其 coin 段注释），网关不在这里开第二处改口的地方。
// 请求体与 RPC 消息都是空的（ParamCoinTossConfig{}/GetTossConfigReq{}），因此没有入参
// 门槛可言；nil 判断只是与同域其它路由一致的接线护栏。
func (l *CoinTossConfigLogic) CoinTossConfig(req *types.ParamCoinTossConfig) (resp *types.CoinTossConfigResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errCoinServiceNotConfigured
	}
	if req == nil {
		return nil, errCoinRequestMissing
	}
	reply, err := l.svcCtx.Coin.GetTossConfig(l.ctx, &coinrpc.GetTossConfigReq{})
	if err != nil {
		l.Errorf("gateway/admin/coinTossConfig: err=%v", err)
		return nil, err
	}
	return &types.CoinTossConfigResponse{
		Code:    0,
		Message: "ok",
		Data:    coinTossConfigToAPI(reply),
		TTL:     0,
	}, nil
}
