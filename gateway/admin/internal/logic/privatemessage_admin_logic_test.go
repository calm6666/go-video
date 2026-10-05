package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"google.golang.org/grpc"
)

// 本文件只锁网关侧面向 private-message 的三条口径（与 collector / live-media 测试同一套边界）：
//
//  1. rpc→types **逐字段不丢**：举报台账的 13 位（含 reason 原因码、audit_task_id、handler、
//     handle_note、ctime/mtime）与清理结果的四个计数——裁掉一位就让后台靠猜；
//  2. 入参**原样交给下游**：cursor / idempotency_key 只判空不改写，0 是合法哨兵
//     （state=0 全部、before_time=0 由服务按留存窗口推算、batch_limit=0 服务端默认值）；
//  3. 写入口门槛：会话身份缺失即 fail-closed、主体位必须 >0、report_id 与 action 的 0 必须被拒。
//
// 刻意不断言下游业务结论（AGENTS.md §5/§8）：举报状态机能否再处置、动作是否在合法集合内、
// 能否连带撤回、page_size 上限、cursor 语法、留存窗口下限与批处理上限——全部由
// private-message 判定。也不断言「处置一定成功」：replayed=true 是结论不是错误。
//
// 打桩方式与 collector 一致：内嵌生成的 client 接口 + 只覆盖本域用到的 3 个方法，
// 其余方法一旦被调用直接 panic（nil 接口提升）——这正是「ApplyModerationVerdict 与
// 用户侧收发不属 admin 面」这条边界的机器可检表达。不建 gRPC 连接、不碰数据库。

var errPMFakeDownstream = errors.New("pm downstream unavailable")

type pmAdminFake struct {
	privatemessagerpc.PrivateMessageClient

	err      error
	calls    int
	lastCall string

	listReq   *privatemessagerpc.ListReportsReq
	listReply *privatemessagerpc.ListReportsReply
	handleReq *privatemessagerpc.HandleReportReq
	handle    *privatemessagerpc.HandleReportReply
	purgeReq  *privatemessagerpc.PurgeExpiredMessagesReq
	purge     *privatemessagerpc.PurgeExpiredMessagesReply
}

func (f *pmAdminFake) ListReports(_ context.Context, in *privatemessagerpc.ListReportsReq,
	_ ...grpc.CallOption) (*privatemessagerpc.ListReportsReply, error) {
	f.calls++
	f.lastCall = "ListReports"
	f.listReq = in
	if f.err != nil {
		return nil, f.err
	}
	return f.listReply, nil
}

func (f *pmAdminFake) HandleReport(_ context.Context, in *privatemessagerpc.HandleReportReq,
	_ ...grpc.CallOption) (*privatemessagerpc.HandleReportReply, error) {
	f.calls++
	f.lastCall = "HandleReport"
	f.handleReq = in
	if f.err != nil {
		return nil, f.err
	}
	return f.handle, nil
}

func (f *pmAdminFake) PurgeExpiredMessages(_ context.Context, in *privatemessagerpc.PurgeExpiredMessagesReq,
	_ ...grpc.CallOption) (*privatemessagerpc.PurgeExpiredMessagesReply, error) {
	f.calls++
	f.lastCall = "PurgeExpiredMessages"
	f.purgeReq = in
	if f.err != nil {
		return nil, f.err
	}
	return f.purge, nil
}

func pmSvc(fake privatemessagerpc.PrivateMessageClient) *svc.ServiceContext {
	return &svc.ServiceContext{PrivateMessage: fake}
}

func pmSession() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77, Roles: []string{"content_operator"},
	})
}

func pmFullReport() *privatemessagerpc.ReportInfo {
	return &privatemessagerpc.ReportInfo{
		ReportId: 4001, ConversationId: 3001, MsgId: 2001, ReporterMid: 11, TargetMid: 22,
		Reason: 5, Description: "骚扰", State: 1, AuditTaskId: 66, Handler: 0,
		HandleNote: "", Ctime: 1700000000, Mtime: 1700000100,
	}
}

