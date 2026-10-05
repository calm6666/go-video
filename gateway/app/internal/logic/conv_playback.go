// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：playback RPC → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	playbackrpc "go-video/services/playback/rpc"
)

func playbackSessionToAPI(s *playbackrpc.PlaybackSessionInfo) types.PlaybackSession {
	if s == nil {
		return types.PlaybackSession{}
	}
	return types.PlaybackSession{
		SessionId:   s.GetSessionId(),
		ContentType: s.GetContentType(),
		ContentId:   s.GetContentId(),
		Vid:         s.GetVid(),
		Mid:         s.GetMid(),
		Platform:    s.GetPlatform(),
		AppVersion:  s.GetAppVersion(),
		Region:      s.GetRegion(),
		ObjectKey:   s.GetObjectKey(),
		Uri:         s.GetUri(),
		RequestId:   s.GetRequestId(),
		ExpireAt:    s.GetExpireAt(),
		State:       s.GetState(),
		Ctime:       s.GetCtime(),
		Mtime:       s.GetMtime(),
	}
}

func playbackProgressToAPI(p *playbackrpc.PlaybackProgressInfo) types.PlaybackProgress {
	if p == nil {
		return types.PlaybackProgress{}
	}
	return types.PlaybackProgress{
		SessionId:   p.GetSessionId(),
		ContentType: p.GetContentType(),
		ContentId:   p.GetContentId(),
		Vid:         p.GetVid(),
		Mid:         p.GetMid(),
		PositionMs:  p.GetPositionMs(),
		DurationMs:  p.GetDurationMs(),
		BufferCount: p.GetBufferCount(),
		AvgBitrate:  p.GetAvgBitrate(),
		LastError:   p.GetLastError(),
		Mtime:       p.GetMtime(),
	}
}
