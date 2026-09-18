package repository

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/account/rpc"
)

// RawInfo 从 user-profile 服务获取用户基础信息。
// 当 UserProfileClient 未注入或调用失败时，返回零值 Info 和 nil err，触发降级。
func (r *Repository) RawInfo(ctx context.Context, mid int64) (*rpc.Info, error) {
	if r.userProfile == nil {
		return &rpc.Info{Mid: mid}, nil
	}
	base, err := r.userProfile.Base(ctx, mid)
	if err != nil {
		logx.Errorf("account/raw: user-profile.Base mid=%d err=%v", mid, err)
		return &rpc.Info{Mid: mid}, nil
	}
	if base == nil {
		return &rpc.Info{Mid: mid}, nil
	}
	return &rpc.Info{
		Mid:  base.Mid,
		Name: base.Name,
		Sex:  base.Sex,
		Face: base.Face,
		Sign: base.Sign,
		Rank: base.Rank,
	}, nil
}

// RawInfos 批量从 user-profile 服务获取用户基础信息。
func (r *Repository) RawInfos(ctx context.Context, mids []int64) (map[int64]*rpc.Info, error) {
	if len(mids) == 0 {
		return map[int64]*rpc.Info{}, nil
	}
	if r.userProfile == nil {
		result := make(map[int64]*rpc.Info, len(mids))
		for _, mid := range mids {
			result[mid] = &rpc.Info{Mid: mid}
		}
		return result, nil
	}
	bases, err := r.userProfile.Bases(ctx, mids)
	if err != nil {
		logx.Errorf("account/raw: user-profile.Bases err=%v", err)
		result := make(map[int64]*rpc.Info, len(mids))
		for _, mid := range mids {
			result[mid] = &rpc.Info{Mid: mid}
		}
		return result, nil
	}
	result := make(map[int64]*rpc.Info, len(bases))
	for _, base := range bases {
		if base == nil {
			continue
		}
		result[base.Mid] = &rpc.Info{
			Mid:  base.Mid,
			Name: base.Name,
			Sex:  base.Sex,
			Face: base.Face,
			Sign: base.Sign,
			Rank: base.Rank,
		}
	}
	return result, nil
}

// RawCard 从 user-profile 服务获取用户名片。
// 依据 AGENTS.md §1 商业化范围外约束，vip、pendant、nameplate 字段降级为零值；
// official 由 user-profile 聚合返回（user-profile 未部署时为零值）。
func (r *Repository) RawCard(ctx context.Context, mid int64) (*rpc.Card, error) {
	if r.userProfile == nil {
		return &rpc.Card{Mid: mid}, nil
	}
	member, err := r.userProfile.Member(ctx, mid)
	if err != nil {
		logx.Errorf("account/raw: user-profile.Member mid=%d err=%v", mid, err)
		return &rpc.Card{Mid: mid}, nil
	}
	if member == nil {
		return &rpc.Card{Mid: mid}, nil
	}
	card := &rpc.Card{
		Mid:   member.Mid,
		Name:  member.Name,
		Sex:   member.Sex,
		Face:  member.Face,
		Sign:  member.Sign,
		Rank:  member.Rank,
		Level: member.Level,
		// Vip/Pendant/Nameplate 降级为零值，对应 AGENTS.md §1 商业化范围外约束
	}
	if member.Official != nil {
		card.Official = member.Official
	}
	return card, nil
}

// RawCards 批量从 user-profile 服务获取用户名片。
func (r *Repository) RawCards(ctx context.Context, mids []int64) (map[int64]*rpc.Card, error) {
	if len(mids) == 0 {
		return map[int64]*rpc.Card{}, nil
	}
	if r.userProfile == nil {
		result := make(map[int64]*rpc.Card, len(mids))
		for _, mid := range mids {
			result[mid] = &rpc.Card{Mid: mid}
		}
		return result, nil
	}
	members, err := r.userProfile.Members(ctx, mids)
	if err != nil {
		logx.Errorf("account/raw: user-profile.Members err=%v", err)
		result := make(map[int64]*rpc.Card, len(mids))
		for _, mid := range mids {
			result[mid] = &rpc.Card{Mid: mid}
		}
		return result, nil
	}
	result := make(map[int64]*rpc.Card, len(members))
	for mid, m := range members {
		if m == nil {
			continue
		}
		card := &rpc.Card{
			Mid:   m.Mid,
			Name:  m.Name,
			Sex:   m.Sex,
			Face:  m.Face,
			Sign:  m.Sign,
			Rank:  m.Rank,
			Level: m.Level,
		}
		if m.Official != nil {
			card.Official = m.Official
		}
		result[mid] = card
	}
	return result, nil
}

