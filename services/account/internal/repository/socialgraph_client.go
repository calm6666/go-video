package repository

// 本文件是 account 服务对 social-graph 服务的 gRPC 客户端适配器（AGENTS.md §5：
// 关系主数据的所有者是 social-graph，account 只能走它的公开 RPC 读关系，不得直连其库表）。
// account 侧的六个方法名与 social-graph 的 RPC 名不同口径，映射关系见各方法注释。

import (
	"context"
	"fmt"

	"github.com/zeromicro/go-zero/zrpc"

	socialgraph "go-video/services/social-graph/rpc"
)

// social-graph 读侧契约里写死的分页与批量上限（超上限服务端直接拒答，
// 所以适配器必须自己切片，不能指望下游兜住）。
const (
	// sgPageSizeMax 与 social-graph ListReq.ps 上限一致（listfollowinglogic.go:29 `in.Ps > 50`）。
	sgPageSizeMax = 50
	// sgBatchMax 与 social-graph 两个批量入口的入参上限一致：
	// RelationsReq.owners（isfollowedbatchlogic.go:35 `len(in.Owners) > 100`）与
	// RichRelationsReq.mids（richrelationslogic.go `len(in.Mids) > 100`）。
	sgBatchMax = 100
	// sgListPageCap 是整表拉取的分页次数上限。列表没有「一次拿完」的形态，
	// 因此必须有硬停：宁可报错也不能让一次账号聚合变成无界 RPC 扇出。
	sgListPageCap = 100
)

// socialGraphClient 通过 zrpc 调用 social-graph 服务，实现 SocialGraphClient 接口。
type socialGraphClient struct {
	cli socialgraph.SocialGraphClient
}

// NewSocialGraphClient 构造 social-graph 客户端适配器。
func NewSocialGraphClient(c zrpc.RpcClientConf) SocialGraphClient {
	conn := zrpc.MustNewClient(c)
	return &socialGraphClient{cli: socialgraph.NewSocialGraphClient(conn.Conn())}
}

// newSocialGraphClient 用已有的生成式 client 构造适配器，供单测注入替身。
func newSocialGraphClient(cli socialgraph.SocialGraphClient) *socialGraphClient {
	return &socialGraphClient{cli: cli}
}

// Relation 查询 mid 是否关注 owner（对应 IsFollowing）。
func (c *socialGraphClient) Relation(ctx context.Context, mid, owner int64) (bool, error) {
	reply, err := c.cli.IsFollowing(ctx, &socialgraph.RelationReq{Mid: mid, Owner: owner})
	if err != nil {
		return false, fmt.Errorf("social-graph IsFollowing mid=%d owner=%d: %w", mid, owner, err)
	}
	return reply.GetFollowing(), nil
}

// Relations 批量查询 mid 是否关注 owners（对应 IsFollowedBatch）。
// 上游按 100 个 owner 切片，任何一片失败都整体报错——半张关系表比报错更容易被读成「没关注」。
func (c *socialGraphClient) Relations(ctx context.Context, mid int64, owners []int64) (map[int64]bool, error) {
	if len(owners) == 0 {
		return map[int64]bool{}, nil
	}
	result := make(map[int64]bool, len(owners))
	for start := 0; start < len(owners); start += sgBatchMax {
		end := start + sgBatchMax
		if end > len(owners) {
			end = len(owners)
		}
		chunk := owners[start:end]
		reply, err := c.cli.IsFollowedBatch(ctx, &socialgraph.RelationsReq{Mid: mid, Owners: chunk})
		if err != nil {
			return nil, fmt.Errorf("social-graph IsFollowedBatch mid=%d owners=[%d,%d): %w", mid, start, end, err)
		}
		for owner, following := range reply.GetFollowing() {
			result[owner] = following
		}
	}
	return result, nil
}

// Stat 查询关注数与粉丝数（对应 Stat）。
func (c *socialGraphClient) Stat(ctx context.Context, mid int64) (*RelationStat, error) {
	reply, err := c.cli.Stat(ctx, &socialgraph.MidReq{Mid: mid})
	if err != nil {
		return nil, fmt.Errorf("social-graph Stat mid=%d: %w", mid, err)
	}
	return &RelationStat{Following: reply.GetFollowing(), Follower: reply.GetFollower()}, nil
}

