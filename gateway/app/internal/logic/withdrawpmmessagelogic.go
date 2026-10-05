// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type WithdrawPmMessageLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 撤回消息（终端仅自助撤回，服务侧校验窗口与授权）
func NewWithdrawPmMessageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WithdrawPmMessageLogic {
	return &WithdrawPmMessageLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WithdrawPmMessage 终端入口的 source 白名单是 {1 发送者, 2 接收方}：
// 审核撤回（3）与运营撤回（4）只能由服务侧内部入口写入，网关不下发 audit_task_id。
// 这里只是入口口径约束（等同 deleteDanmaku 固定 admin=false），
// 真正的「是否本人、是否在窗口内」仍由 private-message 判定，网关不伪造撤回结果。
func (l *WithdrawPmMessageLogic) WithdrawPmMessage(req *types.ParamPmWithdraw) (resp *types.PmWithdrawResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	if req.Source != int32(privatemessagerpc.WithdrawSource_WITHDRAW_SOURCE_SENDER) &&
		req.Source != int32(privatemessagerpc.WithdrawSource_WITHDRAW_SOURCE_RECEIVER) {
		return nil, errors.New("source 只允许 1（发送者）或 2（接收方），系统撤回请走运营/审核入口")
	}
	reply, err := l.svcCtx.PrivateMessage.WithdrawMessage(l.ctx, &privatemessagerpc.WithdrawMessageReq{
		MsgId:       req.MsgId,
		OperatorMid: req.OperatorMid,
		Source:      privatemessagerpc.WithdrawSource(req.Source),
		Reason:      req.Reason,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/withdrawPmMessage: msg_id=%d operator_mid=%d source=%d err=%v",
			req.MsgId, req.OperatorMid, req.Source, err)
		return nil, err
	}
	return &types.PmWithdrawResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmWithdrawData{
			MsgId:        reply.GetMsgId(),
			State:        reply.GetState(),
			Withdrawn:    reply.GetWithdrawn(),
			WithdrawTime: reply.GetWithdrawTime(),
		},
		TTL: 0,
	}, nil
}
