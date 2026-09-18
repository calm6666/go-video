package repository

// 本文件是 account 服务对 user-profile 服务的 gRPC 客户端适配器。
// account 只通过 user-profile 的公开 RPC 契约访问数据（AGENTS.md §5），
// 不引入 user-profile 的内部 model 包。
// 对应参考仓库 member 服务的 gorpc 客户端（Base/Bases/Member/Members/Exp/
// RealnameStatus/AddExp/AddMoral）。

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"

	accountrpc "go-video/services/account/rpc"
	userprofilerc "go-video/services/user-profile/rpc"
)

// userProfileClient 通过 zrpc 调用 user-profile 服务，实现 UserProfileClient 接口。
type userProfileClient struct {
	cli userprofilerc.UserProfileClient
}

// NewUserProfileClient 构造 user-profile 客户端适配器。
func NewUserProfileClient(c zrpc.RpcClientConf) UserProfileClient {
	conn := zrpc.MustNewClient(c)
	return &userProfileClient{cli: userprofilerc.NewUserProfileClient(conn.Conn())}
}

// sexStr 把 user-profile 的数字性别转换为展示字符串。
// 规则与 user-profile model.SexStr 一致（此处按契约复制，禁止 import 其内部包）。
func sexStr(sex int64) string {
	switch sex {
	case 1:
		return "男"
	case 2:
		return "女"
	default:
		return "保密"
	}
}

// Base 查询单个用户基础资料。
func (c *userProfileClient) Base(ctx context.Context, mid int64) (*UserProfileBase, error) {
	reply, err := c.cli.Base(ctx, &userprofilerc.MemberMidReq{Mid: mid})
	if err != nil {
		return nil, err
	}
	if reply == nil || reply.Mid == 0 {
		// mid=0 为 user-profile 的防击穿哨兵，表示用户不存在
		return nil, nil
	}
	return &UserProfileBase{
		Mid:  reply.Mid,
		Name: reply.Name,
		Sex:  sexStr(reply.Sex),
		Face: reply.Face,
		Sign: reply.Sign,
		Rank: int32(reply.Rank),
	}, nil
}

// Bases 批量查询用户基础资料。
func (c *userProfileClient) Bases(ctx context.Context, mids []int64) (map[int64]*UserProfileBase, error) {
	reply, err := c.cli.Bases(ctx, &userprofilerc.MemberMidsReq{Mids: mids})
	if err != nil {
		return nil, err
	}
	result := make(map[int64]*UserProfileBase, len(mids))
	for mid, b := range reply.GetBaseInfos() {
		if b == nil || b.Mid == 0 {
			continue
		}
		result[mid] = &UserProfileBase{
			Mid:  b.Mid,
			Name: b.Name,
			Sex:  sexStr(b.Sex),
			Face: b.Face,
			Sign: b.Sign,
			Rank: int32(b.Rank),
		}
	}
	return result, nil
}

// Member 查询单个用户完整资料（基础+等级+官方认证+节操值）。
func (c *userProfileClient) Member(ctx context.Context, mid int64) (*UserProfileMember, error) {
	reply, err := c.cli.Member(ctx, &userprofilerc.MemberMidReq{Mid: mid})
	if err != nil {
		return nil, err
	}
	if reply == nil || reply.BaseInfo == nil || reply.BaseInfo.Mid == 0 {
		return nil, nil
	}
	m := &UserProfileMember{
		UserProfileBase: UserProfileBase{
			Mid:  reply.BaseInfo.Mid,
			Name: reply.BaseInfo.Name,
			Sex:  sexStr(reply.BaseInfo.Sex),
			Face: reply.BaseInfo.Face,
			Sign: reply.BaseInfo.Sign,
			Rank: int32(reply.BaseInfo.Rank),
		},
		Birthday: reply.BaseInfo.Birthday,
	}
	if reply.LevelInfo != nil {
		m.Level = reply.LevelInfo.Cur
	}
	if reply.OfficialInfo != nil {
		m.Official = &accountrpc.OfficialInfo{
			Role:  reply.OfficialInfo.Role,
			Title: reply.OfficialInfo.Title,
			Desc:  reply.OfficialInfo.Desc,
		}
	}
	// 节操值由 user-profile 的 Moral 接口补充（参考仓库 member.Moral 的聚合方式）；
	// 失败时降级为 0，不阻塞主流程。
	if moralReply, err := c.cli.Moral(ctx, &userprofilerc.MemberMidReq{Mid: mid}); err != nil {
		logx.Errorf("account/userprofile-adapter: Moral mid=%d err=%v", mid, err)
	} else if moralReply != nil {
		// 与参考仓库一致：对外节操值 = 内部值 / 100
		m.Moral = int32(moralReply.Moral / 100)
	}
	return m, nil
}