// Attentions 拉取 mid 的完整关注列表（对应 ListFollowing 的分页全量）。
// 契约语义是「含特别关注」：special 关系本身也是 follow，所以关注列表即其超集。
func (c *socialGraphClient) Attentions(ctx context.Context, mid int64) ([]int64, error) {
	items, err := c.listAll(ctx, mid, true)
	if err != nil {
		return nil, err
	}
	mids := make([]int64, 0, len(items))
	mids = append(mids, items...)
	return mids, nil
}

// Blacks 拉取 mid 的完整黑名单（对应 ListBlacks 的分页全量）。
// 返回 set 而不是切片：调用方要的是「某个 mid 在不在名单里」。
func (c *socialGraphClient) Blacks(ctx context.Context, mid int64) (map[int64]bool, error) {
	items, err := c.listAll(ctx, mid, false)
	if err != nil {
		return nil, err
	}
	result := make(map[int64]bool, len(items))
	for _, id := range items {
		result[id] = true
	}
	return result, nil
}

// RichRelations 批量查询 owner 与 mids 的关系位掩码（对应同名 RPC）。
// 掩码位口径完全由 social-graph 的 RelationAttr 定义（1 FOLLOWING / 2 FOLLOWER /
// 4 BLACKED / 8 SPECIAL，3 MUTUAL 是 1|2 的派生值），本适配器只做切片与合并，
// 不重新解释也不补默认位——account 的 RichRelations3 契约就是「透传关系属性值」。
// 上游按 100 个 mid 切片，任一片失败整体报错：半张掩码表会被读成「谁都不认识」。
func (c *socialGraphClient) RichRelations(ctx context.Context, owner int64, mids []int64) (map[int64]int32, error) {
	if len(mids) == 0 {
		return map[int64]int32{}, nil
	}
	result := make(map[int64]int32, len(mids))
	for start := 0; start < len(mids); start += sgBatchMax {
		end := start + sgBatchMax
		if end > len(mids) {
			end = len(mids)
		}
		chunk := mids[start:end]
		reply, err := c.cli.RichRelations(ctx, &socialgraph.RichRelationsReq{Owner: owner, Mids: chunk})
		if err != nil {
			return nil, fmt.Errorf("social-graph RichRelations owner=%d mids=[%d,%d): %w", owner, start, end, err)
		}
		for mid, attr := range reply.GetAttrs() {
			result[mid] = attr
		}
	}
	return result, nil
}

// listAll 按 pn 从 1、ps=sgPageSizeMax 翻到 total 或空页为止。
// following=true 走 ListFollowing，false 走 ListBlacks。
func (c *socialGraphClient) listAll(ctx context.Context, mid int64, following bool) ([]int64, error) {
	var (
		seen  []int64
		total int32 = -1
	)
	for pn := int32(1); pn <= sgListPageCap; pn++ {
		req := &socialgraph.ListReq{Mid: mid, Pn: pn, Ps: sgPageSizeMax}
		var (
			items []*socialgraph.RelationItem
			t     int32
			err   error
		)
		if following {
			var reply *socialgraph.FollowingReply
			reply, err = c.cli.ListFollowing(ctx, req)
			if reply != nil {
				items, t = reply.GetItems(), reply.GetTotal()
			}
		} else {
			var reply *socialgraph.BlacksReply
			reply, err = c.cli.ListBlacks(ctx, req)
			if reply != nil {
				items, t = reply.GetItems(), reply.GetTotal()
			}
		}
		if err != nil {
			return nil, fmt.Errorf("social-graph list mid=%d pn=%d following=%v: %w", mid, pn, following, err)
		}
		if total < 0 {
			total = t
		}
		for _, it := range items {
			seen = append(seen, it.GetMid())
		}
		if len(items) == 0 || int32(len(seen)) >= total {
			return seen, nil
		}
	}
	return nil, fmt.Errorf("social-graph list mid=%d following=%v 超过 %d 页上限（total=%d），拒绝继续扇出",
		mid, following, sgListPageCap, total)
}
