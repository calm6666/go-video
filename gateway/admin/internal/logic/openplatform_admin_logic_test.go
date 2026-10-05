package logic

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 open-platform 的口径（AGENTS.md §5/§6/§7）：
//   - rpc→types 投影逐字段不丢（changed/replayed/rejected/deleted/deliveries_suppressed/
//     payload_digest/last_error/remaining 这些审计与排障证据都在）；
//   - 15 条受保护入口拿不到会话即 fail-closed，且 operator_mid/caller_mid 必须 >0
//     （mid==0 在服务侧是「owner 自查，归属由网关校验」，后台不是 owner 也没做过那道校验）；
//   - 网关不用 admin_id 覆盖运营自报的 mid：两者不属同一编号空间（见 admin.api 文末缺口 1）；
//   - is_operator/operator 位由网关置 true，表单没有也不该有这一位；
//   - 形状门槛（必填主体、reason、幂等键、负数、倒置区间、enabled=false）一律在调用下游之前失败；
//   - 契约里的 0 值哨兵（全局层级、全部密钥、立即失效、当前窗口、默认页大小、不过滤、通配 api_code）
//     原样下传，网关不替调用方挑值；
//   - 未配置 OpenPlatformRPC 时十六口一律报错，绝不回空列表——那会把「下游没接」读成
//     「一个第三方应用都没接入」；
//   - 秘密与凭证面（client_secret/token_hint/回调 url/redirect_uris/reason 正文）永不进网关日志。
//
// 「状态机能否迁移、scope 是否要求逐次同意、高风险能否批量授予、配额 limit 合不合理、
// 重算区间是否过大、端点是否属于本应用、死信能否重放、页大小上限」全是 services/open-platform
// 的领域规则，网关不复算——这里断言的是「入参原样交给下游 + 下游结论原样回传」。
// 打桩方式与 spm/feature-store/cron 测试一致：内嵌生成的 client 接口 + 覆盖所需方法，
// 不建 gRPC 连接、不碰数据库。未覆盖的八个方法（RegisterApplication/RegisterWebhook/
// EnqueueWebhookEvent 与 OAuth 五法）一旦被调用会直接 panic（nil 接口方法），
// 这正是「三条口径下刻意不接的路由没有任何代码路径」的可执行证明。

var errOpenFakeDownstream = errors.New("open-platform downstream unavailable")

// openAdminFake 记录每次调用的入参，并按预置值返回响应或错误。
type openAdminFake struct {
	openplatformrpc.OpenPlatformClient

	err   error
	calls int

	getAppReq     *openplatformrpc.GetApplicationReq
	listAppsReq   *openplatformrpc.ListApplicationsReq
	updateAppReq  *openplatformrpc.UpdateApplicationReq
	rotateReq     *openplatformrpc.RotateApplicationSecretReq
	revokeSecReq  *openplatformrpc.RevokeApplicationSecretReq
	listScopesReq *openplatformrpc.ListScopesReq
	grantReq      *openplatformrpc.GrantApplicationScopesReq
	revokeAuthReq *openplatformrpc.RevokeAuthorizationReq
	policyListReq *openplatformrpc.ListQuotaPoliciesReq
	upsertReq     *openplatformrpc.UpsertQuotaPolicyReq
	usageListReq  *openplatformrpc.ListQuotaUsageReq
	recomputeReq  *openplatformrpc.RecomputeQuotaReq
	hookListReq   *openplatformrpc.ListWebhooksReq
	deleteReq     *openplatformrpc.DeleteWebhookReq
	deliveryReq   *openplatformrpc.ListWebhookDeliveriesReq
	retryReq      *openplatformrpc.RetryWebhookDeliveryReq
}

func (f *openAdminFake) GetApplication(_ context.Context, in *openplatformrpc.GetApplicationReq,
	_ ...grpc.CallOption) (*openplatformrpc.GetApplicationReply, error) {
	f.calls++
	f.getAppReq = in
	return &openplatformrpc.GetApplicationReply{App: openFixtureAppRPC()}, f.err
}

func (f *openAdminFake) ListApplications(_ context.Context, in *openplatformrpc.ListApplicationsReq,
	_ ...grpc.CallOption) (*openplatformrpc.ListApplicationsReply, error) {
	f.calls++
	f.listAppsReq = in
	return &openplatformrpc.ListApplicationsReply{
		List:       []*openplatformrpc.ApplicationInfo{openFixtureAppRPC()},
		NextCursor: "1699999999:5",
		HasMore:    true,
	}, f.err
}

func (f *openAdminFake) UpdateApplication(_ context.Context, in *openplatformrpc.UpdateApplicationReq,
	_ ...grpc.CallOption) (*openplatformrpc.UpdateApplicationReply, error) {
	f.calls++
	f.updateAppReq = in
	return &openplatformrpc.UpdateApplicationReply{App: openFixtureAppRPC(), Changed: true}, f.err
}

func (f *openAdminFake) RotateApplicationSecret(_ context.Context, in *openplatformrpc.RotateApplicationSecretReq,
	_ ...grpc.CallOption) (*openplatformrpc.RotateApplicationSecretReply, error) {
	f.calls++
	f.rotateReq = in
	return &openplatformrpc.RotateApplicationSecretReply{
		ClientSecret:       "cs_new_once_plaintext",
		SecretId:           22,
		OldSecretId:        21,
		OldSecretExpiresAt: 1700000600,
		RotatedAt:          1700000000,
	}, f.err
}

func (f *openAdminFake) RevokeApplicationSecret(_ context.Context, in *openplatformrpc.RevokeApplicationSecretReq,
	_ ...grpc.CallOption) (*openplatformrpc.RevokeApplicationSecretReply, error) {
	f.calls++
	f.revokeSecReq = in
	return &openplatformrpc.RevokeApplicationSecretReply{Revoked: 2, EffectiveAt: 1700000000}, f.err
}

func (f *openAdminFake) ListScopes(_ context.Context, in *openplatformrpc.ListScopesReq,
	_ ...grpc.CallOption) (*openplatformrpc.ListScopesReply, error) {
	f.calls++
	f.listScopesReq = in
	return &openplatformrpc.ListScopesReply{List: []*openplatformrpc.ScopeInfo{openFixtureScopeRPC()}}, f.err
}

func (f *openAdminFake) GrantApplicationScopes(_ context.Context, in *openplatformrpc.GrantApplicationScopesReq,
	_ ...grpc.CallOption) (*openplatformrpc.GrantApplicationScopesReply, error) {
	f.calls++
	f.grantReq = in
	return &openplatformrpc.GrantApplicationScopesReply{
		Granted:    []string{"video.read"},
		Revoked:    []string{"video.write"},
		Rejected:   []string{"unknown.scope"},
		AppVersion: 4,
		Replayed:   true,
	}, f.err
}

func (f *openAdminFake) RevokeAuthorization(_ context.Context, in *openplatformrpc.RevokeAuthorizationReq,
	_ ...grpc.CallOption) (*openplatformrpc.RevokeAuthorizationReply, error) {
	f.calls++
	f.revokeAuthReq = in
	return &openplatformrpc.RevokeAuthorizationReply{
		GrantsRevoked: 1, TokensRevoked: 3, EffectiveAt: 1700000000,
	}, f.err
}

func (f *openAdminFake) ListQuotaPolicies(_ context.Context, in *openplatformrpc.ListQuotaPoliciesReq,
	_ ...grpc.CallOption) (*openplatformrpc.ListQuotaPoliciesReply, error) {
	f.calls++
	f.policyListReq = in
	return &openplatformrpc.ListQuotaPoliciesReply{
		List:       []*openplatformrpc.QuotaPolicyInfo{openFixturePolicyRPC()},
		NextCursor: "cursor-2",
		HasMore:    false,
	}, f.err
}

func (f *openAdminFake) UpsertQuotaPolicy(_ context.Context, in *openplatformrpc.UpsertQuotaPolicyReq,
	_ ...grpc.CallOption) (*openplatformrpc.UpsertQuotaPolicyReply, error) {
	f.calls++
	f.upsertReq = in
	return &openplatformrpc.UpsertQuotaPolicyReply{PolicyId: 7, Created: false}, f.err
}

