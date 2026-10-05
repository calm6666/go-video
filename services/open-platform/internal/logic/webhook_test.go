package logic

// webhook_test.go：回调端点三法的契约测试
// （RegisterWebhook / ListWebhooks / DeleteWebhook）。
//
// 契约依据：proto:524-564（三个请求/响应字段）、registerwebhooklogic.go 的方法头
// （两条安全属性：回调地址必须公网 https；未验证端点永不投递）、
// deletewebhooklogic.go 的方法头（删除是「配置终态化」而不是物理删行）。
//
// 本文件钉住的不变量：
//  1. RegisterWebhook 的响应字段逐条等于入库真值：endpoint_id/created/sign_key_version
//     都来自那一行，sign_key_version 尤其不得回显「配置当前版本」（历史端点按自己版本签名）；
//  2. 验证挑战明文只在首次登记的响应里出现一次：库里只有 HMAC，重放路径必须回空串
//     而不是补发一个新的「存不进这一行」的挑战；
//  3. 回调地址护栏与 redirect_uri 同源（helpers.validPublicURI）：非 https、带 userinfo、
//     带 fragment、localhost/.internal/.local 一类内部主机名、回环/私网/链路本地/组播/
//     CGNAT/元数据字面 IP 一律拒；公网字面 IP 必须放行（否则护栏会退化成「全拒」而自证通过）；
//  4. 新登记的端点 verified_at=0，绝不因 ctime 存在就被算作可投递；
//  5. 条数上限判定不得吃掉幂等回放：应用满额时同 (事件, 地址) 的重复注册仍要成功；
//  6. DeleteWebhook 与「抑制在途任务」在同一次 TransactCtx 里；已/失败与已死信的结论不动；
//     重复删除回成功、deleted=false，并且仍对「端点已停但任务未抑制」的窗口收敛；
//  7. 归属与身份先于任何读写，越权与不存在的 endpoint_id 同口径回 ErrWebhookNotFound，
//     失败路径一律零写副作用。

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

const (
	hookURL      = "https://hook.example.test/v1"
	hookURL2     = "https://hook2.example.test/v1"
	hookDesc     = "投稿结果回调"
	hookOperator = int64(60001)
)

// webhookFixture 两个 ACTIVE 应用：testAppID 是被测归属，testApp2 用于越权与串数据断言。
// MaxWebhookEndpointsPerApp 必须显式给值：testConf 未设置（=0）时 RegisterWebhook 的上限
// 分支与 ListWebhooks 的截断分支都会被跳过，那条护栏就永远测不到。
func webhookFixture(t *testing.T) (*store, *svc.ServiceContext) {
	t.Helper()
	db := newStore()
	seedApp(db, testAppID, testOwner)
	seedApp(db, testApp2, testOwner+1)
	s := newTestSvc(db)
	s.Config.OpenPlatform.MaxWebhookEndpointsPerApp = 20
	return db, s
}

func callRegisterWebhook(t *testing.T, s *svc.ServiceContext,
	in *rpc.RegisterWebhookReq) (*rpc.RegisterWebhookReply, error) {
	t.Helper()
	return NewRegisterWebhookLogic(t.Context(), s).RegisterWebhook(in)
}

func callListWebhooks(t *testing.T, s *svc.ServiceContext,
	in *rpc.ListWebhooksReq) (*rpc.ListWebhooksReply, error) {
	t.Helper()
	return NewListWebhooksLogic(t.Context(), s).ListWebhooks(in)
}

func callDeleteWebhook(t *testing.T, s *svc.ServiceContext,
	in *rpc.DeleteWebhookReq) (*rpc.DeleteWebhookReply, error) {
	t.Helper()
	return NewDeleteWebhookLogic(t.Context(), s).DeleteWebhook(in)
}

func registerWebhookReq(appID int64, eventType rpc.WebhookEventType, rawURL string) *rpc.RegisterWebhookReq {
	return &rpc.RegisterWebhookReq{
		AppId: appID, EventType: eventType, Url: rawURL, Description: hookDesc,
		OperatorMid: testOwner,
	}
}

func deleteWebhookReq(appID, endpointID, operator int64, isOperator bool,
	reason string) *rpc.DeleteWebhookReq {
	return &rpc.DeleteWebhookReq{
		AppId: appID, EndpointId: endpointID, OperatorMid: operator,
		IsOperator: isOperator, Reason: reason,
	}
}

// ---------------------------------------------------------------- 内存态小工具

// endpointRow 按主键取内存里的端点行（找不到返回 nil）：断言「响应 == 入库真值」用。
func endpointRow(db *store, endpointID int64) *model.WebhookEndpoint {
	for _, ep := range db.eps {
		if ep.EndpointID == endpointID {
			return ep
		}
	}
	return nil
}

// countEndpoints 该应用该地址的行数：>1 就说明「同一地址两种写法」插出了第二个端点，
// 或者幂等回放退化成了重复登记。
func countEndpoints(db *store, appID int64, rawURL string) int {
	var n int
	for _, ep := range db.eps {
		if ep.AppID == appID && ep.URL == rawURL {
			n++
		}
	}
	return n
}

// endpointState 把整张端点表折成可逐条比对的字符串，供「越权失败后一行都没动」用：
// wantNoWrites 只看调用次数，这里看的是「行内容有没有被顺手改过」。
func endpointState(db *store) map[int64]string {
	out := make(map[int64]string, len(db.eps))
	for _, ep := range db.eps {
		out[ep.EndpointID] = fmt.Sprintf("app=%d ev=%d url=%s kv=%d en=%d desc=%q verified=%d "+
			"chash=%q cexp=%d deleted=%d reason=%q ctime=%d mtime=%d",
			ep.AppID, ep.EventType, ep.URL, ep.SignKeyVersion, ep.Enabled, ep.Description,
			ep.VerifiedAt, ep.ChallengeHash, ep.ChallengeExpiresAt, ep.DeletedAt, ep.DeleteReason,
			ep.Ctime, ep.Mtime)
	}
	return out
}

func assertEndpointStateUnchanged(t *testing.T, db *store, before map[int64]string, label string) {
	t.Helper()
	after := endpointState(db)
	if len(after) != len(before) {
		t.Fatalf("%s：端点行数 %d → %d", label, len(before), len(after))
	}
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("%s：端点 %d 被改动\n改前 %s\n改后 %s", label, id, want, got)
		}
	}
}

