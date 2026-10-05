package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	commentrpc "go-video/services/comment/rpc"
	danmakurpc "go-video/services/danmaku/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 comment（以及同为运营写操作的 danmaku 删除）的口径：
// 审计主体怎么传、admin=true 是否真的落到下游、分页与排序枚举怎么升维、RPC 消息如何投影成后台 types。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。
// 标识符统一带 Comment/Danmaku 前缀，避免与同包其它域的测试撞名。

// fakeCommentCli 只实现本文件用到的 5 个方法，其余继承接口（未调用即 panic，用例越界会立刻暴露）。
type fakeCommentCli struct {
	commentrpc.CommentClient

	listCommentsReq  *commentrpc.ListCommentsReq
	listCommentsRply *commentrpc.ListCommentsReply
	listRepliesReq   *commentrpc.ListRepliesReq
	listRepliesRply  *commentrpc.ListRepliesReply
	deleteReq        *commentrpc.DeleteCommentReq
	pinReq           *commentrpc.PinCommentReq
	statsReq         *commentrpc.CommentStatsReq
	statsRply        *commentrpc.CommentStatsReply
	err              error
	calls            int
}

func (f *fakeCommentCli) ListComments(_ context.Context, in *commentrpc.ListCommentsReq,
	_ ...grpc.CallOption) (*commentrpc.ListCommentsReply, error) {
	f.calls++
	f.listCommentsReq = in
	return f.listCommentsRply, f.err
}

func (f *fakeCommentCli) ListReplies(_ context.Context, in *commentrpc.ListRepliesReq,
	_ ...grpc.CallOption) (*commentrpc.ListRepliesReply, error) {
	f.calls++
	f.listRepliesReq = in
	return f.listRepliesRply, f.err
}

func (f *fakeCommentCli) DeleteComment(_ context.Context, in *commentrpc.DeleteCommentReq,
	_ ...grpc.CallOption) (*commentrpc.EmptyReply, error) {
	f.calls++
	f.deleteReq = in
	return &commentrpc.EmptyReply{}, f.err
}

func (f *fakeCommentCli) PinComment(_ context.Context, in *commentrpc.PinCommentReq,
	_ ...grpc.CallOption) (*commentrpc.EmptyReply, error) {
	f.calls++
	f.pinReq = in
	return &commentrpc.EmptyReply{}, f.err
}

func (f *fakeCommentCli) CommentStats(_ context.Context, in *commentrpc.CommentStatsReq,
	_ ...grpc.CallOption) (*commentrpc.CommentStatsReply, error) {
	f.calls++
	f.statsReq = in
	return f.statsRply, f.err
}

// fakeDanmakuAdminCli 只覆盖 DeleteDanmaku：验证运营删除确实带上 admin=true。
type fakeDanmakuAdminCli struct {
	danmakurpc.DanmakuClient

	deleteReq *danmakurpc.DeleteDanmakuReq
	err       error
	calls     int
}

func (f *fakeDanmakuAdminCli) DeleteDanmaku(_ context.Context, in *danmakurpc.DeleteDanmakuReq,
	_ ...grpc.CallOption) (*danmakurpc.EmptyReply, error) {
	f.calls++
	f.deleteReq = in
	return &danmakurpc.EmptyReply{}, f.err
}

// commentErr 用可判定的哨兵错误，确保网关原样传播下游错误而不是伪造成功。
var commentErr = errors.New("comment: rpc unavailable")

func TestCommentNormalizePage(t *testing.T) {
	cases := []struct {
		name           string
		pn, ps         int32
		wantPn, wantPs int32
	}{
		{"缺省页大小保持 20", 1, 20, 1, 20},
		{"页码 0 归一为 1", 0, 20, 1, 20},
		{"负页码归一为 1", -5, 20, 1, 20},
		{"ps 0 回落 20", 1, 0, 1, 20},
		{"负 ps 回落 20", 1, -3, 1, 20},
		// comment 服务对 ps>49 是硬拒绝（model.ErrPsTooLarge），网关先截断，
		// 后台页大小选择器不会构造出必然失败的请求。
		{"ps 超上限截断到 49", 2, 500, 2, 49},
		{"ps 边界 49 原样", 3, 49, 3, 49},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pn, ps := normalizeCommentPage(c.pn, c.ps)
			if pn != c.wantPn || ps != c.wantPs {
				t.Fatalf("normalizeCommentPage(%d,%d) = %d,%d, want %d,%d", c.pn, c.ps, pn, ps, c.wantPn, c.wantPs)
			}
		})
	}
}

