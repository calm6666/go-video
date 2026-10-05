package logic

// authorize_request_test.go：网关前置聚合检查 AuthorizeRequest 的契约测试。
//
// 契约依据：proto:384-415（两种凭证模式、request_id 唯一即幂等、响应十字段）、
// proto:27-28（配额是投影，丢计数只短时放宽限额）、authorizerequestlogic.go 文件头
// 钉住的门禁顺序「参数 → 凭证模式 → 幂等回放 → 凭证/scope → 配额 → 流水」。
//
// 本文件钉住的不变量（逐条对照实现后断言，全部可在实现退化时变红）：
//  1. 判定链不另写一套：oauthDenyCases() 那一张库状态表既喂 IntrospectToken 也喂本方法，
//     同一状态必须给出同一个原因码（两入口是同一个安全属性的两个门，漂移即绕过）；
//     两者唯一允许的分歧是「能不能定位到应用」——只有那一半才决定是否落拒绝流水；
//  2. 拒绝原因只能是固定原因码集合里的值，绝不把下游错误文本回给网关；
//  3. 放行必须同时完成「配额扣减 + 一条流水」，且响应字段 == 入库真值；
//     每条生效规则各扣一次（多窗口不短路），流水只有一行；
//  4. request_id 是硬幂等锚点：命中 uniq_request_id 即原样重放首次判定，
//     不再读凭证、不再扣配额、不再写流水；并发抢跑同样只留一行、只认先落库者的结论；
//  5. fail closed：链路上任何 model 错误都以 gRPC error 返回，绝不折算成
//     「allowed=true」或「allowed=false + 某个原因码」；缺配额规则回 ErrQuotaPolicyNotFound；
//  6. 参数与凭证模式门禁零副作用（不消耗 request_id 命名空间、不扣配额、不写流水）；
//     写令牌桶无余量时限流发生在扣配额之前，因此那条路径同样零写；
//  7. 流水只落摘要与脱敏 IP：明文 access/refresh、完整 client_ip 在整张库里一处都找不到。

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

const (
	authorizeMethod   = "POST"
	authorizePath     = "/api/v1/video/publish"
	authorizeClientIP = "203.0.113.77"
	// authorizeGlobalLimit 全局兜底规则的限额：正例断言 remaining = limit-1 以此为数。
	authorizeGlobalLimit = 1000
)

// authorizeBodyDigest 一枚形态合法的 sha256 摘要（64 位 hex，无 prefix），
// 用于验证「入库前统一成 sha256:<小写 hex>」这条归一口径。
var authorizeBodyDigest = strings.Repeat("ab", 32)

// authorizeKnownDeny 允许出现在 deny_reason 里的固定原因码。
// 集合是显式写出来的（不是从实现里枚举出来的），所以「把 errFakeDown 的文本当原因码回出去」
// 这类退化会当场变红。
var authorizeKnownDeny = map[string]bool{
	denyInactive: true, denyExpired: true, denyRevoked: true,
	denyAppSuspended: true, denyScopeMissing: true, denyConsentMissing: true,
	denyQuotaExceeded: true,
}

// authorizeProbeOps 本方法链路上会真正触碰的 model 算子（逐条对照
// tokencheck.go / quota.go / authorizerequestlogic.go 得到）。
// 这里显式列全是为了让「某个名字 fake 根本没注册」这种断言空转一眼可见：
// 每个名字都在下面的用例里被 failOn 或 wantCalls 真用过一次，写错名字必然当场红。
var authorizeProbeOps = []string{
	"CallLogs.FindByRequestID", "Tokens.FindByAccessHash", "Grants.FindByID",
	"Grants.FindByAppMid", "Apps.FindByID", "AppScopes.FindGrantedScopes",
	"Scopes.FindByScope", "QuotaPolicies.ListCandidates", "QuotaUsages.Add",
	"QuotaUsages.Peek", "CallLogs.Insert",
}

// authorizeFixture 一条「用户已同意、含读写两个权限点、当前可用」的凭证 + 全局兜底配额规则。
// 兜底规则必须有：缺它时放行路径会直接 fail closed 成 ErrQuotaPolicyNotFound，
// 正例就会悄悄退化成「只测了错误分支」。
func authorizeFixture(t *testing.T) (*store, *svc.ServiceContext, *model.Grant, string, string, *model.Token) {
	t.Helper()
	db, s, grant, access, refresh, tok := oauthTokenFixture(t, oauthTokenScopes, 1)
	seedQuota(db, model.GlobalAppID, model.AnyAPICode, quotaWin, authorizeGlobalLimit)
	return db, s, grant, access, refresh, tok
}

func authorizeReq(requestID, access, requiredScope string) *rpc.AuthorizeRequestReq {
	return &rpc.AuthorizeRequestReq{
		RequestId: requestID, AccessToken: access, ApiCode: testAPIC,
		Method: authorizeMethod, Path: authorizePath, RequiredScope: requiredScope,
		BodyDigest: authorizeBodyDigest, ClientIp: authorizeClientIP, TraceId: "trace-1",
	}
}

func callAuthorize(t *testing.T, s *svc.ServiceContext,
	in *rpc.AuthorizeRequestReq) (*rpc.AuthorizeRequestReply, error) {
	t.Helper()
	return NewAuthorizeRequestLogic(t.Context(), s).AuthorizeRequest(in)
}

func authorizeOK(t *testing.T, s *svc.ServiceContext, requestID, access, requiredScope string) (
	*rpc.AuthorizeRequestReply, error) {
	t.Helper()
	return callAuthorize(t, s, authorizeReq(requestID, access, requiredScope))
}

// tokenRowForAccess 用与 logic 同一口径（HMAC(pepper, 明文)）在库里找回 token 行。
// 这一步必须由测试独立算出来：拒绝路径「有没有 app_id 可写」取决于凭证还能不能定位到应用，
// 拿被测代码的中间结果当期望值，等于把判定权交回去。
func tokenRowForAccess(t *testing.T, db *store, s *svc.ServiceContext, access string) *model.Token {
	t.Helper()
	hash, err := model.HashCredential(s.Config.Security.CredentialPepper, access)
	wantOK(t, hash, err, "HashCredential")
	for _, row := range db.tokens {
		if row.AccessHash == hash {
			c := *row
			return &c
		}
	}
	return nil
}

// callLogDump 把整张流水表折成文本，供「明文/完整 IP 绝不落库」的全表扫描断言用。
// mustNoPlaintextStored 不覆盖 op_api_call_log（它是本方法的唯一写表面），故另写一份。
func callLogDump(db *store) string {
	var b strings.Builder
	for _, l := range db.logs {
		b.WriteString(fmt.Sprintf("%+v ", *l))
	}
	return b.String()
}

// wantNoSecretEcho 断言响应文本里找不到任何凭证材料（明文 access/refresh）。
// 不把命中的明文打印出来：失败信息本身会成为第二条泄露路径。
func wantNoSecretEcho(t *testing.T, reply *rpc.AuthorizeRequestReply, label string, plains ...string) {
	t.Helper()
	text := reply.String()
	for _, p := range plains {
		if p != "" && strings.Contains(text, p) {
			t.Fatalf("%s：响应里出现了凭证明文", label)
		}
	}
}

// ---------------------------------------------------------------- 判定链与 Introspect 同源

