package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	engagementrpc "go-video/services/engagement/rpc"

	"google.golang.org/grpc"
)

// 本文件只覆盖网关侧 engagement 新增路由的口径：收藏状态/收藏夹/分享/点赞状态的
// 请求装配与回包投影（含 LikeState 枚举转 int32、map 与 repeated 字段）。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeEngagementClient struct {
	engagementrpc.EngagementClient

	isFavoredReq    *engagementrpc.IsFavoredReq
	isFavoredsReq   *engagementrpc.IsFavoredsReq
	addFolderReq    *engagementrpc.AddFolderReq
	delFolderReq    *engagementrpc.DelFolderReq
	addShareReq     *engagementrpc.AddShareReq
	hasLikeReq      *engagementrpc.HasLikeReq
	userLikesReq    *engagementrpc.UserLikesReq
	isFavoredReply  *engagementrpc.IsFavoredReply
	isFavoredsReply *engagementrpc.IsFavoredsReply
	addFolderReply  *engagementrpc.AddFolderReply
	addShareReply   *engagementrpc.AddShareReply
	hasLikeReply    *engagementrpc.HasLikeReply
	userLikesReply  *engagementrpc.UserLikesReply
	err             error
	calls           int
}

func (f *fakeEngagementClient) IsFavored(_ context.Context, in *engagementrpc.IsFavoredReq,
	_ ...grpc.CallOption) (*engagementrpc.IsFavoredReply, error) {
	f.calls++
	f.isFavoredReq = in
	return f.isFavoredReply, f.err
}

func (f *fakeEngagementClient) IsFavoreds(_ context.Context, in *engagementrpc.IsFavoredsReq,
	_ ...grpc.CallOption) (*engagementrpc.IsFavoredsReply, error) {
	f.calls++
	f.isFavoredsReq = in
	return f.isFavoredsReply, f.err
}

func (f *fakeEngagementClient) AddFolder(_ context.Context, in *engagementrpc.AddFolderReq,
	_ ...grpc.CallOption) (*engagementrpc.AddFolderReply, error) {
	f.calls++
	f.addFolderReq = in
	return f.addFolderReply, f.err
}

func (f *fakeEngagementClient) DelFolder(_ context.Context, in *engagementrpc.DelFolderReq,
	_ ...grpc.CallOption) (*engagementrpc.EmptyReply, error) {
	f.calls++
	f.delFolderReq = in
	return &engagementrpc.EmptyReply{}, f.err
}

func (f *fakeEngagementClient) AddShare(_ context.Context, in *engagementrpc.AddShareReq,
	_ ...grpc.CallOption) (*engagementrpc.AddShareReply, error) {
	f.calls++
	f.addShareReq = in
	return f.addShareReply, f.err
}

func (f *fakeEngagementClient) HasLike(_ context.Context, in *engagementrpc.HasLikeReq,
	_ ...grpc.CallOption) (*engagementrpc.HasLikeReply, error) {
	f.calls++
	f.hasLikeReq = in
	return f.hasLikeReply, f.err
}

func (f *fakeEngagementClient) UserLikes(_ context.Context, in *engagementrpc.UserLikesReq,
	_ ...grpc.CallOption) (*engagementrpc.UserLikesReply, error) {
	f.calls++
	f.userLikesReq = in
	return f.userLikesReply, f.err
}

