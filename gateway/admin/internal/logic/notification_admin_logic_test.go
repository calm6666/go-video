package logic

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	notificationrpc "go-video/services/notification/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 notification 的口径：operator 字符串怎么从 AdminOpContext 派生、
// JSON int32 与 proto 枚举如何互转、RPC 消息如何投影成后台 types、错误是否原样上抛。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。
// 频控/免打扰/渲染/重投的业务判定属于 notification 服务，本文件不重复验证。

// notifyFake 是 notification RPC 的假客户端，只记录被调用的入参并返回预置响应。
type notifyFake struct {
	notificationrpc.NotificationClient

	err error

	upsertReq       *notificationrpc.UpsertTemplateReq
	upsertReply     *notificationrpc.UpsertTemplateReply
	listTplReq      *notificationrpc.ListTemplatesReq
	listTplReply    *notificationrpc.ListTemplatesReply
	publishReq      *notificationrpc.PublishTemplateReq
	publishReply    *notificationrpc.PublishTemplateReply
	renderReq       *notificationrpc.RenderTemplateReq
	renderReply     *notificationrpc.RenderTemplateReply
	deliveryReq     *notificationrpc.GetDeliveryStatusReq
	deliveryReply   *notificationrpc.GetDeliveryStatusReply
	listDeliverReq  *notificationrpc.ListDeliveriesReq
	listDeliverResp *notificationrpc.ListDeliveriesReply
	listDLReq       *notificationrpc.ListDeadLettersReq
	listDLReply     *notificationrpc.ListDeadLettersReply
	retryReq        *notificationrpc.RetryDeadLetterReq
	retryReply      *notificationrpc.RetryDeadLetterReply
	calls           int
}

func (f *notifyFake) UpsertTemplate(_ context.Context, in *notificationrpc.UpsertTemplateReq,
	_ ...grpc.CallOption) (*notificationrpc.UpsertTemplateReply, error) {
	f.calls++
	f.upsertReq = in
	return f.upsertReply, f.err
}

func (f *notifyFake) ListTemplates(_ context.Context, in *notificationrpc.ListTemplatesReq,
	_ ...grpc.CallOption) (*notificationrpc.ListTemplatesReply, error) {
	f.calls++
	f.listTplReq = in
	return f.listTplReply, f.err
}

func (f *notifyFake) PublishTemplate(_ context.Context, in *notificationrpc.PublishTemplateReq,
	_ ...grpc.CallOption) (*notificationrpc.PublishTemplateReply, error) {
	f.calls++
	f.publishReq = in
	return f.publishReply, f.err
}

func (f *notifyFake) RenderTemplate(_ context.Context, in *notificationrpc.RenderTemplateReq,
	_ ...grpc.CallOption) (*notificationrpc.RenderTemplateReply, error) {
	f.calls++
	f.renderReq = in
	return f.renderReply, f.err
}

func (f *notifyFake) GetDeliveryStatus(_ context.Context, in *notificationrpc.GetDeliveryStatusReq,
	_ ...grpc.CallOption) (*notificationrpc.GetDeliveryStatusReply, error) {
	f.calls++
	f.deliveryReq = in
	return f.deliveryReply, f.err
}

func (f *notifyFake) ListDeliveries(_ context.Context, in *notificationrpc.ListDeliveriesReq,
	_ ...grpc.CallOption) (*notificationrpc.ListDeliveriesReply, error) {
	f.calls++
	f.listDeliverReq = in
	return f.listDeliverResp, f.err
}

func (f *notifyFake) ListDeadLetters(_ context.Context, in *notificationrpc.ListDeadLettersReq,
	_ ...grpc.CallOption) (*notificationrpc.ListDeadLettersReply, error) {
	f.calls++
	f.listDLReq = in
	return f.listDLReply, f.err
}

func (f *notifyFake) RetryDeadLetter(_ context.Context, in *notificationrpc.RetryDeadLetterReq,
	_ ...grpc.CallOption) (*notificationrpc.RetryDeadLetterReply, error) {
	f.calls++
	f.retryReq = in
	return f.retryReply, f.err
}

// notifyTemplate 是一个字段齐备的模板投影样本，枚举取值故意互不相同，
// 这样一旦投影时串了字段（例如把 channel 当成 state）就会失败。
func notifyTemplate() *notificationrpc.TemplateInfo {
	return &notificationrpc.TemplateInfo{
		Id:           12,
		TemplateCode: "audit_result",
		Channel:      notificationrpc.Channel_CHANNEL_PUSH,
		Language:     notificationrpc.Language_LANGUAGE_EN,
		TitleTpl:     "审核结果",
		BodyTpl:      "稿件 ${title} 已${verdict}",
		Version:      3,
		State:        notificationrpc.TemplateState_TEMPLATE_STATE_DRAFT,
		Operator:     "ops-a",
		Ctime:        100,
		Mtime:        200,
	}
}

