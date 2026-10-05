package logic

// 本文件是网关聚合层的类型转换（catalog pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	catalogrpc "go-video/services/catalog/rpc"
)

func toCatalogWork(p *catalogrpc.WorkReply) types.CatalogWork {
	if p == nil {
		return types.CatalogWork{}
	}
	return types.CatalogWork{
		SeasonId: p.SeasonId,
		Title:    p.Title,
		Cover:    p.Cover,
		Typeid:   p.Typeid,
		Intro:    p.Intro,
		State:    p.State,
	}
}

func toCatalogWorks(p []*catalogrpc.WorkReply) []types.CatalogWork {
	out := make([]types.CatalogWork, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, toCatalogWork(v))
	}
	return out
}

func toCatalogEpisode(p *catalogrpc.EpisodeReply) types.CatalogEpisode {
	if p == nil {
		return types.CatalogEpisode{}
	}
	return types.CatalogEpisode{
		Epid:     p.Epid,
		SeasonId: p.SeasonId,
		EpNo:     p.EpNo,
		Title:    p.Title,
		AssetId:  p.AssetId,
		Duration: p.Duration,
		State:    p.State,
	}
}

func toCatalogEpisodes(p []*catalogrpc.EpisodeReply) []types.CatalogEpisode {
	out := make([]types.CatalogEpisode, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, toCatalogEpisode(v))
	}
	return out
}

func toCatalogZone(p *catalogrpc.ZoneReply) types.CatalogZone {
	if p == nil {
		return types.CatalogZone{}
	}
	return types.CatalogZone{
		Zoneid: p.Zoneid,
		Name:   p.Name,
		Parent: p.Parent,
	}
}

func toCatalogZones(p []*catalogrpc.ZoneReply) []types.CatalogZone {
	out := make([]types.CatalogZone, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, toCatalogZone(v))
	}
	return out
}