func TestAuthorizeRequest_DeniesWithTheSameReasonAsIntrospect(t *testing.T) {
	for _, c := range oauthDenyCases() {
		t.Run(c.name, func(t *testing.T) {
			db, s, grant, access, refresh, tok := authorizeFixture(t)
			s2 := c.apply(t, db, s, tok, grant)
			// request_id 里不能带空白（idPart 会拒），所以用例名里的空格换成连字符。
			requestID := "authz-deny-" + strings.ReplaceAll(c.name, " ", "-")
			before := snapshotWrites(db)
			readsBefore := readCounts(db, authorizeProbeOps...)
			logRows := len(db.logs)

			reply, err := authorizeOK(t, s2, requestID, access, c.scope)
			if err != nil {
				t.Fatalf("凭证不通过是业务结论，不该回 gRPC 错误：%v", err)
			}
			if reply.Allowed {
				t.Fatalf("库里已是「%s」却判放行：%+v", c.name, reply)
			}
			if reply.DenyReason != c.deny {
				t.Fatalf("deny_reason=%q，与 IntrospectToken 对同一库状态给出的 %q 不一致",
					reply.DenyReason, c.deny)
			}
			if !authorizeKnownDeny[reply.DenyReason] {
				t.Fatalf("deny_reason=%q 不在固定原因码集合内（自由文本会把下游错误回给网关）",
					reply.DenyReason)
			}
			wantNoSecretEcho(t, reply, "拒绝响应", access, refresh)

			// 两入口唯一被允许的分歧：本方法的调用方是网关（可信），拒绝结论还要落给风控看，
			// 所以「仍能定位到应用」时必须落一条拒绝流水并回标识；定位不到时无 app_id 可写
			// （op_api_call_log.app_id NOT NULL 且 model 拒绝 <=0），只能回结论。
			located := tokenRowForAccess(t, db, s2, access)
			if located == nil {
				if reply.AppId != 0 || reply.Mid != 0 || reply.CallLogId != 0 || len(reply.Scope) != 0 {
					t.Fatalf("凭证定位不到却回了标识（凭空造出的归属会被下游当真）：%+v", reply)
				}
				wantNoWrites(t, db, before, c.name)
			} else {
				if reply.AppId != located.AppID || reply.Mid != located.Mid {
					t.Fatalf("响应归属 app=%d mid=%d，库里凭证是 app=%d mid=%d",
						reply.AppId, reply.Mid, located.AppID, located.Mid)
				}
				if reply.CallLogId <= 0 {
					t.Fatalf("可定位的拒绝没落流水（风控看不到这次拒绝）：%+v", reply)
				}
				wantScopeList(t, reply.Scope, oauthSortedScopes, "拒绝响应回显的 scope 快照")
				wantOnlyWriteAttempts(t, db, before, map[string]int{"CallLogs.Insert": 1}, c.name)
			}
			// 拒绝路径共同的三条底线：不扣配额、不做观测写、不回显配额信号。
			wantCalls(t, db, "QuotaUsages.Add", before["QuotaUsages.Add"], 0, "凭证拒绝不扣配额")
			wantCalls(t, db, "Tokens.TouchUsed", before["Tokens.TouchUsed"], 0, "本方法不做使用时间回写")
			if reply.QuotaLimit != 0 || reply.QuotaRemaining != 0 || reply.WindowResetAt != 0 ||
				reply.RetryAfterSeconds != 0 {
				t.Fatalf("凭证拒绝却给了配额信号（网关会按退避重投）：%+v", reply)
			}

			if located == nil {
				if len(db.logs) != logRows {
					t.Fatalf("无归属的拒绝写出了流水行：%d → %d", logRows, len(db.logs))
				}
				return
			}
			row := db.logs[requestID]
			if row == nil {
				t.Fatal("响应带了 call_log_id，库里却没有对应流水行")
			}
			if len(db.logs) != logRows+1 {
				t.Fatalf("一次拒绝写出 %d 行流水，期望 1 行", len(db.logs)-logRows)
			}
			if row.Allowed != model.CallDenied || row.DenyReason != c.deny {
				t.Fatalf("流水结论 allowed=%d deny=%q，期望 denied/%q", row.Allowed, row.DenyReason, c.deny)
			}
			if row.RequestID != requestID || row.AppID != located.AppID || row.Mid != located.Mid ||
				row.TokenID != located.TokenID || row.GrantID != located.GrantID {
				t.Fatalf("流水归属与凭证行不符：%+v", row)
			}
			if row.APICode != testAPIC || row.Method != authorizeMethod || row.Path != authorizePath {
				t.Fatalf("流水的接口/方法/路径没落全：%+v", row)
			}
			// 拒绝路径没查过配额，因此两个配额列必须是 0 而不是「顺手写个 limit」。
			if row.QuotaLimit != 0 || row.QuotaRemaining != 0 {
				t.Fatalf("凭证拒绝的流水写了配额列 limit=%d remaining=%d", row.QuotaLimit, row.QuotaRemaining)
			}
			// 判定链读的是哪张表也在契约内：读错表就等于绕过了撤销位点。
			if _, ok := db.grants[located.GrantID]; ok && grant.GrantID != located.GrantID {
				t.Fatalf("token 的 grant_id=%d 与种子授权 %d 不是同一条", located.GrantID, grant.GrantID)
			}
			if got := db.count("CallLogs.FindByRequestID") - readsBefore["CallLogs.FindByRequestID"]; got < 1 {
				t.Fatalf("未先查 request_id 就直接判定（幂等锚点被跳过）")
			}
		})
	}
}

// ---------------------------------------------------------------- 放行路径

func TestAuthorizeRequest_AllowsAndChargesOncePerRule(t *testing.T) {
	db, s, _, access, refresh, tok := authorizeFixture(t)
	// 应用层规则压过全局兜底（NarrowPolicies 只取命中的最窄一层），因此这两条
	// 就是生效层级的全部：多窗口同时约束时每条都要扣，短路会让日累计被系统性低估。
	seedQuota(db, testAppID, testAPIC, quotaWin, authorizeGlobalLimit)
	seedQuota(db, testAppID, testAPIC, quotaWinHr, authorizeGlobalLimit*3)
	before := snapshotWrites(db)
	readsBefore := readCounts(db, authorizeProbeOps...)

	reply, err := authorizeOK(t, s, "authz-ok-1", access, testScopeR)
	reply = wantOK(t, reply, err, "正常放行")
	if !reply.Allowed {
		t.Fatalf("可用凭证 + 已获批读权限点被判拒：deny=%q", reply.DenyReason)
	}
	if reply.DenyReason != denyNone {
		t.Fatalf("放行时 deny_reason=%q，期望空串", reply.DenyReason)
	}
	// 响应字段 == 入库真值，而不是「err==nil 就算过」。
	if reply.AppId != tok.AppID || reply.Mid != tok.Mid {
		t.Fatalf("响应归属 app=%d mid=%d，凭证行是 app=%d mid=%d", reply.AppId, reply.Mid, tok.AppID, tok.Mid)
	}
	wantScopeList(t, reply.Scope, oauthSortedScopes, "放行响应回显的 scope 快照")
	if strings.Join(reply.Scope, ",") != tok.Scope {
		t.Fatalf("回显 scope %v 与 token 快照 %q 不一致", reply.Scope, tok.Scope)
	}
	if reply.CallLogId <= 0 {
		t.Fatalf("放行必须回 call_log_id（下游据此对账），实得 %+v", reply)
	}
	if reply.RetryAfterSeconds != 0 {
		t.Fatalf("放行时给了等待提示 %d", reply.RetryAfterSeconds)
	}
	wantNoSecretEcho(t, reply, "放行响应", access, refresh)

	// 配额：两条规则各扣一次，回显取「最紧的一条」（remaining 最小的窗口）。
	wantCalls(t, db, "QuotaUsages.Add", before["QuotaUsages.Add"], 2, "生效层级内每条规则各扣一次")
	wantCalls(t, db, "CallLogs.Insert", before["CallLogs.Insert"], 1, "一次判定只落一行流水")
	// 本方法全程不使用事务：配额是投影、流水是唯一事实源，两者故意不同事务提交
	// （见 persist 的注释）。这条断言的意义在于：一旦有人把写包进事务，
	// 「扣减失败但流水已落」的窗口就会变成另一种形状，必须重新审视。
	wantCalls(t, db, "DB.TransactCtx", before["DB.TransactCtx"], 0, "放行路径不开事务")
	// 拒绝路径之外的另一条底线：AuthorizeRequest 不做使用时间观测写（那是 Introspect 的语义）。
	wantCalls(t, db, "Tokens.TouchUsed", before["Tokens.TouchUsed"], 0, "观测写只属于 IntrospectToken")

	type usageSeen struct{ window, used, limit, start int64 }
	var rows []usageSeen
	for _, u := range db.usage {
		if u.AppID != testAppID || u.APICode != testAPIC {
			t.Fatalf("扣到了别人头上的用量：%+v", u)
		}
		rows = append(rows, usageSeen{u.WindowSeconds, u.Used, u.LimitSnapshot, u.WindowStart})
	}
	if len(rows) != 2 {
		t.Fatalf("用量行数=%d，期望按两条规则各一行：%+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.used != 1 {
			t.Fatalf("窗口 %ds 记成 used=%d，期望 1", r.window, r.used)
		}
		if want := model.AlignWindow(r.start+r.window, r.window); want != r.start+r.window {
			t.Fatalf("窗口起点 %d 未按 %ds 取齐（与读面 AlignWindow 口径漂移）", r.start, r.window)
		}
	}
	minuteWindow := usageRow(t, db, quotaWin)
	hourWindow := usageRow(t, db, quotaWinHr)
	if minuteWindow.LimitSnapshot != authorizeGlobalLimit {
		t.Fatalf("分钟窗口限额快照 %d，期望 %d", minuteWindow.LimitSnapshot, authorizeGlobalLimit)
	}
	if hourWindow.LimitSnapshot != int64(authorizeGlobalLimit)*3 {
		t.Fatalf("小时窗口限额快照 %d，期望 %d", hourWindow.LimitSnapshot, authorizeGlobalLimit*3)
	}
	// 回显的是「最先撞到的墙」：分钟窗口额度更紧，因此 limit/remaining/window_end 都取自它。
	if reply.QuotaLimit != authorizeGlobalLimit {
		t.Fatalf("quota_limit=%d，期望最紧窗口（分钟）的 %d", reply.QuotaLimit, authorizeGlobalLimit)
	}
	if reply.QuotaRemaining != authorizeGlobalLimit-1 {
		t.Fatalf("quota_remaining=%d，期望 %d", reply.QuotaRemaining, authorizeGlobalLimit-1)
	}
	now := nowTS()
	if reply.WindowResetAt%quotaWin != 0 || reply.WindowResetAt <= now ||
		reply.WindowResetAt > now+quotaWin {
		t.Fatalf("window_reset_at=%d 不是当前 %ds 窗口的边界（应为 start+%d）",
			reply.WindowResetAt, quotaWin, quotaWin)
	}
	if got := db.count("QuotaUsages.Peek") - readsBefore["QuotaUsages.Peek"]; got != 0 {
		t.Fatalf("放行路径做了只读回看（Peek %d 次），扣减面只允许 Add", got)
	}

	row := mustCallLog(t, db, "authz-ok-1")
	if row.CallLogID != reply.CallLogId {
		t.Fatalf("响应 call_log_id=%d 与入库行 %d 不一致", reply.CallLogId, row.CallLogID)
	}
	if row.Allowed != model.CallAllowed || row.DenyReason != denyNone {
		t.Fatalf("流水结论 allowed=%d deny=%q", row.Allowed, row.DenyReason)
	}
	if row.AppID != tok.AppID || row.Mid != tok.Mid || row.TokenID != tok.TokenID ||
		row.GrantID != tok.GrantID {
		t.Fatalf("流水归属与凭证行不符：%+v", row)
	}
	if row.APICode != testAPIC || row.Method != authorizeMethod || row.Path != authorizePath {
		t.Fatalf("流水的接口标识/方法/路径没落全：%+v", row)
	}
	if row.QuotaLimit != authorizeGlobalLimit || row.QuotaRemaining != authorizeGlobalLimit-1 {
		t.Fatalf("流水的配额快照 %d/%d 与响应 %d/%d 不一致",
			row.QuotaLimit, row.QuotaRemaining, reply.QuotaLimit, reply.QuotaRemaining)
	}
	// 摘要归一：入库统一 sha256:<小写 hex>，请求体原文绝不落库。
	if row.BodyDigest != "sha256:"+authorizeBodyDigest {
		t.Fatalf("body_digest=%q，期望 sha256:%s", row.BodyDigest, authorizeBodyDigest)
	}
	if row.ClientIPMasked != "v4:203.0.x.x" {
		t.Fatalf("client_ip_masked=%q，期望抹掉末段的 v4:203.0.x.x", row.ClientIPMasked)
	}
	if row.Ctime == 0 {
		t.Fatal("流水未落时间列（重算按 ctime 分窗，缺它就永远收敛不了）")
	}
	// 全表扫描：明文凭证与完整 IP 一处都找不到。
	mustNoPlaintextStored(t, db, "放行流水", access, refresh)
	if dump := callLogDump(db); strings.Contains(dump, authorizeClientIP) ||
		strings.Contains(dump, access) || strings.Contains(dump, refresh) {
		t.Fatal("流水表里出现了明文凭证或完整 client_ip")
	}
	if len(db.logs) != 1 {
		t.Fatalf("一次判定落了 %d 行流水", len(db.logs))
	}
}

