package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"google.golang.org/grpc"
)

// 本文件只锁 gateway/admin 面向 membership 的口径，刻意不断言会员域的业务结论
// （AGENTS.md §5/§8）：档位够不够、状态机能不能迁移、时长区间、币种是否支持、
// 幂等指纹是否一致、「付费来源必须能回溯到支付流水」——全部由 services/membership 判定。
//
// 锁死的四条是网关自己的责任边界：
//  1. 不伪造成功：没配 membership 客户端时十条路由一律 not configured，绝不回空台账/
//     found=false；下游报错原样上抛（duplicated=true 例外，那是成功结论不是错误）。
//  2. 主体不信任请求体：operator 一律由会话渲染成 gateway/admin:<admin_id>，
//     表单自称的 operator 只能进日志当线索；无会话即 fail-closed，一次调用都不发。
//  3. 幂等键与 0 哨兵原样下传：idempotency_key→request_id 不改写；page/size/limit/
//     mid=0/plan_id=0/expected_version=0 都是合法哨兵，网关不代填、不裁剪，
//     分页三元组照抄服务回显而不是回显请求值。
//  4. 投影逐字段不丢：台账的 operator/request_id/reason 与套餐的 *_minor 是审计与
//     资金口径的证据位，裁掉一位后台就得靠猜。
//
// 打桩方式与 private-message/collector 一致：内嵌生成的 client 接口 + 只覆盖本域用到的
// 10 个方法，其余方法（ListPlans/GetPlan/CheckEntitlement(s)/SetAutoRenew/ExpireMembership）
// 一旦被调用会 panic 在 nil 接口上——这正是「权益判定、自动续费签约与到期终结不开放后台
// 路由」这条边界的机器可检表达（见 admin.api 的 membership 段注释）。不建 gRPC 连接、不碰数据库。

var errCommerceMembershipDownstream = errors.New("membership downstream unavailable")

type commerceMembershipFake struct {
	membershiprpc.MembershipClient

	calls    int
	lastCall string
	err      error

	plansAdminReq   *membershiprpc.ListPlansAdminReq
	plansAdmin      *membershiprpc.ListPlansAdminReply
	memberReq       *membershiprpc.GetMembershipReq
	member          *membershiprpc.GetMembershipReply
	grantsReq       *membershiprpc.ListGrantsReq
	grants          *membershiprpc.ListGrantsReply
	expiringReq     *membershiprpc.ListExpiringMembershipsReq
	expiring        *membershiprpc.ListExpiringMembershipsReply
	entitlementsReq *membershiprpc.ListEntitlementsReq
	entitlements    *membershiprpc.ListEntitlementsReply

	upsertPlanReq       *membershiprpc.UpsertPlanReq
	upsertPlan          *membershiprpc.PlanInfo
	setPlanStateReq     *membershiprpc.SetPlanStateReq
	setPlanState        *membershiprpc.PlanInfo
	grantReq            *membershiprpc.GrantMembershipReq
	grant               *membershiprpc.GrantMembershipReply
	revokeReq           *membershiprpc.RevokeMembershipReq
	revoke              *membershiprpc.RevokeMembershipReply
	upsertEntitleReq    *membershiprpc.UpsertEntitlementReq
	upsertEntitleResult *membershiprpc.EntitlementInfo
}

// record 记一次调用；返回 true 表示这次要模拟下游失败。
func (f *commerceMembershipFake) record(call string) bool {
	f.calls++
	f.lastCall = call
	return f.err != nil
}

func (f *commerceMembershipFake) ListPlansAdmin(_ context.Context, in *membershiprpc.ListPlansAdminReq,
	_ ...grpc.CallOption) (*membershiprpc.ListPlansAdminReply, error) {
	f.plansAdminReq = in
	if f.record("ListPlansAdmin") {
		return nil, f.err
	}
	return f.plansAdmin, nil
}

func (f *commerceMembershipFake) GetMembership(_ context.Context, in *membershiprpc.GetMembershipReq,
	_ ...grpc.CallOption) (*membershiprpc.GetMembershipReply, error) {
	f.memberReq = in
	if f.record("GetMembership") {
		return nil, f.err
	}
	return f.member, nil
}

func (f *commerceMembershipFake) ListGrants(_ context.Context, in *membershiprpc.ListGrantsReq,
	_ ...grpc.CallOption) (*membershiprpc.ListGrantsReply, error) {
	f.grantsReq = in
	if f.record("ListGrants") {
		return nil, f.err
	}
	return f.grants, nil
}

func (f *commerceMembershipFake) ListExpiringMemberships(_ context.Context, in *membershiprpc.ListExpiringMembershipsReq,
	_ ...grpc.CallOption) (*membershiprpc.ListExpiringMembershipsReply, error) {
	f.expiringReq = in
	if f.record("ListExpiringMemberships") {
		return nil, f.err
	}
	return f.expiring, nil
}

func (f *commerceMembershipFake) ListEntitlements(_ context.Context, in *membershiprpc.ListEntitlementsReq,
	_ ...grpc.CallOption) (*membershiprpc.ListEntitlementsReply, error) {
	f.entitlementsReq = in
	if f.record("ListEntitlements") {
		return nil, f.err
	}
	return f.entitlements, nil
}

func (f *commerceMembershipFake) UpsertPlan(_ context.Context, in *membershiprpc.UpsertPlanReq,
	_ ...grpc.CallOption) (*membershiprpc.PlanInfo, error) {
	f.upsertPlanReq = in
	if f.record("UpsertPlan") {
		return nil, f.err
	}
	return f.upsertPlan, nil
}

func (f *commerceMembershipFake) SetPlanState(_ context.Context, in *membershiprpc.SetPlanStateReq,
	_ ...grpc.CallOption) (*membershiprpc.PlanInfo, error) {
	f.setPlanStateReq = in
	if f.record("SetPlanState") {
		return nil, f.err
	}
	return f.setPlanState, nil
}

func (f *commerceMembershipFake) GrantMembership(_ context.Context, in *membershiprpc.GrantMembershipReq,
	_ ...grpc.CallOption) (*membershiprpc.GrantMembershipReply, error) {
	f.grantReq = in
	if f.record("GrantMembership") {
		return nil, f.err
	}
	return f.grant, nil
}

func (f *commerceMembershipFake) RevokeMembership(_ context.Context, in *membershiprpc.RevokeMembershipReq,
	_ ...grpc.CallOption) (*membershiprpc.RevokeMembershipReply, error) {
	f.revokeReq = in
	if f.record("RevokeMembership") {
		return nil, f.err
	}
	return f.revoke, nil
}

