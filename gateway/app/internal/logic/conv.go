package logic

// 本文件是网关聚合层的类型转换（account pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"
)

func toInfo(p *accountrpc.Info) types.Info {
	if p == nil {
		return types.Info{}
	}
	return types.Info{
		Mid:  p.Mid,
		Name: p.Name,
		Sex:  p.Sex,
		Face: p.Face,
		Sign: p.Sign,
		Rank: p.Rank,
	}
}

func toInfos(p map[int64]*accountrpc.Info) map[int64]types.Info {
	out := make(map[int64]types.Info, len(p))
	for mid, v := range p {
		out[mid] = toInfo(v)
	}
	return out
}

func toVipInfo(p *accountrpc.VipInfo) types.VipInfo {
	if p == nil {
		return types.VipInfo{}
	}
	return types.VipInfo{
		Type:       p.Type,
		Status:     p.Status,
		DueDate:    p.DueDate,
		VipPayType: p.VipPayType,
	}
}

// vipFromReply 转换 Vip3 的返回结构（VipReply 与 Card/Profile 内的 VipInfo 是不同消息）。
func vipFromReply(p *accountrpc.VipReply) types.VipInfo {
	if p == nil {
		return types.VipInfo{}
	}
	return types.VipInfo{
		Type:       p.Type,
		Status:     p.Status,
		DueDate:    p.DueDate,
		VipPayType: p.VipPayType,
	}
}

func toVipInfos(p map[int64]*accountrpc.VipReply) map[int64]types.VipInfo {
	out := make(map[int64]types.VipInfo, len(p))
	for mid, v := range p {
		out[mid] = vipFromReply(v)
	}
	return out
}

func toPendantInfo(p *accountrpc.PendantInfo) types.PendantInfo {
	if p == nil {
		return types.PendantInfo{}
	}
	return types.PendantInfo{Pid: p.Pid, Name: p.Name, Image: p.Image, Expire: p.Expire}
}

func toNameplateInfo(p *accountrpc.NameplateInfo) types.NameplateInfo {
	if p == nil {
		return types.NameplateInfo{}
	}
	return types.NameplateInfo{
		Nid:        p.Nid,
		Name:       p.Name,
		Image:      p.Image,
		ImageSmall: p.ImageSmall,
		Level:      p.Level,
		Condition:  p.Condition,
	}
}

func toOfficialInfo(p *accountrpc.OfficialInfo) types.OfficialInfo {
	if p == nil {
		return types.OfficialInfo{}
	}
	return types.OfficialInfo{Role: p.Role, Title: p.Title, Desc: p.Desc}
}

func toCard(p *accountrpc.Card) types.Card {
	if p == nil {
		return types.Card{}
	}
	return types.Card{
		Mid:       p.Mid,
		Name:      p.Name,
		Sex:       p.Sex,
		Face:      p.Face,
		Sign:      p.Sign,
		Rank:      p.Rank,
		Level:     p.Level,
		Silence:   p.Silence,
		Vip:       toVipInfo(p.Vip),
		Pendant:   toPendantInfo(p.Pendant),
		Nameplate: toNameplateInfo(p.Nameplate),
		Official:  toOfficialInfo(p.Official),
	}
}

func toCards(p map[int64]*accountrpc.Card) map[int64]types.Card {
	out := make(map[int64]types.Card, len(p))
	for mid, v := range p {
		out[mid] = toCard(v)
	}
	return out
}

func toProfile(p *accountrpc.Profile) types.Profile {
	if p == nil {
		return types.Profile{}
	}
	return types.Profile{
		Mid:            p.Mid,
		Name:           p.Name,
		Sex:            p.Sex,
		Face:           p.Face,
		Sign:           p.Sign,
		Rank:           p.Rank,
		Level:          p.Level,
		JoinTime:       p.JoinTime,
		Moral:          p.Moral,
		Silence:        p.Silence,
		EmailStatus:    p.EmailStatus,
		TelStatus:      p.TelStatus,
		Identification: p.Identification,
		Vip:            toVipInfo(p.Vip),
		Pendant:        toPendantInfo(p.Pendant),
		Nameplate:      toNameplateInfo(p.Nameplate),
		Official:       toOfficialInfo(p.Official),
		Birthday:       p.Birthday,
		IsTourist:      p.IsTourist,
	}
}

func toLevelInfo(p *accountrpc.LevelInfo) types.LevelInfo {
	if p == nil {
		return types.LevelInfo{}
	}
	return types.LevelInfo{Cur: p.Cur, Min: p.Min, NowExp: p.NowExp, NextExp: p.NextExp}
}

func toProfileStat(p *accountrpc.ProfileStatReply) types.ProfileStat {
	out := types.ProfileStat{}
	if p == nil {
		return out
	}
	out.Profile = toProfile(p.Profile)
	out.LevelExp = toLevelInfo(p.LevelInfo)
	out.Coins = p.Coins
	out.Following = p.Following
	out.Follower = p.Follower
	return out
}
