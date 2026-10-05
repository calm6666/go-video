// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// open-platform RPC ↔ 管理后台投影 + 入参门槛。
//
// 职责边界（AGENTS.md §5），与 conv_live.go 是同一套口径（本契约同样把操作者放在 mid 空间）：
//  1. 网关只做四件事：会话身份存在、运营主体 mid 非空、必填的 reason/幂等键非空、数值位不为负；
//     调用下游；逐字段投影。应用状态机能否迁移、乐观锁是否命中、scope 是否要求逐次同意、
//     高风险能否批量授予、配额 limit 合不合理、重算区间是否过大、端点是否属于本应用、
//     死信能否重放、页大小上限一律由 services/open-platform 判定，网关不复算，
//     也不把下游错误改写成看起来成功的空结果；
//  2. **不在此实现任何凭证逻辑**：OAuth 五法、RegisterApplication、RegisterWebhook、
//     EnqueueWebhookEvent 刻意不开后台路由，理由见 admin.api 的 open-platform 段头注释三条口径；
//  3. 0 值哨兵一律原样下传：app_id=0（配额规则的全局兜底层级）、secret_id=0（吊销全部生效密钥）、
//     grace_seconds=0（旧密钥立即失效）、window_start=0（当前窗口）、ps=0（服务默认页大小）、
//     policy_id=0（按唯一键新建）、api_code=""（该维度不过滤）、state=0（不按状态过滤）
//     都是契约里的合法语义，网关把它们换成具体值就等于替调用方做了一个它没做的决定。
//     **api_code="*" 是通配规则自身的取值**，不做归一化，否则「列出通配规则」表达不出来；
//  4. `is_operator` / `operator` 位**一律由网关置 true**（表单里没有这一位）：本组每一条路由都
//     由 AdminPermission 保护，「这就是后台入口」是网关已知的事实；漏置会让运营读被服务
//     解释成 owner 自查（归属由网关校验），而网关并没有做过那道归属校验；
//  5. 后台面**全字段**投影：changed/replayed/rejected/deleted/revoked/payload_digest/
//     last_error 这些正是审计与排障证据，裁掉就等于让后台靠猜；
//  6. 反向边界是秘密本身：client_secret（响应）、token_hint（入参）、回调 url、
//     redirect_uris 一律**不进网关日志**（§4 密钥不得提交、凭证面外泄即越权）；
//     日志只打路由、admin_id 与表单自报的 mid；
//  7. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// errOpenPlatformNotConfigured：未配置 OpenPlatformRPC 时本域路由一律返回它。
// 不退化成空目录——那会把「下游没接」读成「一个第三方应用都没有接入」。
var errOpenPlatformNotConfigured = errors.New("gateway/admin: open-platform service client not configured")

// errOpenPlatformRequestMissing：请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errOpenPlatformRequestMissing = errors.New("gateway/admin: request body required")

// errOpenPlatformSessionRequired：受 AdminPermission 保护的路由拿不到会话身份，
// 说明权限表/挂载漂移（本域 15 条受保护路由全部依赖这一位来确认「运营身份是后台给的，
// 不是请求体自封的」），一律 fail-closed。
var errOpenPlatformSessionRequired = errors.New("gateway/admin: admin session identity required")

// openOperatorGate 是本域所有受保护路由的统一门槛（读与写同档，理由见文件头第 4 条）：
//   - operator_mid / caller_mid 必须 > 0：服务把 mid==0 解释成「owner 自查，归属已由网关校验」
//     （ListWebhooks/ListQuotaUsage/ListWebhookDeliveries 的身份口径注释），而后台不是 owner、
//     也没有做过那道归属校验；ListQuotaPolicies 与所有写入口更是直接 requireOperator。
//   - 会话身份必须存在（AdminPermission 判定通过后才会挂上）。
//
// 网关**不用** admin_id 覆盖 mid：admin_id 是 op_admin_user 主键、operator_mid 是用户 mid，
// 两者不是同一编号空间，覆盖等于把处置记到无关用户头上（缺口见 admin.api 段头注释与
// gateway/admin/README.md）。两条主体都写进日志：既能追「谁点的按钮」也能对上台账落在谁身上。
func openOperatorGate(ctx context.Context, route, field string, mid int64) error {
	if err := requireOperator(field, mid); err != nil {
		return err
	}
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return errOpenPlatformSessionRequired
	}
	// 只打路由与两个 ID：token_hint、client_secret、回调地址、reason 正文都不进日志（§4）。
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d %s=%d", route, id.AdminID, field, mid)
	return nil
}

// errOpenAppSubjectRequired：GetApplicationReq 的 app_id / app_key 二选一都没给。
// 放过去下游会去查主键 0 的应用，后台因此看到一条「应用不存在」——那是错误结论，
// 真实缺陷是这一页少填了寻址位。
var errOpenAppSubjectRequired = errors.New("gateway/admin: app_id or app_key required")