func (f *commerceMembershipFake) UpsertEntitlement(_ context.Context, in *membershiprpc.UpsertEntitlementReq,
	_ ...grpc.CallOption) (*membershiprpc.EntitlementInfo, error) {
	f.upsertEntitleReq = in
	if f.record("UpsertEntitlement") {
		return nil, f.err
	}
	return f.upsertEntitleResult, nil
}

func commerceMembershipSvc(fake membershiprpc.MembershipClient) *svc.ServiceContext {
	return &svc.ServiceContext{Membership: fake}
}

// commerceMembershipSession 是中间件判定通过后写入 context 的会话身份。
// admin_id=77 与表单里自称的 operator=9001 故意不同，用来验证「谁赢」。
func commerceMembershipSession() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77, Roles: []string{"membership_operator"},
	})
}

func commerceMembershipFullPlan() *membershiprpc.PlanInfo {
	return &membershiprpc.PlanInfo{
		PlanId: 11, PlanCode: "vip_premium_month", Name: "大会员月卡", Description: "31 天",
		VipType: membershiprpc.VipType_VIP_TYPE_PREMIUM, DurationDays: 31, UnitCount: 1,
		PriceMinor: 2500, PromPriceMinor: 1980, Currency: "CNY",
		Platforms: []membershiprpc.PlanPlatform{
			membershiprpc.PlanPlatform_PLAN_PLATFORM_ANDROID,
			membershiprpc.PlanPlatform_PLAN_PLATFORM_IOS,
			membershiprpc.PlanPlatform_PLAN_PLATFORM_HARMONY,
		},
		AutoRenewSupported: true, State: membershiprpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE,
		Version: 4, Ctime: 1700000000, Mtime: 1700000600,
		CreatedBy: "gateway/admin:77", UpdatedBy: "gateway/admin:78",
	}
}

func commerceMembershipFullMembership() *membershiprpc.MembershipInfo {
	return &membershiprpc.MembershipInfo{
		Mid: 10001, VipType: membershiprpc.VipType_VIP_TYPE_PREMIUM_PLUS,
		StartAt: 1700000000, ExpireAt: 1800000000, AutoRenew: true,
		AutoRenewChannel: "SANDBOX", AutoRenewSignedAt: 1700000100,
		Source:         membershiprpc.GrantSource_GRANT_SOURCE_ADMIN_OPS,
		PaidMonthCount: 12, Version: 7, Ctime: 1700000000, Mtime: 1700000600,
	}
}

func commerceMembershipFullGrant() *membershiprpc.GrantInfo {
	return &membershiprpc.GrantInfo{
		GrantId: 501, Mid: 10001, VipType: membershiprpc.VipType_VIP_TYPE_PREMIUM,
		Action: "EXTEND", DeltaDays: 31, PlanId: 11,
		Source:         membershiprpc.GrantSource_GRANT_SOURCE_SANDBOX_PURCHASE,
		BizOrderNo:     "BO20260922001",
		PaymentNo:      "PM20260922001",
		BeforeExpireAt: 1790000000, AfterExpireAt: 1800000000,
		Operator: "gateway/admin:77", RequestId: "k-1", Reason: "沙箱订单履约", Ctime: 1700000000,
	}
}

func commerceMembershipFullEntitlement() *membershiprpc.EntitlementInfo {
	return &membershiprpc.EntitlementInfo{
		Code: "vip.high_bitrate", Name: "高码率", Description: "1080P+",
		MinVipType: membershiprpc.VipType_VIP_TYPE_PREMIUM, Enabled: true,
		Version: 3, Ctime: 1700000000, Mtime: 1700000600,
	}
}

// --- 五条写路由的合法入参样例（每个用例都取一份新副本，避免用例之间互相污染）---

func commerceGoodPlanUpsert() *types.ParamMembershipPlanUpsert {
	return &types.ParamMembershipPlanUpsert{
		PlanCode: "vip_premium_month", Name: "大会员月卡", VipType: 1, DurationDays: 31, UnitCount: 1,
		PriceMinor: 2500, PromPriceMinor: 1980, Currency: "CNY",
		Platforms: []int32{1, 2, 3}, AutoRenewSupported: true,
		Operator: 9001, IdempotencyKey: "k-plan-1", TraceId: "t-plan-1",
	}
}

func commerceGoodPlanState() *types.ParamMembershipPlanState {
	return &types.ParamMembershipPlanState{
		PlanId: 11, TargetState: 3, ExpectedVersion: 4, Reason: "促销结束下架",
		Operator: 9001, IdempotencyKey: "k-state-1", TraceId: "t-state-1",
	}
}

func commerceGoodGrant() *types.ParamMembershipGrant {
	return &types.ParamMembershipGrant{
		Mid: 10001, VipType: 1, PlanId: 11, DeltaDays: 31, Source: 3,
		BizOrderNo: "BO20260922001", PaymentNo: "PM20260922001", Reason: "客服补偿",
		Operator: 9001, IdempotencyKey: "k-grant-1", TraceId: "t-grant-1",
	}
}

func commerceGoodRevoke() *types.ParamMembershipRevoke {
	return &types.ParamMembershipRevoke{
		Mid: 10001, VipType: 1, ClearRemaining: false, DeltaDays: 31, Reason: "运营纠错",
		Operator: 9001, IdempotencyKey: "k-revoke-1", TraceId: "t-revoke-1",
	}
}

func commerceGoodEntitlementUpsert() *types.ParamMembershipEntitlementUpsert {
	return &types.ParamMembershipEntitlementUpsert{
		Code: "vip.high_bitrate", Name: "高码率", MinVipType: 1, Enabled: true, ExpectedVersion: 3,
		Operator: 9001, IdempotencyKey: "k-ent-1", TraceId: "t-ent-1",
	}
}

// --- 1. 不伪造成功 ---

