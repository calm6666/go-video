package logic

import (
	"go-video/services/feed/model"
	"go-video/services/feed/rpc"
)

// toFeedItem 把 model.FeedOutbox 转为 rpc.FeedItem。
func toFeedItem(f *model.FeedOutbox) *rpc.FeedItem {
	if f == nil {
		return nil
	}
	return &rpc.FeedItem{
		Id:        f.ID,
		Mid:       f.Mid,
		Oid:       f.Oid,
		Otype:     rpc.OType(f.Otype),
		Action:    rpc.Action(f.Action),
		Ctime:     f.Ctime,
		Title:     f.Title,
		Cover:     f.Cover,
		Uri:       f.Uri,
		ForwardId: f.ForwardID,
	}
}

// toFeedItems 批量转换。
func toFeedItems(fs []*model.FeedOutbox) []*rpc.FeedItem {
	if len(fs) == 0 {
		return nil
	}
	out := make([]*rpc.FeedItem, 0, len(fs))
	for _, f := range fs {
		if item := toFeedItem(f); item != nil {
			out = append(out, item)
		}
	}
	return out
}

// normalizePs 校正每页大小到 [1, 50] 区间。
func normalizePs(ps int32) int32 {
	if ps <= 0 || ps > 50 {
		return 20
	}
	return ps
}