func TestCommentSortModeEnumGuard(t *testing.T) {
	for _, c := range []struct {
		name string
		v    int32
		want commentrpc.SortMode
	}{
		{"未指定按热度", 0, commentrpc.SortMode_SORT_UNSPECIFIED},
		{"热度", 1, commentrpc.SortMode_SORT_HOT},
		{"时间倒序", 2, commentrpc.SortMode_SORT_TIME},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := commentSortMode(c.v)
			if err != nil {
				t.Fatalf("commentSortMode(%d) 出错: %v", c.v, err)
			}
			if got != c.want {
				t.Fatalf("commentSortMode(%d) = %v, want %v", c.v, got, c.want)
			}
		})
	}
	// 未知枚举不能让下游静默退回热度序。
	if _, err := commentSortMode(7); err == nil || !strings.Contains(err.Error(), "sort") {
		t.Fatalf("未知 sort 应点名 sort，got %v", err)
	}
	if _, err := commentSortMode(-1); err == nil {
		t.Fatal("负数 sort 应被拒绝")
	}
}

func TestCommentListCommentsForwardsEnumAndPagingAndProjects(t *testing.T) {
	fake := &fakeCommentCli{listCommentsRply: &commentrpc.ListCommentsReply{
		Comments: []*commentrpc.CommentInfo{
			{
				Rpid: 101, Oid: 900, Tp: 6, Root: 0, Parent: 0, Mid: 42,
				Content: "第一层", State: int32(commentrpc.CommentState_STATE_FOLDED),
				Ctime: 1700, Mtime: 1701, LikeCount: 7, ReplyCount: 3,
			},
			nil, // 下游理论上不会给 nil 元素，网关也必须稳定投影成零值而不是 panic
		},
		Total: 12,
	}}
	l := NewAdminListCommentsLogic(context.Background(), &svc.ServiceContext{Comment: fake})

	resp, err := l.AdminListComments(&types.ParamAdminListComments{Oid: 900, Tp: 6, Sort: 2, Pn: 3, Ps: 500})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.listCommentsReq
	if in.GetOid() != 900 || in.GetTp() != 6 {
		t.Fatalf("目标未透传: %+v", in)
	}
	// 枚举升维：后台的 int32 2 必须变成 SortMode_SORT_TIME。
	if in.GetSort() != commentrpc.SortMode_SORT_TIME {
		t.Fatalf("sort = %v, want SORT_TIME", in.GetSort())
	}
	if in.GetPn() != 3 || in.GetPs() != commentMaxPageSize {
		t.Fatalf("分页 = %d/%d, want 3/%d", in.GetPn(), in.GetPs(), commentMaxPageSize)
	}
	// 后台没有「查看者」身份，viewer_mid 必须留 0，让服务侧跳过个性化过滤。
	if in.GetViewerMid() != 0 {
		t.Fatalf("viewer_mid = %d, want 0", in.GetViewerMid())
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data.Total != 12 {
		t.Fatalf("total = %d, want 12", resp.Data.Total)
	}
	if len(resp.Data.List) != 2 {
		t.Fatalf("条目数 = %d, want 2", len(resp.Data.List))
	}
	first := resp.Data.List[0]
	if first.Rpid != 101 || first.Content != "第一层" || first.LikeCount != 7 || first.ReplyCount != 3 ||
		first.Ctime != 1700 || first.Mtime != 1701 || first.Mid != 42 {
		t.Fatalf("投影丢字段: %+v", first)
	}
	// state 是契约里的 int32 快照，网关原样透出（折叠=1），不替后台解释含义。
	if first.State != int32(commentrpc.CommentState_STATE_FOLDED) {
		t.Fatalf("state = %d, want %d", first.State, commentrpc.CommentState_STATE_FOLDED)
	}
	if got := resp.Data.List[1]; got.Rpid != 0 || got.Content != "" {
		t.Fatalf("nil 元素应投影为零值条目: %+v", got)
	}
}

func TestCommentAdminListCommentRepliesProjectsReplies(t *testing.T) {
	fake := &fakeCommentCli{listRepliesRply: &commentrpc.ListRepliesReply{
		Replies: []*commentrpc.CommentInfo{{Rpid: 202, Root: 101, Parent: 101, State: 2}},
		Total:   9,
	}}
	l := NewAdminListCommentRepliesLogic(context.Background(), &svc.ServiceContext{Comment: fake})

	resp, err := l.AdminListCommentReplies(&types.ParamAdminListCommentReplies{Root: 101, Pn: 0, Ps: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.listRepliesReq
	if in.GetRoot() != 101 || in.GetPn() != 1 || in.GetPs() != commentDefaultPageSize {
		t.Fatalf("楼中楼入参 = %+v, want root=101 pn=1 ps=%d", in, commentDefaultPageSize)
	}
	if resp.Data.Total != 9 || len(resp.Data.List) != 1 {
		t.Fatalf("data = %+v", resp.Data)
	}
	if resp.Data.List[0].Rpid != 202 || resp.Data.List[0].Root != 101 || resp.Data.List[0].State != 2 {
		t.Fatalf("投影 = %+v", resp.Data.List[0])
	}
}

// 运营写操作必须带 admin=true，否则 comment/danmaku 会按「本人操作」判定归属并拒绝。
func TestCommentAdminDeleteCommentSendsAdminTrue(t *testing.T) {
	fake := &fakeCommentCli{}
	l := NewAdminDeleteCommentLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Comment: fake})

	resp, err := l.AdminDeleteComment(&types.ParamAdminDeleteComment{Rpid: 101, OperatorMid: 77})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = %+v", resp)
	}
	// 契约是 EmptyReply：data 必须是空对象而不是 null。
	if resp.Data != (types.EmptyData{}) {
		t.Fatalf("EmptyResponse.data = %+v, want 空对象", resp.Data)
	}
	in := fake.deleteReq
	// mid 属于用户 ID 空间：会话里的 admin_id（gateAdminID）绝不能覆盖它，
	// 否则一次处置会被记到某个无关用户头上。口径见 adminsubject.go 文件头。
	if in.GetRpid() != 101 || in.GetMid() != 77 {
		t.Fatalf("删除入参 = %+v, want rpid=101 mid=77", in)
	}
	if !in.GetAdmin() {
		t.Fatal("运营删除必须带 admin=true，否则会被服务侧按本人校验拒绝")
	}
}

