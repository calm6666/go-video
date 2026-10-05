package logic

import (
	"context"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DescribeChannelsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDescribeChannelsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DescribeChannelsLogic {
	return &DescribeChannelsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// sandboxChannelNote 是给运营页显式标注用的文案：沙箱不是「还没接好」的渠道，
// 而是本项目的既定语义——只推进本地台账，不产生真实资金移动。
// 这段文字是契约的一部分，不得省略或改成含糊表述。
const sandboxChannelNote = "沙箱台账渠道：只写入本服务 pm_recharge/pm_payment/pm_flow 台账，" +
	"不请求任何第三方支付网关，不产生真实资金移动；" +
	"渠道回调验签、原路退回银行卡、提现、打款出金、对账文件、发票税务均未配置且不开接口。"

// DescribeChannels 渠道能力自述。
//
// 把「只有沙箱、没有真实资金」做成可查询的事实，而不是只写在 README 里：
//   - sandbox_only 恒为 true（PayChannel 枚举里只有 SANDBOX 一个可用值）；
//   - channels[].real_money 恒为 false，前端必须据此标注「非真实资金」；
//   - enabled 反映 Payment.AllowedChannels 配置，运营把沙箱关掉时这里就是 false，
//     调用侧不必等到写入失败才知道渠道不可用。
func (l *DescribeChannelsLogic) DescribeChannels(in *rpc.DescribeChannelsReq) (*rpc.DescribeChannelsReply, error) {
	cfg := l.svcCtx.Config.Payment
	enabled := cfg.AllowsChannel(channelName(rpc.PayChannel_PAY_CHANNEL_SANDBOX))

	return &rpc.DescribeChannelsReply{
		SandboxOnly: true,
		Channels: []*rpc.ChannelState{{
			Channel:   rpc.PayChannel_PAY_CHANNEL_SANDBOX,
			Enabled:   enabled,
			RealMoney: false,
			Note:      sandboxChannelNote,
		}},
		CurrencyDefault: cfg.NormalizeCurrency(""),
	}, nil
}