func TestPrivateMessageReportList_ProjectionKeepsEveryColumn(t *testing.T) {
	fake := &pmAdminFake{listReply: &privatemessagerpc.ListReportsReply{
		List:       []*privatemessagerpc.ReportInfo{pmFullReport()},
		NextCursor: "4000",
		HasMore:    true,
	}}
	resp, err := NewPrivateMessageReportListLogic(context.Background(), pmSvc(fake)).
		PrivateMessageReportList(&types.ParamPrivateMessageReportList{
			State: 1, TargetMid: 22, Cursor: "5000", PageSize: 20, OperatorMid: 9001, TraceId: "t-1",
		})
	if err != nil {
		t.Fatalf("正常翻页被拒: %v", err)
	}
	if resp.Code != 0 || resp.Message != "ok" {
		t.Fatalf("信封必须固定 code=0/message=ok，实际 %+v", resp)
	}
	got := resp.Data.List[0]
	want := pmFullReport()
	if got.ReportId != want.ReportId || got.ConversationId != want.ConversationId || got.MsgId != want.MsgId ||
		got.ReporterMid != want.ReporterMid || got.TargetMid != want.TargetMid || got.Reason != want.Reason ||
		got.Description != want.Description || got.State != want.State || got.AuditTaskId != want.AuditTaskId ||
		got.Handler != want.Handler || got.Ctime != want.Ctime || got.Mtime != want.Mtime {
		t.Fatalf("举报台账投影丢字段: %+v want %+v", got, want)
	}
	if resp.Data.NextCursor != "4000" || !resp.Data.HasMore {
		t.Fatalf("翻页位点未原样转达: %+v", resp.Data)
	}
	if fake.listReq.GetCursor() != "5000" || fake.listReq.GetPs() != 20 ||
		fake.listReq.GetOperatorMid() != 9001 || fake.listReq.GetState() != privatemessagerpc.ReportState_REPORT_STATE_PENDING {
		t.Fatalf("入参被改写: %+v", fake.listReq)
	}
}

func TestPrivateMessageReportList_EmptyListProjectsToEmptySlice(t *testing.T) {
	fake := &pmAdminFake{listReply: &privatemessagerpc.ListReportsReply{}}
	resp, err := NewPrivateMessageReportListLogic(context.Background(), pmSvc(fake)).
		PrivateMessageReportList(&types.ParamPrivateMessageReportList{OperatorMid: 9001})
	if err != nil {
		t.Fatalf("空台账不该报错: %v", err)
	}
	// state/target_mid/page_size 的 0 都是「不过滤 / 用默认」，网关不得代填成任何具体值。
	if fake.listReq.GetState() != 0 || fake.listReq.GetTargetMid() != 0 || fake.listReq.GetPs() != 0 {
		t.Fatalf("0 是合法哨兵，不能被网关代填: %+v", fake.listReq)
	}
	if resp.Data.List == nil || len(resp.Data.List) != 0 {
		t.Fatalf("空列表必须投影成 []（客户端可直接遍历），实际 %#v", resp.Data.List)
	}
}