// commerceRoutes 把十条路由收成一个表：nil 客户端、下游错误两条门禁都要逐条覆盖，
// 漏一条就等于留了一个「后台看到空台账」的口子。
var commerceRoutes = []struct {
	name string
	run  func(ctx context.Context, svcCtx *svc.ServiceContext) error
}{
	{"plan/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipPlanListLogic(ctx, s).MembershipPlanList(&types.ParamMembershipPlanList{Page: 1, Size: 20})
		return err
	}},
	{"member/get", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipMemberGetLogic(ctx, s).MembershipMemberGet(&types.ParamMembershipMemberGet{Mid: 10001})
		return err
	}},
	{"grant/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipGrantListLogic(ctx, s).MembershipGrantList(&types.ParamMembershipGrantList{Mid: 10001})
		return err
	}},
	{"expiring/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipExpiringListLogic(ctx, s).
			MembershipExpiringList(&types.ParamMembershipExpiringList{ToExpireAt: 1800000000})
		return err
	}},
	{"entitlement/list", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipEntitlementListLogic(ctx, s).
			MembershipEntitlementList(&types.ParamMembershipEntitlementList{EnabledOnly: true})
		return err
	}},
	{"plan/upsert", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipPlanUpsertLogic(ctx, s).MembershipPlanUpsert(commerceGoodPlanUpsert())
		return err
	}},
	{"plan/state", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipPlanStateLogic(ctx, s).MembershipPlanState(commerceGoodPlanState())
		return err
	}},
	{"grant", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipGrantLogic(ctx, s).MembershipGrant(commerceGoodGrant())
		return err
	}},
	{"grant/revoke", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipGrantRevokeLogic(ctx, s).MembershipGrantRevoke(commerceGoodRevoke())
		return err
	}},
	{"entitlement/upsert", func(ctx context.Context, s *svc.ServiceContext) error {
		_, err := NewMembershipEntitlementUpsertLogic(ctx, s).
			MembershipEntitlementUpsert(commerceGoodEntitlementUpsert())
		return err
	}},
}

func TestCommerceMembershipAllRoutes_NilClientDoesNotFakeLedger(t *testing.T) {
	empty := &svc.ServiceContext{} // Membership 未配置
	ctx := commerceMembershipSession()
	for _, route := range commerceRoutes {
		err := route.run(ctx, empty)
		if !errors.Is(err, errMembershipServiceNotConfigured) {
			t.Fatalf("%s: 未配置客户端必须回 not configured 而不是空台账，实际 %v", route.name, err)
		}
		if !strings.Contains(err.Error(), "membership") {
			t.Fatalf("%s: 错误消息要点名是哪个下游没接，实际 %s", route.name, err.Error())
		}
	}
}

func TestCommerceMembershipAllRoutes_DownstreamErrorPropagatesVerbatim(t *testing.T) {
	bad := &commerceMembershipFake{err: errCommerceMembershipDownstream}
	ctx := commerceMembershipSession()
	for i, route := range commerceRoutes {
		err := route.run(ctx, commerceMembershipSvc(bad))
		if !errors.Is(err, errCommerceMembershipDownstream) {
			t.Fatalf("%s: 下游错误必须原样上抛（不得折成成功或空数据），实际 %v", route.name, err)
		}
		if bad.calls != i+1 {
			t.Fatalf("%s: 每条路由都要真打一次下游（累计 %d 次，lastCall=%s）", route.name, bad.calls, bad.lastCall)
		}
	}
	if bad.calls != len(commerceRoutes) {
		t.Fatalf("十条路由各一次，实际 %d", bad.calls)
	}
}

// --- 2. 操作者身份 ---

// TestCommerceMembershipWriteRoutes_OperatorAlwaysFromSession 覆盖五条写路由：
// 进 RPC 的 operator 永远是会话渲染值，request_id 永远是幂等键原值
// （membership.proto 的 req 没有 trace 字段，trace_id 只进日志）。
func TestCommerceMembershipWriteRoutes_OperatorAlwaysFromSession(t *testing.T) {
	fake := &commerceMembershipFake{
		upsertPlan:          commerceMembershipFullPlan(),
		setPlanState:        commerceMembershipFullPlan(),
		grant:               &membershiprpc.GrantMembershipReply{Membership: commerceMembershipFullMembership(), GrantId: 501},
		revoke:              &membershiprpc.RevokeMembershipReply{Membership: commerceMembershipFullMembership(), GrantId: 502},
		upsertEntitleResult: commerceMembershipFullEntitlement(),
	}
	svcCtx := commerceMembershipSvc(fake)
	ctx := commerceMembershipSession()
	if _, err := NewMembershipPlanUpsertLogic(ctx, svcCtx).MembershipPlanUpsert(commerceGoodPlanUpsert()); err != nil {
		t.Fatalf("plan/upsert: %v", err)
	}
	if _, err := NewMembershipPlanStateLogic(ctx, svcCtx).MembershipPlanState(commerceGoodPlanState()); err != nil {
		t.Fatalf("plan/state: %v", err)
	}
	if _, err := NewMembershipGrantLogic(ctx, svcCtx).MembershipGrant(commerceGoodGrant()); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := NewMembershipGrantRevokeLogic(ctx, svcCtx).MembershipGrantRevoke(commerceGoodRevoke()); err != nil {
		t.Fatalf("grant/revoke: %v", err)
	}
	if _, err := NewMembershipEntitlementUpsertLogic(ctx, svcCtx).
		MembershipEntitlementUpsert(commerceGoodEntitlementUpsert()); err != nil {
		t.Fatalf("entitlement/upsert: %v", err)
	}

	wantOperator := "gateway/admin:77" // 会话 admin_id=77；表单自称的 9001 必须作废
	cases := []struct {
		route     string
		operator  string
		requestID string
		want      string
	}{
		{"plan/upsert", fake.upsertPlanReq.GetOperator(), fake.upsertPlanReq.GetRequestId(), "k-plan-1"},
		{"plan/state", fake.setPlanStateReq.GetOperator(), fake.setPlanStateReq.GetRequestId(), "k-state-1"},
		{"grant", fake.grantReq.GetOperator(), fake.grantReq.GetRequestId(), "k-grant-1"},
		{"grant/revoke", fake.revokeReq.GetOperator(), fake.revokeReq.GetRequestId(), "k-revoke-1"},
		{"entitlement/upsert", fake.upsertEntitleReq.GetOperator(), fake.upsertEntitleReq.GetRequestId(), "k-ent-1"},
	}
	for _, c := range cases {
		if c.operator != wantOperator {
			t.Fatalf("%s: operator 必须来自会话而不是请求体，实际 %q", c.route, c.operator)
		}
		if c.requestID != c.want {
			t.Fatalf("%s: idempotency_key 要原样映射成 request_id，实际 %q want %q", c.route, c.requestID, c.want)
		}
	}
}