// usageRow 按窗口长度定位本应用本接口的用量行（fake 的 map 序不可依赖，故显式找）。
func usageRow(t *testing.T, db *store, window int64) *model.QuotaUsage {
	t.Helper()
	for _, u := range db.usage {
		if u.AppID == testAppID && u.APICode == testAPIC && u.WindowSeconds == window {
			return u
		}
	}
	t.Fatalf("库里没有 %s/%ds 的用量行", testAPIC, window)
	return nil
}

// mustCallLog 按 request_id 找回流水真行（不是 FindByRequestID 的副本）。
func mustCallLog(t *testing.T, db *store, requestID string) *model.ApiCallLog {
	t.Helper()
	row, ok := db.logs[requestID]
	if !ok {
		t.Fatalf("库里没有 request_id=%s 的流水行（现有 %d 行）", requestID, len(db.logs))
	}
	return row
}

// usageTotalFor 某接口在某窗口长度上的累计用量（跨窗口求和）。
// 求和而不是直接读「当前窗口」：断言不能依赖测试与 logic 各自读到的那一次分钟边界。
func usageTotalFor(db *store, appID int64, apiCode string, window int64) int64 {
	var total int64
	for _, u := range db.usage {
		if u.AppID == appID && u.APICode == apiCode && u.WindowSeconds == window {
			total += u.Used
		}
	}
	return total
}

// seedUsageBoth 在当前窗口和下一个同长窗口各预置 used。
// 为什么要多铺一格：logic 的窗口起点是它自己读时钟算出来的，只铺当前窗口时
// 「跨过分钟边界」会让预置量落到 logic 看不见的窗口里，超限用例就会偶发放行。
// 铺两格后 logic 无论对齐到哪一格都必然超限，而「本次只扣 1」仍可被总数证明。
func seedUsageBoth(db *store, appID int64, apiCode string, window, used int64) {
	start := model.AlignWindow(nowTS(), window)
	seedUsage(db, appID, apiCode, window, start, used)
	seedUsage(db, appID, apiCode, window, start+window, used)
}

// wantAlignedFuture 断言 v 是一个「从现在起 window 秒内结束」的对齐窗口边界。
// 不钉具体秒数：logic 与自己各读一次时钟，跨秒抖动不该让断言变成运气测试。
func wantAlignedFuture(t *testing.T, v, now, window int64, label string) {
	t.Helper()
	if v%window != 0 {
		t.Fatalf("%s=%d 不是 %ds 窗口的对齐边界", label, v, window)
	}
	if v <= now || v > now+window {
		t.Fatalf("%s=%d 不在当前 %ds 窗口内（now=%d）", label, v, window, now)
	}
}

// ---------------------------------------------------------------- 配额超限与显式禁用

func TestAuthorizeRequest_QuotaExceededDeniesWithoutRollingBackProjection(t *testing.T) {
	db, s, _, access, refresh, tok := authorizeFixture(t)
	// 应用层精确规则压过全局兜底，生效层级就是这一条 limit=2。
	seedQuota(db, testAppID, testAPIC, quotaWin, 2)
	seedUsageBoth(db, testAppID, testAPIC, quotaWin, 2)
	before := snapshotWrites(db)

	reply, err := authorizeOK(t, s, "authz-quota-1", access, testScopeR)
	reply = wantOK(t, reply, err, "超限判定")
	if reply.Allowed {
		t.Fatal("已用满配额却判放行")
	}
	if reply.DenyReason != denyQuotaExceeded {
		t.Fatalf("deny_reason=%q，期望 %q", reply.DenyReason, denyQuotaExceeded)
	}
	if !authorizeKnownDeny[reply.DenyReason] {
		t.Fatalf("deny_reason=%q 不在固定原因码集合内", reply.DenyReason)
	}
	if reply.QuotaLimit != 2 {
		t.Fatalf("quota_limit=%d，期望回显生效限额 2", reply.QuotaLimit)
	}
	// 超限不给「剩余额度」：回了正数网关就会继续投。
	if reply.QuotaRemaining != 0 {
		t.Fatalf("quota_remaining=%d，超限必须回 0", reply.QuotaRemaining)
	}
	now := nowTS()
	wantAlignedFuture(t, reply.WindowResetAt, now, quotaWin, "window_reset_at")
	if reply.RetryAfterSeconds < 1 || reply.RetryAfterSeconds > quotaWin {
		t.Fatalf("retry_after_seconds=%d，期望 1..%d（0 会让客户端在同一窗口内空转重试）",
			reply.RetryAfterSeconds, quotaWin)
	}
	if reply.AppId != tok.AppID || reply.Mid != tok.Mid {
		t.Fatalf("配额拒绝也要能定位归属，实得 app=%d mid=%d", reply.AppId, reply.Mid)
	}
	wantScopeList(t, reply.Scope, oauthSortedScopes, "配额拒绝回显的 scope")
	wantNoSecretEcho(t, reply, "配额拒绝响应", access, refresh)

	// 记账不回滚：投影必须如实反映「被打过来的次数」，藏起来等于让运营看不见冲击。
	wantCalls(t, db, "QuotaUsages.Add", before["QuotaUsages.Add"], 1, "超限前仍扣一次")
	if got := usageTotalFor(db, testAppID, testAPIC, quotaWin); got != 5 {
		t.Fatalf("累计 used=%d，期望 2+2+本次 1=5（回滚投影就是把被拒用量藏起来）", got)
	}
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"QuotaUsages.Add": 1, "CallLogs.Insert": 1,
	}, "超限拒绝")

	row := mustCallLog(t, db, "authz-quota-1")
	if row.Allowed != model.CallDenied || row.DenyReason != denyQuotaExceeded {
		t.Fatalf("流水结论 allowed=%d deny=%q", row.Allowed, row.DenyReason)
	}
	if row.CallLogID != reply.CallLogId || row.CallLogID <= 0 {
		t.Fatalf("响应 call_log_id=%d 与入库行 %d 不一致", reply.CallLogId, row.CallLogID)
	}
	if row.QuotaLimit != 2 || row.QuotaRemaining != 0 {
		t.Fatalf("流水配额快照 limit=%d remaining=%d，期望 2/0", row.QuotaLimit, row.QuotaRemaining)
	}
	if row.TokenID != tok.TokenID || row.GrantID != tok.GrantID || row.AppID != tok.AppID {
		t.Fatalf("流水归属与凭证行不符：%+v", row)
	}

	t.Run("重放超限结论时回看窗口失败也不改判定", func(t *testing.T) {
		// 等待提示是「尽力而为」的附加信息，不参与判定；读不到就留 0，
		// 但绝不能因为回看失败而把已落库的结论改成放行。
		db.failOn("QuotaUsages.Peek", errFakeDown)
		before2 := snapshotWrites(db)
		got, err := authorizeOK(t, s, "authz-quota-1", access, testScopeR)
		got = wantOK(t, got, err, "重放超限结论")
		if got.Allowed || got.DenyReason != denyQuotaExceeded || got.CallLogId != row.CallLogID {
			t.Fatalf("重放结论被回看失败改写：%+v", got)
		}
		if got.WindowResetAt != 0 || got.RetryAfterSeconds != 0 {
			t.Fatalf("回看失败却给了等待提示：%+v", got)
		}
		wantNoWrites(t, db, before2, "重放超限结论")
	})
}

func TestAuthorizeRequest_DisabledInterfaceDeniesWithoutChargingUsage(t *testing.T) {
	db, s, _, access, _, tok := authorizeFixture(t)
	// quota_limit<=0 且 enabled=1 是显式「禁用该接口」，不是超限：
	// 它必须在扣减之前就把请求挡掉，否则被禁接口的投影会一直涨。
	seedQuota(db, testAppID, testAPIC, quotaWin, 0)
	before := snapshotWrites(db)

	reply, err := authorizeOK(t, s, "authz-disabled-1", access, testScopeR)
	reply = wantOK(t, reply, err, "禁用接口判定")
	if reply.Allowed || reply.DenyReason != denyQuotaExceeded {
		t.Fatalf("禁用接口的结论 %+v，期望 allowed=false deny=%q", reply, denyQuotaExceeded)
	}
	if reply.QuotaLimit != 0 {
		t.Fatalf("quota_limit=%d，禁用接口应回 0", reply.QuotaLimit)
	}
	if reply.RetryAfterSeconds < 1 {
		t.Fatalf("retry_after_seconds=%d，禁用接口也必须给可执行的等待提示", reply.RetryAfterSeconds)
	}
	wantAlignedFuture(t, reply.WindowResetAt, nowTS(), quotaWin, "window_reset_at")
	wantCalls(t, db, "QuotaUsages.Add", before["QuotaUsages.Add"], 0, "禁用接口不累加投影")
	if got := usageTotalFor(db, testAppID, testAPIC, quotaWin); got != 0 {
		t.Fatalf("禁用接口仍写下用量 used=%d", got)
	}
	wantOnlyWriteAttempts(t, db, before, map[string]int{"CallLogs.Insert": 1}, "禁用接口拒绝")

	row := mustCallLog(t, db, "authz-disabled-1")
	if row.Allowed != model.CallDenied || row.DenyReason != denyQuotaExceeded ||
		row.QuotaLimit != 0 || row.AppID != tok.AppID {
		t.Fatalf("禁用接口的流水行不对：%+v", row)
	}
	// 停用规则（enabled=0）不等于禁用接口：它应当被 ListCandidates 过滤掉，
	// 剩下的全局兜底继续生效，请求正常放行。
	db2, s2, _, access2, _, _ := authorizeFixture(t)
	disabled := seedQuota(db2, testAppID, testAPIC, quotaWin, 0)
	disabled.Enabled = 0
	reply2, err := authorizeOK(t, s2, "authz-disabled-2", access2, testScopeR)
	reply2 = wantOK(t, reply2, err, "停用规则不参与判定")
	if !reply2.Allowed {
		t.Fatalf("enabled=0 的规则被当成禁用接口用了（应当回到全局兜底）：%+v", reply2)
	}
	if reply2.QuotaLimit != authorizeGlobalLimit {
		t.Fatalf("生效限额=%d，期望全局兜底 %d", reply2.QuotaLimit, authorizeGlobalLimit)
	}
}