func notifyDelivery() *notificationrpc.DeliveryInfo {
	return &notificationrpc.DeliveryInfo{
		DeliveryId:      "dlv-1",
		BizKey:          "submission:99:approved",
		Mid:             42,
		Channel:         notificationrpc.Channel_CHANNEL_SMS,
		TemplateCode:    "audit_result",
		TemplateVersion: 3,
		TargetRef:       "target:hash-1",
		PayloadDigest:   "sha256:abc",
		State:           notificationrpc.DeliveryState_DELIVERY_DEAD_LETTER,
		Provider:        "aliyun-sms",
		ProviderMsgId:   "pm-1",
		RetryCount:      5,
		NextRetryAt:     0,
		LastError:       "provider timeout",
		SentAt:          0,
		ExpireAt:        900,
		Priority:        int32(notificationrpc.Priority_PRIORITY_HIGH),
		TraceId:         "t-1",
		Ctime:           300,
		Mtime:           400,
	}
}

func TestNotifyOperatorDerivation(t *testing.T) {
	// 会话里的 admin_id 才是主体：notification 只有 operator 一列，写进这一列的名字
	// 必须能反查到一个已鉴权的后台账号，而不是调用方自报的 operator_id。
	cases := []struct {
		name    string
		session int64
		op      types.AdminOpContext
		want    string
	}{
		{"有用户名直接用", 7, types.AdminOpContext{OperatorId: 7, OperatorName: "ops-a"}, "ops-a"},
		{"用户名只有空白则回落", 7, types.AdminOpContext{OperatorId: 7, OperatorName: "   "}, "admin:7"},
		{"无用户名回落稳定串", 7, types.AdminOpContext{OperatorId: 7}, "admin:7"},
		// 旧口径在这里报「operator_id required」；现在会话已经证明了是谁在操作，
		// 缺位由会话补齐，比原来更严而不是更松（没会话时见下面的用例）。
		{"客户端没填 operator_id 由会话补齐", 7, types.AdminOpContext{}, "admin:7"},
		{"客户端声明与会话不一致时以会话为准", 7, types.AdminOpContext{OperatorId: 999}, "admin:7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: c.session})
			got, err := notificationOperator(ctx, "notifyTest", c.op)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("notificationOperator(%+v) = %q, want %q", c.op, got, c.want)
			}
		})
	}
	// 没有会话（或会话里的 admin_id 非法）一律拒绝：放过去就会把一次处置记成
	// 任意自报主体，甚至记成无主操作，而台账上看不出区别。
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"中间件没挂身份", context.Background()},
		{"身份非法（admin_id=0）", middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 0})},
		{"身份非法（admin_id<0）", middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: -3})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := notificationOperator(tc.ctx, "notifyTest",
				types.AdminOpContext{OperatorId: 7, OperatorName: "ops-a"}); err == nil {
				t.Fatal("无会话却推导出了操作者")
			} else if !strings.Contains(err.Error(), "admin session required") {
				t.Fatalf("错误应为「需要后台会话」，got %v", err)
			}
		})
	}
}

func TestNotifyNormalizePage(t *testing.T) {
	cases := []struct {
		name           string
		pn, ps         int32
		wantPn, wantPs int32
	}{
		{"缺省回落 1/20", 0, 0, 1, notificationDefaultPageSize},
		{"负值回落 1/20", -5, -1, 1, notificationDefaultPageSize},
		{"页大小超上限截到 100", 2, 5000, 2, notificationMaxPageSize},
		{"上限本身保留", 3, notificationMaxPageSize, 3, notificationMaxPageSize},
		{"区间内原样保留", 4, 30, 4, 30},
		{"只缺页码", 0, 50, 1, 50},
		{"只缺页大小", 6, 0, 6, notificationDefaultPageSize},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pn, ps := normalizeNotificationPage(c.pn, c.ps)
			if pn != c.wantPn || ps != c.wantPs {
				t.Fatalf("normalizeNotificationPage(%d,%d) = (%d,%d), want (%d,%d)",
					c.pn, c.ps, pn, ps, c.wantPn, c.wantPs)
			}
		})
	}
}

