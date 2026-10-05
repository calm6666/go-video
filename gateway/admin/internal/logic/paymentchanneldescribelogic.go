// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	paymentrpc "go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PaymentChannelDescribeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 渠道能力自述：sandbox_only 与每渠道 real_money（恒 false），让「没有真实资金」可查询
func NewPaymentChannelDescribeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PaymentChannelDescribeLogic {
	return &PaymentChannelDescribeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PaymentChannelDescribe 转发 payment DescribeChannels（渠道能力自述）。
//
// 这条路由存在的唯一理由是「让『只有沙箱、没有真实资金』成为可查询的事实」（AGENTS.md §1），
// 所以它的价值全在**逐字转达服务说了什么**：
//   - 网关不硬编码 sandbox_only=true，也不替任何渠道补条目：那样一来即使 payment 被
//     换成了真接渠道的实现，后台也会永远读到「没有真实资金」，这条防线就成了假象；
//   - real_money/enabled/note 一位都不改：enabled 反映 Payment.AllowsChannel 配置，
//     运营把沙箱关掉时这里就是 false，调用方不必等到写入失败（ErrChannelNotConfigured）
//     才知道渠道不可用；
//   - 未配客户端时一律 errPaymentServiceNotConfigured，**绝不**返回「渠道列表为空」：
//     空列表在后台会被读成「渠道都健康，只是没有额外渠道」，正是 §1 禁止的假成功。
//
// 请求体与 RPC 消息都是空的（ParamPaymentChannelDescribe{}/DescribeChannelsReq{}），
// 因此没有入参门槛可言；nil 判断只是与同域其它路由一致的接线护栏。
// 本路由刻意不挂 AdminPermission：排障时可能还没有权限数据（见 admin.api 只读面注释）。
func (l *PaymentChannelDescribeLogic) PaymentChannelDescribe(req *types.ParamPaymentChannelDescribe) (resp *types.PaymentChannelDescribeResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errPaymentServiceNotConfigured
	}
	if req == nil {
		return nil, errPaymentRequestMissing
	}
	reply, err := l.svcCtx.Payment.DescribeChannels(l.ctx, &paymentrpc.DescribeChannelsReq{})
	if err != nil {
		l.Errorf("gateway/admin/paymentChannelDescribe: err=%v", err)
		return nil, err
	}
	return &types.PaymentChannelDescribeResponse{
		Code:    0,
		Message: "ok",
		Data: types.PaymentChannelDescribeData{
			SandboxOnly:     reply.GetSandboxOnly(),
			Channels:        paymentChannelsToAPI(reply.GetChannels()),
			CurrencyDefault: reply.GetCurrencyDefault(),
		},
		TTL: 0,
	}, nil
}