// ---------------------------------------------------------------- 注册

func TestRegisterWebhook_CreatesUnverifiedEndpointAndIssuesChallengeOnce(t *testing.T) {
	db, s := webhookFixture(t)
	s.Config.Security.KeyVersion = 3 // 与 testConf 的 1 不同：证明回显的是配置读到的版本
	before := snapshotWrites(db)

	reply, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, "  "+hookURL+" "))
	reply = wantOK(t, reply, err, "登记回调端点")
	wantCalls(t, db, "WebhookEndpoints.Insert", before["WebhookEndpoints.Insert"], 1, "只写一行")

	row := endpointRow(db, reply.GetEndpointId())
	if row == nil {
		t.Fatalf("响应里的 endpoint_id=%d 在库里不存在（响应与入库不同源）", reply.GetEndpointId())
	}
	if !reply.GetCreated() {
		t.Fatal("首次登记必须 created=true")
	}
	if reply.GetSignKeyVersion() != s.Config.Security.KeyVersion || row.SignKeyVersion != 3 {
		t.Fatalf("sign_key_version reply=%d row=%d，应等于配置的 %d",
			reply.GetSignKeyVersion(), row.SignKeyVersion, s.Config.Security.KeyVersion)
	}
	// 明文挑战：challengeBytes=16 → 32 位 hex，且必须是入库 ChallengeHash 的原像。
	challenge := reply.GetVerificationChallenge()
	if len(challenge) != challengeBytes*2 || strings.ContainsAny(challenge, " \t") {
		t.Fatalf("验证挑战形态异常：%q（期望 %d 位 hex）", challenge, challengeBytes*2)
	}
	wantHash, err := model.HashCredential(testPepper, challenge)
	wantOK(t, wantHash, err, "HashCredential(challenge)")
	if row.ChallengeHash != wantHash {
		t.Fatal("库里的 challenge_hash 与响应挑战的哈希口径不一致，回执验证必然失败")
	}
	// 入参的空白必须被裁掉：带空白的地址既过不了 uniq 键也比不上下游地址。
	if row.URL != hookURL || row.AppID != testAppID || row.EventType != model.WebhookEventContentPublishResult {
		t.Fatalf("入库行与入参不符：%+v", row)
	}
	if row.Description != hookDesc {
		t.Fatalf("description=%q", row.Description)
	}
	// 「未验证永不投递」的锚点就是这一列：新行必须是 0，绝不能被 ctime 顶上。
	if row.VerifiedAt != 0 {
		t.Fatalf("新登记端点 verified_at=%d，必须为 0（未验证不投递）", row.VerifiedAt)
	}
	if row.Enabled != 1 || row.DeletedAt != 0 {
		t.Fatalf("新登记端点 enabled=%d deleted_at=%d，期望 1/0", row.Enabled, row.DeletedAt)
	}
	now := nowTS()
	if left := row.ChallengeExpiresAt - now; left < challengeTTLSeconds-5 || left > challengeTTLSeconds+5 {
		t.Fatalf("挑战有效期剩 %d 秒，期望约 %d", left, challengeTTLSeconds)
	}
	// 明文不得落在任何一行里（含 challenge_hash 列被明文覆盖这种写法）。
	mustNoPlaintextStored(t, db, "登记回调端点", challenge)
}

func TestRegisterWebhook_ReplayKeepsSingleRowAndNeverReturnsSecondChallenge(t *testing.T) {
	db, s := webhookFixture(t)
	first, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, hookURL))
	first = wantOK(t, first, err, "首次登记")

	// 同一地址的第二种写法：scheme 大小写与首尾空白。validPublicURI 返回归一化串，
	// 因此这两次必须是「同一行」，否则 uniq_app_event_url 形同虚设、同一事件双份外呼。
	second, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT,
		" HTTPS://hook.example.test/v1 "))
	second = wantOK(t, second, err, "重复登记（同地址另一种写法）")
	if second.GetCreated() {
		t.Fatal("归一化后同一地址被判成新端点，重复注册护栏失守")
	}
	if second.GetEndpointId() != first.GetEndpointId() {
		t.Fatalf("重复登记换了主键：%d → %d", first.GetEndpointId(), second.GetEndpointId())
	}
	// 库里只有旧挑战的 HMAC，无法反推明文：重放必须回空串，
	// 也不能「顺手补发一个新挑战」（那会把应用引向一个无效值）。
	if second.GetVerificationChallenge() != "" {
		t.Fatalf("重复登记又下发了挑战 %q，库中只存旧挑战摘要，这个值永远无法通过验证",
			second.GetVerificationChallenge())
	}
	if n := countEndpoints(db, testAppID, hookURL); n != 1 {
		t.Fatalf("同一地址端点数=%d，期望 1", n)
	}
	if n := len(db.eps); n != 1 {
		t.Fatalf("端点总行数=%d，期望 1", n)
	}

	// 密钥轮换后重放历史端点：sign_key_version 必须仍是库里那一行的版本，
	// 回显配置当前版本等于告诉应用「用新密钥验签」，而这一行是按旧版本签的。
	row := endpointRow(db, first.GetEndpointId())
	s.Config.Security.KeyVersion = 9
	third, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, hookURL))
	third = wantOK(t, third, err, "轮换配置后再登记同一地址")
	if third.GetSignKeyVersion() != row.SignKeyVersion || row.SignKeyVersion == 9 {
		t.Fatalf("重放回显了配置当前版本：%d（行内 %d）", third.GetSignKeyVersion(), row.SignKeyVersion)
	}

	// 同地址不同事件类型是另一个订阅关系，必须能各自成行（uniq 含 event_type）。
	other, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, hookURL))
	other = wantOK(t, other, err, "同地址换事件类型")
	if !other.GetCreated() || other.GetEndpointId() == first.GetEndpointId() {
		t.Fatalf("换事件类型应新成一行：%+v", other)
	}
	if len(db.eps) != 2 {
		t.Fatalf("端点数=%d，期望 2", len(db.eps))
	}
}

