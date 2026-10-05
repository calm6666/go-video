package logic

import (
	"context"

	"go-video/services/transcode/internal/svc"
	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateTemplateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateTemplateLogic {
	return &CreateTemplateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateTemplate 运营创建模板。
func (l *CreateTemplateLogic) CreateTemplate(in *rpc.CreateTemplateReq) (*rpc.TemplateReply, error) {
	if in.Name == "" {
		return nil, model.ErrTemplateNameEmpty
	}
	t := &model.TranscodeTemplate{
		Name:           in.Name,
		Codec:          in.Codec,
		Width:          in.Width,
		Height:         in.Height,
		Bitrate:        in.Bitrate,
		Fps:            in.Fps,
		SegmentSeconds: in.SegmentSeconds,
	}
	templateID, err := l.svcCtx.Repository.CreateTemplate(l.ctx, t)
	if err != nil {
		l.Errorf("transcode/CreateTemplate: name=%s err=%v", in.Name, err)
		return nil, err
	}
	t.TemplateId = templateID
	return toTemplateReply(t), nil
}
