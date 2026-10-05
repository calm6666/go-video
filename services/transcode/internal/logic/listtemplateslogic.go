package logic

import (
	"context"

	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"

	"github.com/zeromicro/go-zero/core/logx"
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

// ListTemplates 分页查询模板列表。
func (l *ListTemplatesLogic) ListTemplates(in *rpc.ListTemplatesReq) (*rpc.TemplatesReply, error) {
	if in.Ps < 0 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	templates, total, err := l.svcCtx.Repository.ListTemplates(l.ctx, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("transcode/ListTemplates: err=%v", err)
		return nil, err
	}
	reply := &rpc.TemplatesReply{
		Total:     total,
		Templates: make([]*rpc.TemplateReply, 0, len(templates)),
	}
	for _, t := range templates {
		reply.Templates = append(reply.Templates, toTemplateReply(t))
	}
	return reply, nil
}
