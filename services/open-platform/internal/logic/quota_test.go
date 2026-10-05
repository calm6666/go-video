package logic

// quota_test.go：配额四法的契约测试
// （UpsertQuotaPolicy / ListQuotaPolicies / ListQuotaUsage / RecomputeQuota）。
//
// 契约依据：proto:421-504（规则与用量投影字段、四个请求口径）、
// quota.go（生效层级与窗口取齐只有一份实现）、recomputequotalogic.go（真值是流水）。
//
// 本文件钉住的不变量：
//  1. 规则是「限额真值」：响应 policy_id/created 与入库行逐字段一致；同唯一键
//     (app_id, api_code, window_seconds) 重复提交原地更新、保留 policy_id 与 ctime，且只有一行；
//  2. limit=0 是合法取值（quota_limit=0 且 enabled=1 等价「禁用该接口」），只有负数是参数错；
//     enabled=false 在本方法没有 reason 通道，必须失败关闭而不是替调用方补一个空原因；
//  3. policy_id 只是定位辅助：与唯一键算出的行不是同一条、或指向不存在的行时一律拒；
//  4. 生效层级只由 model.NarrowPolicies 决定（app+精确 → app+* → 全局+精确 → 全局+*），
//     读面回显「当前生效限额」而不是 limit_snapshot，窗口起点与扣减面共用 AlignWindow 取齐；
//  5. ListQuotaUsage 的用量来自 op_quota_usage（Peek）：既不聚合流水，也绝不扣减；
//  6. 重算以 op_api_call_log 为真值：幂等（第二次 fixed=0 且不再写库）、dry_run 真的不落库、
//     不改动任何规则行、被区间切断的窗口按整窗口真值计数；
//  7. 失败路径一律 wantNoWrites + 规则/用量行数不变；两处并发交错（唯一键抢跑、
//     在线扣减抢跑）都只留一行，且最终值等于真值。

import (
	"fmt"
	"strings"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

const (
	quotaCode   = testAPIC // 真实接口名：规则与用量都以它为键
	quotaCode2  = "video.read.list"
	quotaWin    = 60   // 分钟窗口
	quotaWinHr  = 3600 // 小时窗口
	quotaWinAny = 600  // 通配规则用的窗口长度
)

// quotaPastWindow 一个确定落在过去、起点本身已是 w 整数倍的窗口起点。
// 重算与「已记账」的断言都以它为锚：拿「当前窗口」做断言会在分钟/小时边界翻转时假失败。
func quotaPastWindow(w int64) int64 { return model.AlignWindow(nowTS(), w) - 10*w }

// quotaFixture 一个 ACTIVE 应用 + 000003 seed 的全局兜底规则（app_id=0 / api_code='*'）。
// 兜底规则是 fail-closed 断言的前提：没有它，「查不到规则」就成了看起来正常的空结果。
func quotaFixture(t *testing.T) (*store, *svc.ServiceContext) {
	t.Helper()
	db := newStore()
	seedApp(db, testAppID, testOwner)
	seedQuota(db, model.GlobalAppID, model.AnyAPICode, quotaWin, 1000)
	return db, newTestSvc(db)
}

func upsertPolicyReq(appID int64, apiCode string, window, limit int64) *rpc.UpsertQuotaPolicyReq {
	return &rpc.UpsertQuotaPolicyReq{
		AppId: appID, ApiCode: apiCode, WindowSeconds: window,
		Limit: limit, Enabled: true, OperatorMid: testMid,
	}
}

func callUpsert(t *testing.T, s *svc.ServiceContext,
	in *rpc.UpsertQuotaPolicyReq) (*rpc.UpsertQuotaPolicyReply, error) {
	t.Helper()
	return NewUpsertQuotaPolicyLogic(t.Context(), s).UpsertQuotaPolicy(in)
}

func callPolicies(t *testing.T, s *svc.ServiceContext,
	in *rpc.ListQuotaPoliciesReq) (*rpc.ListQuotaPoliciesReply, error) {
	t.Helper()
	return NewListQuotaPoliciesLogic(t.Context(), s).ListQuotaPolicies(in)
}

func callUsage(t *testing.T, s *svc.ServiceContext,
	in *rpc.ListQuotaUsageReq) (*rpc.ListQuotaUsageReply, error) {
	t.Helper()
	return NewListQuotaUsageLogic(t.Context(), s).ListQuotaUsage(in)
}

func recomputeReq(appID int64, apiCode string, from, to int64, dryRun bool) *rpc.RecomputeQuotaReq {
	return &rpc.RecomputeQuotaReq{
		AppId: appID, ApiCode: apiCode, WindowStart: from, WindowEnd: to,
		DryRun: dryRun, OperatorMid: testMid,
	}
}

func callRecompute(t *testing.T, s *svc.ServiceContext,
	in *rpc.RecomputeQuotaReq) (*rpc.RecomputeQuotaReply, error) {
	t.Helper()
	return NewRecomputeQuotaLogic(t.Context(), s).RecomputeQuota(in)
}

// ---------------------------------------------------------------- 内存态小工具

// seedQuotaAt 落一条 mtime 可控的规则：seedQuota 把 ctime/mtime 一律刷成 now，
// 只靠它无法验证「按 (mtime, policy_id) 倒序」这条位点语义。
func seedQuotaAt(db *store, appID int64, apiCode string, window, limit, mtime int64) *model.QuotaPolicy {
	p := seedQuota(db, appID, apiCode, window, limit)
	p.Mtime = mtime
	return p
}

// quotaRuleAt 按唯一键定位规则行（找不到返回 nil）。
func quotaRuleAt(db *store, appID int64, apiCode string, window int64) *model.QuotaPolicy {
	for _, p := range db.policies {
		if p.AppID == appID && p.APICode == apiCode && p.WindowSeconds == window {
			return p
		}
	}
	return nil
}

// countQuotaRuleAt 唯一键命中几行：>1 就说明「同规则重复提交」插出了第二行。
func countQuotaRuleAt(db *store, appID int64, apiCode string, window int64) int {
	var n int
	for _, p := range db.policies {
		if p.AppID == appID && p.APICode == apiCode && p.WindowSeconds == window {
			n++
		}
	}
	return n
}

// quotaRuleState 把整张规则表折成可逐条比对的字符串，供「零副作用」断言用：
// wantNoWrites 只看调用次数，这里看的是「行内容有没有被顺手改过」。
func quotaRuleState(db *store) map[int64]string {
	out := make(map[int64]string, len(db.policies))
	for id, p := range db.policies {
		out[id] = fmt.Sprintf("app=%d code=%s w=%d limit=%d enabled=%d op=%d reason=%q ctime=%d mtime=%d",
			p.AppID, p.APICode, p.WindowSeconds, p.QuotaLimit, p.Enabled, p.Operator,
			p.Reason, p.Ctime, p.Mtime)
	}
	return out
}

func assertRuleStateUnchanged(t *testing.T, db *store, before map[int64]string, label string) {
	t.Helper()
	after := quotaRuleState(db)
	if len(after) != len(before) {
		t.Fatalf("%s：规则行数 %d → %d", label, len(before), len(after))
	}
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("%s：规则 %d 被改动\n改前 %s\n改后 %s", label, id, want, got)
		}
	}
}

// seedUsage 落一行配额投影。用量行在读面必须有独立构造入口：
// 只靠 QuotaUsages.Add 造数据会把「投影」与「扣减」两件事绑死，无法构造漂移。
func seedUsage(db *store, appID int64, apiCode string, window, windowStart, used int64) *model.QuotaUsage {
	now := nowTS()
	u := &model.QuotaUsage{
		UsageID: db.next("usage"), AppID: appID, APICode: apiCode,
		WindowSeconds: window, WindowStart: windowStart, WindowEnd: windowStart + window,
		Used: used, UpdatedAt: now - 7, Ctime: now - 20,
	}
	db.usage[usageKey(appID, apiCode, window, windowStart)] = u
	return u
}

// seedCallLogs 落 n 条判定流水（重算的真值来源），ctime 相同即共用一个窗口。
func seedCallLogs(db *store, appID int64, apiCode string, ctime int64, n int) {
	for i := 0; i < n; i++ {
		req := fmt.Sprintf("req-%d-%s-%d-%d", appID, apiCode, ctime, i)
		db.logs[req] = &model.ApiCallLog{
			CallLogID: db.next("calllog"), RequestID: req, AppID: appID, APICode: apiCode,
			Mid: testMid, Allowed: model.CallAllowed, Ctime: ctime,
		}
	}
}

func usageEntry(t *testing.T, reply *rpc.ListQuotaUsageReply,
	apiCode string, window int64) *rpc.QuotaUsageInfo {
	t.Helper()
	for _, e := range reply.GetList() {
		if e.GetApiCode() == apiCode && e.GetWindowSeconds() == window {
			return e
		}
	}
	t.Fatalf("响应里没有 %s/%ds 条目，实得 %d 条：%+v", apiCode, window, len(reply.GetList()), reply)
	return nil
}

func usageCodes(reply *rpc.ListQuotaUsageReply) []string {
	out := make([]string, 0, len(reply.GetList()))
	for _, e := range reply.GetList() {
		out = append(out, fmt.Sprintf("%s/%d", e.GetApiCode(), e.GetWindowSeconds()))
	}
	return out
}

// usageRows 把响应折成逐字段可比对的字符串，并按字典序排好（顺序不是契约，见 usageRows 的用法）。
func usageRows(reply *rpc.ListQuotaUsageReply) []string {
	out := make([]string, 0, len(reply.GetList()))
	for _, e := range reply.GetList() {
		out = append(out, fmt.Sprintf("%s/%ds app=%d start=%d used=%d limit=%d remaining=%d updated=%d",
			e.GetApiCode(), e.GetWindowSeconds(), e.GetAppId(), e.GetWindowStart(),
			e.GetUsed(), e.GetLimit(), e.GetRemaining(), e.GetUpdatedAt()))
	}
	sortStrings(out)
	return out
}