func TestAuthorizeRequest_MissingQuotaPolicyFailsClosedAsError(t *testing.T) {
	// 故意不给全局兜底：一条规则都匹配不上时是运营配置事故，
	// 必须回 gRPC 错误让网关告警，而不是「当成没上限放行」也不是「静默拒绝」。
	db, s, _, access, _, _ := oauthTokenFixture(t, oauthTokenScopes, 1)
	seedQuota(db, testAppID, "some.other.api", quotaWin, 5)
	before := snapshotWrites(db)
	usageRows := len(db.usage)

	reply, err := authorizeOK(t, s, "authz-nopolicy-1", access, testScopeR)
	wantFail(t, reply, err, model.ErrQuotaPolicyNotFound, "缺配额规则")
	wantNoWrites(t, db, before, "缺配额规则")
	if len(db.usage) != usageRows {
		t.Fatalf("缺规则却写出了用量行：%d → %d", usageRows, len(db.usage))
	}
	if len(db.logs) != 0 {
		t.Fatalf("缺规则却落了流水：%d 行", len(db.logs))
	}
	// 凭证判定确实先于配额判定走完（门禁顺序即契约），这里用读次数证明而不是回声断言。
	wantCalls(t, db, "Grants.FindByID", 0, 1, "缺规则前已完成撤销位点比对")
	wantCalls(t, db, "AppScopes.FindGrantedScopes", 0, 1, "缺规则前已完成获批集比对")
	wantCalls(t, db, "Apps.FindByID", 0, 1, "缺规则前已完成应用状态判定")
	wantCalls(t, db, "QuotaPolicies.ListCandidates", 0, 1, "缺规则是查过规则后才知道的")
}

// ---------------------------------------------------------------- 依赖故障一律 fail closed

func TestAuthorizeRequest_FailClosedOnEveryDownstreamError(t *testing.T) {
	// 链路上每个 model 算子都注入一次故障：任何一处都必须以 gRPC error 返回，
	// 既不能伪造「allowed=true」，也不能把错误折算成某个业务原因码后返回 OK。
	// 每条注入的名字都必须真的在路径上（errors.Is 判的就是这个唯一注入的错误），
	// 名字写错时这一次调用会成功，子测试当场红。
	// Scopes.FindByScope 不在表内：它被判定链折叠成业务拒绝，单列在下面的子测试里钉。
	for _, op := range []string{
		"CallLogs.FindByRequestID", "Tokens.FindByAccessHash", "Grants.FindByID",
		"Apps.FindByID", "AppScopes.FindGrantedScopes",
		"QuotaPolicies.ListCandidates", "QuotaUsages.Add", "CallLogs.Insert",
	} {
		t.Run(op, func(t *testing.T) {
			db, s, _, access, _, _ := authorizeFixture(t)
			db.failOn(op, errFakeDown)
			before := snapshotWrites(db)

			reply, err := authorizeOK(t, s, "authz-down-1", access, testScopeR)
			if err == nil {
				t.Fatalf("%s 故障被判成功（fail open）：%+v", op, reply)
			}
			if !errors.Is(err, errFakeDown) {
				t.Fatalf("%s 故障返回的不是注入的错误：%v", op, err)
			}
			if !isNilPtr(reply) {
				t.Fatalf("%s 故障仍带回响应体（网关可能按响应放行）：%+v", op, reply)
			}
			// 流水绝不落库：不存在「出错了但先记一条放行」。
			if len(db.logs) != 0 {
				t.Fatalf("%s 故障后仍有流水行 %d 条", op, len(db.logs))
			}
			// 扣减与写流水故意不同事务（配额是投影）：故障点越靠后，前面已经发生的写越会留下。
			// 这里钉住「留下的是哪几次、各一次」，而不是笼统声称什么都没留。
			quota := map[string]int{}
			if op == "QuotaUsages.Add" || op == "CallLogs.Insert" {
				quota["QuotaUsages.Add"] = 1
			}
			if op == "CallLogs.Insert" {
				quota["CallLogs.Insert"] = 1
			}
			wantOnlyWriteAttempts(t, db, before, quota, op+" 故障后的写表面")
		})
	}

	t.Run("未注入密钥材料时定位凭证即失败关闭", func(t *testing.T) {
		db, s, _, access, _, _ := authorizeFixture(t)
		s.Config.Security.CredentialPepper = ""
		before := snapshotWrites(db)
		reply, err := authorizeOK(t, s, "authz-nopepper-1", access, testScopeR)
		wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "缺 pepper")
		wantNoWrites(t, db, before, "缺 pepper")
	})

	t.Run("token 模型未装配时不猜测结论", func(t *testing.T) {
		db, s, _, access, _, _ := authorizeFixture(t)
		s.Tokens = nil
		before := snapshotWrites(db)
		reply, err := authorizeOK(t, s, "authz-notokens-1", access, testScopeR)
		wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "Tokens 未装配")
		wantNoWrites(t, db, before, "Tokens 未装配")
	})

	t.Run("权限点目录读失败时绝不判放行", func(t *testing.T) {
		// 与 TestIntrospectToken_DependencyFailureIsErrorNotDeny 里同一处已登记观察项对齐：
		// requireScopeConsent 把「目录读失败」和「用户未同意」折叠成同一个 consent_missing
		// （tokencheck.go:160 与 tokencheck.go:178），方向仍是 fail closed，
		// 但一次基础设施故障会被读成「这个用户没点过同意」。
		// 这里钉住两件事：结论必须是拒绝；以及当前那个不精确的原因码本身
		// ——将来区分成因时这条断言会红，逼着改契约的人同步更新两处说明。
		db, s, _, access, _, _ := authorizeFixture(t)
		db.failOn("Scopes.FindByScope", errFakeDown)
		before := snapshotWrites(db)
		reply, err := authorizeOK(t, s, "authz-scopeerr-1", access, testScopeR)
		reply = wantOK(t, reply, err, "目录读失败（当前实现回业务拒绝）")
		if reply.Allowed {
			t.Fatal("权限点目录读失败却判放行")
		}
		if reply.DenyReason != denyConsentMissing {
			t.Fatalf("deny_reason=%q，当前实现把目录故障折叠成 %q",
				reply.DenyReason, denyConsentMissing)
		}
		wantOnlyWriteAttempts(t, db, before, map[string]int{"CallLogs.Insert": 1}, "目录读失败")
	})
}

// ---------------------------------------------------------------- 幂等回放

func TestAuthorizeRequest_ReplayReturnsFirstVerdictAndDoesNotChargeAgain(t *testing.T) {
	db, s, _, access, _, _ := authorizeFixture(t)

	first, err := authorizeOK(t, s, "authz-replay-1", access, testScopeR)
	first = wantOK(t, first, err, "首次判定")
	if !first.Allowed {
		t.Fatalf("首次判定被拒：deny=%q", first.DenyReason)
	}
	usageAfterFirst := usageRow(t, db, quotaWin).Used

	t.Run("重试原样重放，不再扣配额也不再写流水", func(t *testing.T) {
		before := snapshotWrites(db)
		readsBefore := readCounts(db, authorizeProbeOps...)

		second, err := authorizeOK(t, s, "authz-replay-1", access, testScopeR)
		second = wantOK(t, second, err, "重试同一 request_id")
		if second.Allowed != first.Allowed || second.DenyReason != first.DenyReason {
			t.Fatalf("重放结论 %t/%q 与首次 %t/%q 不一致",
				second.Allowed, second.DenyReason, first.Allowed, first.DenyReason)
		}
		if second.CallLogId != first.CallLogId || second.AppId != first.AppId ||
			second.Mid != first.Mid || second.QuotaLimit != first.QuotaLimit ||
			second.QuotaRemaining != first.QuotaRemaining {
			t.Fatalf("重放响应与首次不等：首次 %+v 重放 %+v", first, second)
		}
		wantNoWrites(t, db, before, "幂等重放")
		if got := usageRow(t, db, quotaWin).Used; got != usageAfterFirst {
			t.Fatalf("重放又扣了一次配额：used %d → %d", usageAfterFirst, got)
		}
		if len(db.logs) != 1 {
			t.Fatalf("重放写出了第二行流水：现有 %d 行", len(db.logs))
		}
		// 回放短路在读凭证之前：request_id 命中即返回，凭证判定一次都不做。
		wantCalls(t, db, "CallLogs.FindByRequestID", readsBefore["CallLogs.FindByRequestID"], 1,
			"重放只读一次 request_id")
		wantCalls(t, db, "Tokens.FindByAccessHash", readsBefore["Tokens.FindByAccessHash"], 0,
			"重放不再读凭证")
		wantCalls(t, db, "QuotaPolicies.ListCandidates", readsBefore["QuotaPolicies.ListCandidates"], 0,
			"重放不再读规则")
	})

	t.Run("凭证随后失效也不改重放结论", func(t *testing.T) {
		// 撤销发生在首次判定之后：重放必须回首次的「放行」，否则同一个 request_id
		// 会因重试时机给出两种结论，网关的 401/403 处理就会自相矛盾。
		for _, tok := range db.tokens {
			tok.State = model.TokenStateRevoked
		}
		before := snapshotWrites(db)
		got, err := authorizeOK(t, s, "authz-replay-1", access, testScopeR)
		got = wantOK(t, got, err, "凭证已撤销后重放")
		if !got.Allowed || got.CallLogId != first.CallLogId {
			t.Fatalf("重放被撤销位点改写：%+v", got)
		}
		wantNoWrites(t, db, before, "撤销后重放")
	})

	t.Run("重放的拒绝结论也原样返回", func(t *testing.T) {
		db2, s2, _, access2, _, _ := authorizeFixture(t)
		db2.logs["authz-replay-deny"] = &model.ApiCallLog{
			CallLogID: 555, RequestID: "authz-replay-deny", AppID: testAppID, APICode: testAPIC,
			Mid: testMid, Allowed: model.CallDenied, DenyReason: denyRevoked, Ctime: nowTS(),
		}
		before := snapshotWrites(db2)
		got, err := authorizeOK(t, s2, "authz-replay-deny", access2, testScopeR)
		got = wantOK(t, got, err, "重放既有拒绝结论")
		if got.Allowed || got.DenyReason != denyRevoked || got.CallLogId != 555 {
			t.Fatalf("重放没有原样回首次判定：%+v", got)
		}
		// 文档化的缺口：冻结的流水表没有 scope / 窗口列，重放无法还原，只能回空。
		if len(got.Scope) != 0 || got.WindowResetAt != 0 || got.RetryAfterSeconds != 0 {
			t.Fatalf("重放假装能还原未落库的字段：%+v", got)
		}
		wantNoWrites(t, db2, before, "重放既有拒绝结论")
	})
}

