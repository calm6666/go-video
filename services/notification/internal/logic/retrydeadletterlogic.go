package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/svc"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

type RetryDeadLetterLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryDeadLetterLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryDeadLetterLogic {
	return &RetryDeadLetterLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营侧重投死信（按 operator 记审计，幂等）。
//
// 两类死信的重投方式不同，本方法都要求先核对来源，绝不“看起来重投成功”：
//   - source=delivery：渲染变量与收件人仍在 notification_delivery 里，把任务从 dead_letter
//     复位成 pending（带源状态守卫），由投递调度器按退避重新投递；
//   - source=event：死信表按隐私约束只留摘要，重放依赖 notification_consumer_offset.payload_json
//     里的原始信封；信封缺失（例如报文根本无法解析）时返回显式错误，需生产者重投 Outbox。
//
// 幂等：先重投、再把死信置为 retried（同样带源状态守卫）。
// 反过来的话，标记成功但重投失败就再也无法通过本接口恢复。
// 重复调用会命中 biz_key / event_id 唯一键，不会二次触达用户。
func (l *RetryDeadLetterLogic) RetryDeadLetter(in *rpc.RetryDeadLetterReq) (*rpc.RetryDeadLetterReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil request")
	}
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" {
		return nil, ErrOperatorRequired
	}
	if in.GetId() <= 0 {
		return nil, errors.New("notification/logic: dead letter id is required")
	}
	repo := l.svcCtx.Repository
	dl, err := repo.FindDeadLetter(l.ctx, in.GetId())
	if err != nil {
		return nil, err
	}
	if dl == nil {
		return nil, fmt.Errorf("%w: dead letter id=%d", model.ErrNotFound, in.GetId())
	}
	if dl.State != model.DeadLetterStatePending {
		return nil, fmt.Errorf("%w: id=%d state=%d", ErrDeadLetterHandled, dl.Id, dl.State)
	}

	var (
		deliveryIDs []string
		message     string
	)
	switch dl.Source {
	case model.DeadLetterSourceDelivery:
		if dl.DeliveryId == "" {
			return nil, fmt.Errorf("notification/logic: 死信 id=%d 缺少 delivery_id，无法重投", dl.Id)
		}
		ok, rerr := repo.ResetDeliveryForRetry(l.ctx, dl.DeliveryId)
		if rerr != nil {
			return nil, rerr
		}
		if !ok {
			return nil, fmt.Errorf("%w: delivery=%s 不处于 dead_letter 状态",
				model.ErrIllegalStateTransition, dl.DeliveryId)
		}
		deliveryIDs = []string{dl.DeliveryId}
		message = "投递任务已复位为待投递，由投递调度器按退避重新发送"
	case model.DeadLetterSourceEvent:
		if dl.EventId == "" {
			return nil, errors.New("notification/logic: 事件死信缺少 event_id，无法重放")
		}
		if l.svcCtx.Events == nil {
			return nil, errors.New("notification/logic: 事件处理器未装配，无法重放事件")
		}
		if err := l.svcCtx.Events.ReplayEvent(l.ctx, dl.EventId); err != nil {
			if errors.Is(err, model.ErrNotFound) {
				return nil, fmt.Errorf("%w: event=%s", ErrEventPayloadMissing, dl.EventId)
			}
			return nil, err
		}
		rows, lerr := repo.ListDeliveriesBySourceEvent(l.ctx, dl.EventId, []int32{
			model.DeliveryStatePending, model.DeliveryStateRetry, model.DeliveryStateSent,
		})
		if lerr != nil {
			return nil, lerr
		}
		deliveryIDs = make([]string, 0, len(rows))
		for _, r := range rows {
			deliveryIDs = append(deliveryIDs, r.DeliveryId)
		}
		message = "事件信封已按 notification_consumer_offset.payload_json 重放"
	default:
		return nil, fmt.Errorf("notification/logic: 未知死信来源 %q", dl.Source)
	}

	marked, merr := repo.MarkDeadLetterState(l.ctx, dl.Id, model.DeadLetterStateRetried, operator,
		[]int32{model.DeadLetterStatePending})
	if merr != nil {
		return nil, merr
	}
	if !marked {
		// 并发下别的运营账号已经处置过：重投本身是幂等的，这里只回报事实。
		message += "（死信已被其他操作人处置）"
	}
	l.Infof("notification/RetryDeadLetter id=%d source=%s operator=%s deliveries=%d",
		dl.Id, dl.Source, operator, len(deliveryIDs))
	return &rpc.RetryDeadLetterReply{
		DeliveryIds: deliveryIDs,
		Retried:     int32(len(deliveryIDs)),
		Message:     message,
	}, nil
}
