// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	transcoderpc "go-video/services/transcode/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTranscodeTemplatesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询转码模板
func NewListTranscodeTemplatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTranscodeTemplatesLogic {
	return &ListTranscodeTemplatesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询转码模板：聚合 transcode ListTemplates RPC。
func (l *ListTranscodeTemplatesLogic) ListTranscodeTemplates(req *types.ParamListTranscodeTemplates) (resp *types.TranscodeTemplatesResponse, err error) {
	if l.svcCtx.Transcode == nil {
		return nil, errors.New("transcode service not configured")
	}
	reply, err := l.svcCtx.Transcode.ListTemplates(l.ctx, &transcoderpc.ListTemplatesReq{
		Pn: req.Pn,
		Ps: req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listTranscodeTemplates: pn=%d ps=%d err=%v", req.Pn, req.Ps, err)
		return nil, err
	}
	templates := make([]types.TranscodeTemplateItem, 0, len(reply.GetTemplates()))
	for _, t := range reply.GetTemplates() {
		templates = append(templates, types.TranscodeTemplateItem{
			TemplateId:     t.GetTemplateId(),
			Name:           t.GetName(),
			Codec:          t.GetCodec(),
			Width:          t.GetWidth(),
			Height:         t.GetHeight(),
			Bitrate:        t.GetBitrate(),
			Fps:            t.GetFps(),
			SegmentSeconds: t.GetSegmentSeconds(),
		})
	}
	return &types.TranscodeTemplatesResponse{
		Code:    0,
		Message: "ok",
		Data: types.TranscodeTemplatesData{
			Total:     int64(reply.GetTotal()),
			Templates: templates,
		},
		TTL: 0,
	}, nil
}