// RawProfile 从 user-profile 服务获取用户完整资料，并合并本地 account 表的状态。
// 依据 AGENTS.md §1 商业化范围外约束，pendant、nameplate 降级为零值；
// official/level/birthday/moral/identification 由 user-profile RPC 聚合返回
// （user-profile 未部署时降级为零值）。
// join_time、email_status、tel_status、is_tourist 来自本地 account/account_credential 表。
func (r *Repository) RawProfile(ctx context.Context, mid int64) (*rpc.Profile, error) {
	profile := &rpc.Profile{Mid: mid}
	if r.userProfile != nil {
		member, err := r.userProfile.Member(ctx, mid)
		if err != nil {
			logx.Errorf("account/raw: user-profile.Member mid=%d err=%v", mid, err)
		} else if member != nil {
			profile.Mid = member.Mid
			profile.Name = member.Name
			profile.Sex = member.Sex
			profile.Face = member.Face
			profile.Sign = member.Sign
			profile.Rank = member.Rank
			profile.Level = member.Level
			profile.Birthday = member.Birthday
			profile.Moral = member.Moral
			if member.Official != nil {
				profile.Official = member.Official
			}
		}
		// 实名状态：0 未认证、1 已认证。失败时不阻塞主流程。
		if status, err := r.userProfile.RealnameStatus(ctx, mid); err != nil {
			logx.Errorf("account/raw: user-profile.RealnameStatus mid=%d err=%v", mid, err)
		} else {
			profile.Identification = status
		}
	}

	// 本地 account 表的状态字段
	if acc, err := r.accountModel.FindOne(ctx, mid); err != nil {
		logx.Errorf("account/raw: accountModel.FindOne mid=%d err=%v", mid, err)
	} else if acc != nil {
		profile.IsTourist = acc.IsTourist
		profile.JoinTime = int32(acc.CreatedAt)
		if acc.Status == 1 {
			profile.Silence = 1
		}
	}

	// 本地 account_credential 表的绑定状态
	if creds, err := r.credentialModel.FindByMid(ctx, mid); err != nil {
		logx.Errorf("account/raw: credentialModel.FindByMid mid=%d err=%v", mid, err)
	} else {
		for _, c := range creds {
			if c.Status != 0 {
				continue
			}
			switch c.CredentialType {
			case 2: // 手机
				profile.TelStatus = 1
			case 3: // 邮箱
				profile.EmailStatus = 1
			}
		}
	}
	return profile, nil
}

// RawVip 返回用户的会员信息。
// 依据 AGENTS.md §1 商业化范围外约束，会员属于商业化范围外，降级返回零值。
func (r *Repository) RawVip(ctx context.Context, mid int64) (*rpc.VipInfo, error) {
	return &rpc.VipInfo{}, nil
}

// RawVips 批量返回用户的会员信息。
// 依据 AGENTS.md §1 商业化范围外约束，会员属于商业化范围外，降级返回零值。
func (r *Repository) RawVips(ctx context.Context, mids []int64) (map[int64]*rpc.VipInfo, error) {
	result := make(map[int64]*rpc.VipInfo, len(mids))
	for _, mid := range mids {
		result[mid] = &rpc.VipInfo{}
	}
	return result, nil
}

// LevelExp 从 user-profile 服务获取等级经验信息。
func (r *Repository) LevelExp(ctx context.Context, mid int64) (*rpc.LevelInfo, error) {
	if r.userProfile == nil {
		return &rpc.LevelInfo{}, nil
	}
	le, err := r.userProfile.LevelExp(ctx, mid)
	if err != nil {
		logx.Errorf("account/raw: user-profile.LevelExp mid=%d err=%v", mid, err)
		return &rpc.LevelInfo{}, nil
	}
	if le == nil {
		return &rpc.LevelInfo{}, nil
	}
	return le, nil
}
