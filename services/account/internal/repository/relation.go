package repository

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/account/rpc"
)

// Relation 查询 mid 是否关注 owner。返回的 RelationReply 字段为 following。
// 当 SocialGraphClient 未注入时，返回默认 false，不报错。
func (r *Repository) Relation(ctx context.Context, mid, owner int64) (*rpc.RelationReply, error) {
	if r.socialGraph == nil {
		return &rpc.RelationReply{Following: false}, nil
	}
	following, err := r.socialGraph.Relation(ctx, mid, owner)
	if err != nil {
		logx.Errorf("account/relation: social-graph.Relation mid=%d owner=%d err=%v", mid, owner, err)
		return &rpc.RelationReply{Following: false}, nil
	}
	return &rpc.RelationReply{Following: following}, nil
}

// Relations 批量查询 mid 与 owners 的关注关系。
// 即使某些 owner 查询失败，也补齐默认值，保证返回的 map 包含全部 owners。
func (r *Repository) Relations(ctx context.Context, mid int64, owners []int64) (*rpc.RelationsReply, error) {
	result := make(map[int64]*rpc.RelationReply, len(owners))
	if r.socialGraph == nil {
		for _, owner := range owners {
			result[owner] = &rpc.RelationReply{Following: false}
		}
		return &rpc.RelationsReply{Relations: result}, nil
	}
	relMap, err := r.socialGraph.Relations(ctx, mid, owners)
	if err != nil {
		logx.Errorf("account/relation: social-graph.Relations mid=%d err=%v", mid, err)
	}
	for _, owner := range owners {
		following := false
		if relMap != nil {
			following = relMap[owner]
		}
		result[owner] = &rpc.RelationReply{Following: following}
	}
	return &rpc.RelationsReply{Relations: result}, nil
}

// Attentions 查询 mid 的关注列表（含特别关注）。
func (r *Repository) Attentions(ctx context.Context, mid int64) (*rpc.AttentionsReply, error) {
	if r.socialGraph == nil {
		return &rpc.AttentionsReply{Attentions: []int64{}}, nil
	}
	mids, err := r.socialGraph.Attentions(ctx, mid)
	if err != nil {
		logx.Errorf("account/relation: social-graph.Attentions mid=%d err=%v", mid, err)
		return &rpc.AttentionsReply{Attentions: []int64{}}, nil
	}
	if mids == nil {
		mids = []int64{}
	}
	return &rpc.AttentionsReply{Attentions: mids}, nil
}

// Blacks 查询 mid 的黑名单列表。
func (r *Repository) Blacks(ctx context.Context, mid int64) (*rpc.BlacksReply, error) {
	if r.socialGraph == nil {
		return &rpc.BlacksReply{BlackList: map[int64]bool{}}, nil
	}
	blackMap, err := r.socialGraph.Blacks(ctx, mid)
	if err != nil {
		logx.Errorf("account/relation: social-graph.Blacks mid=%d err=%v", mid, err)
		return &rpc.BlacksReply{BlackList: map[int64]bool{}}, nil
	}
	if blackMap == nil {
		blackMap = map[int64]bool{}
	}
	return &rpc.BlacksReply{BlackList: blackMap}, nil
}

// RichRelations 查询 owner 与 mids 的关系属性值。
// 返回的 map 按 mids 顺序补齐默认值 0。
func (r *Repository) RichRelations(ctx context.Context, owner int64, mids []int64) (*rpc.RichRelationsReply, error) {
	result := make(map[int64]int32, len(mids))
	if r.socialGraph == nil {
		for _, mid := range mids {
			result[mid] = 0
		}
		return &rpc.RichRelationsReply{RichRelations: result}, nil
	}
	relMap, err := r.socialGraph.RichRelations(ctx, owner, mids)
	if err != nil {
		logx.Errorf("account/relation: social-graph.RichRelations owner=%d err=%v", owner, err)
	}
	for _, mid := range mids {
		v := int32(0)
		if relMap != nil {
			v = relMap[mid]
		}
		result[mid] = v
	}
	return &rpc.RichRelationsReply{RichRelations: result}, nil
}

// Stat 查询 mid 的关注数和粉丝数，供 ProfileWithStat 聚合使用。
// 内部方法，不直接暴露为 RPC。
func (r *Repository) Stat(ctx context.Context, mid int64) (*RelationStat, error) {
	if r.socialGraph == nil {
		return &RelationStat{}, nil
	}
	stat, err := r.socialGraph.Stat(ctx, mid)
	if err != nil {
		logx.Errorf("account/relation: social-graph.Stat mid=%d err=%v", mid, err)
		return &RelationStat{}, nil
	}
	if stat == nil {
		return &RelationStat{}, nil
	}
	return stat, nil
}