func (f *openAdminFake) ListQuotaUsage(_ context.Context, in *openplatformrpc.ListQuotaUsageReq,
	_ ...grpc.CallOption) (*openplatformrpc.ListQuotaUsageReply, error) {
	f.calls++
	f.usageListReq = in
	return &openplatformrpc.ListQuotaUsageReply{List: []*openplatformrpc.QuotaUsageInfo{openFixtureUsageRPC()}}, f.err
}

func (f *openAdminFake) RecomputeQuota(_ context.Context, in *openplatformrpc.RecomputeQuotaReq,
	_ ...grpc.CallOption) (*openplatformrpc.RecomputeQuotaReply, error) {
	f.calls++
	f.recomputeReq = in
	return &openplatformrpc.RecomputeQuotaReply{WindowsScanned: 120, WindowsFixed: 3, MaxDelta: 17}, f.err
}

func (f *openAdminFake) ListWebhooks(_ context.Context, in *openplatformrpc.ListWebhooksReq,
	_ ...grpc.CallOption) (*openplatformrpc.ListWebhooksReply, error) {
	f.calls++
	f.hookListReq = in
	return &openplatformrpc.ListWebhooksReply{List: []*openplatformrpc.WebhookEndpointInfo{openFixtureEndpointRPC()}}, f.err
}

func (f *openAdminFake) DeleteWebhook(_ context.Context, in *openplatformrpc.DeleteWebhookReq,
	_ ...grpc.CallOption) (*openplatformrpc.DeleteWebhookReply, error) {
	f.calls++
	f.deleteReq = in
	return &openplatformrpc.DeleteWebhookReply{Deleted: true, DeliveriesSuppressed: 5}, f.err
}

func (f *openAdminFake) ListWebhookDeliveries(_ context.Context, in *openplatformrpc.ListWebhookDeliveriesReq,
	_ ...grpc.CallOption) (*openplatformrpc.ListWebhookDeliveriesReply, error) {
	f.calls++
	f.deliveryReq = in
	return &openplatformrpc.ListWebhookDeliveriesReply{
		List:       []*openplatformrpc.WebhookDeliveryInfo{openFixtureDeliveryRPC()},
		NextCursor: "cursor-3",
		HasMore:    true,
	}, f.err
}

func (f *openAdminFake) RetryWebhookDelivery(_ context.Context, in *openplatformrpc.RetryWebhookDeliveryReq,
	_ ...grpc.CallOption) (*openplatformrpc.RetryWebhookDeliveryReply, error) {
	f.calls++
	f.retryReq = in
	return &openplatformrpc.RetryWebhookDeliveryReply{
		DeliveryId: 11, State: openplatformrpc.WebhookDeliveryState_WEBHOOK_DELIVERY_STATE_PENDING,
		NextRetryAt: 1700000060, Replayed: true,
	}, f.err
}

// --- 测试夹具 ---

func openAdminSvc(fake openplatformrpc.OpenPlatformClient) *svc.ServiceContext {
	return &svc.ServiceContext{OpenPlatform: fake}
}

// openSessionCtx 模拟 AdminPermission 中间件已解析出会话身份（admin_id=77）的请求上下文。
func openSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"open_platform_ops"},
	})
}

func openFixtureAppRPC() *openplatformrpc.ApplicationInfo {
	return &openplatformrpc.ApplicationInfo{
		AppId:           5,
		AppKey:          "cli_8f2a91",
		Name:            "示例开放应用",
		Description:     "第三方投稿客户端",
		OwnerMid:        900,
		Status:          openplatformrpc.AppStatus_APP_STATUS_ACTIVE,
		RedirectUris:    []string{"https://dev.example.com/cb"},
		Scopes:          []string{"video.read", "video.publish"},
		SecretState:     openplatformrpc.SecretState_SECRET_STATE_CONFIGURED,
		SecretRotatedAt: 1699000000,
		Version:         4,
		Ctime:           1690000000,
		Mtime:           1699999999,
		OfflineAt:       0,
	}
}

func openFixtureScopeRPC() *openplatformrpc.ScopeInfo {
	return &openplatformrpc.ScopeInfo{
		Scope:               "video.publish",
		DisplayName:         "发布投稿",
		Access:              openplatformrpc.ScopeAccess_SCOPE_ACCESS_WRITE,
		RiskLevel:           openplatformrpc.ScopeRiskLevel_SCOPE_RISK_LEVEL_HIGH,
		RequiresUserConsent: true,
		Enabled:             true,
		Reason:              "目录文案：需要逐次确认",
		GrantedState:        2,
	}
}

func openFixturePolicyRPC() *openplatformrpc.QuotaPolicyInfo {
	return &openplatformrpc.QuotaPolicyInfo{
		PolicyId:      7,
		AppId:         5,
		ApiCode:       "video.publish",
		WindowSeconds: 60,
		Limit:         100,
		Enabled:       true,
		Operator:      900,
		Ctime:         1690000000,
		Mtime:         1699999999,
	}
}

func openFixtureUsageRPC() *openplatformrpc.QuotaUsageInfo {
	return &openplatformrpc.QuotaUsageInfo{
		AppId:         5,
		ApiCode:       "video.publish",
		WindowSeconds: 60,
		WindowStart:   1699999940,
		Used:          42,
		Limit:         100,
		Remaining:     58,
		UpdatedAt:     1699999999,
	}
}

func openFixtureEndpointRPC() *openplatformrpc.WebhookEndpointInfo {
	return &openplatformrpc.WebhookEndpointInfo{
		EndpointId:     3,
		AppId:          5,
		EventType:      openplatformrpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT,
		Url:            "https://hooks.example.com/publish-result",
		SignKeyVersion: 2,
		Enabled:        true,
		Description:    "投稿转码/审核结果回调",
		VerifiedAt:     1699000000,
		Ctime:          1690000000,
		Mtime:          1699999999,
	}
}

func openFixtureDeliveryRPC() *openplatformrpc.WebhookDeliveryInfo {
	return &openplatformrpc.WebhookDeliveryInfo{
		DeliveryId:     11,
		AppId:          5,
		EndpointId:     3,
		EventType:      openplatformrpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT,
		EventId:        "evt-0001",
		PayloadDigest:  "sha256:ab12cd34",
		State:          openplatformrpc.WebhookDeliveryState_WEBHOOK_DELIVERY_STATE_DEAD,
		Attempt:        6,
		MaxAttempts:    6,
		NextRetryAt:    0,
		LastStatusCode: 503,
		LastError:      "upstream timeout",
		Ctime:          1699999000,
		Mtime:          1699999999,
	}
}

// openRoute 是一条后台路由的可执行描述：请求体、是否受保护、契约里的运营主体字段名，
// 以及把成功响应拆成信封三字段的调用闭包。
type openRoute struct {
	name      string
	protected bool
	midField  string
	req       any
	call      func(ctx context.Context, s *svc.ServiceContext, req any) (int, string, int64, error)
}