// openAppSubject 只判「两个寻址位都为空」这一件事，不判优先级也不做二选一归一：
// 契约规定两者都给时服务以 app_id 为准（允许 app_key 覆盖 app_id 等于给「拿公开标识
// 探测他人应用」留口子），网关若自己挑一个就改变了这条优先级语义。
func openAppSubject(appID int64, appKey string) error {
	if err := openNonNeg("app_id", appID); err != nil {
		return err
	}
	if appID <= 0 && strings.TrimSpace(appKey) == "" {
		return errOpenAppSubjectRequired
	}
	return nil
}

// errOpenQuotaDisableUnsupported：enabled=false 在 UpsertQuotaPolicy 上没有可执行路径。
// 服务对 !enabled 失败关闭（停用要有问责原因，而这个 RPC 没有 reason 位），
// proto 也没有暴露 DisableQuotaPolicy。网关给这条说得清的错，而不是让调用方收到一句
// 指向它没填过的字段的「reason required」，更不把「关不掉」折算成成功。
var errOpenQuotaDisableUnsupported = errors.New("gateway/admin: open-platform quota policy cannot be disabled via upsert (no reason slot in this RPC)")

// openSessionGate 用于「契约里没有操作者位」的受保护路由（当前只有 /application/list：
// ListApplicationsReq 的运营分支只看 operator 布尔，没有任何主体位可读，见 admin.api 文末缺口 2）。
// 本域是后台入口这一点仍要靠会话确认，日志因此至少留下 admin_id；台账侧无法记录这一页是谁拉的，
// 是契约缺口而不是网关可以补齐的东西。
func openSessionGate(ctx context.Context, route string) error {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return errOpenPlatformSessionRequired
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d", route, id.AdminID)
	return nil
}

// openNonNeg 用于哨兵型数值位与时间戳：0 在本域普遍是合法语义（全局层级、全部密钥、
// 立即失效、当前窗口、默认页大小、不按状态过滤），负数没有任何对应语义，
// 透传只会多一次无意义往返。上限与超限行为一律由服务判定。
func openNonNeg(field string, v int64) error {
	if v < 0 {
		return errors.New("gateway/admin: " + field + " must be >= 0")
	}
	return nil
}

// openPositive 用于「写侧枚举位」与乐观锁版本：0 在 UpdateApplicationReq.target_status 上是
// AppStatus_UNSPECIFIED，而运营通道里 target==0 被服务判成 errTargetStatusRequired（资料改动
// 才是 owner 通道的语义）——放过去就是一条「什么都没改但看起来成功了」的处置。
// expected_version 同理：0 不是「任意版本」，applyProfile 把它判成并发冲突。
// 具体哪个值合法、能否迁移仍由服务判定。
func openPositive(field string, v int32) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required (0 = UNSPECIFIED)")
	}
	return nil
}

// openIDGate 挡住「没选主体」的 app_id/endpoint_id/delivery_id：0 在这些位上不是哨兵而是没填，
// 下游会去查主键 0 的行，回给后台一个「不存在」而不是「你少传了参数」。
func openIDGate(field string, v int64) error {
	if v <= 0 {
		return errors.New("gateway/admin: " + field + " required")
	}
	return nil
}

// --- rpc → 后台 types 投影 ---

func openAppToAPI(a *openplatformrpc.ApplicationInfo) types.OpenApplication {
	if a == nil {
		return types.OpenApplication{RedirectUris: []string{}, Scopes: []string{}}
	}
	// 列表位一律非 nil：密钥状态与派生列原样回，网关不折叠「未签发」与「全部已撤销」。
	uris := make([]string, 0, len(a.GetRedirectUris()))
	uris = append(uris, a.GetRedirectUris()...)
	scopes := make([]string, 0, len(a.GetScopes()))
	scopes = append(scopes, a.GetScopes()...)
	return types.OpenApplication{
		AppId:           a.GetAppId(),
		AppKey:          a.GetAppKey(),
		Name:            a.GetName(),
		Description:     a.GetDescription(),
		OwnerMid:        a.GetOwnerMid(),
		Status:          int32(a.GetStatus()),
		RedirectUris:    uris,
		Scopes:          scopes,
		SecretState:     int32(a.GetSecretState()),
		SecretRotatedAt: a.GetSecretRotatedAt(),
		Version:         a.GetVersion(),
		Ctime:           a.GetCtime(),
		Mtime:           a.GetMtime(),
		OfflineAt:       a.GetOfflineAt(),
	}
}

func openAppsToAPI(list []*openplatformrpc.ApplicationInfo) []types.OpenApplication {
	out := make([]types.OpenApplication, 0, len(list))
	for _, a := range list {
		out = append(out, openAppToAPI(a))
	}
	return out
}

func openScopeToAPI(s *openplatformrpc.ScopeInfo) types.OpenScope {
	if s == nil {
		return types.OpenScope{}
	}
	return types.OpenScope{
		Scope:               s.GetScope(),
		DisplayName:         s.GetDisplayName(),
		Access:              int32(s.GetAccess()),
		RiskLevel:           int32(s.GetRiskLevel()),
		RequiresUserConsent: s.GetRequiresUserConsent(),
		Enabled:             s.GetEnabled(),
		Reason:              s.GetReason(),
		GrantedState:        s.GetGrantedState(),
	}
}