func TestCommerceMembershipWriteRoutes_FailClosedWithoutSession(t *testing.T) {
	// 拿不到会话身份 = 这条路由没被 AdminPermission 保护（权限表/挂载漂移），
	// 必须在下传前拒绝，一次 RPC 都不能发。AdminID<=0 与「完全没有身份」都要挡住。
	sessions := []struct {
		name string
		ctx  context.Context
	}{
		{"无身份", context.Background()},
		{"身份非法（admin_id=0）", middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 0})},
	}
	for _, route := range commerceRoutes {
		if !commerceIsWriteRoute(route.name) {
			continue // 只跑五条写路由
		}
		for _, session := range sessions {
			fake := &commerceMembershipFake{}
			err := route.run(session.ctx, commerceMembershipSvc(fake))
			if !errors.Is(err, errMembershipSessionRequired) {
				t.Fatalf("%s/%s: 缺会话身份必须 fail-closed，实际 %v", route.name, session.name, err)
			}
			if fake.calls != 0 {
				t.Fatalf("%s/%s: fail-closed 却已经打了下游 %d 次（lastCall=%s）",
					route.name, session.name, fake.calls, fake.lastCall)
			}
		}
	}
}

// commerceIsWriteRoute 判本域五条写路由：admin.api 里只有它们挂了 AdminPermission。
func commerceIsWriteRoute(name string) bool {
	switch name {
	case "plan/upsert", "plan/state", "grant", "grant/revoke", "entitlement/upsert":
		return true
	default:
		return false
	}
}

func TestCommerceMembershipWriteRoutes_RequiredFieldsGateBeforeDownstream(t *testing.T) {
	ctx := commerceMembershipSession()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"套餐缺编码", func(s *svc.ServiceContext) error {
			r := commerceGoodPlanUpsert()
			r.PlanCode = "  "
			_, err := NewMembershipPlanUpsertLogic(ctx, s).MembershipPlanUpsert(r)
			return err
		}, "plan_code"},
		{"套餐缺币种（不靠服务默认值兜）", func(s *svc.ServiceContext) error {
			r := commerceGoodPlanUpsert()
			r.Currency = ""
			_, err := NewMembershipPlanUpsertLogic(ctx, s).MembershipPlanUpsert(r)
			return err
		}, "currency"},
		{"套餐档位 UNSPECIFIED", func(s *svc.ServiceContext) error {
			r := commerceGoodPlanUpsert()
			r.VipType = 0
			_, err := NewMembershipPlanUpsertLogic(ctx, s).MembershipPlanUpsert(r)
			return err
		}, "vip_type"},
		{"套餐 CAS 位为负", func(s *svc.ServiceContext) error {
			r := commerceGoodPlanUpsert()
			r.ExpectedVersion = -1
			_, err := NewMembershipPlanUpsertLogic(ctx, s).MembershipPlanUpsert(r)
			return err
		}, "expected_version"},
		{"上下架无理由", func(s *svc.ServiceContext) error {
			r := commerceGoodPlanState()
			r.Reason = " "
			_, err := NewMembershipPlanStateLogic(ctx, s).MembershipPlanState(r)
			return err
		}, "reason"},
		{"上下架目标状态 UNSPECIFIED", func(s *svc.ServiceContext) error {
			r := commerceGoodPlanState()
			r.TargetState = 0
			_, err := NewMembershipPlanStateLogic(ctx, s).MembershipPlanState(r)
			return err
		}, "target_state"},
		{"上下架缺 plan_id", func(s *svc.ServiceContext) error {
			r := commerceGoodPlanState()
			r.PlanId = 0
			_, err := NewMembershipPlanStateLogic(ctx, s).MembershipPlanState(r)
			return err
		}, "plan_id"},
		{"开通无幂等键", func(s *svc.ServiceContext) error {
			r := commerceGoodGrant()
			r.IdempotencyKey = ""
			_, err := NewMembershipGrantLogic(ctx, s).MembershipGrant(r)
			return err
		}, "idempotency_key"},
		{"开通来源 UNSPECIFIED", func(s *svc.ServiceContext) error {
			r := commerceGoodGrant()
			r.Source = 0
			_, err := NewMembershipGrantLogic(ctx, s).MembershipGrant(r)
			return err
		}, "source"},
		{"收回 mid=0", func(s *svc.ServiceContext) error {
			r := commerceGoodRevoke()
			r.Mid = 0
			_, err := NewMembershipGrantRevokeLogic(ctx, s).MembershipGrantRevoke(r)
			return err
		}, "mid"},
		{"权益码缺 code", func(s *svc.ServiceContext) error {
			r := commerceGoodEntitlementUpsert()
			r.Code = ""
			_, err := NewMembershipEntitlementUpsertLogic(ctx, s).MembershipEntitlementUpsert(r)
			return err
		}, "code"},
	}
	for _, c := range cases {
		fake := &commerceMembershipFake{}
		err := c.run(commerceMembershipSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 错误消息要点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

// --- 3. 分页 / 时间窗 / 0 哨兵 ---

func TestCommerceMembershipPlanList_PagingPassesThroughAndEchoesServiceClamp(t *testing.T) {
	fake := &commerceMembershipFake{plansAdmin: &membershiprpc.ListPlansAdminReply{
		Plans: []*membershiprpc.PlanInfo{commerceMembershipFullPlan()},
		Total: 37, Page: 2, Size: 100, // 服务把 size=500 裁到 MaxPageSize 并回显
	}}
	resp, err := NewMembershipPlanListLogic(context.Background(), commerceMembershipSvc(fake)).
		MembershipPlanList(&types.ParamMembershipPlanList{State: 2, VipType: 1, Keyword: "vip_", Page: 2, Size: 500})
	if err != nil {
		t.Fatalf("翻页不该被网关拒绝: %v", err)
	}
	// 网关不裁剪：把请求值原样交给服务，由它决定上限。
	if fake.plansAdminReq.GetSize() != 500 || fake.plansAdminReq.GetPage() != 2 {
		t.Fatalf("page/size 被网关改写: %+v", fake.plansAdminReq)
	}
	if fake.plansAdminReq.GetState() != membershiprpc.PlanSaleState_PLAN_SALE_STATE_ON_SALE ||
		fake.plansAdminReq.GetVipType() != membershiprpc.VipType_VIP_TYPE_PREMIUM ||
		fake.plansAdminReq.GetKeyword() != "vip_" {
		t.Fatalf("过滤条件没按契约下传: %+v", fake.plansAdminReq)
	}
	// total/page/size 只能是服务给的那一页，不是请求回显。
	if resp.Data.Total != 37 || resp.Data.Page != 2 || resp.Data.Size != 100 {
		t.Fatalf("分页三元组必须照抄服务回显: %+v", resp.Data)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封必须固定 code=0/message=ok/ttl=0: %+v", resp)
	}
}

func TestCommerceMembershipPlanList_ZeroMeansNoFilter(t *testing.T) {
	fake := &commerceMembershipFake{plansAdmin: &membershiprpc.ListPlansAdminReply{}}
	resp, err := NewMembershipPlanListLogic(context.Background(), commerceMembershipSvc(fake)).
		MembershipPlanList(&types.ParamMembershipPlanList{})
	if err != nil {
		t.Fatalf("全 0 是「不过滤 + 用默认页」: %v", err)
	}
	if fake.plansAdminReq.GetState() != 0 || fake.plansAdminReq.GetVipType() != 0 ||
		fake.plansAdminReq.GetPage() != 0 || fake.plansAdminReq.GetSize() != 0 {
		t.Fatalf("0 是合法哨兵，网关不得代填: %+v", fake.plansAdminReq)
	}
	if resp.Data.List == nil {
		t.Fatalf("空列表要投影成 []，客户端才能直接遍历: %#v", resp.Data.List)
	}
}

func TestCommerceMembershipLists_RejectImpossibleShapes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(svcCtx *svc.ServiceContext) error
		want string
	}{
		{"套餐页码为负", func(s *svc.ServiceContext) error {
			_, err := NewMembershipPlanListLogic(ctx, s).MembershipPlanList(&types.ParamMembershipPlanList{Page: -1})
			return err
		}, "page"},
		{"套餐页大小为负", func(s *svc.ServiceContext) error {
			_, err := NewMembershipPlanListLogic(ctx, s).MembershipPlanList(&types.ParamMembershipPlanList{Size: -1})
			return err
		}, "size"},
		{"台账 mid 为负", func(s *svc.ServiceContext) error {
			_, err := NewMembershipGrantListLogic(ctx, s).MembershipGrantList(&types.ParamMembershipGrantList{Mid: -1})
			return err
		}, "mid"},
		{"台账窗口倒置", func(s *svc.ServiceContext) error {
			_, err := NewMembershipGrantListLogic(ctx, s).MembershipGrantList(&types.ParamMembershipGrantList{
				FromTs: 200, ToTs: 100,
			})
			return err
		}, "from_ts"},
		{"台账窗口为负", func(s *svc.ServiceContext) error {
			_, err := NewMembershipGrantListLogic(ctx, s).MembershipGrantList(&types.ParamMembershipGrantList{ToTs: -1})
			return err
		}, "to_ts"},
		{"到期区间缺上界", func(s *svc.ServiceContext) error {
			_, err := NewMembershipExpiringListLogic(ctx, s).MembershipExpiringList(&types.ParamMembershipExpiringList{
				FromExpireAt: 100, ToExpireAt: 0,
			})
			return err
		}, "to_expire_at"},
		{"到期区间倒置", func(s *svc.ServiceContext) error {
			_, err := NewMembershipExpiringListLogic(ctx, s).MembershipExpiringList(&types.ParamMembershipExpiringList{
				FromExpireAt: 200, ToExpireAt: 100,
			})
			return err
		}, "from_expire_at"},
		{"到期区间 limit 为负", func(s *svc.ServiceContext) error {
			_, err := NewMembershipExpiringListLogic(ctx, s).MembershipExpiringList(&types.ParamMembershipExpiringList{
				ToExpireAt: 100, Limit: -1,
			})
			return err
		}, "limit"},
		{"会员查询 mid=0", func(s *svc.ServiceContext) error {
			_, err := NewMembershipMemberGetLogic(ctx, s).MembershipMemberGet(&types.ParamMembershipMemberGet{Mid: 0})
			return err
		}, "mid"},
	}
	for _, c := range cases {
		fake := &commerceMembershipFake{}
		err := c.run(commerceMembershipSvc(fake))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 要在下传前拒掉并点名字段 %s，实际 %v", c.name, c.want, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 已被拒的入参不该打到下游（lastCall=%s）", c.name, fake.lastCall)
		}
	}
}

