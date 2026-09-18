package repository

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

// PrivacyInfo 是 /privacy 接口返回的隐私信息聚合结构。
// 字段含义与参考仓库 model.Privacy 一致，便于客户端兼容。
type PrivacyInfo struct {
	Realname     string `json:"realname"`
	IdentityCard string `json:"identity_card"`
	IdentitySex  string `json:"identity_sex"`
	Tel          string `json:"tel"`
	RegIP        string `json:"reg_ip"`
	RegTS        int64  `json:"reg_ts"`
	HandIMG      string `json:"hand_img"`
}

// Privacy 查询用户隐私信息。
// 实现依据 AGENTS.md §5 数据所有权：
//   - RegIP、RegTS 来自本地 account 表（account 服务持有账号主表）；
//   - Tel 来自本地 account_credential 表（credential_type=手机）；
//   - Realname、IdentityCard、IdentitySex、HandIMG 属于实名认证数据，
//     应由 user-profile 服务提供；本期未实现该接口，对应字段降级为空字符串。
//
// 后续如需落地完整实名信息查询，需在 UserProfileClient 增加 RealnameDetail 方法并更新本说明。
func (r *Repository) Privacy(ctx context.Context, mid int64) (*PrivacyInfo, error) {
	info := &PrivacyInfo{}

	// 本地 account 表：注册 IP 和注册时间
	acc, err := r.accountModel.FindOne(ctx, mid)
	if err != nil {
		logx.Errorf("account/privacy: accountModel.FindOne mid=%d err=%v", mid, err)
	} else if acc != nil {
		info.RegIP = acc.RegIP
		info.RegTS = acc.CreatedAt
	}

	// 本地 account_credential 表：手机号
	creds, err := r.credentialModel.FindByMid(ctx, mid)
	if err != nil {
		logx.Errorf("account/privacy: credentialModel.FindByMid mid=%d err=%v", mid, err)
	} else {
		for _, c := range creds {
			if c.Status != 0 {
				continue
			}
			if c.CredentialType == model.CredentialTypePhone {
				info.Tel = c.Identifier
			}
		}
	}

	// 实名信息：本期未实现，对应字段保持空字符串
	// 当 user-profile 服务暴露 RealnameDetail 接口后，在此处聚合
	_ = ctx
	return info, nil
}

// ProfileWithStat 聚合 Profile 和统计信息（等级经验、关注数、粉丝数）。
// 依据 AGENTS.md §1 商业化范围外约束，coins 字段固定返回 0。
func (r *Repository) ProfileWithStat(ctx context.Context, mid int64) (*rpc.ProfileStatReply, error) {
	profile, err := r.Profile(ctx, mid)
	if err != nil {
		return nil, err
	}
	levelExp, err := r.LevelExp(ctx, mid)
	if err != nil {
		logx.Errorf("account/profilestat: LevelExp mid=%d err=%v", mid, err)
		levelExp = &rpc.LevelInfo{}
	}
	stat, err := r.Stat(ctx, mid)
	if err != nil {
		logx.Errorf("account/profilestat: Stat mid=%d err=%v", mid, err)
		stat = &RelationStat{}
	}
	return &rpc.ProfileStatReply{
		Profile:   profile,
		LevelInfo: levelExp,
		Coins:     0, // 商业化范围外，固定返回 0
		Following: stat.Following,
		Follower:  stat.Follower,
	}, nil
}