func TestNotifyProjectionsLowerEnumsToInt32(t *testing.T) {
	tpl := notifyTemplateToAPI(notifyTemplate())
	// 枚举降回 JSON int32：通道/语言/状态三者的编号不同，投影串了就必错。
	if tpl.Channel != int32(notificationrpc.Channel_CHANNEL_PUSH) ||
		tpl.Language != int32(notificationrpc.Language_LANGUAGE_EN) ||
		tpl.State != int32(notificationrpc.TemplateState_TEMPLATE_STATE_DRAFT) {
		t.Fatalf("notifyTemplateToAPI 枚举降维错误: %+v", tpl)
	}
	if tpl.Id != 12 || tpl.Version != 3 || tpl.Operator != "ops-a" || tpl.Ctime != 100 || tpl.Mtime != 200 ||
		tpl.BodyTpl != notifyTemplate().BodyTpl {
		t.Fatalf("notifyTemplateToAPI = %+v", tpl)
	}

	dlv := notifyDeliveryToAPI(notifyDelivery())
	if dlv.State != int32(notificationrpc.DeliveryState_DELIVERY_DEAD_LETTER) ||
		dlv.Channel != int32(notificationrpc.Channel_CHANNEL_SMS) {
		t.Fatalf("notifyDeliveryToAPI 枚举降维错误: %+v", dlv)
	}
	// 回执与退避字段必须整条搬齐，否则运营看不出任务停在死信还是仍在重试。
	if dlv.DeliveryId != "dlv-1" || dlv.TemplateVer != 3 || dlv.ProviderMsgId != "pm-1" ||
		dlv.RetryCount != 5 || dlv.NextRetryAt != 0 || dlv.LastError != "provider timeout" ||
		dlv.Priority != int32(notificationrpc.Priority_PRIORITY_HIGH) || dlv.TraceId != "t-1" {
		t.Fatalf("notifyDeliveryToAPI = %+v", dlv)
	}

	dl := notifyDeadLetterToAPI(&notificationrpc.DeadLetterInfo{
		Id: 8, EventId: "evt-1", EventType: "video.approved", Topic: "video-events",
		PayloadDigest: "sha256:def", Reason: "render missing var",
		State: notificationrpc.DeadLetterState_DEAD_LETTER_PENDING, Operator: "admin:7", Ctime: 500,
	})
	if dl.State != int32(notificationrpc.DeadLetterState_DEAD_LETTER_PENDING) || dl.Id != 8 ||
		dl.EventId != "evt-1" || dl.PayloadDigest != "sha256:def" || dl.Operator != "admin:7" {
		t.Fatalf("notifyDeadLetterToAPI = %+v", dl)
	}

	// nil 入参（RPC 未填该消息）投影成零值条目而不是 panic：「没数据」不该被网关当错误。
	if got := notifyTemplateToAPI(nil); got.Id != 0 || got.State != 0 {
		t.Fatalf("notifyTemplateToAPI(nil) = %+v", got)
	}
	if got := notifyDeliveryToAPI(nil); got.DeliveryId != "" || got.State != 0 {
		t.Fatalf("notifyDeliveryToAPI(nil) = %+v", got)
	}
	if got := notifyDeadLetterToAPI(nil); got.Id != 0 {
		t.Fatalf("notifyDeadLetterToAPI(nil) = %+v", got)
	}
	// missing_vars 的 nil 归一：后台需要 [] 而不是 null。
	if mv := notifyMissingVarsToAPI(nil); mv == nil || len(mv) != 0 {
		t.Fatalf("notifyMissingVarsToAPI(nil) = %#v, want 空切片", mv)
	}
	if mv := notifyMissingVarsToAPI([]string{"verdict"}); len(mv) != 1 || mv[0] != "verdict" {
		t.Fatalf("notifyMissingVarsToAPI = %#v", mv)
	}
}

func TestNotifyListProjectionsNeverNil(t *testing.T) {
	cases := map[string]func() reflect.Value{
		"notifyTemplatesToAPI":   func() reflect.Value { return reflect.ValueOf(notifyTemplatesToAPI(nil)) },
		"notifyDeliveriesToAPI":  func() reflect.Value { return reflect.ValueOf(notifyDeliveriesToAPI(nil)) },
		"notifyDeadLettersToAPI": func() reflect.Value { return reflect.ValueOf(notifyDeadLettersToAPI(nil)) },
	}
	for name, fn := range cases {
		v := fn()
		if v.Kind() != reflect.Slice {
			t.Fatalf("%s 返回类型异常: %s", name, v.Kind())
		}
		if v.IsNil() || v.Len() != 0 {
			t.Fatalf("%s(nil) = %#v, want 空切片", name, v.Interface())
		}
	}
	// 列表里的 nil 元素（proto 未填）也必须投影成零值，不能 panic。
	if got := notifyTemplatesToAPI([]*notificationrpc.TemplateInfo{nil, notifyTemplate()}); len(got) != 2 ||
		got[0].Id != 0 || got[1].TemplateCode != "audit_result" {
		t.Fatalf("notifyTemplatesToAPI with nil element = %+v", got)
	}
	if got := notifyDeliveriesToAPI([]*notificationrpc.DeliveryInfo{nil}); len(got) != 1 || got[0].State != 0 {
		t.Fatalf("notifyDeliveriesToAPI with nil element = %+v", got)
	}
	if got := notifyDeadLettersToAPI([]*notificationrpc.DeadLetterInfo{nil}); len(got) != 1 || got[0].Id != 0 {
		t.Fatalf("notifyDeadLettersToAPI with nil element = %+v", got)
	}
}