func TestCommerceMembershipExpiringList_FromZeroCoversHistoryAndLimitZeroUsesServiceCap(t *testing.T) {
	fake := &commerceMembershipFake{expiring: &membershiprpc.ListExpiringMembershipsReply{
		Memberships:        []*membershiprpc.MembershipInfo{commerceMembershipFullMembership()},
		NextExpireAtCursor: 1800000000,
	}}
	resp, err := NewMembershipExpiringListLogic(context.Background(), commerceMembershipSvc(fake)).
		MembershipExpiringList(&types.ParamMembershipExpiringList{ToExpireAt: 1800000000, AutoRenewOnly: true})
	if err != nil {
		t.Fatalf("from_expire_at=0 在服务侧按 1 处理以覆盖历史行: %v", err)
	}
	if fake.expiringReq.GetFromExpireAt() != 0 || fake.expiringReq.GetLimit() != 0 ||
		!fake.expiringReq.GetAutoRenewOnly() {
		t.Fatalf("入参被改写: %+v", fake.expiringReq)
	}
	if resp.Data.NextExpireAtCursor != 1800000000 || len(resp.Data.List) != 1 ||
		resp.Data.List[0].ExpireAt != 1800000000 {
		t.Fatalf("游标与列表要原样转达: %+v", resp.Data)
	}
}

func TestCommerceMembershipGrantList_WindowPassesThroughUnchanged(t *testing.T) {
	fake := &commerceMembershipFake{grants: &membershiprpc.ListGrantsReply{Total: 0, Page: 1, Size: 20}}
	// 单边 0 = 该侧不设界，服务只在两侧都为正且倒置时拒绝。
	if _, err := NewMembershipGrantListLogic(context.Background(), commerceMembershipSvc(fake)).
		MembershipGrantList(&types.ParamMembershipGrantList{FromTs: 1700000000, ToTs: 0, Page: 1, Size: 20}); err != nil {
		t.Fatalf("单边不设界的窗口是合法请求: %v", err)
	}
	if fake.grantsReq.GetFromTs() != 1700000000 || fake.grantsReq.GetToTs() != 0 ||
		fake.grantsReq.GetPage() != 1 || fake.grantsReq.GetSize() != 20 {
		t.Fatalf("时间窗与分页原样下传: %+v", fake.grantsReq)
	}
}

// --- 4. reply → API 投影（表驱动，逐字段） ---

