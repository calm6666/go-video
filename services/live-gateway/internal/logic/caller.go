package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/metadata"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
)

// gRPC metadata 调用方归因约定（本仓库首个落地者，见 README「契约缺口」）。
//
// 为什么必须有它：proto 里 BroadcastToRoom.sender_role / KickConnection.operator 都是**请求字段**，
// 客户端可自报，不能作为授权依据（AGENTS.md §6）。但仓库今天没有任何 caller 身份通道，
// 于是这里定义一个「由可信接入层注入、本服务只读」的最小约定：
//
//	x-gw-caller-attested: "true"   接入层已完成鉴权（gateway/admin、内部服务链路的显式声明）
//	x-gw-caller-role:     OPERATOR / SERVICE（其它值一律按未归因处理，避免越权声明）
//	x-gw-caller-mid:      123        发起动作的人类主体（主播踢人时用它问 live-room 归属）
//	x-gw-caller-service:  moderation 来源服务名，与 ForwardSystemEvent.source_service 比对
//
// 未带 attested 的 metadata 一律**不参与判定**，只写进审计字段：
// 自报的 header 和自报的请求体字段一样不可信，承认它等于把鉴权做成了「谁都会填」。
const (
	mdCallerAttested = "x-gw-caller-attested"
	mdCallerRole     = "x-gw-caller-role"
	mdCallerMid      = "x-gw-caller-mid"
	mdCallerService  = "x-gw-caller-service"
)

// caller 是从鉴权上下文解析出的调用方主体。
type caller struct {
	role     int32
	mid      int64
	service  string
	attested bool
}

// String 供审计与日志使用：只含主体标识，永不含票据、载荷与设备号（AGENTS.md §6）。
func (c caller) String() string {
	if !c.attested {
		return "caller:unattested"
	}
	if c.service != "" {
		return "caller:service:" + c.service
	}
	if c.mid > 0 {
		return "caller:mid:" + strconv.FormatInt(c.mid, 10)
	}
	return "caller:role:" + strconv.Itoa(int(c.role))
}

// isPrivileged 判定是否为可信的运营/内部服务主体。
func (c caller) isPrivileged() bool {
	return c.attested && (c.role == model.RoleOperator || c.role == model.RoleService)
}

// callerFrom 解析 metadata。缺失即 caller{attested:false}，不报错：
// 是否放行由各方法的门禁决定（读方法可按配置放开，处置类写方法一律拒绝未归因主体）。
func callerFrom(ctx context.Context) caller {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return caller{role: model.RoleViewer}
	}
	c := caller{role: model.RoleViewer}
	if v := firstMD(md, mdCallerAttested); strings.EqualFold(v, "true") {
		c.attested = true
	}
	if v := firstMD(md, mdCallerMid); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			c.mid = n
		}
	}
	c.service = firstMD(md, mdCallerService)
	switch strings.ToUpper(firstMD(md, mdCallerRole)) {
	case "OPERATOR":
		c.role = model.RoleOperator
	case "SERVICE":
		c.role = model.RoleService
	default:
		// 其它角色声明（含 VIEWER/ANCHOR）在本服务不构成「内部主体」，按未归因处理。
		c.role = model.RoleViewer
		c.attested = false
	}
	if !c.attested {
		// 未带 attested 时，mid/service 只作观测值，不作为授权输入。
		c.mid = 0
		c.role = model.RoleViewer
	}
	return c
}

func firstMD(md metadata.MD, key string) string {
	v := md.Get(key)
	if len(v) == 0 {
		return ""
	}
	return strings.TrimSpace(v[0])
}

// requireOperatorRead 运营面读门禁（ListRoomConnections / ListRoomRoutes / ListBroadcastLogs）。
// 默认 RequireAttestedOperator=false 时放行未归因主体，但返回值里的 unattested 让调用方把它写进日志，
// 便于「权限闭环完成后翻开关」时确认还有哪些调用没带 metadata。
func (g gate) requireOperatorRead(purpose string) (caller, error) {
	c := callerFrom(g.ctx)
	if c.isPrivileged() {
		return c, nil
	}
	if !g.cfg().RequireAttestedOperator {
		g.errorf("live-gateway: %s 以未归因主体放行（RequireAttestedOperator=false），接入 metadata 后必须收紧", purpose)
		return c, nil
	}
	return c, fmt.Errorf("%w: %s requires attested OPERATOR/SERVICE caller", model.ErrPermissionDenied, purpose)
}