// openAllRoutes 返回 16 条 open-platform 路由，请求体都是「通过全部门槛」的合法形状。
// 同一张表复用给「未配置下游」「缺会话」「缺主体」「信封」「标志位强制」四组断言，
// 新增路由时必须在这里登记，否则漏测的路由不会出现在任何一张表里。
func openAllRoutes() []openRoute {
	return []openRoute{
		{
			name: "application/get", protected: true, midField: "caller_mid",
			req: &types.ParamOpenApplicationGet{AppId: 5, CallerMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenApplicationGetLogic(ctx, s).OpenApplicationGet(r.(*types.ParamOpenApplicationGet))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "application/list", protected: true, midField: "",
			req: &types.ParamOpenApplicationList{Ps: 20},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenApplicationListLogic(ctx, s).OpenApplicationList(r.(*types.ParamOpenApplicationList))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "application/state", protected: true, midField: "operator_mid",
			req: &types.ParamOpenApplicationState{AppId: 5, TargetStatus: 3, Reason: "违规处置", OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenApplicationStateLogic(ctx, s).OpenApplicationState(r.(*types.ParamOpenApplicationState))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "secret/rotate", protected: true, midField: "operator_mid",
			req: &types.ParamOpenSecretRotate{AppId: 5, GraceSeconds: 600, Reason: "例行轮换", OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenSecretRotateLogic(ctx, s).OpenSecretRotate(r.(*types.ParamOpenSecretRotate))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "secret/revoke", protected: true, midField: "operator_mid",
			req: &types.ParamOpenSecretRevoke{AppId: 5, SecretId: 9, Reason: "疑似泄露", OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenSecretRevokeLogic(ctx, s).OpenSecretRevoke(r.(*types.ParamOpenSecretRevoke))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "scope/list", protected: false, midField: "",
			req: &types.ParamOpenScopeList{AppId: 5, OnlyEnabled: true},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenScopeListLogic(ctx, s).OpenScopeList(r.(*types.ParamOpenScopeList))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "scope/grant", protected: true, midField: "operator_mid",
			req: &types.ParamOpenScopeGrant{
				AppId: 5, Grant: []string{"video.read"}, Revoke: []string{"video.write"},
				Reason: "审批通过", IdempotencyKey: "idem-1", OperatorMid: 88,
			},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenScopeGrantLogic(ctx, s).OpenScopeGrant(r.(*types.ParamOpenScopeGrant))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "authorization/revoke", protected: true, midField: "operator_mid",
			req: &types.ParamOpenAuthorizationRevoke{Target: 2, AppId: 5, Mid: 900, Reason: "风控下线", OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenAuthorizationRevokeLogic(ctx, s).OpenAuthorizationRevoke(r.(*types.ParamOpenAuthorizationRevoke))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "quota/policy/list", protected: true, midField: "operator_mid",
			req: &types.ParamOpenQuotaPolicyList{AppId: 5, OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenQuotaPolicyListLogic(ctx, s).OpenQuotaPolicyList(r.(*types.ParamOpenQuotaPolicyList))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "quota/policy/upsert", protected: true, midField: "operator_mid",
			req: &types.ParamOpenQuotaPolicyUpsert{
				PolicyId: 7, AppId: 5, ApiCode: "video.publish",
				WindowSeconds: 60, Limit: 100, Enabled: true, OperatorMid: 88,
			},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenQuotaPolicyUpsertLogic(ctx, s).OpenQuotaPolicyUpsert(r.(*types.ParamOpenQuotaPolicyUpsert))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "quota/usage/list", protected: true, midField: "operator_mid",
			req: &types.ParamOpenQuotaUsageList{AppId: 5, OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenQuotaUsageListLogic(ctx, s).OpenQuotaUsageList(r.(*types.ParamOpenQuotaUsageList))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "quota/recompute", protected: true, midField: "operator_mid",
			req: &types.ParamOpenQuotaRecompute{
				AppId: 5, ApiCode: "*", WindowStart: 1699999000, WindowEnd: 1700000000,
				DryRun: true, OperatorMid: 88,
			},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenQuotaRecomputeLogic(ctx, s).OpenQuotaRecompute(r.(*types.ParamOpenQuotaRecompute))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "webhook/list", protected: true, midField: "operator_mid",
			req: &types.ParamOpenWebhookList{AppId: 5, OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenWebhookListLogic(ctx, s).OpenWebhookList(r.(*types.ParamOpenWebhookList))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "webhook/delete", protected: true, midField: "operator_mid",
			req: &types.ParamOpenWebhookDelete{AppId: 5, EndpointId: 3, Reason: "端点下线", OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenWebhookDeleteLogic(ctx, s).OpenWebhookDelete(r.(*types.ParamOpenWebhookDelete))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "webhook/delivery/list", protected: true, midField: "operator_mid",
			req: &types.ParamOpenWebhookDeliveryList{AppId: 5, OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenWebhookDeliveryListLogic(ctx, s).OpenWebhookDeliveryList(r.(*types.ParamOpenWebhookDeliveryList))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
		{
			name: "webhook/delivery/retry", protected: true, midField: "operator_mid",
			req: &types.ParamOpenWebhookDeliveryRetry{DeliveryId: 11, Reason: "上游已恢复", OperatorMid: 88},
			call: func(ctx context.Context, s *svc.ServiceContext, r any) (int, string, int64, error) {
				resp, err := NewOpenWebhookDeliveryRetryLogic(ctx, s).OpenWebhookDeliveryRetry(r.(*types.ParamOpenWebhookDeliveryRetry))
				return env3(resp, err, func() (int, string, int64) { return resp.Code, resp.Message, resp.TTL })
			},
		},
	}
}

// env3 把「resp + err」压成信封三件套；err 非空时 resp 必为 nil（不回假成功）。
func env3[T any](resp T, err error, want func() (int, string, int64)) (int, string, int64, error) {
	if err != nil {
		if v := reflect.ValueOf(resp); v.IsValid() && !v.IsNil() {
			return 0, "", 0, fmt.Errorf("出错时不应返回响应体，got %+v", resp)
		}
		return 0, "", 0, err
	}
	c, m, ttl := want()
	return c, m, ttl, nil
}

func TestOpenRoutesFailClosedWhenClientNotConfigured(t *testing.T) {
	for _, rt := range openAllRoutes() {
		_, _, _, err := rt.call(openSessionCtx(), &svc.ServiceContext{}, rt.req)
		if !errors.Is(err, errOpenPlatformNotConfigured) {
			t.Fatalf("%s 未配置下游时 err = %v, want errOpenPlatformNotConfigured", rt.name, err)
		}
	}
}

// 15 条受保护入口没有会话身份必须 fail-closed；唯一的例外是 scope/list（免中间件路由组）。
func TestOpenProtectedRoutesRequireSession(t *testing.T) {
	var protected, unprotected int
	for _, rt := range openAllRoutes() {
		fake := &openAdminFake{}
		_, _, _, err := rt.call(context.Background(), openAdminSvc(fake), rt.req)
		if !rt.protected {
			unprotected++
			if err != nil {
				t.Fatalf("%s 免鉴权读不该要求会话，got %v", rt.name, err)
			}
			continue
		}
		protected++
		if !errors.Is(err, errOpenPlatformSessionRequired) {
			t.Fatalf("%s 缺会话时 err = %v, want errOpenPlatformSessionRequired", rt.name, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s 缺会话时不得调用下游, calls=%d", rt.name, fake.calls)
		}
	}
	if protected != 15 || unprotected != 1 {
		t.Fatalf("受保护/免鉴权路由数 = %d/%d, want 15/1（与 routePermissions 表同口径）", protected, unprotected)
	}
}

// 请求体缺失时不得解引用。reflect.Zero 给出的是类型化 nil 指针（*types.ParamX(nil)），
// 断言到具体指针类型能成功，logic 里的 req == nil 分支才会真的走到。
func TestOpenRoutesRejectMissingRequestBody(t *testing.T) {
	for _, rt := range openAllRoutes() {
		fake := &openAdminFake{}
		nilReq := reflect.Zero(reflect.TypeOf(rt.req)).Interface()
		if _, _, _, err := rt.call(openSessionCtx(), openAdminSvc(fake), nilReq); !errors.Is(err, errOpenPlatformRequestMissing) {
			t.Fatalf("%s 空请求体 err = %v, want errOpenPlatformRequestMissing", rt.name, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s 空请求体时不得调用下游, calls=%d", rt.name, fake.calls)
		}
	}
}

// 契约把运营主体放在用户 mid 空间：mid<=0 在服务侧是「owner 自查」，后台不是 owner，
// 因此受保护入口一律要求 >0。
func TestOpenRoutesRequirePositiveOperatorMid(t *testing.T) {
	cases := []struct {
		name, midField string
		wantMsg        string
	}{}
	for _, rt := range openAllRoutes() {
		if rt.midField == "" {
			continue
		}
		cases = append(cases, struct{ name, midField, wantMsg string }{rt.name, rt.midField, "gateway/admin: " + rt.midField + " required"})
	}
	if len(cases) != 14 {
		t.Fatalf("带主体位的受保护路由数 = %d, want 14（15 条受保护 - application/list 无主体位）", len(cases))
	}
	for _, c := range cases {
		for _, mid := range []int64{0, -1} {
			rt := routeByName(t, c.name)
			req := cloneReq(rt.req)
			setMidField(t, req, c.midField, mid)
			fake := &openAdminFake{}
			_, _, _, err := rt.call(openSessionCtx(), openAdminSvc(fake), req)
			if err == nil || err.Error() != c.wantMsg {
				t.Fatalf("%s %s=%d err = %v, want %q", c.name, c.midField, mid, err, c.wantMsg)
			}
			if fake.calls != 0 {
				t.Fatalf("%s 主体非法时不得调用下游, calls=%d", c.name, fake.calls)
			}
		}
	}
}

func routeByName(t *testing.T, name string) openRoute {
	t.Helper()
	for _, rt := range openAllRoutes() {
		if rt.name == name {
			return rt
		}
	}
	t.Fatalf("路由表里没有 %s", name)
	return openRoute{}
}

func cloneReq(req any) any {
	v := reflect.ValueOf(req)
	out := reflect.New(v.Type().Elem())
	out.Elem().Set(v.Elem())
	return out.Interface()
}

func setMidField(t *testing.T, req any, field string, mid int64) {
	t.Helper()
	f := reflect.ValueOf(req).Elem().FieldByName(midGoField(field))
	if !f.IsValid() {
		t.Fatalf("%T 没有 %s 对应的字段", req, field)
	}
	f.SetInt(mid)
}

func midGoField(jsonField string) string {
	switch jsonField {
	case "caller_mid":
		return "CallerMid"
	case "operator_mid":
		return "OperatorMid"
	}
	return ""
}

// 网关不得用 admin_id 覆盖 operator_mid：admin_id 是 op_admin_user 主键，
// operator_mid 是用户 mid，两者不同编号空间（覆盖等于把处置记到无关用户头上）。
func TestOpenOperatorMidPassesThroughVerbatim(t *testing.T) {
	fake := &openAdminFake{}
	ctx := openSessionCtx() // admin_id=77
	svcCtx := openAdminSvc(fake)

	if _, err := NewOpenApplicationStateLogic(ctx, svcCtx).OpenApplicationState(
		&types.ParamOpenApplicationState{AppId: 5, TargetStatus: 3, Reason: "违规处置", OperatorMid: 88}); err != nil {
		t.Fatalf("state: %v", err)
	}
	if fake.updateAppReq.GetOperatorMid() != 88 {
		t.Fatalf("operator_mid = %d, want 88（不得被会话 admin_id=77 覆盖）", fake.updateAppReq.GetOperatorMid())
	}
	if _, err := NewOpenQuotaUsageListLogic(ctx, svcCtx).OpenQuotaUsageList(
		&types.ParamOpenQuotaUsageList{AppId: 5, OperatorMid: 88}); err != nil {
		t.Fatalf("usage/list: %v", err)
	}
	if fake.usageListReq.GetOperatorMid() != 88 {
		t.Fatalf("读侧 operator_mid = %d, want 88", fake.usageListReq.GetOperatorMid())
	}
}

// is_operator / operator 位由网关置 true：本组每条路由都由 AdminPermission 保护，
// 「这就是后台入口」是网关已知的事实；漏置会让运营读被服务解释成 owner 自查。
func TestOpenOperatorFlagsForcedTrue(t *testing.T) {
	fake := &openAdminFake{}
	ctx := openSessionCtx()
	svcCtx := openAdminSvc(fake)

	if _, err := NewOpenApplicationGetLogic(ctx, svcCtx).OpenApplicationGet(
		&types.ParamOpenApplicationGet{AppId: 5, CallerMid: 88}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !fake.getAppReq.GetOperator() {
		t.Fatalf("GetApplicationReq.operator 必须为 true")
	}
	if _, err := NewOpenApplicationListLogic(ctx, svcCtx).OpenApplicationList(
		&types.ParamOpenApplicationList{Status: 2}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !fake.listAppsReq.GetOperator() {
		t.Fatalf("ListApplicationsReq.operator 必须为 true")
	}
	// ListApplicationsReq 没有主体位（契约缺口 2）：网关不得把 admin_id 塞进 owner_mid
	// 伪装成「某个开发者的自查」。
	if fake.listAppsReq.GetOwnerMid() != 0 {
		t.Fatalf("ListApplicationsReq.owner_mid = %d, want 0（后台不是 owner）", fake.listAppsReq.GetOwnerMid())
	}
	if _, err := NewOpenApplicationStateLogic(ctx, svcCtx).OpenApplicationState(
		&types.ParamOpenApplicationState{AppId: 5, TargetStatus: 3, Reason: "x", OperatorMid: 88}); err != nil {
		t.Fatalf("state: %v", err)
	}
	if !fake.updateAppReq.GetIsOperator() {
		t.Fatalf("UpdateApplicationReq.is_operator 必须为 true")
	}
	if _, err := NewOpenSecretRotateLogic(ctx, svcCtx).OpenSecretRotate(
		&types.ParamOpenSecretRotate{AppId: 5, Reason: "x", OperatorMid: 88}); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !fake.rotateReq.GetIsOperator() {
		t.Fatalf("RotateApplicationSecretReq.is_operator 必须为 true")
	}
	if _, err := NewOpenSecretRevokeLogic(ctx, svcCtx).OpenSecretRevoke(
		&types.ParamOpenSecretRevoke{AppId: 5, Reason: "x", OperatorMid: 88}); err != nil {
		t.Fatalf("revoke secret: %v", err)
	}
	if !fake.revokeSecReq.GetIsOperator() {
		t.Fatalf("RevokeApplicationSecretReq.is_operator 必须为 true")
	}
	if _, err := NewOpenAuthorizationRevokeLogic(ctx, svcCtx).OpenAuthorizationRevoke(
		&types.ParamOpenAuthorizationRevoke{Target: 3, Mid: 900, Reason: "x", OperatorMid: 88}); err != nil {
		t.Fatalf("authorization/revoke: %v", err)
	}
	if !fake.revokeAuthReq.GetIsOperator() {
		t.Fatalf("RevokeAuthorizationReq.is_operator 必须为 true")
	}
	if _, err := NewOpenWebhookDeleteLogic(ctx, svcCtx).OpenWebhookDelete(
		&types.ParamOpenWebhookDelete{AppId: 5, EndpointId: 3, Reason: "x", OperatorMid: 88}); err != nil {
		t.Fatalf("webhook/delete: %v", err)
	}
	if !fake.deleteReq.GetIsOperator() {
		t.Fatalf("DeleteWebhookReq.is_operator 必须为 true")
	}
}

// 状态推进入口不得夹带资料字段：name/description/redirect_uris 属 owner 通道，
// 运营通道见到任一资料字段就被服务判 ErrOwnerRequired；expected_version 在状态通道不被采纳，
// 因此表单里根本没有这一位（暴露一个填了也不生效的乐观锁位是假契约）。
func TestOpenStateRouteSendsOnlyStatusFields(t *testing.T) {
	fake := &openAdminFake{}
	if _, err := NewOpenApplicationStateLogic(openSessionCtx(), openAdminSvc(fake)).OpenApplicationState(
		&types.ParamOpenApplicationState{AppId: 5, TargetStatus: 3, Reason: "违规处置", OperatorMid: 88}); err != nil {
		t.Fatalf("state: %v", err)
	}
	in := fake.updateAppReq
	if in.GetName() != "" || in.GetDescription() != "" || len(in.GetRedirectUris()) != 0 {
		t.Fatalf("状态通道不得携带资料字段: %+v", in)
	}
	if in.GetExpectedVersion() != 0 {
		t.Fatalf("状态通道不接受乐观锁基线, got %d", in.GetExpectedVersion())
	}
	if _, ok := reflect.TypeOf(types.ParamOpenApplicationState{}).FieldByName("ExpectedVersion"); ok {
		t.Fatalf("ParamOpenApplicationState 不应有 ExpectedVersion 位（服务在状态通道不采纳它）")
	}
}

// 形状门槛必须在调用下游之前失败：放过去只会换来一条指向「你没填的字段」的下游错误。
func TestOpenGateRejectsBeforeDownstream(t *testing.T) {
	cases := []struct {
		name string
		req  any
		want string
	}{
		{"application/get 双寻址位皆空", &types.ParamOpenApplicationGet{CallerMid: 88}, "app_id or app_key required"},
		{"application/get 只给空白 app_key", &types.ParamOpenApplicationGet{AppKey: "   ", CallerMid: 88}, "app_id or app_key required"},
		{"application/get 缺 caller_mid", &types.ParamOpenApplicationGet{AppId: 5}, "caller_mid required"},
		{"application/get 负 app_id", &types.ParamOpenApplicationGet{AppId: -5, CallerMid: 88}, "app_id must be >= 0"},
		{"application/list 负 status", &types.ParamOpenApplicationList{Status: -1}, "status must be >= 0"},
		{"application/list 负 ps", &types.ParamOpenApplicationList{Ps: -2}, "ps must be >= 0"},
		{"application/state 缺 app_id", &types.ParamOpenApplicationState{TargetStatus: 3, Reason: "x", OperatorMid: 88}, "app_id required"},
		{"application/state target_status=0", &types.ParamOpenApplicationState{AppId: 5, Reason: "x", OperatorMid: 88}, "target_status required (0 = UNSPECIFIED)"},
		{"application/state 负 target_status", &types.ParamOpenApplicationState{AppId: 5, TargetStatus: -1, Reason: "x", OperatorMid: 88}, "target_status required (0 = UNSPECIFIED)"},
		{"application/state 空白 reason", &types.ParamOpenApplicationState{AppId: 5, TargetStatus: 3, Reason: "  ", OperatorMid: 88}, "reason required"},
		{"secret/rotate 缺 app_id", &types.ParamOpenSecretRotate{Reason: "x", OperatorMid: 88}, "app_id required"},
		{"secret/rotate 负 grace_seconds", &types.ParamOpenSecretRotate{AppId: 5, GraceSeconds: -1, Reason: "x", OperatorMid: 88}, "grace_seconds must be >= 0"},
		{"secret/rotate 缺 reason", &types.ParamOpenSecretRotate{AppId: 5, OperatorMid: 88}, "reason required"},
		{"secret/revoke 负 secret_id", &types.ParamOpenSecretRevoke{AppId: 5, SecretId: -1, Reason: "x", OperatorMid: 88}, "secret_id must be >= 0"},
		{"scope/grant 缺 app_id", &types.ParamOpenScopeGrant{Reason: "x", IdempotencyKey: "k", OperatorMid: 88}, "app_id required"},
		{"scope/grant 缺幂等键", &types.ParamOpenScopeGrant{AppId: 5, Reason: "x", OperatorMid: 88}, "idempotency_key required"},
		{"authorization/revoke target=0", &types.ParamOpenAuthorizationRevoke{Mid: 900, Reason: "x", OperatorMid: 88}, "target required (0 = UNSPECIFIED)"},
		{"authorization/revoke 负 mid", &types.ParamOpenAuthorizationRevoke{Target: 3, Mid: -1, Reason: "x", OperatorMid: 88}, "mid must be >= 0"},
		{"authorization/revoke 负 token_id", &types.ParamOpenAuthorizationRevoke{Target: 1, AppId: 5, TokenId: -2, Reason: "x", OperatorMid: 88}, "token_id must be >= 0"},
		{"quota/policy/list 负 ps", &types.ParamOpenQuotaPolicyList{AppId: 5, Ps: -1, OperatorMid: 88}, "ps must be >= 0"},
		{"quota/policy/upsert enabled=false", &types.ParamOpenQuotaPolicyUpsert{AppId: 5, ApiCode: "*", WindowSeconds: 60, Limit: 100, OperatorMid: 88}, "cannot be disabled via upsert"},
		{"quota/policy/upsert 负 limit", &types.ParamOpenQuotaPolicyUpsert{AppId: 5, ApiCode: "*", WindowSeconds: 60, Limit: -1, Enabled: true, OperatorMid: 88}, "limit must be >= 0"},
		{"quota/policy/upsert 负 window_seconds", &types.ParamOpenQuotaPolicyUpsert{AppId: 5, ApiCode: "*", WindowSeconds: -1, Limit: 100, Enabled: true, OperatorMid: 88}, "window_seconds must be >= 0"},
		{"quota/usage/list 缺 app_id", &types.ParamOpenQuotaUsageList{OperatorMid: 88}, "app_id required"},
		{"quota/recompute app_id=0", &types.ParamOpenQuotaRecompute{WindowStart: 10, WindowEnd: 20, OperatorMid: 88}, "app_id required"},
		{"quota/recompute 区间倒置", &types.ParamOpenQuotaRecompute{AppId: 5, WindowStart: 20, WindowEnd: 10, OperatorMid: 88}, "window_end must be greater than window_start"},
		{"quota/recompute 区间相等", &types.ParamOpenQuotaRecompute{AppId: 5, WindowStart: 20, WindowEnd: 20, OperatorMid: 88}, "window_end must be greater than window_start"},
		{"quota/recompute 负 window_start", &types.ParamOpenQuotaRecompute{AppId: 5, WindowStart: -1, WindowEnd: 20, OperatorMid: 88}, "window_start must be >= 0"},
		{"webhook/list 缺 app_id", &types.ParamOpenWebhookList{OperatorMid: 88}, "app_id required"},
		{"webhook/delete 缺 endpoint_id", &types.ParamOpenWebhookDelete{AppId: 5, Reason: "x", OperatorMid: 88}, "endpoint_id required"},
		{"webhook/delivery/list 缺 app_id", &types.ParamOpenWebhookDeliveryList{OperatorMid: 88}, "app_id required"},
		{"webhook/delivery/list 负 state", &types.ParamOpenWebhookDeliveryList{AppId: 5, State: -1, OperatorMid: 88}, "state must be >= 0"},
		{"webhook/delivery/retry 缺 delivery_id", &types.ParamOpenWebhookDeliveryRetry{Reason: "x", OperatorMid: 88}, "delivery_id required"},
		{"webhook/delivery/retry 缺 reason", &types.ParamOpenWebhookDeliveryRetry{DeliveryId: 11, OperatorMid: 88}, "reason required"},
		{"scope/list 负 app_id", &types.ParamOpenScopeList{AppId: -1}, "app_id must be >= 0"},
	}
	for _, c := range cases {
		fake := &openAdminFake{}
		rt := routeByReqType(t, c.req)
		_, _, _, err := rt.call(openSessionCtx(), openAdminSvc(fake), c.req)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want 含 %q", c.name, err, c.want)
		}
		if fake.calls != 0 {
			t.Fatalf("%s: 门槛失败时不得调用下游, calls=%d", c.name, fake.calls)
		}
	}
}

func routeByReqType(t *testing.T, req any) openRoute {
	t.Helper()
	want := reflect.TypeOf(req)
	for _, rt := range openAllRoutes() {
		if reflect.TypeOf(rt.req) == want {
			return rt
		}
	}
	t.Fatalf("路由表里没有请求类型 %v", want)
	return openRoute{}
}

// 0 值哨兵原样下传：网关把它们换成「具体值」就等于替调用方做了一个它没做的决定。
func TestOpenSentinelsPassThroughUnchanged(t *testing.T) {
	fake := &openAdminFake{}
	ctx := openSessionCtx()
	svcCtx := openAdminSvc(fake)

	// app_id=0：配额规则的全局兜底层级。
	if _, err := NewOpenQuotaPolicyUpsertLogic(ctx, svcCtx).OpenQuotaPolicyUpsert(
		&types.ParamOpenQuotaPolicyUpsert{ApiCode: "*", WindowSeconds: 60, Limit: 100, Enabled: true, OperatorMid: 88}); err != nil {
		t.Fatalf("upsert 全局层级: %v", err)
	}
	if fake.upsertReq.GetAppId() != 0 || fake.upsertReq.GetPolicyId() != 0 {
		t.Fatalf("policy_id/app_id 哨兵被改写: %+v", fake.upsertReq)
	}
	if fake.upsertReq.GetApiCode() != "*" {
		t.Fatalf("api_code 通配取值被归一化: %q", fake.upsertReq.GetApiCode())
	}
	// enabled=true 原样带过（false 在上一张表里已被拒）。
	if !fake.upsertReq.GetEnabled() {
		t.Fatalf("enabled=true 丢失")
	}
	// policy_id=0 + created 由服务回读。
	if _, err := NewOpenQuotaPolicyListLogic(ctx, svcCtx).OpenQuotaPolicyList(
		&types.ParamOpenQuotaPolicyList{OperatorMid: 88}); err != nil {
		t.Fatalf("policy/list 全局: %v", err)
	}
	if fake.policyListReq.GetAppId() != 0 || fake.policyListReq.GetApiCode() != "" {
		t.Fatalf("policy/list 过滤位哨兵被改写: %+v", fake.policyListReq)
	}
	// ps=0 = 服务默认页大小、status=0 = 不过滤。
	if _, err := NewOpenApplicationListLogic(ctx, svcCtx).OpenApplicationList(
		&types.ParamOpenApplicationList{}); err != nil {
		t.Fatalf("application/list 默认位: %v", err)
	}
	if fake.listAppsReq.GetPs() != 0 || fake.listAppsReq.GetStatus() != openplatformrpc.AppStatus(0) {
		t.Fatalf("application/list 哨兵被改写: %+v", fake.listAppsReq)
	}
	// secret_id=0 = 吊销全部生效密钥；grace_seconds=0 = 旧密钥立即失效。
	if _, err := NewOpenSecretRevokeLogic(ctx, svcCtx).OpenSecretRevoke(
		&types.ParamOpenSecretRevoke{AppId: 5, Reason: "泄露", OperatorMid: 88}); err != nil {
		t.Fatalf("secret/revoke 全部: %v", err)
	}
	if fake.revokeSecReq.GetSecretId() != 0 {
		t.Fatalf("secret_id=0 被改写: %+v", fake.revokeSecReq)
	}
	if _, err := NewOpenSecretRotateLogic(ctx, svcCtx).OpenSecretRotate(
		&types.ParamOpenSecretRotate{AppId: 5, Reason: "泄露", OperatorMid: 88}); err != nil {
		t.Fatalf("secret/rotate 立即失效: %v", err)
	}
	if fake.rotateReq.GetGraceSeconds() != 0 {
		t.Fatalf("grace_seconds=0 被改写: %+v", fake.rotateReq)
	}
	// window_start=0 = 当前窗口。
	if _, err := NewOpenQuotaUsageListLogic(ctx, svcCtx).OpenQuotaUsageList(
		&types.ParamOpenQuotaUsageList{AppId: 5, OperatorMid: 88}); err != nil {
		t.Fatalf("usage/list 当前窗口: %v", err)
	}
	if fake.usageListReq.GetWindowStart() != 0 {
		t.Fatalf("window_start=0 被改写: %+v", fake.usageListReq)
	}
	// dry_run / include_disabled / ignore_dead 是 bool：不传即 false，网关不「顺手」置位。
	if _, err := NewOpenWebhookDeliveryRetryLogic(ctx, svcCtx).OpenWebhookDeliveryRetry(
		&types.ParamOpenWebhookDeliveryRetry{DeliveryId: 11, Reason: "重放", OperatorMid: 88}); err != nil {
		t.Fatalf("delivery/retry: %v", err)
	}
	if fake.retryReq.GetIgnoreDead() {
		t.Fatalf("ignore_dead 不得被网关置为 true: %+v", fake.retryReq)
	}
	if _, err := NewOpenWebhookListLogic(ctx, svcCtx).OpenWebhookList(
		&types.ParamOpenWebhookList{AppId: 5, OperatorMid: 88}); err != nil {
		t.Fatalf("webhook/list: %v", err)
	}
	if fake.hookListReq.GetIncludeDisabled() {
		t.Fatalf("include_disabled 不得被网关置为 true: %+v", fake.hookListReq)
	}
	// grant/revoke 两侧都空是「只查不授」的合法形状（服务判是否受理），网关不代填。
	if _, err := NewOpenScopeGrantLogic(ctx, svcCtx).OpenScopeGrant(
		&types.ParamOpenScopeGrant{AppId: 5, Reason: "核对", IdempotencyKey: "k", OperatorMid: 88}); err != nil {
		t.Fatalf("scope/grant 空集合: %v", err)
	}
	if len(fake.grantReq.GetGrant()) != 0 || len(fake.grantReq.GetRevoke()) != 0 {
		t.Fatalf("scope 集合被网关代填: %+v", fake.grantReq)
	}
	// 幂等键只做判空，不改写原值：改一个字符等于换一次执行权。
	if _, err := NewOpenScopeGrantLogic(ctx, svcCtx).OpenScopeGrant(
		&types.ParamOpenScopeGrant{AppId: 5, Reason: "授予", IdempotencyKey: " idem-with-spaces ", OperatorMid: 88}); err != nil {
		t.Fatalf("scope/grant 幂等键: %v", err)
	}
	if fake.grantReq.GetIdempotencyKey() != " idem-with-spaces " {
		t.Fatalf("幂等键被 trim 改写: %q", fake.grantReq.GetIdempotencyKey())
	}
	// app_id=0 + app_key 有值：网关不代挑寻址位（契约规定两者都给时服务以 app_id 为准）。
	if _, err := NewOpenApplicationGetLogic(ctx, svcCtx).OpenApplicationGet(
		&types.ParamOpenApplicationGet{AppKey: "cli_8f2a91", CallerMid: 88}); err != nil {
		t.Fatalf("application/get by key: %v", err)
	}
	if fake.getAppReq.GetAppId() != 0 || fake.getAppReq.GetAppKey() != "cli_8f2a91" {
		t.Fatalf("寻址位被网关改写: %+v", fake.getAppReq)
	}
}

// 只读/免鉴权口的 app_id=0 是「只要目录本身」，不是「没填」。
func TestOpenScopeListNeedsNoSessionAndKeepsZeroAppID(t *testing.T) {
	fake := &openAdminFake{}
	resp, err := NewOpenScopeListLogic(context.Background(), openAdminSvc(fake)).OpenScopeList(&types.ParamOpenScopeList{})
	if err != nil {
		t.Fatalf("scope/list: %v", err)
	}
	if fake.listScopesReq.GetAppId() != 0 {
		t.Fatalf("app_id=0 被改写: %+v", fake.listScopesReq)
	}
	if resp.Code != 0 || resp.Message != "ok" || resp.TTL != 0 {
		t.Fatalf("信封 = %+v, want 0/ok/0", resp)
	}
}

// 后台面全字段投影：裁掉任何一个证据位都等于让后台靠猜。
func TestOpenProjectionKeepsEveryField(t *testing.T) {
	if got, want := openAppToAPI(openFixtureAppRPC()), (types.OpenApplication{
		AppId: 5, AppKey: "cli_8f2a91", Name: "示例开放应用", Description: "第三方投稿客户端",
		OwnerMid: 900, Status: 2, RedirectUris: []string{"https://dev.example.com/cb"},
		Scopes: []string{"video.read", "video.publish"}, SecretState: 1, SecretRotatedAt: 1699000000,
		Version: 4, Ctime: 1690000000, Mtime: 1699999999,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ApplicationInfo 投影丢失或改名:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := openScopeToAPI(openFixtureScopeRPC()), (types.OpenScope{
		Scope: "video.publish", DisplayName: "发布投稿", Access: 2, RiskLevel: 3,
		RequiresUserConsent: true, Enabled: true, Reason: "目录文案：需要逐次确认", GrantedState: 2,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ScopeInfo 投影:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := openQuotaPolicyToAPI(openFixturePolicyRPC()), (types.OpenQuotaPolicy{
		PolicyId: 7, AppId: 5, ApiCode: "video.publish", WindowSeconds: 60, Limit: 100,
		Enabled: true, Operator: 900, Ctime: 1690000000, Mtime: 1699999999,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("QuotaPolicyInfo 投影:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := openQuotaUsageToAPI(openFixtureUsageRPC()), (types.OpenQuotaUsage{
		AppId: 5, ApiCode: "video.publish", WindowSeconds: 60, WindowStart: 1699999940,
		Used: 42, Limit: 100, Remaining: 58, UpdatedAt: 1699999999,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("QuotaUsageInfo 投影:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := openWebhookEndpointToAPI(openFixtureEndpointRPC()), (types.OpenWebhookEndpoint{
		EndpointId: 3, AppId: 5, EventType: 1, Url: "https://hooks.example.com/publish-result",
		SignKeyVersion: 2, Enabled: true, Description: "投稿转码/审核结果回调",
		VerifiedAt: 1699000000, Ctime: 1690000000, Mtime: 1699999999,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("WebhookEndpointInfo 投影:\ngot  %+v\nwant %+v", got, want)
	}
	if got, want := openWebhookDeliveryToAPI(openFixtureDeliveryRPC()), (types.OpenWebhookDelivery{
		DeliveryId: 11, AppId: 5, EndpointId: 3, EventType: 1, EventId: "evt-0001",
		PayloadDigest: "sha256:ab12cd34", State: 5, Attempt: 6, MaxAttempts: 6,
		LastStatusCode: 503, LastError: "upstream timeout",
		Ctime: 1699999000, Mtime: 1699999999,
	}); !reflect.DeepEqual(got, want) {
		t.Fatalf("WebhookDeliveryInfo 投影:\ngot  %+v\nwant %+v", got, want)
	}
}

// 列表一律非 nil：把 null 与 [] 区分给前端是多余的契约负担。
func TestOpenNilProjectionsReturnEmptySliceNotNil(t *testing.T) {
	app := openAppToAPI(nil)
	if app.RedirectUris == nil || app.Scopes == nil {
		t.Fatalf("空应用投影的回填位应是 []: %+v", app)
	}
	if got := openAppsToAPI(nil); got == nil {
		t.Fatalf("openAppsToAPI(nil) = nil")
	}
	if got := openScopesToAPI(nil); got == nil {
		t.Fatalf("openScopesToAPI(nil) = nil")
	}
	if got := openQuotaPoliciesToAPI(nil); got == nil {
		t.Fatalf("openQuotaPoliciesToAPI(nil) = nil")
	}
	if got := openQuotaUsagesToAPI(nil); got == nil {
		t.Fatalf("openQuotaUsagesToAPI(nil) = nil")
	}
	if got := openWebhookEndpointsToAPI(nil); got == nil {
		t.Fatalf("openWebhookEndpointsToAPI(nil) = nil")
	}
	if got := openWebhookDeliveriesToAPI(nil); got == nil {
		t.Fatalf("openWebhookDeliveriesToAPI(nil) = nil")
	}
	if got := openStrings(nil); got == nil {
		t.Fatalf("openStrings(nil) = nil")
	}
	// 逐元素投影必须容忍列表里的 nil 行（protobuf 允许 repeated 含空消息）。
	if got := openAppsToAPI([]*openplatformrpc.ApplicationInfo{nil}); len(got) != 1 {
		t.Fatalf("nil 行应仍占一位, got %d", len(got))
	}
}

// scope 授予的 rejected 不得折叠成整体失败，也不得伪装成已授予。
func TestOpenScopeGrantReportsPartialResultVerbatim(t *testing.T) {
	fake := &openAdminFake{}
	resp, err := NewOpenScopeGrantLogic(openSessionCtx(), openAdminSvc(fake)).OpenScopeGrant(
		&types.ParamOpenScopeGrant{AppId: 5, Grant: []string{"video.read"}, Revoke: []string{"video.write"},
			Reason: "审批通过", IdempotencyKey: "idem-1", OperatorMid: 88})
	if err != nil {
		t.Fatalf("scope/grant: %v", err)
	}
	d := resp.Data
	if len(d.Granted) != 1 || d.Granted[0] != "video.read" {
		t.Fatalf("granted = %v", d.Granted)
	}
	if len(d.Revoked) != 1 || d.Revoked[0] != "video.write" {
		t.Fatalf("revoked = %v", d.Revoked)
	}
	if len(d.Rejected) != 1 || d.Rejected[0] != "unknown.scope" {
		t.Fatalf("rejected 被折叠: %v", d.Rejected)
	}
	if d.AppVersion != 4 || !d.Replayed {
		t.Fatalf("版本与重放位丢失: %+v", d)
	}
}

// 一次性明文密钥只回给调用方：不回日志的口径由下面的源码闸兜底，这里锁住「原样回、不外传」。
func TestOpenRotateReturnsOncePlaintextVerbatim(t *testing.T) {
	fake := &openAdminFake{}
	resp, err := NewOpenSecretRotateLogic(openSessionCtx(), openAdminSvc(fake)).OpenSecretRotate(
		&types.ParamOpenSecretRotate{AppId: 5, GraceSeconds: 600, Reason: "泄露", OperatorMid: 88})
	if err != nil {
		t.Fatalf("secret/rotate: %v", err)
	}
	if resp.Data.ClientSecret != "cs_new_once_plaintext" {
		t.Fatalf("client_secret 被改写: %q", resp.Data.ClientSecret)
	}
	if resp.Data.SecretId != 22 || resp.Data.OldSecretId != 21 ||
		resp.Data.OldSecretExpiresAt != 1700000600 || resp.Data.RotatedAt != 1700000000 {
		t.Fatalf("轮换台账丢失: %+v", resp.Data)
	}
	if resp.TTL != 0 {
		t.Fatalf("一次性凭证的 ttl = %d, want 0（不得让任何中间层缓存）", resp.TTL)
	}
}

// 下游错误原样上抛：不折成空结果、不伪装成成功。
func TestOpenDownstreamErrorPropagatesVerbatim(t *testing.T) {
	fake := &openAdminFake{err: errOpenFakeDownstream}
	for _, rt := range openAllRoutes() {
		code, msg, _, err := rt.call(openSessionCtx(), openAdminSvc(fake), cloneReq(rt.req))
		if !errors.Is(err, errOpenFakeDownstream) {
			t.Fatalf("%s 下游错误被改写: %v", rt.name, err)
		}
		if code != 0 || msg != "" {
			t.Fatalf("%s 出错时不该有信封: code=%d message=%q", rt.name, code, msg)
		}
	}
}

// 四条台账读的 next_cursor/has_more 与游标位一并回传，供后台续翻。
func TestOpenListRoutesReturnCursorAndEnvelope(t *testing.T) {
	fake := &openAdminFake{}
	svcCtx := openAdminSvc(fake)
	ctx := openSessionCtx()

	apps, err := NewOpenApplicationListLogic(ctx, svcCtx).OpenApplicationList(&types.ParamOpenApplicationList{Cursor: "c1", Ps: 20})
	if err != nil {
		t.Fatalf("application/list: %v", err)
	}
	if apps.Data.NextCursor != "1699999999:5" || !apps.Data.HasMore || len(apps.Data.List) != 1 {
		t.Fatalf("application/list 投影: %+v", apps.Data)
	}
	if fake.listAppsReq.GetCursor() != "c1" {
		t.Fatalf("游标未透传: %q", fake.listAppsReq.GetCursor())
	}
	if apps.Code != 0 || apps.Message != "ok" || apps.TTL != 0 {
		t.Fatalf("信封 = %+v, want 0/ok/0", apps)
	}
	policies, err := NewOpenQuotaPolicyListLogic(ctx, svcCtx).OpenQuotaPolicyList(&types.ParamOpenQuotaPolicyList{OperatorMid: 88})
	if err != nil {
		t.Fatalf("quota/policy/list: %v", err)
	}
	if policies.Data.HasMore || policies.Data.NextCursor != "cursor-2" || len(policies.Data.List) != 1 {
		t.Fatalf("quota/policy/list 投影: %+v", policies.Data)
	}
	deliveries, err := NewOpenWebhookDeliveryListLogic(ctx, svcCtx).OpenWebhookDeliveryList(
		&types.ParamOpenWebhookDeliveryList{AppId: 5, EndpointId: 0, State: 0, OperatorMid: 88})
	if err != nil {
		t.Fatalf("webhook/delivery/list: %v", err)
	}
	if deliveries.Data.NextCursor != "cursor-3" || len(deliveries.Data.List) != 1 {
		t.Fatalf("投递台账投影: %+v", deliveries.Data)
	}
	if fake.deliveryReq.GetEndpointId() != 0 || fake.deliveryReq.GetState() != openplatformrpc.WebhookDeliveryState(0) {
		t.Fatalf("投递过滤哨兵被改写: %+v", fake.deliveryReq)
	}
	usage, err := NewOpenQuotaUsageListLogic(ctx, svcCtx).OpenQuotaUsageList(&types.ParamOpenQuotaUsageList{AppId: 5, OperatorMid: 88})
	if err != nil {
		t.Fatalf("quota/usage/list: %v", err)
	}
	if len(usage.Data.List) != 1 || usage.Data.List[0].Remaining != 58 {
		t.Fatalf("配额用量投影: %+v", usage.Data)
	}
	hooks, err := NewOpenWebhookListLogic(ctx, svcCtx).OpenWebhookList(&types.ParamOpenWebhookList{AppId: 5, OperatorMid: 88})
	if err != nil {
		t.Fatalf("webhook/list: %v", err)
	}
	if len(hooks.Data.List) != 1 || hooks.Data.List[0].VerifiedAt != 1699000000 {
		t.Fatalf("端点台账投影: %+v", hooks.Data)
	}
	scopes, err := NewOpenScopeListLogic(ctx, svcCtx).OpenScopeList(&types.ParamOpenScopeList{AppId: 5})
	if err != nil {
		t.Fatalf("scope/list: %v", err)
	}
	if len(scopes.Data.List) != 1 || !scopes.Data.List[0].RequiresUserConsent || scopes.Data.List[0].GrantedState != 2 {
		t.Fatalf("scope 目录投影: %+v", scopes.Data)
	}
}

// 写入口的成功信封固定 0/ok/0，且服务回读的布尔位不得被网关复算。
func TestOpenWriteRoutesEnvelopeAndServiceOwnedFlags(t *testing.T) {
	fake := &openAdminFake{}
	svcCtx := openAdminSvc(fake)
	ctx := openSessionCtx()

	state, err := NewOpenApplicationStateLogic(ctx, svcCtx).OpenApplicationState(
		&types.ParamOpenApplicationState{AppId: 5, TargetStatus: 3, Reason: "违规处置", OperatorMid: 88})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if state.Code != 0 || state.Message != "ok" || state.TTL != 0 {
		t.Fatalf("state 信封: %+v", state)
	}
	if !state.Data.Changed {
		t.Fatalf("changed 由服务判定，网关不得折叠: %+v", state.Data)
	}
	if state.Data.App.AppId != 5 || state.Data.App.Version != 4 {
		t.Fatalf("state 后应用投影: %+v", state.Data.App)
	}
	upsert, err := NewOpenQuotaPolicyUpsertLogic(ctx, svcCtx).OpenQuotaPolicyUpsert(
		&types.ParamOpenQuotaPolicyUpsert{PolicyId: 7, AppId: 5, ApiCode: "video.publish",
			WindowSeconds: 60, Limit: 100, Enabled: true, OperatorMid: 88})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if upsert.Data.PolicyId != 7 || upsert.Data.Created {
		t.Fatalf("created 由服务回读: %+v", upsert.Data)
	}
	del, err := NewOpenWebhookDeleteLogic(ctx, svcCtx).OpenWebhookDelete(
		&types.ParamOpenWebhookDelete{AppId: 5, EndpointId: 3, Reason: "端点下线", OperatorMid: 88})
	if err != nil {
		t.Fatalf("webhook/delete: %v", err)
	}
	// deleted=false 时服务照样回台账，网关不把它伪装成失败（这里桩回 true）。
	if !del.Data.Deleted || del.Data.DeliveriesSuppressed != 5 {
		t.Fatalf("端点删除台账: %+v", del.Data)
	}
	retry, err := NewOpenWebhookDeliveryRetryLogic(ctx, svcCtx).OpenWebhookDeliveryRetry(
		&types.ParamOpenWebhookDeliveryRetry{DeliveryId: 11, IgnoreDead: true, Reason: "上游恢复", OperatorMid: 88})
	if err != nil {
		t.Fatalf("delivery/retry: %v", err)
	}
	// 重放后的状态是服务给的真实值（这里 PENDING=1），不是「已入队」的猜测。
	if retry.Data.State != 1 || !retry.Data.Replayed || retry.Data.NextRetryAt != 1700000060 {
		t.Fatalf("重放结果投影: %+v", retry.Data)
	}
	revoke, err := NewOpenSecretRevokeLogic(ctx, svcCtx).OpenSecretRevoke(
		&types.ParamOpenSecretRevoke{AppId: 5, Reason: "泄露", OperatorMid: 88})
	if err != nil {
		t.Fatalf("secret/revoke: %v", err)
	}
	if revoke.Data.Revoked != 2 || revoke.Data.EffectiveAt != 1700000000 {
		t.Fatalf("吊销把数由服务回读: %+v", revoke.Data)
	}
	auth, err := NewOpenAuthorizationRevokeLogic(ctx, svcCtx).OpenAuthorizationRevoke(
		&types.ParamOpenAuthorizationRevoke{Target: 3, Mid: 900, Reason: "风控下线", OperatorMid: 88})
	if err != nil {
		t.Fatalf("authorization/revoke: %v", err)
	}
	if auth.Data.GrantsRevoked != 1 || auth.Data.TokensRevoked != 3 || auth.Data.EffectiveAt != 1700000000 {
		t.Fatalf("撤销位点投影: %+v", auth.Data)
	}
	rc, err := NewOpenQuotaRecomputeLogic(ctx, svcCtx).OpenQuotaRecompute(
		&types.ParamOpenQuotaRecompute{AppId: 5, ApiCode: "*", WindowStart: 1699999000,
			WindowEnd: 1700000000, DryRun: true, OperatorMid: 88})
	if err != nil {
		t.Fatalf("quota/recompute: %v", err)
	}
	if rc.Data.WindowsScanned != 120 || rc.Data.WindowsFixed != 3 || rc.Data.MaxDelta != 17 {
		t.Fatalf("重算差异投影: %+v", rc.Data)
	}
	if !fake.recomputeReq.GetDryRun() {
		t.Fatalf("dry_run=true 丢失：网关不得顺手写回")
	}
}

// openLogVerbs 是会把内容写进网关日志的调用名；源码闸只查这些调用的实参。
var openLogVerbs = map[string]bool{
	"Infof": true, "Errorf": true, "Info": true, "Error": true, "Errorv": true, "Infov": true,
	"Debugf": true, "Logf": true, "Slowf": true, "Alarmed": true, "Errorw": true, "Infow": true,
}

// openSecretSelectors 是永不进网关日志的字段（含 protobuf getter 形式）：
// client_secret/token_hint 是凭证，redirect_uris/url 是第三方接收面，
// reason 正文可能含用户标识与工单细节，scopes/grant/revoke 是「谁被授予了什么」的名单。
// 唯一允许的用法是 len()——「这一页给了几个 scope」不泄露名单本身，
// 而 openScopeGrantLogic 正是按这个口径打日志的。
var openSecretSelectors = map[string]bool{
	"ClientSecret": true, "GetClientSecret": true,
	"TokenHint": true, "GetTokenHint": true,
	"Url": true, "GetUrl": true,
	"RedirectUris": true, "GetRedirectUris": true,
	"Reason": true, "GetReason": true,
	"Scopes": true, "GetScopes": true,
	"Grant": true, "GetGrant": true,
	"Revoke": true, "GetRevoke": true,
}

// TestOpenLogsNeverCarryCredentials 是本域的秘密不出面闸门：
// 只要有人在 Infof/Errorf 的实参里引用了凭证字段（例如把 req.TokenHint 或 hook.Url 打进日志），
// 该测试立即失败——运行期断言抓不到日志内容（logx 默认写 stdout），因此这里查语法树。
func TestOpenLogsNeverCarryCredentials(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读取 logic 目录: %v", err)
	}
	var checked, logSites int
	fset := token.NewFileSet()
	for _, e := range files {
		name := e.Name()
		if !strings.HasPrefix(name, "open") || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("解析 %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !openLogVerbs[sel.Sel.Name] {
				return true
			}
			logSites++
			// 先收集本次日志调用里 len(...) 覆盖的区间，再判断凭证字段是否落在其外。
			var lenSpans []span
			ast.Inspect(call, func(inner ast.Node) bool {
				if c, ok := inner.(*ast.CallExpr); ok {
					if fn, ok := c.Fun.(*ast.Ident); ok && fn.Name == "len" {
						lenSpans = append(lenSpans, span{start: c.Args[0].Pos(), end: c.Args[0].End()})
					}
				}
				return true
			})
			ast.Inspect(call, func(inner ast.Node) bool {
				used, ok := inner.(*ast.SelectorExpr)
				if !ok || !openSecretSelectors[used.Sel.Name] {
					return true
				}
				for _, s := range lenSpans {
					if s.contains(used.Pos(), used.End()) {
						return true
					}
				}
				t.Errorf("%s 的日志实参引用了凭证字段 %s（只能打 len 或 ID）", name, used.Sel.Name)
				return true
			})
			return false
		})
	}
	if checked < 16 {
		t.Fatalf("源码闸只扫到 %d 个 open*.go 文件，路由被删了还是文件名变了？", checked)
	}
	// 防空转：每条路由的错误日志位都在，闸口才真的在看东西（16 个 Errorf 站点）。
	if logSites < 16 {
		t.Fatalf("源码闸只看到 %d 个日志调用站点，want >=16（每条路由至少一处错误日志）", logSites)
	}
}

type span struct{ start, end token.Pos }

func (s span) contains(from, to token.Pos) bool { return s.start <= from && to <= s.end }
