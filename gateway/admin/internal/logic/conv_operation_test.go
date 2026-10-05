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
	operationrpc "go-video/services/operation/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 operation 的口径：OpContext 主体/幂等键怎么装配、分页与步骤规模
// 与服务端如何对齐、RPC 消息如何投影成后台 types。
// 打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

type fakeOperation struct {
	operationrpc.OperationClient

	adminLoginReq   *operationrpc.AdminLoginReq
	adminLoginReply *operationrpc.AdminLoginReply
	submitTaskReq   *operationrpc.SubmitAdminTaskReq
	submitTaskReply *operationrpc.SubmitAdminTaskReply
	listUsersReq    *operationrpc.ListAdminUsersReq
	listUsersReply  *operationrpc.ListAdminUsersReply
	getMenuReq      *operationrpc.GetMenuReq
	getMenuReply    *operationrpc.GetMenuReply
	saveConfigReq   *operationrpc.SaveOpsConfigReq
	saveConfigReply *operationrpc.SaveOpsConfigReply
	runTaskReq      *operationrpc.RunAdminTaskReq
	runTaskReply    *operationrpc.RunAdminTaskReply
	listAuditReq    *operationrpc.ListAuditIndexReq
	listAuditReply  *operationrpc.ListAuditIndexReply
	err             error
	calls           int
}

func (f *fakeOperation) AdminLogin(_ context.Context, in *operationrpc.AdminLoginReq,
	_ ...grpc.CallOption) (*operationrpc.AdminLoginReply, error) {
	f.calls++
	f.adminLoginReq = in
	return f.adminLoginReply, f.err
}

func (f *fakeOperation) SubmitAdminTask(_ context.Context, in *operationrpc.SubmitAdminTaskReq,
	_ ...grpc.CallOption) (*operationrpc.SubmitAdminTaskReply, error) {
	f.calls++
	f.submitTaskReq = in
	return f.submitTaskReply, f.err
}

func (f *fakeOperation) ListAdminUsers(_ context.Context, in *operationrpc.ListAdminUsersReq,
	_ ...grpc.CallOption) (*operationrpc.ListAdminUsersReply, error) {
	f.calls++
	f.listUsersReq = in
	return f.listUsersReply, f.err
}

func (f *fakeOperation) GetMenu(_ context.Context, in *operationrpc.GetMenuReq,
	_ ...grpc.CallOption) (*operationrpc.GetMenuReply, error) {
	f.calls++
	f.getMenuReq = in
	return f.getMenuReply, f.err
}

func (f *fakeOperation) SaveOpsConfig(_ context.Context, in *operationrpc.SaveOpsConfigReq,
	_ ...grpc.CallOption) (*operationrpc.SaveOpsConfigReply, error) {
	f.calls++
	f.saveConfigReq = in
	return f.saveConfigReply, f.err
}

func (f *fakeOperation) RunAdminTask(_ context.Context, in *operationrpc.RunAdminTaskReq,
	_ ...grpc.CallOption) (*operationrpc.RunAdminTaskReply, error) {
	f.calls++
	f.runTaskReq = in
	return f.runTaskReply, f.err
}

func (f *fakeOperation) ListAuditIndex(_ context.Context, in *operationrpc.ListAuditIndexReq,
	_ ...grpc.CallOption) (*operationrpc.ListAuditIndexReply, error) {
	f.calls++
	f.listAuditReq = in
	return f.listAuditReply, f.err
}

// sessionCtx 模拟 AdminPermission 中间件已判定通过的请求上下文。
func sessionCtx(adminID int64, roles ...string) context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: adminID, Roles: roles})
}

func TestNormalizeOperationPage(t *testing.T) {
	cases := []struct {
		name   string
		pn, ps int32
		wantPn int32
		wantPs int32
	}{
		{"缺省回落 1/20", 0, 0, 1, operationDefaultPageSize},
		{"负值回落 1/20", -5, -1, 1, operationDefaultPageSize},
		{"页大小超上限截到 100", 2, 5000, 2, operationMaxPageSize},
		{"上限本身保留", 2, operationMaxPageSize, 2, operationMaxPageSize},
		{"区间内原样保留", 3, 7, 3, 7},
		{"只缺页码", 0, 50, 1, 50},
		{"只缺页大小", 4, 0, 4, operationDefaultPageSize},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pn, ps := normalizeOperationPage(c.pn, c.ps)
			if pn != c.wantPn || ps != c.wantPs {
				t.Fatalf("normalizeOperationPage(%d,%d) = (%d,%d), want (%d,%d)",
					c.pn, c.ps, pn, ps, c.wantPn, c.wantPs)
			}
		})
	}
}