func TestCommentAdminPinCommentForwardsOperatorAsAdminMid(t *testing.T) {
	fake := &fakeCommentCli{}
	l := NewAdminPinCommentLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Comment: fake})

	if _, err := l.AdminPinComment(&types.ParamAdminPinComment{Rpid: 101, Oid: 900, Pin: false, OperatorMid: 77}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	in := fake.pinReq
	if in.GetRpid() != 101 || in.GetOid() != 900 || in.GetPin() || in.GetAdminMid() != 77 {
		t.Fatalf("置顶入参 = %+v, want rpid=101 oid=900 pin=false admin_mid=77", in)
	}
}

func TestCommentAdminCommentStatsProjectsServerCounters(t *testing.T) {
	fake := &fakeCommentCli{statsRply: &commentrpc.CommentStatsReply{Total: 4000, RootTotal: 37}}
	l := NewAdminCommentStatsLogic(context.Background(), &svc.ServiceContext{Comment: fake})

	resp, err := l.AdminCommentStats(&types.ParamAdminCommentStats{Oid: 900, Tp: 6})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.statsReq.GetOid() != 900 || fake.statsReq.GetTp() != 6 {
		t.Fatalf("stats 入参 = %+v", fake.statsReq)
	}
	if resp.Data.Total != 4000 || resp.Data.RootTotal != 37 || resp.TTL != 0 {
		t.Fatalf("data = %+v", resp.Data)
	}
}

