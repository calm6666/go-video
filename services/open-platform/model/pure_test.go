package model

import "testing"

// 本文件只覆盖不连库的纯函数：scope 解析与包含关系、配额窗口计算与层级选取、
// 退避时间、状态机与凭证可用性判定。这些函数是 logic 判定的唯一算式来源，
// 一旦与 SQL 侧口径漂移（例如配额取齐），就会出现「看到的限额」和「扣减的限额」不一致。

func TestJoinScopesNormalizesDedupsAndSorts(t *testing.T) {
	got := JoinScopes([]string{" video.publish ", "comment.read", "video.publish", "", "  ", "profile.read"})
	if want := "comment.read,profile.read,video.publish"; got != want {
		t.Fatalf("JoinScopes 必须去重、去空、升序、逗号分隔: got %q want %q", got, want)
	}
	if s := JoinScopes(nil); s != "" {
		t.Fatalf("空集合必须回空串（入库为 ''，不是 ','）: %q", s)
	}
	// 往返稳定：入库串再解析再拼必须一字不差，否则 granted_scope 快照无法比对。
	back := JoinScopes(SplitScopes(got))
	if back != got {
		t.Fatalf("SplitScopes/JoinScopes 不幂等: %q -> %q", got, back)
	}
	if SplitScopes(" , ,, ") != nil {
		t.Fatal("全空分隔串必须解析成 nil，而不是含空串的切片")
	}
}

func TestScopeContainment(t *testing.T) {
	granted := SplitScopes("comment.read,video.publish")
	if !ContainsScope(granted, "video.publish") {
		t.Fatal("已获批 scope 必须判定为包含")
	}
	if ContainsScope(granted, "video.publish.write") {
		t.Fatal("前缀匹配不等于包含（否则 video.publish 会误放行 video.publish.write）")
	}
	if !ScopesSubset([]string{"video.publish"}, granted) {
		t.Fatal("子集判定失败")
	}
	if ScopesSubset([]string{"video.publish", "comment.write"}, granted) {
		t.Fatal("超出获批集合的 scope 必须被拒（最小权限边界）")
	}
	if !ScopesSubset(nil, granted) {
		t.Fatal("空请求集是任何集合的子集")
	}
}

func TestAlignWindowMatchesSQLFloor(t *testing.T) {
	// 与 op_api_call_log.ListWindowTotals 的 FLOOR(ctime/w)*w 必须同口径，否则重算永远对不上。
	cases := []struct{ ts, w, want int64 }{
		{0, 60, 0},
		{59, 60, 0},
		{60, 60, 60},
		{160, 60, 120},
		{86401, 86400, 86400},
		{123, 0, 0},  // 窗口长度非法时返回 0，由调用方报错而不是默默除零
		{123, -5, 0}, //nolint:dupl // 负窗口同样必须回 0
	}
	for _, c := range cases {
		if got := AlignWindow(c.ts, c.w); got != c.want {
			t.Errorf("AlignWindow(%d,%d)=%d want %d", c.ts, c.w, got, c.want)
		}
	}
	if windowEndOf(AlignWindow(160, 60), 60) != 180 {
		t.Fatal("windowEndOf 必须与 QuotaUsage.ResetAt 同口径（start+seconds）")
	}
}

