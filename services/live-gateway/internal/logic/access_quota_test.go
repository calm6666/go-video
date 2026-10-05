package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖配额这一对方法：写侧 UpsertAccessQuota、读侧 GetAccessQuota。
// 配额是「一个开关影响全平台接入松紧」的东西，所以风险恰好是两组相反的事故：
//
//  1. 写侧越权与盲写。配额决定接入数/QPS/TTL/载荷/游客准入，内部服务能改就等于绕过运营审批链，
//     未归因主体能改就等于把鉴权做成「谁都会填 metadata」。
//     本服务唯一的硬证据是「授权在读写之前」：门禁拒绝时必须一次都没碰存储。
//  2. 读侧把「存储故障」读成「没有配额」。resolveQuota 一旦在依赖不可用时降级成默认值，
//     「运营已把某房间降到 3 QPS」会被静默读成「按 200 放行」，限流判定与运营意图相反。
//
// 因此每个拒绝分支都配一条放行对照，每个「钉住现状」的断言都写明它红在哪一行。

// 六个存储探针：注入到对应依赖上，用来区分
// 「请求真的走到了这一步」与「在更早的门禁就被拒了」。
// 为什么用报错而不是调用计数：配额替身没有读写计数器（Create/Update 有，但三个读方法没有），
// 而「一读就报错」同时证明了两件事——没读、以及读了就会红。
var (
	errQuotaResolveProbe = errors.New("quota-resolve-probe")
	errQuotaFindOneProbe = errors.New("quota-findone-probe")
	errQuotaReplayProbe  = errors.New("quota-findbyrequest-probe")
	errQuotaCreateProbe  = errors.New("quota-create-probe")
	errQuotaUpdateProbe  = errors.New("quota-update-probe")
	errQuotaEpochProbe   = errors.New("quota-epoch-probe")
)

// assertQuotaStorageUntouched 的替代物：poisonQuotaReads 给用例一个开局基线——
// 配额表三个读入口都「一读就报错」，配合 assertQuotaNoDbWrite 就是
// 「请求在触库前就被挡住」的完整证据。
// 用法：`assertNotRead := poisonQuotaReads(e)`，拿到 err 后 `assertNotRead(t, err, "被拒")`。
//
// 为什么用报错而不是调用计数：fakeQuotas 只给 Create/Update 记了次数，
// 三个读方法（FindOne/FindByRequestID/Resolve）没有计数器，而「一读就报错」同时证明了两件事——
// 没读、以及读了就会红。
//
// 为什么「零调用」是本文件最重要的断言：配额表是全平台接入松紧的唯一事实源，
// 一旦未授权主体能触发一次读，它就能用 request_id / scope 组合探测
// 「哪一层已经有配置」「幂等键是否已被用过」——那是把写接口变成只读情报源。
func poisonQuotaReads(e *testEnv) func(t *testing.T, err error, why string) {
	e.Quotas.fail("Resolve", errQuotaResolveProbe)
	e.Quotas.fail("FindOne", errQuotaFindOneProbe)
	e.Quotas.fail("FindByRequestID", errQuotaReplayProbe)
	return func(t *testing.T, err error, why string) {
		t.Helper()
		if errors.Is(err, errQuotaResolveProbe) || errors.Is(err, errQuotaFindOneProbe) ||
			errors.Is(err, errQuotaReplayProbe) {
			t.Fatalf("%s：但请求已经读到配额表（%v）", why, err)
		}
	}
}

// mustBumpEpoch 直接顶一次配额代次，等价于 UpsertAccessQuota 成功后的失效动作。
// 用途：证明读缓存的键里真的编了 epoch（旁路改库不会自己顶代次）。
func mustBumpEpoch(t *testing.T, e *testEnv) {
	t.Helper()
	if _, err := e.Leases.BumpQuotaEpoch(context.Background()); err != nil {
		t.Fatalf("自增配额代次失败: %v", err)
	}
}

// quotaReq 造一份「只改一处就能判别」的配额写请求：字段全是合法值，
// 用例只翻它要证明的那一格（scope、version、ttl、identity……）。
func quotaReq(scope int32, scopeID int64, requestID string, version int64) *rpc.UpsertAccessQuotaReq {
	return &rpc.UpsertAccessQuotaReq{
		Quota: &rpc.AccessQuotaInfo{
			Scope:            rpc.QuotaScope(scope),
			ScopeId:          scopeID,
			ScopeKey:         "room-7001",
			MaxConnections:   10,
			BroadcastQps:     5,
			DanmakuQps:       2,
			LeaseTtlSeconds:  60,
			TicketTtlSeconds: 300,
			MaxPayloadBytes:  512,
			AllowGuest:       true,
		},
		ExpectedVersion: version,
		Operator:        "ops-zhang",
		RequestId:       requestID,
		TraceId:         "trace-quota-1",
	}
}

// assertQuotaNoDbWrite 断言「这一笔请求没有把任何配额改动落进库」。
//
// 为什么不用 fakeQuotas 的 creates/updates 计数器：那两个计数器统计的是「尝试」，
// 而注入写故障的分支本来就必须真的调一次 Create/Update 才拿得到那个错误 ——
// 用计数器会把「写失败所以没落库」误判成「写了半行」，正好丢掉本文件最想证明的东西。
// 表内容才是事实：这些用例的配额表开局为空，所以「无半写」等价于「行数仍为 0」。
func assertQuotaNoDbWrite(t *testing.T, e *testEnv) {
	t.Helper()
	if n := len(e.Quotas.rows); n != 0 {
		t.Fatalf("被拒/失败的请求不该在 live_gw_access_quota 留下任何行，实际 %d 行：%+v", n, e.Quotas.rows)
	}
	if n := e.Logs.inserts; n != 0 {
		t.Fatalf("配额写不该另写 live_gw_broadcast_log，实际 Insert %d 次", n)
	}
	if n := e.Leases.callCount("BumpQuotaEpoch"); n != 0 {
		t.Fatalf("被拒的请求不该自增配额代次（会让全平台缓存回源），实际 %d 次", n)
	}
}

// --- UpsertAccessQuota：参数门禁 ---

