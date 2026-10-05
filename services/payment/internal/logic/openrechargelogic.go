package logic

import (
	"context"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpenRechargeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOpenRechargeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenRechargeLogic {
	return &OpenRechargeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// OpenRecharge 开充值单（只落 PENDING，钱还没到账）。
//
// 判定口径：
//   - 金额必须落在 [Payment.MinRechargeMinor, Payment.MaxRechargeMinor]，单位是分；
//   - 渠道只接受 SANDBOX。给别的枚举值（含 UNSPECIFIED）是非法渠道，直接拒绝，
//     绝不「看不见就当沙箱处理」；SANDBOX 合法但被配置关掉时返回 not-configured；
//   - request_id 是幂等键：命中重放返回首单并置 duplicated=true，不重复建单；
//     并发下靠 uniq_request_id 兜住，插失败后回查同样按重放返回；
//   - 这一步不动余额、不写流水，入账只发生在 SettleSandboxRecharge。
func (l *OpenRechargeLogic) OpenRecharge(in *rpc.OpenRechargeReq) (*rpc.OpenRechargeReply, error) {
	cfg := l.svcCtx.Config.Payment
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if !cfg.RechargeAmountAllowed(in.AmountMinor) {
		return nil, model.ErrRechargeAmountOutOfRange
	}
	currency, err := resolveCurrency(cfg, in.Currency)
	if err != nil {
		return nil, err
	}
	if err := requireRequestID(in.RequestId); err != nil {
		return nil, err
	}
	if err := requireMaxLength("client_trace_id", in.ClientTraceId, 64); err != nil {
		return nil, err
	}
	if in.Channel != rpc.PayChannel_PAY_CHANNEL_SANDBOX {
		return nil, model.ErrDetail(model.ErrInvalidChannel, "channel="+in.Channel.String())
	}
	if !cfg.AllowsChannel(channelName(in.Channel)) {
		return nil, model.ErrChannelNotConfigured
	}

	recharges := l.svcCtx.Models.Recharge
	// 幂等重放：同一 request_id 必须回到同一张单。
	existing, err := recharges.FindByRequestID(l.ctx, in.RequestId)
	if err != nil {
		l.Errorf("payment/OpenRecharge: replay lookup request_id=%s err=%v", in.RequestId, err)
		return nil, err
	}
	if existing != nil {
		return &rpc.OpenRechargeReply{Duplicated: true, Recharge: rechargeInfo(existing)}, nil
	}

	rechargeNo, err := newDocumentNo(prefixRecharge)
	if err != nil {
		return nil, err
	}
	row := &model.Recharge{
		RechargeNo:    rechargeNo,
		RequestId:     in.RequestId,
		Mid:           in.Mid,
		AmountMinor:   in.AmountMinor,
		Currency:      currency,
		Channel:       model.ChannelSandbox,
		State:         model.RechargeStatePending,
		ClientTraceId: in.ClientTraceId,
	}
	if _, err := recharges.Insert(l.ctx, row); err != nil {
		if recharges.IsDuplicate(err) {
			// 并发重复建单：回查命中就按重放返回，查不到则原样上抛，不猜。
			concurrent, lookupErr := recharges.FindByRequestID(l.ctx, in.RequestId)
			if lookupErr == nil && concurrent != nil {
				l.Infof("payment/OpenRecharge: duplicate key replayed request_id=%s recharge_no=%s", in.RequestId, concurrent.RechargeNo)
				return &rpc.OpenRechargeReply{Duplicated: true, Recharge: rechargeInfo(concurrent)}, nil
			}
		}
		l.Errorf("payment/OpenRecharge: insert mid=%d request_id=%s err=%v", in.Mid, in.RequestId, err)
		return nil, err
	}
	return &rpc.OpenRechargeReply{Recharge: rechargeInfo(row)}, nil
}
