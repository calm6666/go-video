package logic

import (
	"context"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type DeleteWebhookLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteWebhookLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteWebhookLogic {
	return &DeleteWebhookLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 删除回调端点（抑制未投递任务）。
//
// 契约口径（proto:552-564「删除回调端点（抑制未投递任务）」+ 本方法占位注释）：
//   - 删除是「配置终态化」而不是物理删行：op_webhook_delivery.endpoint_id 的归属必须长期可解释，
//     死信复盘要看清当初是哪个地址（占位注释第 5 条）；正文清理走 PurgePayloadBefore，不靠删端点；
//   - 重复删除不是错误：SoftDelete 的条件是 deleted_at=0，第二次 applied=false，
//     整体仍回成功、deleted=false、deliveries_suppressed 回本次实际抑制数（第 4 条）；
//   - reason 必填（proto:557）：配置删除是运营排障线索，delete_reason 列不允许空串。
//
// 错误映射：ErrInvalidAppID/ErrAppNotFound/errReasonRequired/errReasonTooLong→InvalidArgument；
// ErrOwnerRequired→PermissionDenied；ErrWebhookNotFound→NotFound；SQL 失败→Internal。
func (l *DeleteWebhookLogic) DeleteWebhook(in *rpc.DeleteWebhookReq) (*rpc.DeleteWebhookReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 参数与身份先于任何读与写。endpoint_id<=0 与「不存在/跨应用」同口径回 ErrWebhookNotFound，
	//    不给枚举他人 endpoint_id 留可分辨信号。
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	if err := requireOwnerOrOperator(app, in.OperatorMid, in.IsOperator); err != nil {
		return nil, err
	}
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	endpoint, err := webhookEndpointOfApp(ctx, s, app.AppID, in.EndpointId)
	if err != nil {
		return nil, err
	}

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// 2. 事务边界：先封端点、再抑制在途任务，两步原子生效。
	//    顺序刻意为「端点先行」：中间失败最坏是端点已停但任务未抑制，
	//    下一轮投递时这些任务因端点 Deliverable()=false 被跳过；
	//    反过来（任务未停而端点还在）等于留下一个已下线地址继续被外呼。
	//    SuppressByEndpoint 只吃 pending/delivering/retry_scheduled → ignored，
	//    已成功的投递与已死信的记录不动（结论是历史事实，不能被删除操作改写）。
	now := nowUnix()
	var (
		deleted    bool
		suppressed int64
	)
	err = s.DB.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		applied, err := s.WebhookEndpoints.SoftDelete(tctx, session, endpoint.EndpointID, in.OperatorMid, reason, now)
		if err != nil {
			return err
		}
		deleted = applied
		// 第二次删除也必须跑一次抑制：占位注释第 3 条承认存在「端点已停但任务未抑制」的
		// 中间窗口，本调用对该窗口是收敛的（条件 UPDATE，影响 0 行时不产生任何变更）。
		n, err := s.WebhookDeliveries.SuppressByEndpoint(tctx, session, endpoint.EndpointID,
			"webhook endpoint deleted", now)
		if err != nil {
			return err
		}
		suppressed = n
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 3. 审计：日志记标识与实际抑制条数，不回显 URL 的查询串。
	logx.WithContext(ctx).Infof("open-platform: 回调端点删除 endpoint_id=%d app_id=%d event_type=%d host=%s "+
		"deleted=%t deliveries_suppressed=%d operator_mid=%d is_operator=%t reason=%q",
		endpoint.EndpointID, app.AppID, endpoint.EventType, webhookURLHostForLog(endpoint.URL),
		deleted, suppressed, in.OperatorMid, in.IsOperator, reason)

	return &rpc.DeleteWebhookReply{
		Deleted:              deleted,
		DeliveriesSuppressed: int32(suppressed),
	}, nil
}