// 参数门禁必须全部在触库之前，且边界是「>上限才拒」。
//
// 为什么每条都要配对照：门禁写成 `>=` 或把 TTL 上限抄错一位，配额就悄悄少了一档；
// 反过来把 request_id 长度放宽，幂等键会被 MySQL 静默截断，
// 截断后的键与客户端记的不是同一个值 —— 重放打不中，于是同一笔写落两行。
func TestUpsertAccessQuotaParameterGateRejectsBeforeAnyStorageCall(t *testing.T) {
	longIdent := strings.Repeat("r", 65)
	okOperator := strings.Repeat("o", 64)
	okRequest := strings.Repeat("q", 64)

	cases := []struct {
		name    string
		mut     func(*rpc.UpsertAccessQuotaReq)
		want    error
		wantTxt string
	}{
		{"nil 请求", nil, model.ErrInvalidQuotaScope, "quota is required"},
		{"quota 缺失", func(r *rpc.UpsertAccessQuotaReq) { r.Quota = nil }, model.ErrInvalidQuotaScope, "quota is required"},
		{"operator 空", func(r *rpc.UpsertAccessQuotaReq) { r.Operator = "" }, model.ErrEmptyOperator, ""},
		{"operator 只有空格", func(r *rpc.UpsertAccessQuotaReq) { r.Operator = "   " }, model.ErrEmptyOperator, ""},
		{"operator 65 字节", func(r *rpc.UpsertAccessQuotaReq) { r.Operator = longIdent }, model.ErrEmptyOperator, "1..64 bytes"},
		{"operator 带换行（日志注入）", func(r *rpc.UpsertAccessQuotaReq) {
			r.Operator = "ops\nfake-audit-line"
		}, model.ErrEmptyOperator, "without newlines"},
		{"request_id 空", func(r *rpc.UpsertAccessQuotaReq) { r.RequestId = "" }, model.ErrEmptyRequestID, ""},
		{"request_id 含空格", func(r *rpc.UpsertAccessQuotaReq) { r.RequestId = "req 1" }, model.ErrEmptyRequestID, "without whitespace"},
		{"request_id 65 字节", func(r *rpc.UpsertAccessQuotaReq) { r.RequestId = longIdent }, model.ErrEmptyRequestID, "1..64 bytes"},
		{"expected_version 为负", func(r *rpc.UpsertAccessQuotaReq) { r.ExpectedVersion = -1 }, model.ErrVersionConflict, "must be >= 0"},
		{"scope 未设", func(r *rpc.UpsertAccessQuotaReq) { r.Quota.Scope = rpc.QuotaScope_QUOTA_SCOPE_UNSPECIFIED }, model.ErrInvalidQuotaScope, "scope=0"},
		{"scope 越界 5", func(r *rpc.UpsertAccessQuotaReq) { r.Quota.Scope = rpc.QuotaScope(5) }, model.ErrInvalidQuotaScope, "scope=5"},
		{"GLOBAL 却带 scope_id", func(r *rpc.UpsertAccessQuotaReq) {
			r.Quota.Scope = rpc.QuotaScope_QUOTA_SCOPE_GLOBAL
			r.Quota.ScopeId = 7
		}, model.ErrInvalidQuotaScope, "global scope needs scope_id=0"},
		{"ROOM 但 scope_id=0", func(r *rpc.UpsertAccessQuotaReq) { r.Quota.ScopeId = 0 }, model.ErrInvalidQuotaScope, "needs positive scope_id"},
		{"lease_ttl 超硬上限", func(r *rpc.UpsertAccessQuotaReq) { r.Quota.LeaseTtlSeconds = 301 }, model.ErrQuotaExceeded, "lease_ttl_seconds=301"},
		{"ticket_ttl 超硬上限", func(r *rpc.UpsertAccessQuotaReq) { r.Quota.TicketTtlSeconds = 601 }, model.ErrQuotaExceeded, "ticket_ttl_seconds=601"},
		{"载荷上限超进程天花板", func(r *rpc.UpsertAccessQuotaReq) { r.Quota.MaxPayloadBytes = 1025 }, model.ErrPayloadTooLarge, "max_payload_bytes=1025"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			assertNotRead := poisonQuotaReads(e)
			req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-gate", 0)
			if tc.mut == nil {
				req = nil
			} else {
				tc.mut(req)
			}
			got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("应返回 %v，实际 %v", tc.want, err)
			}
			if tc.wantTxt != "" && !strings.Contains(err.Error(), tc.wantTxt) {
				t.Fatalf("错误要指出越界的具体字段/取值，缺 %q：%v", tc.wantTxt, err)
			}
			if got != nil {
				t.Fatalf("参数被拒却回了半份配额：%+v", got)
			}
			assertNotRead(t, err, "参数被拒")
			assertQuotaNoDbWrite(t, e)
		})
	}

	// 边界另一侧：正好等于上限必须放行（证明守卫是 > 而不是 >=）。
	e := newTestEnv(t)
	req := quotaReq(model.QuotaScopeRoom, roomID, okRequest, 0)
	req.Operator = okOperator
	req.Quota.LeaseTtlSeconds = 300  // MaxLeaseTTLSeconds
	req.Quota.TicketTtlSeconds = 600 // MaxTicketTTLSeconds
	req.Quota.MaxPayloadBytes = 1024 // 配置里的进程天花板
	if got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req); err != nil {
		t.Fatalf("取上限的合法配额应被接受: %v", err)
	} else if got.GetLeaseTtlSeconds() != 300 || got.GetMaxPayloadBytes() != 1024 {
		t.Fatalf("回显与写入不符: %+v", got)
	}
}

// --- UpsertAccessQuota：授权 ---

