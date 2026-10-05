// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	notificationrpc "go-video/services/notification/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryNotifyDeadLetterLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重投死信（按 operator 记审计，返回新投递任务 ID）
func NewRetryNotifyDeadLetterLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryNotifyDeadLetterLogic {
	return &RetryNotifyDeadLetterLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 死信重投：聚合 notification RetryDeadLetter RPC。
// 审计主体落到下游的 operator 字符串（notification 只有这一列），因此 operator_id 必填；
// 重投按 biz_key/event_id 幂等——重复点击不会二次触达用户，已被处置的死信由服务侧显式拒绝。
// reason 只做留痕，是否可重投（来源类型、当前状态）全部由 notification 判定。
func (l *RetryNotifyDeadLetterLogic) RetryNotifyDeadLetter(req *types.ParamRetryNotifyDeadLetter) (resp *types.NotifyRetryResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	operator, err := notificationOperator(l.ctx, "retryNotifyDeadLetter", req.Op)
	if err != nil {
		return nil, err
	}
	if req.Id <= 0 {
		return nil, errors.New("gateway/admin: id required")
	}
	reply, err := l.svcCtx.Notification.RetryDeadLetter(l.ctx, &notificationrpc.RetryDeadLetterReq{
		Id:       req.Id,
		Operator: operator,
		Reason:   req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/retryNotifyDeadLetter: dead_letter_id=%d operator_id=%d reason_len=%d err=%v",
			req.Id, req.Op.OperatorId, len(req.Reason), err)
		return nil, err
	}
	return &types.NotifyRetryResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyRetryData{
			DeliveryIds: reply.GetDeliveryIds(),
			Retried:     reply.GetRetried(),
			Message:     reply.GetMessage(),
		},
		TTL: 0,
	}, nil
}