// 门槛校验必须在调用下游之前完成：缺主体时一次 RPC 都不该发出去。
func TestCommentWriteLogicsRequireOperatorBeforeCalling(t *testing.T) {
	fake := &fakeCommentCli{}
	if _, err := NewAdminDeleteCommentLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Comment: fake}).
		AdminDeleteComment(&types.ParamAdminDeleteComment{Rpid: 101, OperatorMid: 0}); err == nil {
		t.Fatal("缺 operator_mid 的删除应被拒绝")
	} else if !strings.Contains(err.Error(), "operator_mid") {
		t.Fatalf("错误消息应点名 operator_mid（与请求体字段一致），got %v", err)
	}
	if _, err := NewAdminPinCommentLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Comment: fake}).
		AdminPinComment(&types.ParamAdminPinComment{Rpid: 101, Oid: 900, OperatorMid: -1}); err == nil {
		t.Fatal("负 operator_mid 的置顶应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	// 没有后台会话时，即使 operator_mid 合法也必须拒绝：网关无法证明这个人是谁。
	if _, err := NewAdminDeleteCommentLogic(context.Background(), &svc.ServiceContext{Comment: fake}).
		AdminDeleteComment(&types.ParamAdminDeleteComment{Rpid: 101, OperatorMid: 77}); err == nil ||
		!strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话却放行: %v", err)
	}
	dmNoSession := &fakeDanmakuAdminCli{}
	if _, err := NewAdminDeleteDanmakuLogic(context.Background(), &svc.ServiceContext{Danmaku: dmNoSession}).
		AdminDeleteDanmaku(&types.ParamAdminDeleteDanmaku{Dmid: 1, OperatorMid: 77}); err == nil ||
		!strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("弹幕无会话却放行: %v", err)
	}
	if dmNoSession.calls != 0 {
		t.Fatalf("弹幕无会话被拒后仍调用下游 %d 次", dmNoSession.calls)
	}
	if fake.calls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", fake.calls)
	}

	// 未知 sort 同样在网关挡下，不让服务侧静默退回热度序。
	if _, err := NewAdminListCommentsLogic(context.Background(), &svc.ServiceContext{Comment: fake}).
		AdminListComments(&types.ParamAdminListComments{Oid: 1, Sort: 9}); err == nil {
		t.Fatal("未知 sort 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("枚举校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}
}

// 未配置下游时必须显式失败，而不是返回一个看起来成功的空列表。
func TestCommentLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	cases := []struct {
		name string
		call func() error
	}{
		{"list", func() error {
			_, err := NewAdminListCommentsLogic(context.Background(), empty).AdminListComments(&types.ParamAdminListComments{Oid: 1})
			return err
		}},
		{"replies", func() error {
			_, err := NewAdminListCommentRepliesLogic(context.Background(), empty).AdminListCommentReplies(&types.ParamAdminListCommentReplies{Root: 1})
			return err
		}},
		{"delete", func() error {
			_, err := NewAdminDeleteCommentLogic(withAdminSession(gateAdminID), empty).AdminDeleteComment(&types.ParamAdminDeleteComment{Rpid: 1, OperatorMid: 7})
			return err
		}},
		{"pin", func() error {
			_, err := NewAdminPinCommentLogic(withAdminSession(gateAdminID), empty).AdminPinComment(&types.ParamAdminPinComment{Rpid: 1, OperatorMid: 7})
			return err
		}},
		{"stats", func() error {
			_, err := NewAdminCommentStatsLogic(context.Background(), empty).AdminCommentStats(&types.ParamAdminCommentStats{Oid: 1})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil || !strings.Contains(err.Error(), "comment service not configured") {
				t.Fatalf("%s 未配置 comment 时应失败, got %v", c.name, err)
			}
		})
	}
}

