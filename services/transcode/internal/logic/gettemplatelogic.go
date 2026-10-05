package logic

import (
	"context"

	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTemplateLogic {
	return &GetTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetTemplate 查询模板详情。
func (l *GetTemplateLogic) GetTemplate(in *rpc.TemplateReq) (*rpc.TemplateReply, error) {
	if in.TemplateId <= 0 {
		return nil, model.ErrInvalidTemplateID
	}
	t, err := l.svcCtx.Repository.GetTemplate(l.ctx, in.TemplateId)
	if err != nil {
		if err == model.ErrTemplateNotFound {
			return nil, err
		}
		l.Errorf("transcode/GetTemplate: template_id=%d err=%v", in.TemplateId, err)
		return nil, err
	}
	return toTemplateReply(t), nil
}
