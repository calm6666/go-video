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

type CreateTranscodeTemplateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营创建转码模板
func NewCreateTranscodeTemplateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateTranscodeTemplateLogic {
	return &CreateTranscodeTemplateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 运营创建转码模板：聚合 transcode CreateTemplate RPC。
// 模板参数的合法性（编码器、码率、分片时长等）由 transcode 服务校验，网关不重复实现规则。
func (l *CreateTranscodeTemplateLogic) CreateTranscodeTemplate(req *types.ParamCreateTranscodeTemplate) (resp *types.TranscodeTemplateResponse, err error) {
	if l.svcCtx.Transcode == nil {
		return nil, errors.New("transcode service not configured")
	}
	if err := adminSessionGate(l.ctx, "createTranscodeTemplate"); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Transcode.CreateTemplate(l.ctx, &transcoderpc.CreateTemplateReq{
		Name:           req.Name,
		Codec:          req.Codec,
		Width:          req.Width,
		Height:         req.Height,
		Bitrate:        req.Bitrate,
		Fps:            req.Fps,
		SegmentSeconds: req.SegmentSeconds,
	})
	if err != nil {
		l.Errorf("gateway/admin/createTranscodeTemplate: name=%s codec=%s err=%v", req.Name, req.Codec, err)
		return nil, err
	}
	// pb getter 对 nil reply 安全，返回零值条目而不是报错。
	template := types.TranscodeTemplateItem{
		TemplateId:     reply.GetTemplateId(),
		Name:           reply.GetName(),
		Codec:          reply.GetCodec(),
		Width:          reply.GetWidth(),
		Height:         reply.GetHeight(),
		Bitrate:        reply.GetBitrate(),
		Fps:            reply.GetFps(),
		SegmentSeconds: reply.GetSegmentSeconds(),
	}
	return &types.TranscodeTemplateResponse{
		Code:    0,
		Message: "ok",
		Data:    types.TranscodeTemplateData{Template: template},
		TTL:     0,
	}, nil
}