// assertUsageCodes 比对汇总视图条目（"code/window"）。
//
// 接口名升序成组是实现的硬承诺（quotaUsageAPICodes 里 sortStrings 过），所以单独钉；
// 但同一接口内各窗口的先后顺序来自 ListCandidates 的返回序 —— 生产是行序、fake 是 map 序，
// 把它当断言等于把 fake 的偶然顺序误当成契约，故这里只钉集合相等。
func assertUsageCodes(t *testing.T, reply *rpc.ListQuotaUsageReply, want []string, label string) {
	t.Helper()
	got := usageCodes(reply)
	if len(got) != len(want) {
		t.Fatalf("%s：条目 %v，期望 %d 条 %v", label, got, len(want), want)
	}
	gotSorted, wantSorted := append([]string(nil), got...), append([]string(nil), want...)
	sortStrings(gotSorted)
	sortStrings(wantSorted)
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Fatalf("%s：条目集合=%v，期望 %v", label, got, want)
		}
	}
	prev := ""
	for _, e := range got {
		code := e[:strings.LastIndex(e, "/")]
		if code < prev {
			t.Fatalf("%s：接口名未升序成组：%v", label, got)
		}
		prev = code
	}
}

// readCounts 取若干读侧方法的调用计数基线。
// snapshotWrites 只覆盖 writeOps 里的写侧方法；「某条读路径一次都没走过」这类断言
// 必须单独取基线，否则 key 缺失只能取到 map 零值，同一 store 上累计一旦非零就假失败。
func readCounts(db *store, ops ...string) map[string]int {
	out := make(map[string]int, len(ops))
	for _, op := range ops {
		out[op] = db.count(op)
	}
	return out
}

// wantOnlyWriteAttempt 钉「失败的那一次写就是全部写副作用」。
// db.failOn 是先计数再返回错误，所以这种用例里 wantNoWrites 必然假失败：
// 放行被注入故障的那一次尝试（增量 1），其余写侧方法必须一次都没发生。
func wantOnlyWriteAttempt(t *testing.T, db *store, before map[string]int, op, label string) {
	t.Helper()
	for name, n := range before {
		want := 0
		if name == op {
			want = 1
		}
		if got := db.count(name) - n; got != want {
			t.Fatalf("%s：%s 调用增量=%d，期望 %d", label, name, got, want)
		}
	}
}

// ---------------------------------------------------------------- 配额规则写入

func TestUpsertQuotaPolicy_CreatedRowMatchesRequest(t *testing.T) {
	db, s := quotaFixture(t)
	before := snapshotWrites(db)
	rules := len(db.policies)

	reply, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 10))
	reply = wantOK(t, reply, err, "新建规则")
	if !reply.GetCreated() {
		t.Fatal("首次写入必须回 created=true，否则运营无法区分「改现有规则」与「新增一档」")
	}
	row := quotaRuleAt(db, testAppID, quotaCode, quotaWin)
	if row == nil {
		t.Fatal("响应成功却没有规则行")
	}
	if reply.GetPolicyId() != row.PolicyID || row.PolicyID <= 0 {
		t.Fatalf("响应 policy_id=%d 与入库行 %d 不一致", reply.GetPolicyId(), row.PolicyID)
	}
	// 逐字段等于请求：reason 走 Disable 通道，这里必须留空，
	// 否则审计列会被一句「运营改了限额」填成噪声。
	if row.AppID != testAppID || row.APICode != quotaCode || row.WindowSeconds != quotaWin ||
		row.QuotaLimit != 10 || row.Enabled != 1 || row.Operator != testMid || row.Reason != "" {
		t.Fatalf("入库行与请求不符：%+v", row)
	}
	if row.Ctime == 0 || row.Mtime == 0 {
		t.Fatalf("新行的时间列未落：%+v", row)
	}
	if len(db.policies) != rules+1 {
		t.Fatalf("规则行数 %d → %d，期望 +1", rules, len(db.policies))
	}
	wantCalls(t, db, "QuotaPolicies.Upsert", before["QuotaPolicies.Upsert"], 1, "一次写入只调一次 UPSERT")
	wantCalls(t, db, "QuotaPolicies.Disable", before["QuotaPolicies.Disable"], 0, "启用不走 Disable")

	// 写侧与读侧口径必须一致：新限额立刻出现在生效层级里（下一个窗口边界即生效）。
	usage, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: quotaCode})
	usage = wantOK(t, usage, err, "写入后立刻读用量")
	if e := usageEntry(t, usage, quotaCode, quotaWin); e.GetLimit() != 10 {
		t.Fatalf("生效限额回显 %d，期望 10", e.GetLimit())
	}
}

func TestUpsertQuotaPolicy_SameUniqueKeyUpdatesInPlace(t *testing.T) {
	db, s := quotaFixture(t)

	first, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 10))
	first = wantOK(t, first, err, "首次写入")
	row := quotaRuleAt(db, testAppID, quotaCode, quotaWin)
	firstCtime, firstID := row.Ctime, row.PolicyID

	// 同唯一键、换限额与操作人：必须是原地更新（created=false、policy_id 与 ctime 不变），
	// 「删了重插」会把审计时间线与 policy_id 引用一起打掉。
	second := upsertPolicyReq(testAppID, quotaCode, quotaWin, 6)
	second.OperatorMid = testMid + 1
	reply2, err := callUpsert(t, s, second)
	reply2 = wantOK(t, reply2, err, "同唯一键再次写入")
	if reply2.GetCreated() {
		t.Fatal("命中唯一键却回 created=true")
	}
	if reply2.GetPolicyId() != firstID {
		t.Fatalf("policy_id %d → %d，唯一键命中必须复用原行", firstID, reply2.GetPolicyId())
	}
	if n := countQuotaRuleAt(db, testAppID, quotaCode, quotaWin); n != 1 {
		t.Fatalf("同唯一键有 %d 行，期望 1", n)
	}
	row = quotaRuleAt(db, testAppID, quotaCode, quotaWin)
	if row.QuotaLimit != 6 || row.Operator != testMid+1 || row.Ctime != firstCtime {
		t.Fatalf("原地更新不符：limit=%d op=%d ctime=%d（期望 6/%d/%d）",
			row.QuotaLimit, row.Operator, row.Ctime, testMid+1, firstCtime)
	}

	// 窗口长度是唯一键的一部分：另一档窗口是新增规则，不是覆盖。
	hourly, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWinHr, 200))
	hourly = wantOK(t, hourly, err, "新增小时窗口")
	if !hourly.GetCreated() || hourly.GetPolicyId() == firstID {
		t.Fatalf("不同窗口长度却复用/未新增：created=%t id=%d", hourly.GetCreated(), hourly.GetPolicyId())
	}

	// policy_id 只是可选定位辅助：与唯一键算出的行不同一条就是「以为在改 A 其实改了 B」。
	sameTriple := upsertPolicyReq(testAppID, quotaCode, quotaWin, 8)
	sameTriple.PolicyId = firstID
	ok, err := callUpsert(t, s, sameTriple)
	ok = wantOK(t, ok, err, "policy_id 与唯一键一致")
	if ok.GetCreated() || ok.GetPolicyId() != firstID {
		t.Fatalf("policy_id 一致时形态变了：created=%t id=%d", ok.GetCreated(), ok.GetPolicyId())
	}

	before := snapshotWrites(db)
	stateBefore := quotaRuleState(db)
	mismatch := upsertPolicyReq(testAppID, quotaCode, quotaWinHr, 9)
	mismatch.PolicyId = firstID // 指向分钟窗口那行，唯一键却算出小时窗口
	reply, err := callUpsert(t, s, mismatch)
	wantFail(t, reply, err, errInvalidPolicyID, "policy_id 与唯一键不同行")

	ghost := upsertPolicyReq(testAppID, quotaCode, quotaWin, 9)
	ghost.PolicyId = 424242
	reply, err = callUpsert(t, s, ghost)
	wantFail(t, reply, err, model.ErrQuotaPolicyNotFound, "policy_id 指向不存在的行")

	wantNoWrites(t, db, before, "policy_id 一致性拒绝")
	assertRuleStateUnchanged(t, db, stateBefore, "policy_id 一致性拒绝")
}

