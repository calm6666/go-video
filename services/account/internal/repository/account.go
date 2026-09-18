package repository

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/account/rpc"
)

// Info 从缓存读取用户基础信息，miss 时回源 user-profile 并异步回填缓存。
func (r *Repository) Info(ctx context.Context, mid int64) (*rpc.Info, error) {
	cached, err := r.cache.CacheInfo(ctx, mid)
	if err != nil {
		logx.Errorf("account/repository: cache info mid=%d err=%v", mid, err)
	}
	if cached != nil {
		return cached, nil
	}
	raw, err := r.RawInfo(ctx, mid)
	if err != nil {
		return nil, err
	}
	if raw != nil {
		_ = r.async.Do(ctx, func(c context.Context) {
			r.cache.AddCacheInfo(c, mid, raw)
		})
	}
	return raw, nil
}

// Infos 批量读取用户基础信息。缓存 miss 的 mid 分组回源并异步回填。
func (r *Repository) Infos(ctx context.Context, mids []int64) (map[int64]*rpc.Info, error) {
	if len(mids) == 0 {
		return map[int64]*rpc.Info{}, nil
	}
	result := make(map[int64]*rpc.Info, len(mids))
	miss := make([]int64, 0, len(mids))
	for _, mid := range mids {
		cached, err := r.cache.CacheInfo(ctx, mid)
		if err != nil {
			logx.Errorf("account/repository: cache info mid=%d err=%v", mid, err)
		}
		if cached != nil {
			result[mid] = cached
		} else {
			miss = append(miss, mid)
		}
	}
	if len(miss) == 0 {
		return result, nil
	}
	raw, err := r.RawInfos(ctx, miss)
	if err != nil {
		return result, err
	}
	for mid, info := range raw {
		result[mid] = info
	}
	_ = r.async.Do(ctx, func(c context.Context) {
		for mid, info := range raw {
			r.cache.AddCacheInfo(c, mid, info)
		}
	})
	return result, nil
}

// Card 从缓存读取用户名片，miss 时回源并异步回填。
func (r *Repository) Card(ctx context.Context, mid int64) (*rpc.Card, error) {
	cached, err := r.cache.CacheCard(ctx, mid)
	if err != nil {
		logx.Errorf("account/repository: cache card mid=%d err=%v", mid, err)
	}
	if cached != nil {
		return cached, nil
	}
	raw, err := r.RawCard(ctx, mid)
	if err != nil {
		return nil, err
	}
	if raw != nil {
		_ = r.async.Do(ctx, func(c context.Context) {
			r.cache.AddCacheCard(c, mid, raw)
		})
	}
	return raw, nil
}

// Cards 批量读取用户名片。
func (r *Repository) Cards(ctx context.Context, mids []int64) (map[int64]*rpc.Card, error) {
	if len(mids) == 0 {
		return map[int64]*rpc.Card{}, nil
	}
	result := make(map[int64]*rpc.Card, len(mids))
	miss := make([]int64, 0, len(mids))
	for _, mid := range mids {
		cached, err := r.cache.CacheCard(ctx, mid)
		if err != nil {
			logx.Errorf("account/repository: cache card mid=%d err=%v", mid, err)
		}
		if cached != nil {
			result[mid] = cached
		} else {
			miss = append(miss, mid)
		}
	}
	if len(miss) == 0 {
		return result, nil
	}
	raw, err := r.RawCards(ctx, miss)
	if err != nil {
		return result, err
	}
	for mid, card := range raw {
		result[mid] = card
	}
	_ = r.async.Do(ctx, func(c context.Context) {
		for mid, card := range raw {
			r.cache.AddCacheCard(c, mid, card)
		}
	})
	return result, nil
}

// Profile 从缓存读取用户完整资料，miss 时回源并异步回填。
func (r *Repository) Profile(ctx context.Context, mid int64) (*rpc.Profile, error) {
	cached, err := r.cache.CacheProfile(ctx, mid)
	if err != nil {
		logx.Errorf("account/repository: cache profile mid=%d err=%v", mid, err)
	}
	if cached != nil {
		return cached, nil
	}
	raw, err := r.RawProfile(ctx, mid)
	if err != nil {
		return nil, err
	}
	if raw != nil {
		_ = r.async.Do(ctx, func(c context.Context) {
			r.cache.AddCacheProfile(c, mid, raw)
		})
	}
	return raw, nil
}

// Vip 从缓存读取会员信息，miss 时回源（降级为零值）并异步回填。
// 依据 AGENTS.md §1 商业化范围外约束，会员业务不在本期实现。
func (r *Repository) Vip(ctx context.Context, mid int64) (*rpc.VipInfo, error) {
	cached, err := r.cache.CacheVip(ctx, mid)
	if err != nil {
		logx.Errorf("account/repository: cache vip mid=%d err=%v", mid, err)
	}
	if cached != nil {
		return cached, nil
	}
	raw, err := r.RawVip(ctx, mid)
	if err != nil {
		return nil, err
	}
	if raw != nil {
		_ = r.async.Do(ctx, func(c context.Context) {
			r.cache.AddCacheVip(c, mid, raw)
		})
	}
	return raw, nil
}

// Vips 批量读取会员信息。
func (r *Repository) Vips(ctx context.Context, mids []int64) (map[int64]*rpc.VipInfo, error) {
	if len(mids) == 0 {
		return map[int64]*rpc.VipInfo{}, nil
	}
	result := make(map[int64]*rpc.VipInfo, len(mids))
	miss := make([]int64, 0, len(mids))
	for _, mid := range mids {
		cached, err := r.cache.CacheVip(ctx, mid)
		if err != nil {
			logx.Errorf("account/repository: cache vip mid=%d err=%v", mid, err)
		}
		if cached != nil {
			result[mid] = cached
		} else {
			miss = append(miss, mid)
		}
	}
	if len(miss) == 0 {
		return result, nil
	}
	raw, err := r.RawVips(ctx, miss)
	if err != nil {
		return result, err
	}
	for mid, v := range raw {
		result[mid] = v
	}
	_ = r.async.Do(ctx, func(c context.Context) {
		for mid, v := range raw {
			r.cache.AddCacheVip(c, mid, v)
		}
	})
	return result, nil
}