func TestRegisterWebhook_ConcurrentCommitBeforeInsertReplaysSingleRow(t *testing.T) {
	db, s := webhookFixture(t)
	// 交错：logic 已通过全部读侧门禁，INSERT 即将发生的瞬间另一个请求先把同一行提交了。
	// 纯前置构造做不到这一点——logic 读 CountByApp 时那行还不存在。
	db.onHit("WebhookEndpoints.Insert", func() {
		db.eps = append(db.eps, &model.WebhookEndpoint{
			EndpointID: db.next("endpoint"), AppID: testAppID,
			EventType: model.WebhookEventGrantRevoked, URL: hookURL2, SignKeyVersion: 1,
			Enabled: 1, Description: "抢先提交的对手方", ChallengeHash: "racer-hash",
		})
	})

	reply, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_GRANT_REVOKED, hookURL2))
	reply = wantOK(t, reply, err, "并发抢跑后的重复登记")
	if reply.GetCreated() {
		t.Fatal("唯一键已被对手方占用，仍报 created=true 等于让应用以为拿到了新端点")
	}
	if reply.GetVerificationChallenge() != "" {
		t.Fatalf("抢跑行不该再拿挑战：%q", reply.GetVerificationChallenge())
	}
	racer := endpointRow(db, reply.GetEndpointId())
	if racer == nil || racer.Description != "抢先提交的对手方" {
		t.Fatalf("重放必须指向库里既存的那一行，实际 %+v", racer)
	}
	if n := countEndpoints(db, testAppID, hookURL2); n != 1 {
		t.Fatalf("并发登记留下 %d 行，期望 1", n)
	}
}

func TestRegisterWebhook_RejectsInternalAndNonHTTPSCallbackURL(t *testing.T) {
	db, s := webhookFixture(t)

	// 每一条都是「一旦入库就是长期外呼暴露面」的地址：SSRF 护栏必须在写之前拦住。
	cases := []struct{ name, raw string }{
		{"明文 http", "http://hook.example.test/v1"},
		{"缺 scheme", "hook.example.test/v1"},
		{"空地址", ""},
		{"纯空白地址", "   "},
		{"file 协议", "file:///etc/passwd"},
		{"回环 IPv4", "https://127.0.0.1/v1"},
		{"回环 IPv6", "https://[::1]/v1"},
		{"RFC1918 网段", "https://10.0.0.5/v1"},
		{"家用网段", "https://192.168.1.10/v1"},
		{"链路本地/云元数据", "https://169.254.169.254/latest/meta-data/"},
		{"CGNAT 段", "https://100.64.0.1/v1"},
		{"组播", "https://224.0.0.1/v1"},
		{"未指定地址", "https://0.0.0.0/v1"},
		{"localhost", "https://localhost:8443/v1"},
		{".localhost 后缀", "https://db.localhost/v1"},
		{".internal 后缀", "https://svc.internal/v1"},
		{".local 后缀", "https://host.local/v1"},
		{"反向解析域", "https://5.6.7.10.in-addr.arpa/v1"},
		{"云元数据主机名", "https://metadata.google.internal/v1"},
		{"夹带 userinfo 凭证", "https://user:pass@hook.example.test/v1"},
		{"带 fragment", "https://hook.example.test/v1#token"},
		{"地址里含空白", "https://exa mple.test/v1"},
	}
	for _, tc := range cases {
		before := snapshotWrites(db)
		rows := endpointState(db)
		reply, err := callRegisterWebhook(t, s, registerWebhookReq(
			testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, tc.raw))
		wantFail(t, reply, err, model.ErrInvalidWebhookURL, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertEndpointStateUnchanged(t, db, rows, tc.name+"：行集不变")
	}

	// 反向护栏：公网字面 IP 必须放行。否则上面那批断言只是「全拒」的副产品。
	for _, ok := range []string{"https://93.184.216.34/v1", "https://[2001:db8::1]/v1",
		"https://hook.example.test:8443/v1"} {
		reply, err := callRegisterWebhook(t, s, registerWebhookReq(
			testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, ok))
		wantOK(t, reply, err, "公网地址应放行："+ok)
	}
}

func TestRegisterWebhook_URLGuardIsSameVerdictAsRedirectURIGuard(t *testing.T) {
	db, s := webhookFixture(t)
	// 两条链路共用 helpers.validPublicURI：口径必须一致，否则「redirect 拒的地址
	// 却能在 webhook 登记」就是同一份 SSRF 护栏上的第二个洞。逐字比较判定而不是错误码
	// （redirect 用 ErrInvalidRedirectURI、webhook 用 ErrInvalidWebhookURL 是刻意的分流）。
	for _, raw := range []string{
		hookURL, "http://hook.example.test/v1", "https://127.0.0.1/v1", "https://a.internal/v1",
		"https://u:p@hook.example.test/v1", "https://hook.example.test/v1#f", "https://[::1]/",
		"https://100.64.0.1/", "https://93.184.216.34/v1", "not a url at all",
	} {
		_, wErr := validWebhookURL(s, raw)
		_, rErr := normalizeRedirectURIs(s, []string{raw})
		if (wErr == nil) != (rErr == nil) {
			t.Fatalf("同一地址两路口径分叉：webhook err=%v redirect err=%v raw=%q", wErr, rErr, raw)
		}
	}
	// 超过 defaultWebhookURLBytes 的地址：拒，且复用的是「条数/体积」哨兵（现状记录，
	// 关键属性是绝不入库、绝不静默截断）。
	before := snapshotWrites(db)
	long := "https://hook.example.test/" + strings.Repeat("a", defaultWebhookURLBytes)
	if _, err := url.Parse(long); err != nil {
		t.Fatalf("构造超长地址失败：%v", err)
	}
	reply, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT, long))
	wantFail(t, reply, err, model.ErrTooManyRedirectURIs, "回调地址超字节上限")
	wantNoWrites(t, db, before, "超长地址零写入")
}