func TestEngagementIsFavoredProjectsSingleState(t *testing.T) {
	fake := &fakeEngagementClient{isFavoredReply: &engagementrpc.IsFavoredReply{Faved: true}}
	l := NewIsFavoredLogic(context.Background(), &svc.ServiceContext{Engagement: fake})
	resp, err := l.IsFavored(&types.ParamIsFavored{Tp: 2, Mid: 11, Oid: 100})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.isFavoredReq.GetTp() != 2 || fake.isFavoredReq.GetMid() != 11 || fake.isFavoredReq.GetOid() != 100 {
		t.Fatalf("收藏状态入参未透传: %+v", fake.isFavoredReq)
	}
	if !resp.Data.Faved || resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestEngagementIsFavoredsProjectsMapAndLimit(t *testing.T) {
	fake := &fakeEngagementClient{isFavoredsReply: &engagementrpc.IsFavoredsReply{
		Faveds: map[int64]bool{100: true, 101: false},
	}}
	l := NewIsFavoredsLogic(context.Background(), &svc.ServiceContext{Engagement: fake})
	resp, err := l.IsFavoreds(&types.ParamIsFavoreds{Tp: 2, Mid: 11, Oids: []int64{100, 101, 102}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.isFavoredsReq.GetOids()) != 3 || fake.isFavoredsReq.GetTp() != 2 {
		t.Fatalf("oids 未原样透传: %+v", fake.isFavoredsReq)
	}
	// false 也要保留：缺失 key 与显式 false 的区分由 engagement 决定，网关不裁剪。
	if len(resp.Data.Faveds) != 2 || !resp.Data.Faveds[100] || resp.Data.Faveds[101] {
		t.Fatalf("faveds = %+v", resp.Data.Faveds)
	}

	// 超过 proto 声明的硬批量上限（100）时网关先拦，不打下游。
	over := &fakeEngagementClient{isFavoredsReply: &engagementrpc.IsFavoredsReply{}}
	ol := NewIsFavoredsLogic(context.Background(), &svc.ServiceContext{Engagement: over})
	if _, err = ol.IsFavoreds(&types.ParamIsFavoreds{Oids: make([]int64, isFavoredsMaxOids+1)}); err == nil {
		t.Fatal("oids 超上限应被拒绝")
	}
	if over.calls != 0 {
		t.Fatalf("预检失败不得调用下游，实际调用 %d 次", over.calls)
	}
}

func TestEngagementAddFolderReturnsServerFid(t *testing.T) {
	fake := &fakeEngagementClient{addFolderReply: &engagementrpc.AddFolderReply{Fid: 555}}
	l := NewAddFolderLogic(context.Background(), &svc.ServiceContext{Engagement: fake})
	resp, err := l.AddFolder(&types.ParamAddFolder{
		Tp: 2, Mid: 11, Name: "工作素材", Description: "d", Cover: "https://cdn/1.jpg", Public: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := fake.addFolderReq
	if req.GetName() != "工作素材" || req.GetDescription() != "d" || req.GetCover() != "https://cdn/1.jpg" ||
		req.GetPublic() != 1 || req.GetTp() != 2 || req.GetMid() != 11 {
		t.Fatalf("收藏夹入参未透传: %+v", req)
	}
	if resp.Data.Fid != 555 || resp.Data.Shares != 0 {
		t.Fatalf("data = %+v（fid 只能来自服务端；本路由不产分享数）", resp.Data)
	}
}

func TestEngagementDelFolderIsVoid(t *testing.T) {
	fake := &fakeEngagementClient{}
	l := NewDelFolderLogic(context.Background(), &svc.ServiceContext{Engagement: fake})
	resp, err := l.DelFolder(&types.ParamDelFolder{Tp: 2, Mid: 11, Fid: 555})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.delFolderReq.GetFid() != 555 || fake.delFolderReq.GetMid() != 11 || fake.delFolderReq.GetTp() != 2 {
		t.Fatalf("删除收藏夹入参未透传: %+v", fake.delFolderReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = %+v", resp)
	}
}

func TestEngagementAddShareReturnsServerCount(t *testing.T) {
	fake := &fakeEngagementClient{addShareReply: &engagementrpc.AddShareReply{Shares: 42}}
	l := NewAddShareLogic(context.Background(), &svc.ServiceContext{Engagement: fake})
	resp, err := l.AddShare(&types.ParamAddShare{Oid: 100, Mid: 11, Type: 2, IP: "10.0.0.7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.addShareReq.GetOid() != 100 || fake.addShareReq.GetType() != 2 || fake.addShareReq.GetIp() != "10.0.0.7" {
		t.Fatalf("分享入参未透传: %+v", fake.addShareReq)
	}
	if resp.Data.Shares != 42 || resp.Data.Fid != 0 {
		t.Fatalf("data = %+v（分享数来自服务端，网关不做自增）", resp.Data)
	}
}

func TestEngagementHasLikeConvertsLikeStateEnum(t *testing.T) {
	fake := &fakeEngagementClient{hasLikeReply: &engagementrpc.HasLikeReply{
		States: map[int64]*engagementrpc.UserLikeState{
			200: {Mid: 11, Time: 1001, State: engagementrpc.LikeState_STATE_LIKE},
			201: {Mid: 11, Time: 1002, State: engagementrpc.LikeState_STATE_DISLIKE},
			202: nil,
		},
	}}
	l := NewHasLikeLogic(context.Background(), &svc.ServiceContext{Engagement: fake})
	resp, err := l.HasLike(&types.ParamHasLike{Business: "archive", MessageIds: []int64{200, 201}, Mid: 11, IP: "10.0.0.8"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.hasLikeReq.GetBusiness() != "archive" || len(fake.hasLikeReq.GetMessageIds()) != 2 ||
		fake.hasLikeReq.GetMid() != 11 || fake.hasLikeReq.GetIp() != "10.0.0.8" {
		t.Fatalf("has_like 入参未透传: %+v", fake.hasLikeReq)
	}
	if len(resp.Data.States) != 3 {
		t.Fatalf("states = %+v", resp.Data.States)
	}
	if resp.Data.States[200].State != 1 || resp.Data.States[201].State != 2 {
		t.Fatalf("LikeState 枚举未正确转 int32: %+v", resp.Data.States)
	}
	if resp.Data.States[200].Time != 1001 || resp.Data.States[200].Mid != 11 {
		t.Fatalf("状态字段丢失: %+v", resp.Data.States[200])
	}
	if resp.Data.States[202] != (types.EngagementLikeStateData{}) {
		t.Fatalf("nil 元素应投影为零值: %+v", resp.Data.States[202])
	}
}

func TestEngagementUserLikesProjectsPaging(t *testing.T) {
	fake := &fakeEngagementClient{userLikesReply: &engagementrpc.UserLikesReply{
		Total: 2,
		Items: []*engagementrpc.ItemRecord{
			{MessageId: 200, Time: 1001},
			{MessageId: 201, Time: 1002},
		},
	}}
	l := NewUserLikesLogic(context.Background(), &svc.ServiceContext{Engagement: fake})
	resp, err := l.UserLikes(&types.ParamUserLikes{Business: "archive", Mid: 11, Pn: 2, Ps: 20, IP: "10.0.0.9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.userLikesReq.GetPn() != 2 || fake.userLikesReq.GetPs() != 20 || fake.userLikesReq.GetIp() != "10.0.0.9" {
		t.Fatalf("分页入参未原样透传: %+v", fake.userLikesReq)
	}
	if resp.Data.Total != 2 || len(resp.Data.Items) != 2 ||
		resp.Data.Items[1].MessageId != 201 || resp.Data.Items[1].Time != 1002 {
		t.Fatalf("data = %+v", resp.Data)
	}
}

func TestEngagementExtraProjectionsNilSafe(t *testing.T) {
	if got := toEngagementFaveds(nil); got == nil || len(got) != 0 {
		t.Fatalf("toEngagementFaveds(nil) = %v, want 空 map", got)
	}
	if got := toEngagementLikeStates(nil); got == nil || len(got) != 0 {
		t.Fatalf("toEngagementLikeStates(nil) = %v, want 空 map", got)
	}
	got := toEngagementLikeItems([]*engagementrpc.ItemRecord{nil, {MessageId: 5}})
	if len(got) != 1 || got[0].MessageId != 5 {
		t.Fatalf("toEngagementLikeItems = %+v, want 跳过 nil 元素", got)
	}
}

func TestEngagementExtraLogicsPropagateDownstreamError(t *testing.T) {
	sentinel := errors.New("engagement: unavailable")
	cases := []struct {
		name string
		call func(*svc.ServiceContext) error
	}{
		{"isFavored", func(sc *svc.ServiceContext) error {
			_, err := NewIsFavoredLogic(context.Background(), sc).IsFavored(&types.ParamIsFavored{Tp: 2, Mid: 1})
			return err
		}},
		{"isFavoreds", func(sc *svc.ServiceContext) error {
			_, err := NewIsFavoredsLogic(context.Background(), sc).IsFavoreds(&types.ParamIsFavoreds{Tp: 2, Mid: 1})
			return err
		}},
		{"addFolder", func(sc *svc.ServiceContext) error {
			_, err := NewAddFolderLogic(context.Background(), sc).AddFolder(&types.ParamAddFolder{Mid: 1, Name: "n"})
			return err
		}},
		{"delFolder", func(sc *svc.ServiceContext) error {
			_, err := NewDelFolderLogic(context.Background(), sc).DelFolder(&types.ParamDelFolder{Mid: 1, Fid: 2})
			return err
		}},
		{"addShare", func(sc *svc.ServiceContext) error {
			_, err := NewAddShareLogic(context.Background(), sc).AddShare(&types.ParamAddShare{Oid: 1, Mid: 2})
			return err
		}},
		{"hasLike", func(sc *svc.ServiceContext) error {
			_, err := NewHasLikeLogic(context.Background(), sc).HasLike(&types.ParamHasLike{Mid: 1})
			return err
		}},
		{"userLikes", func(sc *svc.ServiceContext) error {
			_, err := NewUserLikesLogic(context.Background(), sc).UserLikes(&types.ParamUserLikes{Mid: 1})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeEngagementClient{
				err:             sentinel,
				isFavoredReply:  &engagementrpc.IsFavoredReply{},
				isFavoredsReply: &engagementrpc.IsFavoredsReply{},
				addFolderReply:  &engagementrpc.AddFolderReply{},
				addShareReply:   &engagementrpc.AddShareReply{},
				hasLikeReply:    &engagementrpc.HasLikeReply{},
				userLikesReply:  &engagementrpc.UserLikesReply{},
			}
			if err := c.call(&svc.ServiceContext{Engagement: fake}); !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want %v（不能吞错后返回成功信封）", err, sentinel)
			}
		})
	}
}

func TestEngagementExtraLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func() error
	}{
		{"isFavored", func() error {
			_, err := NewIsFavoredLogic(context.Background(), empty).IsFavored(&types.ParamIsFavored{Tp: 2, Mid: 1})
			return err
		}},
		{"isFavoreds", func() error {
			_, err := NewIsFavoredsLogic(context.Background(), empty).IsFavoreds(&types.ParamIsFavoreds{Tp: 2, Mid: 1})
			return err
		}},
		{"addFolder", func() error {
			_, err := NewAddFolderLogic(context.Background(), empty).AddFolder(&types.ParamAddFolder{Mid: 1, Name: "n"})
			return err
		}},
		{"delFolder", func() error {
			_, err := NewDelFolderLogic(context.Background(), empty).DelFolder(&types.ParamDelFolder{Mid: 1, Fid: 2})
			return err
		}},
		{"addShare", func() error {
			_, err := NewAddShareLogic(context.Background(), empty).AddShare(&types.ParamAddShare{Oid: 1, Mid: 2})
			return err
		}},
		{"hasLike", func() error {
			_, err := NewHasLikeLogic(context.Background(), empty).HasLike(&types.ParamHasLike{Mid: 1})
			return err
		}},
		{"userLikes", func() error {
			_, err := NewUserLikesLogic(context.Background(), empty).UserLikes(&types.ParamUserLikes{Mid: 1})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil || err.Error() != "engagement service not configured" {
				t.Fatalf("%s：未配置 EngagementRPC 时必须报 not configured，got %v", c.name, err)
			}
		})
	}
}
