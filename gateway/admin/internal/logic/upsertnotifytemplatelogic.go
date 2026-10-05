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

type UpsertNotifyTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新增/更新通知模板（publish=false 存草稿，true 直接发布新版本）
func NewUpsertNotifyTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertNotifyTemplateLogic {
	return &UpsertNotifyTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 模板新增/更新：聚合 notification UpsertTemplate RPC。
// publish=false 只留草稿版本（不可用于投递），true 直接把这条草稿发布出去；
// 同一 (code, channel, language) 的草稿复用与版本号递增由服务侧完成，网关不传也不推算 version。
// 模板正文合法性（变量语法、长度、控制字符）由 notification 判定，网关只挡缺主体/缺模板码。
func (l *UpsertNotifyTemplateLogic) UpsertNotifyTemplate(req *types.ParamUpsertNotifyTemplate) (resp *types.NotifyTemplateResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	operator, err := notificationOperator(l.ctx, "upsertNotifyTemplate", req.Op)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("template_code", req.TemplateCode); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Notification.UpsertTemplate(l.ctx, &notificationrpc.UpsertTemplateReq{
		TemplateCode: req.TemplateCode,
		Channel:      notificationrpc.Channel(req.Channel),
		Language:     notificationrpc.Language(req.Language),
		TitleTpl:     req.TitleTpl,
		BodyTpl:      req.BodyTpl,
		Operator:     operator,
		Publish:      req.Publish,
	})
	if err != nil {
		// 不打印 title_tpl/body_tpl：模板正文可能含用户内容（AGENTS.md §7 隐私约束）。
		l.Errorf("gateway/admin/upsertNotifyTemplate: template_code=%s channel=%d language=%d publish=%v operator_id=%d err=%v",
			req.TemplateCode, req.Channel, req.Language, req.Publish, req.Op.OperatorId, err)
		return nil, err
	}
	return &types.NotifyTemplateResponse{
		Code:    0,
		Message: "ok",
		Data:    types.NotifyTemplateData{Template: notifyTemplateToAPI(reply.GetTemplate())},
		TTL:     0,
	}, nil
}
