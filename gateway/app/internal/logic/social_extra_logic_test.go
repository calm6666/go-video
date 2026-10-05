package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	socialgraphrpc "go-video/services/social-graph/rpc"

	"google.golang.org/grpc"
)

// 本文件只覆盖网关侧 social-graph 新增路由的口径：请求字段怎么装配、
// 下游回包怎么投影成 HTTP types、错误是否原样上抛。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeSocialGraphClient struct {
	socialgraphrpc.SocialGraphClient

	blackReq    *socialgraphrpc.BlackReq
	relationReq *socialgraphrpc.RelationReq
	listReq     *socialgraphrpc.ListReq
	specialReq  *socialgraphrpc.SpecialReq

	relationReply *socialgraphrpc.RelationReply
	blacksReply   *socialgraphrpc.BlacksReply
	err           error
	calls         int
}

func (f *fakeSocialGraphClient) AddBlack(_ context.Context, in *socialgraphrpc.BlackReq,
	_ ...grpc.CallOption) (*socialgraphrpc.EmptyReply, error) {
	f.calls++
	f.blackReq = in
	return &socialgraphrpc.EmptyReply{}, f.err
}

func (f *fakeSocialGraphClient) DelBlack(_ context.Context, in *socialgraphrpc.BlackReq,
	_ ...grpc.CallOption) (*socialgraphrpc.EmptyReply, error) {
	f.calls++
	f.blackReq = in
	return &socialgraphrpc.EmptyReply{}, f.err
}

func (f *fakeSocialGraphClient) IsBlacked(_ context.Context, in *socialgraphrpc.RelationReq,
	_ ...grpc.CallOption) (*socialgraphrpc.RelationReply, error) {
	f.calls++
	f.relationReq = in
	return f.relationReply, f.err
}

func (f *fakeSocialGraphClient) ListBlacks(_ context.Context, in *socialgraphrpc.ListReq,
	_ ...grpc.CallOption) (*socialgraphrpc.BlacksReply, error) {
	f.calls++
	f.listReq = in
	return f.blacksReply, f.err
}

func (f *fakeSocialGraphClient) AddSpecial(_ context.Context, in *socialgraphrpc.SpecialReq,
	_ ...grpc.CallOption) (*socialgraphrpc.EmptyReply, error) {
	f.calls++
	f.specialReq = in
	return &socialgraphrpc.EmptyReply{}, f.err
}

func (f *fakeSocialGraphClient) DelSpecial(_ context.Context, in *socialgraphrpc.SpecialReq,
	_ ...grpc.CallOption) (*socialgraphrpc.EmptyReply, error) {
	f.calls++
	f.specialReq = in
	return &socialgraphrpc.EmptyReply{}, f.err
}