func TestRegisterWebhook_GatesAreSideEffectFree(t *testing.T) {
	db, s := webhookFixture(t)
	seedEndpointAt(db, testAppID, model.WebhookEventQuotaWarning, hookURL2, true, true, 0)

	cases := []struct {
		name string
		in   *rpc.RegisterWebhookReq
		want error
	}{
		{"app_id=0", &rpc.RegisterWebhookReq{}, model.ErrInvalidAppID},
		{"负 app_id", &rpc.RegisterWebhookReq{AppId: -7}, model.ErrInvalidAppID},
		{"应用不存在", registerWebhookReq(99999,
			rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, hookURL), model.ErrAppNotFound},
		{"匿名调用（operator_mid=0）", &rpc.RegisterWebhookReq{AppId: testAppID,
			EventType: rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE,
			Url:       hookURL}, model.ErrOwnerRequired},
		{"非 owner 非运营", func() *rpc.RegisterWebhookReq {
			r := registerWebhookReq(testAppID,
				rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, hookURL)
			r.OperatorMid = testOwner + 99
			return r
		}(), model.ErrOwnerRequired},
		{"自称运营但无 mid", func() *rpc.RegisterWebhookReq {
			r := registerWebhookReq(testAppID,
				rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, hookURL)
			r.OperatorMid = 0
			r.IsOperator = true
			return r
		}(), model.ErrOwnerRequired},
		{"event_type UNSPECIFIED", registerWebhookReq(testAppID,
			rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_UNSPECIFIED, hookURL), model.ErrInvalidEventType},
		{"event_type 未知值", registerWebhookReq(testAppID,
			rpc.WebhookEventType(97), hookURL), model.ErrInvalidEventType},
		{"description 超列宽", func() *rpc.RegisterWebhookReq {
			r := registerWebhookReq(testAppID,
				rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, hookURL)
			r.Description = strings.Repeat("说", maxWebhookDescRunes+1)
			return r
		}(), errDescriptionTooLong},
	}
	for _, tc := range cases {
		before := snapshotWrites(db)
		rows := endpointState(db)
		reply, err := callRegisterWebhook(t, s, tc.in)
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertEndpointStateUnchanged(t, db, rows, tc.name)
	}
	// 门禁顺序：owner 校验必须先于地址校验，越权者拿不到「地址是否合法」的反馈信号。
	probe := snapshotWrites(db)
	r := registerWebhookReq(testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE,
		"http://127.0.0.1/x")
	r.OperatorMid = testOwner + 99
	_, err := callRegisterWebhook(t, s, r)
	if !errors.Is(err, model.ErrOwnerRequired) {
		t.Fatalf("越权必须先报归属错而不是地址错，实际 %v", err)
	}
	wantNoWrites(t, db, probe, "越权探测内网地址")
}

func TestRegisterWebhook_FailClosedOnPepperAndDependency(t *testing.T) {
	req := registerWebhookReq(testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, hookURL)

	// 没有派生根就不能登记一个「将来必然无法签名」的端点（与 credentialPepper 同一闸门）。
	db, _ := webhookFixture(t)
	noPepper := newTestSvc(db)
	noPepper.Config.Security.WebhookMasterPepper = ""
	noPepper.Config.OpenPlatform.MaxWebhookEndpointsPerApp = 20
	before := snapshotWrites(db)
	reply, err := callRegisterWebhook(t, noPepper, req)
	wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "缺 webhook pepper")
	wantNoWrites(t, db, before, "缺 pepper")

	// 写令牌先于写库：被限流的请求一行都不落。
	limited := limitedSvc(db)
	limited.Config.OpenPlatform.MaxWebhookEndpointsPerApp = 20
	before = snapshotWrites(db)
	reply, err = callRegisterWebhook(t, limited, req)
	wantFail(t, reply, err, model.ErrRateLimited, "写侧限流")
	wantNoWrites(t, db, before, "限流后零写入")

	// 读侧故障一律透传：绝不能把「读不到」解释成「没有端点」然后继续写。
	// pre 负责把执行流推到 op 那一步之前，max 决定上限分支是否命中。
	failClosed := func(name string, max int32, pre func(*store), op string) {
		t.Helper()
		d := newStore()
		seedApp(d, testAppID, testOwner)
		svcCtx := newTestSvc(d)
		svcCtx.Config.OpenPlatform.MaxWebhookEndpointsPerApp = max
		pre(d)
		rows := endpointState(d)
		d.failOn(op, errFakeDown)
		writes := snapshotWrites(d)
		got, err := callRegisterWebhook(t, svcCtx, req)
		if got != nil || !errors.Is(err, errFakeDown) {
			t.Fatalf("%s/%s：应透传下游故障，实际 reply=%v err=%v", name, op, got, err)
		}
		assertEndpointStateUnchanged(t, d, rows, name+"：故障未改动任何行")
		if n := d.count("WebhookEndpoints.Insert") - writes["WebhookEndpoints.Insert"]; n > 1 {
			t.Fatalf("%s/%s：故障后仍重试写入 %d 次", name, op, n)
		}
		for _, side := range []string{"WebhookEndpoints.SoftDelete", "WebhookEndpoints.MarkVerified",
			"WebhookEndpoints.SetEnabled", "WebhookDeliveries.Insert"} {
			if n := d.count(side) - writes[side]; n != 0 {
				t.Fatalf("%s/%s：%s 被顺手调用 %d 次", name, op, side, n)
			}
		}
	}
	failClosed("归属读取", 20, func(*store) {}, "Apps.FindByID")
	failClosed("上限计数", 20, func(*store) {}, "WebhookEndpoints.CountByApp")
	failClosed("满额后比对已登记端点", 1, func(d *store) {
		seedEndpointAt(d, testAppID, model.WebhookEventContentPublishResult, hookURL2, true, true, 0)
	}, "WebhookEndpoints.ListByApp")
	failClosed("幂等回放回查", 20, func(d *store) {
		// 同 (应用, 事件, 地址) 已有一行：Insert 撞 uniq 回既有 ID，随后 FindByID 回查失败。
		seedEndpointAt(d, testAppID, model.WebhookEventContentOffline, hookURL, true, true, 0)
	}, "WebhookEndpoints.FindByID")
}

func TestRegisterWebhook_InsertFailureKeepsNoEndpointRow(t *testing.T) {
	db, s := webhookFixture(t)
	db.failOn("WebhookEndpoints.Insert", errFakeDown)
	before := snapshotWrites(db)
	rows := len(db.eps)

	reply, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, hookURL))
	if err == nil || !strings.Contains(err.Error(), "fake: downstream") {
		t.Fatalf("INSERT 失败必须透传：%v", err)
	}
	if reply != nil {
		t.Fatal("写失败不得回 endpoint_id：应用拿不到可用端点")
	}
	wantOnlyWriteAttempt(t, db, before, "WebhookEndpoints.Insert", "INSERT 失败没有第二次写")
	if len(db.eps) != rows {
		t.Fatalf("INSERT 失败仍落了行：%d → %d", rows, len(db.eps))
	}
}