func TestOperationOpContextWithoutSession(t *testing.T) {
	// 公开路由（adminLogin / verifyAdminPermission）之外都挂了中间件；
	// 这里显式用空 context 覆盖「无会话」分支：只能靠客户端声明的 operator_id，且必须 > 0。
	if _, err := operationOpContext(context.Background(), types.AdminOpContext{}, false); err == nil {
		t.Fatal("operator_id<=0 应被拒绝：审计必须有主体")
	}
	op, err := operationOpContext(context.Background(), types.AdminOpContext{
		OperatorId: 9, OperatorName: "ops-a", Ip: "10.0.0.9", UserAgent: "curl", TraceId: "t-1",
	}, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op.GetOperatorId() != 9 || op.GetOperatorName() != "ops-a" || op.GetIp() != "10.0.0.9" ||
		op.GetUserAgent() != "curl" || op.GetTraceId() != "t-1" {
		t.Fatalf("可选字段未原样透传: %+v", op)
	}
	if op.GetRequestId() != "" {
		t.Fatalf("读接口不该造 request_id: %q", op.GetRequestId())
	}
}

func TestOperationOpContextSessionOverridesClaimed(t *testing.T) {
	ctx := sessionCtx(77, "content_ops")
	op, err := operationOpContext(ctx, types.AdminOpContext{OperatorId: 9, RequestId: "req-1"}, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if op.GetOperatorId() != 77 {
		t.Fatalf("operator_id = %d, want 77（会话主体必须覆盖客户端声明，否则可伪造审计归属）", op.GetOperatorId())
	}
	if op.GetRequestId() != "req-1" {
		t.Fatalf("request_id = %q, want req-1", op.GetRequestId())
	}
	// 声明值与会话一致时同样取会话值，行为可预测。
	same, err := operationOpContext(ctx, types.AdminOpContext{OperatorId: 77, RequestId: "req-2"}, true)
	if err != nil || same.GetOperatorId() != 77 {
		t.Fatalf("一致场景 err=%v op=%+v", err, same)
	}
}

func TestOperationOpContextRequestIDOnlyRequiredOnWrites(t *testing.T) {
	// 写接口：request_id 是 operation 的幂等键与审计关联键，缺失必须拒绝。
	if _, err := operationOpContext(sessionCtx(77), types.AdminOpContext{OperatorId: 77}, true); err == nil {
		t.Fatal("写接口缺少 op.request_id 应被拒绝")
	} else if !strings.Contains(err.Error(), "op.request_id") {
		t.Fatalf("错误消息应点名嵌套字段，got %v", err)
	}
	// 纯空白等同缺失（幂等键不能带脏值）。
	if _, err := operationOpContext(sessionCtx(77),
		types.AdminOpContext{RequestId: "   "}, true); err == nil {
		t.Fatal("op.request_id 全空白应被拒绝")
	}
	// 读接口不强制。
	if _, err := operationOpContext(sessionCtx(77), types.AdminOpContext{}, false); err != nil {
		t.Fatalf("读接口不该要求 request_id: %v", err)
	}
}

func TestOperationStepsProjection(t *testing.T) {
	if got := operationSteps(nil); got == nil || len(got) != 0 {
		t.Fatalf("operationSteps(nil) = %v, want 空切片（proto 需要非 nil 才不报 marshal 警告）", got)
	}
	got := operationSteps([]types.OperationTaskStepSpec{
		{TargetType: "submission", TargetId: "sub-1"},
		{TargetType: "episode", TargetId: ""},
	})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].GetTargetType() != "submission" || got[0].GetTargetId() != "sub-1" {
		t.Fatalf("步骤未原样透传: %+v", got[0])
	}
	// target_id 为空是否合法由 operation 判定，网关不做业务规则。
	if got[1].GetTargetId() != "" {
		t.Fatalf("网关不应改写空 target_id: %+v", got[1])
	}
}

func TestOperationProjectionsKeepServerFields(t *testing.T) {
	user := adminUserToAPI(&operationrpc.AdminUserItem{
		AdminId: 7, Username: "ops", State: 1, Remark: "r", OperatorId: 1,
		LastLoginAt: 111, Ctime: 222, Mtime: 333,
		RoleIds: []int64{4, 5}, RoleNames: []string{"content_ops"}, SecondFactorEnabled: true,
	})
	if user.AdminId != 7 || user.State != 1 || !user.SecondFactorEnabled ||
		len(user.RoleIds) != 2 || user.RoleNames[0] != "content_ops" || user.LastLoginAt != 111 {
		t.Fatalf("adminUserToAPI = %+v", user)
	}

	role := roleToAPI(&operationrpc.RoleItem{
		RoleId: 4, Name: "content_ops", Title: "内容运营", State: 1, MemberCount: 3, PermissionIds: []int64{9},
	})
	if role.MemberCount != 3 || role.Name != "content_ops" || len(role.PermissionIds) != 1 {
		t.Fatalf("roleToAPI = %+v（member_count 是删除保护依据，不得丢失）", role)
	}

	// resource/action 可能是通配模式（*、video:*、*:read），必须原样透出。
	perm := permissionToAPI(&operationrpc.PermissionItem{
		PermissionId: 9, Resource: "video:*", Action: "read", Domain: "video", Description: "d",
	})
	if perm.Resource != "video:*" || perm.Action != "read" || perm.Domain != "video" {
		t.Fatalf("permissionToAPI = %+v", perm)
	}

	menu := menuToAPI(&operationrpc.MenuItem{
		MenuId: 1, ParentId: 0, Name: "审核", Path: "/moderation", Icon: "shield",
		Sort: 2, RequiredPermission: "video:submission:read", State: 1,
	})
	if menu.ParentId != 0 || menu.RequiredPermission != "video:submission:read" || menu.Sort != 2 {
		t.Fatalf("menuToAPI = %+v（层级与权限点靠这两个字段表达）", menu)
	}

	cfg := configToAPI(&operationrpc.ConfigItem{
		Id: 3, CfgKey: "ops.audit.threshold", CfgValue: "0.9", ValueType: "string",
		Scope: "global", Version: 7, State: 1, OperatorId: 9, Remark: "rr",
	})
	if cfg.Version != 7 || cfg.CfgValue != "0.9" || cfg.Scope != "global" {
		t.Fatalf("configToAPI = %+v（version 是乐观锁回显依据）", cfg)
	}

	task := taskToAPI(&operationrpc.TaskInfo{
		TaskId: 88, TaskType: "batch_offline_submission", State: "partial", RequestId: "req-1",
		Total: 10, Succeeded: 6, Failed: 4, Progress: 10, OperatorId: 7, TraceId: "t-1",
		StartedAt: 100, FinishedAt: 200,
	})
	if task.Progress != 10 || task.Succeeded != 6 || task.Failed != 4 || task.RequestId != "req-1" ||
		task.StartedAt != 100 || task.FinishedAt != 200 {
		t.Fatalf("taskToAPI = %+v（进度只能来自服务端）", task)
	}

	step := taskStepToAPI(&operationrpc.TaskStepInfo{
		Id: 1, TaskId: 88, StepNo: 3, TargetType: "submission", TargetId: "sub-1",
		State: "failed", Result: "ok", ErrMsg: "rights denied",
	})
	if step.StepNo != 3 || step.ErrMsg != "rights denied" || step.State != "failed" {
		t.Fatalf("taskStepToAPI = %+v", step)
	}

	audit := auditToAPI(&operationrpc.AuditIndexItem{
		Id: 5, AdminId: 7, Username: "ops", Action: "ops_config.save", ResourceType: "ops:config",
		ResourceId: "3", Result: "ok", IpHash: "sha256:abc", TraceId: "t-1", RequestId: "req-1", Ctime: 42,
	})
	if audit.IpHash != "sha256:abc" || audit.RequestId != "req-1" || audit.Ctime != 42 {
		t.Fatalf("auditToAPI = %+v（ip_hash 是不可逆摘要，网关不再加工）", audit)
	}
}

func TestOperationListProjectionsNilInput(t *testing.T) {
	cases := map[string]func() reflect.Value{
		"adminUsersToAPI":  func() reflect.Value { return reflect.ValueOf(adminUsersToAPI(nil)) },
		"rolesToAPI":       func() reflect.Value { return reflect.ValueOf(rolesToAPI(nil)) },
		"permissionsToAPI": func() reflect.Value { return reflect.ValueOf(permissionsToAPI(nil)) },
		"menusToAPI":       func() reflect.Value { return reflect.ValueOf(menusToAPI(nil)) },
		"tasksToAPI":       func() reflect.Value { return reflect.ValueOf(tasksToAPI(nil)) },
		"taskStepsToAPI":   func() reflect.Value { return reflect.ValueOf(taskStepsToAPI(nil)) },
		"auditsToAPI":      func() reflect.Value { return reflect.ValueOf(auditsToAPI(nil)) },
	}
	for name, fn := range cases {
		v := fn()
		if v.Kind() != reflect.Slice {
			t.Fatalf("%s 返回类型异常: %s", name, v.Kind())
		}
		if v.IsNil() {
			t.Fatalf("%s(nil) = nil, want 空切片（后台应拿到 [] 而非 null）", name)
		}
		if v.Len() != 0 {
			t.Fatalf("%s(nil) len = %d, want 0", name, v.Len())
		}
	}
	// 列表里的 nil 元素（proto 未填）也必须投影成零值，不能 panic。
	if users := adminUsersToAPI([]*operationrpc.AdminUserItem{nil}); len(users) != 1 || users[0].AdminId != 0 {
		t.Fatalf("adminUsersToAPI with nil element = %+v", users)
	}
	if items := auditsToAPI([]*operationrpc.AuditIndexItem{nil}); len(items) != 1 || items[0].Id != 0 {
		t.Fatalf("auditsToAPI with nil element = %+v", items)
	}
	if tasks := tasksToAPI([]*operationrpc.TaskInfo{nil}); len(tasks) != 1 || tasks[0].TaskId != 0 {
		t.Fatalf("tasksToAPI with nil element = %+v", tasks)
	}
}

func TestOperationTTLNormalization(t *testing.T) {
	if got := int64TTL(-5); got != 0 {
		t.Fatalf("int64TTL(-5) = %d, want 0（负 ttl 会让前端反复回源）", got)
	}
	if got := int64TTL(0); got != 0 {
		t.Fatalf("int64TTL(0) = %d, want 0", got)
	}
	if got := int64TTL(300); got != 300 {
		t.Fatalf("int64TTL(300) = %d, want 300", got)
	}
	// GetMenuReply.ttl 本身是 int64，走 normalizeTTL 分支。
	if got := normalizeTTL(int64(1) << 40); got != int64(1)<<40 {
		t.Fatalf("normalizeTTL 大数被截断: %d", got)
	}
	if got := normalizeTTL(-1); got != 0 {
		t.Fatalf("normalizeTTL(-1) = %d, want 0", got)
	}
}

func TestAdminLoginLogicValidation(t *testing.T) {
	fake := &fakeOperation{}
	l := NewAdminLoginLogic(context.Background(), &svc.ServiceContext{Operation: fake})
	cases := []struct {
		name string
		req  *types.ParamAdminLogin
	}{
		{"缺用户名", &types.ParamAdminLogin{Password: "p", RequestId: "r"}},
		{"缺口令", &types.ParamAdminLogin{Username: "ops", RequestId: "r"}},
		{"缺幂等键", &types.ParamAdminLogin{Username: "ops", Password: "p"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := l.AdminLogin(c.req); err == nil {
				t.Fatal("应返回校验错误")
			}
		})
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	// 成功路径：token/角色/过期时间原样映射，ttl 归一。
	fake.adminLoginReply = &operationrpc.AdminLoginReply{
		Token: "adm_op_1a2b", AdminId: 7, Username: "ops", ExpiresAt: 1800,
		Roles: []string{"super_admin"}, Ttl: 0,
	}
	resp, err := l.AdminLogin(&types.ParamAdminLogin{Username: "ops", Password: "p", SecondFactor: "123456", RequestId: "req-9"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data.Token != "adm_op_1a2b" || resp.Data.AdminId != 7 || len(resp.Data.Roles) != 1 ||
		resp.Data.ExpiresAt != 1800 {
		t.Fatalf("data = %+v", resp.Data)
	}
	if fake.adminLoginReq.GetSecondFactor() != "123456" || fake.adminLoginReq.GetRequestId() != "req-9" {
		t.Fatalf("登录入参未透传: %+v", fake.adminLoginReq)
	}
}

func TestSubmitAdminTaskLogicGuards(t *testing.T) {
	ctx := sessionCtx(77)

	// 缺 op.request_id：批量任务会产生第二个不可关联的任务，必须在网关挡下。
	fake := &fakeOperation{}
	l := NewSubmitAdminTaskLogic(ctx, &svc.ServiceContext{Operation: fake})
	if _, err := l.SubmitAdminTask(&types.ParamSubmitAdminTask{
		Op: types.AdminOpContext{OperatorId: 77}, TaskType: "batch_offline_submission",
	}); err == nil {
		t.Fatal("缺少 op.request_id 应被拒绝")
	}

	// 步骤数超过服务端上限。
	steps := make([]types.OperationTaskStepSpec, operationMaxTaskSteps+1)
	if _, err := l.SubmitAdminTask(&types.ParamSubmitAdminTask{
		Op: types.AdminOpContext{RequestId: "req-1"}, TaskType: "batch_offline_submission", Steps: steps,
	}); err == nil {
		t.Fatal("steps 超上限应被拒绝")
	} else if !strings.Contains(err.Error(), "steps") {
		t.Fatalf("错误消息应点名 steps，got %v", err)
	}

	// 缺 task_type。
	if _, err := l.SubmitAdminTask(&types.ParamSubmitAdminTask{
		Op: types.AdminOpContext{RequestId: "req-1"},
	}); err == nil {
		t.Fatal("缺 task_type 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	// 上限内正常提交：会话主体覆盖 op.operator_id，步骤全部投影。
	steps = steps[:operationMaxTaskSteps]
	fake.submitTaskReply = &operationrpc.SubmitAdminTaskReply{
		Task: &operationrpc.TaskInfo{TaskId: 88, State: "pending", Total: int32(len(steps))}, Reused: true,
	}
	resp, err := l.SubmitAdminTask(&types.ParamSubmitAdminTask{
		Op:       types.AdminOpContext{OperatorId: 9, RequestId: "req-1"},
		TaskType: "batch_offline_submission",
		Params:   `{"reason":"违规"}`,
		Steps:    steps,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.submitTaskReq.GetCtx().GetOperatorId() != 77 {
		t.Fatalf("operator_id = %d, want 77", fake.submitTaskReq.GetCtx().GetOperatorId())
	}
	if len(fake.submitTaskReq.GetSteps()) != operationMaxTaskSteps {
		t.Fatalf("steps = %d, want %d", len(fake.submitTaskReq.GetSteps()), operationMaxTaskSteps)
	}
	if resp.Data.Reused != true || resp.Data.Task.TaskId != 88 || resp.Data.Task.Total != int32(operationMaxTaskSteps) {
		t.Fatalf("data = %+v（reused 必须回显，否则运营以为提交了两批）", resp.Data)
	}
}

func TestListAdminUsersLogicNormalizesPageAndForwardsFilters(t *testing.T) {
	fake := &fakeOperation{listUsersReply: &operationrpc.ListAdminUsersReply{
		Items: []*operationrpc.AdminUserItem{{AdminId: 7, Username: "ops"}},
		Total: 1,
	}}
	l := NewListAdminUsersLogic(sessionCtx(77), &svc.ServiceContext{Operation: fake})
	resp, err := l.ListAdminUsers(&types.ParamListAdminUsers{
		Op: types.AdminOpContext{OperatorId: 9, RequestId: "ignored-on-read"}, State: 1, Keyword: "op", Pn: 0, Ps: 5000,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.listUsersReq.GetPn() != 1 || fake.listUsersReq.GetPs() != operationMaxPageSize {
		t.Fatalf("pn/ps = %d/%d, want 1/%d（网关不得替运营放大页大小）",
			fake.listUsersReq.GetPn(), fake.listUsersReq.GetPs(), operationMaxPageSize)
	}
	if fake.listUsersReq.GetState() != 1 || fake.listUsersReq.GetKeyword() != "op" {
		t.Fatalf("过滤条件未透传: %+v", fake.listUsersReq)
	}
	if fake.listUsersReq.GetCtx().GetOperatorId() != 77 {
		t.Fatalf("operator_id = %d, want 77", fake.listUsersReq.GetCtx().GetOperatorId())
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = code=%d message=%q ttl=%d", resp.Code, resp.Message, resp.TTL)
	}
	if resp.Data.Total != 1 || len(resp.Data.Items) != 1 || resp.Data.Items[0].Username != "ops" {
		t.Fatalf("data = %+v", resp.Data)
	}
}

func TestGetMenuLogicPropagatesServerTTL(t *testing.T) {
	fake := &fakeOperation{getMenuReply: &operationrpc.GetMenuReply{
		Items: []*operationrpc.MenuItem{{MenuId: 1, Name: "审核", RequiredPermission: "video:submission:read"}},
		Ttl:   300,
	}}
	l := NewGetMenuLogic(sessionCtx(77), &svc.ServiceContext{Operation: fake})
	resp, err := l.GetMenu(&types.ParamGetMenu{Op: types.AdminOpContext{}, AdminId: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.getMenuReq.GetAdminId() != 7 {
		t.Fatalf("admin_id = %d, want 7（查询目标不能被会话主体覆盖）", fake.getMenuReq.GetAdminId())
	}
	if resp.TTL != 300 || len(resp.Data.Items) != 1 {
		t.Fatalf("ttl = %d data = %+v", resp.TTL, resp.Data)
	}

	// 服务侧给出负 ttl（异常值）时归零，不传染前端缓存逻辑。
	fake.getMenuReply.Ttl = -1
	again, err := l.GetMenu(&types.ParamGetMenu{Op: types.AdminOpContext{}})
	if err != nil || again.TTL != 0 {
		t.Fatalf("ttl = %d err = %v, want 0", again.TTL, err)
	}
}

func TestSaveOpsConfigLogicValidation(t *testing.T) {
	fake := &fakeOperation{}
	l := NewSaveOpsConfigLogic(sessionCtx(77), &svc.ServiceContext{Operation: fake})
	if _, err := l.SaveOpsConfig(&types.ParamSaveOpsConfig{
		Op: types.AdminOpContext{RequestId: "req-1"}, ExpectVersion: -1,
	}); err == nil {
		t.Fatal("expect_version<0 应被拒绝")
	}
	if _, err := l.SaveOpsConfig(&types.ParamSaveOpsConfig{
		Op: types.AdminOpContext{RequestId: "req-1"}, ExpectVersion: 0,
	}); err == nil {
		t.Fatal("缺 cfg_key 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	fake.saveConfigReply = &operationrpc.SaveOpsConfigReply{
		Config: &operationrpc.ConfigItem{Id: 3, CfgKey: "ops.x", Version: 8},
	}
	resp, err := l.SaveOpsConfig(&types.ParamSaveOpsConfig{
		Op: types.AdminOpContext{RequestId: "req-1"}, CfgKey: "ops.x", CfgValue: "v",
		ValueType: "string", Scope: "global", ExpectVersion: 7,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.saveConfigReq.GetExpectVersion() != 7 {
		t.Fatalf("expect_version = %d, want 7（乐观锁不得被网关改写）", fake.saveConfigReq.GetExpectVersion())
	}
	if resp.Data.Config.Version != 8 || resp.Data.Config.CfgKey != "ops.x" {
		t.Fatalf("data = %+v（新版本号来自服务端）", resp.Data)
	}

	sentinel := errors.New("operation: config version conflict")
	fake.err = sentinel
	if _, err := l.SaveOpsConfig(&types.ParamSaveOpsConfig{
		Op: types.AdminOpContext{RequestId: "req-2"}, CfgKey: "ops.x",
	}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 下游错误原样上抛（冲突要交给 httpresponse 渲染）", err)
	}
}

func TestRunAdminTaskLogicValidation(t *testing.T) {
	fake := &fakeOperation{}
	l := NewRunAdminTaskLogic(sessionCtx(77), &svc.ServiceContext{Operation: fake})
	// 推进是写操作：缺 request_id 先拒。
	if _, err := l.RunAdminTask(&types.ParamRunAdminTask{TaskId: 88}); err == nil {
		t.Fatal("缺少 op.request_id 应被拒绝")
	}
	if _, err := l.RunAdminTask(&types.ParamRunAdminTask{
		Op: types.AdminOpContext{RequestId: "req-1"},
	}); err == nil {
		t.Fatal("task_id<=0 应被拒绝")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}

	fake.runTaskReply = &operationrpc.RunAdminTaskReply{
		Task:     &operationrpc.TaskInfo{TaskId: 88, State: "running", Succeeded: 2},
		Steps:    []*operationrpc.TaskStepInfo{{StepNo: 1, State: "succeeded"}},
		Executed: 2,
	}
	resp, err := l.RunAdminTask(&types.ParamRunAdminTask{
		Op: types.AdminOpContext{RequestId: "req-1"}, TaskId: 88,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// max_steps<=0 时由服务端按默认步数推进，网关不擅自放大批量。
	if fake.runTaskReq.GetMaxSteps() != 0 {
		t.Fatalf("max_steps = %d, want 0", fake.runTaskReq.GetMaxSteps())
	}
	if resp.Data.Executed != 2 || resp.Data.Task.State != "running" || len(resp.Data.Steps) != 1 {
		t.Fatalf("data = %+v", resp.Data)
	}
}

func TestListAuditIndexLogicValidation(t *testing.T) {
	fake := &fakeOperation{}
	l := NewListAuditIndexLogic(sessionCtx(77), &svc.ServiceContext{Operation: fake})
	if _, err := l.ListAuditIndex(&types.ParamListAuditIndex{StartAt: -1}); err == nil {
		t.Fatal("start_at<0 应被拒绝")
	}
	if _, err := l.ListAuditIndex(&types.ParamListAuditIndex{StartAt: 100, EndAt: 100}); err == nil {
		t.Fatal("start_at>=end_at 应被拒绝（空区间会让运营误判为无审计）")
	}
	if fake.calls != 0 {
		t.Fatalf("校验失败不得调用下游，实际调用 %d 次", fake.calls)
	}
	fake.listAuditReply = &operationrpc.ListAuditIndexReply{
		Items: []*operationrpc.AuditIndexItem{{Id: 5, AdminId: 7, Action: "ops_config.save"}}, Total: 1,
	}
	resp, err := l.ListAuditIndex(&types.ParamListAuditIndex{
		AdminId: 7, Action: "ops_config.save", StartAt: 100, EndAt: 200, Pn: 2, Ps: 30,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.listAuditReq.GetPn() != 2 || fake.listAuditReq.GetPs() != 30 {
		t.Fatalf("pn/ps = %d/%d, want 2/30", fake.listAuditReq.GetPn(), fake.listAuditReq.GetPs())
	}
	if fake.listAuditReq.GetAdminId() != 7 || fake.listAuditReq.GetAction() != "ops_config.save" {
		t.Fatalf("查询条件未透传: %+v", fake.listAuditReq)
	}
	if resp.Data.Total != 1 || resp.Data.Items[0].Action != "ops_config.save" {
		t.Fatalf("data = %+v", resp.Data)
	}
}

func TestOperationLogicsWithoutClientConfigured(t *testing.T) {
	empty := &svc.ServiceContext{}
	ctx := sessionCtx(77)
	req := types.AdminOpContext{RequestId: "req-1"}
	cases := []struct {
		name string
		call func() error
	}{
		{"adminLogin", func() error {
			_, err := NewAdminLoginLogic(ctx, empty).AdminLogin(&types.ParamAdminLogin{
				Username: "ops", Password: "p", RequestId: "req-1"})
			return err
		}},
		{"createAdminUser", func() error {
			_, err := NewCreateAdminUserLogic(ctx, empty).CreateAdminUser(&types.ParamCreateAdminUser{
				Op: req, Username: "ops", Password: "p"})
			return err
		}},
		{"submitAdminTask", func() error {
			_, err := NewSubmitAdminTaskLogic(ctx, empty).SubmitAdminTask(&types.ParamSubmitAdminTask{
				Op: req, TaskType: "batch_offline_submission"})
			return err
		}},
		{"getMenu", func() error {
			_, err := NewGetMenuLogic(ctx, empty).GetMenu(&types.ParamGetMenu{Op: req})
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); err == nil {
				t.Fatalf("%s：未配置 OperationRPC 时必须报错，不能退化成免鉴权空响应", c.name)
			}
		})
	}
}
