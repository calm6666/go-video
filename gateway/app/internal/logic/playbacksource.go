// 本文件是 gateway/app 的手写业务扩展（非 goctl 生成产物）。
// 职责：把终端给的 aid/epid 解析成 playback 可签名的 object_key。
// 依据 AGENTS.md §5，aid→媒资版次归 video、epid→媒资归 catalog、转码产物归 transcode，
// playback 只持有播放会话与签名；网关是唯一知道如何把它们拼起来的地方。

package logic

import (
	"context"
	"errors"
	"strconv"

	"go-video/gateway/app/internal/svc"
	catalogrpc "go-video/services/catalog/rpc"
	pbcrpc "go-video/services/playback/rpc"
	transcoderpc "go-video/services/transcode/rpc"
	videorpc "go-video/services/video/rpc"
)

// 播放内容类型（与 playback.v1.ContentType 一致，网关入参沿用同一套编号）。
const (
	contentTypeUGC = int32(pbcrpc.ContentType_CONTENT_TYPE_UGC)
	contentTypePGC = int32(pbcrpc.ContentType_CONTENT_TYPE_PGC)
)

// catalog.EpisodeReply.state 的上架值（与 services/catalog 的集状态常量一致）。
const catalogEpisodePublished = int32(1)

var (
	// ErrSourceNotConfigured 播放链路依赖的下游 RPC 未配置，属部署问题而非用户错误。
	ErrSourceNotConfigured = errors.New("gateway/app: playback source services not configured")
	// ErrInvalidContent 请求的内容类型或主键非法。
	ErrInvalidContent = errors.New("gateway/app: invalid content type or id")
	// ErrNoPlayableVersion 媒资没有任何转码成功的版本：尚未转码完成，不可播放。
	ErrNoPlayableVersion = errors.New("gateway/app: no transcoded version available")
)

// playableSource 是解析出的可播放版次。
type playableSource struct {
	assetID    int64
	objectKey  string
	templateID int64
	quality    string
}

// resolvePlayableSource 解析当前可播放版次的 CDN 相对路径。
// templateID > 0 时精确匹配该模板；为 0 时选择可用版次里清晰度最高（height，其次 bitrate）的一个。
func resolvePlayableSource(ctx context.Context, svcCtx *svc.ServiceContext,
	contentType int32, aid, epid, templateID int64) (*playableSource, error) {
	if svcCtx.Transcode == nil {
		return nil, ErrSourceNotConfigured
	}
	assetID, err := lookupAssetID(ctx, svcCtx, contentType, aid, epid)
	if err != nil {
		return nil, err
	}
	tasks, err := svcCtx.Transcode.ListTasks(ctx, &transcoderpc.ListReq{
		AssetId: assetID,
		State:   transcoderpc.TaskState_TASK_STATE_SUCCEEDED,
		Pn:      1,
		Ps:      50,
	})
	if err != nil {
		return nil, err
	}
	var candidates []*transcoderpc.TaskReply
	for _, t := range tasks.GetTasks() {
		if t.GetState() != transcoderpc.TaskState_TASK_STATE_SUCCEEDED || t.GetOutputKey() == "" {
			continue
		}
		if templateID > 0 && t.GetTemplateId() != templateID {
			continue
		}
		candidates = append(candidates, t)
	}
	if len(candidates) == 0 {
		return nil, ErrNoPlayableVersion
	}
	if templateID > 0 {
		return toPlayableSource(ctx, svcCtx, candidates[0])
	}
	// 未指定模板：按模板规格挑最高清晰度。取不到模板详情时保持候选顺序兜底。
	var best *transcoderpc.TaskReply
	var bestScore templateScore
	for _, t := range candidates {
		sc, err := fetchTemplateScore(ctx, svcCtx, t.GetTemplateId())
		if err != nil {
			if best == nil {
				best, bestScore = t, templateScore{}
			}
			continue
		}
		if best == nil || sc.betterThan(bestScore) {
			best, bestScore = t, sc
		}
	}
	return toPlayableSource(ctx, svcCtx, best)
}

// lookupAssetID 取内容对应的媒资 ID：UGC 由 video 提供当前可播放版次，PGC 由 catalog 提供集媒资。
func lookupAssetID(ctx context.Context, svcCtx *svc.ServiceContext, contentType int32, aid, epid int64) (int64, error) {
	switch contentType {
	case 0, contentTypeUGC:
		if aid <= 0 || svcCtx.Video == nil {
			return 0, ErrInvalidContent
		}
		src, err := svcCtx.Video.GetPlayableSource(ctx, &videorpc.PlayableSourceReq{Aid: aid})
		if err != nil {
			return 0, err
		}
		assetID, err := strconv.ParseInt(src.GetVersion().GetAssetId(), 10, 64)
		if err != nil || assetID <= 0 {
			return 0, ErrNoPlayableVersion
		}
		return assetID, nil
	case contentTypePGC:
		if epid <= 0 || svcCtx.Catalog == nil {
			return 0, ErrInvalidContent
		}
		ep, err := svcCtx.Catalog.GetEpisode(ctx, &catalogrpc.EpisodeReq{Epid: epid})
		if err != nil {
			return 0, err
		}
		if ep.GetState() != catalogEpisodePublished || ep.GetAssetId() <= 0 {
			return 0, ErrNoPlayableVersion
		}
		return ep.GetAssetId(), nil
	default:
		return 0, ErrInvalidContent
	}
}

// templateScore 是用于比较清晰度高低的模板规格。
type templateScore struct {
	height  int32
	bitrate int32
}

func (s templateScore) betterThan(other templateScore) bool {
	if s.height != other.height {
		return s.height > other.height
	}
	return s.bitrate > other.bitrate
}

func fetchTemplateScore(ctx context.Context, svcCtx *svc.ServiceContext, templateID int64) (templateScore, error) {
	tpl, err := svcCtx.Transcode.GetTemplate(ctx, &transcoderpc.TemplateReq{TemplateId: templateID})
	if err != nil {
		return templateScore{}, err
	}
	return templateScore{height: tpl.GetHeight(), bitrate: tpl.GetBitrate()}, nil
}

// toPlayableSource 组装 CDN 相对路径：/{output_bucket}/{output_key}。
// playback 的 Sign.BaseURL 配置为 CDN 源站根（不含 bucket），因此路径需带 bucket 段；
// 换 bucket 或改回源规则时只调整 playback 配置，网关契约不变。
func toPlayableSource(ctx context.Context, svcCtx *svc.ServiceContext, t *transcoderpc.TaskReply) (*playableSource, error) {
	if t == nil {
		return nil, ErrNoPlayableVersion
	}
	key := t.GetOutputKey()
	if b := t.GetOutputBucket(); b != "" {
		key = b + "/" + key
	}
	quality := ""
	if tpl, err := svcCtx.Transcode.GetTemplate(ctx, &transcoderpc.TemplateReq{TemplateId: t.GetTemplateId()}); err == nil {
		quality = tpl.GetName()
	}
	return &playableSource{
		assetID:    t.GetAssetId(),
		objectKey:  key,
		templateID: t.GetTemplateId(),
		quality:    quality,
	}, nil
}
