// 本文件是 gateway/app 的手写转换扩展（非 goctl 生成产物）：search-query RPC → 客户端投影。

package logic

import (
	"go-video/gateway/app/internal/types"
	searchqueryrpc "go-video/services/search-query/rpc"
)

func searchHitsToAPI(list []*searchqueryrpc.SearchHit) []types.SearchHit {
	out := make([]types.SearchHit, 0, len(list))
	for _, h := range list {
		out = append(out, types.SearchHit{
			DocType:        h.GetDocType(),
			DocId:          h.GetDocId(),
			Title:          h.GetTitle(),
			ContentSnippet: h.GetContentSnippet(),
			AuthorMid:      h.GetAuthorMid(),
			AuthorName:     h.GetAuthorName(),
			ZoneId:         h.GetZoneId(),
			CoverUrl:       h.GetCoverUrl(),
			ViewCount:      h.GetViewCount(),
			LikeCount:      h.GetLikeCount(),
			DanmakuCount:   h.GetDanmakuCount(),
			FansCount:      h.GetFansCount(),
			DurationSec:    h.GetDurationSec(),
			PubTime:        h.GetPubTime(),
			Score:          h.GetScore(),
			Highlights:     h.GetHighlights(),
			State:          h.GetState(),
		})
	}
	return out
}

func suggestItemsToAPI(list []*searchqueryrpc.SuggestItem) []types.SuggestItem {
	out := make([]types.SuggestItem, 0, len(list))
	for _, s := range list {
		out = append(out, types.SuggestItem{
			Keyword:     s.GetKeyword(),
			Weight:      s.GetWeight(),
			Source:      s.GetSource(),
			FromHistory: s.GetFromHistory(),
		})
	}
	return out
}

func hotKeywordsToAPI(list []*searchqueryrpc.HotKeyword) []types.HotKeyword {
	out := make([]types.HotKeyword, 0, len(list))
	for _, k := range list {
		out = append(out, types.HotKeyword{
			Keyword:    k.GetKeyword(),
			Score:      k.GetScore(),
			SnapshotAt: k.GetSnapshotAt(),
			Scope:      k.GetScope(),
		})
	}
	return out
}

func searchHistoryItemsToAPI(list []*searchqueryrpc.SearchHistoryItem) []types.SearchHistoryItem {
	out := make([]types.SearchHistoryItem, 0, len(list))
	for _, h := range list {
		out = append(out, types.SearchHistoryItem{
			Keyword:  h.GetKeyword(),
			Ctime:    h.GetCtime(),
			Mtime:    h.GetMtime(),
			State:    h.GetState(),
			Platform: h.GetPlatform(),
		})
	}
	return out
}
