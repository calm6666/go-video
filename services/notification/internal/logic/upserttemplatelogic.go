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

type UpsertTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertTemplateLogic {
	return &UpsertTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 新增或更新模板（草稿或直接发布新版本）。
//
// 入库前就执行与投递时完全相同的模板校验（policy.CheckTemplate）：
// 畸形/嵌套占位符、超长、标题换行一律拒绝，避免发布出一个永远投不出去的死信来源。
func (l *UpsertTemplateLogic) UpsertTemplate(in *rpc.UpsertTemplateReq) (*rpc.UpsertTemplateReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil upsert request")
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
	operator := strings.TrimSpace(in.GetOperator())
	if operator == "" {
		return nil, ErrOperatorRequired
	}
	if err := policy.CheckTemplate(in.GetTitleTpl(), in.GetBodyTpl()); err != nil {
		return nil, err
	}
	// 变量清单仅用于日志核对（渲染时缺失会显式报错）。
	l.Infof("notification/UpsertTemplate code=%s channel=%d lang=%s vars=%v publish=%v operator=%s",
		code, int32(in.GetChannel()), lang, policy.TemplateVars(in.GetTitleTpl(), in.GetBodyTpl()), in.GetPublish(), operator)

	saved, err := l.svcCtx.Repository.UpsertTemplate(l.ctx, &model.NotificationTemplate{
		TemplateCode: code,
		Channel:      int32(in.GetChannel()),
		Lang:         lang,
		TitleTpl:     in.GetTitleTpl(),
		BodyTpl:      in.GetBodyTpl(),
		Operator:     operator,
	}, in.GetPublish())
	if err != nil {
		return nil, err
	}
	return &rpc.UpsertTemplateReply{Template: policy.ToTemplateInfo(saved)}, nil
}