// ---------------------------------------------------------------- 参数与凭证模式门禁

// wantZeroChainContact 断言「这一趟连库都没去过」：判定链上每个 model 算子的调用增量都是 0。
//
// 参数/凭证模式门禁排在幂等回放之前（authorizerequestlogic.go:46-86），所以非法入参
// 必须一次读都没有——只断言「没写」的话，「先查了幂等键再拒」这种退化（真实场景下
// 等于让陌生人用畸形包打穿 DB）会悄悄放过。
func wantZeroChainContact(t *testing.T, db *store, before chainSnapshot, label string) {
	t.Helper()
	for _, op := range authorizeProbeOps {
		before.calls(t, db, op, 0, label)
	}
	before.noWrites(t, db, label)
	if len(db.logs) != 0 {
		t.Fatalf("%s：非法入参却留下了流水：%s", label, callLogDump(db))
	}
}

// chainSnapshot 一次调用前的两组计数：判定链上的算子、以及全部写侧算子。
// 必须分成两份——合并成一份会让 wantNoWrites 把「正常的读」也判成不该发生。
type chainSnapshot struct {
	probe  map[string]int
	writes map[string]int
}

func captureChain(db *store) chainSnapshot {
	return chainSnapshot{probe: probeCounts(db), writes: snapshotWrites(db)}
}

// calls 断言某个判定链算子的调用增量。op 必须是 authorizeProbeOps 里的名字。
func (c chainSnapshot) calls(t *testing.T, db *store, op string, want int, label string) {
	t.Helper()
	if _, ok := c.probe[op]; !ok {
		t.Fatalf("%s：%q 不在 authorizeProbeOps 里，fake 不会计这个数", label, op)
	}
	wantCalls(t, db, op, c.probe[op], want, label)
}

// noWrites 断言整条写路径一次都没发生。
func (c chainSnapshot) noWrites(t *testing.T, db *store, label string) {
	t.Helper()
	wantNoWrites(t, db, c.writes, label)
}

// probeCounts 取判定链上全部算子的当前调用数。
func probeCounts(db *store) map[string]int {
	out := make(map[string]int, len(authorizeProbeOps))
	for _, op := range authorizeProbeOps {
		out[op] = db.count(op)
	}
	return out
}

// TestAuthorizeRequest_ParameterGatesAreSideEffectFree 钉住「参数形状先于任何读写」。
// 每条哨兵都是显式写出来的常量：把「必填缺失」和「长度超限」混成一个错误、
// 或者把商业化红线放到配额扣减之后，都会当场变红。
func TestAuthorizeRequest_ParameterGatesAreSideEffectFree(t *testing.T) {
	db, s, _, access, _, _ := authorizeFixture(t)
	longRequestID := strings.Repeat("r", maxRequestIDRunes+1)
	longAPICode := strings.Repeat("a", maxAPICodeRunes+1)
	longPath := "/" + strings.Repeat("p", maxPathRunes) // 1+255 rune → 超限一个
	longScope := strings.Repeat("s", maxScopeTokenRunes+1)

	cases := []struct {
		name   string
		mutate func(in *rpc.AuthorizeRequestReq)
		want   error
	}{
		{"request_id 缺失", func(in *rpc.AuthorizeRequestReq) { in.RequestId = "" },
			model.ErrRequestIDRequired},
		{"request_id 全空白", func(in *rpc.AuthorizeRequestReq) { in.RequestId = "   " },
			model.ErrRequestIDRequired},
		{"request_id 含内部空格", func(in *rpc.AuthorizeRequestReq) { in.RequestId = "auth z" },
			model.ErrRequestIDRequired},
		{"request_id 含制表符", func(in *rpc.AuthorizeRequestReq) { in.RequestId = "auth\tz" },
			model.ErrRequestIDRequired},
		{"request_id 超长", func(in *rpc.AuthorizeRequestReq) { in.RequestId = longRequestID },
			model.ErrRequestIDRequired},
		{"api_code 缺失", func(in *rpc.AuthorizeRequestReq) { in.ApiCode = "" }, errAPICodeRequired},
		{"api_code 全空白", func(in *rpc.AuthorizeRequestReq) { in.ApiCode = " \t " },
			errAPICodeRequired},
		{"api_code 超长", func(in *rpc.AuthorizeRequestReq) { in.ApiCode = longAPICode },
			errAPICodeRequired},
		{"method 缺失", func(in *rpc.AuthorizeRequestReq) { in.Method = "" }, errMethodPathRequired},
		{"method 全空白", func(in *rpc.AuthorizeRequestReq) { in.Method = "   " },
			errMethodPathRequired},
		{"path 缺失", func(in *rpc.AuthorizeRequestReq) { in.Path = "" }, errMethodPathRequired},
		{"path 全空白", func(in *rpc.AuthorizeRequestReq) { in.Path = "   " }, errMethodPathRequired},
		{"path 超长", func(in *rpc.AuthorizeRequestReq) { in.Path = longPath }, errPathTooLong},
		{"required_scope 含空白", func(in *rpc.AuthorizeRequestReq) { in.RequiredScope = "video read" },
			model.ErrScopeUnknown},
		{"required_scope 含非法分隔符", func(in *rpc.AuthorizeRequestReq) {
			in.RequiredScope = "video/read"
		}, model.ErrScopeUnknown},
		{"required_scope 超长", func(in *rpc.AuthorizeRequestReq) { in.RequiredScope = longScope },
			model.ErrScopeUnknown},
		// 商业化红线必须在配额扣减与流水之前拦住：否则「被拒的调用」也会积累成可用配额消耗。
		{"api_code 命中未开放类目", func(in *rpc.AuthorizeRequestReq) { in.ApiCode = "member.info" },
			model.ErrForbiddenScopeCategory},
		{"required_scope 命中未开放类目", func(in *rpc.AuthorizeRequestReq) {
			in.RequiredScope = "pay.write"
		}, model.ErrForbiddenScopeCategory},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := authorizeReq("authz-param-"+strings.ReplaceAll(c.name, " ", "-"), access, testScopeR)
			c.mutate(in)
			before := captureChain(db)
			got, err := callAuthorize(t, s, in)
			wantFail(t, got, err, c.want, c.name)
			wantZeroChainContact(t, db, before, c.name)
		})
	}

	// 反证 1：同一 fixture、同一 request_id 形状的正常请求必须放行。
	// 没有它，上面的表可能只是在测「fixture 本来就是坏的」。
	t.Run("同一 fixture 的正常请求必须放行", func(t *testing.T) {
		db2, s2, _, access2, _, _ := authorizeFixture(t)
		before := captureChain(db2)
		in := authorizeReq("authz-param-control", access2, testScopeR)
		got, err := callAuthorize(t, s2, in)
		got = wantOK(t, got, err, "合法入参")
		if !got.Allowed {
			t.Fatalf("正例被拒，说明上面的表测的不是参数门禁：%+v", got)
		}
		// 正例必须真的走到写阶段——否则「反例零副作用」只是「整条链路都没跑起来」。
		before.calls(t, db2, "CallLogs.Insert", 1, "合法入参写了一条流水")
		before.calls(t, db2, "QuotaUsages.Add", 1, "合法入参扣了一次配额")
		if len(db2.logs) != 1 {
			t.Fatalf("正例流水行数=%d，期望 1", len(db2.logs))
		}
	})

	// 反证 2：路径按 rune 计长，不按字节。
	// op_api_call_log.path 是 VARCHAR(255)（字符数），用 len(bytes) 判会把中文路径全部误拒。
	t.Run("path 按 rune 计长而不是按字节", func(t *testing.T) {
		db3, s3, _, access3, _, _ := authorizeFixture(t)
		// 1 + 254 个汉字 = 255 rune（刚好在界内），但字节数是 763。
		path := "/" + strings.Repeat("中", maxPathRunes-1)
		if utf8Len(path) != maxPathRunes {
			t.Fatalf("用例自身失效：rune 数=%d", utf8Len(path))
		}
		if len(path) <= maxPathRunes {
			t.Fatalf("用例自身失效：字节数=%d，无法区分 rune 与字节口径", len(path))
		}
		in := authorizeReq("authz-param-rune", access3, testScopeR)
		in.Path = path
		got, err := callAuthorize(t, s3, in)
		got = wantOK(t, got, err, "255 个汉字的合法路径")
		if !got.Allowed {
			t.Fatalf("rune 计长的路径被拒：%+v", got)
		}
		if row := mustCallLog(t, db3, "authz-param-rune"); row.Path != path {
			t.Fatalf("流水里的路径被截断/改写：rune=%d 字节=%d", utf8Len(row.Path), len(row.Path))
		}
		// 同一 shape 多一个汉字（256 rune）必须被 errPathTooLong 拒掉：
		// 少了这一半，「完全不判长度」也能让上面那半变绿。
		in2 := authorizeReq("authz-param-rune-over", access3, testScopeR)
		in2.Path = path + "中"
		got2, err2 := callAuthorize(t, s3, in2)
		wantFail(t, got2, err2, errPathTooLong, "256 rune 路径")
	})
}

