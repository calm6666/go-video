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

type RenderTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRenderTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenderTemplateLogic {
	return &RenderTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 仅渲染模板供上游预览，不落库。
//
// version>0 时按指定版本预览（可以是草稿，供运营核对未发布内容）；
// version=0 时使用当前已发布版本。变量缺失不会被“补空串”蒙混过去：
// 错误里带缺失变量清单，运营据此判断模板与调用方参数是否匹配。
func (l *RenderTemplateLogic) RenderTemplate(in *rpc.RenderTemplateReq) (*rpc.RenderTemplateReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil render request")
	}
	if strings.TrimSpace(in.GetOperator()) == "" {
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
	if lang == "" {
		return nil, ErrTemplateLanguageRequired
	}
	params := in.GetTemplateParams()
	if err := policy.ValidateParams(params); err != nil {
		return nil, err
	}

	var tpl *model.NotificationTemplate
	if v := in.GetVersion(); v > 0 {
		t, ferr := l.svcCtx.Repository.FindTemplateVersion(l.ctx, code, int32(in.GetChannel()), lang, v)
		if ferr != nil {
			return nil, ferr
		}
		tpl = t
	} else {
		t, _, ferr := l.svcCtx.Repository.FindPublished(l.ctx, code, int32(in.GetChannel()), []string{lang})
		if ferr != nil {
			return nil, ferr
		}
		tpl = t
	}
	if tpl == nil {
		return nil, fmt.Errorf("%w: code=%s channel=%d lang=%s version=%d",
			model.ErrTemplateNotFound, code, int32(in.GetChannel()), lang, in.GetVersion())
	}
	// 先给缺失清单，再交给 Render 做长度与控制符校验（两者口径必须一致，否则预览通过、实发失败）。
	if missing := policy.MissingVars(tpl.TitleTpl, tpl.BodyTpl, params); len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", policy.ErrRenderMissingVar, strings.Join(missing, ","))
	}
	out, err := policy.Render(tpl.TitleTpl, tpl.BodyTpl, params)
	if err != nil {
		return nil, err
	}
	return &rpc.RenderTemplateReply{
		Title:    out.Title,
		Body:     out.Body,
		Version:  tpl.Version,
		Language: in.GetLanguage(),
	}, nil
}