func TestNotifyUpsertTemplateLogic(t *testing.T) {
	fake := &notifyFake{}
	l := NewUpsertNotifyTemplateLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Notification: fake})

	// 无会话的同一入参必须先拒，不能让下游留下无主版本；
	// 客户端漏填 op.operator_id 不再是拒绝理由，因为会话已经证明了是谁在操作。
	noSession := &notifyFake{}
	if _, err := NewUpsertNotifyTemplateLogic(context.Background(), &svc.ServiceContext{Notification: noSession}).
		UpsertNotifyTemplate(&types.ParamUpsertNotifyTemplate{
			TemplateCode: "audit_result", Channel: 1, Language: 3,
		}); err == nil || !strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话却放行: %v", err)
	}
	if noSession.calls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", noSession.calls)
	}
	if _, err := l.UpsertNotifyTemplate(&types.ParamUpsertNotifyTemplate{
		Op: types.AdminOpContext{OperatorId: 7}, Channel: 1, Language: 3,
	}); err == nil {
		t.Fatal("缺 template_code 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	fake.upsertReply = &notificationrpc.UpsertTemplateReply{Template: notifyTemplate()}
	resp, err := l.UpsertNotifyTemplate(&types.ParamUpsertNotifyTemplate{
		Op:           types.AdminOpContext{OperatorId: 7, OperatorName: "ops-a"},
		TemplateCode: "audit_result",
		Channel:      int32(notificationrpc.Channel_CHANNEL_PUSH),
		Language:     int32(notificationrpc.Language_LANGUAGE_EN),
		TitleTpl:     "审核结果",
		BodyTpl:      "稿件 ${title} 已${verdict}",
		Publish:      true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 枚举升维：JSON int32 必须落到 proto 枚举类型上，否则服务侧会判为非法通道。
	if fake.upsertReq.GetChannel() != notificationrpc.Channel_CHANNEL_PUSH ||
		fake.upsertReq.GetLanguage() != notificationrpc.Language_LANGUAGE_EN {
		t.Fatalf("枚举入参转换错误: %+v", fake.upsertReq)
	}
	if fake.upsertReq.GetOperator() != "ops-a" || !fake.upsertReq.GetPublish() ||
		fake.upsertReq.GetTemplateCode() != "audit_result" {
		t.Fatalf("入参未正确装配: %+v", fake.upsertReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	// 版本号与状态必须来自服务端回显（草稿/发布语义由服务侧决定）。
	if resp.Data.Template.Version != 3 || resp.Data.Template.State != int32(notificationrpc.TemplateState_TEMPLATE_STATE_DRAFT) {
		t.Fatalf("data = %+v", resp.Data)
	}

	// 无用户名时下游拿到稳定回落串，且串里的主体号是会话 admin_id，不是自报的 7。
	if _, err := l.UpsertNotifyTemplate(&types.ParamUpsertNotifyTemplate{
		Op: types.AdminOpContext{OperatorId: 7}, TemplateCode: "audit_result",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.upsertReq.GetOperator() != adminOperatorString(gateAdminID) {
		t.Fatalf("operator = %q, want %q", fake.upsertReq.GetOperator(), adminOperatorString(gateAdminID))
	}

	sentinel := errors.New("notification: invalid language")
	fake.err = sentinel
	if _, err := l.UpsertNotifyTemplate(&types.ParamUpsertNotifyTemplate{
		Op: types.AdminOpContext{OperatorId: 7, OperatorName: "ops-a"}, TemplateCode: "audit_result",
	}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}
}

func TestNotifyPublishTemplateLogic(t *testing.T) {
	fake := &notifyFake{}
	l := NewPublishNotifyTemplateLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Notification: fake})

	// 无会话先拒：发布是写操作，notification 只在 operator 列留痕，不能写成无名发布。
	// 「客户端没填 op.operator_id」已不是拒绝理由——会话本身就是主体。
	noSession := &notifyFake{}
	if _, err := NewPublishNotifyTemplateLogic(context.Background(), &svc.ServiceContext{Notification: noSession}).
		PublishNotifyTemplate(&types.ParamPublishNotifyTemplate{
			TemplateCode: "audit_result", Version: 3,
		}); err == nil || !strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话却放行: %v", err)
	}
	if noSession.calls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", noSession.calls)
	}
	// operator_id 合法但 version<=0 时必须拒：否则服务侧会去查「版本 0」这种不存在的草稿。
	if _, err := l.PublishNotifyTemplate(&types.ParamPublishNotifyTemplate{
		Op: types.AdminOpContext{OperatorId: 7}, TemplateCode: "audit_result",
	}); err == nil {
		t.Fatal("version<=0 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}
	// 补齐主体与版本号后走成功路径。
	fake.publishReply = &notificationrpc.PublishTemplateReply{Template: notifyTemplate()}
	resp, err := l.PublishNotifyTemplate(&types.ParamPublishNotifyTemplate{
		Op:           types.AdminOpContext{OperatorId: 7, OperatorName: "ops-a"},
		TemplateCode: "audit_result",
		Channel:      int32(notificationrpc.Channel_CHANNEL_EMAIL),
		Language:     int32(notificationrpc.Language_LANGUAGE_ZH_TW),
		Version:      3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.publishReq.GetChannel() != notificationrpc.Channel_CHANNEL_EMAIL ||
		fake.publishReq.GetLanguage() != notificationrpc.Language_LANGUAGE_ZH_TW ||
		fake.publishReq.GetVersion() != 3 || fake.publishReq.GetOperator() != "ops-a" {
		t.Fatalf("发布入参错误: %+v", fake.publishReq)
	}
	if resp.Data.Template.State != int32(notificationrpc.TemplateState_TEMPLATE_STATE_DRAFT) || resp.TTL != 0 {
		t.Fatalf("data = %+v", resp.Data)
	}
}

func TestNotifyRenderTemplateLogicMarksRejected(t *testing.T) {
	fake := &notifyFake{}
	l := NewRenderNotifyTemplateLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Notification: fake})

	// 无会话先拒，且不能把 params 传下去。
	noSession := &notifyFake{}
	if _, err := NewRenderNotifyTemplateLogic(context.Background(), &svc.ServiceContext{Notification: noSession}).
		RenderNotifyTemplate(&types.ParamRenderNotifyTemplate{
			TemplateCode: "audit_result", Params: map[string]string{"mid": "1"},
		}); err == nil || !strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话却放行: %v", err)
	}
	if noSession.calls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", noSession.calls)
	}

	fake.renderReply = &notificationrpc.RenderTemplateReply{
		Title: "审核结果", Body: "稿件 A 已通过", Version: 4,
		Language: notificationrpc.Language_LANGUAGE_ZH_CN,
	}
	resp, err := l.RenderNotifyTemplate(&types.ParamRenderNotifyTemplate{
		Op:           types.AdminOpContext{OperatorId: 7},
		Channel:      int32(notificationrpc.Channel_CHANNEL_PUSH),
		TemplateCode: "audit_result",
		Language:     int32(notificationrpc.Language_LANGUAGE_ZH_CN),
		Params:       map[string]string{"title": "A", "verdict": "通过"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// params 映射到 TemplateParams；version=0 表示「预览当前已发布版本」，网关不改写。
	if len(fake.renderReq.GetTemplateParams()) != 2 || fake.renderReq.GetTemplateParams()["verdict"] != "通过" ||
		fake.renderReq.GetVersion() != 0 ||
		fake.renderReq.GetChannel() != notificationrpc.Channel_CHANNEL_PUSH ||
		fake.renderReq.GetLanguage() != notificationrpc.Language_LANGUAGE_ZH_CN ||
		fake.renderReq.GetOperator() != adminOperatorString(gateAdminID) {
		t.Fatalf("渲染入参错误: %+v", fake.renderReq)
	}
	if resp.Data.Rejected || len(resp.Data.MissingVars) != 0 || resp.Data.Version != 4 ||
		resp.Data.Language != int32(notificationrpc.Language_LANGUAGE_ZH_CN) {
		t.Fatalf("data = %+v, want 通过且 missing_vars 为空数组", resp.Data)
	}

	// 缺变量：RPC 仍成功返回，网关只标 rejected=true，不转成 HTTP 错误（后台要看缺失清单）。
	fake.renderReply.MissingVars = []string{"verdict"}
	again, err := l.RenderNotifyTemplate(&types.ParamRenderNotifyTemplate{
		Op: types.AdminOpContext{OperatorId: 7}, TemplateCode: "audit_result",
		Channel: int32(notificationrpc.Channel_CHANNEL_PUSH),
	})
	if err != nil {
		t.Fatalf("缺变量不该变成 RPC/HTTP 错误: %v", err)
	}
	if !again.Data.Rejected || len(again.Data.MissingVars) != 1 || again.Data.MissingVars[0] != "verdict" {
		t.Fatalf("data = %+v, want rejected=true 且带缺失清单", again.Data)
	}

	sentinel := errors.New("notification: template not found")
	fake.err = sentinel
	if _, err := l.RenderNotifyTemplate(&types.ParamRenderNotifyTemplate{
		Op: types.AdminOpContext{OperatorId: 7}, TemplateCode: "nope",
	}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}
}

func TestNotifyGetDeliveryStatusLogicPassesFoundThrough(t *testing.T) {
	fake := &notifyFake{}
	l := NewGetNotifyDeliveryStatusLogic(context.Background(), &svc.ServiceContext{Notification: fake})
	if _, err := l.GetNotifyDeliveryStatus(&types.ParamNotifyDeliveryStatus{}); err == nil {
		t.Fatal("空 delivery_id 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	// found=true：整条投递记录（含供应商回执）投影回后台。
	fake.deliveryReply = &notificationrpc.GetDeliveryStatusReply{Delivery: notifyDelivery(), Found: true}
	resp, err := l.GetNotifyDeliveryStatus(&types.ParamNotifyDeliveryStatus{DeliveryId: "dlv-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.deliveryReq.GetDeliveryId() != "dlv-1" {
		t.Fatalf("delivery_id 未透传: %+v", fake.deliveryReq)
	}
	if !resp.Data.Found || resp.Data.Delivery.DeliveryId != "dlv-1" ||
		resp.Data.Delivery.State != int32(notificationrpc.DeliveryState_DELIVERY_DEAD_LETTER) {
		t.Fatalf("data = %+v", resp.Data)
	}

	// found=false 且 delivery 为 nil：这是合法结果，必须原样回 found=false，不报错也不伪造记录。
	fake.deliveryReply = &notificationrpc.GetDeliveryStatusReply{Found: false}
	miss, err := l.GetNotifyDeliveryStatus(&types.ParamNotifyDeliveryStatus{DeliveryId: "dlv-404"})
	if err != nil {
		t.Fatalf("未找到不是错误: %v", err)
	}
	if miss.Data.Found || miss.Data.Delivery.DeliveryId != "" || miss.Data.Delivery.State != 0 {
		t.Fatalf("data = %+v, want found=false + 零值 delivery", miss.Data)
	}

	sentinel := errors.New("notification: db down")
	fake.err = sentinel
	if _, err := l.GetNotifyDeliveryStatus(&types.ParamNotifyDeliveryStatus{DeliveryId: "dlv-1"}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}
}

func TestNotifyListTemplatesLogicClampsPageAndProjectsTotal(t *testing.T) {
	fake := &notifyFake{listTplReply: &notificationrpc.ListTemplatesReply{
		Templates: []*notificationrpc.TemplateInfo{notifyTemplate()},
		Total:     42,
	}}
	l := NewListNotifyTemplatesLogic(context.Background(), &svc.ServiceContext{Notification: fake})
	resp, err := l.ListNotifyTemplates(&types.ParamListNotifyTemplates{
		TemplateCode: "audit_result",
		Channel:      int32(notificationrpc.Channel_CHANNEL_PUSH),
		Language:     int32(notificationrpc.Language_LANGUAGE_EN),
		State:        int32(notificationrpc.TemplateState_TEMPLATE_STATE_PUBLISHED),
		Pn:           0, Ps: 5000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.listTplReq.GetPn() != 1 || fake.listTplReq.GetPs() != notificationMaxPageSize {
		t.Fatalf("pn/ps = %d/%d, want 1/%d（网关不得替运营放大页大小）",
			fake.listTplReq.GetPn(), fake.listTplReq.GetPs(), notificationMaxPageSize)
	}
	if fake.listTplReq.GetChannel() != notificationrpc.Channel_CHANNEL_PUSH ||
		fake.listTplReq.GetLanguage() != notificationrpc.Language_LANGUAGE_EN ||
		fake.listTplReq.GetState() != notificationrpc.TemplateState_TEMPLATE_STATE_PUBLISHED ||
		fake.listTplReq.GetTemplateCode() != "audit_result" {
		t.Fatalf("过滤条件未正确升维: %+v", fake.listTplReq)
	}
	if resp.Data.Total != 42 || len(resp.Data.List) != 1 || resp.Data.List[0].Version != 3 {
		t.Fatalf("data = %+v, want total 来自服务端", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}

	// 零值过滤条件在 RPC 侧语义是「不过滤」，网关不得替它填默认值。
	zero := &notifyFake{listTplReply: &notificationrpc.ListTemplatesReply{}}
	if _, err := NewListNotifyTemplatesLogic(context.Background(), &svc.ServiceContext{Notification: zero}).
		ListNotifyTemplates(&types.ParamListNotifyTemplates{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zero.listTplReq.GetChannel() != notificationrpc.Channel_CHANNEL_UNSPECIFIED ||
		zero.listTplReq.GetState() != notificationrpc.TemplateState_TEMPLATE_STATE_UNSPECIFIED {
		t.Fatalf("0 值过滤条件被改写: %+v", zero.listTplReq)
	}
	if zero.listTplReq.GetPn() != 1 || zero.listTplReq.GetPs() != notificationDefaultPageSize {
		t.Fatalf("pn/ps = %d/%d, want 1/%d", zero.listTplReq.GetPn(), zero.listTplReq.GetPs(), notificationDefaultPageSize)
	}
	// 空列表投影成 []，而不是 null：后台表格按数组渲染。
	if r, err := NewListNotifyTemplatesLogic(context.Background(), &svc.ServiceContext{Notification: zero}).
		ListNotifyTemplates(&types.ParamListNotifyTemplates{}); err != nil || r.Data.List == nil || r.Data.Total != 0 {
		t.Fatalf("resp = %+v err = %v, want 空切片 + total 0", r, err)
	}
}

func TestNotifyListDeliveriesLogic(t *testing.T) {
	fake := &notifyFake{listDeliverResp: &notificationrpc.ListDeliveriesReply{
		Deliveries: []*notificationrpc.DeliveryInfo{notifyDelivery()},
		Total:      7,
	}}
	l := NewListNotifyDeliveriesLogic(context.Background(), &svc.ServiceContext{Notification: fake})

	// 时间窗倒置会返回空结果，运营会误判「没有投递」，先挡一次给出可读消息。
	if _, err := l.ListNotifyDeliveries(&types.ParamListNotifyDeliveries{StartCtime: 200, EndCtime: 100}); err == nil {
		t.Fatal("start_ctime>=end_ctime 应被拒绝")
	}
	if _, err := l.ListNotifyDeliveries(&types.ParamListNotifyDeliveries{StartCtime: -1}); err == nil {
		t.Fatal("负的 start_ctime 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	resp, err := l.ListNotifyDeliveries(&types.ParamListNotifyDeliveries{
		Mid: 42, Channel: int32(notificationrpc.Channel_CHANNEL_SMS),
		State: int32(notificationrpc.DeliveryState_DELIVERY_SUPPRESSED), BizKey: "bk-1",
		StartCtime: 100, EndCtime: 200, Pn: 2, Ps: 10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.listDeliverReq.GetMid() != 42 || fake.listDeliverReq.GetBizKey() != "bk-1" ||
		fake.listDeliverReq.GetChannel() != notificationrpc.Channel_CHANNEL_SMS ||
		fake.listDeliverReq.GetState() != notificationrpc.DeliveryState_DELIVERY_SUPPRESSED ||
		fake.listDeliverReq.GetStartCtime() != 100 || fake.listDeliverReq.GetEndCtime() != 200 ||
		fake.listDeliverReq.GetPn() != 2 || fake.listDeliverReq.GetPs() != 10 {
		t.Fatalf("查询条件未透传: %+v", fake.listDeliverReq)
	}
	if resp.Data.Total != 7 || len(resp.Data.List) != 1 ||
		resp.Data.List[0].State != int32(notificationrpc.DeliveryState_DELIVERY_DEAD_LETTER) {
		t.Fatalf("data = %+v", resp.Data)
	}

	sentinel := errors.New("notification: deliveries list failed")
	fake.err = sentinel
	if _, err := l.ListNotifyDeliveries(&types.ParamListNotifyDeliveries{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛", err)
	}
}

func TestNotifyListDeadLettersLogic(t *testing.T) {
	fake := &notifyFake{listDLReply: &notificationrpc.ListDeadLettersReply{
		DeadLetters: []*notificationrpc.DeadLetterInfo{{
			Id: 8, EventId: "evt-1", Topic: "video-events",
			State: notificationrpc.DeadLetterState_DEAD_LETTER_RETRIED,
		}},
		Total: 1,
	}}
	l := NewListNotifyDeadLettersLogic(context.Background(), &svc.ServiceContext{Notification: fake})
	resp, err := l.ListNotifyDeadLetters(&types.ParamListNotifyDeadLetters{
		EventId: "evt-1", Topic: "video-events",
		State: int32(notificationrpc.DeadLetterState_DEAD_LETTER_PENDING), Pn: -1, Ps: 999,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.listDLReq.GetState() != notificationrpc.DeadLetterState_DEAD_LETTER_PENDING ||
		fake.listDLReq.GetEventId() != "evt-1" || fake.listDLReq.GetTopic() != "video-events" {
		t.Fatalf("死信过滤条件未透传: %+v", fake.listDLReq)
	}
	if fake.listDLReq.GetPn() != 1 || fake.listDLReq.GetPs() != notificationMaxPageSize {
		t.Fatalf("pn/ps = %d/%d, want 1/%d",
			fake.listDLReq.GetPn(), fake.listDLReq.GetPs(), notificationMaxPageSize)
	}
	if resp.Data.Total != 1 || len(resp.Data.List) != 1 ||
		resp.Data.List[0].State != int32(notificationrpc.DeadLetterState_DEAD_LETTER_RETRIED) {
		t.Fatalf("data = %+v（处置状态必须由服务侧回显）", resp.Data)
	}
}

func TestNotifyRetryDeadLetterLogic(t *testing.T) {
	fake := &notifyFake{}
	l := NewRetryNotifyDeadLetterLogic(withAdminSession(gateAdminID), &svc.ServiceContext{Notification: fake})

	// 无会话必须拒绝：重投必须留审计主体，而主体只能来自会话（客户端自报的 operator_name 不算）。
	noSession := &notifyFake{}
	if _, err := NewRetryNotifyDeadLetterLogic(context.Background(), &svc.ServiceContext{Notification: noSession}).
		RetryNotifyDeadLetter(&types.ParamRetryNotifyDeadLetter{
			Op: types.AdminOpContext{OperatorName: "ops-a"}, Id: 8,
		}); err == nil || !strings.Contains(err.Error(), "admin session required") {
		t.Fatalf("无会话却放行: %v", err)
	}
	if noSession.calls != 0 {
		t.Fatalf("无会话被拒后仍调用下游 %d 次", noSession.calls)
	}
	if _, err := l.RetryNotifyDeadLetter(&types.ParamRetryNotifyDeadLetter{
		Op: types.AdminOpContext{OperatorId: 7},
	}); err == nil {
		t.Fatal("id<=0 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	fake.retryReply = &notificationrpc.RetryDeadLetterReply{
		DeliveryIds: []string{"dlv-1", "dlv-2"}, Retried: 2, Message: "已复位为待投递",
	}
	resp, err := l.RetryNotifyDeadLetter(&types.ParamRetryNotifyDeadLetter{
		Op: types.AdminOpContext{OperatorId: 7, OperatorName: "ops-a"}, Id: 8, Reason: "供应商恢复",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// operator 字符串进下游（notification 只有这一列），reason 原样透传供留痕。
	if fake.retryReq.GetOperator() != "ops-a" || fake.retryReq.GetId() != 8 ||
		fake.retryReq.GetReason() != "供应商恢复" {
		t.Fatalf("重投入参错误: %+v", fake.retryReq)
	}
	if resp.Data.Retried != 2 || len(resp.Data.DeliveryIds) != 2 || resp.Data.Message != "已复位为待投递" {
		t.Fatalf("data = %+v", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}

	sentinel := errors.New("notification: dead letter already handled")
	fake.err = sentinel
	if _, err := l.RetryNotifyDeadLetter(&types.ParamRetryNotifyDeadLetter{
		Op: types.AdminOpContext{OperatorId: 7}, Id: 8,
	}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 重复重投的错误原样上抛", err)
	}
}

func TestNotifyLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	op := types.AdminOpContext{OperatorId: 7, OperatorName: "ops-a"}
	cases := []struct {
		name string
		call func() error
	}{
		{"upsertNotifyTemplate", func() error {
			_, err := NewUpsertNotifyTemplateLogic(withAdminSession(gateAdminID), empty).UpsertNotifyTemplate(
				&types.ParamUpsertNotifyTemplate{Op: op, TemplateCode: "audit_result"})
			return err
		}},
		{"listNotifyTemplates", func() error {
			_, err := NewListNotifyTemplatesLogic(context.Background(), empty).ListNotifyTemplates(
				&types.ParamListNotifyTemplates{})
			return err
		}},
		{"publishNotifyTemplate", func() error {
			_, err := NewPublishNotifyTemplateLogic(withAdminSession(gateAdminID), empty).PublishNotifyTemplate(
				&types.ParamPublishNotifyTemplate{Op: op, TemplateCode: "audit_result", Version: 1})
			return err
		}},
		{"renderNotifyTemplate", func() error {
			_, err := NewRenderNotifyTemplateLogic(withAdminSession(gateAdminID), empty).RenderNotifyTemplate(
				&types.ParamRenderNotifyTemplate{Op: op, TemplateCode: "audit_result"})
			return err
		}},
		{"getNotifyDeliveryStatus", func() error {
			_, err := NewGetNotifyDeliveryStatusLogic(context.Background(), empty).GetNotifyDeliveryStatus(
				&types.ParamNotifyDeliveryStatus{DeliveryId: "dlv-1"})
			return err
		}},
		{"listNotifyDeliveries", func() error {
			_, err := NewListNotifyDeliveriesLogic(context.Background(), empty).ListNotifyDeliveries(
				&types.ParamListNotifyDeliveries{})
			return err
		}},
		{"listNotifyDeadLetters", func() error {
			_, err := NewListNotifyDeadLettersLogic(context.Background(), empty).ListNotifyDeadLetters(
				&types.ParamListNotifyDeadLetters{})
			return err
		}},
		{"retryNotifyDeadLetter", func() error {
			_, err := NewRetryNotifyDeadLetterLogic(withAdminSession(gateAdminID), empty).RetryNotifyDeadLetter(
				&types.ParamRetryNotifyDeadLetter{Op: op, Id: 8})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatalf("%s：未配置 NotificationRPC 时必须报错，不能退化成空响应", c.name)
			}
			if !strings.Contains(err.Error(), "notification service not configured") {
				t.Fatalf("%s：err = %v，want 明确的服务未配置错误", c.name, err)
			}
		})
	}
}