func TestUpsertQuotaPolicy_RejectsIllegalParamsWithoutWrites(t *testing.T) {
	cases := []struct {
		name   string
		want   error
		mutate func(*rpc.UpsertQuotaPolicyReq)
	}{
		{"缺运营身份", model.ErrOperatorRequired, func(in *rpc.UpsertQuotaPolicyReq) { in.OperatorMid = 0 }},
		{"负运营身份", model.ErrOperatorRequired, func(in *rpc.UpsertQuotaPolicyReq) { in.OperatorMid = -1 }},
		{"负 app_id", model.ErrInvalidAppID, func(in *rpc.UpsertQuotaPolicyReq) { in.AppId = -1 }},
		{"应用不存在", model.ErrAppNotFound, func(in *rpc.UpsertQuotaPolicyReq) { in.AppId = 555 }},
		{"缺 api_code", errAPICodeRequired, func(in *rpc.UpsertQuotaPolicyReq) { in.ApiCode = "" }},
		{"api_code 超列宽", errAPICodeRequired, func(in *rpc.UpsertQuotaPolicyReq) {
			in.ApiCode = strings.Repeat("a", maxAPICodeRunes+1)
		}},
		// requireLen 先 TrimSpace，所以首尾空白救得回来；内嵌空白才是非法字符
		// （逗号/空格会破坏 JoinScopes/SplitScopes 的可逆性）。
		{"api_code 含空白", model.ErrScopeUnknown, func(in *rpc.UpsertQuotaPolicyReq) {
			in.ApiCode = "video publish"
		}},
		{"未开放类目", model.ErrForbiddenScopeCategory, func(in *rpc.UpsertQuotaPolicyReq) {
			in.ApiCode = "member.read"
		}},
		{"窗口为 0", model.ErrWindowInvalid, func(in *rpc.UpsertQuotaPolicyReq) { in.WindowSeconds = 0 }},
		{"窗口为负", model.ErrWindowInvalid, func(in *rpc.UpsertQuotaPolicyReq) { in.WindowSeconds = -60 }},
		{"窗口超上界", model.ErrWindowInvalid, func(in *rpc.UpsertQuotaPolicyReq) {
			in.WindowSeconds = maxQuotaWindowSeconds + 1
		}},
		{"限额为负", model.ErrQuotaLimitInvalid, func(in *rpc.UpsertQuotaPolicyReq) { in.Limit = -1 }},
		{"停用不走本方法", errReasonRequired, func(in *rpc.UpsertQuotaPolicyReq) { in.Enabled = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, s := quotaFixture(t)
			seedQuota(db, testAppID, quotaCode, quotaWin, 10) // 已有一行，用于「不得覆盖」比对
			before := snapshotWrites(db)
			stateBefore := quotaRuleState(db)

			in := upsertPolicyReq(testAppID, quotaCode, quotaWin, 10)
			tc.mutate(in)

			reply, err := callUpsert(t, s, in)
			wantFail(t, reply, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			assertRuleStateUnchanged(t, db, stateBefore, tc.name)
			if reply != nil {
				t.Fatalf("%s 失败却回了响应：%+v", tc.name, reply)
			}
		})
	}
}

func TestUpsertQuotaPolicy_ZeroLimitIsADenyRuleNotUnlimited(t *testing.T) {
	db, s := quotaFixture(t)

	// 任务口径里的「limit<=0 拒」在这一版契约中只对负数成立：
	// quota_limit=0 且 enabled=1 是显式「禁用该接口」（QuotaPolicy.Denied），
	// 把它当参数错拒掉，运营就没有任何停用单个接口的通道（本方法又不收 enabled=false）。
	reply, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 0))
	reply = wantOK(t, reply, err, "limit=0 合法")
	row := quotaRuleAt(db, testAppID, quotaCode, quotaWin)
	if row == nil || !row.Denied() {
		t.Fatalf("limit=0 未落成「禁用」规则：%+v", row)
	}

	// 读面同样把 0 呈现为「额度为 0」，而不是「没有上限」：
	// 负数那半由参数用例钉（见上面的表）。
	usage, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: quotaCode})
	usage = wantOK(t, usage, err, "禁用规则的用量视图")
	e := usageEntry(t, usage, quotaCode, quotaWin)
	if e.GetLimit() != 0 || e.GetRemaining() != 0 {
		t.Fatalf("limit=0 的条目回显 limit=%d remaining=%d，期望 0/0（不得当成无上限）",
			e.GetLimit(), e.GetRemaining())
	}
}

func TestUpsertQuotaPolicy_FailClosedOnDependency(t *testing.T) {
	t.Run("规则写入失败", func(t *testing.T) {
		db, s := quotaFixture(t)
		before := snapshotWrites(db)
		state := quotaRuleState(db)
		db.failOn("QuotaPolicies.Upsert", errFakeDown)
		reply, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 10))
		if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
			t.Fatalf("写入失败必须原样上抛，实际 err=%v", err)
		}
		if reply != nil {
			t.Fatalf("失败不得回半份结果：%+v", reply)
		}
		wantOnlyWriteAttempt(t, db, before, "QuotaPolicies.Upsert", "UPSERT 失败之后没有第二次写")
		assertRuleStateUnchanged(t, db, state, "UPSERT 失败")
		if quotaRuleAt(db, testAppID, quotaCode, quotaWin) != nil {
			t.Fatal("UPSERT 报错却多了规则行")
		}
	})

	t.Run("应用定位失败", func(t *testing.T) {
		db, s := quotaFixture(t)
		before := snapshotWrites(db)
		db.failOn("Apps.FindByID", errFakeDown)
		reply, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 10))
		wantFail(t, reply, err, errFakeDown, "应用读取失败")
		wantNoWrites(t, db, before, "应用读取失败")
	})

	t.Run("policy_id 定位失败", func(t *testing.T) {
		db, s := quotaFixture(t)
		p := seedQuota(db, testAppID, quotaCode, quotaWin, 10)
		before := snapshotWrites(db)
		db.failOn("QuotaPolicies.FindByID", errFakeDown)
		in := upsertPolicyReq(testAppID, quotaCode, quotaWin, 7)
		in.PolicyId = p.PolicyID
		reply, err := callUpsert(t, s, in)
		wantFail(t, reply, err, errFakeDown, "policy_id 读取失败")
		wantNoWrites(t, db, before, "policy_id 读取失败")
	})

	t.Run("层级预判失败只降级日志", func(t *testing.T) {
		db, s := quotaFixture(t)
		// previewQuotaTier 是纯可观测性（响应契约里没有这个字段）：
		// 它读候选集失败时不该把运营改限额的动作一起拒掉。
		db.failOn("QuotaPolicies.ListCandidates", errFakeDown)
		reply, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 10))
		reply = wantOK(t, reply, err, "预判失败仍完成写入")
		if quotaRuleAt(db, testAppID, quotaCode, quotaWin) == nil {
			t.Fatalf("规则没落库：%+v", reply)
		}
	})

	t.Run("写令牌在写库之前", func(t *testing.T) {
		db, s := quotaFixture(t)
		// 正向对照：同一个请求在额度充足的桶里必须成功，否则下面的断言是永真检查。
		okReply, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 10))
		wantOK(t, okReply, err, "正常桶写入")
		beforeOK := snapshotWrites(db)
		state := quotaRuleState(db)

		limited := limitedSvc(db)
		reply, err := callUpsert(t, limited, upsertPolicyReq(testAppID, quotaCode, quotaWinHr, 5))
		wantFail(t, reply, err, model.ErrRateLimited, "写令牌耗尽")
		wantNoWrites(t, db, beforeOK, "限流发生在写库之前")
		assertRuleStateUnchanged(t, db, state, "限流后规则不变")
	})
}

func TestUpsertQuotaPolicy_ConcurrentSameKeyKeepsSingleRow(t *testing.T) {
	db, s := quotaFixture(t)
	before := snapshotWrites(db)

	// 抢跑：另一个运营（或网关重试）在本次 UPSERT 前提交了同一唯一键的行。
	// 唯一键约束在真库里是 INSERT...ON DUPLICATE KEY UPDATE，这里复刻其裁决：
	// 后写者复用先写者的行，而不是插出第二行。
	winner := &model.QuotaPolicy{PolicyID: 777, AppID: testAppID, APICode: quotaCode,
		WindowSeconds: quotaWin, QuotaLimit: 3, Enabled: 1, Operator: testMid + 1}
	winner.Ctime, winner.Mtime = nowTS(), nowTS()
	db.onHit("QuotaPolicies.Upsert", func() { db.policies[winner.PolicyID] = winner })

	reply, err := callUpsert(t, s, upsertPolicyReq(testAppID, quotaCode, quotaWin, 10))
	reply = wantOK(t, reply, err, "唯一键抢跑后的写入")
	if reply.GetCreated() {
		t.Fatal("抢跑行已存在却回 created=true")
	}
	if reply.GetPolicyId() != winner.PolicyID {
		t.Fatalf("policy_id=%d，期望复用抢跑行 %d", reply.GetPolicyId(), winner.PolicyID)
	}
	if n := countQuotaRuleAt(db, testAppID, quotaCode, quotaWin); n != 1 {
		t.Fatalf("并发下留下 %d 行，期望 1", n)
	}
	row := quotaRuleAt(db, testAppID, quotaCode, quotaWin)
	if row.QuotaLimit != 10 || row.Operator != testMid {
		t.Fatalf("后写者未覆盖抢跑行：limit=%d op=%d", row.QuotaLimit, row.Operator)
	}
	wantCalls(t, db, "QuotaPolicies.Upsert", before["QuotaPolicies.Upsert"], 1, "本次只写一次")
}

// ---------------------------------------------------------------- 配额规则分页

