package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListWebhooksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListWebhooksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListWebhooksLogic {
	return &ListWebhooksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 回调端点列表。
//
// 本方法是只读的「单应用配置视图」，不是配置导出面：它按 app_id 取数，
// 不提供跨应用枚举（proto:541-546 没有 owner/运营分支字段，归属由网关校验后注入）。
//
// 不回显的东西（一条都不能松）：
//   - 签名密钥：库里本就没有（只有 sign_key_version，见 model.WebhookEndpoint 文件头）；
//   - challenge_hash / challenge_expires_at：验证挑战的摘要属服务端验证材料，
//     rpc.WebhookEndpointInfo 里连字段都没有，投影函数也就无从泄露（helpers.projectWebhookEndpoint）。
//
// 错误映射：ErrInvalidAppID/ErrAppNotFound→InvalidArgument；SQL 失败→Internal。
func (l *ListWebhooksLogic) ListWebhooks(in *rpc.ListWebhooksReq) (*rpc.ListWebhooksReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 范围：app_id 必须是真实应用（不存在时报错而不是回空数组——
	//    空数组会被上游解释成「这个应用没配回调」，与「没有这个应用」是两种结论）。
	app, err := findApp(ctx, s, in.AppId)
	if err != nil {
		return nil, err
	}
	// 身份口径与 ListQuotaUsage 一致：operator_mid==0 是 owner 自查（网关已校验归属），
	// >0 是运营；两者读到的行集相同（都只有本应用的端点），差别只进日志，
	// 让「谁在枚举回调地址」这条审计可追溯。负数是脏参数。
	if in.OperatorMid < 0 {
		return nil, model.ErrOperatorRequired
	}

	// 2. 读取：include_disabled=false 时只回 enabled=1 且未软删的行；
	//    软删行永远不回（它们只在投递台账里解释归属），所以本方法也不是「已删配置」的查询面。
	rows, err := s.WebhookEndpoints.ListByApp(ctx, app.AppID, in.IncludeDisabled)
	if err != nil {
		return nil, err
	}

	// 3. 条数护栏：端点数本应受 MaxWebhookEndpointsPerApp 约束（RegisterWebhook 的写入闸门）。
	//    超出说明库里出现了配置之外的行（运维直写或上限被下调），此时截断并打 Error 日志，
	//    而不是静默丢弃后半段，也不放行一个无界列表。
	if max := s.Config.OpenPlatform.MaxWebhookEndpointsPerApp; max > 0 && int32(len(rows)) > max {
		logx.WithContext(ctx).Errorf("open-platform: 回调端点数异常 app_id=%d n=%d max=%d，本次响应已按上限截断",
			app.AppID, len(rows), max)
		rows = rows[:max]
	}

	// 4. 投影：ListByApp 的 SQL 已按 event_type, endpoint_id 升序，端点数在几十条量级，
	//    因此一次全量返回、不引入游标（契约里也没有 cursor 字段）。
	list := make([]*rpc.WebhookEndpointInfo, 0, len(rows))
	for _, ep := range rows {
		if info := projectWebhookEndpoint(ep); info != nil {
			// verified_at=0 原样回 0：授权页据此提示「尚未验证，不会收到推送」，
			// 绝不用 ctime 伪装成已验证。
			list = append(list, info)
		}
	}
	logx.WithContext(ctx).Infof("open-platform: 回调端点列表 app_id=%d include_disabled=%t "+
		"operator_mid=%d n=%d", app.AppID, in.IncludeDisabled, in.OperatorMid, len(list))

	return &rpc.ListWebhooksReply{List: list}, nil
}
