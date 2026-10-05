package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 private-message 的口径：请求→RPC 入参映射、RPC DTO→响应投影、
// 错误传播与「未配置客户端」的显式失败。门禁、幂等、密文与审核结论属服务职责，
// 这里不测也不在网关重复实现（AGENTS.md §5/§6）。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakePrivateMessageClient struct {
	privatemessagerpc.PrivateMessageClient

	err   error
	calls int

	conversationsReq *privatemessagerpc.ListConversationsReq
	conversationsRpl *privatemessagerpc.ListConversationsReply
	getOrCreateReq   *privatemessagerpc.GetOrCreateConversationReq
	getOrCreateRpl   *privatemessagerpc.GetOrCreateConversationReply
	listMessagesReq  *privatemessagerpc.ListMessagesReq
	listMessagesRpl  *privatemessagerpc.ListMessagesReply
	sendReq          *privatemessagerpc.SendMessageReq
	sendRpl          *privatemessagerpc.SendMessageReply
	withdrawReq      *privatemessagerpc.WithdrawMessageReq
	withdrawRpl      *privatemessagerpc.WithdrawMessageReply
	hideReq          *privatemessagerpc.HideConversationReq
	markReadReq      *privatemessagerpc.MarkReadReq
	markReadRpl      *privatemessagerpc.MarkReadReply
	unreadReq        *privatemessagerpc.GetUnreadSummaryReq
	unreadRpl        *privatemessagerpc.GetUnreadSummaryReply
	reportReq        *privatemessagerpc.ReportMessageReq
	reportRpl        *privatemessagerpc.ReportMessageReply
	getSettingReq    *privatemessagerpc.GetUserSettingReq
	setting          *privatemessagerpc.UserSettingInfo
	updateSettingReq *privatemessagerpc.UpdateUserSettingReq
}

func (f *fakePrivateMessageClient) record() {
	f.calls++
}

func (f *fakePrivateMessageClient) ListConversations(_ context.Context, in *privatemessagerpc.ListConversationsReq,
	_ ...grpc.CallOption) (*privatemessagerpc.ListConversationsReply, error) {
	f.record()
	f.conversationsReq = in
	return f.conversationsRpl, f.err
}

func (f *fakePrivateMessageClient) GetOrCreateConversation(_ context.Context, in *privatemessagerpc.GetOrCreateConversationReq,
	_ ...grpc.CallOption) (*privatemessagerpc.GetOrCreateConversationReply, error) {
	f.record()
	f.getOrCreateReq = in
	return f.getOrCreateRpl, f.err
}

func (f *fakePrivateMessageClient) ListMessages(_ context.Context, in *privatemessagerpc.ListMessagesReq,
	_ ...grpc.CallOption) (*privatemessagerpc.ListMessagesReply, error) {
	f.record()
	f.listMessagesReq = in
	return f.listMessagesRpl, f.err
}

func (f *fakePrivateMessageClient) SendMessage(_ context.Context, in *privatemessagerpc.SendMessageReq,
	_ ...grpc.CallOption) (*privatemessagerpc.SendMessageReply, error) {
	f.record()
	f.sendReq = in
	return f.sendRpl, f.err
}

func (f *fakePrivateMessageClient) WithdrawMessage(_ context.Context, in *privatemessagerpc.WithdrawMessageReq,
	_ ...grpc.CallOption) (*privatemessagerpc.WithdrawMessageReply, error) {
	f.record()
	f.withdrawReq = in
	return f.withdrawRpl, f.err
}

func (f *fakePrivateMessageClient) HideConversation(_ context.Context, in *privatemessagerpc.HideConversationReq,
	_ ...grpc.CallOption) (*privatemessagerpc.EmptyReply, error) {
	f.record()
	f.hideReq = in
	return &privatemessagerpc.EmptyReply{}, f.err
}

func (f *fakePrivateMessageClient) MarkRead(_ context.Context, in *privatemessagerpc.MarkReadReq,
	_ ...grpc.CallOption) (*privatemessagerpc.MarkReadReply, error) {
	f.record()
	f.markReadReq = in
	return f.markReadRpl, f.err
}