// Members 批量查询用户完整资料。
func (c *userProfileClient) Members(ctx context.Context, mids []int64) (map[int64]*UserProfileMember, error) {
	reply, err := c.cli.Members(ctx, &userprofilerc.MemberMidsReq{Mids: mids})
	if err != nil {
		return nil, err
	}
	result := make(map[int64]*UserProfileMember, len(mids))
	for mid, r := range reply.GetMemberInfos() {
		if r == nil || r.BaseInfo == nil || r.BaseInfo.Mid == 0 {
			continue
		}
		m := &UserProfileMember{
			UserProfileBase: UserProfileBase{
				Mid:  r.BaseInfo.Mid,
				Name: r.BaseInfo.Name,
				Sex:  sexStr(r.BaseInfo.Sex),
				Face: r.BaseInfo.Face,
				Sign: r.BaseInfo.Sign,
				Rank: int32(r.BaseInfo.Rank),
			},
			Birthday: r.BaseInfo.Birthday,
		}
		if r.LevelInfo != nil {
			m.Level = r.LevelInfo.Cur
		}
		if r.OfficialInfo != nil {
			m.Official = &accountrpc.OfficialInfo{
				Role:  r.OfficialInfo.Role,
				Title: r.OfficialInfo.Title,
				Desc:  r.OfficialInfo.Desc,
			}
		}
		result[mid] = m
	}
	return result, nil
}

// LevelExp 查询等级经验信息。
func (c *userProfileClient) LevelExp(ctx context.Context, mid int64) (*accountrpc.LevelInfo, error) {
	reply, err := c.cli.Exp(ctx, &userprofilerc.MidReq{Mid: mid})
	if err != nil {
		return nil, err
	}
	if reply == nil {
		return &accountrpc.LevelInfo{}, nil
	}
	return &accountrpc.LevelInfo{
		Cur:     reply.Cur,
		Min:     reply.Min,
		NowExp:  reply.NowExp,
		NextExp: reply.NextExp,
	}, nil
}

// RealnameStatus 查询实名认证状态：0 未认证、1 已认证。
func (c *userProfileClient) RealnameStatus(ctx context.Context, mid int64) (int32, error) {
	reply, err := c.cli.RealnameStatus(ctx, &userprofilerc.MemberMidReq{Mid: mid})
	if err != nil {
		return 0, err
	}
	if reply == nil {
		return 0, nil
	}
	return reply.RealnameStatus, nil
}

// AddExp 增加经验值（委托 user-profile 的 UpdateExp，参考仓库 member.UpdateExp）。
// operater 在 member 契约中无对应字段，仅保留接口签名兼容。
func (c *userProfileClient) AddExp(ctx context.Context, mid int64, exp float64, operater, operate, reason string) error {
	_, err := c.cli.UpdateExp(ctx, &userprofilerc.AddExpReq{
		Mid:     mid,
		Count:   exp,
		Reason:  reason,
		Operate: operate,
		Ip:      "",
	})
	return err
}

// AddMoral 增加道德值（委托 user-profile 的 AddMoral，参考仓库 member.UpdateMoral）。
// 映射规则与参考仓库 service/exp.go 的 AddMoral 一致：
//
//	delta = moral × 100；delta<0 为违规惩罚、否则举报奖励；
//	操作人缺省为"系统"；原因类型固定为评论（ReplyReasonType=2）；is_notify=true。
func (c *userProfileClient) AddMoral(ctx context.Context, mid int64, moral float64, oper, reason, remark string) error {
	delta := int64(moral * 100)
	origin := int64(1) // 举报奖励
	if delta < 0 {
		origin = 2 // 违规惩罚
	}
	operator := oper
	if operator == "" {
		operator = "系统"
	}
	_, err := c.cli.AddMoral(ctx, &userprofilerc.UpdateMoralReq{
		Mid:        mid,
		Delta:      delta,
		Origin:     origin,
		Reason:     reason,
		ReasonType: 2, // 评论
		Operator:   operator,
		Remark:     remark,
		IsNotify:   true,
	})
	return err
}