func TestListQuotaPolicies_FiltersAndProjectionMatchRows(t *testing.T) {
	db, s := quotaFixture(t)
	base := nowTS()
	own1 := seedQuotaAt(db, testAppID, quotaCode, quotaWin, 10, base-50)
	own2 := seedQuotaAt(db, testAppID, quotaCode, quotaWinHr, 200, base-40)
	wild := seedQuotaAt(db, testAppID, model.AnyAPICode, quotaWinAny, 30, base-30)
	// 全局那行由 quotaFixture 播下（app_id=0 / api_code='*'）：mtime 也刻意错开，
	// 让它在本应用分支里必须不出现、在全局分支里必须唯一。
	var globalRow *model.QuotaPolicy
	for _, p := range db.policies {
		if p.AppID == model.GlobalAppID {
			globalRow = p
			p.Mtime = base - 20
		}
	}
	if globalRow == nil {
		t.Fatal("fixture 没播全局兜底规则")
	}
	off := seedQuotaAt(db, testAppID, quotaCode2, quotaWin, 5, base-10)
	off.Enabled = 0
	off.Reason = "能力下线，保留行以便重算解释"

	before := snapshotWrites(db)
	// 读侧方法不在 snapshotWrites 的集合里，要单独取基线（否则 key 缺失取到 map 零值，
	// 同一 store 上累计调用一旦非零就假失败）。
	peeked := db.count("QuotaUsages.Peek")
	lg := NewListQuotaPoliciesLogic(t.Context(), s)

	// app_id 精确匹配：0 是「全局默认层」而不是「不过滤」。
	appPage, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{AppId: testAppID, OperatorMid: testMid, Ps: 20})
	appPage = wantOK(t, appPage, err, "本应用规则")
	wantIDs := []int64{off.PolicyID, wild.PolicyID, own2.PolicyID, own1.PolicyID} // (mtime, policy_id) 倒序
	if len(appPage.GetList()) != len(wantIDs) {
		t.Fatalf("n=%d，期望 %d：%v", len(appPage.GetList()), len(wantIDs), appPage.GetList())
	}
	for i, info := range appPage.GetList() {
		if info.GetPolicyId() != wantIDs[i] {
			t.Fatalf("第 %d 行 policy_id=%d，期望 %d（排序不稳定会让上游短缓存拿到错页）",
				i, info.GetPolicyId(), wantIDs[i])
		}
	}
	for _, info := range appPage.GetList() {
		var row *model.QuotaPolicy
		for _, p := range db.policies {
			if p.PolicyID == info.GetPolicyId() {
				row = p
			}
		}
		if row == nil {
			t.Fatalf("响应里有库中不存在的规则 %d", info.GetPolicyId())
		}
		if info.GetAppId() != row.AppID || info.GetApiCode() != row.APICode ||
			info.GetWindowSeconds() != row.WindowSeconds || info.GetLimit() != row.QuotaLimit ||
			info.GetEnabled() != (row.Enabled == 1) || info.GetOperator() != row.Operator ||
			info.GetCtime() != row.Ctime || info.GetMtime() != row.Mtime {
			t.Fatalf("规则投影与入库行不符：响应 %+v 行 %+v", info, row)
		}
		// 停用项必须出现在规则读面：它仍解释着历史窗口，藏起来等于让运营看不到事实。
		if row.PolicyID == off.PolicyID && (info.GetEnabled() || info.GetLimit() != 5) {
			t.Fatalf("停用规则被美化成生效：%+v", info)
		}
	}

	globalPage, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{AppId: model.GlobalAppID, OperatorMid: testMid})
	globalPage = wantOK(t, globalPage, err, "全局层规则")
	if len(globalPage.GetList()) != 1 || globalPage.GetList()[0].GetPolicyId() != globalRow.PolicyID {
		t.Fatalf("app_id=0 结果=%+v，期望只有全局那行", globalPage.GetList())
	}

	// api_code="*" 是规则取值而不是「不过滤」，否则「列出通配规则」无法表达。
	starPage, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{
		AppId: testAppID, ApiCode: model.AnyAPICode, OperatorMid: testMid})
	starPage = wantOK(t, starPage, err, "通配规则")
	if len(starPage.GetList()) != 1 || starPage.GetList()[0].GetApiCode() != model.AnyAPICode {
		t.Fatalf("api_code=\"*\" 过滤结果=%+v", starPage.GetList())
	}
	codePage, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{
		AppId: testAppID, ApiCode: quotaCode, OperatorMid: testMid})
	codePage = wantOK(t, codePage, err, "精确接口")
	if len(codePage.GetList()) != 2 {
		t.Fatalf("精确接口过滤 n=%d，期望 2：%+v", len(codePage.GetList()), codePage.GetList())
	}

	wantNoWrites(t, db, before, "规则读面零副作用")
	wantCalls(t, db, "QuotaUsages.Peek", peeked, 0, "规则分页不碰用量表")
}

func TestListQuotaPolicies_CursorWalkAndPageLimits(t *testing.T) {
	db, s := quotaFixture(t)
	base := nowTS()
	// mtime 刻意不与播种顺序同向，并留一对同 mtime 的行（seeded[1] 与 seeded[3]）：
	// 前者挡住「按 policy_id 排也能蒙过」，后者挡住「只按 mtime 排、tie-break 丢了」。
	// 真实排序口径见 model/op_quota_policy.go 的 ListByApp：
	// ORDER BY mtime DESC, policy_id DESC，位点条件 (mtime < ? OR (mtime = ? AND policy_id < ?))。
	mtimes := []int64{base - 30, base - 10, base - 70, base - 10, base - 20}
	var seeded []int64
	for i, m := range mtimes {
		p := seedQuotaAt(db, testAppID, fmt.Sprintf("api.%02d", i), quotaWin, int64(10+i), m)
		seeded = append(seeded, p.PolicyID)
	}
	// 手工排出的期望序：base-10 内 policy_id 大者先（seeded[3] 再 seeded[1]）、
	// 然后 base-20 / base-30 / base-70。
	wantOrder := []int64{seeded[3], seeded[1], seeded[4], seeded[0], seeded[2]}
	lg := NewListQuotaPoliciesLogic(t.Context(), s)

	var walk []int64
	cursor := ""
	for page := 0; page < 6; page++ {
		before := snapshotWrites(db)
		opened := db.count("QuotaPolicies.ListByApp")
		reply, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{
			AppId: testAppID, OperatorMid: testMid, Ps: 2, Cursor: cursor})
		reply = wantOK(t, reply, err, fmt.Sprintf("第 %d 页", page+1))
		if len(reply.GetList()) > 2 {
			t.Fatalf("第 %d 页 n=%d 超过 ps=2", page+1, len(reply.GetList()))
		}
		for _, info := range reply.GetList() {
			walk = append(walk, info.GetPolicyId())
		}
		// 多取一条判 has_more：每页只问一次，读面不许退化成 N+1。
		wantCalls(t, db, "QuotaPolicies.ListByApp", opened, 1, "每页一次查询")
		wantNoWrites(t, db, before, "翻页全程不写库")
		// 尾页同样带位点（trimPage 只在空集时才不给）：续页必须接着最后那一行往下走。
		cursor = reply.GetNextCursor()
		if !reply.GetHasMore() {
			break
		}
		if cursor == "" {
			t.Fatal("has_more=true 却不给位点")
		}
	}
	if len(walk) != 5 {
		t.Fatalf("翻页共取 %d 行，期望 5：%v", len(walk), walk)
	}
	for i := range walk {
		if walk[i] != wantOrder[i] {
			t.Fatalf("第 %d 行 policy_id=%d，期望 %d", i, walk[i], wantOrder[i])
		}
	}
	// 位点走完之后再续一页：必须是空结果而不是回到首页。
	tail, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{
		AppId: testAppID, OperatorMid: testMid, Ps: 2, Cursor: cursor})
	tail = wantOK(t, tail, err, "尾页之后")
	if len(tail.GetList()) != 0 || tail.GetHasMore() {
		t.Fatalf("尾页之后仍有数据：n=%d has_more=%t", len(tail.GetList()), tail.GetHasMore())
	}

	// 未传 ps 走配置默认值；上限本身允许、+1 才拒（只测一侧会漏掉 off-by-one）。
	def, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{AppId: testAppID, OperatorMid: testMid})
	def = wantOK(t, def, err, "默认页大小")
	if len(def.GetList()) != 5 || def.GetHasMore() {
		t.Fatalf("默认页 n=%d has_more=%t，期望 5/false", len(def.GetList()), def.GetHasMore())
	}
	if _, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{
		AppId: testAppID, OperatorMid: testMid, Ps: testMaxPS}); err != nil {
		t.Fatalf("ps 取上限本身应通过：%v", err)
	}

	empty, err := lg.ListQuotaPolicies(&rpc.ListQuotaPoliciesReq{
		AppId: testApp2, OperatorMid: testMid, Ps: 10})
	empty = wantOK(t, empty, err, "无规则应用")
	if len(empty.GetList()) != 0 || empty.GetNextCursor() != "" || empty.GetHasMore() {
		t.Fatalf("空结果形态：n=%d cursor=%q has_more=%t",
			len(empty.GetList()), empty.GetNextCursor(), empty.GetHasMore())
	}
}

func TestListQuotaPolicies_GatesAndFailClosed(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 10)
	lg := NewListQuotaPoliciesLogic(t.Context(), s)

	cases := []struct {
		name string
		in   *rpc.ListQuotaPoliciesReq
		want error
	}{
		{"缺运营身份", &rpc.ListQuotaPoliciesReq{}, model.ErrOperatorRequired},
		{"负运营身份", &rpc.ListQuotaPoliciesReq{OperatorMid: -3}, model.ErrOperatorRequired},
		{"负 app_id", &rpc.ListQuotaPoliciesReq{AppId: -1, OperatorMid: testMid}, model.ErrInvalidAppID},
		{"api_code 超列宽", &rpc.ListQuotaPoliciesReq{OperatorMid: testMid,
			ApiCode: strings.Repeat("x", maxAPICodeRunes+1)}, errAPICodeRequired},
		{"api_code 非法字符", &rpc.ListQuotaPoliciesReq{OperatorMid: testMid,
			ApiCode: "a|b"}, model.ErrScopeUnknown},
		{"ps 超上限", &rpc.ListQuotaPoliciesReq{OperatorMid: testMid, Ps: testMaxPS + 1}, model.ErrPsTooLarge},
		{"ps 负数", &rpc.ListQuotaPoliciesReq{OperatorMid: testMid, Ps: -1}, model.ErrInvalidPage},
		{"游标非法", &rpc.ListQuotaPoliciesReq{OperatorMid: testMid, Cursor: "not-a-cursor"},
			model.ErrInvalidCursor},
	}
	for _, tc := range cases {
		before := snapshotWrites(db)
		reply, err := lg.ListQuotaPolicies(tc.in)
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
	}

	// 依赖故障必须整次失败：回一份缺列的半页比报错更糟。
	// 本方法的依赖只有规则表分页读这一处 —— 它刻意不查应用行（Apps.FindByID）：
	// 运营要能列出已下架应用的历史规则，规则是限额真值，与应用的生死无关。
	db, s = quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 10)
	{
		before := snapshotWrites(db)
		appReads := db.count("Apps.FindByID")
		db.failOn("QuotaPolicies.ListByApp", errFakeDown)
		reply, err := NewListQuotaPoliciesLogic(t.Context(), s).ListQuotaPolicies(
			&rpc.ListQuotaPoliciesReq{AppId: testAppID, OperatorMid: testMid, Ps: 10})
		if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
			t.Fatalf("QuotaPolicies.ListByApp 故障必须整次失败，实际 err=%v", err)
		}
		if reply != nil {
			t.Fatalf("QuotaPolicies.ListByApp 故障却回了半页：%+v", reply)
		}
		wantNoWrites(t, db, before, "QuotaPolicies.ListByApp")
		wantCalls(t, db, "Apps.FindByID", appReads, 0, "规则读面不依赖应用行")
	}
}