func (f *fakePrivateMessageClient) GetUnreadSummary(_ context.Context, in *privatemessagerpc.GetUnreadSummaryReq,
	_ ...grpc.CallOption) (*privatemessagerpc.GetUnreadSummaryReply, error) {
	f.record()
	f.unreadReq = in
	return f.unreadRpl, f.err
}

func (f *fakePrivateMessageClient) ReportMessage(_ context.Context, in *privatemessagerpc.ReportMessageReq,
	_ ...grpc.CallOption) (*privatemessagerpc.ReportMessageReply, error) {
	f.record()
	f.reportReq = in
	return f.reportRpl, f.err
}

func (f *fakePrivateMessageClient) GetUserSetting(_ context.Context, in *privatemessagerpc.GetUserSettingReq,
	_ ...grpc.CallOption) (*privatemessagerpc.UserSettingInfo, error) {
	f.record()
	f.getSettingReq = in
	return f.setting, f.err
}

func (f *fakePrivateMessageClient) UpdateUserSetting(_ context.Context, in *privatemessagerpc.UpdateUserSettingReq,
	_ ...grpc.CallOption) (*privatemessagerpc.UserSettingInfo, error) {
	f.record()
	f.updateSettingReq = in
	return f.setting, f.err
}

func TestPmListConversationsMapsCursorAndProjectsPreview(t *testing.T) {
	fake := &fakePrivateMessageClient{conversationsRpl: &privatemessagerpc.ListConversationsReply{
		List: []*privatemessagerpc.ConversationInfo{
			{
				ConversationId: 9001, PeerMid: 222, State: 2, LastMsgId: 555, LastSeq: 77,
				LastMsgType: 1, LastPreview: "[已撤回]", LastMsgTime: 1700, ReadSeq: 70,
				UnreadCount: 7, Hidden: false, Ctime: 1600,
			},
			nil, // 服务端不会给 nil 元素，但投影必须兜底而不是 panic
		},
		NextCursor:  "1700-9001",
		HasMore:     true,
		UnreadTotal: 12,
	}}
	resp, err := NewListPmConversationsLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		ListPmConversations(&types.ParamPmConversations{
			Mid: 111, Cursor: "prev", Ps: 30, OnlyUnread: true, IncludeHidden: true, TraceId: "tr-1",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.conversationsReq
	if in.GetMid() != 111 || in.GetCursor() != "prev" || in.GetPs() != 30 ||
		!in.GetOnlyUnread() || !in.GetIncludeHidden() || in.GetTraceId() != "tr-1" {
		t.Fatalf("ListConversations 入参未透传: %+v", in)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data.NextCursor != "1700-9001" || !resp.Data.HasMore || resp.Data.UnreadTotal != 12 {
		t.Fatalf("分页字段投影不完整: %+v", resp.Data)
	}
	if len(resp.Data.List) != 2 {
		t.Fatalf("list 长度 = %d, want 2", len(resp.Data.List))
	}
	first := resp.Data.List[0]
	if first.ConversationId != 9001 || first.PeerMid != 222 || first.State != 2 || first.LastSeq != 77 ||
		first.LastPreview != "[已撤回]" || first.ReadSeq != 70 || first.UnreadCount != 7 || first.Ctime != 1600 {
		t.Fatalf("会话投影不完整: %+v", first)
	}
	if first.Hidden {
		t.Fatal("hidden 必须原样投影服务侧结果")
	}
	// 越界索引与 nil 元素都由 getter 兜底成零值，端上按 conversation_id=0 自行忽略。
	if resp.Data.List[1].ConversationId != 0 {
		t.Fatalf("nil 元素应投影为零值: %+v", resp.Data.List[1])
	}
}

func TestPmListConversationsEmptyListIsNotNilSlice(t *testing.T) {
	fake := &fakePrivateMessageClient{conversationsRpl: &privatemessagerpc.ListConversationsReply{}}
	resp, err := NewListPmConversationsLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		ListPmConversations(&types.ParamPmConversations{Mid: 111})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.List == nil {
		t.Fatal("空页必须投影成 []，不能让端上把 null 当成加载失败")
	}
	// ps 省略时保持 0：页大小由服务配置 PrivateMessage.PageSize 决定，网关不写死。
	if fake.conversationsReq.GetPs() != 0 {
		t.Fatalf("ps = %d, want 0（交给服务默认）", fake.conversationsReq.GetPs())
	}
}

func TestPmListMessagesProjectsSeqCursorAndPlainText(t *testing.T) {
	fake := &fakePrivateMessageClient{listMessagesRpl: &privatemessagerpc.ListMessagesReply{
		List: []*privatemessagerpc.MessageInfo{{
			MsgId: 555, ConversationId: 9001, Seq: 77, SenderMid: 222, MsgType: 1,
			Content: "明文正文", MediaRef: "asset:12", State: 3, AuditTaskId: 8,
			ClientMsgId: "c-1", Ctime: 1700, WithdrawTime: 1750,
		}},
		NextCursorSeq: 76,
		HasMore:       true,
		ReadSeq:       70,
	}}
	resp, err := NewListPmMessagesLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		ListPmMessages(&types.ParamPmMessages{ConversationId: 9001, Mid: 111, CursorSeq: 77, Ps: 20, TraceId: "tr-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.listMessagesReq
	if in.GetConversationId() != 9001 || in.GetMid() != 111 || in.GetCursorSeq() != 77 ||
		in.GetPs() != 20 || in.GetTraceId() != "tr-2" {
		t.Fatalf("ListMessages 入参未透传: %+v", in)
	}
	m := resp.Data.List[0]
	if m.Seq != 77 || m.MsgType != 1 || m.State != 3 || m.Content != "明文正文" ||
		m.MediaRef != "asset:12" || m.ClientMsgId != "c-1" || m.WithdrawTime != 1750 || m.AuditTaskId != 8 {
		t.Fatalf("消息投影不完整: %+v", m)
	}
	if resp.Data.NextCursorSeq != 76 || !resp.Data.HasMore || resp.Data.ReadSeq != 70 {
		t.Fatalf("游标投影不完整: %+v", resp.Data)
	}
}

func TestPmSendMessagePassesIdempotencyKey(t *testing.T) {
	fake := &fakePrivateMessageClient{sendRpl: &privatemessagerpc.SendMessageReply{
		MsgId: 555, ConversationId: 9001, Seq: 77, State: 2, Ctime: 1700,
		Replayed: true, AuditTaskId: 8, Preview: "摘要",
	}}
	resp, err := NewSendPmMessageLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		SendPmMessage(&types.ParamPmSend{
			Mid: 111, PeerMid: 222, MsgType: 1, Content: "正文", ClientMsgId: "c-1", TraceId: "tr-3",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.sendReq
	if in.GetMid() != 111 || in.GetPeerMid() != 222 || in.GetConversationId() != 0 {
		t.Fatalf("SendMessage 主键透传错误: %+v", in)
	}
	if in.GetMsgType() != privatemessagerpc.MsgType_MSG_TYPE_TEXT {
		t.Fatalf("msg_type 必须转成 MsgType 枚举: %v", in.GetMsgType())
	}
	if in.GetClientMsgId() != "c-1" || in.GetTraceId() != "tr-3" {
		t.Fatalf("幂等键/trace 未透传: %+v", in)
	}
	// replayed 与 audit_task_id 是服务侧幂等与送审结论，网关原样回显。
	if resp.Data.MsgId != 555 || !resp.Data.Replayed || resp.Data.AuditTaskId != 8 ||
		resp.Data.Preview != "摘要" || resp.Data.State != 2 {
		t.Fatalf("发送结果投影不完整: %+v", resp.Data)
	}
}

func TestPmSendRejectsMissingIdempotencyKeyWithoutCallingService(t *testing.T) {
	fake := &fakePrivateMessageClient{}
	if _, err := NewSendPmMessageLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		SendPmMessage(&types.ParamPmSend{Mid: 111, PeerMid: 222, MsgType: 1, Content: "x", ClientMsgId: "   "}); err == nil {
		t.Fatal("缺幂等键必须拒绝，否则重试会落出重复消息")
	}
	if fake.calls != 0 {
		t.Fatalf("参数校验失败时不得调用下游，calls=%d", fake.calls)
	}
}

func TestPmWithdrawLimitsSourceToSelfService(t *testing.T) {
	fake := &fakePrivateMessageClient{withdrawRpl: &privatemessagerpc.WithdrawMessageReply{
		MsgId: 555, State: 3, Withdrawn: true, WithdrawTime: 1750,
	}}
	resp, err := NewWithdrawPmMessageLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		WithdrawPmMessage(&types.ParamPmWithdraw{
			MsgId: 555, OperatorMid: 111, Source: int32(privatemessagerpc.WithdrawSource_WITHDRAW_SOURCE_SENDER),
			Reason: "发错了", TraceId: "tr-4",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.withdrawReq.GetSource() != privatemessagerpc.WithdrawSource_WITHDRAW_SOURCE_SENDER ||
		fake.withdrawReq.GetOperatorMid() != 111 || fake.withdrawReq.GetReason() != "发错了" {
		t.Fatalf("WithdrawMessage 入参未透传: %+v", fake.withdrawReq)
	}
	if fake.withdrawReq.GetAuditTaskId() != 0 {
		t.Fatal("终端入口不得下发 audit_task_id，那是审核撤回的关联字段")
	}
	if resp.Data.State != 3 || !resp.Data.Withdrawn || resp.Data.WithdrawTime != 1750 {
		t.Fatalf("撤回结果投影不完整: %+v", resp.Data)
	}
	// 系统/运营撤回来源不能从终端入口伪造。
	for _, src := range []int32{0, 3, 4, 99} {
		blocked := &fakePrivateMessageClient{}
		if _, err := NewWithdrawPmMessageLogic(context.Background(), &svc.ServiceContext{PrivateMessage: blocked}).
			WithdrawPmMessage(&types.ParamPmWithdraw{MsgId: 555, OperatorMid: 111, Source: src}); err == nil {
			t.Fatalf("source=%d 必须被拒绝", src)
		}
		if blocked.calls != 0 {
			t.Fatalf("source=%d 被拒时不得调用下游", src)
		}
	}
}

func TestPmHideConversationReturnsEmptyEnvelope(t *testing.T) {
	fake := &fakePrivateMessageClient{}
	resp, err := NewHidePmConversationLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		HidePmConversation(&types.ParamPmHideConversation{Mid: 111, ConversationId: 9001, Hide: true, TraceId: "tr-5"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.hideReq.GetMid() != 111 || fake.hideReq.GetConversationId() != 9001 || !fake.hideReq.GetHide() ||
		fake.hideReq.GetTraceId() != "tr-5" {
		t.Fatalf("HideConversation 入参未透传: %+v", fake.hideReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.Data != (types.EmptyData{}) || resp.TTL != 0 {
		t.Fatalf("空响应信封不符: %+v", resp)
	}
}

func TestPmMarkReadKeepsChangedFalseAsSuccess(t *testing.T) {
	fake := &fakePrivateMessageClient{markReadRpl: &privatemessagerpc.MarkReadReply{
		ReadSeq: 70, Changed: false, UnreadCount: 7,
	}}
	resp, err := NewMarkPmReadLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		MarkPmRead(&types.ParamPmMarkRead{ConversationId: 9001, Mid: 111, ReadSeq: 60})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 游标回退是幂等成功（服务回当前值 + changed=false），网关不得改写成语义不明的失败。
	if resp.Data.ReadSeq != 70 || resp.Data.Changed || resp.Data.UnreadCount != 7 {
		t.Fatalf("MarkRead 投影不完整: %+v", resp.Data)
	}
	if fake.markReadReq.GetReadSeq() != 60 {
		t.Fatalf("read_seq 必须原样下发由服务判断是否前进: %d", fake.markReadReq.GetReadSeq())
	}
}

func TestPmUnreadSummaryProjectsAllCounters(t *testing.T) {
	fake := &fakePrivateMessageClient{unreadRpl: &privatemessagerpc.GetUnreadSummaryReply{
		UnreadTotal: 42, UnreadConversations: 5, ComputedAt: 1700,
	}}
	resp, err := NewPmUnreadSummaryLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		PmUnreadSummary(&types.ParamPmUnread{Mid: 111, Force: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fake.unreadReq.GetForce() || fake.unreadReq.GetMid() != 111 {
		t.Fatalf("GetUnreadSummary 入参未透传: %+v", fake.unreadReq)
	}
	if resp.Data.UnreadTotal != 42 || resp.Data.UnreadConversations != 5 || resp.Data.ComputedAt != 1700 {
		t.Fatalf("未读汇总投影不完整: %+v", resp.Data)
	}
}

func TestPmReportPassesReasonCodeWithoutJudgement(t *testing.T) {
	fake := &fakePrivateMessageClient{reportRpl: &privatemessagerpc.ReportMessageReply{
		ReportId: 7001, Duplicated: true, AuditTaskId: 88,
	}}
	resp, err := NewReportPmMessageLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		ReportPmMessage(&types.ParamPmReport{
			MsgId: 555, ReporterMid: 111, Reason: 3, Description: "引流", TraceId: "tr-6",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.reportReq
	if in.GetMsgId() != 555 || in.GetReporterMid() != 111 || in.GetReason() != 3 ||
		in.GetDescription() != "引流" || in.GetTraceId() != "tr-6" {
		t.Fatalf("ReportMessage 入参未透传: %+v", in)
	}
	if !resp.Data.Duplicated || resp.Data.ReportId != 7001 || resp.Data.AuditTaskId != 88 {
		t.Fatalf("举报结果投影不完整: %+v", resp.Data)
	}
}

func TestPmGetSettingKeepsUnspecifiedAllowFrom(t *testing.T) {
	fake := &fakePrivateMessageClient{setting: &privatemessagerpc.UserSettingInfo{
		Mid: 111, AllowFrom: privatemessagerpc.AllowFrom_ALLOW_FROM_MUTUAL,
		RejectStranger: true, KeywordFilter: false, MuteConversation: true, Mtime: 1700,
	}}
	resp, err := NewGetPmSettingLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		GetPmSetting(&types.ParamPmSetting{Mid: 111, TraceId: "tr-7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.getSettingReq.GetMid() != 111 || fake.getSettingReq.GetTraceId() != "tr-7" {
		t.Fatalf("GetUserSetting 入参未透传: %+v", fake.getSettingReq)
	}
	s := resp.Data.Setting
	if s.AllowFrom != int32(privatemessagerpc.AllowFrom_ALLOW_FROM_MUTUAL) || !s.RejectStranger ||
		s.KeywordFilter || !s.MuteConversation || s.Mtime != 1700 {
		t.Fatalf("偏好投影不完整: %+v", s)
	}
	// 服务回 UNSPECIFIED 时不得被网关美化成 ANYONE。
	zero := &fakePrivateMessageClient{setting: &privatemessagerpc.UserSettingInfo{Mid: 111}}
	zeroResp, err := NewGetPmSettingLogic(context.Background(), &svc.ServiceContext{PrivateMessage: zero}).
		GetPmSetting(&types.ParamPmSetting{Mid: 111})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zeroResp.Data.Setting.AllowFrom != 0 {
		t.Fatalf("allow_from = %d, want 0（未设置由服务解释）", zeroResp.Data.Setting.AllowFrom)
	}
}

func TestPmUpdateSettingUsesTriState(t *testing.T) {
	fake := &fakePrivateMessageClient{setting: &privatemessagerpc.UserSettingInfo{Mid: 111, Mtime: 1800}}
	if _, err := NewUpdatePmSettingLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		UpdatePmSetting(&types.ParamPmSettingUpdate{
			Mid: 111, AllowFrom: int32(privatemessagerpc.AllowFrom_ALLOW_FROM_NONE),
			RejectStranger: 2, KeywordFilter: 1, TraceId: "tr-8",
		}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.updateSettingReq
	if in.GetAllowFrom() != privatemessagerpc.AllowFrom_ALLOW_FROM_NONE {
		t.Fatalf("allow_from 未转枚举: %v", in.GetAllowFrom())
	}
	if in.RejectStranger == nil || *in.RejectStranger {
		t.Fatalf("reject_stranger=2 应显式写 false, got %v", in.RejectStranger)
	}
	if in.KeywordFilter == nil || !*in.KeywordFilter {
		t.Fatalf("keyword_filter=1 应显式写 true, got %v", in.KeywordFilter)
	}
	// 未填（0）必须是 nil，表示「不修改」而不是「关掉」。
	if in.MuteConversation != nil {
		t.Fatalf("mute_conversation=0 必须保持未设置, got %v", *in.MuteConversation)
	}
	if in.GetTraceId() != "tr-8" {
		t.Fatalf("trace_id 未透传: %+v", in)
	}
}

func TestPmUpdateSettingRejectsInvalidTriState(t *testing.T) {
	cases := []struct {
		name string
		req  *types.ParamPmSettingUpdate
	}{
		{"reject_stranger", &types.ParamPmSettingUpdate{Mid: 111, RejectStranger: 7}},
		{"keyword_filter", &types.ParamPmSettingUpdate{Mid: 111, KeywordFilter: -1}},
		{"mute_conversation", &types.ParamPmSettingUpdate{Mid: 111, MuteConversation: 3}},
	}
	for _, tc := range cases {
		fake := &fakePrivateMessageClient{}
		if _, err := NewUpdatePmSettingLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
			UpdatePmSetting(tc.req); err == nil {
			t.Fatalf("%s 非法三态必须拒绝", tc.name)
		}
		if fake.calls != 0 {
			t.Fatalf("%s 被拒时不得调用下游，calls=%d", tc.name, fake.calls)
		}
	}
}

func TestPmGetOrCreateConversationProjectsCreatedFlag(t *testing.T) {
	fake := &fakePrivateMessageClient{getOrCreateRpl: &privatemessagerpc.GetOrCreateConversationReply{
		ConversationId: 9001, Created: true, State: 1, Ctime: 1600,
	}}
	resp, err := NewGetOrCreatePmConversationLogic(context.Background(), &svc.ServiceContext{PrivateMessage: fake}).
		GetOrCreatePmConversation(&types.ParamPmConversationGet{Mid: 111, PeerMid: 222, TraceId: "tr-9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.getOrCreateReq.GetMid() != 111 || fake.getOrCreateReq.GetPeerMid() != 222 ||
		fake.getOrCreateReq.GetTraceId() != "tr-9" {
		t.Fatalf("GetOrCreateConversation 入参未透传: %+v", fake.getOrCreateReq)
	}
	if resp.Data.ConversationId != 9001 || !resp.Data.Created || resp.Data.State != 1 || resp.Data.Ctime != 1600 {
		t.Fatalf("会话创建结果投影不完整: %+v", resp.Data)
	}
}

func TestPmLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	calls := []struct {
		name string
		run  func() error
	}{
		{"listPmConversations", func() error {
			_, err := NewListPmConversationsLogic(context.Background(), empty).
				ListPmConversations(&types.ParamPmConversations{Mid: 1})
			return err
		}},
		{"getOrCreatePmConversation", func() error {
			_, err := NewGetOrCreatePmConversationLogic(context.Background(), empty).
				GetOrCreatePmConversation(&types.ParamPmConversationGet{Mid: 1, PeerMid: 2})
			return err
		}},
		{"listPmMessages", func() error {
			_, err := NewListPmMessagesLogic(context.Background(), empty).
				ListPmMessages(&types.ParamPmMessages{ConversationId: 1, Mid: 1})
			return err
		}},
		{"sendPmMessage", func() error {
			_, err := NewSendPmMessageLogic(context.Background(), empty).
				SendPmMessage(&types.ParamPmSend{Mid: 1, PeerMid: 2, MsgType: 1, ClientMsgId: "c"})
			return err
		}},
		{"withdrawPmMessage", func() error {
			_, err := NewWithdrawPmMessageLogic(context.Background(), empty).
				WithdrawPmMessage(&types.ParamPmWithdraw{MsgId: 1, OperatorMid: 1, Source: 1})
			return err
		}},
		{"hidePmConversation", func() error {
			_, err := NewHidePmConversationLogic(context.Background(), empty).
				HidePmConversation(&types.ParamPmHideConversation{Mid: 1, ConversationId: 1})
			return err
		}},
		{"markPmRead", func() error {
			_, err := NewMarkPmReadLogic(context.Background(), empty).
				MarkPmRead(&types.ParamPmMarkRead{ConversationId: 1, Mid: 1, ReadSeq: 1})
			return err
		}},
		{"pmUnreadSummary", func() error {
			_, err := NewPmUnreadSummaryLogic(context.Background(), empty).
				PmUnreadSummary(&types.ParamPmUnread{Mid: 1})
			return err
		}},
		{"reportPmMessage", func() error {
			_, err := NewReportPmMessageLogic(context.Background(), empty).
				ReportPmMessage(&types.ParamPmReport{MsgId: 1, ReporterMid: 1, Reason: 1})
			return err
		}},
		{"getPmSetting", func() error {
			_, err := NewGetPmSettingLogic(context.Background(), empty).
				GetPmSetting(&types.ParamPmSetting{Mid: 1})
			return err
		}},
		{"updatePmSetting", func() error {
			_, err := NewUpdatePmSettingLogic(context.Background(), empty).
				UpdatePmSetting(&types.ParamPmSettingUpdate{Mid: 1})
			return err
		}},
	}
	for _, tc := range calls {
		if err := tc.run(); err == nil {
			t.Fatalf("%s：未配置 PrivateMessageRPC 时必须报错", tc.name)
		}
	}
}

func TestPmLogicsPropagateDownstreamError(t *testing.T) {
	// 契约轮阶段 private-message 全部方法返回 ErrNotImplemented：
	// 网关必须把它原样上抛，绝不返回空列表/零值成功（否则端上误显示「没有消息」）。
	sentinel := errors.New("privatemessage: not implemented")
	empty := &svc.ServiceContext{PrivateMessage: &fakePrivateMessageClient{err: sentinel}}
	calls := []struct {
		name string
		run  func() error
	}{
		{"listPmConversations", func() error {
			_, err := NewListPmConversationsLogic(context.Background(), empty).
				ListPmConversations(&types.ParamPmConversations{Mid: 1})
			return err
		}},
		{"getOrCreatePmConversation", func() error {
			_, err := NewGetOrCreatePmConversationLogic(context.Background(), empty).
				GetOrCreatePmConversation(&types.ParamPmConversationGet{Mid: 1, PeerMid: 2})
			return err
		}},
		{"listPmMessages", func() error {
			_, err := NewListPmMessagesLogic(context.Background(), empty).
				ListPmMessages(&types.ParamPmMessages{ConversationId: 1, Mid: 1})
			return err
		}},
		{"sendPmMessage", func() error {
			_, err := NewSendPmMessageLogic(context.Background(), empty).
				SendPmMessage(&types.ParamPmSend{Mid: 1, PeerMid: 2, MsgType: 1, ClientMsgId: "c"})
			return err
		}},
		{"withdrawPmMessage", func() error {
			_, err := NewWithdrawPmMessageLogic(context.Background(), empty).
				WithdrawPmMessage(&types.ParamPmWithdraw{MsgId: 1, OperatorMid: 1, Source: 2})
			return err
		}},
		{"hidePmConversation", func() error {
			_, err := NewHidePmConversationLogic(context.Background(), empty).
				HidePmConversation(&types.ParamPmHideConversation{Mid: 1, ConversationId: 1})
			return err
		}},
		{"markPmRead", func() error {
			_, err := NewMarkPmReadLogic(context.Background(), empty).
				MarkPmRead(&types.ParamPmMarkRead{ConversationId: 1, Mid: 1, ReadSeq: 1})
			return err
		}},
		{"pmUnreadSummary", func() error {
			_, err := NewPmUnreadSummaryLogic(context.Background(), empty).
				PmUnreadSummary(&types.ParamPmUnread{Mid: 1})
			return err
		}},
		{"reportPmMessage", func() error {
			_, err := NewReportPmMessageLogic(context.Background(), empty).
				ReportPmMessage(&types.ParamPmReport{MsgId: 1, ReporterMid: 1, Reason: 1})
			return err
		}},
		{"getPmSetting", func() error {
			_, err := NewGetPmSettingLogic(context.Background(), empty).
				GetPmSetting(&types.ParamPmSetting{Mid: 1})
			return err
		}},
		{"updatePmSetting", func() error {
			_, err := NewUpdatePmSettingLogic(context.Background(), empty).
				UpdatePmSetting(&types.ParamPmSettingUpdate{Mid: 1})
			return err
		}},
	}
	for _, tc := range calls {
		if err := tc.run(); !errors.Is(err, sentinel) {
			t.Fatalf("%s err = %v, want %v", tc.name, err, sentinel)
		}
	}
}