func TestRegisterWebhook_EndpointLimitStillAnswersIdempotentReplay(t *testing.T) {
	db, s := webhookFixture(t)
	s.Config.OpenPlatform.MaxWebhookEndpointsPerApp = 2
	ev := rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_PUBLISH_RESULT

	// 两行「还活着」的端点占满额度（其中一行是已登记的 hookURL）。
	seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	seedEndpointAt(db, testAppID, model.WebhookEventContentOffline, hookURL2, true, true, 0)
	live := endpointState(db)

	before := snapshotWrites(db)
	reply, err := callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_QUOTA_WARNING, "https://new.example.test/v1"))
	wantFail(t, reply, err, errWebhookEndpointLimit, "已达上限再登记新地址")
	wantNoWrites(t, db, before, "上限拒绝")
	assertEndpointStateUnchanged(t, db, live, "上限拒绝不改任何行")

	// 满额时的重复注册必须仍然成功：否则应用再也查不到自己已登记的端点，
	// 也只能靠「删一个再加一个」来重放，那是把幂等做成破坏性操作。
	reply, err = callRegisterWebhook(t, s, registerWebhookReq(testAppID, ev, hookURL))
	reply = wantOK(t, reply, err, "满额时的幂等回放")
	if reply.GetCreated() || reply.GetVerificationChallenge() != "" {
		t.Fatalf("回放形态不对：%+v", reply)
	}
	if want := endpointRow(db, reply.GetEndpointId()); want == nil || want.URL != hookURL {
		t.Fatalf("回放未指向既有行：%+v", want)
	}
	wantNoWrites(t, db, before, "上限分支的回放不新增行")

	// 同 (事件, 地址) 的软删行仍占 uniq 键：登记回去必须显式报错——那一行永远投不出去，
	// 把它的 endpoint_id 当成可用端点回给应用是骗人。上限先放宽，让执行流落到写阶段，
	// 否则撞到的会是「已达上限」而不是「唯一键被已删行占用」，两件事不是一回事。
	third := seedEndpointAt(db, testAppID, model.WebhookEventQuotaWarning,
		"https://gone.example.test/v1", true, true, 0)
	third.DeletedAt = nowTS()
	s.Config.OpenPlatform.MaxWebhookEndpointsPerApp = 20
	collided := endpointState(db)
	before = snapshotWrites(db)
	reply, err = callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_QUOTA_WARNING, "https://gone.example.test/v1"))
	wantFail(t, reply, err, model.ErrWebhookAlreadyRegistered, "重复注册撞上已软删行")
	wantOnlyWriteAttempts(t, db, before, map[string]int{"WebhookEndpoints.Insert": 1},
		"唯一键命中不产生第二行，也不得顺手改任何行")
	assertEndpointStateUnchanged(t, db, collided, "已软删行既没被复活也没被复制")

	// 软删行不计入额度：腾出一个位置后新地址仍能登记（CountByApp 与 findRegisteredEndpoint
	// 读的是同一个「未删除」行集，两处口径必须一致，否则上限会随删除次数越收越紧）。
	endpointRow(db, seedHookURL2ID(db)).DeletedAt = nowTS()
	s.Config.OpenPlatform.MaxWebhookEndpointsPerApp = 2
	reply, err = callRegisterWebhook(t, s, registerWebhookReq(
		testAppID, rpc.WebhookEventType_WEBHOOK_EVENT_TYPE_CONTENT_OFFLINE, "https://fresh.example.test/v1"))
	reply = wantOK(t, reply, err, "已删端点不占额度")
	if !reply.GetCreated() {
		t.Fatalf("额度算错了：已删除端点仍在占位：%+v", reply)
	}
}

// seedHookURL2ID 定位 hookURL2 那一行并返回主键（腾额度用，避免测试里到处记播种顺序号）。
func seedHookURL2ID(db *store) int64 {
	for _, ep := range db.eps {
		if ep.URL == hookURL2 {
			return ep.EndpointID
		}
	}
	return 0
}

// ---------------------------------------------------------------- 列表

