// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PrivateMessageReportHandleLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 处置举报（驳回/撤回/转处罚/升级人审；重复提交回首次结论，replayed=true）
func NewPrivateMessageReportHandleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrivateMessageReportHandleLogic {
	return &PrivateMessageReportHandleLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PrivateMessageReportHandle 聚合 private-message HandleReport。
//
// 权限点 pm:report:handle：处置会把举报推到终态，PUNISH 还会把人交给 risk-control，
// 而 DISMISS 会让一条真实举报静默消失——两种误操作都需要单独收回，不给「读台账」顺带「点处置」。
//
// 网关只挡形状与必填：report_id>0、action≠UNSPECIFIED、handler>0、idempotency_key 非空。
// 动作集合是否合法、PENDING 之外能否再处置、能否连带撤回（撤回窗口与消息状态）全部由服务判定，
// 网关不预判「这个动作现在能不能点」——那需要读举报与消息状态，等于把业务规则抄进网关（AGENTS.md §5）。
//
// withdraw_message=true 时撤回的是**被举报的那条消息**，与本次处置结论是两件事：
// 服务会把撤回动作单独记成 source=ADMIN 的撤回审计，网关只如实回显 withdraw_msg_id。
func (l *PrivateMessageReportHandleLogic) PrivateMessageReportHandle(req *types.ParamPrivateMessageReportHandle) (resp *types.PrivateMessageReportHandleResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errPMServiceNotConfigured
	}
	if req == nil {
		return nil, errPMRequestMissing
	}
	if err := pmSessionGate(l.ctx, "privateMessageReportHandle", req.Handler); err != nil {
		return nil, err
	}
	if err := requireOperator("handler", req.Handler); err != nil {
		return nil, err
	}
	if err := pmNonNeg("report_id", req.ReportId); err != nil {
		return nil, err
	}
	if req.ReportId == 0 {
		return nil, errReportIDRequired
	}
	if err := pmEnum("action", req.Action); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.PrivateMessage.HandleReport(l.ctx, &privatemessagerpc.HandleReportReq{
		ReportId:        req.ReportId,
		Action:          privatemessagerpc.ReportAction(req.Action),
		Handler:         req.Handler,
		Note:            req.Note,
		WithdrawMessage: req.WithdrawMessage,
		IdempotencyKey:  req.IdempotencyKey,
		TraceId:         req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/privateMessageReportHandle: report_id=%d action=%d handler=%d withdraw=%t idempotency_key=%s err=%v",
			req.ReportId, req.Action, req.Handler, req.WithdrawMessage, req.IdempotencyKey, err)
		return nil, err
	}
	return &types.PrivateMessageReportHandleResponse{
		Code:    0,
		Message: "ok",
		Data: types.PrivateMessageReportHandleData{
			ReportId:      reply.GetReportId(),
			State:         reply.GetState(),
			Replayed:      reply.GetReplayed(),
			WithdrawMsgId: reply.GetWithdrawMsgId(),
		},
		TTL: 0,
	}, nil
}
