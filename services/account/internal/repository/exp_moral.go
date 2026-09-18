package repository

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
)

// AddExp 增加经验值。
// 依据 AGENTS.md §5，经验值数据由 user-profile 服务持有，account 委托调用。
// 当 UserProfileClient 未注入时返回 ErrNotImplemented。
func (r *Repository) AddExp(ctx context.Context, mid int64, exp float64, operater, operate, reason string) error {
	if r.userProfile == nil {
		return ErrNotImplemented
	}
	if err := r.userProfile.AddExp(ctx, mid, exp, operater, operate, reason); err != nil {
		logx.Errorf("account/exp: user-profile.AddExp mid=%d exp=%v err=%v", mid, exp, err)
		return err
	}
	// 经验值变更可能影响等级，失效相关缓存
	_ = r.cache.DelCache(ctx, mid)
	return nil
}

// AddMoral 增加道德值。
// 依据 AGENTS.md §5，道德值数据由 user-profile 服务持有，account 委托调用。
// 当 UserProfileClient 未注入时返回 ErrNotImplemented。
func (r *Repository) AddMoral(ctx context.Context, mid int64, moral float64, oper, reason, remark string) error {
	if r.userProfile == nil {
		return ErrNotImplemented
	}
	if err := r.userProfile.AddMoral(ctx, mid, moral, oper, reason, remark); err != nil {
		logx.Errorf("account/exp: user-profile.AddMoral mid=%d moral=%v err=%v", mid, moral, err)
		return err
	}
	// 道德值变更影响 Profile，失效相关缓存
	_ = r.cache.DelCache(ctx, mid)
	return nil
}
