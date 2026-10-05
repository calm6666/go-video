package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	creatorrpc "go-video/services/creator/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 creator 的口径：mid/mids 与 from/state 如何透传、
// UpSpecial.group_ids（repeated）与 UpsSpecial.up_specials（map）如何投影成客户端 types。
// creator.proto 全部为标量 int32，无 protobuf 枚举，故 from/state 原样传递、不做类型转换。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeCreatorClient struct {
	creatorrpc.CreatorClient

	err   error
	calls int

	specialReq    *creatorrpc.UpSpecialReq
	specialReply  *creatorrpc.UpSpecialReply
	upsReq        *creatorrpc.UpsSpecialReq
	upsReply      *creatorrpc.UpsSpecialReply
	attrReq       *creatorrpc.UpAttrReq
	attrReply     *creatorrpc.UpAttrReply
	switchReq     *creatorrpc.UpSwitchReq
	switchReply   *creatorrpc.UpSwitchReply
	setSwitchReq  *creatorrpc.UpSwitchReq
	setSwitchCall int
}

func (f *fakeCreatorClient) UpSpecial(_ context.Context, in *creatorrpc.UpSpecialReq,
	_ ...grpc.CallOption) (*creatorrpc.UpSpecialReply, error) {
	f.calls++
	f.specialReq = in
	return f.specialReply, f.err
}

func (f *fakeCreatorClient) UpsSpecial(_ context.Context, in *creatorrpc.UpsSpecialReq,
	_ ...grpc.CallOption) (*creatorrpc.UpsSpecialReply, error) {
	f.calls++
	f.upsReq = in
	return f.upsReply, f.err
}

func (f *fakeCreatorClient) UpAttr(_ context.Context, in *creatorrpc.UpAttrReq,
	_ ...grpc.CallOption) (*creatorrpc.UpAttrReply, error) {
	f.calls++
	f.attrReq = in
	return f.attrReply, f.err
}

func (f *fakeCreatorClient) UpSwitch(_ context.Context, in *creatorrpc.UpSwitchReq,
	_ ...grpc.CallOption) (*creatorrpc.UpSwitchReply, error) {
	f.calls++
	f.switchReq = in
	return f.switchReply, f.err
}

func (f *fakeCreatorClient) SetUpSwitch(_ context.Context, in *creatorrpc.UpSwitchReq,
	_ ...grpc.CallOption) (*creatorrpc.EmptyReply, error) {
	f.calls++
	f.setSwitchCall++
	f.setSwitchReq = in
	return &creatorrpc.EmptyReply{}, f.err
}

