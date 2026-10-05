package logic

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/notification/internal/policy"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

type ListTemplatesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTemplatesLogic {
	return &ListTemplatesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询模板（含草稿与已下线版本，供运营后台核对版本历史）。
func (l *ListTemplatesLogic) ListTemplates(in *rpc.ListTemplatesReq) (*rpc.ListTemplatesReply, error) {
	if in == nil {
		return nil, errors.New("notification/logic: nil list request")
	}
	pn, ps := normalizePage(in.GetPn(), in.GetPs())
	lang := policy.LangCodeOfEnum(in.GetLanguage())
	if in.GetLanguage() != rpc.Language_LANGUAGE_UNSPECIFIED && !model.IsValidLang(lang) {
		return nil, model.ErrInvalidLang
	}
	rows, total, err := l.svcCtx.Repository.ListTemplates(l.ctx, model.TemplateFilter{
		TemplateCode: in.GetTemplateCode(),
		Channel:      int32(in.GetChannel()),
		Lang:         lang,
		State:        int32(in.GetState()),
	}, pn, ps)
	if err != nil {
		return nil, err
	}
	return &rpc.ListTemplatesReply{Templates: policy.ToTemplateInfos(rows), Total: total}, nil
}