func TestSocialAddBlackForwardsAllFields(t *testing.T) {
	fake := &fakeSocialGraphClient{}
	l := NewAddBlackLogic(context.Background(), &svc.ServiceContext{SocialGraph: fake})
	resp, err := l.AddBlack(&types.ParamAddBlack{Mid: 11, BlackMid: 22, IP: "10.0.0.1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.blackReq.GetMid() != 11 || fake.blackReq.GetBlackMid() != 22 || fake.blackReq.GetRealIp() != "10.0.0.1" {
		t.Fatalf("拉黑入参未完整透传: %+v", fake.blackReq)
	}
	// 拉黑自动取关由 social-graph 负责，网关只回信封，不回任何关系快照。
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
}

func TestSocialDelBlackIsIdempotentPassthrough(t *testing.T) {
	fake := &fakeSocialGraphClient{}
	l := NewDelBlackLogic(context.Background(), &svc.ServiceContext{SocialGraph: fake})
	if _, err := l.DelBlack(&types.ParamAddBlack{Mid: 11, BlackMid: 22, IP: "10.0.0.2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.calls != 1 || fake.blackReq.GetBlackMid() != 22 || fake.blackReq.GetRealIp() != "10.0.0.2" {
		t.Fatalf("取消拉黑入参 = %+v calls=%d", fake.blackReq, fake.calls)
	}
}

func TestSocialIsBlackedProjectsBool(t *testing.T) {
	fake := &fakeSocialGraphClient{relationReply: &socialgraphrpc.RelationReply{Following: true}}
	l := NewIsBlackedLogic(context.Background(), &svc.ServiceContext{SocialGraph: fake})
	resp, err := l.IsBlacked(&types.ParamRelation{Mid: 11, Owner: 33, IP: "10.0.0.3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.relationReq.GetMid() != 11 || fake.relationReq.GetOwner() != 33 || fake.relationReq.GetRealIp() != "10.0.0.3" {
		t.Fatalf("查询入参未透传: %+v", fake.relationReq)
	}
	if !resp.Data.Following {
		t.Fatalf("data = %+v, want following=true（下游结论不得被网关改写）", resp.Data)
	}
}

func TestSocialListBlacksProjectsRepeatedItems(t *testing.T) {
	fake := &fakeSocialGraphClient{blacksReply: &socialgraphrpc.BlacksReply{
		Total: 2,
		Items: []*socialgraphrpc.RelationItem{
			{Mid: 7, Ctime: 100, Attr: int32(socialgraphrpc.RelationAttr_RELATION_ATTR_BLACKED)},
			nil,
			{Mid: 8, Ctime: 200, Attr: int32(socialgraphrpc.RelationAttr_RELATION_ATTR_MUTUAL)},
		},
	}}
	l := NewListBlacksLogic(context.Background(), &svc.ServiceContext{SocialGraph: fake})
	resp, err := l.ListBlacks(&types.ParamListBlacks{Mid: 11, Pn: 2, Ps: 20, IP: "10.0.0.4"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.listReq.GetMid() != 11 || fake.listReq.GetPn() != 2 || fake.listReq.GetPs() != 20 ||
		fake.listReq.GetRealIp() != "10.0.0.4" {
		t.Fatalf("分页入参未原样透传: %+v", fake.listReq)
	}
	if resp.Data.Total != 2 {
		t.Fatalf("total = %d, want 2（总数来自服务端）", resp.Data.Total)
	}
	// 列表里的 nil 元素被跳过，attr 枚举值原样透出供客户端区分互关/拉黑。
	if len(resp.Data.Items) != 2 || resp.Data.Items[0].Attr != 4 || resp.Data.Items[1].Mid != 8 ||
		resp.Data.Items[1].Ctime != 200 {
		t.Fatalf("items = %+v", resp.Data.Items)
	}
}

func TestSocialListBlacksNilReplyYieldsEmptySlice(t *testing.T) {
	fake := &fakeSocialGraphClient{blacksReply: &socialgraphrpc.BlacksReply{Total: 0}}
	l := NewListBlacksLogic(context.Background(), &svc.ServiceContext{SocialGraph: fake})
	resp, err := l.ListBlacks(&types.ParamListBlacks{Mid: 11})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.Items == nil || len(resp.Data.Items) != 0 {
		t.Fatalf("items = %v, want 空切片（客户端应拿到 [] 而非 null）", resp.Data.Items)
	}
}

func TestSocialAddSpecialRequiresFollow(t *testing.T) {
	sentinel := errors.New("socialgraph: special need follow")
	fake := &fakeSocialGraphClient{err: sentinel}
	l := NewAddSpecialLogic(context.Background(), &svc.ServiceContext{SocialGraph: fake})
	// 「须先关注」（ErrSpecialNeedFollow）由 social-graph 判定，网关必须原样上抛。
	if _, err := l.AddSpecial(&types.ParamAddSpecial{Mid: 11, SpecialMid: 44, IP: "10.0.0.5"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}
	if fake.specialReq.GetMid() != 11 || fake.specialReq.GetSpecialMid() != 44 || fake.specialReq.GetRealIp() != "10.0.0.5" {
		t.Fatalf("特别关注入参未透传: %+v", fake.specialReq)
	}
}

func TestSocialDelSpecialSuccess(t *testing.T) {
	fake := &fakeSocialGraphClient{}
	l := NewDelSpecialLogic(context.Background(), &svc.ServiceContext{SocialGraph: fake})
	resp, err := l.DelSpecial(&types.ParamAddSpecial{Mid: 11, SpecialMid: 44, IP: "10.0.0.6"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.Data != (types.EmptyData{}) || resp.TTL != 0 {
		t.Fatalf("信封 = %+v", resp)
	}
}

func TestSocialExtraLogicsPropagateDownstreamError(t *testing.T) {
	sentinel := errors.New("socialgraph: unavailable")
	cases := []struct {
		name string
		call func(*svc.ServiceContext) error
	}{
		{"addBlack", func(sc *svc.ServiceContext) error {
			_, err := NewAddBlackLogic(context.Background(), sc).AddBlack(&types.ParamAddBlack{Mid: 1, BlackMid: 2})
			return err
		}},
		{"delBlack", func(sc *svc.ServiceContext) error {
			_, err := NewDelBlackLogic(context.Background(), sc).DelBlack(&types.ParamAddBlack{Mid: 1, BlackMid: 2})
			return err
		}},
		{"isBlacked", func(sc *svc.ServiceContext) error {
			_, err := NewIsBlackedLogic(context.Background(), sc).IsBlacked(&types.ParamRelation{Mid: 1, Owner: 2})
			return err
		}},
		{"listBlacks", func(sc *svc.ServiceContext) error {
			_, err := NewListBlacksLogic(context.Background(), sc).ListBlacks(&types.ParamListBlacks{Mid: 1})
			return err
		}},
		{"addSpecial", func(sc *svc.ServiceContext) error {
			_, err := NewAddSpecialLogic(context.Background(), sc).AddSpecial(&types.ParamAddSpecial{Mid: 1, SpecialMid: 2})
			return err
		}},
		{"delSpecial", func(sc *svc.ServiceContext) error {
			_, err := NewDelSpecialLogic(context.Background(), sc).DelSpecial(&types.ParamAddSpecial{Mid: 1, SpecialMid: 2})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeSocialGraphClient{
				err:           sentinel,
				relationReply: &socialgraphrpc.RelationReply{},
				blacksReply:   &socialgraphrpc.BlacksReply{},
			}
			if err := c.call(&svc.ServiceContext{SocialGraph: fake}); !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want %v（不能吞错后返回免错误空响应）", err, sentinel)
			}
		})
	}
}

func TestSocialExtraLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func() error
	}{
		{"addBlack", func() error {
			_, err := NewAddBlackLogic(context.Background(), empty).AddBlack(&types.ParamAddBlack{Mid: 1, BlackMid: 2})
			return err
		}},
		{"delBlack", func() error {
			_, err := NewDelBlackLogic(context.Background(), empty).DelBlack(&types.ParamAddBlack{Mid: 1, BlackMid: 2})
			return err
		}},
		{"isBlacked", func() error {
			_, err := NewIsBlackedLogic(context.Background(), empty).IsBlacked(&types.ParamRelation{Mid: 1, Owner: 2})
			return err
		}},
		{"listBlacks", func() error {
			_, err := NewListBlacksLogic(context.Background(), empty).ListBlacks(&types.ParamListBlacks{Mid: 1})
			return err
		}},
		{"addSpecial", func() error {
			_, err := NewAddSpecialLogic(context.Background(), empty).AddSpecial(&types.ParamAddSpecial{Mid: 1, SpecialMid: 2})
			return err
		}},
		{"delSpecial", func() error {
			_, err := NewDelSpecialLogic(context.Background(), empty).DelSpecial(&types.ParamAddSpecial{Mid: 1, SpecialMid: 2})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil || err.Error() != "social-graph service not configured" {
				t.Fatalf("%s：未配置 SocialGraphRPC 时必须报 not configured，got %v", c.name, err)
			}
		})
	}
}
