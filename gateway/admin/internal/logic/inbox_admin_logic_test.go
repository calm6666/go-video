package logic

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	inboxrpc "go-video/services/inbox/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 inbox 的口径：存在性校验、审计归属（operator）、proto 枚举互转、
// 未读 map 的投影顺序，以及错误是否原样上抛。
// 接收人去重/人数上限/幂等命中/未读重算口径全部由 inbox 服务判定，此处不复算（AGENTS.md §5）。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

// inboxAdminFake 是 inbox RPC 的假客户端，只记录被调用的入参并返回预置响应。
type inboxAdminFake struct {
	inboxrpc.InboxClient

	err error

	sendReq    *inboxrpc.SendSystemMessageReq
	sendReply  *inboxrpc.SendSystemMessageReply
	recompute  *inboxrpc.RecomputeUnreadReq
	unreadResp *inboxrpc.RecomputeUnreadReply
	calls      int
}

func (f *inboxAdminFake) SendSystemMessage(_ context.Context, in *inboxrpc.SendSystemMessageReq,
	_ ...grpc.CallOption) (*inboxrpc.SendSystemMessageReply, error) {
	f.calls++
	f.sendReq = in
	return f.sendReply, f.err
}

func (f *inboxAdminFake) RecomputeUnread(_ context.Context, in *inboxrpc.RecomputeUnreadReq,
	_ ...grpc.CallOption) (*inboxrpc.RecomputeUnreadReply, error) {
	f.calls++
	f.recompute = in
	return f.unreadResp, f.err
}

func inboxAdminSendReq() *types.ParamAdminSendInboxMessage {
	return &types.ParamAdminSendInboxMessage{
		OperatorMid:    9527,
		Mids:           []int64{10001, 10002},
		Title:          "系统维护通知",
		Content:        "今晚 02:00 服务升级",
		IdempotencyKey: "op-2026-09-20-001",
	}
}

func TestAdminSendInboxMessageRequiresConfiguredService(t *testing.T) {
	l := NewAdminSendInboxMessageLogic(withAdminSession(gateAdminID), &svc.ServiceContext{})
	_, err := l.AdminSendInboxMessage(inboxAdminSendReq())
	if err == nil || err.Error() != "inbox service not configured" {
		t.Fatalf("未配置 inbox 时应返回明确错误，实际: %v", err)
	}
}

func TestAdminSendInboxMessageValidatesPresenceOnly(t *testing.T) {
	cases := []struct {
		name  string
		patch func(*types.ParamAdminSendInboxMessage)
		want  string
	}{
		{"operator_mid", func(r *types.ParamAdminSendInboxMessage) { r.OperatorMid = 0 }, "operator_mid required"},
		{"mids", func(r *types.ParamAdminSendInboxMessage) { r.Mids = nil }, "mids required"},
		{"title", func(r *types.ParamAdminSendInboxMessage) { r.Title = "   " }, "title required"},
		{"content", func(r *types.ParamAdminSendInboxMessage) { r.Content = "" }, "content required"},
		{"idempotency_key", func(r *types.ParamAdminSendInboxMessage) { r.IdempotencyKey = "" }, "idempotency_key required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &inboxAdminFake{}
			l := NewAdminSendInboxMessageLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})
			req := inboxAdminSendReq()
			tc.patch(req)
			if _, err := l.AdminSendInboxMessage(req); err == nil {
				t.Fatalf("%s 为空时应拒绝", tc.name)
			}
			// 校验必须在调用下游之前完成，否则会留下未授权的投递记录。
			if fake.calls != 0 {
				t.Fatalf("入参非法时不应调用 inbox，实际调用 %d 次", fake.calls)
			}
		})
	}
}

func TestAdminSendInboxMessageMapsRequest(t *testing.T) {
	fake := &inboxAdminFake{sendReply: &inboxrpc.SendSystemMessageReply{
		MsgId: 777, Delivered: 2, Deduplicated: false, Ctime: 1700000000,
	}}
	l := NewAdminSendInboxMessageLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})

	resp, err := l.AdminSendInboxMessage(&types.ParamAdminSendInboxMessage{
		OperatorMid:    9527,
		Mids:           []int64{10001, 10002},
		Title:          "标题",
		Content:        "正文",
		Category:       int32(inboxrpc.Category_CATEGORY_ENGAGEMENT),
		MsgType:        int32(inboxrpc.MsgType_MSG_TYPE_RICH),
		SenderMid:      8888,
		BizType:        "op_notice",
		BizId:          "biz-1",
		Extra:          `{"aid":1}`,
		IdempotencyKey: "key-1",
		TraceId:        "trace-1",
	})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	in := fake.sendReq
	if in.Category != inboxrpc.Category_CATEGORY_ENGAGEMENT || in.MsgType != inboxrpc.MsgType_MSG_TYPE_RICH {
		t.Fatalf("枚举未按 proto 类型转换: category=%v msg_type=%v", in.Category, in.MsgType)
	}
	// operator 由后台审计主体 operator_mid 派生，客户端不能冒充他人。
	if in.Operator != 9527 {
		t.Fatalf("operator 应取 operator_mid，实际 %d", in.Operator)
	}
	if !reflect.DeepEqual(in.Mids, []int64{10001, 10002}) {
		t.Fatalf("mids 透传错误: %v", in.Mids)
	}
	if in.SenderMid != 8888 || in.BizType != "op_notice" || in.BizId != "biz-1" ||
		in.Extra != `{"aid":1}` || in.IdempotencyKey != "key-1" || in.TraceId != "trace-1" {
		t.Fatalf("字段未原样透传: %+v", in)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封字段错误: %+v", resp)
	}
	want := types.AdminSendInboxMessageData{MsgId: 777, Delivered: 2, Ctime: 1700000000}
	if resp.Data != want {
		t.Fatalf("投影结果错误: %+v", resp.Data)
	}
}

