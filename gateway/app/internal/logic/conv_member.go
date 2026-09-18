package logic

// 本文件是网关聚合层的类型转换（user-profile pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"
)

func toMemberBase(p *userprofilerc.BaseInfoReply) types.MemberBaseInfo {
	if p == nil {
		return types.MemberBaseInfo{}
	}
	return types.MemberBaseInfo{
		Mid:      p.Mid,
		Name:     p.Name,
		Sex:      p.Sex,
		Face:     p.Face,
		Sign:     p.Sign,
		Rank:     p.Rank,
		Birthday: p.Birthday,
	}
}

func toMemberBaseMap(p map[int64]*userprofilerc.BaseInfoReply) map[int64]types.MemberBaseInfo {
	out := make(map[int64]types.MemberBaseInfo, len(p))
	for mid, v := range p {
		out[mid] = toMemberBase(v)
	}
	return out
}

func toMemberLevelInfo(p *userprofilerc.LevelInfoReply) types.LevelInfo {
	if p == nil {
		return types.LevelInfo{}
	}
	return types.LevelInfo{Cur: p.Cur, Min: p.Min, NowExp: p.NowExp, NextExp: p.NextExp}
}

func toMemberOfficial(p *userprofilerc.OfficialInfoReply) types.OfficialInfo {
	if p == nil {
		return types.OfficialInfo{}
	}
	return types.OfficialInfo{Role: p.Role, Title: p.Title, Desc: p.Desc}
}

func toMemberFull(p *userprofilerc.MemberInfoReply) types.MemberFullInfo {
	m := types.MemberFullInfo{}
	if p == nil {
		return m
	}
	m.BaseInfo = toMemberBase(p.BaseInfo)
	m.LevelInfo = toMemberLevelInfo(p.LevelInfo)
	m.OfficialInfo = toMemberOfficial(p.OfficialInfo)
	return m
}

func toMoral(p *userprofilerc.MoralReply) types.MoralInfo {
	if p == nil {
		return types.MoralInfo{}
	}
	return types.MoralInfo{
		Mid:             p.Mid,
		Moral:           p.Moral,
		Added:           p.Added,
		Deducted:        p.Deducted,
		LastRecoverDate: p.LastRecoverDate,
	}
}

func toUserLogs(p []*userprofilerc.UserLogReply) []types.UserLog {
	out := make([]types.UserLog, 0, len(p))
	for _, l := range p {
		if l == nil {
			continue
		}
		out = append(out, types.UserLog{
			Mid:     l.Mid,
			IP:      l.Ip,
			TS:      l.Ts,
			LogID:   l.LogId,
			Content: l.Content,
		})
	}
	return out
}

func toExpStat(p *userprofilerc.ExpStatReply) types.ExpStat {
	if p == nil {
		return types.ExpStat{}
	}
	return types.ExpStat{Login: p.Login, Watch: p.Watch, Coin: p.Coin, Share: p.Share}
}

func toRealnameBrief(r *realnameBriefData) types.RealnameBrief {
	if r == nil {
		return types.RealnameBrief{}
	}
	return types.RealnameBrief{
		Realname: r.Realname,
		Card:     r.Card,
		CardType: r.CardType,
		Status:   r.Status,
	}
}

func toRealnameStripped(p *userprofilerc.RealnameStrippedInfoReply) types.RealnameStrippedInfo {
	if p == nil {
		return types.RealnameStrippedInfo{}
	}
	return types.RealnameStrippedInfo{
		Mid:       p.Mid,
		Status:    int8(p.Status),
		Channel:   int8(p.Channel),
		Country:   int16(p.Country),
		CardType:  int8(p.CardType),
		AdultType: int8(p.AdultType),
	}
}

// realnameBriefData 实名简要信息（从 RealnameDetailReply 与 RealnameStatusReply 组合）。
type realnameBriefData struct {
	Realname string
	Card     string
	CardType int
	Status   int8
}

func emptyResponse() *types.EmptyResponse {
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}
}