func TestCommentLogicsPropagateDownstreamError(t *testing.T) {
	fake := &fakeCommentCli{err: commentErr, statsRply: &commentrpc.CommentStatsReply{}}
	ctx := context.Background()
	// delete/pin 是受保护写入口：无会话会在触达下游前就被拒，所以这里必须带上会话身份。
	wCtx := withAdminSession(gateAdminID)
	empty := &svc.ServiceContext{}

	calls := map[string]func() error{
		"list": func() error {
			_, err := NewAdminListCommentsLogic(ctx, &svc.ServiceContext{Comment: fake}).
				AdminListComments(&types.ParamAdminListComments{Oid: 1, Ps: 20})
			return err
		},
		"replies": func() error {
			_, err := NewAdminListCommentRepliesLogic(ctx, &svc.ServiceContext{Comment: fake}).
				AdminListCommentReplies(&types.ParamAdminListCommentReplies{Root: 1})
			return err
		},
		"delete": func() error {
			_, err := NewAdminDeleteCommentLogic(wCtx, &svc.ServiceContext{Comment: fake}).
				AdminDeleteComment(&types.ParamAdminDeleteComment{Rpid: 1, OperatorMid: 7})
			return err
		},
		"pin": func() error {
			_, err := NewAdminPinCommentLogic(wCtx, &svc.ServiceContext{Comment: fake}).
				AdminPinComment(&types.ParamAdminPinComment{Rpid: 1, OperatorMid: 7})
			return err
		},
		"stats": func() error {
			_, err := NewAdminCommentStatsLogic(ctx, &svc.ServiceContext{Comment: fake}).
				AdminCommentStats(&types.ParamAdminCommentStats{Oid: 1})
			return err
		},
		"nil-client": func() error {
			_, err := NewAdminListCommentsLogic(ctx, empty).AdminListComments(&types.ParamAdminListComments{Oid: 1})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatalf("%s 应把下游错误原样抛出，让 httpresponse 渲染信封", name)
			}
			if name == "nil-client" {
				if !strings.Contains(err.Error(), "comment service not configured") {
					t.Fatalf("nil-client 错误 = %v", err)
				}
				return
			}
			if !errors.Is(err, commentErr) {
				t.Fatalf("错误未原样传播: %v", err)
			}
		})
	}
}

func TestCommentProjectionsNilInput(t *testing.T) {
	if got := commentListToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("nil 列表应投影成空切片, got %#v", got)
	}
	if got := commentListToAPI([]*commentrpc.CommentInfo{nil}); len(got) != 1 || got[0].Rpid != 0 {
		t.Fatalf("nil 元素应保留为零值条目, got %#v", got)
	}
	if item := commentItemToAPI(nil); item != (types.AdminCommentItem{}) {
		t.Fatalf("nil 条目 = %+v, want 零值", item)
	}
}

// danmaku 运营删除与评论删除共用同一条「带上审计主体 + admin=true」的写路径，
// 为不新增第三个测试文件放在本文件末尾（标识符仍带 Danmaku 前缀）。
func TestDanmakuAdminDeleteSendsAdminTrueAndReason(t *testing.T) {
	fake := &fakeDanmakuAdminCli{}
	l := NewAdminDeleteDanmakuLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Danmaku: fake})

	resp, err := l.AdminDeleteDanmaku(&types.ParamAdminDeleteDanmaku{
		Dmid: 505, OperatorMid: 77, Reason: "涉政违规", TraceId: "trace-dm-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.Data != (types.EmptyData{}) || resp.TTL != 0 {
		t.Fatalf("信封 = %+v", resp)
	}
	in := fake.deleteReq
	if in.GetDmid() != 505 || in.GetMid() != 77 {
		t.Fatalf("删除入参 = %+v, want dmid=505 mid=77", in)
	}
	if !in.GetAdmin() {
		t.Fatal("运营删除弹幕必须带 admin=true")
	}
	if in.GetReason() != "涉政违规" || in.GetTraceId() != "trace-dm-1" {
		t.Fatalf("reason/trace_id 必须透传给 danmaku 的 op_log 审计: %+v", in)
	}

	// 缺主体：一次 RPC 都不该发出。
	if _, err := l.AdminDeleteDanmaku(&types.ParamAdminDeleteDanmaku{Dmid: 505}); err == nil ||
		!strings.Contains(err.Error(), "operator_mid") {
		t.Fatalf("缺 operator_mid 应点名该字段, got %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}
}

func TestDanmakuAdminDeleteWithoutClientAndOnError(t *testing.T) {
	dmErr := errors.New("danmaku: danmaku not found")
	fake := &fakeDanmakuAdminCli{err: dmErr}
	if _, err := NewAdminDeleteDanmakuLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Danmaku: fake}).
		AdminDeleteDanmaku(&types.ParamAdminDeleteDanmaku{Dmid: 1, OperatorMid: 7}); !errors.Is(err, dmErr) {
		t.Fatalf("下游错误未原样传播: %v", err)
	}

	_, err := NewAdminDeleteDanmakuLogic(withAdminSession(gateAdminID), &svc.ServiceContext{}).
		AdminDeleteDanmaku(&types.ParamAdminDeleteDanmaku{Dmid: 1, OperatorMid: 7})
	if err == nil || !strings.Contains(err.Error(), "danmaku service not configured") {
		t.Fatalf("未配置 danmaku 时应失败, got %v", err)
	}
}