// ---------------------------------------------------------------- 配额用量读面

func TestListQuotaUsage_UsedComesFromUsageRowsNotCallLogs(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	ws := quotaPastWindow(quotaWin)
	row := seedUsage(db, testAppID, quotaCode, quotaWin, ws, 42)
	row.LimitSnapshot = 7 // 记账时的旧限额：绝不能当「当前限额」回显
	// 流水里同窗口只有 3 次：如果读面改成聚合流水，这条断言就会变红。
	seedCallLogs(db, testAppID, quotaCode, ws+10, 3)
	before := snapshotWrites(db)
	logReads := readCounts(db, "CallLogs.ListWindowTotals", "CallLogs.CountByWindow")

	reply, err := callUsage(t, s, &rpc.ListQuotaUsageReq{
		AppId: testAppID, ApiCode: quotaCode, WindowStart: ws, OperatorMid: testMid})
	reply = wantOK(t, reply, err, "用量投影")
	e := usageEntry(t, reply, quotaCode, quotaWin)
	if e.GetAppId() != testAppID || e.GetWindowSeconds() != quotaWin || e.GetWindowStart() != ws {
		t.Fatalf("条目定位列不符：%+v", e)
	}
	if e.GetUsed() != 42 {
		t.Fatalf("used=%d，期望 42（投影行才是记账口径，流水只用于重算）", e.GetUsed())
	}
	if e.GetLimit() != 100 {
		t.Fatalf("limit=%d，期望当前生效限额 100 而不是快照 7", e.GetLimit())
	}
	if e.GetRemaining() != 58 {
		t.Fatalf("remaining=%d，期望 100-42=58", e.GetRemaining())
	}
	if e.GetUpdatedAt() != row.UpdatedAt {
		t.Fatalf("updated_at=%d 与投影行 %d 不一致", e.GetUpdatedAt(), row.UpdatedAt)
	}
	// 快照与生效限额不同本身就意味着「限额漂移」，读面把它保留成可见的事实：
	// 投影行仍是 7，响应仍是 100。
	if row.LimitSnapshot != 7 {
		t.Fatalf("读面改写了 limit_snapshot：%d", row.LimitSnapshot)
	}

	wantCalls(t, db, "CallLogs.ListWindowTotals", logReads["CallLogs.ListWindowTotals"], 0,
		"用量读面不聚合流水")
	wantCalls(t, db, "CallLogs.CountByWindow", logReads["CallLogs.CountByWindow"], 0, "同上")
	wantCalls(t, db, "QuotaUsages.Add", before["QuotaUsages.Add"], 0, "读面绝不扣减")
	wantNoWrites(t, db, before, "ListQuotaUsage 只读")
}

func TestListQuotaUsage_DefaultWindowIsAlignedCurrentWindow(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	ws := quotaPastWindow(quotaWin)
	seedUsage(db, testAppID, quotaCode, quotaWin, ws, 42) // 历史窗口有账
	before := snapshotWrites(db)

	reply, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: quotaCode})
	reply = wantOK(t, reply, err, "window_start 未传")
	e := usageEntry(t, reply, quotaCode, quotaWin)
	now := nowTS()
	// 与扣减面同一个取齐口径：起点必须是 w 的整数倍，并且就是包含 now 的那个窗口。
	if e.GetWindowStart()%quotaWin != 0 {
		t.Fatalf("window_start=%d 未取齐到 %d 的整数倍", e.GetWindowStart(), quotaWin)
	}
	if !(e.GetWindowStart() <= now && now < e.GetWindowStart()+quotaWin) {
		t.Fatalf("window_start=%d 不是当前窗口（now=%d）", e.GetWindowStart(), now)
	}
	if e.GetUsed() != 0 || e.GetRemaining() != 100 {
		t.Fatalf("缺失窗口行应呈现 used=0/remaining=limit：%+v", e)
	}
	wantNoWrites(t, db, before, "缺失窗口行按 0 呈现")

	// 显式传值时按传入值精确定位（回看刚过去的窗口）。
	back, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: quotaCode, WindowStart: ws})
	back = wantOK(t, back, err, "显式窗口起点")
	if e2 := usageEntry(t, back, quotaCode, quotaWin); e2.GetWindowStart() != ws || e2.GetUsed() != 42 {
		t.Fatalf("显式窗口定位失败：%+v", e2)
	}
}

func TestListQuotaUsage_OnlyOneTierTakesEffect(t *testing.T) {
	db, s := quotaFixture(t)
	ownExact := seedQuota(db, testAppID, quotaCode, quotaWin, 10)
	ownHour := seedQuota(db, testAppID, quotaCode, quotaWinHr, 200)
	ownWild := seedQuota(db, testAppID, model.AnyAPICode, quotaWinAny, 30)
	globExact := seedQuota(db, model.GlobalAppID, quotaCode, quotaWin, 999)
	globalWild := int64(0)
	for id, p := range db.policies {
		if p.AppID == model.GlobalAppID && p.APICode == model.AnyAPICode {
			globalWild = id
		}
	}

	lg := NewListQuotaUsageLogic(t.Context(), s)
	usage := func() *rpc.ListQuotaUsageReply {
		t.Helper()
		reply, err := lg.ListQuotaUsage(&rpc.ListQuotaUsageReq{
			AppId: testAppID, ApiCode: quotaCode, WindowStart: quotaPastWindow(quotaWin)})
		return wantOK(t, reply, err, "层级视图")
	}

	// app+精确层存在时，本应用通配层与整个全局层都不参与判定（层级只取一层）。
	full := usage()
	if len(full.GetList()) != 2 {
		t.Fatalf("生效层级条目数=%d，期望 2：%v", len(full.GetList()), usageCodes(full))
	}
	if e := usageEntry(t, full, quotaCode, quotaWin); e.GetLimit() != 10 {
		t.Fatalf("app+精确限额=%d，期望 %d", e.GetLimit(), ownExact.QuotaLimit)
	}
	if e := usageEntry(t, full, quotaCode, quotaWinHr); e.GetLimit() != 200 {
		t.Fatalf("app+精确小时限额=%d，期望 200", e.GetLimit())
	}

	// 撤掉 app 精确层 → 落到 app 通配层（只有它自己的窗口）。
	for id, p := range db.policies {
		if p.AppID == testAppID && p.APICode == quotaCode {
			delete(db.policies, id)
		}
	}
	afterWild := usage()
	if len(afterWild.GetList()) != 1 {
		t.Fatalf("通配层条目=%v，期望 1 条", usageCodes(afterWild))
	}
	if e := usageEntry(t, afterWild, quotaCode, quotaWinAny); e.GetLimit() != 30 ||
		e.GetAppId() != testAppID {
		t.Fatalf("app 通配层限额回显 %+v，期望 30（窗口 %ds）", e, ownWild.WindowSeconds)
	}

	// 再撤掉 app 通配层 → 全局精确优先于全局通配。
	delete(db.policies, ownWild.PolicyID)
	delete(db.policies, globalWild)
	afterGlobal := usage()
	if len(afterGlobal.GetList()) != 1 {
		t.Fatalf("全局层条目=%v，期望 1 条", usageCodes(afterGlobal))
	}
	if e := usageEntry(t, afterGlobal, quotaCode, quotaWin); e.GetLimit() != globExact.QuotaLimit {
		t.Fatalf("全局精确限额=%d，期望 %d（不得被全局通配的 1000 遮蔽）", e.GetLimit(), globExact.QuotaLimit)
	}
	_ = ownHour
}

func TestListQuotaUsage_WildcardRuleAccountsUnderRealAPICode(t *testing.T) {
	db := newStore()
	seedApp(db, testAppID, testOwner)
	seedQuota(db, testAppID, model.AnyAPICode, quotaWin, 5) // 本应用只有通配规则
	s := newTestSvc(db)
	ws := quotaPastWindow(quotaWin)
	seedUsage(db, testAppID, quotaCode, quotaWin, ws, 3) // 真实接口名记账

	lg := NewListQuotaUsageLogic(t.Context(), s)
	byCode, err := lg.ListQuotaUsage(&rpc.ListQuotaUsageReq{
		AppId: testAppID, ApiCode: quotaCode, WindowStart: ws})
	byCode = wantOK(t, byCode, err, "精确接口")
	if e := usageEntry(t, byCode, quotaCode, quotaWin); e.GetUsed() != 3 || e.GetLimit() != 5 {
		t.Fatalf("通配规则下的真实接口用量=%+v，期望 used=3/limit=5", e)
	}

	// 汇总视图里 '*' 自身也是一条目录条目，但它恒 used=0：
	// 窗口行的 api_code 列存的是真实接口名，不存在 '*' 行。
	sum, err := lg.ListQuotaUsage(&rpc.ListQuotaUsageReq{AppId: testAppID, WindowStart: ws})
	sum = wantOK(t, sum, err, "汇总视图")
	if len(sum.GetList()) != 1 || sum.GetList()[0].GetApiCode() != model.AnyAPICode {
		t.Fatalf("汇总视图=%v，期望只有 *", usageCodes(sum))
	}
	if sum.GetList()[0].GetUsed() != 0 {
		t.Fatalf("通配条目 used=%d，期望 0（通配层不直接记账）", sum.GetList()[0].GetUsed())
	}
}