// 配额写门禁比其它处置类写接口**多收紧一层**：只有归因的 OPERATOR 能改。
//
// SERVICE 过了 requireDispositionWrite（isPrivileged 含 SERVICE），但在这道_extra_ 守卫前必须被挡：
// 内部服务能改配额 = 绕过运营审批链，「 moderation 服务把自己房间的 QPS 调到 10 万」是真实事故形状。
// AllowUnattestedOperatorWrites 的逃生门在这里也不生效（未归因主体 role=VIEWER，不是 OPERATOR），
// 这是三条方向相反的断言凑成的一组对照。
func TestUpsertAccessQuotaOnlyAnAttestedOperatorMayChangeQuota(t *testing.T) {
	t.Run("归因 OPERATOR 放行", func(t *testing.T) {
		e := newTestEnv(t)
		if _, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-ok", 0)); err != nil {
			t.Fatalf("合法运营被拒: %v", err)
		}
		if e.Quotas.creates != 1 {
			t.Fatalf("放行时应有恰好一次落库，实际 Create %d 次", e.Quotas.creates)
		}
	})
	t.Run("归因 SERVICE 被拒", func(t *testing.T) {
		e := newTestEnv(t)
		assertNotRead := poisonQuotaReads(e)
		got, err := NewUpsertAccessQuotaLogic(ctxService(t, "moderation"), e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-svc", 0))
		if !errors.Is(err, model.ErrPermissionDenied) || got != nil {
			t.Fatalf("SERVICE 不该能改配额，实际 err=%v reply=%+v", err, got)
		}
		if !strings.Contains(err.Error(), "attested OPERATOR") {
			t.Fatalf("错误要写明需要哪种主体，实际 %v", err)
		}
		assertNotRead(t, err, "SERVICE 被拒")
		assertQuotaNoDbWrite(t, e)
	})
	t.Run("未归因客户端被拒", func(t *testing.T) {
		e := newTestEnv(t)
		assertNotRead := poisonQuotaReads(e)
		_, err := NewUpsertAccessQuotaLogic(ctxClient(t), e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-client", 0))
		if !errors.Is(err, model.ErrPermissionDenied) {
			t.Fatalf("未归因主体应被拒，实际 %v", err)
		}
		assertNotRead(t, err, "未归因主体被拒")
		assertQuotaNoDbWrite(t, e)
	})
	t.Run("逃生门也过不了 OPERATOR 守卫", func(t *testing.T) {
		e := newTestEnv(t)
		assertNotRead := poisonQuotaReads(e)
		e.apply(func(c *config.LiveGatewayConf) { c.AllowUnattestedOperatorWrites = true })
		_, err := NewUpsertAccessQuotaLogic(ctxClient(t), e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-escape", 0))
		if !errors.Is(err, model.ErrPermissionDenied) {
			t.Fatalf("AllowUnattestedOperatorWrites 不该放开配额写，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "caller:unattested") {
			t.Fatalf("拒绝理由要回显实际主体标识，实际 %v", err)
		}
		assertNotRead(t, err, "逃生门下被 OPERATOR 守卫拦住")
		assertQuotaNoDbWrite(t, e)
	})
	t.Run("自报 OPERATOR 角色但不带 attested 仍被拒", func(t *testing.T) {
		e := newTestEnv(t)
		assertNotRead := poisonQuotaReads(e)
		// callerFrom 把非 OPERATOR/SERVICE 的声明按未归因处理，而 OPERATOR 声明缺 attested 也不可信。
		_, err := NewUpsertAccessQuotaLogic(ctxAs(t, "", "OPERATOR", "2002", ""), e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-forged", 0))
		if !errors.Is(err, model.ErrPermissionDenied) {
			t.Fatalf("只声明角色不构成授权，实际 %v", err)
		}
		assertNotRead(t, err, "伪造角色被拒")
		assertQuotaNoDbWrite(t, e)
	})
}

// 落库的 updated_by 必须是归因主体，不是请求里的自报值（AGENTS.md §6 不信自报字段）。
//
// 同时钉住「回显的是解析后的生效配额」：写了 MaxConnections=10 但 BroadcastQps 留空时，
// 回显必须给出继承来的具体数字，否则运营会以为留空＝无限。
func TestUpsertAccessQuotaCreateStoresAttributedIdentityAndResolvedView(t *testing.T) {
	e := newTestEnv(t)
	// 上一层先给个 GLOBAL 基线：ROOM 层留空的字段应从它继承，而不是掉到配置默认。
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeGlobal, MaxConnections: 1000,
		BroadcastQps: 100, TicketTtlSeconds: 300, ScopeKey: "global"})

	req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-create", 0)
	req.Operator = "self-reported-boss" // 与归因主体不同的人名：落库必须是后者
	req.Quota.ScopeKey = "room-7001"
	req.Quota.DanmakuQps = 0      // 留空 = 继承
	req.Quota.MaxPayloadBytes = 0 // 留空 = 继承
	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
	if err != nil {
		t.Fatalf("新建配额失败: %v", err)
	}
	row := e.Quotas.find(model.QuotaScopeRoom, roomID)
	if row == nil {
		t.Fatalf("新建后本层没有配置行")
	}
	if row.UpdatedBy != "caller:mid:2002" {
		t.Fatalf("updated_by 必须是归因主体而不是自报 operator，实际 %q", row.UpdatedBy)
	}
	if strings.Contains(row.UpdatedBy, "self-reported") {
		t.Fatalf("自报值被当身份落库了: %q", row.UpdatedBy)
	}
	if row.RequestId != "req-quota-create" || row.TraceId != "trace-quota-1" {
		t.Fatalf("request_id/trace_id 要落库留痕（AGENTS.md §8）: %+v", row)
	}
	if row.Version != 1 || row.AllowGuest != 1 {
		t.Fatalf("新建行应 version=1、allow_guest=1: %+v", row)
	}
	// 回显 = 解析后的生效值：本层写的与继承来的都要展开成数字。
	if got.GetMaxConnections() != 10 || got.GetBroadcastQps() != 5 {
		t.Fatalf("本层写的字段必须原样生效，实际 %+v", got)
	}
	if got.GetTicketTtlSeconds() != 300 {
		t.Fatalf("留空的 ticket_ttl 应从 GLOBAL 层继承 300，实际 %d", got.GetTicketTtlSeconds())
	}
	if got.GetMaxPayloadBytes() != 1024 || got.GetDanmakuQps() != 5 {
		t.Fatalf("GLOBAL 没配的字段要落到进程默认（1024/5），实际 payload=%d danmaku=%d",
			got.GetMaxPayloadBytes(), got.GetDanmakuQps())
	}
	if got.GetVersion() != 1 || got.GetUpdatedBy() != "caller:mid:2002" || got.GetScopeKey() != "room-7001" {
		t.Fatalf("回显要带本层原始行的版本/修改者/可读键，实际 %+v", got)
	}
	// 读写同一投影：GetAccessQuota 与本次写回必须逐字段一致，否则两条路径迟早漂移。
	readBack, err := NewGetAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	if readBack.String() != got.String() {
		t.Fatalf("写侧回显与读侧结论不一致：\n写=%+v\n读=%+v", got, readBack)
	}
	if n := e.Logs.inserts; n != 0 {
		t.Fatalf("配额是配置变更不是处置动作，不该往广播审计表写行，实际 %d 行", n)
	}
}

// 幂等回放只认同一 (scope, scope_id)：
//   - 同一把 request_id + 同一作用域 ⇒ 只读回放，绝不第二次写（哪怕请求内容不同）；
//   - 同一把 request_id 换作用域 ⇒ 显式拒绝，不能把别处的键当作「已登记」。
//
// 为什么要紧：回放如果按「request_id 命中就照新内容写」，重试会改写别人的配置行；
// 如果换作用域复用也放行，客户端一处键名冲突就会静默吞掉另一处的写。
func TestUpsertAccessQuotaReplayIsScopedAndNeverWritesTwice(t *testing.T) {
	e := newTestEnv(t)
	first := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-replay", 0)
	first.Quota.MaxConnections = 10
	if _, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(first); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	before := e.Quotas.find(model.QuotaScopeRoom, roomID)

	// 同一把键、同一作用域，但内容被改过：必须回放旧结论，不写新值。
	retry := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-replay", 0)
	retry.Quota.MaxConnections = 99999
	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(retry)
	if err != nil {
		t.Fatalf("同键重放应成功返回首次结论: %v", err)
	}
	if e.Quotas.creates != 1 || e.Quotas.updates != 0 {
		t.Fatalf("重放不该再写库（Create %d / Update %d）", e.Quotas.creates, e.Quotas.updates)
	}
	if now := e.Quotas.find(model.QuotaScopeRoom, roomID); now.MaxConnections != before.MaxConnections {
		t.Fatalf("重放把已生效的值改写了: %d → %d", before.MaxConnections, now.MaxConnections)
	}
	if got.GetMaxConnections() != 10 {
		t.Fatalf("重放要回首次生效值 10，实际 %+v", got)
	}

	// 对照：换一把键就是新写（说明上面的「不写」来自幂等命中，不是「写路径整体坏了」）。
	other := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-fresh", 0)
	other.Quota.MaxConnections = 20
	if _, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(other); !errors.Is(err, model.ErrVersionConflict) {
		t.Fatalf("本层已有行时换键新建必须撞 uniq_scope，实际 %v", err)
	}

	// 同一把键换作用域：显式拒绝，且新作用域一行都没落。
	cross := quotaReq(model.QuotaScopeRoom, otherRoom, "req-quota-replay", 0)
	beforeRows := len(e.Quotas.rows)
	beforeCreates := e.Quotas.creates
	got2, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(cross)
	if !errors.Is(err, model.ErrRequestIdDuplicated) || got2 != nil {
		t.Fatalf("request_id 跨作用域复用必须拒绝，实际 err=%v reply=%+v", err, got2)
	}
	if !strings.Contains(err.Error(), "不能复用到") {
		t.Fatalf("拒绝理由要说清键登记在哪个作用域，实际 %v", err)
	}
	// 幂等回读在写之前，所以连一次 Create 尝试都不该发生（上面那条对照已经把
	// 「换键就会真的走到 Create」证明过了，这里的零尝试才有意义）。
	if len(e.Quotas.rows) != beforeRows || e.Quotas.creates != beforeCreates {
		t.Fatalf("跨作用域拒绝不该产生任何写：行数 %d→%d，Create %d→%d",
			beforeRows, len(e.Quotas.rows), beforeCreates, e.Quotas.creates)
	}
}

// 盲写覆盖（expected_version=0 但行已存在）必须回 ErrVersionConflict，
// 并把可执行结论写进错误文本：「更新必须带 expected_version」。
//
// 为什么单独一条：Create 走 uniq_scope 冲突，model 把「唯一作用域冲突」与「唯一 request_id 冲突」
// 压成同一个 ErrVersionConflict，实现靠回读区分。这里回读命中 ⇒ 是覆盖冲突。
func TestUpsertAccessQuotaCreateRefusesBlindOverwrite(t *testing.T) {
	e := newTestEnv(t)
	existing := e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 7, Version: 3})

	req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-blind", 0)
	req.Quota.MaxConnections = 50000
	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
	if !errors.Is(err, model.ErrVersionConflict) || got != nil {
		t.Fatalf("盲写必须撞版本冲突，实际 err=%v reply=%+v", err, got)
	}
	if !strings.Contains(err.Error(), "已有配置行 version=3") || !strings.Contains(err.Error(), "expected_version") {
		t.Fatalf("错误要给出可执行结论（带哪个 version 重来），实际 %v", err)
	}
	now := e.Quotas.find(model.QuotaScopeRoom, roomID)
	if now.MaxConnections != 7 || now.Version != 3 {
		t.Fatalf("冲突必须原样留着旧行，不能半写：%+v", now)
	}
	if n := e.Leases.callCount("BumpQuotaEpoch"); n != 0 {
		t.Fatalf("没写成就不该顶 epoch（顶了会让全平台白回源一次），实际 %d 次", n)
	}
	// 前提自检：预置行没按预期形状落库的话，上面的「冲突」其实是别的原因。
	if existing.Version != 3 || existing.MaxConnections != 7 {
		t.Fatalf("预置行形状不对，冲突用例前提不成立：%+v", existing)
	}
}

