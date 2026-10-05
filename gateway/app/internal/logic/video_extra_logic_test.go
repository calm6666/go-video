package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	videorpc "go-video/services/video/rpc"

	"google.golang.org/grpc"
)

// 本文件只覆盖网关面向 video 的投稿补充路由（编辑元信息、软删）：
// 请求字段是否原样透传、回复是否投影成信封、下游错误是否上抛。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeVideoExtraClient struct {
	videorpc.VideoClient

	updateReq   *videorpc.UpdateSubmissionReq
	updateReply *videorpc.SubmissionReply
	deleteReq   *videorpc.SubmissionReq
	calls       int
	err         error
}

func (f *fakeVideoExtraClient) UpdateSubmission(_ context.Context, in *videorpc.UpdateSubmissionReq,
	_ ...grpc.CallOption) (*videorpc.SubmissionReply, error) {
	f.calls++
	f.updateReq = in
	return f.updateReply, f.err
}

func (f *fakeVideoExtraClient) DeleteSubmission(_ context.Context, in *videorpc.SubmissionReq,
	_ ...grpc.CallOption) (*videorpc.EmptyReply, error) {
	f.calls++
	f.deleteReq = in
	return &videorpc.EmptyReply{}, f.err
}

func TestVideoUpdateSubmissionForwardsPartialFields(t *testing.T) {
	fake := &fakeVideoExtraClient{updateReply: &videorpc.SubmissionReply{
		Submission: &videorpc.Submission{
			Aid: 101, Mid: 7, Title: "新标题", Desc: "简介", Cover: "https://cdn/cover.jpg",
			Typeid: 3, Tag: "动画,原创", State: videorpc.SubmissionState_STATE_READY_FOR_REVIEW,
			Ctime: 111, Mtime: 222,
		},
	}}
	l := NewUpdateSubmissionLogic(context.Background(), &svc.ServiceContext{Video: fake})
	resp, err := l.UpdateSubmission(&types.ParamUpdateSubmission{
		Aid: 101, Mid: 7, Title: "新标题", Desc: "简介",
		Cover: "https://cdn/cover.jpg", Typeid: 3, Tag: "动画,原创", IP: "10.0.0.7",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 网关不做业务规则：path 参数 aid 与空字段都必须原样下发，由 video 判定所有者与可编辑状态。
	if fake.updateReq.GetAid() != 101 || fake.updateReq.GetMid() != 7 || fake.updateReq.GetIp() != "10.0.0.7" {
		t.Fatalf("aid/mid/ip 未透传: %+v", fake.updateReq)
	}
	if fake.updateReq.GetTitle() != "新标题" || fake.updateReq.GetDesc() != "简介" ||
		fake.updateReq.GetCover() != "https://cdn/cover.jpg" || fake.updateReq.GetTypeid() != 3 ||
		fake.updateReq.GetTag() != "动画,原创" {
		t.Fatalf("可编辑字段未原样透传: %+v", fake.updateReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	got := resp.Data.Submission
	if got.Aid != 101 || got.Mid != 7 || got.Typeid != 3 || got.Tag != "动画,原创" ||
		got.Ctime != 111 || got.Mtime != 222 {
		t.Fatalf("submission = %+v", got)
	}
	// SubmissionState 是 protobuf 枚举，投影后必须是信封里的 int32。
	if got.State != int32(videorpc.SubmissionState_STATE_READY_FOR_REVIEW) {
		t.Fatalf("state = %d, want %d", got.State, videorpc.SubmissionState_STATE_READY_FOR_REVIEW)
	}
}

func TestVideoUpdateSubmissionKeepsEmptyFieldsAsNoop(t *testing.T) {
	fake := &fakeVideoExtraClient{updateReply: &videorpc.SubmissionReply{
		Submission: &videorpc.Submission{Aid: 101, Mid: 7, Title: "原标题", Typeid: 3},
	}}
	l := NewUpdateSubmissionLogic(context.Background(), &svc.ServiceContext{Video: fake})
	if _, err := l.UpdateSubmission(&types.ParamUpdateSubmission{Aid: 101, Mid: 7, IP: "10.0.0.7"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 只改标题以外的字段留空：网关不得把零值替换成服务端回显值，否则会变成误覆盖。
	if fake.updateReq.GetTitle() != "" || fake.updateReq.GetDesc() != "" || fake.updateReq.GetCover() != "" ||
		fake.updateReq.GetTypeid() != 0 || fake.updateReq.GetTag() != "" {
		t.Fatalf("空字段被改写: %+v", fake.updateReq)
	}
}

func TestVideoUpdateSubmissionNilSubmissionAndErrors(t *testing.T) {
	// 下游返回空 submission（异常但非错误）时投影成零值，不能 panic。
	fake := &fakeVideoExtraClient{}
	l := NewUpdateSubmissionLogic(context.Background(), &svc.ServiceContext{Video: fake})
	resp, err := l.UpdateSubmission(&types.ParamUpdateSubmission{Aid: 101, Mid: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Data.Submission != (types.VideoSubmission{}) {
		t.Fatalf("nil submission 应投影成零值, got %+v", resp.Data.Submission)
	}

	sentinel := errors.New("video: only owner can update")
	fake.err = sentinel
	if _, err = l.UpdateSubmission(&types.ParamUpdateSubmission{Aid: 101, Mid: 8}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}

	// 未配置 VideoRPC 时必须报错，不能退化成成功信封。
	if _, err = NewUpdateSubmissionLogic(context.Background(), &svc.ServiceContext{}).
		UpdateSubmission(&types.ParamUpdateSubmission{Aid: 101, Mid: 7}); err == nil {
		t.Fatal("未配置 video 客户端时必须报错")
	}
}

func TestVideoDeleteSubmission(t *testing.T) {
	fake := &fakeVideoExtraClient{}
	l := NewDeleteSubmissionLogic(context.Background(), &svc.ServiceContext{Video: fake})
	resp, err := l.DeleteSubmission(&types.ParamDeleteSubmission{Aid: 101, Mid: 7, IP: "10.0.0.7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.deleteReq.GetAid() != 101 || fake.deleteReq.GetMid() != 7 || fake.deleteReq.GetIp() != "10.0.0.7" {
		t.Fatalf("删除入参未透传: %+v", fake.deleteReq)
	}
	if fake.calls != 1 {
		t.Fatalf("DeleteSubmission 调用 %d 次, want 1（软删不做双写重试）", fake.calls)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}

	sentinel := errors.New("video: illegal state transition")
	fake.err = sentinel
	if _, err = l.DeleteSubmission(&types.ParamDeleteSubmission{Aid: 101, Mid: 7}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 状态机错误原样上抛（软删合法性由 video 判定）", err)
	}

	if _, err = NewDeleteSubmissionLogic(context.Background(), &svc.ServiceContext{}).
		DeleteSubmission(&types.ParamDeleteSubmission{Aid: 101, Mid: 7}); err == nil {
		t.Fatal("未配置 video 客户端时必须报错")
	}
}
