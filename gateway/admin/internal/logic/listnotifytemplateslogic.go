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

type ListNotifyTemplatesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询模板（code/channel/language/state 过滤）
func NewListNotifyTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListNotifyTemplatesLogic {
	return &ListNotifyTemplatesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 模板分页查询：聚合 notification ListTemplates RPC。
// 四个过滤条件均为「0 表示不过滤」（CHANNEL/LANGUAGE/TEMPLATE_STATE_UNSPECIFIED），
// 列表含草稿与已下线版本，便于运营核对版本历史；状态含义由服务侧决定。
func (l *ListNotifyTemplatesLogic) ListNotifyTemplates(req *types.ParamListNotifyTemplates) (resp *types.NotifyTemplatesResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	pn, ps := normalizeNotificationPage(req.Pn, req.Ps)
	reply, err := l.svcCtx.Notification.ListTemplates(l.ctx, &notificationrpc.ListTemplatesReq{
		TemplateCode: req.TemplateCode,
		Channel:      notificationrpc.Channel(req.Channel),
		Language:     notificationrpc.Language(req.Language),
		State:        notificationrpc.TemplateState(req.State),
		Pn:           pn,
		Ps:           ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listNotifyTemplates: template_code=%s channel=%d language=%d state=%d pn=%d ps=%d err=%v",
			req.TemplateCode, req.Channel, req.Language, req.State, pn, ps, err)
		return nil, err
	}
	return &types.NotifyTemplatesResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyTemplatesData{
			List:  notifyTemplatesToAPI(reply.GetTemplates()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
