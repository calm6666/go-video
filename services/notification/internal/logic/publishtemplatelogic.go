package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

type PublishTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPublishTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PublishTemplateLogic {
	return &PublishTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 发布指定草稿版本。
//
// 发布是单事务：同 (code, channel, lang) 的旧已发布版本转下线，保证投递时只会命中一个版本
// （model.PublishDraft 实现）。已发布/已下线的版本再次发布会被拒（ErrIllegalStateTransition）。
// 已经按旧版本落库的待投递任务不受影响——它们锁定了 template_version，按各自版本渲染。
func (l *PublishTemplateLogic) PublishTemplate(in *rpc.PublishTemplateReq) (*rpc.PublishTemplateReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil publish request")
	}
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" {
		return nil, ErrOperatorRequired
	}
	if _, err := policy.ChannelOfEnum(in.GetChannel()); err != nil {
		return nil, err
	}
	code, err := requireTemplateCode(in.GetTemplateCode())
	if err != nil {
		return nil, err
	}
	lang := policy.LangCodeOfEnum(in.GetLanguage())
	if !model.IsValidLang(lang) {
		return nil, fmt.Errorf("%w: %q", model.ErrInvalidLang, in.GetLanguage())
	}
	if in.GetVersion() <= 0 {
		return nil, errors.New("notification/logic: version must be positive to publish")
	}
	tpl, err := l.svcCtx.Repository.FindTemplateVersion(l.ctx, code, int32(in.GetChannel()), lang, in.GetVersion())
	if err != nil {
		return nil, err
	}
	if tpl == nil {
		return nil, fmt.Errorf("%w: code=%s channel=%d lang=%s version=%d",
			model.ErrTemplateNotFound, code, int32(in.GetChannel()), lang, in.GetVersion())
	}
	if tpl.State != model.TemplateStateDraft {
		return nil, fmt.Errorf("%w: template id=%d state=%d 不是草稿", model.ErrIllegalStateTransition, tpl.Id, tpl.State)
	}
	published, err := l.svcCtx.Repository.PublishTemplate(l.ctx, tpl.Id, operator)
	if err != nil {
		return nil, err
	}
	l.Infof("notification/PublishTemplate 发布完成 id=%d code=%s version=%d operator=%s",
		tpl.Id, code, published.Version, operator)
	return &rpc.PublishTemplateReply{Template: policy.ToTemplateInfo(published)}, nil
}