func TestListQuotaUsage_SummaryExpandsEveryWindowAndRejectsOversizedCatalog(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 10)
	seedQuota(db, testAppID, quotaCode, quotaWinHr, 200)
	seedQuota(db, testAppID, quotaCode2, quotaWin, 5)
	before := snapshotWrites(db)

	// 汇总视图：接口名升序，同接口按生效窗口各展开一条；全局兜底行被本应用层遮蔽。
	sum, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, WindowStart: quotaPastWindow(quotaWin)})
	sum = wantOK(t, sum, err, "汇总视图")
	want := []string{quotaCode + "/60", quotaCode + "/3600", quotaCode2 + "/60"}
	assertUsageCodes(t, sum, want, "汇总视图")

	// 本应用没有规则时退回全局默认层：那才是它实际撞的限额。
	own := newStore()
	seedApp(own, testAppID, testOwner)
	seedQuota(own, model.GlobalAppID, model.AnyAPICode, quotaWin, 1000)
	seedQuota(own, model.GlobalAppID, quotaCode, quotaWinHr, 50)
	fallback, err := callUsage(t, newTestSvc(own), &rpc.ListQuotaUsageReq{AppId: testAppID})
	fallback = wantOK(t, fallback, err, "回退全局层")
	if got := usageCodes(fallback); !equalStrings(got,
		[]string{model.AnyAPICode + "/60", quotaCode + "/3600"}) {
		t.Fatalf("全局层枚举=%v，期望通配与精确两条", got)
	}

	// 枚举上限是硬护栏：超出时报错要求显式传 api_code，而不是静默截断成「就这些接口有用量」。
	for i := 0; i < 31; i++ {
		seedQuota(db, testAppID, fmt.Sprintf("api.%02d", i), quotaWin, 20)
	}
	tooMany, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID})
	wantFail(t, tooMany, err, errTooManyQuotaAPIs, "接口数超汇总上限")
	// 显式指定接口仍然可查（这正是报错要引导的出路）。
	single, err := callUsage(t, s, &rpc.ListQuotaUsageReq{
		AppId: testAppID, ApiCode: "api.07", WindowStart: quotaPastWindow(quotaWin)})
	single = wantOK(t, single, err, "超限后显式查询")
	if len(single.GetList()) != 1 || single.GetList()[0].GetLimit() != 20 {
		t.Fatalf("显式查询=%+v", single.GetList())
	}

	// 身份口径：owner 自查（operator_mid=0）与运营读到的内容必须相同，区别只进日志。
	ownerView, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: quotaCode,
		WindowStart: quotaPastWindow(quotaWin)})
	ownerView = wantOK(t, ownerView, err, "owner 自查")
	opView, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: quotaCode,
		WindowStart: quotaPastWindow(quotaWin), OperatorMid: testMid})
	opView = wantOK(t, opView, err, "运营查询")
	// 比的是逐字段内容而不是位置：同接口内各窗口的先后来自 ListCandidates 的返回序
	// （生产是行序、fake 是 map 序），顺序不是契约，「两条响应一模一样」才是。
	if ownerRows, opRows := usageRows(ownerView), usageRows(opView); !equalStrings(ownerRows, opRows) {
		t.Fatalf("同一查询因身份不同而结果不同：%v vs %v", ownerRows, opRows)
	}
	if len(usageRows(opView)) != 2 {
		t.Fatalf("显式接口查询展开 %d 条，期望 2（分钟+小时两个生效窗口）：%v",
			len(usageRows(opView)), usageRows(opView))
	}
	wantNoWrites(t, db, before, "用量读面全程零副作用")
}

func TestListQuotaUsage_GatesAndFailClosed(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListQuotaUsageReq
		want error
	}{
		{"app_id=0 不是用量口径", &rpc.ListQuotaUsageReq{}, model.ErrInvalidAppID},
		{"负 app_id", &rpc.ListQuotaUsageReq{AppId: -1}, model.ErrInvalidAppID},
		{"负运营位", &rpc.ListQuotaUsageReq{AppId: testAppID, OperatorMid: -1}, model.ErrOperatorRequired},
		{"窗口起点为负", &rpc.ListQuotaUsageReq{AppId: testAppID, WindowStart: -5}, model.ErrWindowInvalid},
		{"api_code 非法", &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: "a b"}, model.ErrScopeUnknown},
		{"api_code 超列宽", &rpc.ListQuotaUsageReq{AppId: testAppID,
			ApiCode: strings.Repeat("z", maxAPICodeRunes+1)}, errAPICodeRequired},
	}
	for _, tc := range cases {
		db, s := quotaFixture(t)
		seedQuota(db, testAppID, quotaCode, quotaWin, 10)
		before := snapshotWrites(db)
		reply, err := callUsage(t, s, tc.in)
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
	}

	t.Run("应用不存在", func(t *testing.T) {
		db, s := quotaFixture(t)
		before := snapshotWrites(db)
		reply, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: 555})
		wantFail(t, reply, err, model.ErrAppNotFound, "应用不存在")
		wantNoWrites(t, db, before, "应用不存在")
	})

	t.Run("连兜底规则都没有时失败关闭", func(t *testing.T) {
		db, s := quotaFixture(t)
		for id := range db.policies {
			delete(db.policies, id)
		}
		before := snapshotWrites(db)
		reply, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID, ApiCode: quotaCode})
		// 精确查询必须报错：回空数组等于告诉运营「这应用没限额」，而实际是 seed 丢了。
		wantFail(t, reply, err, model.ErrQuotaPolicyNotFound, "缺兜底规则")
		// 汇总视图的枚举源是规则集，规则集为空时确实无事可列 —— 空结果而不是报错。
		sum, err := callUsage(t, s, &rpc.ListQuotaUsageReq{AppId: testAppID})
		sum = wantOK(t, sum, err, "空目录汇总")
		if len(sum.GetList()) != 0 {
			t.Fatalf("空目录汇总=%v", usageCodes(sum))
		}
		wantNoWrites(t, db, before, "缺兜底规则")
	})

	// 依赖故障逐条 fail closed：两种模式（显式接口 / 汇总）走的查询不同，都要覆盖。
	for _, tc := range []struct {
		op      string
		apiCode string
	}{
		{"Apps.FindByID", quotaCode},
		{"QuotaPolicies.ListCandidates", quotaCode},
		{"QuotaPolicies.ListByApp", ""},
		{"QuotaUsages.Peek", quotaCode},
	} {
		t.Run("依赖故障 "+tc.op, func(t *testing.T) {
			db, s := quotaFixture(t)
			seedQuota(db, testAppID, quotaCode, quotaWin, 10)
			before := snapshotWrites(db)
			db.failOn(tc.op, errFakeDown)
			reply, err := callUsage(t, s, &rpc.ListQuotaUsageReq{
				AppId: testAppID, ApiCode: tc.apiCode, WindowStart: quotaPastWindow(quotaWin)})
			if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
				t.Fatalf("%s 故障必须整次失败，实际 err=%v", tc.op, err)
			}
			if reply != nil {
				t.Fatalf("%s 故障却回了半份用量：%+v", tc.op, reply)
			}
			wantNoWrites(t, db, before, tc.op)
		})
	}
}

// ---------------------------------------------------------------- 配额重算

func TestRecomputeQuota_ConvergesToLogTruthAndIsIdempotent(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	ws := quotaPastWindow(quotaWin)
	row := seedUsage(db, testAppID, quotaCode, quotaWin, ws, 7) // 虚高 4 次
	row.LimitSnapshot = 3
	seedCallLogs(db, testAppID, quotaCode, ws+5, 3) // 真值 3 次
	before := snapshotWrites(db)
	ruleState := quotaRuleState(db)
	logs := len(db.logs)

	reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
	reply = wantOK(t, reply, err, "首次重算")
	if reply.GetWindowsScanned() != 1 || reply.GetWindowsFixed() != 1 {
		t.Fatalf("scanned=%d fixed=%d，期望 1/1", reply.GetWindowsScanned(), reply.GetWindowsFixed())
	}
	if reply.GetMaxDelta() != -4 {
		t.Fatalf("max_delta=%d，期望 -4（虚高 4 次）", reply.GetMaxDelta())
	}
	if row.Used != 3 {
		t.Fatalf("重算后 used=%d，期望流水真值 3", row.Used)
	}
	if row.LimitSnapshot != 100 {
		t.Fatalf("limit_snapshot=%d，期望写回当前生效限额 100", row.LimitSnapshot)
	}
	wantCalls(t, db, "QuotaUsages.RecomputeOverwrite", before["QuotaUsages.RecomputeOverwrite"], 1,
		"只在有漂移时写一次")
	wantCalls(t, db, "QuotaUsages.Add", before["QuotaUsages.Add"], 0, "重算不是扣减")
	wantCalls(t, db, "CallLogs.Insert", before["CallLogs.Insert"], 0, "重算不写流水")

	// 幂等：再跑一次必须「扫到同一个窗口但不再写库」，否则定时任务会把投影表刷成热点。
	again, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
	again = wantOK(t, again, err, "第二次重算")
	if again.GetWindowsScanned() != 1 || again.GetWindowsFixed() != 0 || again.GetMaxDelta() != 0 {
		t.Fatalf("第二次重算 scanned=%d fixed=%d max_delta=%d，期望 1/0/0",
			again.GetWindowsScanned(), again.GetWindowsFixed(), again.GetMaxDelta())
	}
	wantCalls(t, db, "QuotaUsages.RecomputeOverwrite", before["QuotaUsages.RecomputeOverwrite"], 1,
		"已收敛不得重复写库")
	if len(db.usage) != 1 || len(db.logs) != logs {
		t.Fatalf("重算凭空新增行数：usage=%d logs=%d", len(db.usage), len(db.logs))
	}
	// 不变量：重算只修投影，绝不改写已经生效的规则行（规则才是限额真值）。
	assertRuleStateUnchanged(t, db, ruleState, "两次重算之后规则不变")
}