// 并发下 uniq_request_id 冲突也被 model 压成 ErrVersionConflict：回读发现「行不在」时，
// 真实含义是「同一把键已被另一笔请求提交」，必须回 ErrRequestIdDuplicated 而不是「已有配置行」。
//
// 为什么必须显式测：这两种含义给运营的下一步动作完全相反
// （前者回读首次结果即可，后者要带 expected_version 重试）。
// 注入缝：fake 的 Create 在真正撞 uniq_scope 前会先看 failOn，
// 用它返回裸 ErrVersionConflict 就能造出「冲突但行不存在」这个只在并发下存在的形状。
func TestUpsertAccessQuotaConcurrentRequestIdCollisionSaysReplay(t *testing.T) {
	e := newTestEnv(t)
	e.Quotas.fail("Create", model.ErrVersionConflict)

	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).
		UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-race", 0))
	if !errors.Is(err, model.ErrRequestIdDuplicated) || got != nil {
		t.Fatalf("冲突但本层无行时应判并发重放，实际 err=%v reply=%+v", err, got)
	}
	if strings.Contains(err.Error(), "已有配置行") {
		t.Fatalf("把重放说成覆盖冲突会误导运营去带 version 重试，实际 %v", err)
	}
	assertQuotaNoDbWrite(t, e)
}

// 更新必须「只改提交的那几格」：0 值按原行继承（model.Update 是整行覆盖写）。
//
// 这是配额写路径最容易被改坏的一条：少了 lgwMergeQuotaInheritance，
// 「只改弹幕 QPS」会把同层的 max_connections/lease_ttl 全写成 0，
// 而 0 在继承链里是「交给上一层」——等于顺手把这个房间的接入上限上移一层，
// 事故面从「一个字段」扩散到「整层配置」。
func TestUpsertAccessQuotaUpdateInheritsUnsetFields(t *testing.T) {
	e := newTestEnv(t)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 40, BroadcastQps: 30, DanmakuQps: 7,
		LeaseTtlSeconds: 100, TicketTtlSeconds: 200, MaxPayloadBytes: 512, AllowGuest: 1,
		Version: 2, UpdatedBy: "caller:mid:111", RequestId: "req-quota-seed"})

	req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-update", 2)
	req.Quota.MaxConnections = 0
	req.Quota.BroadcastQps = 0
	req.Quota.DanmakuQps = 1 // 这次只动这一格
	req.Quota.LeaseTtlSeconds = 0
	req.Quota.TicketTtlSeconds = 0
	req.Quota.MaxPayloadBytes = 0
	req.Quota.AllowGuest = false
	req.Quota.ScopeKey = ""
	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	row := e.Quotas.find(model.QuotaScopeRoom, roomID)
	if row.DanmakuQps != 1 {
		t.Fatalf("提交的字段没生效: %+v", row)
	}
	for _, f := range []struct {
		name string
		got  int32
		want int32
	}{
		{"max_connections", row.MaxConnections, 40},
		{"broadcast_qps", row.BroadcastQps, 30},
		{"lease_ttl_seconds", row.LeaseTtlSeconds, 100},
		{"ticket_ttl_seconds", row.TicketTtlSeconds, 200},
		{"max_payload_bytes", row.MaxPayloadBytes, 512},
	} {
		if f.got != f.want {
			t.Fatalf("只改一格时 %s 被写成了 %d（应为原行 %d，0=继承）", f.name, f.got, f.want)
		}
	}
	if row.AllowGuest != 1 || got.GetAllowGuest() != true {
		t.Fatalf("allow_guest 留空时也要继承原行的 1，实际 row=%d reply=%v", row.AllowGuest, got.GetAllowGuest())
	}
	if row.ScopeKey != "room-7001" {
		t.Fatalf("可读键 scope_key 留空时被清空，排障就只剩哈希了: %q", row.ScopeKey)
	}
	if row.Version != 3 || got.GetVersion() != 3 {
		t.Fatalf("条件更新要自增 version（2→3），实际 row=%d reply=%d", row.Version, got.GetVersion())
	}
	if row.UpdatedBy != "caller:mid:2002" || row.RequestId != "req-quota-update" {
		t.Fatalf("更新也要把归因主体与新 request_id 落库: %+v", row)
	}
	if e.Quotas.creates != 0 || e.Quotas.updates != 1 {
		t.Fatalf("expected_version>0 只能走 Update，实际 Create=%d Update=%d", e.Quotas.creates, e.Quotas.updates)
	}
	// 判别性对照：显式提交的值必须真的覆盖旧值（证明上面的继承不是「整行原样没动」）。
	full := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-update2", 3)
	full.Quota.MaxConnections = 11
	if _, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(full); err != nil {
		t.Fatalf("第二次更新失败: %v", err)
	}
	if row2 := e.Quotas.find(model.QuotaScopeRoom, roomID); row2.MaxConnections != 11 {
		t.Fatalf("显式提交的 max_connections 没被覆盖，实际 %d", row2.MaxConnections)
	}
}