// TestAuthorizeRequest_CredentialModesAreMutuallyExclusive 钉住 authorizerequestlogic.go:77-86
// 「凭证模式二选一，不做猜一种」。
//
// 三条独立契约分别由三组用例把守：
//   - 都不给 / 都给 → ErrInvalidGrantType（判定身份的依据不完整或可能自相矛盾）；
//   - 只给签名四元组的任一项 → ErrSignatureModeUnavailable，且发生在幂等查询之前，
//     因此不消耗 request_id 命名空间、不消耗 nonce、不扣配额、不写流水；
//   - access_token 走 TrimSpace 而签名四元组不走：两侧各自的边界都要有用例，
//     否则「忘了去空白」与「多去了空白」两种退化都测不出来。
func TestAuthorizeRequest_CredentialModesAreMutuallyExclusive(t *testing.T) {
	db, s, _, access, _, _ := authorizeFixture(t)
	cases := []struct {
		name   string
		mutate func(in *rpc.AuthorizeRequestReq)
		want   error
	}{
		{"两种凭证都不给", func(in *rpc.AuthorizeRequestReq) { in.AccessToken = "" },
			model.ErrInvalidGrantType},
		{"access_token 只有空白等同于不给", func(in *rpc.AuthorizeRequestReq) {
			in.AccessToken = "   "
		}, model.ErrInvalidGrantType},
		{"只给 app_key", func(in *rpc.AuthorizeRequestReq) {
			in.AccessToken, in.AppKey = "", "appk_1"
		}, model.ErrSignatureModeUnavailable},
		{"app_key 只有空白也算声明了签名模式", func(in *rpc.AuthorizeRequestReq) {
			in.AccessToken, in.AppKey = "", " "
		}, model.ErrSignatureModeUnavailable},
		{"只给 signature", func(in *rpc.AuthorizeRequestReq) {
			in.AccessToken, in.Signature = "", strings.Repeat("cd", 32)
		}, model.ErrSignatureModeUnavailable},
		{"只给 timestamp", func(in *rpc.AuthorizeRequestReq) {
			in.AccessToken, in.Timestamp = "", nowUnix()
		}, model.ErrSignatureModeUnavailable},
		{"只给 nonce", func(in *rpc.AuthorizeRequestReq) {
			in.AccessToken, in.Nonce = "", "nonce-1"
		}, model.ErrSignatureModeUnavailable},
		{"access_token 与 app_key 都给", func(in *rpc.AuthorizeRequestReq) {
			in.AppKey = "appk_1"
		}, model.ErrInvalidGrantType},
		{"access_token 与 nonce 都给", func(in *rpc.AuthorizeRequestReq) {
			in.Nonce = "nonce-1"
		}, model.ErrInvalidGrantType},
		{"access_token 与 timestamp 都给", func(in *rpc.AuthorizeRequestReq) {
			in.Timestamp = nowUnix()
		}, model.ErrInvalidGrantType},
		{"access_token 与空白 app_key 都给", func(in *rpc.AuthorizeRequestReq) {
			in.AppKey = " "
		}, model.ErrInvalidGrantType},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := authorizeReq("authz-mode-"+strings.ReplaceAll(c.name, " ", "-"), access, testScopeR)
			c.mutate(in)
			before := captureChain(db)
			got, err := callAuthorize(t, s, in)
			wantFail(t, got, err, c.want, c.name)
			// 幂等查询排在模式门禁之后：这里连 FindByRequestID 都不该发生。
			wantZeroChainContact(t, db, before, c.name)
		})
	}

	// 反证：门禁不能退化成「任何请求都判成非法组合」——同一 fixture 只给 access_token
	// 必须正常放行，且凭证模式门禁不得提前把这条正例拒掉。
	t.Run("只给 access_token 时门禁不得拦截", func(t *testing.T) {
		db2, s2, _, access2, _, _ := authorizeFixture(t)
		got, err := authorizeOK(t, s2, "authz-mode-control", access2, testScopeR)
		got = wantOK(t, got, err, "只给 access_token")
		if !got.Allowed || got.CallLogId == 0 {
			t.Fatalf("正例没走到写阶段：%+v", got)
		}
		if len(db2.logs) != 1 {
			t.Fatalf("正例流水行数=%d", len(db2.logs))
		}
	})
}

// ---------------------------------------------------------------- 写令牌桶与写的边界

// TestAuthorizeRequest_WriteRateLimitHappensBeforeAnyWrite 钉住 authorizerequestlogic.go:123
// 与 :144 两处 writePermit 的位置：拿不到写令牌时必须在「扣配额 / 写流水」之前返回。
//
// 为什么值得单独钉：令牌桶保护的是 MySQL。若把 writePermit 挪到 chargeQuota 之后，
// 被限流的请求仍会把 op_quota_usage 抬高——429 变成「白打」，客户端重试即可自我耗尽配额。
// 三条子用例分别覆盖三个出口，缺任何一条都会让「限流点位置」的某一半退化成无人看守：
//   - 放行路径限流：既不扣也不写；
//   - 可定位凭证的拒绝路径限流：连拒绝流水都写不下（这条最容易漏，它在另一个分支里）；
//   - 定位不到凭证的拒绝路径：压根不申请写令牌，因此限流时仍能给出结论（无写可做）。
func TestAuthorizeRequest_WriteRateLimitHappensBeforeAnyWrite(t *testing.T) {
	t.Run("放行路径被限流时既不扣配额也不写流水", func(t *testing.T) {
		db, s, _, access, _, _ := authorizeFixture(t)
		drainWriteBucket(s)
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-rl-allow", access, testScopeR)
		wantFail(t, got, err, model.ErrRateLimited, "放行路径被限流")
		before.noWrites(t, db, "放行路径被限流")
		if n := usageTotalFor(db, testAppID, testAPIC, quotaWin); n != 0 {
			t.Fatalf("被限流的请求仍抬高了配额投影 used=%d", n)
		}
		if len(db.logs) != 0 {
			t.Fatalf("被限流的请求仍写了流水：%s", callLogDump(db))
		}
		// 限流发生在判定之后（凭证/scope 已读完），所以读侧确实跑过一趟：
		// 不钉这一条，「一进门就拒」的实现也能让上面全部变绿。
		before.calls(t, db, "Tokens.FindByAccessHash", 1, "限流前已定位凭证")
		before.calls(t, db, "Apps.FindByID", 1, "限流前已读应用状态")
		// 但配额连读都没读——限流挡在 chargeQuota 之前。
		before.calls(t, db, "QuotaPolicies.ListCandidates", 0, "限流不得触碰配额规则")
	})

	t.Run("可定位凭证的拒绝路径同样受写令牌约束", func(t *testing.T) {
		db, s, _, access, _, tok := authorizeFixture(t)
		if tok.State != model.TokenStateActive {
			t.Fatalf("用例前提失效：fixture 的 token 初始状态=%d，应为 ACTIVE", tok.State)
		}
		for _, row := range db.tokens {
			row.State = model.TokenStateRevoked
		}
		drainWriteBucket(s)
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-rl-deny", access, testScopeR)
		wantFail(t, got, err, model.ErrRateLimited, "拒绝路径被限流")
		before.noWrites(t, db, "拒绝路径被限流")
		if len(db.logs) != 0 {
			t.Fatalf("被限流的拒绝仍写了流水：%s", callLogDump(db))
		}
	})

	t.Run("定位不到凭证的拒绝不申请写令牌", func(t *testing.T) {
		db, s, _, _, _, _ := authorizeFixture(t)
		drainWriteBucket(s)
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-rl-unknown", "never-issued-access-token", testScopeR)
		got = wantOK(t, got, err, "无归属的拒绝不得被限流挡住")
		if got.Allowed || got.DenyReason != denyInactive || got.CallLogId != 0 {
			t.Fatalf("定位不到应用时应只回结论：%+v", got)
		}
		// 无 app_id 可写 → 该分支不申请写令牌，因此限流中也能给出结论；结论里不得有任何标识符。
		if got.AppId != 0 || got.Mid != 0 || len(got.Scope) != 0 {
			t.Fatalf("未知凭证回出了标识符：app=%d mid=%d scope=%v", got.AppId, got.Mid, got.Scope)
		}
		before.noWrites(t, db, "无归属的拒绝")
	})
}

// ---------------------------------------------------------------- consent / required_scope 语义

// consentlessFixture 与 authorizeFixture 同构，唯一差别是用户没勾过同意（consent_given=0）。
func consentlessFixture(t *testing.T) (*store, *svc.ServiceContext, *model.Grant, string) {
	t.Helper()
	db, s, grant, access, _, _ := oauthTokenFixture(t, oauthTokenScopes, 0)
	seedQuota(db, model.GlobalAppID, model.AnyAPICode, quotaWin, authorizeGlobalLimit)
	if grant.ConsentGiven != 0 {
		t.Fatalf("用例前提失效：consent_given=%d，应为 0", grant.ConsentGiven)
	}
	return db, s, grant, access
}