func TestCommerceMembershipProjection_FieldByField(t *testing.T) {
	plans := commerceMembershipPlanDiff(membershipPlanToAPI(commerceMembershipFullPlan()), commerceMembershipFullPlan())
	slices := membershipPlansToAPI([]*membershiprpc.PlanInfo{nil, commerceMembershipFullPlan()})
	if len(slices) != 2 {
		t.Fatalf("套餐列表投影条数不对: %d", len(slices))
	}
	if got := commerceMembershipPlanDiff(slices[0], &membershiprpc.PlanInfo{}); len(got) != 0 {
		t.Fatalf("nil 行要投影成零值而不是 panic: %v", got)
	}
	if got := commerceMembershipPlanDiff(slices[1], commerceMembershipFullPlan()); len(got) != 0 {
		t.Fatalf("列表里的行与单行投影不一致: %v", got)
	}

	cases := []struct {
		name  string
		diffs []string
	}{
		{"PlanInfo", plans},
		{"MembershipInfo", commerceMembershipMemberDiff(membershipMemberToAPI(commerceMembershipFullMembership()),
			commerceMembershipFullMembership())},
		{"GrantInfo", commerceMembershipGrantDiff(membershipGrantToAPI(commerceMembershipFullGrant()),
			commerceMembershipFullGrant())},
		{"EntitlementInfo", commerceMembershipEntitlementDiff(membershipEntitlementToAPI(commerceMembershipFullEntitlement()),
			commerceMembershipFullEntitlement())},
		{"nil MembershipInfo", commerceMembershipMemberDiff(membershipMemberToAPI(nil), &membershiprpc.MembershipInfo{})},
		{"nil GrantInfo", commerceMembershipGrantDiff(membershipGrantToAPI(nil), &membershiprpc.GrantInfo{})},
		{"nil EntitlementInfo", commerceMembershipEntitlementDiff(membershipEntitlementToAPI(nil),
			&membershiprpc.EntitlementInfo{})},
	}
	for _, c := range cases {
		if len(c.diffs) > 0 {
			t.Fatalf("%s 投影丢字段/改字段: %v", c.name, c.diffs)
		}
	}
	if got := membershipPlatformsToAPI(nil); got == nil || len(got) != 0 {
		t.Fatalf("空平台列表要投影成 []: %#v", got)
	}
}

func TestCommerceMembershipMemberGet_FoundFalseIsNotAnError(t *testing.T) {
	fake := &commerceMembershipFake{member: &membershiprpc.GetMembershipReply{
		Found:     false,
		ServerNow: 1800000000, // 没有授予记录时服务仍回真实时钟
	}}
	resp, err := NewMembershipMemberGetLogic(context.Background(), commerceMembershipSvc(fake)).
		MembershipMemberGet(&types.ParamMembershipMemberGet{Mid: 10001})
	if err != nil {
		t.Fatalf("found=false 是结论不是错误: %v", err)
	}
	if resp.Data.Found || resp.Data.ServerNow != 1800000000 || resp.Data.Membership.Mid != 0 {
		t.Fatalf("未开通必须投影成零值身份行 + 服务时钟: %+v", resp.Data)
	}
	if resp.Data.GrantedEntitlements == nil {
		t.Fatalf("权益投影为空时也要给 []，不能是 null: %#v", resp.Data.GrantedEntitlements)
	}
	if fake.memberReq.GetVipType() != membershiprpc.VipType_VIP_TYPE_UNSPECIFIED {
		t.Fatalf("vip_type=0 表示「取当前最高档」，网关不得代填: %+v", fake.memberReq)
	}
}

func TestCommerceMembershipMemberGet_GrantedEntitlementsKept(t *testing.T) {
	fake := &commerceMembershipFake{member: &membershiprpc.GetMembershipReply{
		Found:      true,
		Membership: commerceMembershipFullMembership(),
		ServerNow:  1750000000,
		GrantedEntitlements: []*membershiprpc.EntitlementInfo{
			commerceMembershipFullEntitlement(), nil,
		},
	}}
	resp, err := NewMembershipMemberGetLogic(context.Background(), commerceMembershipSvc(fake)).
		MembershipMemberGet(&types.ParamMembershipMemberGet{Mid: 10001, VipType: 2})
	if err != nil {
		t.Fatalf("会员详情页: %v", err)
	}
	if len(resp.Data.GrantedEntitlements) != 2 || resp.Data.GrantedEntitlements[0].Code != "vip.high_bitrate" {
		t.Fatalf("权益投影要保序保条数（nil 行给零值而不是丢掉）: %+v", resp.Data.GrantedEntitlements)
	}
	if resp.Data.Membership.ExpireAt != 1800000000 || resp.Data.ServerNow != 1750000000 {
		t.Fatalf("到期时间与服务时钟不能丢: %+v", resp.Data)
	}
}

func TestCommerceMembershipGrant_ReplayIsSuccessWithFirstResult(t *testing.T) {
	fake := &commerceMembershipFake{grant: &membershiprpc.GrantMembershipReply{
		Duplicated: true, GrantId: 501, Membership: commerceMembershipFullMembership(),
	}}
	resp, err := NewMembershipGrantLogic(commerceMembershipSession(), commerceMembershipSvc(fake)).
		MembershipGrant(commerceGoodGrant())
	if err != nil {
		t.Fatalf("命中幂等键重放是成功结论: %v", err)
	}
	if !resp.Data.Duplicated || resp.Data.GrantId != 501 || resp.Data.Membership.Version != 7 {
		t.Fatalf("首次结论被丢了: %+v", resp.Data)
	}
}

func TestCommerceMembershipRevoke_VipTypeAndDeltaGoUnchangedToService(t *testing.T) {
	fake := &commerceMembershipFake{revoke: &membershiprpc.RevokeMembershipReply{
		GrantId: 502, Membership: commerceMembershipFullMembership(),
	}}
	// vip_type=0 与 clear_remaining/delta_days 的互斥都是服务的判定，网关一个都不接管。
	req := commerceGoodRevoke()
	req.VipType = 0
	req.ClearRemaining = true
	req.DeltaDays = 7
	if _, err := NewMembershipGrantRevokeLogic(commerceMembershipSession(), commerceMembershipSvc(fake)).
		MembershipGrantRevoke(req); err != nil {
		t.Fatalf("这些冲突要交给服务判，网关不预改: %v", err)
	}
	if fake.revokeReq.GetVipType() != membershiprpc.VipType_VIP_TYPE_UNSPECIFIED ||
		!fake.revokeReq.GetClearRemaining() || fake.revokeReq.GetDeltaDays() != 7 {
		t.Fatalf("收回入参被网关改写: %+v", fake.revokeReq)
	}
	// 表单没给追溯位时必须保持零值：网关不猜单号、不把「手工收回」伪造成「按单回收」。
	if fake.revokeReq.GetPlanId() != 0 || fake.revokeReq.GetBizOrderNo() != "" || fake.revokeReq.GetPaymentNo() != "" {
		t.Fatalf("后台路由不得凭空造追溯位: %+v", fake.revokeReq)
	}
}