// TODO(缺陷)：0 值只能表达「继承上一层」，所以「把某一格降回不限/把游客准入关掉」这个动作不可达。
//
// proto3 标量没有 present 位，logic 无法区分「没填」与「填了 0」，只能一律按继承合并。
// 影响最大的是 allow_guest：一旦某层放开游客准入，就没有任何请求能把关回去
// —— 只能 DBA 直接改库，而这正是「运营动作必须有可执行入口」的反例。
// 这里钉住**当前观察到的行为**（而不是我想要的行为）：两个动作都静默保持原值并回成功。
func TestUpsertAccessQuotaCannotWriteZeroOrDisableAllowGuest(t *testing.T) {
	e := newTestEnv(t)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 40, BroadcastQps: 30, AllowGuest: 1, Version: 1})

	req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-unclose", 1)
	req.Quota.MaxConnections = 0 // 想表达「取消本层上限」
	req.Quota.AllowGuest = false // 想表达「不再允许游客」
	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
	if err != nil {
		t.Fatalf("当前实现不拒绝这类请求: %v", err)
	}
	row := e.Quotas.find(model.QuotaScopeRoom, roomID)
	if row.MaxConnections != 40 || row.AllowGuest != 1 {
		t.Fatalf("行为已变（现在能写 0 / 关 allow_guest 了），本用例需随之改写: %+v", row)
	}
	if got.GetMaxConnections() != 40 || !got.GetAllowGuest() {
		t.Fatalf("回显要与落库一致地保持原值: %+v", got)
	}
	// 对照：把值写成一个非零的更紧的限制是能生效的，说明「写不进去」只发生在 0 这一格。
	tighten := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-tighten", 2)
	tighten.Quota.MaxConnections = 1
	if _, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(tighten); err != nil {
		t.Fatalf("收紧上限应能写入: %v", err)
	}
	if row := e.Quotas.find(model.QuotaScopeRoom, roomID); row.MaxConnections != 1 {
		t.Fatalf("非零值没落库，实际 %d", row.MaxConnections)
	}
}

// 本层没有配置行时，expected_version>0 必须回 ErrQuotaNotFound 并说明「该走新建」。
//
// 为什么不能直接落成新建：那样就等于允许「带旧 version 盲写」，
// 而运营看到 version=1 会以为自己在改一个已存在的配置。
func TestUpsertAccessQuotaUpdateWithoutRowSaysCreateFirst(t *testing.T) {
	e := newTestEnv(t)
	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).
		UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-norow", 1))
	if !errors.Is(err, model.ErrQuotaNotFound) || got != nil {
		t.Fatalf("无行时带 version 更新必须报「未找到」，实际 err=%v reply=%+v", err, got)
	}
	if !strings.Contains(err.Error(), "expected_version=0") {
		t.Fatalf("错误要给出可执行结论（改用新建），实际 %v", err)
	}
	if e.Quotas.updates != 0 {
		t.Fatalf("发现无行时不该试探性更新，实际 Update %d 次", e.Quotas.updates)
	}
	assertQuotaNoDbWrite(t, e)
}

// CAS 打空时把两个版本号都写进错误文本：运营据此决定是重试还是改意图。
//
// 判别性：同一行先被别人推进过（version 3→4），本次带 3 ⇒ 必须报「当前 version=4，请求 expected_version=3」，
// 而不是 ErrQuotaNotFound（行在）也不是成功。
func TestUpsertAccessQuotaStaleVersionReportsBothVersions(t *testing.T) {
	e := newTestEnv(t)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 40, BroadcastQps: 30, Version: 4, UpdatedBy: "caller:mid:111"})

	req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-stale", 3)
	req.Quota.MaxConnections = 5
	got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
	if !errors.Is(err, model.ErrVersionConflict) || got != nil {
		t.Fatalf("version 过期必须冲突，实际 err=%v reply=%+v", err, got)
	}
	if !strings.Contains(err.Error(), "当前 version=4") || !strings.Contains(err.Error(), "expected_version=3") {
		t.Fatalf("冲突文本要同时给出当前值与请求值，实际 %v", err)
	}
	row := e.Quotas.find(model.QuotaScopeRoom, roomID)
	if row.MaxConnections != 40 || row.Version != 4 {
		t.Fatalf("冲突时旧行必须分毫不动: %+v", row)
	}
	if n := e.Leases.callCount("BumpQuotaEpoch"); n != 0 {
		t.Fatalf("没提交成功不该顶 epoch，实际 %d 次", n)
	}
}

// 依赖故障一律原样返回，且不留半写：
// Resolve/FindOne/FindByRequestID/Create/Update 五个注入点 + 存储未装配。
//
// 为什么逐个注入：配额写路径上有四个读、两个写，
// 「某个读失败被当成『没有冲突』继续写」会造成重复行或覆盖别人刚写的值。
func TestUpsertAccessQuotaStorageFailuresPropagateRawAndLeaveNoHalfWrittenRow(t *testing.T) {
	operator := ctxOperator(t, "2002")

	t.Run("存储未装配", func(t *testing.T) {
		e := newTestEnv(t)
		e.markStoreMissing()
		_, err := NewUpsertAccessQuotaLogic(operator, e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-nostore", 0))
		if !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("存储未装配必须显式失败而不是按默认放行，实际 %v", err)
		}
		assertQuotaNoDbWrite(t, e)
	})

	t.Run("幂等回读失败", func(t *testing.T) {
		e := newTestEnv(t)
		e.Quotas.fail("FindByRequestID", errQuotaReplayProbe)
		if _, err := NewUpsertAccessQuotaLogic(operator, e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-replayfail", 0)); !errors.Is(err, errQuotaReplayProbe) {
			t.Fatalf("幂等回读失败必须原样返回，实际 %v", err)
		}
		assertQuotaNoDbWrite(t, e)
	})

	t.Run("新建落库失败", func(t *testing.T) {
		e := newTestEnv(t)
		e.Quotas.fail("Create", errQuotaCreateProbe)
		if _, err := NewUpsertAccessQuotaLogic(operator, e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-createfail", 0)); !errors.Is(err, errQuotaCreateProbe) {
			t.Fatalf("Create 失败要原样返回（不能翻译成业务结论），实际 %v", err)
		}
		assertQuotaNoDbWrite(t, e)
	})

	t.Run("冲突后回读也失败", func(t *testing.T) {
		e := newTestEnv(t)
		e.Quotas.fail("Create", model.ErrVersionConflict) // 造出「冲突」
		e.Quotas.fail("FindOne", errQuotaFindOneProbe)    // 回读定位失败
		_, err := NewUpsertAccessQuotaLogic(operator, e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-readfail", 0))
		if !errors.Is(err, errQuotaFindOneProbe) {
			t.Fatalf("区分不了两种冲突含义时必须报底层故障，不能猜一个是幂等，实际 %v", err)
		}
		assertQuotaNoDbWrite(t, e)
	})

	t.Run("更新前读不到行", func(t *testing.T) {
		e := newTestEnv(t)
		e.Quotas.fail("FindOne", errQuotaFindOneProbe)
		_, err := NewUpsertAccessQuotaLogic(operator, e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-prefail", 1))
		if !errors.Is(err, errQuotaFindOneProbe) {
			t.Fatalf("读原行失败要原样返回，不能按『无行』走新建，实际 %v", err)
		}
		if e.Quotas.creates != 0 {
			t.Fatalf("读失败绝不能退化成盲写，Create %d 次", e.Quotas.creates)
		}
		assertQuotaNoDbWrite(t, e)
	})

	t.Run("更新落库失败", func(t *testing.T) {
		e := newTestEnv(t)
		seed := e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
			ScopeKey: "room-7001", MaxConnections: 40, BroadcastQps: 30, Version: 1})
		e.Quotas.fail("Update", errQuotaUpdateProbe)
		req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-updatefail", 1)
		req.Quota.MaxConnections = 9
		if _, err := NewUpsertAccessQuotaLogic(operator, e.Svc).UpsertAccessQuota(req); !errors.Is(err, errQuotaUpdateProbe) {
			t.Fatalf("Update 失败要原样返回，实际 %v", err)
		}
		if now := e.Quotas.find(model.QuotaScopeRoom, roomID); now.MaxConnections != seed.MaxConnections || now.Version != 1 {
			t.Fatalf("写失败不留半写: %+v", now)
		}
		if n := e.Leases.callCount("BumpQuotaEpoch"); n != 0 {
			t.Fatalf("写失败不该顶 epoch，实际 %d 次", n)
		}
	})

	t.Run("回显解析失败", func(t *testing.T) {
		e := newTestEnv(t)
		e.Quotas.fail("Resolve", errQuotaResolveProbe)
		_, err := NewUpsertAccessQuotaLogic(operator, e.Svc).
			UpsertAccessQuota(quotaReq(model.QuotaScopeRoom, roomID, "req-quota-viewfail", 0))
		if !errors.Is(err, errQuotaResolveProbe) {
			t.Fatalf("回显阶段解析失败也要原样返回，实际 %v", err)
		}
		// 写本身已提交（配额生效值与运营意图一致比「报错」更重要，见实现注释第 8 条）。
		if e.Quotas.creates != 1 {
			t.Fatalf("Create 应已提交一次，实际 %d", e.Quotas.creates)
		}
	})
}

