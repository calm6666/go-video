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

type ListNotifyDeadLettersLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询死信（事件 ID/状态/topic 过滤）
func NewListNotifyDeadLettersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListNotifyDeadLettersLogic {
	return &ListNotifyDeadLettersLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 死信分页查询：聚合 notification ListDeadLetters RPC。
// 死信表按隐私约束只留 payload_digest（原始报文不落库），因此后台看到的是摘要与原因；
// 能否重投（来源是投递任务还是事件信封、是否已处置）在 RetryDeadLetter 里由服务侧判定。
func (l *ListNotifyDeadLettersLogic) ListNotifyDeadLetters(req *types.ParamListNotifyDeadLetters) (resp *types.NotifyDeadLettersResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	pn, ps := normalizeNotificationPage(req.Pn, req.Ps)
	reply, err := l.svcCtx.Notification.ListDeadLetters(l.ctx, &notificationrpc.ListDeadLettersReq{
		EventId: req.EventId,
		State:   notificationrpc.DeadLetterState(req.State),
		Topic:   req.Topic,
		Pn:      pn,
		Ps:      ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listNotifyDeadLetters: event_id=%s state=%d topic=%s pn=%d ps=%d err=%v",
			req.EventId, req.State, req.Topic, pn, ps, err)
		return nil, err
	}
	return &types.NotifyDeadLettersResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyDeadLettersData{
			List:  notifyDeadLettersToAPI(reply.GetDeadLetters()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