func TestNarrowPoliciesPicksOneTier(t *testing.T) {
	p := func(app int64, api string, w, limit int64) *QuotaPolicy {
		return &QuotaPolicy{AppID: app, APICode: api, WindowSeconds: w, QuotaLimit: limit, Enabled: 1}
	}
	appExact := p(7, "video.publish", 60, 100)
	appAny := p(7, AnyAPICode, 3600, 5000)
	globalExact := p(GlobalAppID, "video.publish", 60, 10)
	globalAny := p(GlobalAppID, AnyAPICode, 60, 1)

	got := NarrowPolicies([]*QuotaPolicy{globalAny, appAny, globalExact, appExact}, 7, "video.publish")
	if len(got) != 1 || got[0] != appExact {
		t.Fatalf("必须取最高层级（本应用+精确接口），实得 %+v", got)
	}
	// 同层级多窗口全部保留（同时限流），不跨层级相加。
	got = NarrowPolicies([]*QuotaPolicy{appExact, p(7, "video.publish", 86400, 999), globalAny}, 7, "video.publish")
	if len(got) != 2 {
		t.Fatalf("同层级的多个窗口必须一并生效，实得 %d 条", len(got))
	}
	// 本应用没有精确规则时降到「本应用 + *」。
	got = NarrowPolicies([]*QuotaPolicy{appAny, globalExact, globalAny}, 7, "video.publish")
	if len(got) != 1 || got[0] != appAny {
		t.Fatalf("层级 2 失配: %+v", got)
	}
	// 只有全局时按 3→4 层降级。
	got = NarrowPolicies([]*QuotaPolicy{globalAny, globalExact}, 7, "video.publish")
	if len(got) != 1 || got[0] != globalExact {
		t.Fatalf("层级 3 失配: %+v", got)
	}
	if got = NarrowPolicies([]*QuotaPolicy{p(8, "video.publish", 60, 1)}, 7, "video.publish"); got != nil {
		t.Fatalf("别的应用的规则不得串档（app_id 必须精确等于请求应用），实得 %+v", got)
	}
	if got = NarrowPolicies(nil, 7, "video.publish"); got != nil {
		t.Fatal("无任何规则时返回 nil，由 logic 按 fail-closed 拒绝")
	}
	if !p(7, "video.publish", 60, 0).Denied() {
		t.Fatal("limit<=0 必须等价「禁用该接口」")
	}
}

func TestNextRetryAtBackoff(t *testing.T) {
	const base, max int64 = 30, 600
	if got := NextRetryAt(1000, base, max, 1); got != 1030 {
		t.Fatalf("第一次重试间隔应为 base: %d", got)
	}
	if got := NextRetryAt(1000, base, max, 3); got != 1000+120 {
		t.Fatalf("指数退避 30*2^2=120: %d", got)
	}
	if got := NextRetryAt(1000, base, max, 200); got != 1600 {
		t.Fatalf("退避必须被 max 截住（同时避免位移溢出）: %d", got)
	}
	if got := NextRetryAt(1000, base, max, 0); got != 1030 {
		t.Fatalf("attempt=0（人工重放归零后）按第一次处理: %d", got)
	}
	if got := NextRetryAt(1000, 0, 0, 1); got != 1030 {
		t.Fatalf("配置为 0 时退回安全默认 base=30/max=3600: %d", got)
	}
}

func TestCanTransitionAppStatus(t *testing.T) {
	ok := [][2]int32{
		{AppStatusPendingReview, AppStatusActive},
		{AppStatusPendingReview, AppStatusRejected},
		{AppStatusActive, AppStatusSuspended},
		{AppStatusSuspended, AppStatusActive},
		{AppStatusRejected, AppStatusPendingReview},
		{AppStatusActive, AppStatusOffline},
	}
	for _, tr := range ok {
		if !CanTransitionAppStatus(tr[0], tr[1]) {
			t.Errorf("合法迁移被拒: %d -> %d", tr[0], tr[1])
		}
	}
	bad := [][2]int32{
		{AppStatusPendingReview, AppStatusSuspended},
		{AppStatusRejected, AppStatusActive}, // 驳回后必须重新提审，不能直接放开
		{AppStatusOffline, AppStatusActive},  // 终态：重新接入要重新注册
		{AppStatusOffline, AppStatusOffline},
		{AppStatusActive, AppStatusPendingReview},
	}
	for _, tr := range bad {
		if CanTransitionAppStatus(tr[0], tr[1]) {
			t.Errorf("非法迁移被放行: %d -> %d", tr[0], tr[1])
		}
	}
	// from==to 视为幂等成功（重复提交同一状态不是错误），但未知值一律拒绝。
	if !CanTransitionAppStatus(AppStatusActive, AppStatusActive) {
		t.Fatal("同状态重复提交必须幂等放行")
	}
	if CanTransitionAppStatus(AppStatusActive, 9) || ValidAppStatus(9) || ValidAppStatus(0) {
		t.Fatal("未定义状态值必须拒绝（0 是 UNSPECIFIED，不允许当状态写库）")
	}
	for _, tr := range [][2]int32{{0, 0}, {99, 99}, {42, 42}, {0, AppStatusOffline}, {AppStatusOffline, 0}} {
		if CanTransitionAppStatus(tr[0], tr[1]) {
			t.Errorf("未定义状态迁移被放行: %d -> %d", tr[0], tr[1])
		}
	}
}