func openScopesToAPI(list []*openplatformrpc.ScopeInfo) []types.OpenScope {
	out := make([]types.OpenScope, 0, len(list))
	for _, s := range list {
		out = append(out, openScopeToAPI(s))
	}
	return out
}

func openQuotaPolicyToAPI(p *openplatformrpc.QuotaPolicyInfo) types.OpenQuotaPolicy {
	if p == nil {
		return types.OpenQuotaPolicy{}
	}
	return types.OpenQuotaPolicy{
		PolicyId:      p.GetPolicyId(),
		AppId:         p.GetAppId(),
		ApiCode:       p.GetApiCode(),
		WindowSeconds: p.GetWindowSeconds(),
		Limit:         p.GetLimit(),
		Enabled:       p.GetEnabled(),
		// Operator 是「最后改这条规则的人的 mid」（服务侧 int64 列），不是本次操作者。
		Operator: p.GetOperator(),
		Ctime:    p.GetCtime(),
		Mtime:    p.GetMtime(),
	}
}

func openQuotaPoliciesToAPI(list []*openplatformrpc.QuotaPolicyInfo) []types.OpenQuotaPolicy {
	out := make([]types.OpenQuotaPolicy, 0, len(list))
	for _, p := range list {
		out = append(out, openQuotaPolicyToAPI(p))
	}
	return out
}

func openQuotaUsageToAPI(u *openplatformrpc.QuotaUsageInfo) types.OpenQuotaUsage {
	if u == nil {
		return types.OpenQuotaUsage{}
	}
	return types.OpenQuotaUsage{
		AppId:         u.GetAppId(),
		ApiCode:       u.GetApiCode(),
		WindowSeconds: u.GetWindowSeconds(),
		WindowStart:   u.GetWindowStart(),
		Used:          u.GetUsed(),
		Limit:         u.GetLimit(),
		Remaining:     u.GetRemaining(),
		UpdatedAt:     u.GetUpdatedAt(),
	}
}

func openQuotaUsagesToAPI(list []*openplatformrpc.QuotaUsageInfo) []types.OpenQuotaUsage {
	out := make([]types.OpenQuotaUsage, 0, len(list))
	for _, u := range list {
		out = append(out, openQuotaUsageToAPI(u))
	}
	return out
}

func openWebhookEndpointToAPI(e *openplatformrpc.WebhookEndpointInfo) types.OpenWebhookEndpoint {
	if e == nil {
		return types.OpenWebhookEndpoint{}
	}
	return types.OpenWebhookEndpoint{
		EndpointId:     e.GetEndpointId(),
		AppId:          e.GetAppId(),
		EventType:      int32(e.GetEventType()),
		Url:            e.GetUrl(),
		SignKeyVersion: e.GetSignKeyVersion(),
		Enabled:        e.GetEnabled(),
		Description:    e.GetDescription(),
		VerifiedAt:     e.GetVerifiedAt(),
		Ctime:          e.GetCtime(),
		Mtime:          e.GetMtime(),
	}
}

func openWebhookEndpointsToAPI(list []*openplatformrpc.WebhookEndpointInfo) []types.OpenWebhookEndpoint {
	out := make([]types.OpenWebhookEndpoint, 0, len(list))
	for _, e := range list {
		out = append(out, openWebhookEndpointToAPI(e))
	}
	return out
}

func openWebhookDeliveryToAPI(d *openplatformrpc.WebhookDeliveryInfo) types.OpenWebhookDelivery {
	if d == nil {
		return types.OpenWebhookDelivery{}
	}
	return types.OpenWebhookDelivery{
		DeliveryId:     d.GetDeliveryId(),
		AppId:          d.GetAppId(),
		EndpointId:     d.GetEndpointId(),
		EventType:      int32(d.GetEventType()),
		EventId:        d.GetEventId(),
		PayloadDigest:  d.GetPayloadDigest(),
		State:          int32(d.GetState()),
		Attempt:        d.GetAttempt(),
		MaxAttempts:    d.GetMaxAttempts(),
		NextRetryAt:    d.GetNextRetryAt(),
		LastStatusCode: d.GetLastStatusCode(),
		LastError:      d.GetLastError(),
		Ctime:          d.GetCtime(),
		Mtime:          d.GetMtime(),
	}
}

func openWebhookDeliveriesToAPI(list []*openplatformrpc.WebhookDeliveryInfo) []types.OpenWebhookDelivery {
	out := make([]types.OpenWebhookDelivery, 0, len(list))
	for _, d := range list {
		out = append(out, openWebhookDeliveryToAPI(d))
	}
	return out
}

// openStrings 复制 rpc 里的 repeated string（scope 授予结果等）：回非 nil，且不与下游缓冲区共享，
// 免得调用方在投影上 append 时改写到一个仍被 rpc 消息持有的切片。
func openStrings(list []string) []string {
	out := make([]string, 0, len(list))
	return append(out, list...)
}
