package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	commentrpc "go-video/services/comment/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 comment 的口径：入参如何装配成 RPC 请求（含 SortMode 枚举与
// 待审初始状态）、RPC 回复如何投影成客户端 types（列表、计数快照、信封四字段）。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeCommentClient struct {
	commentrpc.CommentClient

	err   error
	calls int

	postReq    *commentrpc.PostCommentReq
	postReply  *commentrpc.PostCommentReply
	listReq    *commentrpc.ListCommentsReq
	listReply  *commentrpc.ListCommentsReply
	repliesReq *commentrpc.ListRepliesReq
	replies    *commentrpc.ListRepliesReply
	deleteReq  *commentrpc.DeleteCommentReq
	pinReq     *commentrpc.PinCommentReq
	reportReq  *commentrpc.ReportCommentReq
	statsReq   *commentrpc.CommentStatsReq
	statsReply *commentrpc.CommentStatsReply
}

func (f *fakeCommentClient) PostComment(_ context.Context, in *commentrpc.PostCommentReq,
	_ ...grpc.CallOption) (*commentrpc.PostCommentReply, error) {
	f.calls++
	f.postReq = in
	return f.postReply, f.err
}

func (f *fakeCommentClient) DeleteComment(_ context.Context, in *commentrpc.DeleteCommentReq,
	_ ...grpc.CallOption) (*commentrpc.EmptyReply, error) {
	f.calls++
	f.deleteReq = in
	return &commentrpc.EmptyReply{}, f.err
}

func (f *fakeCommentClient) ListComments(_ context.Context, in *commentrpc.ListCommentsReq,
	_ ...grpc.CallOption) (*commentrpc.ListCommentsReply, error) {
	f.calls++
	f.listReq = in
	return f.listReply, f.err
}

func (f *fakeCommentClient) ListReplies(_ context.Context, in *commentrpc.ListRepliesReq,
	_ ...grpc.CallOption) (*commentrpc.ListRepliesReply, error) {
	f.calls++
	f.repliesReq = in
	return f.replies, f.err
}

func (f *fakeCommentClient) PinComment(_ context.Context, in *commentrpc.PinCommentReq,
	_ ...grpc.CallOption) (*commentrpc.EmptyReply, error) {
	f.calls++
	f.pinReq = in
	return &commentrpc.EmptyReply{}, f.err
}

func (f *fakeCommentClient) ReportComment(_ context.Context, in *commentrpc.ReportCommentReq,
	_ ...grpc.CallOption) (*commentrpc.EmptyReply, error) {
	f.calls++
	f.reportReq = in
	return &commentrpc.EmptyReply{}, f.err
}

func (f *fakeCommentClient) CommentStats(_ context.Context, in *commentrpc.CommentStatsReq,
	_ ...grpc.CallOption) (*commentrpc.CommentStatsReply, error) {
	f.calls++
	f.statsReq = in
	return f.statsReply, f.err
}

// commentFixture 一条覆盖全字段的评论，state 用审核驳回态以确认网关不加工语义。
func commentFixture() *commentrpc.CommentInfo {
	return &commentrpc.CommentInfo{
		Rpid:       555,
		Oid:        42,
		Tp:         1,
		Root:       100,
		Parent:     200,
		Mid:        777,
		Content:    "内容明文",
		State:      int32(commentrpc.CommentState_STATE_REJECTED),
		Ctime:      1700,
		Mtime:      1800,
		LikeCount:  9,
		ReplyCount: 3,
	}
}

func assertCommentItem(t *testing.T, got types.CommentItem) {
	t.Helper()
	want := commentFixture()
	if got.Rpid != want.GetRpid() || got.Oid != want.GetOid() || got.Tp != want.GetTp() ||
		got.Root != want.GetRoot() || got.Parent != want.GetParent() || got.Mid != want.GetMid() ||
		got.Content != want.GetContent() || got.State != want.GetState() ||
		got.Ctime != want.GetCtime() || got.Mtime != want.GetMtime() ||
		got.LikeCount != want.GetLikeCount() || got.ReplyCount != want.GetReplyCount() {
		t.Fatalf("CommentItem 投影不完整: %+v", got)
	}
}