func TestRevocationSiteBeatsTokenState(t *testing.T) {
	// 撤销位点是「立即生效」的第二道保险：签发时间不晚于位点即拒绝。
	revoked := &Grant{RevokedAt: 1000, Status: GrantStatusRevoked}
	if revoked.Granted(1001) {
		t.Fatal("已撤销 grant 不得判为有效授权")
	}
	if revoked.TokenAccepted(1000) || revoked.TokenAccepted(999) {
		t.Fatal("撤销位点之前（含同一秒）签发的 token 必须被拒")
	}
	if !revoked.TokenAccepted(1001) {
		t.Fatal("位点之后重新授权的 token 必须可用（撤销不能变成永久封禁）")
	}
	if !(&Grant{RevokedAt: 0, Status: GrantStatusActive}).Granted(1) {
		t.Fatal("未撤销 grant 必须有效")
	}
}

func TestCredentialUsabilityPredicates(t *testing.T) {
	// 判定一律以时间戳真值为准，state 只作展示与索引。
	if (&Token{State: TokenStateActive, AccessExpiresAt: 100}).AccessUsable(100) {
		t.Fatal("到期当刻即不可用（> 而非 >=）")
	}
	if !(&Token{State: TokenStateActive, AccessExpiresAt: 101}).AccessUsable(100) {
		t.Fatal("未过期且 ACTIVE 必须可用")
	}
	if (&Token{State: TokenStateRevoked, AccessExpiresAt: 9999}).AccessUsable(100) {
		t.Fatal("已撤销 token 即使未过期也不可用")
	}
	if (&Token{State: TokenStateRotated, RefreshExpiresAt: 9999}).RefreshUsable(100) {
		t.Fatal("被轮换的 refresh 不可再用（重放检测的入口条件）")
	}
	code := &AuthCode{ExpiresAt: 160, UsedAt: 0}
	if !code.Usable(159) || code.Usable(160) {
		t.Fatal("授权码必须在过期当刻立即不可消费")
	}
	used := &AuthCode{ExpiresAt: 9999, UsedAt: 150}
	if used.Usable(160) || !used.Expired(99999) {
		t.Fatal("已消费/已过期的授权码都不可用")
	}
	if (&AppSecret{Status: SecretStatusActive, ExpiresAt: 100}).Usable(100) {
		t.Fatal("宽限期到期当刻密钥不可再验签")
	}
	if !(&AppSecret{Status: SecretStatusActive, ExpiresAt: 0}).Usable(1 << 40) {
		t.Fatal("expires_at=0 表示长期有效（靠轮换/吊销）")
	}
	if (&AppSecret{Status: SecretStatusHistory, ExpiresAt: 0}).Usable(1) {
		t.Fatal("置历史的密钥永不可用")
	}
}

