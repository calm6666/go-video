package logic

// 本文件是网关聚合层的类型转换（feed pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	feedrpc "go-video/services/feed/rpc"
)

func toFeedItem(p *feedrpc.FeedItem) types.FeedItem {
	if p == nil {
		return types.FeedItem{}
	}
	return types.FeedItem{
		Id:        p.Id,
		Mid:       p.Mid,
		Oid:       p.Oid,
		OType:     int32(p.Otype),
		Action:    int32(p.Action),
		Ctime:     p.Ctime,
		Title:     p.Title,
		Cover:     p.Cover,
		Uri:       p.Uri,
		ForwardId: p.ForwardId,
	}
}

func toFeedItems(p []*feedrpc.FeedItem) []types.FeedItem {
	out := make([]types.FeedItem, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, toFeedItem(v))
	}
	return out
}

// toFeedData 把 feed.FeedReply 转换为网关 FeedData 载荷（PullFeed / ListUserFeed 共用）。
func toFeedData(p *feedrpc.FeedReply) types.FeedData {
	if p == nil {
		return types.FeedData{Items: []types.FeedItem{}}
	}
	return types.FeedData{
		Items:      toFeedItems(p.GetItems()),
		NextCursor: p.GetNextCursor(),
		HasMore:    p.GetHasMore(),
	}
}