// TestAuthorizeRequest_ConsentGateOnlyBlocksWriteScopes 钉住 tokencheck.go:148-188 这一段
// 「scope 门禁到底门住什么」。
//
// 成对的正/反用例是关键：只给反例（未同意 → 拒），实现把「写 scope 判定」整段删掉也能变绿；
// 只给正例（读 scope → 放行）则把门禁反过来（读也拦）同样能变绿。
// 因此每一条拒绝都配一条「只差一个比特/一行目录」的放行，反之亦然。
func TestAuthorizeRequest_ConsentGateOnlyBlocksWriteScopes(t *testing.T) {
	t.Run("用户未同意时读权限点仍放行", func(t *testing.T) {
		db, s, _, access := consentlessFixture(t)
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-consent-read", access, testScopeR)
		got = wantOK(t, got, err, "未同意下的读权限点")
		if !got.Allowed {
			t.Fatalf("consent_given 拦到了读权限点上（只该拦写）：%+v", got)
		}
		// 放行必须真的扣了配额，否则「门禁放错位置」也能让本例变绿。
		before.calls(t, db, "QuotaUsages.Add", 1, "读权限点放行即扣一次")
		before.calls(t, db, "CallLogs.Insert", 1, "读权限点放行落一行流水")
	})

	t.Run("用户未同意时写权限点必须拒且不扣配额", func(t *testing.T) {
		db, s, _, access := consentlessFixture(t)
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-consent-write", access, testScopeW)
		got = wantOK(t, got, err, "未同意下的写权限点")
		if got.Allowed || got.DenyReason != denyConsentMissing {
			t.Fatalf("写权限点的同意门禁没生效：%+v", got)
		}
		if !authorizeKnownDeny[got.DenyReason] {
			t.Fatalf("原因码不在固定集合里：%q", got.DenyReason)
		}
		before.calls(t, db, "QuotaUsages.Add", 0, "同意门禁排在配额扣减之前")
		before.calls(t, db, "CallLogs.Insert", 1, "可定位凭证的拒绝仍要落流水")
		row := mustCallLog(t, db, "authz-consent-write")
		if row.Allowed != model.CallDenied || row.DenyReason != denyConsentMissing {
			t.Fatalf("落库的结论与响应不一致：%+v", *row)
		}
		wantNoSecretEcho(t, got, "同意门禁拒绝", access)
	})

	// 上面那条的反证：唯一变化是同意位点，结论必须翻成放行。
	// 少了这一条，「写权限点永远被拒」（例如 fixture 里 scope 声明写错）也能让上面全绿。
	t.Run("补齐同意位点后同一写权限点放行", func(t *testing.T) {
		_, s, grant, access := consentlessFixture(t)
		grant.ConsentGiven = 1
		got, err := authorizeOK(t, s, "authz-consent-granted", access, testScopeW)
		got = wantOK(t, got, err, "已同意的写权限点")
		if !got.Allowed {
			t.Fatalf("consent_given=1 后仍被拒：%+v", got)
		}
	})

	// 门禁读的是目录里的 access 声明，不是 scope 名字：把 video.publish 改成读，
	// 未同意也必须放行。这一条同时排除「按名字前缀猜读写」的实现退化。
	t.Run("门禁只跟目录的 access 声明绑定", func(t *testing.T) {
		db, s, _, access := consentlessFixture(t)
		def, ok := db.scopes[testScopeW]
		if !ok || !def.IsWrite() {
			t.Fatalf("用例前提失效：目录里的 %s 不是写权限点", testScopeW)
		}
		def.Access = model.ScopeAccessRead
		got, err := authorizeOK(t, s, "authz-consent-demote", access, testScopeW)
		got = wantOK(t, got, err, "目录降级为读之后")
		if !got.Allowed {
			t.Fatalf("目录声明为读却仍要求用户同意：%+v", got)
		}
	})

	// 特征化（缺陷 2）：tokencheck.go:174 的注释写「目录缺项按未知处理（不放宽）」，
	// 但 :181 的实现是 `if def == nil || !def.IsWrite() { return nil }`——目录整行不见了
	// 恰好等于「不是写权限点」，于是同意门禁整段跳过。
	// 这里断言的是**当前行为**，不是应然行为：修好那行（def == nil → 拒绝）后本例必须变红，
	// 迫使改动者显式确认新的原因码。跨入口比对保证两入口一起漂移不了。
	t.Run("目录缺项时同意门禁当前整段跳过（缺陷 2 特征化）", func(t *testing.T) {
		db, s, _, access := consentlessFixture(t)
		if _, ok := db.scopes[testScopeW]; !ok {
			t.Fatalf("用例前提失效：目录里没有 %s", testScopeW)
		}
		delete(db.scopes, testScopeW)
		got, err := authorizeOK(t, s, "authz-catalog-gap", access, testScopeW)
		got = wantOK(t, got, err, "目录缺项")
		if !got.Allowed {
			t.Fatalf("目录缺项已被改为不放宽（缺陷 2 已修复）：请同步更新本用例为断言 %q",
				denyConsentMissing)
		}
		// 同一库状态走 IntrospectToken 必须给同样结论：两入口共用判定链，
		// 若这里出现分歧，说明有人给其中一个入口另写了一套 scope 判定。
		insp, ierr := introspect(s, access, 0, testScopeW)
		insp = wantOK(t, insp, ierr, "IntrospectToken 同状态")
		if insp.Active != got.Allowed {
			t.Fatalf("两入口对同一库状态结论分歧：authorize=%v introspect=%v", got.Allowed, insp.Active)
		}
	})

	// required_scope 为空 = 网关声明「这次调用不需要权限点」，整段 scope 判定跳过
	// （tokencheck.go:149）。这是契约（authorizerequestlogic.go:98-100 说明服务端无法由
	// api_code 推导 scope），因此必须钉住「连读都没读目录/审批集」，否则实现改成
	// 「空 scope 时按 api_code 猜一个」也会悄悄通过其余全部用例。
	t.Run("required_scope 为空时整段 scope 判定跳过", func(t *testing.T) {
		db, s, _, access := consentlessFixture(t)
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-scope-empty", access, "   ")
		got = wantOK(t, got, err, "required_scope 全空白")
		if !got.Allowed {
			t.Fatalf("空 required_scope 被拒（应视为不要求权限点）：%+v", got)
		}
		before.calls(t, db, "Scopes.FindByScope", 0, "空 scope 不得读目录")
		before.calls(t, db, "AppScopes.FindGrantedScopes", 0, "空 scope 不得读审批集")
		// 但凭证判定照旧，响应回的是 token 快照全集，不是空列表。
		before.calls(t, db, "Tokens.FindByAccessHash", 1, "空 scope 仍要定位凭证")
		wantScopeList(t, got.Scope, oauthSortedScopes, "空 required_scope 的 scope 回显")
		// 与 IntrospectToken 同结论：两入口对「不要求权限点」的口径必须一致。
		insp, ierr := introspect(s, access, 0, "")
		insp = wantOK(t, insp, ierr, "IntrospectToken 空 scope")
		if insp.Active != got.Allowed {
			t.Fatalf("空 scope 时两入口分歧：authorize=%v introspect=%v", got.Allowed, insp.Active)
		}
	})
}

// ---------------------------------------------------------------- grant 归属与并发抢跑

// TestAuthorizeRequest_RevokeCheckReadsTheTokensOwnGrant 钉住 tokencheck.go:122
// 「撤销位点按 token 行自己绑定的 grant_id 读」。
//
// 这是撤销能否立即生效的关键分叉：若改成按 (app_id, mid) 反查，同一用户在同一应用下的
// **另一条**活动授权就会救回已撤销的凭证（撤销只对那条 grant 生效，攻击者换一条即可绕过）。
// 构造方式因此必须让两种实现给出不同结论：库里额外放一条「同应用、同 token.mid、未撤销」的
// 诱饵 grant，并把 token 行的 mid 改成与它一致（数据不一致态），此时：
//   - 按 grant_id 读（正确）→ 读到已撤销的那条 → revoked；
//   - 按 app+mid 读（退化）→ 读到诱饵那条活动授权 → 放行。
func TestAuthorizeRequest_RevokeCheckReadsTheTokensOwnGrant(t *testing.T) {
	// decoy：把 token 的 mid 指到另一条活动授权上，同时撤销它真正绑定的 grant。
	decoy := func(t *testing.T) (*store, *svc.ServiceContext, *model.Grant, *model.Grant, string) {
		t.Helper()
		db, s, grant, access, _, tok := authorizeFixture(t)
		other := seedGrant(db, testAppID, testMid+1, oauthTokenScopes, 1)
		if other.GrantID == grant.GrantID {
			t.Fatalf("用例前提失效：诱饵 grant 与 token 的 grant 撞号")
		}
		if other.RevokedAt != 0 || other.Status != model.GrantStatusActive {
			t.Fatalf("用例前提失效：诱饵 grant 不是活动状态")
		}
		tok.Mid = other.Mid
		grant.RevokedAt = tok.Ctime + 1
		grant.Status = model.GrantStatusRevoked
		return db, s, grant, other, access
	}

	t.Run("撤销位点按 token 自己的 grant 比对", func(t *testing.T) {
		db, s, grant, other, access := decoy(t)
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-grant-own", access, testScopeR)
		got = wantOK(t, got, err, "诱饵活动授权在场")
		if got.Allowed || got.DenyReason != denyRevoked {
			t.Fatalf("撤销被同用户的另一条活动授权绕过：%+v", got)
		}
		before.calls(t, db, "Grants.FindByID", 1, "撤销比对只读 grant_id")
		before.calls(t, db, "Grants.FindByAppMid", 0, "判定链不得按 (app,mid) 反查 grant")
		before.calls(t, db, "QuotaUsages.Add", 0, "撤销不扣配额")
		// 流水归因取的是 token 行上的值（persist 直接用 res.token.Mid/GrantID）：
		// 记录必须是「这条凭证绑定的那条 grant」，否则审计会把撤销追到无关授权上。
		row := mustCallLog(t, db, "authz-grant-own")
		if row.GrantID != grant.GrantID {
			t.Fatalf("流水记的 grant=%d，应为 token 绑定的 %d（诱饵是 %d）",
				row.GrantID, grant.GrantID, other.GrantID)
		}
		if row.Mid != other.Mid {
			t.Fatalf("流水的 mid=%d，与 token 行上的 %d 不一致（不得二次推导）", row.Mid, other.Mid)
		}
	})

	// 反证：同一构造只要不写撤销位点就必须放行——证明上面的拒绝来自位点，而不是
	// 「mid 与 grant.mid 不一致」本身被另加了一道门禁（那会让上面那条正例变成假正例）。
	t.Run("未写撤销位点时同一构造放行", func(t *testing.T) {
		_, s, grant, _, access := decoy(t)
		grant.RevokedAt, grant.Status = 0, model.GrantStatusActive
		got, err := authorizeOK(t, s, "authz-grant-own-ok", access, testScopeR)
		got = wantOK(t, got, err, "未撤销的同构造")
		if !got.Allowed {
			t.Fatalf("mid 与 grant.mid 不一致本身就把它拒了，撤销用例失去区分力：%+v", got)
		}
	})

	// 数据不一致态的可观测性（登记进报告的观察项，非安全洞）：判定按 grant_id、
	// 归因按 token 行，两者可以指向不同用户。这里钉住「不会误升权」——结论仍是 revoked，
	// 且拒绝流水同时带上两个值，风控看得见。
	t.Run("不一致态下结论仍 fail-closed", func(t *testing.T) {
		db, s, _, other, access := decoy(t)
		got, err := authorizeOK(t, s, "authz-grant-skew", access, testScopeR)
		got = wantOK(t, got, err, "mid 与 grant 不一致且已撤销")
		row := mustCallLog(t, db, "authz-grant-skew")
		if got.Allowed || row.Allowed != model.CallDenied {
			t.Fatalf("不一致态被放行：reply=%+v row=%+v", got.String(), *row)
		}
		if row.GrantID == other.GrantID || row.Mid != other.Mid {
			t.Fatalf("拒绝流水未能同时暴露两个归属：mid=%d grant=%d", row.Mid, row.GrantID)
		}
	})
}