func TestAdminSendInboxMessageReportsDeduplicated(t *testing.T) {
	fake := &inboxAdminFake{sendReply: &inboxrpc.SendSystemMessageReply{MsgId: 777, Deduplicated: true}}
	l := NewAdminSendInboxMessageLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})

	resp, err := l.AdminSendInboxMessage(inboxAdminSendReq())
	if err != nil {
		t.Fatalf("幂等命中不是错误: %v", err)
	}
	if !resp.Data.Deduplicated {
		t.Fatal("deduplicated 应回传给调用方，否则重试无法判断是否重复投递")
	}
}

func TestAdminSendInboxMessagePropagatesDownstreamError(t *testing.T) {
	sentinel := errors.New("inbox: recipients exceed limit")
	fake := &inboxAdminFake{err: sentinel}
	l := NewAdminSendInboxMessageLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})

	_, err := l.AdminSendInboxMessage(inboxAdminSendReq())
	// 错误原样上抛，由 common/httpresponse 统一映射，网关不吞掉也不改写。
	if !errors.Is(err, sentinel) {
		t.Fatalf("下游错误应原样上抛，实际: %v", err)
	}
}

func TestAdminRecomputeInboxUnreadRequiresConfiguredService(t *testing.T) {
	l := NewAdminRecomputeInboxUnreadLogic(withAdminSession(gateAdminID), &svc.ServiceContext{})
	_, err := l.AdminRecomputeInboxUnread(&types.ParamAdminRecomputeInboxUnread{Mid: 1, OperatorMid: 2})
	if err == nil || err.Error() != "inbox service not configured" {
		t.Fatalf("未配置 inbox 时应返回明确错误，实际: %v", err)
	}
}

func TestAdminRecomputeInboxUnreadRequiresOperator(t *testing.T) {
	fake := &inboxAdminFake{}
	l := NewAdminRecomputeInboxUnreadLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})
	// mid 由下游校验（ErrInvalidMid），网关只补审计主体这一条。
	if _, err := l.AdminRecomputeInboxUnread(&types.ParamAdminRecomputeInboxUnread{Mid: 10001}); err == nil {
		t.Fatal("缺少 operator_mid 时应拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("入参非法时不应调用 inbox，实际调用 %d 次", fake.calls)
	}
}

func TestAdminRecomputeInboxUnreadFlattensCategoriesAscending(t *testing.T) {
	fake := &inboxAdminFake{unreadResp: &inboxrpc.RecomputeUnreadReply{
		Total:      6,
		ByCategory: map[int32]int64{3: 1, 1: 4, 2: 1},
	}}
	l := NewAdminRecomputeInboxUnreadLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})

	resp, err := l.AdminRecomputeInboxUnread(&types.ParamAdminRecomputeInboxUnread{Mid: 10001, OperatorMid: 9527})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if fake.recompute.Mid != 10001 {
		t.Fatalf("mid 透传错误: %d", fake.recompute.Mid)
	}
	want := []types.AdminInboxUnreadItem{{Category: 1, Count: 4}, {Category: 2, Count: 1}, {Category: 3, Count: 1}}
	if !reflect.DeepEqual(resp.Data.ByCategory, want) {
		t.Fatalf("未读投影应按分类升序: %+v", resp.Data.ByCategory)
	}
	if resp.Data.Mid != 10001 || resp.Data.Total != 6 {
		t.Fatalf("mid/total 投影错误: %+v", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封字段错误: %+v", resp)
	}
}

func TestAdminRecomputeInboxUnreadReturnsNonNilSlice(t *testing.T) {
	fake := &inboxAdminFake{unreadResp: &inboxrpc.RecomputeUnreadReply{Total: 0}}
	l := NewAdminRecomputeInboxUnreadLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})

	resp, err := l.AdminRecomputeInboxUnread(&types.ParamAdminRecomputeInboxUnread{Mid: 10001, OperatorMid: 9527})
	if err != nil {
		t.Fatalf("合法请求不应失败: %v", err)
	}
	if resp.Data.ByCategory == nil {
		t.Fatal("by_category 必须是非 nil 数组，客户端要能直接当列表渲染")
	}
	if len(resp.Data.ByCategory) != 0 {
		t.Fatalf("无未读时应为空数组: %+v", resp.Data.ByCategory)
	}
}

func TestAdminRecomputeInboxUnreadPropagatesDownstreamError(t *testing.T) {
	sentinel := errors.New("inbox: invalid mid")
	fake := &inboxAdminFake{err: sentinel}
	l := NewAdminRecomputeInboxUnreadLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Inbox: fake})

	if _, err := l.AdminRecomputeInboxUnread(&types.ParamAdminRecomputeInboxUnread{Mid: 0, OperatorMid: 9527}); !errors.Is(err, sentinel) {
		t.Fatalf("下游错误应原样上抛，实际: %v", err)
	}
}
