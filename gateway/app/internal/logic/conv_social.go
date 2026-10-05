package logic

// 本文件是网关聚合层的类型转换（social-graph pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	socialgraphrpc "go-video/services/social-graph/rpc"
)

func toSocialRelationItem(p *socialgraphrpc.RelationItem) types.SocialRelationItem {
	if p == nil {
		return types.SocialRelationItem{}
	}
	return types.SocialRelationItem{
		Mid:   p.Mid,
		Ctime: p.Ctime,
		Attr:  p.Attr,
	}
}

func toSocialRelationItems(p []*socialgraphrpc.RelationItem) []types.SocialRelationItem {
	out := make([]types.SocialRelationItem, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, toSocialRelationItem(v))
	}
	return out
}

func toSocialStat(p *socialgraphrpc.StatReply) types.SocialStat {
	if p == nil {
		return types.SocialStat{}
	}
	return types.SocialStat{
		Following: p.Following,
		Follower:  p.Follower,
		Whisper:   p.Whisper,
	}
}
