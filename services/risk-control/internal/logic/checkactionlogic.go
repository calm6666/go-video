package logic

import (
	"context"
	"time"

	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CheckActionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCheckActionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CheckActionLogic {
	return &CheckActionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 同步裁决一次受保护动作（黑名单 → 生效处罚 → 白名单 → 规则 → 降级）。
//
// 错误语义：只有入参非法（动作越界、传了明文 IP）才返回 gRPC 错误；
// 依赖故障一律走降级并返回一个带 basis/degraded 标记的可解释裁决，
// 让调用方永远拿到「一个裁决」而不是一个裸错误（AGENTS.md §9）。
func (l *CheckActionLogic) CheckAction(in *rpc.CheckActionReq) (*rpc.CheckActionReply, error) {
	input, err := checkActionInput(in)
	if err != nil {
		// 只记录脱敏维度，绝不打印 device_id / ip_hash 原值。
		l.Errorf("risk-control/CheckAction: invalid request mid=%d action=%d err=%v", in.GetMid(), in.GetAction(), err)
		return nil, err
	}

	res, err := l.svcCtx.Engine.Decide(l.ctx, input)
	if err != nil {
		l.Errorf("risk-control/CheckAction: decide failed mid=%d action=%d request_id=%s err=%v",
			input.Mid, input.Action, input.RequestID, err)
		return nil, err
	}

	if res.Degraded {
		l.Infof("risk-control/CheckAction: degraded decision mid=%d action=%d request_id=%s basis=%s decision=%d",
			input.Mid, input.Action, res.RequestID, res.Basis, res.Decision)
	}
	return decisionToReply(res, time.Now().Unix()), nil
}