// epoch 自增失败只告警：写已提交，回滚反而让 DB 与运营意图更不一致。
// 但代价必须被看见 —— 这里钉住「新值在 QuotaCacheTTLSeconds 内不可见，连写接口自己的回显都是旧值」。
//
// 对照（不注入故障）：epoch 顶动后解析缓存整体失效，回显与后续读都拿到新值。
// 这条对照是必须的，否则上面的断言只是「缓存从不出错」。
func TestUpsertAccessQuotaEpochBumpFailureCommitsButDelaysVisibility(t *testing.T) {
	run := func(t *testing.T, bumpFails bool) (writtenMax int32, repliedMax int32, readBackMax int32) {
		t.Helper()
		e := newTestEnv(t)
		e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
			ScopeKey: "room-7001", MaxConnections: 40, BroadcastQps: 30, Version: 1})
		// 先用读接口把生效值按当前 epoch 缓存住。
		first, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
			GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
		if err != nil || first.GetMaxConnections() != 40 {
			t.Fatalf("基线读失败或值不对: err=%v reply=%+v", err, first)
		}
		if bumpFails {
			e.Leases.failWith("BumpQuotaEpoch", errQuotaEpochProbe)
		}
		req := quotaReq(model.QuotaScopeRoom, roomID, "req-quota-epoch", 1)
		req.Quota.MaxConnections = 5
		got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
		if err != nil {
			t.Fatalf("epoch 自增失败不该让写失败: %v", err)
		}
		return e.Quotas.find(model.QuotaScopeRoom, roomID).MaxConnections, got.GetMaxConnections(),
			first.GetMaxConnections()
	}

	// 注入故障：落库 5，但回显仍是缓存里的 40。
	written, replied, _ := run(t, true)
	if written != 5 {
		t.Fatalf("写必须提交，实际库里的 max_connections=%d", written)
	}
	if replied != 40 {
		t.Fatalf("缺陷现场：写已提交但回显来自过期缓存（epoch 没顶动），实际回显 max_connections=%d", replied)
	}

	// 对照：epoch 正常顶动时，回显立刻是新值。
	written2, replied2, _ := run(t, false)
	if written2 != 5 || replied2 != 5 {
		t.Fatalf("epoch 生效后回显必须是刚写入的值，实际库=%d 回显=%d", written2, replied2)
	}
}

// NODE 层的 scope_key 只是可读标识：缺失时记告警但不拒绝。
//
// 为什么这是一条真实契约：降配动作（比如节点出事要立刻收紧）不该被一个展示字段卡住；
// 但反过来，给了 scope_key 就必须落库，否则排障时「以 scope_id 为准」的承诺是空的。
func TestUpsertAccessQuotaNodeScopeKeyIsOptionalButStored(t *testing.T) {
	t.Run("缺 scope_key 仍写入", func(t *testing.T) {
		e := newTestEnv(t)
		req := quotaReq(model.QuotaScopeNode, 8801, "req-quota-node-nokey", 0)
		req.Quota.ScopeKey = ""
		got, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req)
		if err != nil {
			t.Fatalf("NODE 层缺可读键不该被拒: %v", err)
		}
		row := e.Quotas.find(model.QuotaScopeNode, 8801)
		if row == nil || row.ScopeKey != "" {
			t.Fatalf("缺键时不该凭空造值: %+v", row)
		}
		if got.GetScope() != rpc.QuotaScope_QUOTA_SCOPE_NODE || got.GetScopeId() != 8801 {
			t.Fatalf("NODE 层回显要如实标出作用域: %+v", got)
		}
	})

	t.Run("给了 scope_key 就落库", func(t *testing.T) {
		e := newTestEnv(t)
		req := quotaReq(model.QuotaScopeNode, 8801, "req-quota-node-key", 0)
		req.Quota.ScopeKey = "  gw-node-a  " // 两端空白是运营手填的常见形状，落库前要收敛
		if _, err := NewUpsertAccessQuotaLogic(ctxOperator(t, "2002"), e.Svc).UpsertAccessQuota(req); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
		row := e.Quotas.find(model.QuotaScopeNode, 8801)
		if row.ScopeKey != "gw-node-a" {
			t.Fatalf("scope_key 要 Trim 后落库，实际 %q", row.ScopeKey)
		}
	})
}

// --- GetAccessQuota ---

// 读侧参数门禁：scope/scope_id 组合与写侧同一套约束。
//
// 为什么读路径也要校：不校的话「拿 scope_id=0 查 ROOM 层」会把 GLOBAL 行
// 当成某个房间的生效值回给运营，看起来完全正常，实际是另一个作用域的数字。
// 这里不需要任何调用方门禁（见后面的钉桩用例），所以探针就是唯一护栏。
func TestGetAccessQuotaParameterGateRejectsBeforeAnyStorageCall(t *testing.T) {
	cases := []struct {
		name    string
		req     *rpc.AccessQuotaReq
		wantTxt string
	}{
		{"scope 未设", &rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_UNSPECIFIED}, "scope=0"},
		{"scope 越界", &rpc.AccessQuotaReq{Scope: rpc.QuotaScope(5), ScopeId: roomID}, "scope=5"},
		{"GLOBAL 带 scope_id", &rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_GLOBAL, ScopeId: 7},
			"global scope needs scope_id=0"},
		{"ROOM scope_id=0", &rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM}, "needs positive scope_id"},
		{"USER scope_id 为负", &rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_USER, ScopeId: -1},
			"needs positive scope_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			assertNotRead := poisonQuotaReads(e)
			got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).GetAccessQuota(tc.req)
			if !errors.Is(err, model.ErrInvalidQuotaScope) {
				t.Fatalf("应返回 ErrInvalidQuotaScope，实际 %v", err)
			}
			assertNotRead(t, err, "参数被拒")
			if got != nil {
				t.Fatalf("参数被拒却回了半份配额：%+v", got)
			}
			if !strings.Contains(err.Error(), tc.wantTxt) {
				t.Fatalf("错误要说明组合约束，缺 %q：%v", tc.wantTxt, err)
			}
			assertNoWrites(t, e)
		})
	}
	if got, err := NewGetAccessQuotaLogic(ctxClient(t), newTestEnv(t).Svc).GetAccessQuota(nil); !errors.Is(err, model.ErrInvalidQuotaScope) || got != nil {
		t.Fatalf("nil 请求应显式拒绝，实际 err=%v reply=%+v", err, got)
	}
}

