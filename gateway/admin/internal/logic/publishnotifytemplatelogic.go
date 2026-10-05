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

type PublishNotifyTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发布指定草稿版本（operator 必填，写审计）
func NewPublishNotifyTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PublishNotifyTemplateLogic {
	return &PublishNotifyTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 发布草稿版本：聚合 notification PublishTemplate RPC。
// 发布语义由服务侧保证（同 code/channel/language 的旧已发布版本转下线，非草稿版本再发布被拒），
// 网关只做「有主体、有模板码、有版本号」的门槛校验，不判定状态机是否允许迁移。
func (l *PublishNotifyTemplateLogic) PublishNotifyTemplate(req *types.ParamPublishNotifyTemplate) (resp *types.NotifyTemplateResponse, err error) {
	if l.svcCtx.Notification == nil {
		return nil, errors.New("notification service not configured")
	}
	operator, err := notificationOperator(l.ctx, "publishNotifyTemplate", req.Op)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("template_code", req.TemplateCode); err != nil {
		return nil, err
	}
	if req.Version <= 0 {
		return nil, errors.New("gateway/admin: version must be > 0")
	}
	reply, err := l.svcCtx.Notification.PublishTemplate(l.ctx, &notificationrpc.PublishTemplateReq{
		TemplateCode: req.TemplateCode,
		Channel:      notificationrpc.Channel(req.Channel),
		Language:     notificationrpc.Language(req.Language),
		Version:      req.Version,
		Operator:     operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/publishNotifyTemplate: template_code=%s channel=%d language=%d version=%d operator_id=%d err=%v",
			req.TemplateCode, req.Channel, req.Language, req.Version, req.Op.OperatorId, err)
		return nil, err
	}
	return &types.NotifyTemplateResponse{
		Code:    0,
		Message: "ok",
		Data:    types.NotifyTemplateData{Template: notifyTemplateToAPI(reply.GetTemplate())},
		TTL:     0,
	}, nil
}