func TestListWebhooks_ProjectionMatchesStoredRowsAndNeverLeadsDelivery(t *testing.T) {
	db, s := webhookFixture(t)
	base := nowTS()
	verified := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	verified.Description = "投稿结果"
	verified.SignKeyVersion = 2
	verified.ChallengeHash = "deadbeef-hash-must-never-leave-the-server"
	verified.ChallengeExpiresAt = base + challengeTTLSeconds
	unverified := seedEndpointAt(db, testAppID, model.WebhookEventContentOffline, hookURL2, false, true, 0)
	disabled := seedEndpointAt(db, testAppID, model.WebhookEventQuotaWarning,
		"https://paused.example.test/v1", true, false, 0)
	deleted := seedEndpointAt(db, testAppID, model.WebhookEventGrantRevoked,
		"https://deleted.example.test/v1", true, true, base-10)
	deleted.DeleteReason = "违规处置"
	other := seedEndpointAt(db, testApp2, model.WebhookEventContentPublishResult,
		"https://other.example.test/v1", true, true, 0)
	// 播种顺序刻意与 (event_type, endpoint_id) 升序反向，用来钉「顺序由查询给出」而不是碰巧。
	for i, ep := range []*model.WebhookEndpoint{verified, unverified, disabled, deleted, other} {
		ep.Ctime = int64(1000 + i)
		ep.Mtime = int64(2000 + i)
	}
	before := snapshotWrites(db)
	lg := NewListWebhooksLogic(t.Context(), s)

	assertRow := func(info *rpc.WebhookEndpointInfo, row *model.WebhookEndpoint, label string) {
		t.Helper()
		if info.GetEndpointId() != row.EndpointID || info.GetAppId() != row.AppID {
			t.Fatalf("%s：定位错行 %+v vs %+v", label, info, row)
		}
		if int32(info.GetEventType()) != row.EventType || info.GetUrl() != row.URL {
			t.Fatalf("%s：订阅关系不符 info=(%v,%s) row=(%d,%s)", label,
				info.GetEventType(), info.GetUrl(), row.EventType, row.URL)
		}
		if info.GetSignKeyVersion() != row.SignKeyVersion || info.GetEnabled() != (row.Enabled == 1) {
			t.Fatalf("%s：签名版本/启停不符：%+v row en=%d kv=%d", label, info, row.Enabled, row.SignKeyVersion)
		}
		if info.GetDescription() != row.Description || info.GetCtime() != row.Ctime ||
			info.GetMtime() != row.Mtime {
			t.Fatalf("%s：备注或时间戳与行不符：%+v", label, info)
		}
		// 未验证就回 0：授权页据此提示「尚未验证，不会收到推送」，绝不用 ctime 伪装。
		if info.GetVerifiedAt() != row.VerifiedAt {
			t.Fatalf("%s：verified_at=%d 与行 %d 不一致", label, info.GetVerifiedAt(), row.VerifiedAt)
		}
	}

	only, err := lg.ListWebhooks(&rpc.ListWebhooksReq{AppId: testAppID})
	only = wantOK(t, only, err, "只看启用端点")
	if len(only.GetList()) != 2 {
		t.Fatalf("n=%d，期望 2（停用与软删都不回）：%+v", len(only.GetList()), only.GetList())
	}
	// (event_type, endpoint_id) 升序：播种顺序是反的，能对得上就说明顺序不是插入顺序。
	assertRow(only.GetList()[0], verified, "第一行")
	assertRow(only.GetList()[1], unverified, "第二行")

	withDisabled, err := lg.ListWebhooks(&rpc.ListWebhooksReq{AppId: testAppID, IncludeDisabled: true})
	withDisabled = wantOK(t, withDisabled, err, "含停用端点")
	if len(withDisabled.GetList()) != 3 {
		t.Fatalf("n=%d，期望 3：%+v", len(withDisabled.GetList()), withDisabled.GetList())
	}
	assertRow(withDisabled.GetList()[2], disabled, "停用行")
	// 软删行永远不回：本方法不是「已删配置」的查询面（那归投递台账解释）。
	for _, info := range withDisabled.GetList() {
		if info.GetEndpointId() == deleted.EndpointID || info.GetAppId() == testApp2 {
			t.Fatalf("列表漏进了不该出现的行：%+v", info)
		}
	}

	// 验证材料与密钥材料一处都不能出现：库里本就只存 challenge_hash 与版本号，
	// 响应投影里连字段都没有。这条断言挡的是「以后有人给投影加一列」。
	dump := fmt.Sprintf("%+v", withDisabled)
	for _, banned := range []string{deleted.DeleteReason, "deadbeef-hash", "ChallengeHash",
		"challenge_hash", "DeleteReason"} {
		if strings.Contains(dump, banned) {
			t.Fatalf("回调端点列表外泄了 %q：%s", banned, dump)
		}
	}
	// 运营自查与 owner 自查读到同一行集，差别只进日志。
	asOp, err := lg.ListWebhooks(&rpc.ListWebhooksReq{AppId: testAppID, OperatorMid: hookOperator,
		IncludeDisabled: true})
	asOp = wantOK(t, asOp, err, "运营视角")
	if fmt.Sprintf("%+v", asOp.GetList()) != fmt.Sprintf("%+v", withDisabled.GetList()) {
		t.Fatal("运营与 owner 读到不同行集：本方法按 app_id 取数，身份不该改变可见性")
	}

	wantNoWrites(t, db, before, "列表全程只读")
}

func TestListWebhooks_GatesTruncationAndFailClosed(t *testing.T) {
	db, s := webhookFixture(t)
	lg := NewListWebhooksLogic(t.Context(), s)

	before := snapshotWrites(db)
	cases := []struct {
		name string
		in   *rpc.ListWebhooksReq
		want error
	}{
		{"app_id=0", &rpc.ListWebhooksReq{}, model.ErrInvalidAppID},
		{"负 app_id", &rpc.ListWebhooksReq{AppId: -1}, model.ErrInvalidAppID},
		{"负 operator_mid", &rpc.ListWebhooksReq{AppId: testAppID, OperatorMid: -5},
			model.ErrOperatorRequired},
	}
	for _, tc := range cases {
		reply, err := lg.ListWebhooks(tc.in)
		wantFail(t, reply, err, tc.want, tc.name)
	}
	// 应用不存在必须报错而不是回空数组：空数组会被上游解释成「这个应用没配回调」。
	_, err := lg.ListWebhooks(&rpc.ListWebhooksReq{AppId: 4242})
	if err == nil || !strings.Contains(err.Error(), "application not found") {
		t.Fatalf("未知应用应报 ErrAppNotFound，实际 %v", err)
	}
	wantNoWrites(t, db, before, "列表门禁零副作用")

	// 库里出现配置之外的行数（运维直写/上限被下调）：截断并告警，而不是放行无界列表。
	s.Config.OpenPlatform.MaxWebhookEndpointsPerApp = 1
	kept := seedEndpointAt(db, testAppID, model.WebhookEventContentPublishResult, hookURL, true, true, 0)
	seedEndpointAt(db, testAppID, model.WebhookEventContentOffline, hookURL2, true, true, 0)
	trunc, err := lg.ListWebhooks(&rpc.ListWebhooksReq{AppId: testAppID})
	trunc = wantOK(t, trunc, err, "超上限截断")
	if len(trunc.GetList()) != 1 || trunc.GetList()[0].GetEndpointId() != kept.EndpointID {
		t.Fatalf("截断后 %+v，期望只回 event_type 最小的那一行", trunc.GetList())
	}

	// 读失败一律透传，不能退化成「空列表」。
	db.failOn("WebhookEndpoints.ListByApp", errFakeDown)
	reply, err := lg.ListWebhooks(&rpc.ListWebhooksReq{AppId: testAppID})
	wantFail(t, reply, err, errFakeDown, "ListByApp 故障必须透传而不是回空列表")
	wantNoWrites(t, db, before, "读故障零写入")
}

// ---------------------------------------------------------------- 删除