// requireDispositionWrite 处置类写门禁（KickConnection / DrainRoomRoute / UpsertAccessQuota / RevokeReconnectTicket）。
// fail-closed：拿不到可信主体就不执行，配置开关只用于「明知风险仍要放开」的过渡环境。
func (g gate) requireDispositionWrite(purpose string) (caller, error) {
	c := callerFrom(g.ctx)
	if c.isPrivileged() {
		return c, nil
	}
	if g.cfg().AllowUnattestedOperatorWrites {
		g.errorf("live-gateway: %s 由未归因主体执行（AllowUnattestedOperatorWrites=true），审计里主体标识记为 unattested", purpose)
		return c, nil
	}
	return c, fmt.Errorf("%w: %s requires attested OPERATOR/SERVICE caller (%s)",
		model.ErrPermissionDenied, purpose, repository.ErrCallerUnattributed)
}

// requireInternalService 内部服务门禁（ForwardSystemEvent）：
// 必须是归因的 SERVICE 主体，且其 caller-service 在 TrustedSourceServices 白名单里。
// 白名单为空时不放行任何来源（fail-closed）：系统事件能驱动「开播/禁言/封停」，
// 让任意内网调用方自报来源等于给它一条伪造房间状态的通道。
func (g gate) requireInternalService(claimedSource string) (caller, error) {
	c := callerFrom(g.ctx)
	if !c.isPrivileged() {
		return c, fmt.Errorf("%w: only attested internal callers may forward system events", model.ErrPermissionDenied)
	}
	if c.role == model.RoleOperator {
		// 运营主体走的是人工处置，允许发系统事件，但来源记成运营自己而不是请求字段。
		return c, nil
	}
	allowed := g.cfg().TrustedSourceServices
	if len(allowed) == 0 {
		return c, fmt.Errorf("%w: TrustedSourceServices is empty, refusing source_service=%q",
			model.ErrPermissionDenied, safeIdent(claimedSource))
	}
	attested := c.service
	if attested == "" {
		return c, fmt.Errorf("%w: internal caller did not attest x-gw-caller-service", model.ErrPermissionDenied)
	}
	if !containsFold(allowed, attested) {
		return c, fmt.Errorf("%w: attested caller service %q not in TrustedSourceServices", model.ErrPermissionDenied, attested)
	}
	if claimedSource != "" && !strings.EqualFold(claimedSource, attested) {
		// 自报来源与鉴权归因不一致：以归因为准并拒绝，放过会造成审计里来源与实际发起者两个版本。
		return c, fmt.Errorf("%w: source_service=%q mismatches attested caller service %q",
			model.ErrPermissionDenied, safeIdent(claimedSource), attested)
	}
	return c, nil
}

// safeIdent 把调用方可控的标识串收敛成可进日志/错误文本的形状：
// 超长截断、换行与制表符替换成空格（日志注入是审计被污染的经典手法）。
func safeIdent(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "-"
	}
	v = strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(v)
	if len(v) > 48 {
		return v[:48] + "…"
	}
	return v
}

func containsFold(list []string, v string) bool {
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), v) {
			return true
		}
	}
	return false
}

// gate 承载各 logic 共用的上下文与配置访问：每个方法开头 `g := newGate(l.ctx, l.svcCtx, l.Logger)`，
// 于是门禁语句保持一行，而判定逻辑只有一份（两条路径各写一遍鉴权迟早漂移，这是本服务最怕的 bug 类别）。
type gate struct {
	ctx      context.Context
	svcCtx   *svc.ServiceContext
	log      logx.Logger
	maxIDLen int
}

func newGate(ctx context.Context, svcCtx *svc.ServiceContext, log logx.Logger) gate {
	maxIDLen := svcCtx.Config.LiveGateway.MaxIdLenBytes
	if maxIDLen <= 0 {
		maxIDLen = defaultMaxIDLenBytes
	}
	return gate{ctx: ctx, svcCtx: svcCtx, log: log, maxIDLen: maxIDLen}
}

// cfg 是配置快捷方式，避免门禁里出现长串字段路径。
func (g gate) cfg() config.LiveGatewayConf { return g.svcCtx.Config.LiveGateway }

func (g gate) errorf(format string, args ...any) {
	if g.log != nil {
		g.log.Errorf(format, args...)
	}
}

func (g gate) infof(format string, args ...any) {
	if g.log != nil {
		g.log.Infof(format, args...)
	}
}
