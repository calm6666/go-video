package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	feedrpc "go-video/services/feed/rpc"

	"google.golang.org/grpc"
)

// 本文件只覆盖网关侧 feed 新增路由（置顶/取消置顶/删除）的口径：
// 三个写接口的 mid/feed_id/operator/real_ip 怎么透传，下游 EmptyReply 怎么变四字段信封。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeFeedClient struct {
	feedrpc.FeedClient

	pinReq    *feedrpc.PinFeedReq
	unpinReq  *feedrpc.UnpinFeedReq
	deleteReq *feedrpc.DeleteFeedReq
	err       error
	calls     int
}

func (f *fakeFeedClient) PinFeed(_ context.Context, in *feedrpc.PinFeedReq,
	_ ...grpc.CallOption) (*feedrpc.EmptyReply, error) {
	f.calls++
	f.pinReq = in
	return &feedrpc.EmptyReply{}, f.err
}

func (f *fakeFeedClient) UnpinFeed(_ context.Context, in *feedrpc.UnpinFeedReq,
	_ ...grpc.CallOption) (*feedrpc.EmptyReply, error) {
	f.calls++
	f.unpinReq = in
	return &feedrpc.EmptyReply{}, f.err
}

func (f *fakeFeedClient) DeleteFeed(_ context.Context, in *feedrpc.DeleteFeedReq,
	_ ...grpc.CallOption) (*feedrpc.EmptyReply, error) {
	f.calls++
	f.deleteReq = in
	return &feedrpc.EmptyReply{}, f.err
}

func TestFeedPinFeedForwardsOperatorAndRealIP(t *testing.T) {
	fake := &fakeFeedClient{}
	l := NewPinFeedLogic(context.Background(), &svc.ServiceContext{Feed: fake})
	resp, err := l.PinFeed(&types.ParamPinFeed{Mid: 11, FeedId: 900, Operator: "self", IP: "10.0.0.1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := fake.pinReq
	if req.GetMid() != 11 || req.GetFeedId() != 900 || req.GetOperator() != "self" || req.GetRealIp() != "10.0.0.1" {
		t.Fatalf("置顶入参未完整透传: %+v", req)
	}
	// 置顶位数量与归属校验在 feed，网关不返回投影后的动态列表。
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = %+v", resp)
	}
}

func TestFeedUnpinFeedIsIdempotentPassthrough(t *testing.T) {
	fake := &fakeFeedClient{}
	l := NewUnpinFeedLogic(context.Background(), &svc.ServiceContext{Feed: fake})
	if _, err := l.UnpinFeed(&types.ParamPinFeed{Mid: 11, FeedId: 900, Operator: "self", IP: "10.0.0.2"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.unpinReq.GetFeedId() != 900 || fake.unpinReq.GetOperator() != "self" ||
		fake.unpinReq.GetRealIp() != "10.0.0.2" {
		t.Fatalf("取消置顶入参未透传: %+v", fake.unpinReq)
	}
}

func TestFeedDeleteFeedForwardsOperatorForAudit(t *testing.T) {
	fake := &fakeFeedClient{}
	l := NewDeleteFeedLogic(context.Background(), &svc.ServiceContext{Feed: fake})
	resp, err := l.DeleteFeed(&types.ParamDeleteFeed{Mid: 11, FeedId: 900, Operator: "ops-7", IP: "10.0.0.3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 删除动态的写扩散撤销（粉丝收件箱清理、未读回退）由 feed 负责，
	// 网关只需把 operator 和 real_ip 传下去留审计证据。
	if fake.deleteReq.GetMid() != 11 || fake.deleteReq.GetFeedId() != 900 ||
		fake.deleteReq.GetOperator() != "ops-7" || fake.deleteReq.GetRealIp() != "10.0.0.3" {
		t.Fatalf("删除动态入参未透传: %+v", fake.deleteReq)
	}
	if resp.Data != (types.EmptyData{}) {
		t.Fatalf("data = %+v, want 空对象", resp.Data)
	}
}

func TestFeedExtraLogicsPropagateDownstreamError(t *testing.T) {
	sentinel := errors.New("feed: forbidden")
	cases := []struct {
		name string
		call func(*svc.ServiceContext) error
	}{
		{"pinFeed", func(sc *svc.ServiceContext) error {
			_, err := NewPinFeedLogic(context.Background(), sc).PinFeed(&types.ParamPinFeed{Mid: 1, FeedId: 2})
			return err
		}},
		{"unpinFeed", func(sc *svc.ServiceContext) error {
			_, err := NewUnpinFeedLogic(context.Background(), sc).UnpinFeed(&types.ParamPinFeed{Mid: 1, FeedId: 2})
			return err
		}},
		{"deleteFeed", func(sc *svc.ServiceContext) error {
			_, err := NewDeleteFeedLogic(context.Background(), sc).DeleteFeed(&types.ParamDeleteFeed{Mid: 1, FeedId: 2})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeFeedClient{err: sentinel}
			if err := c.call(&svc.ServiceContext{Feed: fake}); !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want %v（权限结论必须交给 httpresponse 渲染）", err, sentinel)
			}
		})
	}
}

func TestFeedExtraLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func() error
	}{
		{"pinFeed", func() error {
			_, err := NewPinFeedLogic(context.Background(), empty).PinFeed(&types.ParamPinFeed{Mid: 1, FeedId: 2})
			return err
		}},
		{"unpinFeed", func() error {
			_, err := NewUnpinFeedLogic(context.Background(), empty).UnpinFeed(&types.ParamPinFeed{Mid: 1, FeedId: 2})
			return err
		}},
		{"deleteFeed", func() error {
			_, err := NewDeleteFeedLogic(context.Background(), empty).DeleteFeed(&types.ParamDeleteFeed{Mid: 1, FeedId: 2})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil || err.Error() != "feed service not configured" {
				t.Fatalf("%s：未配置 FeedRPC 时必须报 not configured，got %v", c.name, err)
			}
		})
	}
}