func TestDeleteWebhook_SuppressesInFlightAndKeepsTerminalTruth(t *testing.T) {
	db, s := webhookFixture(t)
	ep := seedEndpointAt(db, testAppID, model.WebhookEventGrantRevoked, hookURL, true, true, 0)
	inFlight := []*model.WebhookDelivery{
		seedDelivery(db, testAppID, ep, "evt-pending", model.DeliveryStatePending, `{"a":1}`),
		seedDelivery(db, testAppID, ep, "evt-delivering", model.DeliveryStateDelivering, `{"a":2}`),
		seedDelivery(db, testAppID, ep, "evt-retry", model.DeliveryStateRetryScheduled, `{"a":3}`),
	}
	done := seedDelivery(db, testAppID, ep, "evt-success", model.DeliveryStateSuccess, `{"a":4}`)
	dead := seedDelivery(db, testAppID, ep, "evt-dead", model.DeliveryStateDead, `{"a":5}`)
	ignored := seedDelivery(db, testAppID, ep, "evt-ignored", model.DeliveryStateIgnored, `{"a":6}`)
	otherEp := seedEndpointAt(db, testAppID, model.WebhookEventContentOffline, hookURL2, true, true, 0)
	untouched := seedDelivery(db, testAppID, otherEp, "evt-other", model.DeliveryStatePending, `{"a":7}`)
	deliveries := len(db.dels)
	before := snapshotWrites(db)

	reply, err := callDeleteWebhook(t, s, deleteWebhookReq(testAppID, ep.EndpointID, testOwner, false,
		"  应用方要求下线  "))
	reply = wantOK(t, reply, err, "owner 删除端点")
	wantCalls(t, db, "DB.TransactCtx", before["DB.TransactCtx"], 1, "封端点与抑制任务共用一个事务")
	wantCalls(t, db, "WebhookEndpoints.SoftDelete", before["WebhookEndpoints.SoftDelete"], 1, "端点只软删一次")
	if !reply.GetDeleted() {
		t.Fatal("首次删除必须 deleted=true")
	}
	if reply.GetDeliveriesSuppressed() != 3 {
		t.Fatalf("抑制数=%d，期望 3（pending/delivering/retry_scheduled）", reply.GetDeliveriesSuppressed())
	}

	row := endpointRow(db, ep.EndpointID)
	if row == nil {
		t.Fatal("删除必须是软删：行还在才能长期解释投递归属")
	}
	if row.DeletedAt == 0 || row.Enabled != 0 {
		t.Fatalf("端点未终态化：deleted_at=%d enabled=%d", row.DeletedAt, row.Enabled)
	}
	if row.DeleteReason != "应用方要求下线" {
		t.Fatalf("delete_reason=%q，应入库去空白后的原因", row.DeleteReason)
	}
	if row.URL != hookURL || row.VerifiedAt != ep.VerifiedAt {
		t.Fatal("删除改写了地址或验证位点：配置终态化不是抹历史")
	}
	for _, d := range inFlight {
		if d.State != model.DeliveryStateIgnored {
			t.Fatalf("在途任务 %s 未抑制：state=%d", d.EventID, d.State)
		}
		if d.LastError != "webhook endpoint deleted" || d.LeaseUntil != 0 {
			t.Fatalf("抑制后的任务缺解释：last_error=%q lease=%d", d.LastError, d.LeaseUntil)
		}
	}
	// 已成功的投递与已死信/已忽略的结论是历史事实，删除配置不能改写它们。
	for _, d := range []*model.WebhookDelivery{done, dead, ignored, untouched} {
		if want := map[string]int32{"evt-success": model.DeliveryStateSuccess,
			"evt-dead": model.DeliveryStateDead, "evt-ignored": model.DeliveryStateIgnored,
			"evt-other": model.DeliveryStatePending}[d.EventID]; d.State != want {
			t.Fatalf("%s 状态被删除动作改成 %d（期望 %d）", d.EventID, d.State, want)
		}
	}
	if len(db.dels) != deliveries {
		t.Fatalf("删除端点不该造任务行：%d → %d", deliveries, len(db.dels))
	}
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"DB.TransactCtx":                       1,
		"WebhookEndpoints.SoftDelete":          1,
		"WebhookDeliveries.SuppressByEndpoint": 1,
	}, "删除只写这两处")
}

func TestDeleteWebhook_RepeatIsIdempotentAndConvergesTheWindow(t *testing.T) {
	db, s := webhookFixture(t)
	ep := seedEndpointAt(db, testAppID, model.WebhookEventGrantRevoked, hookURL, true, true, 0)
	d := seedDelivery(db, testAppID, ep, "evt-1", model.DeliveryStatePending, `{"a":1}`)

	first, err := callDeleteWebhook(t, s, deleteWebhookReq(testAppID, ep.EndpointID, hookOperator, true, "r"))
	first = wantOK(t, first, err, "运营首次删除")
	if !first.GetDeleted() || first.GetDeliveriesSuppressed() != 1 {
		t.Fatalf("首次删除 %+v", first)
	}

	// 第二次：SoftDelete 的 WHERE deleted_at=0 不命中 → deleted=false，但整体仍是成功
	// （proto:680 的幂等口径），抑制数回 0。
	before := snapshotWrites(db)
	second, err := callDeleteWebhook(t, s, deleteWebhookReq(testAppID, ep.EndpointID, hookOperator, true, "r"))
	second = wantOK(t, second, err, "重复删除")
	if second.GetDeleted() {
		t.Fatal("重复删除仍报 deleted=true：行早已终态，这是在谎报第二次生效")
	}
	if second.GetDeliveriesSuppressed() != 0 {
		t.Fatalf("重复删除抑制数=%d，在途任务已清空应为 0", second.GetDeliveriesSuppressed())
	}
	wantCalls(t, db, "WebhookDeliveries.SuppressByEndpoint",
		before["WebhookDeliveries.SuppressByEndpoint"], 1, "第二次仍要跑一次抑制（条件 UPDATE 不产生变更）")

	// 「端点已停但任务未抑制」的中间窗口（方法头第 3 条承认它存在）：本调用必须收敛它。
	d.State = model.DeliveryStatePending
	d.LeaseUntil = nowTS() + 60
	third, err := callDeleteWebhook(t, s, deleteWebhookReq(testAppID, ep.EndpointID, hookOperator, true, "补一次抑制"))
	third = wantOK(t, third, err, "窗口收敛")
	if third.GetDeleted() || third.GetDeliveriesSuppressed() != 1 {
		t.Fatalf("窗口未收敛：%+v", third)
	}
	if d.State != model.DeliveryStateIgnored || d.LeaseUntil != 0 {
		t.Fatalf("漏网任务未被收回：state=%d lease=%d", d.State, d.LeaseUntil)
	}
}

