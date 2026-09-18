package logic

// 本文件移植自参考仓库 server/http/v1.go 与 v2.go 的响应转换规则
// （V1Info.FromCard、V1Card.FromProfile、V1Vip.FromVip、V2MyInfo.FromProfile、
// model.CvtOfficial）。v1/v2 是老客户端兼容路由，字段名与转换规则保持一致。

import (
	"strconv"
	"time"

	"go-video/gateway/app/internal/types"
	accountrpc "go-video/services/account/rpc"
)

// cvtOfficial 把新官方认证结构转换为 v1/v2 旧结构（参考 model.CvtOfficial）。
func cvtOfficial(o *accountrpc.OfficialInfo) types.V1OfficialVerify {
	old := types.V1OfficialVerify{}
	if o == nil || o.Role == 0 {
		old.Type = -1
		return old
	}
	if o.Role <= 2 {
		old.Type = 0
	} else {
		old.Type = 1
	}
	old.Desc = o.Title
	return old
}

func toV1VipDetail(v *accountrpc.VipInfo) types.V1VipDetail {
	d := types.V1VipDetail{}
	if v == nil {
		return d
	}
	d.Type = int(v.Type)
	d.VipStatus = int(v.Status)
	d.DueDate = v.DueDate
	return d
}

func toV1LevelInfo(level int32) types.V1LevelInfo {
	return types.V1LevelInfo{Cur: int(level), Min: 0, NowExp: 0, NextExp: 0}
}

// toV1Info 把名片转换为 v1 用户基础信息（参考 V1Info.FromCard）。
func toV1Info(card *accountrpc.Card) types.V1Info {
	i := types.V1Info{}
	if card == nil {
		return i
	}
	i.Mid = strconv.FormatInt(card.Mid, 10)
	i.Name = card.Name
	i.Sex = card.Sex
	i.Sign = card.Sign
	i.Avatar = card.Face
	i.Rank = strconv.FormatInt(int64(card.Rank), 10)
	i.DisplayRank = "0"
	i.LevelInfo = toV1LevelInfo(card.Level)
	i.Pendant = toPendantInfo(card.Pendant)
	i.Nameplate = toNameplateInfo(card.Nameplate)
	i.OfficialVerify = cvtOfficial(card.Official)
	i.Vip = toV1VipDetail(card.Vip)
	return i
}

// toV1InfoMap 批量把名片转换为 v1 信息映射，key 为十进制 mid 字符串。
func toV1InfoMap(cards map[int64]*accountrpc.Card) map[string]types.V1Info {
	out := make(map[string]types.V1Info, len(cards))
	for mid, card := range cards {
		if card == nil {
			continue
		}
		out[strconv.FormatInt(mid, 10)] = toV1Info(card)
	}
	return out
}

// toV1Card 把带统计的资料转换为 v1 名片（参考 V1Card.FromProfile）。
func toV1Card(ps *accountrpc.ProfileStatReply) types.V1Card {
	c := types.V1Card{}
	if ps == nil || ps.Profile == nil {
		return c
	}
	p := ps.Profile
	c.Mid = strconv.FormatInt(p.Mid, 10)
	c.Name = p.Name
	c.Sex = p.Sex
	c.Sign = p.Sign
	c.Face = p.Face
	c.Rank = strconv.FormatInt(int64(p.Rank), 10)
	c.DisplayRank = "0"
	c.Regtime = int64(p.JoinTime)
	if p.Silence == 1 {
		c.Spacesta = -2
	}
	c.Attentions = []int64{}
	c.Fans = int(ps.Follower)
	c.Attention = int(ps.Following)
	c.LevelInfo = types.V1LevelInfo{
		Cur:     int(ps.LevelInfo.Cur),
		Min:     int(ps.LevelInfo.Min),
		NowExp:  int(ps.LevelInfo.NowExp),
		NextExp: ps.LevelInfo.NextExp,
	}
	// 参考实现：满级（now_exp=-1）时 next_exp 用字符串 "--" 表示
	if ps.LevelInfo.NowExp == -1 {
		c.LevelInfo.NextExp = "--"
	}
	c.Pendant = toPendantInfo(p.Pendant)
	c.Nameplate = toNameplateInfo(p.Nameplate)
	c.OfficialVerify = cvtOfficial(p.Official)
	c.Vip = toV1VipDetail(p.Vip)
	if p.Birthday > 0 {
		c.Birthday = time.Unix(p.Birthday, 0).Format("2006-01-02")
	}
	return c
}

// toV1Vip 把会员信息转换为 v1 会员结构（参考 V1Vip.FromVip）。
func toV1Vip(v *accountrpc.VipInfo) types.V1Vip {
	d := types.V1Vip{}
	if v == nil {
		return d
	}
	d.Type = int(v.Type)
	d.VipStatus = int(v.Status)
	d.DueDate = v.DueDate
	return d
}

// toV2MyInfo 把带统计的资料转换为 v2 我的资料（参考 V2MyInfo.FromProfile）。
func toV2MyInfo(ps *accountrpc.ProfileStatReply) types.V2MyInfo {
	i := types.V2MyInfo{}
	if ps == nil || ps.Profile == nil {
		return i
	}
	p := ps.Profile
	i.Mid = p.Mid
	i.Name = p.Name
	switch p.Sex {
	case "男":
		i.Sex = 1
	case "女":
		i.Sex = 2
	default:
		i.Sex = 0
	}
	i.Sign = p.Sign
	i.Face = p.Face
	i.Rank = p.Rank
	i.JoinTime = p.JoinTime
	i.Silence = p.Silence
	if p.Silence == 1 {
		i.Spacesta = -2
	}
	i.EmailStatus = p.EmailStatus
	i.TelStatus = p.TelStatus
	if p.EmailStatus == 1 || p.TelStatus == 1 {
		i.Active = 1
	}
	i.Identification = p.Identification
	i.Coins = ps.Coins
	i.Moral = p.Moral
	i.LevelInfo = toLevelInfo(ps.LevelInfo)
	i.Pendant = toPendantInfo(p.Pendant)
	i.Nameplate = toNameplateInfo(p.Nameplate)
	i.OfficialVerify = cvtOfficial(p.Official)
	i.Vip = toV1VipDetail(p.Vip)
	if p.Birthday > 0 {
		i.Birthday = time.Unix(p.Birthday, 0).Format("2006-01-02")
	}
	return i
}