func TestRecomputeQuota_DryRunReportsWithoutWriting(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	ws := quotaPastWindow(quotaWin)
	row := seedUsage(db, testAppID, quotaCode, quotaWin, ws, 7)
	seedCallLogs(db, testAppID, quotaCode, ws+5, 3)
	before := snapshotWrites(db)
	state := quotaRuleState(db)

	reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, true))
	reply = wantOK(t, reply, err, "dry run")
	if reply.GetWindowsScanned() != 1 || reply.GetWindowsFixed() != 1 || reply.GetMaxDelta() != -4 {
		t.Fatalf("dry run 差异报告失真：scanned=%d fixed=%d max_delta=%d",
			reply.GetWindowsScanned(), reply.GetWindowsFixed(), reply.GetMaxDelta())
	}
	if row.Used != 7 {
		t.Fatalf("dry_run=true 却改写了投影：%d", row.Used)
	}
	wantNoWrites(t, db, before, "dry run")
	assertRuleStateUnchanged(t, db, state, "dry run")

	// 真写一次作为对照：dry run 的报告与实际写入结果一致，才说明它不是「空转报错」。
	if _, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false)); err != nil {
		t.Fatalf("非 dry run 失败：%v", err)
	}
	if row.Used != 3 {
		t.Fatalf("实际重算后 used=%d，期望 3", row.Used)
	}
}

func TestRecomputeQuota_RebuildsMissingWindowWithoutDuplicates(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	ws := quotaPastWindow(quotaWin)
	seedCallLogs(db, testAppID, quotaCode, ws+3, 2) // 投影行被清理任务删掉了
	before := snapshotWrites(db)

	reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
	reply = wantOK(t, reply, err, "补建缺失窗口")
	if reply.GetWindowsScanned() != 1 || reply.GetWindowsFixed() != 1 {
		t.Fatalf("scanned=%d fixed=%d，期望 1/1", reply.GetWindowsScanned(), reply.GetWindowsFixed())
	}
	created := db.usage[usageKey(testAppID, quotaCode, quotaWin, ws)]
	if created == nil {
		t.Fatal("流水里有账、投影却没被补回来")
	}
	if created.Used != 2 || created.WindowEnd != ws+quotaWin || created.LimitSnapshot != 100 {
		t.Fatalf("补建行不符：%+v", created)
	}

	// 再跑两次都不该多出第二行（真库靠 uniq (app_id, api_code, window_seconds, window_start)）。
	for i := 0; i < 2; i++ {
		again, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
		again = wantOK(t, again, err, "重复重算")
		if again.GetWindowsFixed() != 0 {
			t.Fatalf("第 %d 次重复重算 fixed=%d，期望 0", i+2, again.GetWindowsFixed())
		}
	}
	if n := len(db.usage); n != 1 {
		t.Fatalf("重复重算留下 %d 行同窗口投影，期望 1", n)
	}
	wantCalls(t, db, "QuotaUsages.RecomputeOverwrite", before["QuotaUsages.RecomputeOverwrite"], 1,
		"只有补建那一次写库")
}

func TestRecomputeQuota_CrossBoundaryWindowUsesWholeWindowTruth(t *testing.T) {
	db := newStore()
	seedApp(db, testAppID, testOwner)
	seedQuota(db, testAppID, quotaCode, quotaWinHr, 500)
	s := newTestSvc(db)

	hb := quotaPastWindow(quotaWinHr)
	seedCallLogs(db, testAppID, quotaCode, hb+600, 1)   // 在区间之前
	seedCallLogs(db, testAppID, quotaCode, hb+2400, 1)  // 同一小时窗口，落在区间内
	seedCallLogs(db, testAppID, quotaCode, hb+4000, 10) // 下一个整小时窗口
	from, to := hb+1800, hb+9000

	reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, from, to, false))
	reply = wantOK(t, reply, err, "跨界窗口重算")
	if reply.GetWindowsScanned() != 2 || reply.GetWindowsFixed() != 2 {
		t.Fatalf("scanned=%d fixed=%d，期望 2/2", reply.GetWindowsScanned(), reply.GetWindowsFixed())
	}
	// 被区间切断的窗口必须按整窗口真值覆盖：拿切断后的计数写回会把一个正常窗口的用量抹小。
	first := db.usage[usageKey(testAppID, quotaCode, quotaWinHr, hb)]
	if first == nil || first.Used != 2 {
		t.Fatalf("跨界窗口 used=%v，期望整窗口真值 2", first)
	}
	second := db.usage[usageKey(testAppID, quotaCode, quotaWinHr, hb+quotaWinHr)]
	if second == nil || second.Used != 10 {
		t.Fatalf("完整窗口 used=%v，期望 10", second)
	}
	if reply.GetMaxDelta() != 10 {
		t.Fatalf("max_delta=%d，期望 10", reply.GetMaxDelta())
	}
}

func TestRecomputeQuota_WildcardRuleNeedsExplicitAPICode(t *testing.T) {
	db := newStore()
	seedApp(db, testAppID, testOwner)
	seedQuota(db, testAppID, model.AnyAPICode, quotaWin, 5) // 只有通配规则
	s := newTestSvc(db)
	ws := quotaPastWindow(quotaWin)
	row := seedUsage(db, testAppID, quotaCode, quotaWin, ws, 9)
	seedCallLogs(db, testAppID, quotaCode, ws+3, 2)
	before := snapshotWrites(db)

	// 全部接口模式的枚举源是目录里的具体接口名；只有通配规则时无事可列，
	// 绝不能拿 api_code='*' 去合并计数（那会伪造用量）。
	all, err := callRecompute(t, s, recomputeReq(testAppID, model.AnyAPICode, ws, ws+quotaWin, false))
	all = wantOK(t, all, err, "通配目录下的全接口重算")
	if all.GetWindowsScanned() != 0 || all.GetWindowsFixed() != 0 {
		t.Fatalf("全接口模式 scanned=%d fixed=%d，期望 0/0", all.GetWindowsScanned(), all.GetWindowsFixed())
	}
	if row.Used != 9 {
		t.Fatalf("全接口模式改写了投影：%d", row.Used)
	}
	wantNoWrites(t, db, before, "通配目录下的全接口重算")

	// 显式传接口才真正按通配规则的窗口长度收敛。
	one, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
	one = wantOK(t, one, err, "显式接口重算")
	if one.GetWindowsScanned() != 1 || one.GetWindowsFixed() != 1 {
		t.Fatalf("scanned=%d fixed=%d，期望 1/1", one.GetWindowsScanned(), one.GetWindowsFixed())
	}
	if row.Used != 2 {
		t.Fatalf("used=%d，期望流水真值 2", row.Used)
	}
	if row.LimitSnapshot != 5 {
		t.Fatalf("limit_snapshot=%d，期望取通配规则的限额 5", row.LimitSnapshot)
	}
}

func TestRecomputeQuota_GatesAndRangeLimits(t *testing.T) {
	ws := quotaPastWindow(quotaWin)
	zeroWin := recomputeReq(testAppID, quotaCode, ws, ws, false)
	cases := []struct {
		name     string
		lookback int64
		in       *rpc.RecomputeQuotaReq
		want     error
	}{
		{"缺运营身份", 0, gateReq(ws, func(in *rpc.RecomputeQuotaReq) { in.OperatorMid = 0 }),
			model.ErrOperatorRequired},
		{"负运营身份", 0, gateReq(ws, func(in *rpc.RecomputeQuotaReq) { in.OperatorMid = -1 }),
			model.ErrOperatorRequired},
		// proto:490 注释写着「0 = 全部应用」，实现刻意不支持：
		// 冻结的 model 读法在 app_id<=0 时不加 app_id 条件，会把所有应用的流水并进同一个桶，
		// 拿它覆盖单应用行会直接打穿限额语义，所以这里必须拒。
		{"app_id=0 拒", 0, gateReq(ws, func(in *rpc.RecomputeQuotaReq) { in.AppId = 0 }),
			model.ErrInvalidAppID},
		{"负 app_id", 0, gateReq(ws, func(in *rpc.RecomputeQuotaReq) { in.AppId = -1 }),
			model.ErrInvalidAppID},
		{"区间反向", 0, zeroWin, model.ErrWindowInvalid},
		{"区间起点为负", 0, gateReq(-1, func(in *rpc.RecomputeQuotaReq) { in.WindowStart = -1 }),
			model.ErrWindowInvalid},
		{"接口标识非法", 0, gateReq(ws, func(in *rpc.RecomputeQuotaReq) { in.ApiCode = "a b" }),
			model.ErrScopeUnknown},
		{"接口标识超列宽", 0, gateReq(ws, func(in *rpc.RecomputeQuotaReq) {
			in.ApiCode = strings.Repeat("q", maxAPICodeRunes+1)
		}), errAPICodeRequired},
		{"区间超过回溯上界", 60, gateReq(ws, func(in *rpc.RecomputeQuotaReq) {
			in.WindowEnd = in.WindowStart + 60*recomputeRangeSpanFactor + 1
		}), errRecomputeRangeTooWide},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, s := quotaFixture(t)
			seedQuota(db, testAppID, quotaCode, quotaWin, 10)
			if tc.lookback > 0 {
				s.Config.OpenPlatform.QuotaRecomputeLookbackSeconds = tc.lookback
			}
			before := snapshotWrites(db)
			reply, err := callRecompute(t, s, tc.in)
			wantFail(t, reply, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
		})
	}

	t.Run("应用不存在", func(t *testing.T) {
		db, s := quotaFixture(t)
		before := snapshotWrites(db)
		reply, err := callRecompute(t, s, recomputeReq(555, quotaCode, ws, ws+quotaWin, false))
		wantFail(t, reply, err, model.ErrAppNotFound, "应用不存在")
		wantNoWrites(t, db, before, "应用不存在")
	})

	t.Run("默认回溯基准", func(t *testing.T) {
		db, s := quotaFixture(t)
		seedQuota(db, testAppID, quotaCode, quotaWin, 10)
		// 配置缺省时用 defaultRecomputeLookbackSeconds（7200）的 recomputeRangeSpanFactor 倍
		// 作上界：刚好通过、+1 就拒——只测一侧会漏掉边界。
		maxSpan := int64(recomputeRangeSpanFactor) * defaultRecomputeLookbackSeconds
		reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+maxSpan, true))
		if err != nil {
			t.Fatalf("区间取上界本身应通过：%v", err)
		}
		wide := recomputeReq(testAppID, quotaCode, ws, ws+maxSpan+1, true)
		reply, err = callRecompute(t, s, wide)
		wantFail(t, reply, err, errRecomputeRangeTooWide, "区间超上界")
	})
}