func TestDeleteWebhook_GatesOwnershipAndFailClosed(t *testing.T) {
	db, s := webhookFixture(t)
	ep := seedEndpointAt(db, testAppID, model.WebhookEventGrantRevoked, hookURL, true, true, 0)
	seedDelivery(db, testAppID, ep, "evt-1", model.DeliveryStatePending, `{"a":1}`)
	otherEp := seedEndpointAt(db, testApp2, model.WebhookEventGrantRevoked, hookURL2, true, true, 0)

	cases := []struct {
		name string
		in   *rpc.DeleteWebhookReq
		want error
	}{
		{"app_id=0", &rpc.DeleteWebhookReq{}, model.ErrInvalidAppID},
		{"应用不存在", deleteWebhookReq(4242, ep.EndpointID, testOwner, false, "r"), model.ErrAppNotFound},
		{"缺 operator_mid", deleteWebhookReq(testAppID, ep.EndpointID, 0, false, "r"), model.ErrOwnerRequired},
		{"非 owner", deleteWebhookReq(testAppID, ep.EndpointID, testOwner+1, false, "r"),
			model.ErrOwnerRequired},
		{"缺 reason", deleteWebhookReq(testAppID, ep.EndpointID, testOwner, false, "   "),
			errReasonRequired},
		{"reason 超列宽", deleteWebhookReq(testAppID, ep.EndpointID, testOwner, false,
			strings.Repeat("因", maxReasonRunes+1)), errReasonTooLong},
		{"endpoint_id=0", deleteWebhookReq(testAppID, 0, testOwner, false, "r"), model.ErrWebhookNotFound},
		{"endpoint_id 负数", deleteWebhookReq(testAppID, -3, testOwner, false, "r"), model.ErrWebhookNotFound},
		{"endpoint 不存在", deleteWebhookReq(testAppID, 999999, testOwner, false, "r"),
			model.ErrWebhookNotFound},
		// 跨应用探测与「不存在」同口径：不给「猜别人 endpoint_id」留可分辨信号。
		{"endpoint 属别的应用", deleteWebhookReq(testAppID, otherEp.EndpointID, testOwner, false, "r"),
			model.ErrWebhookNotFound},
	}
	for _, tc := range cases {
		before := snapshotWrites(db)
		rows := endpointState(db)
		states := deliveryState(db)
		reply, err := callDeleteWebhook(t, s, tc.in)
		wantFail(t, reply, err, tc.want, tc.name)
		wantNoWrites(t, db, before, tc.name)
		assertEndpointStateUnchanged(t, db, rows, tc.name+"：端点行不变")
		assertDeliveryStateUnchanged(t, db, states, tc.name+"：投递行不变")
	}

	// 归属校验必须先于 reason 校验：越权者不能用「缺 reason」探端点是否存在。
	before := snapshotWrites(db)
	r := deleteWebhookReq(testAppID, otherEp.EndpointID, testOwner+1, false, "")
	_, err := callDeleteWebhook(t, s, r)
	if err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("越权必须先报归属错，实际 %v", err)
	}
	wantNoWrites(t, db, before, "越权探测")

	// 读侧失败：宁可不做删除，也不能「端点没停但报成功」。
	db.failOn("WebhookEndpoints.FindByID", errFakeDown)
	reply, err := callDeleteWebhook(t, s, deleteWebhookReq(testAppID, ep.EndpointID, testOwner, false, "r"))
	wantFail(t, reply, err, errFakeDown, "端点读取失败必须透传")
	wantNoWrites(t, db, before, "读失败不写")
}

func TestDeleteWebhook_SuppressFailurePropagates(t *testing.T) {
	db, s := webhookFixture(t)
	ep := seedEndpointAt(db, testAppID, model.WebhookEventGrantRevoked, hookURL, true, true, 0)
	seedDelivery(db, testAppID, ep, "evt-1", model.DeliveryStatePending, `{"a":1}`)
	db.failOn("WebhookDeliveries.SuppressByEndpoint", errFakeDown)
	before := snapshotWrites(db)

	reply, err := callDeleteWebhook(t, s, deleteWebhookReq(testAppID, ep.EndpointID, testOwner, false, "r"))
	if err == nil || !strings.Contains(err.Error(), "fake: downstream") {
		t.Fatalf("抑制失败必须整体报错，不能回 deleted=true：%v", err)
	}
	if reply != nil {
		t.Fatal("事务失败仍带回响应体")
	}
	// 两步写共用一次 TransactCtx：真实现中途失败会整体回滚（内存 fake 只做行集回滚，
	// 原地字段改写在 fake 里不可回退，所以这里只钉「没有第三步写、响应不谎报成功」）。
	wantOnlyWriteAttempts(t, db, before, map[string]int{
		"DB.TransactCtx":                       1,
		"WebhookEndpoints.SoftDelete":          1,
		"WebhookDeliveries.SuppressByEndpoint": 1,
	}, "抑制失败后没有第二次抑制尝试")
}

// ---------------------------------------------------------------- 跨用例小工具

// deliveryState 把整张投递表折成可逐条比对的字符串。
func deliveryState(db *store) map[int64]string {
	out := make(map[int64]string, len(db.dels))
	for _, d := range db.dels {
		out[d.DeliveryID] = fmt.Sprintf("app=%d ep=%d ev=%d eid=%s payload=%q digest=%q state=%d "+
			"attempt=%d max=%d next=%d lease=%d status=%d err=%q ctime=%d mtime=%d",
			d.AppID, d.EndpointID, d.EventType, d.EventID, d.Payload, d.PayloadDigest, d.State,
			d.Attempt, d.MaxAttempts, d.NextRetryAt, d.LeaseUntil, d.LastStatusCode, d.LastError,
			d.Ctime, d.Mtime)
	}
	return out
}

func assertDeliveryStateUnchanged(t *testing.T, db *store, before map[int64]string, label string) {
	t.Helper()
	after := deliveryState(db)
	if len(after) != len(before) {
		t.Fatalf("%s：投递行数 %d → %d", label, len(before), len(after))
	}
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("%s：投递 %d 被改动\n改前 %s\n改后 %s", label, id, want, got)
		}
	}
}

// wantOnlyWriteAttempts 是 wantOnlyWriteAttempt 的多算子版本：
// db.failOn 先计数再返回错误，所以「写失败」用例里被注入故障的那一次尝试必须放行（增量为其配额），
// 其余写侧方法一次都不能发生。
func wantOnlyWriteAttempts(t *testing.T, db *store, before map[string]int, quota map[string]int,
	label string) {
	t.Helper()
	for name, n := range before {
		want := quota[name]
		if got := db.count(name) - n; got != want {
			t.Fatalf("%s：%s 调用增量=%d，期望 %d", label, name, got, want)
		}
	}
}