func TestQuotaUsageProjectionMath(t *testing.T) {
	u := &QuotaUsage{WindowStart: 60, WindowSeconds: 60, LimitSnapshot: 100, Used: 30}
	if u.Remaining() != 70 || u.ResetAt() != 120 {
		t.Fatalf("remaining/reset_at 计算错误: %d %d", u.Remaining(), u.ResetAt())
	}
	// 限额被下调后剩余按 0 处理，不返回负数（客户端会据此算 retry_after）。
	down := &QuotaUsage{LimitSnapshot: 10, Used: 30}
	if down.Remaining() != 0 || !down.Exceeded() {
		t.Fatal("超限投影必须 remaining=0 且 exceeded")
	}
	if (&QuotaUsage{LimitSnapshot: 0, Used: 0}).Exceeded() == false {
		t.Fatal("limit=0 表示禁用，任何调用都算超限")
	}
	if (&QuotaUsage{WindowEnd: 500}).ResetAt() != 500 {
		t.Fatal("window_end 列存在时优先用它（清理与展示同一口径）")
	}
	if (*QuotaUsage)(nil).Remaining() != 0 || (*QuotaUsage)(nil).Exceeded() {
		t.Fatal("nil 投影按「未记账」处理：remaining=0 且不算超限，由调用方建行")
	}
}

func TestWebhookEndpointAndDeliveryGating(t *testing.T) {
	if (&WebhookEndpoint{Enabled: 1, VerifiedAt: 0}).Deliverable() {
		t.Fatal("未验证端点永不投递（注册任意 URL 不能变成 SSRF 跳板）")
	}
	if (&WebhookEndpoint{Enabled: 1, VerifiedAt: 5, DeletedAt: 9}).Deliverable() {
		t.Fatal("软删端点不再投递")
	}
	if !(&WebhookEndpoint{Enabled: 1, VerifiedAt: 5}).Deliverable() {
		t.Fatal("已验证且启用的端点必须可投递")
	}
	if (&WebhookDelivery{State: DeliveryStateDelivering}).Retryable() {
		t.Fatal("投递中的任务由租约回收，不算可重试")
	}
	for _, s := range []int32{DeliveryStatePending, DeliveryStateRetryScheduled} {
		if !(&WebhookDelivery{State: s}).Retryable() {
			t.Fatalf("状态 %d 应可继续投递", s)
		}
	}
	for _, s := range []int32{DeliveryStateDead, DeliveryStateIgnored} {
		if !(&WebhookDelivery{State: s}).ManuallyReplayable() {
			t.Fatalf("状态 %d 应允许人工重放", s)
		}
	}
	if (&WebhookDelivery{State: DeliveryStateSuccess}).ManuallyReplayable() {
		t.Fatal("成功任务不得重放（会对应用端点产生第二次副作用）")
	}
	// 0 = proto3 默认值/UNSPECIFIED，不能被视为「待投递」或「未知事件」。
	if ValidDeliveryState(0) || ValidWebhookEventType(0) {
		t.Fatal("未定义状态/事件值必须拒绝")
	}
}

func TestForbiddenScopeCategoryBlocksCommercialization(t *testing.T) {
	for _, v := range []string{"member.read", "vip.order", "payment.write", "coin.toss",
		"revenue.share", "ads.place", "ad_banner.read", "sponsor.video", "charge.wallet", "divide.list"} {
		if !IsForbiddenScopeCategory(v) {
			t.Errorf("商业化类目 %q 必须被拒（AGENTS.md §1）", v)
		}
	}
	for _, v := range []string{"profile.read", "video.publish", "comment.write", "danmaku.read",
		"favorite.write", "following.read", "video.stats.read"} {
		if IsForbiddenScopeCategory(v) {
			t.Errorf("本期开放的 scope %q 被误拒", v)
		}
	}
	// 大小写不敏感：目录里可能混入大写写法。
	if !IsForbiddenScopeCategory("PAYMENT.WRITE") {
		t.Fatal("禁用词根判定必须大小写不敏感")
	}
	// 已知局限（fail-closed 方向）：词根按子串匹配，
	// 含 "pay" 的普通词（如 display）会被误拒；宁可多拒也不放过商业化口子，
	// 新增 scope 时若命中该误判必须改词根表而不是绕过校验。
	if !IsForbiddenScopeCategory("media.display.read") {
		t.Log("子串匹配未误伤 display（若此日志出现说明词根表已调整，请同步本注释）")
	}
}
