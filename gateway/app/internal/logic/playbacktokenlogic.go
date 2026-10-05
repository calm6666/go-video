// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	playbackrpc "go-video/services/playback/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PlaybackTokenLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// NewPlaybackTokenLogic 签发短期防盗链播放地址（request_id 幂等）
func NewPlaybackTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PlaybackTokenLogic {
	return &PlaybackTokenLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PlaybackToken 先解析可播放版次（video/catalog + transcode），再交给 playback 签名。
// 版权窗口、游客策略与签名算法都在 playback 内判定，网关不复制这些规则（AGENTS.md §4）。
func (l *PlaybackTokenLogic) PlaybackToken(req *types.ParamPlaybackToken) (resp *types.PlaybackTokenResponse, err error) {
	if l.svcCtx.Playback == nil {
		return nil, errors.New("playback service not configured")
	}
	contentType := req.ContentType
	if contentType == 0 {
		contentType = contentTypeUGC
	}
	if contentType == contentTypePGC && req.Region == "" {
		return nil, errors.New("gateway/app: region is required for PGC playback")
	}
	src, err := resolvePlayableSource(l.ctx, l.svcCtx, contentType, req.Aid, req.Epid, req.TemplateId)
	if err != nil {
		l.Errorf("gateway/app/playbackToken: resolve source failed content_type=%d aid=%d epid=%d err=%v",
			contentType, req.Aid, req.Epid, err)
		return nil, err
	}
	var contentID int64
	if contentType == contentTypePGC {
		contentID = req.Epid
	} else {
		contentID = req.Aid
	}
	reply, err := l.svcCtx.Playback.GetPlaybackToken(l.ctx, &playbackrpc.GetPlaybackTokenReq{
		ContentType: playbackrpc.ContentType(contentType),
		ContentId:   contentID,
		Vid:         req.Vid,
		ObjectKey:   src.objectKey,
		Mid:         req.Mid,
		Platform:    playbackrpc.Platform(req.Platform),
		AppVersion:  req.AppVersion,
		Region:      req.Region,
		RequestId:   req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/playbackToken: aid=%d epid=%d mid=%d err=%v", req.Aid, req.Epid, req.Mid, err)
		return nil, err
	}
	return &types.PlaybackTokenResponse{
		Code:    0,
		Message: "ok",
		Data: types.PlaybackTokenData{
			SessionId:  reply.GetSessionId(),
			PlayUrl:    reply.GetPlayUrl(),
			AuthKey:    reply.GetAuthKey(),
			ExpireAt:   reply.GetExpireAt(),
			TTL:        reply.GetTtl(),
			KeyId:      reply.GetKeyId(),
			TemplateId: src.templateID,
			Quality:    src.quality,
			ObjectKey:  src.objectKey,
		},
		TTL: reply.GetTtl(),
	}, nil
}