// TestCommerceMembershipRevoke_TraceabilityFieldsGoVerbatim 钉 2026-09-22 补进 .api 的三个追溯位。
// 服务侧 source 列只有 ADMIN_OPS 一个值，「退款回收」与「运营纠错」在台账上就靠
// plan_id/biz_order_no/payment_no 区分，所以：给了要逐字下传（不 trim、不补前缀），
// 含空白的单号在网关就拒（跨服务引用是精确匹配，带空格等于一条对不上账的台账），
// 负 plan_id 同样不下传（0 已经是「无套餐」的合法哨兵）。
func TestCommerceMembershipRevoke_TraceabilityFieldsGoVerbatim(t *testing.T) {
	fake := &commerceMembershipFake{revoke: &membershiprpc.RevokeMembershipReply{
		GrantId: 503, Membership: commerceMembershipFullMembership(),
	}}
	req := commerceGoodRevoke()
	req.PlanId = 77
	req.BizOrderNo = "to_01HXAF8Q9Y2K7N3P5R7S9T0V1W"
	req.PaymentNo = "PM_2026.0001" // 大小写与点号都是单号的一部分：不裁、不规范化、不补前缀
	if _, err := NewMembershipGrantRevokeLogic(commerceMembershipSession(), commerceMembershipSvc(fake)).
		MembershipGrantRevoke(req); err != nil {
		t.Fatalf("带单号的收回要能打到服务: %v", err)
	}
	got := fake.revokeReq
	if got.GetPlanId() != 77 || got.GetBizOrderNo() != req.BizOrderNo || got.GetPaymentNo() != req.PaymentNo {
		t.Fatalf("追溯位被改写（不 trim、不猜是底线）: %+v", got)
	}

	for _, c := range []struct {
		name   string
		field  string
		mutate func(*types.ParamMembershipRevoke)
	}{
		{"单号带首尾空格", "biz_order_no", func(r *types.ParamMembershipRevoke) { r.BizOrderNo = " to_1" }},
		{"单号带内部空格", "payment_no", func(r *types.ParamMembershipRevoke) { r.PaymentNo = "pm 2026 0001" }},
		{"单号带换行", "payment_no", func(r *types.ParamMembershipRevoke) { r.PaymentNo = "pm_1\n" }},
		{"plan_id 为负", "plan_id", func(r *types.ParamMembershipRevoke) { r.PlanId = -1 }},
	} {
		before := fake.calls
		req := *commerceGoodRevoke()
		c.mutate(&req)
		_, err := NewMembershipGrantRevokeLogic(commerceMembershipSession(), commerceMembershipSvc(fake)).
			MembershipGrantRevoke(&req)
		if err == nil || !strings.Contains(err.Error(), c.field) {
			t.Fatalf("%s: 错误消息要点名 %s，实际 %v", c.name, c.field, err)
		}
		if fake.calls != before {
			t.Fatalf("%s: 已被网关拒的入参不该打到下游", c.name)
		}
	}
}

func TestCommerceMembershipPlanUpsert_MoneyAndSpecPassThroughUntouched(t *testing.T) {
	fake := &commerceMembershipFake{upsertPlan: commerceMembershipFullPlan()}
	req := commerceGoodPlanUpsert()
	req.PriceMinor = -1    // 服务拒（.api 已写明「负数由服务拒」）
	req.UnitCount = 0      // 服务按 1 个售卖单位处理
	req.PromPriceMinor = 0 // 0 = 无促销
	if _, err := NewMembershipPlanUpsertLogic(commerceMembershipSession(), commerceMembershipSvc(fake)).
		MembershipPlanUpsert(req); err != nil {
		t.Fatalf("规格合法性归服务: %v", err)
	}
	got := fake.upsertPlanReq
	if got.GetPriceMinor() != -1 || got.GetPromPriceMinor() != 0 || got.GetUnitCount() != 0 {
		t.Fatalf("金额/时长位必须原样 *_minor 下传，网关不做换算: %+v", got)
	}
	if got.GetReason() != "" {
		t.Fatalf("表单留空时网关不得代生成摘要（「写没写理由」是台账要能分辨的事实）: %q", got.GetReason())
	}
	if len(got.GetPlatforms()) != 3 ||
		got.GetPlatforms()[2] != membershiprpc.PlanPlatform_PLAN_PLATFORM_HARMONY {
		t.Fatalf("平台列表被改: %+v", got.GetPlatforms())
	}
	if got.GetVipType() != membershiprpc.VipType_VIP_TYPE_PREMIUM {
		t.Fatalf("档位没转成枚举: %+v", got)
	}

	// reason 位（2026-09-22 补进 .api）：非空则逐字下传，落 mb_plan_change_log.reason；
	// 它是可选位但不是「可编造」位——网关不加前后缀、不 trim。
	reasonFake := &commerceMembershipFake{upsertPlan: commerceMembershipFullPlan()}
	withReason := commerceGoodPlanUpsert()
	withReason.Reason = "  8 月促销调价  "
	if _, err := NewMembershipPlanUpsertLogic(commerceMembershipSession(), commerceMembershipSvc(reasonFake)).
		MembershipPlanUpsert(withReason); err != nil {
		t.Fatalf("带 reason 的改价要能打到服务: %v", err)
	}
	if got := reasonFake.upsertPlanReq; got.GetReason() != withReason.Reason {
		t.Fatalf("reason 必须原样下传（不 trim、不补摘要），实际 %q", got.GetReason())
	}
}

func TestCommerceMembershipEntitlementUpsert_EnabledFalseIsAnActionNotAMissingValue(t *testing.T) {
	fake := &commerceMembershipFake{upsertEntitleResult: commerceMembershipFullEntitlement()}
	req := commerceGoodEntitlementUpsert()
	req.Enabled = false // 关掉即全站该能力判否，必须原样下传
	if _, err := NewMembershipEntitlementUpsertLogic(commerceMembershipSession(), commerceMembershipSvc(fake)).
		MembershipEntitlementUpsert(req); err != nil {
		t.Fatalf("关闸是合法动作: %v", err)
	}
	if fake.upsertEntitleReq.GetEnabled() {
		t.Fatalf("enabled=false 被网关改成了 true: %+v", fake.upsertEntitleReq)
	}
	if fake.upsertEntitleReq.GetMinVipType() != membershiprpc.VipType_VIP_TYPE_PREMIUM {
		t.Fatalf("min_vip_type 未转枚举: %+v", fake.upsertEntitleReq)
	}
}