// 逐字段继承：本层写了的用本层，没写的用上一层，最后才到进程默认。
//
// 三条对照缺一不可：
//   - 换查 GLOBAL 作用域不能被房间的下调污染；
//   - 删掉房间行后该字段必须回到上一层的值（证明「覆盖」真的来自那一行）；
//   - USER 层的链里**没有 ROOM**（只有 USER→GLOBAL），所以「关一个房间的配额」
//     不会影响该房间里某个用户的生效值 —— 这条容易被误以为会，值得单独钉。
func TestGetAccessQuotaResolvesInheritancePerField(t *testing.T) {
	e := newTestEnv(t)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeGlobal, ScopeKey: "global",
		MaxConnections: 1000, BroadcastQps: 100, TicketTtlSeconds: 300})
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", BroadcastQps: 30, MaxPayloadBytes: 512})

	room, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
	if err != nil {
		t.Fatalf("读房间配额失败: %v", err)
	}
	if room.GetBroadcastQps() != 30 || room.GetMaxPayloadBytes() != 512 {
		t.Fatalf("本层写的字段没生效: %+v", room)
	}
	if room.GetMaxConnections() != 1000 || room.GetTicketTtlSeconds() != 300 {
		t.Fatalf("本层留空的字段应从 GLOBAL 继承（1000/300）: %+v", room)
	}
	if room.GetLeaseTtlSeconds() != 30 || room.GetDanmakuQps() != 5 || !room.GetAllowGuest() {
		t.Fatalf("GLOBAL 也没有的字段要落到进程默认（30/5/允许游客）: %+v", room)
	}
	// 回显里不能残留 0（0 是继承标记，不是「不限」）。
	for name, v := range map[string]int32{"max_connections": room.GetMaxConnections(),
		"broadcast_qps": room.GetBroadcastQps(), "danmaku_qps": room.GetDanmakuQps(),
		"lease_ttl_seconds": room.GetLeaseTtlSeconds(), "ticket_ttl_seconds": room.GetTicketTtlSeconds(),
		"max_payload_bytes": room.GetMaxPayloadBytes()} {
		if v == 0 {
			t.Fatalf("生效值里 %s 仍是 0，客户端就得自己再解一次继承链", name)
		}
	}

	glob, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_GLOBAL})
	if err != nil {
		t.Fatalf("读全局配额失败: %v", err)
	}
	if glob.GetBroadcastQps() != 100 {
		t.Fatalf("房间层的下调污染了 GLOBAL 视图: %+v", glob)
	}

	// 对照：删掉房间行后同一个字段必须回到上一层的值。
	e.Quotas.rows = removeQuotaRow(e.Quotas.rows, model.QuotaScopeRoom, roomID)
	mustBumpEpoch(t, e)
	after, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
	if err != nil {
		t.Fatalf("删行后读取失败: %v", err)
	}
	if after.GetBroadcastQps() != 100 || after.GetMaxPayloadBytes() != 1024 {
		t.Fatalf("删掉本层行后应回到 GLOBAL/默认（100/1024），实际 %+v", after)
	}
	if after.GetVersion() != 0 || after.GetUpdatedBy() != "" {
		t.Fatalf("本层无行时不该凭空造 version/updated_by: %+v", after)
	}

	// USER 层：链上没有 ROOM，只有 USER→GLOBAL。
	user, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_USER, ScopeId: mid})
	if err != nil {
		t.Fatalf("读用户配额失败: %v", err)
	}
	if user.GetBroadcastQps() != 100 {
		t.Fatalf("USER 层的继承链是 USER→GLOBAL，房间层不该参与，实际 %+v", user)
	}
}

// 没有任何配置行时用进程默认兜底，且这不报错（README：配置是兜底，数据以 DB 为准）。
//
// 判别性对照：MaxPayloadBytes 配置成 0 时兜底到代码里的 32768 ——
// 证明回显的数字真的来自配置链路，而不是替身里某个恰好等于它的常数。
func TestGetAccessQuotaFallsBackToConfigDefaults(t *testing.T) {
	e := newTestEnv(t)
	got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: otherRoom})
	if err != nil {
		t.Fatalf("无行时不该报错: %v", err)
	}
	if got.GetMaxConnections() != 50000 || got.GetBroadcastQps() != 200 || got.GetDanmakuQps() != 5 {
		t.Fatalf("应回落到配置的默认值: %+v", got)
	}
	if got.GetMaxPayloadBytes() != 1024 || got.GetLeaseTtlSeconds() != 30 || got.GetTicketTtlSeconds() != 120 {
		t.Fatalf("TTL/载荷默认值不对: %+v", got)
	}
	if !got.GetAllowGuest() {
		t.Fatalf("AllowGuestByDefault=true 应被展开成生效值: %+v", got)
	}

	e2 := newTestEnv(t)
	e2.apply(func(c *config.LiveGatewayConf) { c.MaxPayloadBytes = 0 })
	got2, err := NewGetAccessQuotaLogic(ctxClient(t), e2.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: otherRoom})
	if err != nil {
		t.Fatalf("配置为 0 时读取失败: %v", err)
	}
	if got2.GetMaxPayloadBytes() != 32768 {
		t.Fatalf("配置里的载荷上限为 0 时应兜底到 32768，实际 %d", got2.GetMaxPayloadBytes())
	}
}

// 存储故障绝不能被读成「没有配额」。
//
// 三条都是同一个方向的断言，但落在不同步骤上：
//  1. Resolve 报错 → 原样返回（实现里明确写了「绝不降级成默认值」）；
//  2. 取本层原始行的 FindOne 报错 → 原样返回，而不是回一份少了 version/updated_by 的裸生效值；
//  3. 存储未装配 → ErrStoreUnavailable，而不是按配置默认给一份「看起来正常」的答复。
//
// 为什么这三条重要：读接口一旦在依赖故障时返回默认值，
// 运营会看到「房间上限 5 万」而实际库里刚被人降到 10，随后的处置全部基于假数字。
func TestGetAccessQuotaStoreFailureNeverReadsAsNoQuota(t *testing.T) {
	t.Run("Resolve 失败", func(t *testing.T) {
		e := newTestEnv(t)
		e.Quotas.fail("Resolve", errQuotaResolveProbe)
		got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
			GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
		if !errors.Is(err, errQuotaResolveProbe) || got != nil {
			t.Fatalf("解析失败要原样报错且回 nil，实际 err=%v reply=%+v", err, got)
		}
	})

	t.Run("原始行读取失败", func(t *testing.T) {
		e := newTestEnv(t)
		e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
			ScopeKey: "room-7001", MaxConnections: 3, BroadcastQps: 2, Version: 5})
		e.Quotas.fail("FindOne", errQuotaFindOneProbe)
		got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
			GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
		if !errors.Is(err, errQuotaFindOneProbe) || got != nil {
			t.Fatalf("读不到原始行不能退化成裸生效值，实际 err=%v reply=%+v", err, got)
		}
	})

	t.Run("存储未装配", func(t *testing.T) {
		e := newTestEnv(t)
		e.markStoreMissing()
		got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
			GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
		if !errors.Is(err, repository.ErrStoreUnavailable) || got != nil {
			t.Fatalf("未装配存储要显式失败，实际 err=%v reply=%+v", err, got)
		}
	})

	t.Run("Redis epoch 读不到时回源 DB 而不是用默认值", func(t *testing.T) {
		e := newTestEnv(t)
		e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
			ScopeKey: "room-7001", MaxConnections: 3, Version: 1})
		e.Leases.failWith("QuotaEpoch", errQuotaEpochProbe)
		got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
			GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
		if err != nil {
			t.Fatalf("缓存不可用应该只是绕过缓存，不是失败: %v", err)
		}
		if got.GetMaxConnections() != 3 {
			t.Fatalf("拿不到 epoch 时必须回源 DB 的 3，不能给配置默认的 50000，实际 %d", got.GetMaxConnections())
		}
	})
}