// gateReq 造一个除被改动字段外全部合法的负例请求。
func gateReq(ws int64, mutate func(*rpc.RecomputeQuotaReq)) *rpc.RecomputeQuotaReq {
	in := recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false)
	mutate(in)
	return in
}

func TestRecomputeQuota_WindowCapBailsOutAfterConvergingPrefix(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	// 让 120120 秒的区间通过回溯上界检查（配置项本身不是断言对象）。
	s.Config.OpenPlatform.QuotaRecomputeLookbackSeconds = 100000
	start := model.AlignWindow(nowTS(), quotaWin) - quotaWin*int64(recomputeWindowCap+2)
	for i := 0; i < recomputeWindowCap+2; i++ {
		seedUsage(db, testAppID, quotaCode, quotaWin, start+quotaWin*int64(i), 99)
	}
	before := snapshotWrites(db)

	reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, start,
		start+quotaWin*int64(recomputeWindowCap+2), false))
	wantFail(t, reply, err, errRecomputeTooManyWindows, "窗口数超上界")
	if reply != nil {
		t.Fatalf("超限却回了报告：%+v", reply)
	}
	// 上界是「边收敛边检查」而不是先数一遍再写：前 recomputeWindowCap 个窗口已收敛，
	// 剩下的保持旧值。重算本身可重复执行，所以部分收敛是设计而非事故——
	// 但绝不允许凭空多出窗口行。
	var converged, stale int
	for _, u := range db.usage {
		if u.Used == 0 {
			converged++
		} else {
			stale++
		}
	}
	if converged != recomputeWindowCap || stale != 2 {
		t.Fatalf("收敛 %d 行 / 未收敛 %d 行，期望 %d/2", converged, stale, recomputeWindowCap)
	}
	if len(db.usage) != recomputeWindowCap+2 {
		t.Fatalf("行数 %d 变了，期望 %d", len(db.usage), recomputeWindowCap+2)
	}
	wantCalls(t, db, "QuotaUsages.RecomputeOverwrite", before["QuotaUsages.RecomputeOverwrite"],
		recomputeWindowCap, "上界之前每窗口写一次")
}

func TestRecomputeQuota_RuleCatalogScanTruncationFails(t *testing.T) {
	db, s := quotaFixture(t)
	for i := 0; i < quotaRuleScanLimit; i++ {
		seedQuota(db, testAppID, fmt.Sprintf("api.%02d", i), quotaWin, 20)
	}
	ws := quotaPastWindow(quotaWin)
	before := snapshotWrites(db)

	// 枚举被扫描上界截断时必须报错：静默「少算几个接口却报已收敛」比失败更糟。
	reply, err := callRecompute(t, s, recomputeReq(testAppID, model.AnyAPICode, ws, ws+quotaWin, false))
	wantFail(t, reply, err, errTooManyQuotaAPIs, "规则目录读满上界")
	wantNoWrites(t, db, before, "规则目录读满上界")

	// 精确接口模式只扫四批（本应用/全局 × 精确/'*'），不受目录规模影响。
	one, err := callRecompute(t, s, recomputeReq(testAppID, "api.00", ws, ws+quotaWin, false))
	one = wantOK(t, one, err, "精确接口仍可重算")
	if one.GetWindowsScanned() != 0 {
		t.Fatalf("scanned=%d，期望 0（没有该接口的账）", one.GetWindowsScanned())
	}
}

func TestRecomputeQuota_DryRunNeedsNoWritePermit(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	ws := quotaPastWindow(quotaWin)
	row := seedUsage(db, testAppID, quotaCode, quotaWin, ws, 7)
	seedCallLogs(db, testAppID, quotaCode, ws+5, 3)
	before := snapshotWrites(db)

	// 正向对照：额度充足的桶里同一份 dry run 报告是 1 个待修窗口。
	// 少了这一步，下面的「limited 也通过」可能只是因为压根没漂移。
	normal, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, true))
	normal = wantOK(t, normal, err, "正常桶 dry run")
	if normal.GetWindowsFixed() != 1 || normal.GetMaxDelta() != -4 {
		t.Fatalf("对照组报告失真：fixed=%d max_delta=%d，期望 1/-4",
			normal.GetWindowsFixed(), normal.GetMaxDelta())
	}
	wantNoWrites(t, db, before, "dry run 全程零写")

	limited := limitedSvc(db)
	ok, err := callRecompute(t, limited, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, true))
	ok = wantOK(t, ok, err, "dry run 不占写令牌")
	if ok.GetWindowsFixed() != normal.GetWindowsFixed() || row.Used != 7 {
		t.Fatalf("限流桶下 dry run 报告=%+v used=%d，期望与对照组一致且不改写投影", ok, row.Used)
	}

	reply, err := callRecompute(t, limited, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
	wantFail(t, reply, err, model.ErrRateLimited, "真写要占写令牌")
	wantNoWrites(t, db, before, "限流发生在写库之前")
}

func TestRecomputeQuota_FailClosedOnDependency(t *testing.T) {
	for _, tc := range []struct {
		op string
		// attempts 依赖故障发生后仍应发生的写回次数：读面失败要 fail fast（0 次），
		// 只有写回自己失败才是「试了一次没成功」。
		attempts int
		desc     string
	}{
		{"QuotaUsages.ListWindowsInRange", 0, "投影侧读失败"},
		{"CallLogs.ListWindowTotals", 0, "流水真值读失败"},
		{"QuotaUsages.RecomputeOverwrite", 1, "写回失败"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			db, s := quotaFixture(t)
			seedQuota(db, testAppID, quotaCode, quotaWin, 100)
			ws := quotaPastWindow(quotaWin)
			row := seedUsage(db, testAppID, quotaCode, quotaWin, ws, 7)
			seedCallLogs(db, testAppID, quotaCode, ws+5, 3)
			before := snapshotWrites(db)
			db.failOn(tc.op, errFakeDown)

			reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
			if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
				t.Fatalf("%s 必须整次失败，实际 err=%v", tc.op, err)
			}
			if reply != nil {
				t.Fatalf("%s 失败却回了报告：%+v", tc.op, reply)
			}
			// 写回失败必须保持旧值：半份重算结果既不是真值也不是旧投影，最难排障。
			if row.Used != 7 {
				t.Fatalf("%s 之后投影被改写：%d", tc.op, row.Used)
			}
			wantCalls(t, db, "QuotaUsages.RecomputeOverwrite",
				before["QuotaUsages.RecomputeOverwrite"], tc.attempts, "写回次数")
			// 读面失败不得留下任何写操作：没有真值就写回，等于用旧值覆盖旧值还装作修过。
			if tc.attempts == 0 {
				wantNoWrites(t, db, before, tc.desc)
			}
		})
	}
}

func TestRecomputeQuota_OnlineChargeDuringOverwriteKeepsLogTruth(t *testing.T) {
	db, s := quotaFixture(t)
	seedQuota(db, testAppID, quotaCode, quotaWin, 100)
	ws := quotaPastWindow(quotaWin)
	row := seedUsage(db, testAppID, quotaCode, quotaWin, ws, 7)
	seedCallLogs(db, testAppID, quotaCode, ws+5, 3)
	before := snapshotWrites(db)

	// 交错：重算读完 used=7 之后、写回之前，一次在线扣减抢先提交（7 → 8）。
	// 不加锁、不清空是设计取舍，语义是「下一次重算继续收敛」，
	// 因此本用例要钉的是：最终值等于流水真值，且不多出第二个同窗口行。
	db.onHit("QuotaUsages.RecomputeOverwrite", func() {
		if _, err := s.QuotaUsages.Add(t.Context(), testAppID, quotaCode, quotaWin, ws, 1, 100); err != nil {
			t.Fatalf("在线扣减失败：%v", err)
		}
	})

	reply, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
	reply = wantOK(t, reply, err, "重算与在线扣减交错")
	if row.Used != 3 {
		t.Fatalf("交错后 used=%d，期望流水真值 3", row.Used)
	}
	if n := len(db.usage); n != 1 {
		t.Fatalf("交错留下 %d 行投影，期望 1（同窗口不得插第二行）", n)
	}
	// max_delta 是「写入时刻的差值」：抢跑把旧值抬到 8，所以修正是 -5 而不是 -4。
	if reply.GetWindowsScanned() != 1 || reply.GetWindowsFixed() != 1 || reply.GetMaxDelta() != -5 {
		t.Fatalf("scanned=%d fixed=%d max_delta=%d，期望 1/1/-5",
			reply.GetWindowsScanned(), reply.GetWindowsFixed(), reply.GetMaxDelta())
	}
	wantCalls(t, db, "QuotaUsages.RecomputeOverwrite", before["QuotaUsages.RecomputeOverwrite"], 1,
		"写回只一次")
	wantCalls(t, db, "QuotaUsages.Add", before["QuotaUsages.Add"], 1, "抢跑的在线扣减一次")

	// 收敛性：再跑一次没有漂移可修。
	again, err := callRecompute(t, s, recomputeReq(testAppID, quotaCode, ws, ws+quotaWin, false))
	again = wantOK(t, again, err, "交错后再重算")
	if again.GetWindowsFixed() != 0 || row.Used != 3 {
		t.Fatalf("交错后未收敛：fixed=%d used=%d", again.GetWindowsFixed(), row.Used)
	}
}
