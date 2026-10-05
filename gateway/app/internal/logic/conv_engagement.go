package logic

// 本文件是网关聚合层的类型转换（engagement pb → gateway HTTP types）。
// 网关只做转换与聚合，不持有业务规则。

import (
	"go-video/gateway/app/internal/types"
	engagementrpc "go-video/services/engagement/rpc"
)

func toEngagementStatState(p *engagementrpc.StatState) types.EngagementStatState {
	if p == nil {
		return types.EngagementStatState{}
	}
	return types.EngagementStatState{
		OriginId:      p.OriginId,
		MessageId:     p.MessageId,
		LikeNumber:    p.LikeNumber,
		DislikeNumber: p.DislikeNumber,
		LikeState:     int32(p.LikeState),
	}
}

func toEngagementStats(p map[int64]*engagementrpc.StatState) map[int64]types.EngagementStatState {
	out := make(map[int64]types.EngagementStatState, len(p))
	for k, v := range p {
		out[k] = toEngagementStatState(v)
	}
	return out
}

func toEngagementFolder(p *engagementrpc.Folder) types.EngagementFolder {
	if p == nil {
		return types.EngagementFolder{}
	}
	return types.EngagementFolder{
		Fid:         p.Fid,
		Mid:         p.Mid,
		Name:        p.Name,
		Description: p.Description,
		Cover:       p.Cover,
		Public:      p.Public,
		State:       p.State,
		Ctime:       p.Ctime,
		Mtime:       p.Mtime,
		Count:       p.Count,
	}
}

func toEngagementFolders(p []*engagementrpc.Folder) []types.EngagementFolder {
	out := make([]types.EngagementFolder, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, toEngagementFolder(v))
	}
	return out
}

// toEngagementFaveds 复制 oid → 是否已收藏映射。
// 返回非 nil map，未收藏的对象在 JSON 里表现为 {} 而非 null。
func toEngagementFaveds(p map[int64]bool) map[int64]bool {
	out := make(map[int64]bool, len(p))
	for oid, faved := range p {
		out[oid] = faved
	}
	return out
}

// toEngagementLikeStates 把 message_id → UserLikeState 映射投影为 HTTP 结构。
// state 是 engagement.LikeState 枚举，必须显式转 int32 才能进 JSON 信封。
func toEngagementLikeStates(p map[int64]*engagementrpc.UserLikeState) map[int64]types.EngagementLikeStateData {
	out := make(map[int64]types.EngagementLikeStateData, len(p))
	for messageId, v := range p {
		if v == nil {
			out[messageId] = types.EngagementLikeStateData{}
			continue
		}
		out[messageId] = types.EngagementLikeStateData{
			Mid:   v.Mid,
			Time:  v.Time,
			State: int32(v.State),
		}
	}
	return out
}

// toEngagementLikeItems 投影 UserLikes 的点赞记录列表（顺序由 engagement 服务端决定）。
func toEngagementLikeItems(p []*engagementrpc.ItemRecord) []types.EngagementLikeItem {
	out := make([]types.EngagementLikeItem, 0, len(p))
	for _, v := range p {
		if v == nil {
			continue
		}
		out = append(out, types.EngagementLikeItem{
			MessageId: v.MessageId,
			Time:      v.Time,
		})
	}
	return out
}
