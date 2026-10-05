package logic

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/svc"
	"go-video/services/notification/rpc"
)

type SendNotificationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSendNotificationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SendNotificationLogic {
	return &SendNotificationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 投递通知：模板渲染 -> 频次/免打扰校验 -> 落投递任务（biz_key 幂等）-> 按配置同步或异步投递
//
// 校验与幂等口径全部在 internal/send 用例里（与 notification.request.v1 事件入口共用），
// 这里只负责 gRPC 上下文与 SyncSend 的“落库后立即投一次”。
// 返回的 deliveries 表示“任务已 durable 落库”，不等于用户已收到：
// 真正的供应商回执由 Dispatcher 异步写回，调用方用 GetDeliveryStatus 轮询。
func (l *SendNotificationLogic) SendNotification(in *rpc.SendNotificationReq) (*rpc.SendNotificationReply, error) {
	res, err := l.svcCtx.Enqueuer.Enqueue(l.ctx, in)
	if err != nil {
		l.Errorf("notification/SendNotification 拒绝请求 channel=%d template=%s err=%v",
			in.GetChannel(), in.GetTemplateCode(), err)
		return nil, err
	}
	if !l.svcCtx.Config.Notification.SyncSend {
		return res.Reply, nil
	}
	// SyncSend：对“本次新建且待投递”的任务立即投一次。
	// 失败不改变返回结果——任务状态已由 Dispatcher 按退避落库，绝不能因为投递失败
	// 就把已经受理的请求报成错误（那会诱导调用方换 biz_key 重发，造成重复打扰）。
	d := l.svcCtx.Dispatcher
	if d == nil {
		l.Errorf("notification/SendNotification SyncSend=true 但 DispatcherEnabled=false，任务将由下次扫描处理 delivery_ids=%d 条",
			len(res.NewDeliveryIDs))
		return res.Reply, nil
	}
	for _, id := range res.NewDeliveryIDs {
		if derr := d.Dispatch(l.ctx, id); derr != nil {
			l.Errorf("notification/SendNotification 同步投递失败 delivery_id=%s err=%v", id, derr)
		}
	}
	return res.Reply, nil
}