// TestAuthorizeRequest_ConcurrentSameRequestIDYieldsToFirstWriter 钉住 persist 的
// 「uniq_request_id 撞车即放弃本次结论」（authorizerequestlogic.go:288-296）。
//
// 交错用 db.onHit 表达：钩子在 CallLogs.Insert 真正执行的那一瞬间插入对手的流水，
// 纯前置构造做不到（logic 读的就是同一个快照），改 logic 返回的副本更是不影响库。
// 关键断言是「两个结论只留一个，且留的是先落库那个」——本线程已扣的配额投影不回滚
// （投影允许短时放宽），但响应绝不能是两份结论的混合体。
func TestAuthorizeRequest_ConcurrentSameRequestIDYieldsToFirstWriter(t *testing.T) {
	t.Run("放行路径撞车时重放对手的拒绝结论", func(t *testing.T) {
		db, s, _, access, _, tok := authorizeFixture(t)
		const racerID = int64(424242)
		db.onHit("CallLogs.Insert", func() {
			db.logs["authz-race-allow"] = &model.ApiCallLog{
				CallLogID: racerID, RequestID: "authz-race-allow", AppID: testAppID,
				APICode: testAPIC, Mid: testMid, TokenID: tok.TokenID, GrantID: tok.GrantID,
				Allowed: model.CallDenied, DenyReason: denyQuotaExceeded, QuotaLimit: 2,
				Ctime: nowTS(),
			}
		})
		before := captureChain(db)
		got, err := authorizeOK(t, s, "authz-race-allow", access, testScopeR)
		got = wantOK(t, got, err, "并发撞车")
		// 本线程的判定（allowed=true + 剩余额度）必须整个丢掉，换成对手的拒绝。
		if got.Allowed || got.DenyReason != denyQuotaExceeded || got.CallLogId != racerID {
			t.Fatalf("没有重放先落库者的结论：%+v", got)
		}
		if got.QuotaLimit != 2 || got.QuotaRemaining != 0 {
			t.Fatalf("重放把两份结论混在一起了：limit=%d remaining=%d", got.QuotaLimit,
				got.QuotaRemaining)
		}
		if len(db.logs) != 1 {
			t.Fatalf("撞车后流水行数=%d，期望 1：%s", len(db.logs), callLogDump(db))
		}
		// 已扣的配额投影不回滚（quota.go 文件头第 3 条：藏起被拒用量等于让运营看不见冲击）。
		before.calls(t, db, "QuotaUsages.Add", 1, "撞车那次已发生的记账保留")
		before.calls(t, db, "CallLogs.Insert", 1, "撞车不产生第二行流水")
	})

	t.Run("拒绝路径撞车时重放对手的放行结论", func(t *testing.T) {
		db, s, _, access, _, _ := authorizeFixture(t)
		for _, row := range db.tokens {
			row.State = model.TokenStateRevoked
		}
		db.onHit("CallLogs.Insert", func() {
			db.logs["authz-race-deny"] = &model.ApiCallLog{
				CallLogID: 777, RequestID: "authz-race-deny", AppID: testAppID, APICode: testAPIC,
				Mid: testMid, Allowed: model.CallAllowed, DenyReason: denyNone, QuotaLimit: 9,
				QuotaRemaining: 8, Ctime: nowTS(),
			}
		})
		got, err := authorizeOK(t, s, "authz-race-deny", access, testScopeR)
		got = wantOK(t, got, err, "拒绝路径撞车")
		if !got.Allowed || got.DenyReason != denyNone || got.CallLogId != 777 {
			t.Fatalf("对手已放行，本次却仍回自己的拒绝：%+v", got)
		}
		if len(db.logs) != 1 {
			t.Fatalf("撞车后流水行数=%d", len(db.logs))
		}
	})
}

// recordChain 给判定链上每个算子挂一次性钩子，按「第一次被调用」的先后收集名字。
// 钩子走 db.onHit，执行时机就是 logic 真正调到那一行的时机，因此序列本身就是证据。
func recordChain(db *store) *[]string {
	seen := &[]string{}
	for _, op := range authorizeProbeOps {
		db.onHit(op, func() { *seen = append(*seen, op) })
	}
	return seen
}

// TestAuthorizeRequest_GateOrderIsTheContract 钉住文件头声明的门禁顺序
// 「参数 → 凭证模式 → 幂等回放 → 凭证/scope → 配额 → 流水」。
//
// 顺序本身是契约而不是实现细节，两处错位会造成实际危害：
//   - 幂等回放晚于任何写 → 重试者先扣了配额才看到既有流水，同一个 request_id 扣两次；
//   - 配额扣减晚于流水写入 → 落库的 quota_remaining 与实际记账脱节，审计看不到超用。
//
// 断言用「整条序列相等」而不是「A 在 B 之前」：链路上任何新增的第一个算子（例如有人
// 把目录读取提前到参数校验之前）都会改变序列并当场变红。
func TestAuthorizeRequest_GateOrderIsTheContract(t *testing.T) {
	t.Run("放行路径的首次调用序列", func(t *testing.T) {
		db, s, _, access, _, _ := authorizeFixture(t)
		seen := recordChain(db)
		got, err := authorizeOK(t, s, "authz-order-allow", access, testScopeW)
		got = wantOK(t, got, err, "放行路径")
		if !got.Allowed {
			t.Fatalf("正例没走到写阶段：%+v", got)
		}
		want := []string{
			"CallLogs.FindByRequestID",     // 1. 幂等回放先于一切写
			"Tokens.FindByAccessHash",      // 2. 定位凭证
			"Grants.FindByID",              // 3. 撤销位点
			"Apps.FindByID",                // 4. 应用状态
			"AppScopes.FindGrantedScopes",  // 5. 应用当前获批集
			"Scopes.FindByScope",           // 6. 目录读写声明 → 同意门禁
			"QuotaPolicies.ListCandidates", // 7. 生效配额层级
			"QuotaUsages.Add",              // 8. 记账
			"CallLogs.Insert",              // 9. 流水最后落
		}
		if len(*seen) != len(want) {
			t.Fatalf("门禁序列长度=%d %v，期望 %d %v", len(*seen), *seen, len(want), want)
		}
		for i := range want {
			if (*seen)[i] != want[i] {
				t.Fatalf("门禁序列第 %d 项=%s，期望 %s（全序列 %v）", i, (*seen)[i], want[i], *seen)
			}
		}
	})

	// 撤销的凭证必须在定位之后立刻短路：后续的 grant/应用/目录/审批集读一次都不做。
	// 这条同时钉住「拒绝仍要落流水」的写点位置（读链末尾、配额之前）。
	t.Run("撤销凭证的读链在定位处即结束", func(t *testing.T) {
		db, s, _, access, _, tok := authorizeFixture(t)
		if tok.State != model.TokenStateActive {
			t.Fatalf("用例前提失效：初始状态=%d", tok.State)
		}
		for _, row := range db.tokens {
			row.State = model.TokenStateRevoked
		}
		seen := recordChain(db)
		got, err := authorizeOK(t, s, "authz-order-revoked", access, testScopeW)
		got = wantOK(t, got, err, "撤销凭证")
		if got.Allowed || got.DenyReason != denyRevoked {
			t.Fatalf("撤销未短路：%+v", got)
		}
		want := []string{"CallLogs.FindByRequestID", "Tokens.FindByAccessHash", "CallLogs.Insert"}
		if len(*seen) != len(want) {
			t.Fatalf("撤销路径的算子序列=%v，期望 %v", *seen, want)
		}
		for i := range want {
			if (*seen)[i] != want[i] {
				t.Fatalf("撤销路径序列第 %d 项=%s，期望 %s", i, (*seen)[i], want[i])
			}
		}
	})
}