func TestCommentPostCommentMapsPendingState(t *testing.T) {
	fake := &fakeCommentClient{postReply: &commentrpc.PostCommentReply{Rpid: 999, Ctime: 1710}}
	l := NewPostCommentLogic(context.Background(), &svc.ServiceContext{Comment: fake})
	resp, err := l.PostComment(&types.ParamPostComment{
		Oid: 42, Tp: 1, Root: 100, Parent: 200, Mid: 777, Content: "内容明文", TraceId: "tr-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 终端不能自选状态：网关必须固定以待审态提交，否则等于绕过 moderation（AGENTS.md §8）。
	if fake.postReq.GetState() != int32(commentrpc.CommentState_STATE_PENDING) {
		t.Fatalf("state = %d, want %d（STATE_PENDING）",
			fake.postReq.GetState(), int32(commentrpc.CommentState_STATE_PENDING))
	}
	if fake.postReq.GetOid() != 42 || fake.postReq.GetTp() != 1 || fake.postReq.GetRoot() != 100 ||
		fake.postReq.GetParent() != 200 || fake.postReq.GetMid() != 777 ||
		fake.postReq.GetContent() != "内容明文" || fake.postReq.GetTraceId() != "tr-1" {
		t.Fatalf("入参未原样透传: %+v", fake.postReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data.Rpid != 999 || resp.Data.Ctime != 1710 {
		t.Fatalf("data = %+v（rpid/ctime 必须来自服务端）", resp.Data)
	}
}

func TestCommentListCommentsForwardsViewerSortAndPaging(t *testing.T) {
	fake := &fakeCommentClient{listReply: &commentrpc.ListCommentsReply{
		Comments: []*commentrpc.CommentInfo{commentFixture()},
		Total:    1,
	}}
	l := NewListCommentsLogic(context.Background(), &svc.ServiceContext{Comment: fake})
	resp, err := l.ListComments(&types.ParamListComments{
		Oid: 42, Tp: 6, Mid: 777, Sort: int32(commentrpc.SortMode_SORT_TIME), Pn: 3, Ps: 20,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 查看者 mid 与分页参数必须原样落到 viewer_mid/pn/ps：过滤规则在服务侧，网关不改写。
	if fake.listReq.GetViewerMid() != 777 {
		t.Fatalf("viewer_mid = %d, want 777", fake.listReq.GetViewerMid())
	}
	if fake.listReq.GetPn() != 3 || fake.listReq.GetPs() != 20 {
		t.Fatalf("pn/ps = %d/%d, want 3/20（网关不擅自放大页大小）", fake.listReq.GetPn(), fake.listReq.GetPs())
	}
	if fake.listReq.GetSort() != commentrpc.SortMode_SORT_TIME {
		t.Fatalf("sort = %v, want SORT_TIME（int32 → 枚举必须显式转换）", fake.listReq.GetSort())
	}
	if fake.listReq.GetOid() != 42 || fake.listReq.GetTp() != 6 {
		t.Fatalf("目标未透传: %+v", fake.listReq)
	}
	if resp.Data.Total != 1 || len(resp.Data.List) != 1 {
		t.Fatalf("data = %+v", resp.Data)
	}
	assertCommentItem(t, resp.Data.List[0])
}

func TestCommentListCommentsEmptyReplyYieldsNonNilSlice(t *testing.T) {
	fake := &fakeCommentClient{listReply: &commentrpc.ListCommentsReply{Total: 0}}
	l := NewListCommentsLogic(context.Background(), &svc.ServiceContext{Comment: fake})
	resp, err := l.ListComments(&types.ParamListComments{Oid: 42, Tp: 1, Ps: 20, Pn: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.List == nil || len(resp.Data.List) != 0 {
		t.Fatalf("list = %v, want 空切片（客户端要拿到 [] 而不是 null）", resp.Data.List)
	}
	// sort=0 落到 SORT_UNSPECIFIED，由 comment 服务按热度默认排序，网关不替客户端猜排序。
	if fake.listReq.GetSort() != commentrpc.SortMode_SORT_UNSPECIFIED {
		t.Fatalf("sort = %v, want SORT_UNSPECIFIED", fake.listReq.GetSort())
	}
	if got := commentListToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("commentListToAPI(nil) = %v, want 空切片", got)
	}
	// 列表中的 nil 元素（proto 未填）投影成零值，不能 panic。
	if got := commentListToAPI([]*commentrpc.CommentInfo{nil}); len(got) != 1 || got[0].Rpid != 0 {
		t.Fatalf("commentListToAPI 含 nil 元素 = %+v", got)
	}
}

func TestCommentListRepliesProjectsReplyField(t *testing.T) {
	fake := &fakeCommentClient{replies: &commentrpc.ListRepliesReply{
		Replies: []*commentrpc.CommentInfo{commentFixture()},
		Total:   7,
	}}
	l := NewListCommentRepliesLogic(context.Background(), &svc.ServiceContext{Comment: fake})
	resp, err := l.ListCommentReplies(&types.ParamListCommentReplies{Root: 100, Mid: 777, Pn: 2, Ps: 30})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 楼中楼回复在 RPC 里是 replies 字段（根评论是 comments），投影必须走 GetReplies()。
	if fake.repliesReq.GetRoot() != 100 || fake.repliesReq.GetViewerMid() != 777 ||
		fake.repliesReq.GetPn() != 2 || fake.repliesReq.GetPs() != 30 {
		t.Fatalf("ListReplies 入参未透传: %+v", fake.repliesReq)
	}
	if resp.Data.Total != 7 || len(resp.Data.List) != 1 {
		t.Fatalf("data = %+v", resp.Data)
	}
	assertCommentItem(t, resp.Data.List[0])
}

func TestCommentDeletePinReportPassThroughAndEmptyEnvelope(t *testing.T) {
	fake := &fakeCommentClient{}
	ctx := context.Background()

	del, err := NewDeleteCommentLogic(ctx, &svc.ServiceContext{Comment: fake}).
		DeleteComment(&types.ParamDeleteComment{Rpid: 555, Mid: 777})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 终端网关固定 admin=false：管理员删除只在 gateway/admin 暴露（AGENTS.md §3）。
	if fake.deleteReq.GetRpid() != 555 || fake.deleteReq.GetMid() != 777 || fake.deleteReq.GetAdmin() {
		t.Fatalf("DeleteComment 入参 = %+v", fake.deleteReq)
	}
	if del.Code != 0 || del.Message != "ok" || del.TTL != 0 || del.Data != (types.EmptyData{}) {
		t.Fatalf("EmptyResponse 信封 = %+v", del)
	}

	pin, err := NewPinCommentLogic(ctx, &svc.ServiceContext{Comment: fake}).
		PinComment(&types.ParamPinComment{Rpid: 555, Oid: 42, Pin: true, AdminMid: 888})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.pinReq.GetRpid() != 555 || fake.pinReq.GetOid() != 42 || !fake.pinReq.GetPin() ||
		fake.pinReq.GetAdminMid() != 888 {
		t.Fatalf("PinComment 入参未透传: %+v", fake.pinReq)
	}
	if pin.Code != 0 || pin.Message != "ok" {
		t.Fatalf("EmptyResponse 信封 = %+v", pin)
	}

	rep, err := NewReportCommentLogic(ctx, &svc.ServiceContext{Comment: fake}).
		ReportComment(&types.ParamReportComment{
			Rpid: 555, ReporterMid: 777, Reason: 3, Content: "广告", TraceId: "tr-2",
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.reportReq.GetRpid() != 555 || fake.reportReq.GetReporterMid() != 777 ||
		fake.reportReq.GetReason() != 3 || fake.reportReq.GetContent() != "广告" ||
		fake.reportReq.GetTraceId() != "tr-2" {
		t.Fatalf("ReportComment 入参未透传: %+v", fake.reportReq)
	}
	if rep.Code != 0 || rep.Message != "ok" {
		t.Fatalf("EmptyResponse 信封 = %+v", rep)
	}
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3", fake.calls)
	}
}

func TestCommentStatsProjectsBothCounters(t *testing.T) {
	fake := &fakeCommentClient{statsReply: &commentrpc.CommentStatsReply{Total: 1234, RootTotal: 56}}
	l := NewCommentStatsLogic(context.Background(), &svc.ServiceContext{Comment: fake})
	resp, err := l.CommentStats(&types.ParamCommentStats{Oid: 42, Tp: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.statsReq.GetOid() != 42 || fake.statsReq.GetTp() != 1 {
		t.Fatalf("CommentStats 入参未透传: %+v", fake.statsReq)
	}
	if resp.Data.Total != 1234 || resp.Data.RootTotal != 56 {
		t.Fatalf("data = %+v（两个计数缺一不可，客户端要区分总楼与根评论）", resp.Data)
	}
}

func TestCommentLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func() error
	}{
		{"postComment", func() error {
			_, err := NewPostCommentLogic(context.Background(), empty).
				PostComment(&types.ParamPostComment{Oid: 42, Mid: 777, Content: "x"})
			return err
		}},
		{"listComments", func() error {
			_, err := NewListCommentsLogic(context.Background(), empty).
				ListComments(&types.ParamListComments{Oid: 42})
			return err
		}},
		{"listCommentReplies", func() error {
			_, err := NewListCommentRepliesLogic(context.Background(), empty).
				ListCommentReplies(&types.ParamListCommentReplies{Root: 100})
			return err
		}},
		{"deleteComment", func() error {
			_, err := NewDeleteCommentLogic(context.Background(), empty).
				DeleteComment(&types.ParamDeleteComment{Rpid: 555, Mid: 777})
			return err
		}},
		{"pinComment", func() error {
			_, err := NewPinCommentLogic(context.Background(), empty).
				PinComment(&types.ParamPinComment{Rpid: 555, Oid: 42, AdminMid: 888})
			return err
		}},
		{"reportComment", func() error {
			_, err := NewReportCommentLogic(context.Background(), empty).
				ReportComment(&types.ParamReportComment{Rpid: 555, ReporterMid: 777})
			return err
		}},
		{"commentStats", func() error {
			_, err := NewCommentStatsLogic(context.Background(), empty).
				CommentStats(&types.ParamCommentStats{Oid: 42})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err == nil {
				t.Fatalf("%s：未配置 CommentRPC 时必须报错，不能退化成空成功响应", c.name)
			}
		})
	}
}

func TestCommentLogicsPropagateDownstreamError(t *testing.T) {
	sentinel := errors.New("comment: target not found")
	cases := []struct {
		name string
		call func(commentrpc.CommentClient) error
	}{
		{"postComment", func(c commentrpc.CommentClient) error {
			_, err := NewPostCommentLogic(context.Background(), &svc.ServiceContext{Comment: c}).
				PostComment(&types.ParamPostComment{Oid: 42, Mid: 777, Content: "x"})
			return err
		}},
		{"listComments", func(c commentrpc.CommentClient) error {
			_, err := NewListCommentsLogic(context.Background(), &svc.ServiceContext{Comment: c}).
				ListComments(&types.ParamListComments{Oid: 42, Ps: 20, Pn: 1})
			return err
		}},
		{"listCommentReplies", func(c commentrpc.CommentClient) error {
			_, err := NewListCommentRepliesLogic(context.Background(), &svc.ServiceContext{Comment: c}).
				ListCommentReplies(&types.ParamListCommentReplies{Root: 100, Ps: 20, Pn: 1})
			return err
		}},
		{"deleteComment", func(c commentrpc.CommentClient) error {
			_, err := NewDeleteCommentLogic(context.Background(), &svc.ServiceContext{Comment: c}).
				DeleteComment(&types.ParamDeleteComment{Rpid: 555, Mid: 777})
			return err
		}},
		{"pinComment", func(c commentrpc.CommentClient) error {
			_, err := NewPinCommentLogic(context.Background(), &svc.ServiceContext{Comment: c}).
				PinComment(&types.ParamPinComment{Rpid: 555, Oid: 42, AdminMid: 888})
			return err
		}},
		{"reportComment", func(c commentrpc.CommentClient) error {
			_, err := NewReportCommentLogic(context.Background(), &svc.ServiceContext{Comment: c}).
				ReportComment(&types.ParamReportComment{Rpid: 555, ReporterMid: 777})
			return err
		}},
		{"commentStats", func(c commentrpc.CommentClient) error {
			_, err := NewCommentStatsLogic(context.Background(), &svc.ServiceContext{Comment: c}).
				CommentStats(&types.ParamCommentStats{Oid: 42})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeCommentClient{err: sentinel}
			err := c.call(fake)
			// 错误必须原样上抛，由 common/httpresponse 渲染 code/message/data/ttl，
			// 网关不得伪造成功（docs/api-and-events.md §2）。
			if !errors.Is(err, sentinel) {
				t.Fatalf("err = %v, want %v", err, sentinel)
			}
		})
	}
}
