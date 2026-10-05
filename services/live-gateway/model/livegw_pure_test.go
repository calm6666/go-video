package model

// 本文件只测 model 包里的**纯函数**：到期/租约/票据/路由状态机、权限矩阵、
// 票据摘要、副本节点编解码、配额边界与分页夹取。
// 一律不连库（仓库无 sqlmock 依赖，也不新增），因此这些用例在没有 MySQL 的 CI 上也必须全绿。
// 契约来源：rpc/livegateway.proto 的枚举注释 + services/live-gateway/README.md「数据分层/状态机」。

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestExpired(t *testing.T) {
	// 心跳超时与租约到期的唯一判定：0 表示「未设置」，绝不能被判成已过期，
	// 否则 last_heartbeat_at 为空的新连接会被立刻判失联。
	cases := []struct {
		name string
		at   int64
		now  int64
		want bool
	}{
		{"零值视为未设置_不过期", 0, 1700000000, false},
		{"恰好到点即过期", 100, 100, true},
		{"尚未到期", 101, 100, false},
		{"已经过期", 99, 100, true},
		{"负数截止不过期", -5, 100, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Expired(c.at, c.now); got != c.want {
				t.Fatalf("Expired(%d,%d)=%v want %v", c.at, c.now, got, c.want)
			}
		})
	}
}

func TestSetClockDrivesNowUnix(t *testing.T) {
	// 注入时钟必须同时驱动 NowUnix/NowMilli 与包内 nowUnix：
	// 租约 TTL、票据有效期、禁止重连窗口必须同源，否则测不出「刚好到点」的边界。
	fixed := time.Unix(1_700_000_000, 123_456_789)
	restore := SetClock(func() time.Time { return fixed })
	if got := NowUnix(); got != 1_700_000_000 {
		t.Fatalf("NowUnix()=%d want 1700000000", got)
	}
	if got := NowMilli(); got != 1_700_000_000_123 {
		t.Fatalf("NowMilli()=%d want 1700000000123", got)
	}
	if got := nowUnix(); got != 1_700_000_000 {
		t.Fatalf("包内 nowUnix()=%d want 1700000000", got)
	}
	restore()
	// 恢复后必须回到系统时间（容差 5 分钟，避免机器时钟偏差导致脆断）。
	if diff := time.Now().Unix() - NowUnix(); diff < 0 || diff > 300 {
		t.Fatalf("restore 后 NowUnix 未回到系统时间, diff=%d", diff)
	}
}

func TestSetClockNilRestoresSystemTime(t *testing.T) {
	restore := SetClock(func() time.Time { return time.Unix(1, 0) })
	if NowUnix() != 1 {
		t.Fatal("注入时钟未生效")
	}
	restore()
	defer SetClock(nil)()
	if now := NowUnix(); now < 1_700_000_000 {
		t.Fatalf("SetClock(nil) 应恢复系统时间, got %d", now)
	}
}

func TestNormalizeRoleNeverReturnsUnknownRole(t *testing.T) {
	// 客户端自报角色不可信：任何未识别/越界取值都必须收敛为 VIEWER，
	// 绝不能保留成 0 或 6 这种「谁都不认识但可能被下游当成特权」的值。
	for _, r := range []int32{-100, -1, RoleUnspecified, 6, 7, 9999, RoleService + 1} {
		if got := NormalizeRole(r); got != RoleViewer {
			t.Fatalf("NormalizeRole(%d)=%d want RoleViewer(%d)", r, got, RoleViewer)
		}
	}
	for _, r := range []int32{RoleViewer, RoleAnchor, RoleRoomAdmin, RoleOperator, RoleService} {
		if NormalizeRole(r) != r {
			t.Fatalf("NormalizeRole(%d) 应保持原值, got %d", r, NormalizeRole(r))
		}
	}
	if ValidRole(RoleUnspecified) != true || ValidRole(6) != false {
		t.Fatal("ValidRole 边界与 rpc.ConnRole 取值范围不一致")
	}
}