func TestCreatorUpSpecialProjectsRepeatedGroupIds(t *testing.T) {
	fake := &fakeCreatorClient{specialReply: &creatorrpc.UpSpecialReply{
		UpSpecial: &creatorrpc.UpSpecial{GroupIds: []int64{7, 3, 11}},
	}}
	l := NewUpSpecialLogic(context.Background(), &svc.ServiceContext{Creator: fake})
	resp, err := l.UpSpecial(&types.ParamUpMid{Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.specialReq.GetMid() != 777 {
		t.Fatalf("mid = %d, want 777", fake.specialReq.GetMid())
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	// repeated 字段按 RPC 顺序原样投影，网关不排序不去重（分组语义属 creator 服务）。
	if len(resp.Data.UpSpecial.GroupIds) != 3 ||
		resp.Data.UpSpecial.GroupIds[0] != 7 || resp.Data.UpSpecial.GroupIds[1] != 3 ||
		resp.Data.UpSpecial.GroupIds[2] != 11 {
		t.Fatalf("group_ids = %v, want [7 3 11]", resp.Data.UpSpecial.GroupIds)
	}
	// up_special 为 nil 时也必须给空数组，客户端不用判 null。
	nilFake := &fakeCreatorClient{specialReply: &creatorrpc.UpSpecialReply{}}
	nilResp, err := NewUpSpecialLogic(context.Background(), &svc.ServiceContext{Creator: nilFake}).
		UpSpecial(&types.ParamUpMid{Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nilResp.Data.UpSpecial.GroupIds == nil || len(nilResp.Data.UpSpecial.GroupIds) != 0 {
		t.Fatalf("group_ids = %v, want 空切片", nilResp.Data.UpSpecial.GroupIds)
	}
}

func TestCreatorUpsSpecialProjectsMapAndEnforcesBatchLimit(t *testing.T) {
	fake := &fakeCreatorClient{upsReply: &creatorrpc.UpsSpecialReply{
		UpSpecials: map[int64]*creatorrpc.UpSpecial{
			777: {GroupIds: []int64{7}},
			888: {},
		},
	}}
	l := NewUpsSpecialLogic(context.Background(), &svc.ServiceContext{Creator: fake})
	resp, err := l.UpsSpecial(&types.ParamUpMids{Mids: []int64{777, 888, 999}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.upsReq.GetMids()) != 3 || fake.upsReq.GetMids()[2] != 999 {
		t.Fatalf("mids = %v, want 原样透传 3 个", fake.upsReq.GetMids())
	}
	// map 投影：命中的 mid 保留其分组，未命中的 mid 由 creator 省略，网关不补 999 的零值。
	if len(resp.Data.UpSpecials) != 2 {
		t.Fatalf("up_specials = %v, want 只含服务端返回的 2 个 mid", resp.Data.UpSpecials)
	}
	if _, ok := resp.Data.UpSpecials[999]; ok {
		t.Fatal("网关不得为未命中 mid 伪造条目")
	}
	if ids := resp.Data.UpSpecials[777].GroupIds; len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("mid=777 group_ids = %v", ids)
	}
	if got := resp.Data.UpSpecials[888].GroupIds; got == nil || len(got) != 0 {
		t.Fatalf("mid=888 group_ids = %v, want 空切片", got)
	}

	// 契约硬上限（obc validate max=100）：超量批次在网关拒绝，不放大下游查询。
	over := &fakeCreatorClient{}
	mids := make([]int64, creatorUpsSpecialMaxMids+1)
	_, limitErr := NewUpsSpecialLogic(context.Background(), &svc.ServiceContext{Creator: over}).
		UpsSpecial(&types.ParamUpMids{Mids: mids})
	if limitErr == nil {
		t.Fatal("mids 超上限应被拒绝")
	}
	if !strings.Contains(limitErr.Error(), "mids") {
		t.Fatalf("错误消息应点名超限字段，got %v", limitErr)
	}
	if over.calls != 0 {
		t.Fatalf("超限不得调用下游，实际调用 %d 次", over.calls)
	}
	// 上限本身放行。
	ok := &fakeCreatorClient{upsReply: &creatorrpc.UpsSpecialReply{}}
	if _, err := NewUpsSpecialLogic(context.Background(), &svc.ServiceContext{Creator: ok}).
		UpsSpecial(&types.ParamUpMids{Mids: mids[:creatorUpsSpecialMaxMids]}); err != nil {
		t.Fatalf("上限内请求被拒: %v", err)
	}
	if ok.calls != 1 {
		t.Fatalf("calls = %d, want 1", ok.calls)
	}
}

func TestCreatorUpAttrPassesFromAndIsAuthor(t *testing.T) {
	fake := &fakeCreatorClient{attrReply: &creatorrpc.UpAttrReply{IsAuthor: 1}}
	l := NewUpAttrLogic(context.Background(), &svc.ServiceContext{Creator: fake})
	resp, err := l.UpAttr(&types.ParamUpAttr{Mid: 777, From: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// from 的取值语义（0 稿件作者/1 移动投稿/2 直播/3 直播白名单）由 creator 解释。
	if fake.attrReq.GetMid() != 777 || fake.attrReq.GetFrom() != 2 {
		t.Fatalf("UpAttr 入参未透传: %+v", fake.attrReq)
	}
	if resp.Data.IsAuthor != 1 {
		t.Fatalf("is_author = %d, want 1", resp.Data.IsAuthor)
	}
}

func TestCreatorSwitchGetAndSetShareReqType(t *testing.T) {
	fake := &fakeCreatorClient{switchReply: &creatorrpc.UpSwitchReply{State: 1}}
	l := NewGetUpSwitchLogic(context.Background(), &svc.ServiceContext{Creator: fake})
	resp, err := l.GetUpSwitch(&types.ParamUpSwitch{Mid: 777, From: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 查询走 UpSwitchReq 但不带 state，避免把 0 当成“关闭”写下去。
	if fake.switchReq.GetMid() != 777 || fake.switchReq.GetFrom() != 1 || fake.switchReq.GetState() != 0 {
		t.Fatalf("UpSwitch 入参 = %+v", fake.switchReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.Data.State != 1 {
		t.Fatalf("resp = %+v", resp)
	}

	setResp, err := NewSetUpSwitchLogic(context.Background(), &svc.ServiceContext{Creator: fake}).
		SetUpSwitch(&types.ParamSetUpSwitch{Mid: 777, From: 1, State: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.setSwitchReq.GetMid() != 777 || fake.setSwitchReq.GetFrom() != 1 ||
		fake.setSwitchReq.GetState() != 0 {
		t.Fatalf("SetUpSwitch 入参未透传: %+v", fake.setSwitchReq)
	}
	if setResp.Code != 0 || setResp.Message != "ok" || setResp.TTL != 0 ||
		setResp.Data != (types.EmptyData{}) {
		t.Fatalf("EmptyResponse 信封 = %+v", setResp)
	}
	if fake.calls != 2 || fake.setSwitchCall != 1 {
		t.Fatalf("calls = %d set = %d, want 2/1", fake.calls, fake.setSwitchCall)
	}
}

func TestCreatorLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func() error
	}{
		{"upSpecial", func() error {
			_, err := NewUpSpecialLogic(context.Background(), empty).UpSpecial(&types.ParamUpMid{Mid: 777})
			return err
		}},
		{"upsSpecial", func() error {
			_, err := NewUpsSpecialLogic(context.Background(), empty).
				UpsSpecial(&types.ParamUpMids{Mids: []int64{777}})
			return err
		}},
		{"upAttr", func() error {
			_, err := NewUpAttrLogic(context.Background(), empty).UpAttr(&types.ParamUpAttr{Mid: 777})
			return err
		}},
		{"getUpSwitch", func() error {
			_, err := NewGetUpSwitchLogic(context.Background(), empty).
				GetUpSwitch(&types.ParamUpSwitch{Mid: 777, From: 1})
			return err
		}},
		{"setUpSwitch", func() error {
			_, err := NewSetUpSwitchLogic(context.Background(), empty).
				SetUpSwitch(&types.ParamSetUpSwitch{Mid: 777, From: 1, State: 1})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err == nil {
				t.Fatalf("%s：未配置 CreatorRPC 时必须报错，不能返回免鉴权空成功", c.name)
			}
		})
	}
}

func TestCreatorLogicsPropagateDownstreamError(t *testing.T) {
	sentinel := errors.New("creator: mids count exceeds 100")
	cases := []struct {
		name string
		call func(creatorrpc.CreatorClient) error
	}{
		{"upSpecial", func(c creatorrpc.CreatorClient) error {
			_, err := NewUpSpecialLogic(context.Background(), &svc.ServiceContext{Creator: c}).
				UpSpecial(&types.ParamUpMid{Mid: 777})
			return err
		}},
		{"upsSpecial", func(c creatorrpc.CreatorClient) error {
			_, err := NewUpsSpecialLogic(context.Background(), &svc.ServiceContext{Creator: c}).
				UpsSpecial(&types.ParamUpMids{Mids: []int64{777}})
			return err
		}},
		{"upAttr", func(c creatorrpc.CreatorClient) error {
			_, err := NewUpAttrLogic(context.Background(), &svc.ServiceContext{Creator: c}).
				UpAttr(&types.ParamUpAttr{Mid: 777, From: 1})
			return err
		}},
		{"getUpSwitch", func(c creatorrpc.CreatorClient) error {
			_, err := NewGetUpSwitchLogic(context.Background(), &svc.ServiceContext{Creator: c}).
				GetUpSwitch(&types.ParamUpSwitch{Mid: 777, From: 1})
			return err
		}},
		{"setUpSwitch", func(c creatorrpc.CreatorClient) error {
			_, err := NewSetUpSwitchLogic(context.Background(), &svc.ServiceContext{Creator: c}).
				SetUpSwitch(&types.ParamSetUpSwitch{Mid: 777, From: 1, State: 1})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeCreatorClient{err: sentinel}
			if err := c.call(fake); !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want %v（下游错误原样上抛给 httpresponse）", err, sentinel)
			}
		})
	}
}