func TestPrivateMessageAdminPlane_FailClosedBeforeDownstream(t *testing.T) {
	good := func() *types.ParamPrivateMessageReportList {
		return &types.ParamPrivateMessageReportList{OperatorMid: 9001}
	}
	goodHandle := func() *types.ParamPrivateMessageReportHandle {
		return &types.ParamPrivateMessageReportHandle{
			ReportId: 4001, Action: 1, Handler: 9001, IdempotencyKey: "k-1",
		}
	}
	goodPurge := func() *types.ParamPrivateMessagePurge {
		return &types.ParamPrivateMessagePurge{Operator: 9001, DryRun: true}
	}

	cases := []struct {
		name  string
		run   func(t *testing.T, fake *pmAdminFake, ctx context.Context) error
		wants []string // 错误串必须含的片段（逐条给出为什么被拒）
	}{
		{"举报读取缺主体", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := good()
			r.OperatorMid = 0
			_, err := NewPrivateMessageReportListLogic(ctx, pmSvc(f)).PrivateMessageReportList(r)
			return err
		}, []string{"operator_mid"}},
		{"举报读取主体为负", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := good()
			r.OperatorMid = -1
			_, err := NewPrivateMessageReportListLogic(ctx, pmSvc(f)).PrivateMessageReportList(r)
			return err
		}, []string{"operator_mid"}},
		{"target_mid 为负", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := good()
			r.TargetMid = -1
			_, err := NewPrivateMessageReportListLogic(ctx, pmSvc(f)).PrivateMessageReportList(r)
			return err
		}, []string{"target_mid"}},
		{"page_size 为负", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := good()
			r.PageSize = -1
			_, err := NewPrivateMessageReportListLogic(ctx, pmSvc(f)).PrivateMessageReportList(r)
			return err
		}, []string{"page_size"}},
		{"处置无会话身份", func(_ *testing.T, f *pmAdminFake, _ context.Context) error {
			_, err := NewPrivateMessageReportHandleLogic(pmNoSessionCtx(), pmSvc(f)).
				PrivateMessageReportHandle(goodHandle())
			return err
		}, []string{"session identity"}},
		{"处置缺 report_id", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := goodHandle()
			r.ReportId = 0
			_, err := NewPrivateMessageReportHandleLogic(ctx, pmSvc(f)).PrivateMessageReportHandle(r)
			return err
		}, []string{"report_id"}},
		{"处置动作 UNSPECIFIED", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := goodHandle()
			r.Action = 0
			_, err := NewPrivateMessageReportHandleLogic(ctx, pmSvc(f)).PrivateMessageReportHandle(r)
			return err
		}, []string{"action"}},
		{"处置缺幂等键", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := goodHandle()
			r.IdempotencyKey = "   "
			_, err := NewPrivateMessageReportHandleLogic(ctx, pmSvc(f)).PrivateMessageReportHandle(r)
			return err
		}, []string{"idempotency_key"}},
		{"处置缺处理人", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := goodHandle()
			r.Handler = 0
			_, err := NewPrivateMessageReportHandleLogic(ctx, pmSvc(f)).PrivateMessageReportHandle(r)
			return err
		}, []string{"handler"}},
		{"清理缺触发者", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := goodPurge()
			r.Operator = 0
			_, err := NewPrivateMessageRetentionPurgeLogic(ctx, pmSvc(f)).PrivateMessageRetentionPurge(r)
			return err
		}, []string{"operator"}},
		{"清理 before_time 为负", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := goodPurge()
			r.BeforeTime = -1
			_, err := NewPrivateMessageRetentionPurgeLogic(ctx, pmSvc(f)).PrivateMessageRetentionPurge(r)
			return err
		}, []string{"before_time"}},
		{"清理 batch_limit 为负", func(_ *testing.T, f *pmAdminFake, ctx context.Context) error {
			r := goodPurge()
			r.BatchLimit = -1
			_, err := NewPrivateMessageRetentionPurgeLogic(ctx, pmSvc(f)).PrivateMessageRetentionPurge(r)
			return err
		}, []string{"batch_limit"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &pmAdminFake{}
			err := c.run(t, fake, pmSession())
			if err == nil {
				t.Fatalf("%s 必须被拒，不能放行到下游", c.name)
			}
			for _, w := range c.wants {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("%s 错误串应含 %q，实际 %v", c.name, w, err)
				}
			}
			if fake.calls != 0 {
				t.Fatalf("%s 被拒后不得调用下游（calls=%d last=%s）", c.name, fake.calls, fake.lastCall)
			}
		})
	}
}

// pmNoSessionCtx 不带 admin 会话的上下文：受 AdminPermission 保护的路由拿不到身份时必须 fail-closed。
func pmNoSessionCtx() context.Context { return context.Background() }