func TestLeaseStateTransition(t *testing.T) {
	// KICKED / RELEASED 是终态：被风控踢掉的连接不得靠续租复活。
	if !IsLeaseTerminal(LeaseStateKicked) || !IsLeaseTerminal(LeaseStateReleased) {
		t.Fatal("KICKED/RELEASED 必须是租约终态")
	}
	if IsLeaseTerminal(LeaseStateExpired) {
		t.Fatal("EXPIRED 不是终态：客户端可带新凭据重连恢复")
	}
	cases := []struct {
		from, to int32
		want     bool
	}{
		{LeaseStateActive, LeaseStateKicked, true},
		{LeaseStateActive, LeaseStateReleased, true},
		{LeaseStateActive, LeaseStateExpired, true},
		{LeaseStateExpired, LeaseStateActive, true}, // 续租复活
		{LeaseStateKicked, LeaseStateActive, false}, // 禁止复活
		{LeaseStateReleased, LeaseStateActive, false},
		{LeaseStateKicked, LeaseStateReleased, false},
	}
	for _, c := range cases {
		if got := IsValidLeaseTransition(c.from, c.to); got != c.want {
			t.Fatalf("IsValidLeaseTransition(%d,%d)=%v want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestTicketTransitionIsOneShot(t *testing.T) {
	// 票据是一次性凭据：USED 与 REVOKED 互斥且都不可逆。
	// 已撤销的票据绝不能被 Redeem 重新置为 USED（否则撤销名单形同虚设）。
	if !IsTicketTerminal(TicketStateUsed) || !IsTicketTerminal(TicketStateRevoked) ||
		!IsTicketTerminal(TicketStateExpired) {
		t.Fatal("USED/REVOKED/EXPIRED 必须是票据终态")
	}
	if IsTicketTerminal(TicketStateIssued) {
		t.Fatal("ISSUED 不是终态")
	}
	cases := []struct {
		from, to int32
		want     bool
	}{
		{TicketStateIssued, TicketStateUsed, true},
		{TicketStateIssued, TicketStateRevoked, true},
		{TicketStateIssued, TicketStateExpired, true},
		{TicketStateUsed, TicketStateRevoked, false},
		{TicketStateRevoked, TicketStateUsed, false},
		{TicketStateExpired, TicketStateIssued, false},
	}
	for _, c := range cases {
		if got := IsValidTicketTransition(c.from, c.to); got != c.want {
			t.Fatalf("IsValidTicketTransition(%d,%d)=%v want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestRouteTransitionForbidsShortcutToOffline(t *testing.T) {
	// SERVING 不能直接跳 OFFLINE：必须先 DRAINING 把连接引走，
	// 否则发布期节点下线会把房间里的连接硬切（proto RouteState 注释）。
	cases := []struct {
		from, to int32
		want     bool
	}{
		{RouteStateServing, RouteStateDraining, true},
		{RouteStateServing, RouteStateOffline, false},
		{RouteStateDraining, RouteStateServing, true},  // 排空可撤销
		{RouteStateDraining, RouteStateOffline, true},  // 排空完成才下线
		{RouteStateOffline, RouteStateServing, true},   // 节点重启后重建路由
		{RouteStateOffline, RouteStateDraining, false}, // 未承接就谈不上排空
	}
	for _, c := range cases {
		if got := IsValidRouteTransition(c.from, c.to); got != c.want {
			t.Fatalf("IsValidRouteTransition(%d,%d)=%v want %v", c.from, c.to, got, c.want)
		}
	}
	if ValidRouteState(RouteStateUnspecified) || !ValidRouteState(RouteStateOffline) {
		t.Fatal("ValidRouteState 边界与 rpc.RouteState 不一致")
	}
}

func TestRoleAllowedToSendMatrix(t *testing.T) {
	// 权限矩阵是下发权限的唯一事实来源，逐格钉住，防止有人改一行就开出后门。
	cases := []struct {
		role, kind int32
		want       bool
	}{
		// 观众：只有弹幕与互动提示
		{RoleViewer, KindDanmaku, true},
		{RoleViewer, KindInteraction, true},
		{RoleViewer, KindModeration, false}, // 观众不得发审核处置
		{RoleViewer, KindRoomState, false},  // 观众不得发开播/下播
		{RoleViewer, KindSystem, false},
		{RoleViewer, KindAnchorTip, false},
		// 主播：弹幕/互动/提词；房间状态由 live-room 事件驱动
		{RoleAnchor, KindAnchorTip, true},
		{RoleAnchor, KindRoomState, false},
		{RoleAnchor, KindModeration, false}, // 主播也不能借单播发处置
		// 房管：公告与提示，不能改房间状态
		{RoleRoomAdmin, KindSystem, true},
		{RoleRoomAdmin, KindRoomState, false},
		{RoleRoomAdmin, KindModeration, false},
		// 内部服务/运营：全类别
		{RoleService, KindModeration, true},
		{RoleOperator, KindRoomState, true},
		// 未识别角色与非法类别一律拒
		{RoleUnspecified, KindDanmaku, false},
		{RoleViewer, KindUnspecified, false},
		{RoleService, KindUnspecified, false},
		{RoleService, KindAnchorTip + 1, false},
	}
	for _, c := range cases {
		if got := RoleAllowedToSend(c.role, c.kind); got != c.want {
			t.Fatalf("RoleAllowedToSend(role=%d,kind=%d)=%v want %v", c.role, c.kind, got, c.want)
		}
	}
}

func TestKindRequiresTrustedSender(t *testing.T) {
	// 这四项不能被观众态连接触发（model 注释）；弹幕与互动提示可以。
	for _, k := range []int32{KindRoomState, KindSystem, KindModeration, KindAnchorTip} {
		if !KindRequiresTrustedSender(k) {
			t.Fatalf("kind=%d 必须是可信来源专属", k)
		}
	}
	for _, k := range []int32{KindDanmaku, KindInteraction, KindUnspecified} {
		if KindRequiresTrustedSender(k) {
			t.Fatalf("kind=%d 不应要求可信来源", k)
		}
	}
}

func TestValidEnumBoundaries(t *testing.T) {
	if !ValidBroadcastKind(KindDanmaku) || !ValidBroadcastKind(KindAnchorTip) ||
		ValidBroadcastKind(KindUnspecified) || ValidBroadcastKind(KindAnchorTip+1) {
		t.Fatal("ValidBroadcastKind 边界与 rpc.BroadcastKind 不一致")
	}
	if ValidDropReason(DropUnspecified) || !ValidDropReason(DropOK) ||
		!ValidDropReason(DropTransportUnavailable) || ValidDropReason(DropTransportUnavailable+1) {
		t.Fatal("ValidDropReason 边界与 rpc.DropReason 不一致")
	}
	if !ValidBroadcastLogState(BroadcastLogSent) || !ValidBroadcastLogState(BroadcastLogDuplicated) ||
		ValidBroadcastLogState(BroadcastLogDuplicated+1) || ValidBroadcastLogState(0) {
		t.Fatal("ValidBroadcastLogState 边界与 rpc.BroadcastLogInfo.state 注释不一致")
	}
	if !ValidQuotaScope(QuotaScopeGlobal) || !ValidQuotaScope(QuotaScopeUser) ||
		ValidQuotaScope(QuotaScopeUnspecified) || ValidQuotaScope(QuotaScopeUser+1) {
		t.Fatal("ValidQuotaScope 边界与 rpc.QuotaScope 不一致")
	}
}

func TestTicketHashNeverLeaksPlaintext(t *testing.T) {
	// 落库只存摘要：sha256 hex 64 位、确定性、不同票据不同摘要、且摘要里不含原文片段。
	const ticket = "lgw.t1.9f2c7a41e0b6d83c5a17"
	h := TicketHash(ticket)
	if len(h) != 64 {
		t.Fatalf("TicketHash 长度=%d want 64 (%s)", len(h), h)
	}
	if TicketHash(ticket) != h {
		t.Fatal("TicketHash 必须确定性，否则无法按摘要反查")
	}
	if TicketHash(ticket+"x") == h {
		t.Fatal("不同票据不得撞同一摘要")
	}
	if strings.Contains(h, "9f2c7a41") || strings.Contains(strings.ToLower(h), "lgw") {
		t.Fatalf("摘要里泄漏了票据原文片段: %s", h)
	}
	// 钉住实现是 sha256 而不是别的弱哈希（空串摘要全仓唯一）。
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if TicketHash("") != emptySHA256 {
		t.Fatal("TicketHash 不是 sha256(ticket) 的 hex")
	}
	for _, r := range []rune(h) {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("摘要含非小写十六进制字符 %q", r)
		}
	}
}

func TestClampPageBoundaries(t *testing.T) {
	// rpc.PageParam：pn 从 1 开始，ps 上限 50，越界由服务端夹取而不报错。
	cases := []struct {
		pn, ps, maxPS      int32
		wantLimit, wantOff int32
	}{
		{0, 0, 50, 20, 0},    // 全默认
		{-3, -1, 50, 20, 0},  // 负数归位
		{1, 10, 50, 10, 0},   // 正常
		{3, 10, 50, 10, 20},  // 第 3 页
		{1, 50, 50, 50, 0},   // 恰好到上限
		{1, 51, 50, 20, 0},   // 越上限回落到默认 20
		{2, 500, 50, 20, 20}, // 明显越界
		{1, 0, 0, 20, 0},     // maxPS<=0 时按 50 处理
		{1, 80, 3, 3, 0},     // 默认 20 也大于 maxPS 时夹到 maxPS
		{2, 80, 3, 3, 3},     // offset 用夹取后的 limit 计算，不能按 ps 算
	}
	for _, c := range cases {
		limit, offset := clampPage(c.pn, c.ps, c.maxPS)
		if limit != c.wantLimit || offset != c.wantOff {
			t.Fatalf("clampPage(%d,%d,%d)=(%d,%d) want (%d,%d)",
				c.pn, c.ps, c.maxPS, limit, offset, c.wantLimit, c.wantOff)
		}
		if limit < 1 {
			t.Fatalf("clampPage(%d,%d,%d) 返回了非正 limit=%d", c.pn, c.ps, c.maxPS, limit)
		}
	}
}

func TestEncodeDecodeReplicaNodes(t *testing.T) {
	// replica_nodes 是 JSON 列：空集合必须编码成 "[]"，写空串会被 MySQL 判为无效 JSON。
	raw, err := encodeReplicaNodes(nil)
	if err != nil || raw != "[]" {
		t.Fatalf("encodeReplicaNodes(nil)=(%q,%v) want \"[]\"", raw, err)
	}
	if raw, err = encodeReplicaNodes([]string{}); err != nil || raw != "[]" {
		t.Fatalf("空切片应编码为 \"[]\", got (%q,%v)", raw, err)
	}
	if _, err = encodeReplicaNodes([]string{"n1", ""}); !errors.Is(err, ErrEmptyNodeID) {
		t.Fatalf("空节点标识必须被拒绝, got %v", err)
	}
	if _, err = encodeReplicaNodes([]string{"n1", "   "}); !errors.Is(err, ErrEmptyNodeID) {
		t.Fatalf("纯空白节点标识必须被拒绝, got %v", err)
	}
	// 去重 + 去空白，顺序保留（广播扇出顺序影响排障可读性）。
	raw, err = encodeReplicaNodes([]string{" n1 ", "n1", "n2"})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := decodeReplicaNodes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 || nodes[0] != "n1" || nodes[1] != "n2" {
		t.Fatalf("round trip 失败: raw=%q nodes=%v", raw, nodes)
	}
	for _, empty := range []string{"", "  ", "[]", "null"} {
		got, err := decodeReplicaNodes(empty)
		if err != nil || got != nil {
			t.Fatalf("decodeReplicaNodes(%q)=(%v,%v) 应为 (nil,nil)", empty, got, err)
		}
	}
	// 非 JSON 字符串数组必须报错而不是静默当无副本（脏数据会悄悄改变扇出面）。
	if _, err := decodeReplicaNodes(`{"a":1}`); err == nil {
		t.Fatal("对象形态不应被当成节点数组")
	}
	if _, err := decodeReplicaNodes("not-json"); err == nil {
		t.Fatal("非法 JSON 必须报错")
	}
}

func TestRoomRouteNormalizeReplicaNodes(t *testing.T) {
	r := &LiveGwRoomRoute{}
	if err := r.NormalizeReplicaNodes(nil); err != nil {
		t.Fatal(err)
	}
	if r.ReplicaNodes != "[]" {
		t.Fatalf("空值必须补成 \"[]\", got %q", r.ReplicaNodes)
	}
	if err := r.NormalizeReplicaNodes([]string{"n2", "n1"}); err != nil {
		t.Fatal(err)
	}
	list, err := r.ReplicaNodesList()
	if err != nil || len(list) != 2 {
		t.Fatalf("ReplicaNodesList=(%v,%v)", list, err)
	}
	r.ReplicaNodes = "broken"
	if err := r.NormalizeReplicaNodes(nil); err == nil {
		t.Fatal("已有脏值必须在校验阶段暴露，不能带着脏值写库")
	}
}

func TestQuotaScopeChainNeverInterleavesNodeAboveRoom(t *testing.T) {
	// USER 链只含 USER→GLOBAL：把 ROOM/NODE 夹在中间会让「某房间某用户」的
	// 风控降配被节点配置覆盖，与运营直觉相反（model 注释）。
	cases := []struct {
		scope int32
		want  []int32
	}{
		{QuotaScopeUser, []int32{QuotaScopeUser, QuotaScopeGlobal}},
		{QuotaScopeRoom, []int32{QuotaScopeRoom, QuotaScopeGlobal}},
		{QuotaScopeNode, []int32{QuotaScopeNode, QuotaScopeGlobal}},
		{QuotaScopeGlobal, []int32{QuotaScopeGlobal}},
		{QuotaScopeUnspecified, []int32{QuotaScopeGlobal}},
		{99, []int32{QuotaScopeGlobal}},
	}
	for _, c := range cases {
		got := QuotaScopeChain(c.scope)
		if len(got) != len(c.want) {
			t.Fatalf("QuotaScopeChain(%d)=%v want %v", c.scope, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("QuotaScopeChain(%d)=%v want %v", c.scope, got, c.want)
			}
		}
		if last := got[len(got)-1]; last != QuotaScopeGlobal {
			t.Fatalf("继承链必须以 GLOBAL 收尾, got %v", got)
		}
	}
}

func TestApplyQuotaLayerOnlyNonZeroWins(t *testing.T) {
	// 0 = 继承：更具体的层只覆盖自己写了非 0 的项，其余保留上一层结果。
	eff := &EffectiveQuota{MaxConnections: 100, BroadcastQps: 50, AllowGuest: true}
	applyQuotaLayer(eff, &LiveGwAccessQuota{
		Scope: QuotaScopeRoom, ScopeId: 7, MaxConnections: 5000, AllowGuest: 1,
	}, QuotaScopeRoom)
	if eff.MaxConnections != 5000 {
		t.Fatalf("非 0 值应覆盖, got %d", eff.MaxConnections)
	}
	if eff.BroadcastQps != 50 {
		t.Fatalf("0 值必须继承上一层, got %d", eff.BroadcastQps)
	}
	if !eff.AllowGuest {
		t.Fatal("AllowGuest=1 应解析为 true")
	}
	if len(eff.HitScopes) != 1 || eff.HitScopes[0] != "3:7" {
		t.Fatalf("HitScopes=%v want [3:7]（排障要能说清哪层生效）", eff.HitScopes)
	}

	// AllowGuest=0 是「继承」，不能把上一层的允许翻转成禁止。
	eff2 := &EffectiveQuota{AllowGuest: true}
	applyQuotaLayer(eff2, &LiveGwAccessQuota{Scope: QuotaScopeUser, ScopeId: 9}, QuotaScopeUser)
	if !eff2.AllowGuest {
		t.Fatal("allow_guest=0 是继承语义，不得当成显式禁止")
	}
	if len(eff2.HitScopes) != 0 {
		t.Fatalf("整行都是 0 时不该记命中层: %v", eff2.HitScopes)
	}

	// 显式禁止（1 之外唯一合法的非 0 值只能是 1）：0/1 之外的值由 CheckQuotaBounds 拦下。
	eff3 := &EffectiveQuota{MaxPayloadBytes: 32768}
	applyQuotaLayer(eff3, &LiveGwAccessQuota{ScopeId: 1, MaxPayloadBytes: 1024}, QuotaScopeRoom)
	if eff3.MaxPayloadBytes != 1024 || len(eff3.HitScopes) != 1 {
		t.Fatalf("载荷上限未被覆盖: %+v", eff3)
	}
}

func TestCheckQuotaBounds(t *testing.T) {
	base := func() *LiveGwAccessQuota {
		return &LiveGwAccessQuota{Scope: QuotaScopeRoom, ScopeId: 42, MaxConnections: 100}
	}
	if err := CheckQuotaBounds(base(), 300, 600, 32768); err != nil {
		t.Fatalf("合法配额被拒: %v", err)
	}
	ok := base()
	ok.Scope, ok.ScopeId = QuotaScopeGlobal, 0
	if err := CheckQuotaBounds(ok, 300, 600, 32768); err != nil {
		t.Fatalf("GLOBAL 行 scope_id=0 合法: %v", err)
	}

	cases := []struct {
		name string
		mut  func(q *LiveGwAccessQuota)
		want error
	}{
		{"scope 非法", func(q *LiveGwAccessQuota) { q.Scope = QuotaScopeUnspecified }, ErrInvalidQuotaScope},
		{"GLOBAL 带 scope_id", func(q *LiveGwAccessQuota) { q.Scope = QuotaScopeGlobal }, ErrInvalidQuotaScope},
		{"非 GLOBAL 缺 scope_id", func(q *LiveGwAccessQuota) { q.ScopeId = 0 }, ErrInvalidQuotaScope},
		{"租约 TTL 为负", func(q *LiveGwAccessQuota) { q.LeaseTtlSeconds = -1 }, ErrQuotaExceeded},
		{"租约 TTL 越上限", func(q *LiveGwAccessQuota) { q.LeaseTtlSeconds = 301 }, ErrQuotaExceeded},
		{"票据 TTL 越上限", func(q *LiveGwAccessQuota) { q.TicketTtlSeconds = 601 }, ErrQuotaExceeded},
		{"载荷上限越界", func(q *LiveGwAccessQuota) { q.MaxPayloadBytes = 32769 }, ErrPayloadTooLarge},
		{"载荷上限为负", func(q *LiveGwAccessQuota) { q.MaxPayloadBytes = -1 }, ErrPayloadTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := base()
			c.mut(q)
			if err := CheckQuotaBounds(q, 300, 600, 32768); !errors.Is(err, c.want) {
				t.Fatalf("got %v want %v", err, c.want)
			}
		})
	}
	// max* 传 0 表示该项不加上限（由调用方决定），边界值此时必须放行。
	q := base()
	q.MaxPayloadBytes = 1 << 30
	if err := CheckQuotaBounds(q, 0, 0, 0); err != nil {
		t.Fatalf("max*=0 时不该拦: %v", err)
	}
	// allow_guest 只允许 0/1：2 会被解析成「允许」还是「禁止」取决于实现，必须拒掉。
	q = base()
	q.AllowGuest = 2
	if err := CheckQuotaBounds(q, 0, 0, 0); err == nil {
		t.Fatal("allow_guest=2 必须被拒绝")
	}
}

func TestIsDuplicateErr(t *testing.T) {
	// 唯一索引冲突判定沿用 notification 的文本匹配（不新增驱动依赖）。
	if !isDuplicateErr(errors.New("Error 1062: Duplicate entry '7-abc' for key 'uniq_room_message'")) {
		t.Fatal("1062 必须被识别为幂等冲突")
	}
	if isDuplicateErr(nil) || isDuplicateErr(errors.New("Error 1146: Table doesn't exist")) {
		t.Fatal("非冲突错误不得被吞成幂等重放")
	}
}

func TestRoleNormalizerMatchesNormalizeRole(t *testing.T) {
	// 导出的 RoleNormalizer 与包内 NormalizeRole 必须同口径，
	// 否则 model 兜底与 logic 判定会给出两个不同的角色。
	for _, r := range []int32{-1, 0, 1, 2, 3, 4, 5, 6, 42} {
		if RoleNormalizer(r) != NormalizeRole(r) {
			t.Fatalf("RoleNormalizer(%d)=%d != NormalizeRole(%d)=%d", r, RoleNormalizer(r), r, NormalizeRole(r))
		}
	}
	if RoleNormalizer(RoleUnspecified) != RoleViewer {
		t.Fatal("未指定角色必须收敛为 VIEWER")
	}
}
