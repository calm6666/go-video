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

type RenderNotifyTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 模板渲染预览（不落库，缺变量时返回 missing_vars）
func NewRenderNotifyTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenderNotifyTemplateLogic {
	return &RenderNotifyTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 渲染预览：聚合 notification RenderTemplate RPC，只读不落库、不调用供应商、不发通知。
// version=0 表示预览当前已发布版本，>0 表示预览指定版本（可以是草稿）；
// 语言必须显式传值（预览不允许落到不确定的回落语言），该判定留在服务侧。
// 契约上「模板不可用」用 missing_vars 表达（RPC 仍然成功），网关据此置 rejected=true，
// 不把它升级成 HTTP 错误——后台要拿到已渲染部分和缺失清单才能改模板；
// 若服务侧改为直接返回缺变量错误，那条错误同样原样上抛，网关两种口径都不伪造结果。
// 注意：日志与错误消息都不带 params 与渲染结果正文（可能含用户内容）。
func (l *RenderNotifyTemplateLogic) RenderNotifyTemplate(req *types.ParamRenderNotifyTemplate) (resp *types.NotifyRenderResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	operator, err := notificationOperator(l.ctx, "renderNotifyTemplate", req.Op)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("template_code", req.TemplateCode); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Notification.RenderTemplate(l.ctx, &notificationrpc.RenderTemplateReq{
		Channel:        notificationrpc.Channel(req.Channel),
		TemplateCode:   req.TemplateCode,
		Version:        req.Version,
		Language:       notificationrpc.Language(req.Language),
		TemplateParams: req.Params,
		Operator:       operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/renderNotifyTemplate: template_code=%s channel=%d language=%d version=%d params=%d operator_id=%d err=%v",
			req.TemplateCode, req.Channel, req.Language, req.Version, len(req.Params), req.Op.OperatorId, err)
		return nil, err
	}
	return &types.NotifyRenderResponse{
		Code:    0,
		Message: "ok",
		Data: types.NotifyRenderData{
			Title:       reply.GetTitle(),
			Body:        reply.GetBody(),
			Version:     reply.GetVersion(),
			Language:    int32(reply.GetLanguage()),
			MissingVars: notifyMissingVarsToAPI(reply.GetMissingVars()),
			Rejected:    len(reply.GetMissingVars()) > 0,
		},
		TTL: 0,
	}, nil
}