// TestCommerceMembershipProjection_NilReplyDoesNotPanic 覆盖「下游回了 nil 但没报错」：
// protobuf 的 Get* 访问器是 nil 安全的，投影必须落到空集合/零值而不是 panic。
// 这条只保证不崩——真实服务不会回 (nil, nil)，回了我也不加业务判断去补数。
func TestCommerceMembershipProjection_NilReplyDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	svcCtx := commerceMembershipSvc(&commerceMembershipFake{})
	if _, err := NewMembershipPlanListLogic(ctx, svcCtx).MembershipPlanList(&types.ParamMembershipPlanList{}); err != nil {
		t.Fatalf("nil reply: %v", err)
	}
	if _, err := NewMembershipEntitlementListLogic(ctx, svcCtx).
		MembershipEntitlementList(&types.ParamMembershipEntitlementList{}); err != nil {
		t.Fatalf("nil reply: %v", err)
	}
}

// --- 投影 diff 助手：字段名直接进失败消息，避免「少投影一位」只报两句不等的字符串 ---

func commerceMembershipPlanDiff(got types.MembershipPlanItem, want *membershiprpc.PlanInfo) []string {
	var diffs []string
	eq := func(name string, g, w any) {
		if g != w {
			diffs = append(diffs, fmt.Sprintf("%s: got %#v want %#v", name, g, w))
		}
	}
	eq("plan_id", got.PlanId, want.GetPlanId())
	eq("plan_code", got.PlanCode, want.GetPlanCode())
	eq("name", got.Name, want.GetName())
	eq("description", got.Description, want.GetDescription())
	eq("vip_type", got.VipType, int32(want.GetVipType()))
	eq("duration_days", got.DurationDays, want.GetDurationDays())
	eq("unit_count", got.UnitCount, want.GetUnitCount())
	eq("price_minor", got.PriceMinor, want.GetPriceMinor())
	eq("prom_price_minor", got.PromPriceMinor, want.GetPromPriceMinor())
	eq("currency", got.Currency, want.GetCurrency())
	eq("auto_renew_supported", got.AutoRenewSupported, want.GetAutoRenewSupported())
	eq("state", got.State, int32(want.GetState()))
	eq("version", got.Version, want.GetVersion())
	eq("ctime", got.Ctime, want.GetCtime())
	eq("mtime", got.Mtime, want.GetMtime())
	eq("created_by", got.CreatedBy, want.GetCreatedBy())
	eq("updated_by", got.UpdatedBy, want.GetUpdatedBy())
	if len(got.Platforms) != len(want.GetPlatforms()) {
		diffs = append(diffs, fmt.Sprintf("platforms: got %v want %v", got.Platforms, want.GetPlatforms()))
	} else {
		for i, p := range want.GetPlatforms() {
			eq(fmt.Sprintf("platforms[%d]", i), got.Platforms[i], int32(p))
		}
	}
	return diffs
}

func commerceMembershipMemberDiff(got types.MembershipMemberItem, want *membershiprpc.MembershipInfo) []string {
	var diffs []string
	eq := func(name string, g, w any) {
		if g != w {
			diffs = append(diffs, fmt.Sprintf("%s: got %#v want %#v", name, g, w))
		}
	}
	eq("mid", got.Mid, want.GetMid())
	eq("vip_type", got.VipType, int32(want.GetVipType()))
	eq("start_at", got.StartAt, want.GetStartAt())
	eq("expire_at", got.ExpireAt, want.GetExpireAt())
	eq("auto_renew", got.AutoRenew, want.GetAutoRenew())
	eq("auto_renew_channel", got.AutoRenewChannel, want.GetAutoRenewChannel())
	eq("auto_renew_signed_at", got.AutoRenewSignedAt, want.GetAutoRenewSignedAt())
	eq("source", got.Source, int32(want.GetSource()))
	eq("paid_month_count", got.PaidMonthCount, want.GetPaidMonthCount())
	eq("version", got.Version, want.GetVersion())
	eq("ctime", got.Ctime, want.GetCtime())
	eq("mtime", got.Mtime, want.GetMtime())
	return diffs
}

func commerceMembershipGrantDiff(got types.MembershipGrantItem, want *membershiprpc.GrantInfo) []string {
	var diffs []string
	eq := func(name string, g, w any) {
		if g != w {
			diffs = append(diffs, fmt.Sprintf("%s: got %#v want %#v", name, g, w))
		}
	}
	eq("grant_id", got.GrantId, want.GetGrantId())
	eq("mid", got.Mid, want.GetMid())
	eq("vip_type", got.VipType, int32(want.GetVipType()))
	eq("action", got.Action, want.GetAction())
	eq("delta_days", got.DeltaDays, want.GetDeltaDays())
	eq("plan_id", got.PlanId, want.GetPlanId())
	eq("source", got.Source, int32(want.GetSource()))
	eq("biz_order_no", got.BizOrderNo, want.GetBizOrderNo())
	eq("payment_no", got.PaymentNo, want.GetPaymentNo())
	eq("before_expire_at", got.BeforeExpireAt, want.GetBeforeExpireAt())
	eq("after_expire_at", got.AfterExpireAt, want.GetAfterExpireAt())
	eq("operator", got.Operator, want.GetOperator())
	eq("request_id", got.RequestId, want.GetRequestId())
	eq("reason", got.Reason, want.GetReason())
	eq("ctime", got.Ctime, want.GetCtime())
	return diffs
}

func commerceMembershipEntitlementDiff(got types.MembershipEntitlementItem, want *membershiprpc.EntitlementInfo) []string {
	var diffs []string
	eq := func(name string, g, w any) {
		if g != w {
			diffs = append(diffs, fmt.Sprintf("%s: got %#v want %#v", name, g, w))
		}
	}
	eq("code", got.Code, want.GetCode())
	eq("name", got.Name, want.GetName())
	eq("description", got.Description, want.GetDescription())
	eq("min_vip_type", got.MinVipType, int32(want.GetMinVipType()))
	eq("enabled", got.Enabled, want.GetEnabled())
	eq("version", got.Version, want.GetVersion())
	eq("ctime", got.Ctime, want.GetCtime())
	eq("mtime", got.Mtime, want.GetMtime())
	return diffs
}