// 读侧的缓存靠「配额代次 epoch」编进键：顶动 epoch 就整体失效，不需要 SCAN 删键。
//
// 判别性三格：
//   - 写完直接改库（模拟旁路改数据）后立刻读，仍是缓存里的旧值 ⇒ 缓存确实在生效；
//   - 顶一次 epoch 后读到新值 ⇒ 键里带 epoch，失效是全局的；
//   - QuotaCacheTTLSeconds=0 时一次缓存都不碰 ⇒ 上面的「旧值」不是 TTL 兜住的巧合。
func TestGetAccessQuotaCacheIsEpochScoped(t *testing.T) {
	e := newTestEnv(t)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 40, Version: 1})
	first := mustGetQuotaMax(t, e, 40)
	if first != 40 {
		t.Fatalf("基线值不对: %d", first)
	}
	// 旁路改库（不走 UpsertAccessQuota，所以不顶 epoch）。
	e.Quotas.rows[0].MaxConnections = 7
	if got := mustGetQuotaMax(t, e, 40); got != 40 {
		t.Fatalf("缓存没生效（本应读到旧值 40），实际 %d", got)
	}
	if n := e.Leases.callCount("CacheSet"); n != 1 {
		t.Fatalf("首次解析应写一次缓存，实际 CacheSet %d 次", n)
	}
	if n, want := e.Leases.callCount("CacheGet"), 2; n < want {
		t.Fatalf("第二次读应命中缓存，实际 CacheGet %d 次", n)
	}
	// 顶 epoch：等价于 UpsertAccessQuota 成功后的失效动作。
	mustBumpEpoch(t, e)
	if got := mustGetQuotaMax(t, e, 7); got != 7 {
		t.Fatalf("epoch 顶动后必须回源读到 7，实际 %d", got)
	}

	// 对照：TTL=0 时一次缓存都不碰，每次都回源。
	e2 := newTestEnv(t)
	e2.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 40, Version: 1})
	e2.apply(func(c *config.LiveGatewayConf) { c.QuotaCacheTTLSeconds = 0 })
	if got := mustGetQuotaMax(t, e2, 40); got != 40 {
		t.Fatalf("TTL=0 时读取失败: %d", got)
	}
	e2.Quotas.rows[0].MaxConnections = 7
	if got := mustGetQuotaMax(t, e2, 7); got != 7 {
		t.Fatalf("TTL=0 时第二次读必须看到刚改的值，实际 %d", got)
	}
	if n := e2.Leases.callCount("CacheGet") + e2.Leases.callCount("CacheSet"); n != 0 {
		t.Fatalf("TTL=0 时不该碰缓存，实际调用 %d 次", n)
	}
}

// 读接口必须真的是只读的：不写库、不顶 epoch、不清缓存。
//
// 为什么单独一条：读路径「顺手」写缓存/写计数/建默认行，是把只读取证变成事实源的起点；
// 一旦有人为了让「无行」的房间有个默认值而 INSERT 一行，全平台配额就有了隐式来源。
func TestGetAccessQuotaIsReadOnly(t *testing.T) {
	e := newTestEnv(t)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 40, Version: 1})
	if _, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID}); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if _, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_USER, ScopeId: mid}); err != nil {
		t.Fatalf("读未配置的作用域失败: %v", err)
	}
	assertNoWrites(t, e)
	if n := e.Quotas.creates + e.Quotas.updates; n != 0 {
		t.Fatalf("读路径写了配额行，实际 %d 次", n)
	}
	if n := e.Leases.callCount("BumpQuotaEpoch") + e.Leases.callCount("CacheDel"); n != 0 {
		t.Fatalf("读路径不该顶 epoch 或删缓存，实际 %d 次", n)
	}
	if n := e.Leases.callCount("AllowRate") + e.Leases.callCount("RememberRequest"); n != 0 {
		t.Fatalf("读路径不该计数或占幂等键，实际 %d 次", n)
	}
}

// TODO(缺陷)：GetAccessQuota 没有任何调用方门禁 —— 连 requireOperatorRead 都没调。
//
// 钉住现状（不是期望）：完全未归因的客户端能读任意房间/用户/节点的生效配额，
// 把 RequireAttestedOperator 翻成 true 也照读不误，因为这条路径上根本没有门禁可翻。
// 同类读接口（ListRoomConnections / ListRoomRoutes / ListBroadcastLogs）都走 requireOperatorRead，
// 唯独这里例外：配额上限本身是「攻击面测绘数据」（某房间最多能挂多少连接、弹幕 QPS 天花板），
// 拿到它就等于知道每间房的洪量阈值。
// 若后续补上门禁，本用例即红，届时把这两条断言改写成拒绝用例。
func TestGetAccessQuotaHasNoCallerGatePin(t *testing.T) {
	e := newTestEnv(t)
	e.addQuotaRow(&model.LiveGwAccessQuota{Scope: model.QuotaScopeRoom, ScopeId: roomID,
		ScopeKey: "room-7001", MaxConnections: 40, BroadcastQps: 30, Version: 1})

	got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
	if err != nil || got.GetMaxConnections() != 40 {
		t.Fatalf("当前实现：未归因主体照读，实际 err=%v reply=%+v", err, got)
	}

	e.apply(func(c *config.LiveGatewayConf) { c.RequireAttestedOperator = true })
	got2, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
	if err != nil || got2.GetMaxConnections() != 40 {
		t.Fatalf("当前实现：RequireAttestedOperator=true 也管不到这条读路径，实际 err=%v reply=%+v", err, got2)
	}
}

// --- 本文件私用小工具 ---

// removeQuotaRow 从替身切片里去掉某一行（只用于构造「本层已撤配置」这一既成事实）。
func removeQuotaRow(rows []*model.LiveGwAccessQuota, scope int32, scopeID int64) []*model.LiveGwAccessQuota {
	out := rows[:0]
	for _, r := range rows {
		if r.Scope == scope && r.ScopeId == scopeID {
			continue
		}
		out = append(out, r)
	}
	return out
}

// mustGetQuotaMax 读一次房间层生效配额，返回 max_connections。
// want 只用于失败时的信息完整性检查（不做断言），断言留给调用方，好让失败消息带上场景。
func mustGetQuotaMax(t *testing.T, e *testEnv, want int32) int32 {
	t.Helper()
	got, err := NewGetAccessQuotaLogic(ctxClient(t), e.Svc).
		GetAccessQuota(&rpc.AccessQuotaReq{Scope: rpc.QuotaScope_QUOTA_SCOPE_ROOM, ScopeId: roomID})
	if err != nil {
		t.Fatalf("读取配额失败（期望读到 %d）: %v", want, err)
	}
	return got.GetMaxConnections()
}