func TestPrivateMessageAdminPlane_NilClientNeverFakesSuccess(t *testing.T) {
	// svcCtx.PrivateMessage == nil（未配 PrivateMessageRPC）：三条路由都必须显式报错。
	// 返回空台账/「已受理」会让运营以为「今天没有举报」「清理已经跑完」。
	if _, err := NewPrivateMessageReportListLogic(pmSession(), pmSvc(nil)).
		PrivateMessageReportList(&types.ParamPrivateMessageReportList{OperatorMid: 9001}); !errors.Is(err, errPMServiceNotConfigured) {
		t.Fatalf("举报读取未配置下游必须报错，实际 %v", err)
	}
	if _, err := NewPrivateMessageReportHandleLogic(pmSession(), pmSvc(nil)).
		PrivateMessageReportHandle(&types.ParamPrivateMessageReportHandle{
			ReportId: 1, Action: 1, Handler: 1, IdempotencyKey: "k"}); !errors.Is(err, errPMServiceNotConfigured) {
		t.Fatalf("举报处置未配置下游必须报错，实际 %v", err)
	}
	if _, err := NewPrivateMessageRetentionPurgeLogic(pmSession(), pmSvc(nil)).
		PrivateMessageRetentionPurge(&types.ParamPrivateMessagePurge{Operator: 1}); !errors.Is(err, errPMServiceNotConfigured) {
		t.Fatalf("留存清理未配置下游必须报错，实际 %v", err)
	}
	// req == nil 也不当成「零值请求」。
	fake := &pmAdminFake{}
	if _, err := NewPrivateMessageReportListLogic(pmSession(), pmSvc(fake)).PrivateMessageReportList(nil); !errors.Is(err, errPMRequestMissing) {
		t.Fatalf("缺请求体必须报错，实际 %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("请求体缺失不得触达下游")
	}
}

func TestPrivateMessageHandle_KeepsIdempotencyKeyAndReportsReplay(t *testing.T) {
	fake := &pmAdminFake{handle: &privatemessagerpc.HandleReportReply{
		ReportId: 4001, State: 2, Replayed: true, WithdrawMsgId: 2001,
	}}
	resp, err := NewPrivateMessageReportHandleLogic(pmSession(), pmSvc(fake)).
		PrivateMessageReportHandle(&types.ParamPrivateMessageReportHandle{
			ReportId: 4001, Action: 2, Handler: 9001, Note: "已核实", WithdrawMessage: true,
			IdempotencyKey: "  key-with-spaces  ", TraceId: "t-2",
		})
	if err != nil {
		t.Fatalf("正常处置被拒: %v", err)
	}
	if fake.handleReq.GetIdempotencyKey() != "  key-with-spaces  " {
		t.Fatalf("幂等键被改写（改一个字符等于换了执行权）: %q", fake.handleReq.GetIdempotencyKey())
	}
	if !fake.handleReq.GetWithdrawMessage() || fake.handleReq.GetNote() != "已核实" {
		t.Fatalf("连带撤回与备注未原样转达: %+v", fake.handleReq)
	}
	// replayed=true 是「命中幂等键、回的是首次结论」这个**结论**，不能折叠成错误也不能伪装成新处置。
	if !resp.Data.Replayed || resp.Data.State != 2 || resp.Data.WithdrawMsgId != 2001 {
		t.Fatalf("处置结论投影异常: %+v", resp.Data)
	}
}

func TestPrivateMessagePurge_KeepsDryRunAndCounts(t *testing.T) {
	fake := &pmAdminFake{purge: &privatemessagerpc.PurgeExpiredMessagesReply{
		ExpiredBefore: 1690000000, Scanned: 120, Purged: 0, Remaining: 8880,
	}}
	resp, err := NewPrivateMessageRetentionPurgeLogic(pmSession(), pmSvc(fake)).
		PrivateMessageRetentionPurge(&types.ParamPrivateMessagePurge{Operator: 9001, DryRun: true})
	if err != nil {
		t.Fatalf("dry_run 清理被拒: %v", err)
	}
	if !fake.purgeReq.GetDryRun() {
		t.Fatalf("dry_run 位丢失：网关把「只看不动」改成了「真删」")
	}
	if fake.purgeReq.GetBeforeTime() != 0 || fake.purgeReq.GetBatchLimit() != 0 {
		t.Fatalf("0 是「由服务推算/用默认」的哨兵，网关不得代填: %+v", fake.purgeReq)
	}
	if resp.Data.ExpiredBefore != 1690000000 || resp.Data.Scanned != 120 ||
		resp.Data.Purged != 0 || resp.Data.Remaining != 8880 {
		t.Fatalf("清理计数投影异常: %+v", resp.Data)
	}
}

func TestPrivateMessageAdminPlane_DownstreamErrorPassesThrough(t *testing.T) {
	// 下游错误原样上抛，交给 common/httpresponse 出四字段信封；
	// 网关不把它翻译成「成功但空台账」，也不吞掉「举报不存在」这类必须让运营看到的结论。
	fake := &pmAdminFake{err: errPMFakeDownstream}
	if _, err := NewPrivateMessageReportListLogic(pmSession(), pmSvc(fake)).
		PrivateMessageReportList(&types.ParamPrivateMessageReportList{OperatorMid: 9001}); !errors.Is(err, errPMFakeDownstream) {
		t.Fatalf("下游错误被改写: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("应当只调用一次下游，实际 %d", fake.calls)
	}
}
