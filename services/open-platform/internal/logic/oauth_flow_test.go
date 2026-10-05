package logic

// oauth_flow_test.go：OAuth 授权链路五法的契约测试
// （IssueAuthorizationCode / ExchangeAuthorizationCode / IntrospectToken /
//  RevokeAuthorization / AuthorizeRequest）。
//
// 契约依据：proto:310-326（换码一致性与一次性）、proto:342-361（RFC 7009 撤销 +
// 「位点 + 逐条标记」双保险）、proto:363-382（校验顺序）、proto:384-415
// （AuthorizeRequest 的门禁顺序与 request_id 幂等）。
//
// 本文件钉住的不变量（安全语义优先，逐条对照实现后断言）：
//  1. redirect_uri：注册期只收 https/非内网地址（app_lifecycle_test.go 已覆盖），
//     签发期必须逐字命中白名单（前缀、加路径段、加查询串都算 miss），
//     换码期必须与授权时逐字一致，且不一致时**绝不消耗 code**；
//  2. 授权码一次性：重放、过期、并发只有一方成功，重放必须留下 replay_count 证据；
//  3. scope 只能收窄不能放大：请求 ⊆ 应用已获批 ⊆ 目录开放，任一环越界即拒；
//  4. 应用状态门禁：非 ACTIVE 一律拒绝签发/兑换/校验（requireActiveApp 是唯一实现）；
//  5. Introspect 判定完全走 DB 真值：撤销、过期、换主密钥都立刻反映，定位缓存不参与结论；
//  6. 撤销按目标与归属收紧，撤销后凭证立即不可用，重复撤销可重入且不再外呼通知；
//  7. 失败路径零副作用（wantNoWrites + 关键表行数不变），成功路径响应字段 == 入库真值。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

const (
	oauthRedirect  = "https://app.example.test/cb"
	oauthRedirect2 = "https://alt.example.test/cb"
	// oauthMaxURIBytes / oauthMaxURIs / oauthCodeRateLimit 是 config 里的 json default 值。
	oauthMaxURIBytes  = 512
	oauthMaxURIs      = 5
	oauthCodeRateLine = 30
)

// oauthSvc 在 newTestSvc 之上补齐「线上由 go-zero 默认值注入、手工装配结构体拿不到」的配置。
// 不补的后果不是「测试更宽松」，而是「测试跑的不是线上那条分支」：
//   - MaxRedirectURIBytes=0 会让 requireLen 把任何非空回调地址判成超长，签发与换码直接失败；
//   - AuthCodeMaxPerUserPerHour=0 会让签发限频整段被跳过，而线上默认 30 是会读计数器的。
func oauthSvc(db *store) *svc.ServiceContext {
	s := newTestSvc(db)
	s.Config.OpenPlatform.MaxRedirectURIBytes = oauthMaxURIBytes
	s.Config.OpenPlatform.MaxRedirectURIs = oauthMaxURIs
	s.Config.OpenPlatform.AuthCodeMaxPerUserPerHour = oauthCodeRateLine
	return s
}

// oauthFixture：ACTIVE 应用 + 回调白名单 + 权限点目录（读 + 声明需同意的写）+ 已获批 scope 集。
func oauthFixture(t *testing.T, scopes ...string) (*store, *svc.ServiceContext) {
	t.Helper()
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	seedWriteScope(db, testScopeW)
	app := seedApp(db, testAppID, testOwner, scopes...)
	app.RedirectURIs = oauthRedirect
	return db, oauthSvc(db)
}

// oauthTokenFixture：在 oauthFixture 之上补一条「用户已授权 + 一代可用 token」，
// 返回 access/refresh 明文与行指针，供 Introspect / AuthorizeRequest / Revoke 共用。
func oauthTokenFixture(t *testing.T, scopes []string, consent int8) (
	*store, *svc.ServiceContext, *model.Grant, string, string, *model.Token) {
	t.Helper()
	db, s := oauthFixture(t, scopes...)
	grant := seedGrant(db, testAppID, testMid, scopes, consent)
	access, refresh, tok := seedToken(t, db, grant, scopes, 10, 3600, 2592000)
	return db, s, grant, access, refresh, tok
}

func issueReq(appID, mid int64, redirect string, scopes []string) *rpc.IssueAuthorizationCodeReq {
	return &rpc.IssueAuthorizationCodeReq{
		AppId: appID, Mid: mid, Scope: scopes, RedirectUri: redirect,
		ConsentGiven: true, State: "st-1",
	}
}

// issueOK 走一次正常签发（返回明文 code 只在响应里出现一次）。
func issueOK(t *testing.T, s *svc.ServiceContext, scopes ...string) *rpc.IssueAuthorizationCodeReply {
	t.Helper()
	reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
		IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, scopes))
	return wantOK(t, reply, err, "签发授权码")
}

// issuedCode 用真签发路径取一枚可用授权码（不用手搓行，保证测的是「实现签出来的东西」）。
// 返回明文、授权码行与它绑定的 grant。
func issuedCode(t *testing.T, db *store, s *svc.ServiceContext, scopes ...string) (
	string, *model.AuthCode, *model.Grant) {
	t.Helper()
	reply := issueOK(t, s, scopes...)
	row := codeRowByHash(t, db, reply.Code)
	grant, ok := db.grants[reply.GrantId]
	if !ok {
		t.Fatalf("签发的 grant_id=%d 不在库里", reply.GrantId)
	}
	return reply.Code, row, grant
}

// codeRowByHash 按「明文 → HMAC(pepper, 明文)」这条唯一读路径找回授权码行。
func codeRowByHash(t *testing.T, db *store, plain string) *model.AuthCode {
	t.Helper()
	hash := mustHash(t, plain)
	for _, row := range db.codes {
		if row.Hash == hash {
			return row
		}
	}
	t.Fatal("按值定位不到刚签发的授权码：入库哈希与读路径口径不一致，换码必然失败")
	return nil
}

// exchange 换码（redirect 由调用方给，负例要传与签发时不同的值）。
func exchange(s *svc.ServiceContext, appID int64, code, redirect string) (
	*rpc.ExchangeAuthorizationCodeReply, error) {
	return NewExchangeAuthorizationCodeLogic(context.Background(), s).
		ExchangeAuthorizationCode(&rpc.ExchangeAuthorizationCodeReq{
			AppId: appID, Code: code, RedirectUri: redirect,
		})
}

func introspect(s *svc.ServiceContext, accessToken string, tokenID int64,
	requiredScope string) (*rpc.IntrospectTokenReply, error) {
	return NewIntrospectTokenLogic(context.Background(), s).
		IntrospectToken(&rpc.IntrospectTokenReq{
			AccessToken: accessToken, TokenId: tokenID, RequiredScope: requiredScope,
		})
}

// dupHashAuthCodes 把「uniq_hash 撞上并发对手先落的行」这条分支变成可测路径。
// 授权码哈希是随机值的 HMAC，onHit 无从预知它，只能在插入这一句执行前按同一哈希抄一行占位；
// 占位行进入的是事务快照之后的内存态，回滚时随快照一起消失，因此不削弱零残留断言。
type dupHashAuthCodes struct {
	model.AuthCodeModel
	db *store
}

const racerCodeID = int64(999999)

func (d dupHashAuthCodes) InsertTx(ctx context.Context, session sqlx.Session,
	a *model.AuthCode) (int64, bool, error) {
	d.db.codes[racerCodeID] = &model.AuthCode{
		CodeID: racerCodeID, AppID: a.AppID, Mid: a.Mid, Salt: a.Salt, Hash: a.Hash,
		Scope: a.Scope, RedirectURI: a.RedirectURI, State: a.State,
		GrantID: a.GrantID, ExpiresAt: a.ExpiresAt,
	}
	return d.AuthCodeModel.InsertTx(ctx, session, a)
}

// wantWindow 断言「相对现在的秒数差」落在 [want-slack, want+slack]：
// logic 与测试各自读一次时钟，跨秒抖动不能让断言变成运气测试。
func wantWindow(t *testing.T, got, want, slack int64, label string) {
	t.Helper()
	if got < want-slack || got > want+slack {
		t.Fatalf("%s=%d，期望 %d±%d", label, got, want, slack)
	}
}

// wantScopeList 逐条比对响应里的 scope 与「独立写出的期望集合」。
// 不用 model.SplitScopes(库里那一列) 当期望值：那是用被测代码自己的函数造期望，
// 快照漂移会被一起算进期望值而检查不出来。
func wantScopeList(t *testing.T, got, want []string, label string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s=%v，期望 %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s=%v，期望 %v", label, got, want)
		}
	}
}

// tooManyScopes 造一份「条数刚好越过单次请求上限」的 scope 列表。
// 上限必须按归一前的入参条数判定：按归一后判定的话，重复项就能把「一次要 100 个 scope」
// 伪装成 50 个以下，放大权限的请求面随之被放宽。
func tooManyScopes() []string {
	out := make([]string, 0, maxScopeCountPerReq+1)
	for i := 0; i <= maxScopeCountPerReq; i++ {
		out = append(out, fmt.Sprintf("api.scope%03d", i))
	}
	return out
}

// ---------------------------------------------------------------- 签发授权码

func TestIssueAuthorizationCode_BindsCodeToAppRedirectAndScopeTruth(t *testing.T) {
	db, s := oauthFixture(t, testScopeR, testScopeW)
	scopes := []string{testScopeW, testScopeR} // 故意乱序：入库快照必须是规范化升序集
	before := nowTS()

	reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
		IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, scopes))
	reply = wantOK(t, reply, err, "正常签发")

	// 响应 ↔ 入库真值：一条一条对，不看「err==nil」就走。
	row := codeRowByHash(t, db, reply.Code)
	if row.AppID != testAppID || row.Mid != testMid {
		t.Fatalf("码的应用/用户归属错：app_id=%d mid=%d", row.AppID, row.Mid)
	}
	if row.RedirectURI != oauthRedirect {
		t.Fatalf("入库回调地址=%q，与请求逐字值不一致", row.RedirectURI)
	}
	if row.State != "st-1" {
		t.Fatalf("入库 state=%q，期望原样回显（state 是 CSRF 关联位点）", row.State)
	}
	if model.JoinScopes(reply.Scope) != row.Scope {
		t.Fatalf("响应 scope=%v 与入库快照 %q 不是同一份", reply.Scope, row.Scope)
	}
	if row.Scope != model.JoinScopes(scopes) {
		t.Fatalf("入库快照=%q，期望规范化（去重升序）%q", row.Scope, model.JoinScopes(scopes))
	}
	if !row.Usable(nowTS()) {
		t.Fatalf("刚签发的授权码不可用（used_at=%d expires_at=%d），换码必然失败", row.UsedAt, row.ExpiresAt)
	}
	// 明文只出一次：库里只有 salt + HMAC(pepper, 明文)。
	if row.Salt == "" || row.Hash == "" {
		t.Fatalf("凭证列不完整：salt/hash=%q/%q", row.Salt, row.Hash)
	}
	if row.Hash == reply.Code || row.Salt == reply.Code {
		t.Fatal("哈希列被明文覆盖")
	}
	if want := mustHash(t, reply.Code); row.Hash != want {
		t.Fatalf("入库哈希与 HMAC(pepper, 明文) 口径不符，换码必然定位不到")
	}
	mustNoPlaintextStored(t, db, "签发授权码", reply.Code)

	// 时效：expires_at = 签发时刻 + AuthCodeTTLSeconds（短期是防截获的核心手段）。
	wantWindow(t, row.ExpiresAt, before+s.Config.OpenPlatform.AuthCodeTTLSeconds, 2, "expires_at")
	if reply.ExpiresIn != s.Config.OpenPlatform.AuthCodeTTLSeconds {
		t.Fatalf("ExpiresIn=%d，与配置 TTL %d 不一致", reply.ExpiresIn,
			s.Config.OpenPlatform.AuthCodeTTLSeconds)
	}
	if row.UsedAt != 0 || row.ConsumedByTokenID != 0 || row.ReplayCount != 0 {
		t.Fatalf("新签发的码不该带消费/重放痕迹：%+v", row)
	}

	// grant 预建：撤销位点从签发起就连贯，换码前不必回填 grant_id。
	grant := db.grants[reply.GrantId]
	if grant == nil {
		t.Fatalf("响应 grant_id=%d 不存在", reply.GrantId)
	}
	if row.GrantID != grant.GrantID {
		t.Fatalf("码绑定的 grant_id=%d，与返回的 %d 不一致", row.GrantID, grant.GrantID)
	}
	if grant.Scope != row.Scope {
		t.Fatalf("grant 快照 %q 与码快照 %q 漂移", grant.Scope, row.Scope)
	}
	if grant.ConsentGiven != 1 || grant.ConsentAt == 0 {
		t.Fatalf("用户同意未被记录：consent_given=%d consent_at=%d", grant.ConsentGiven, grant.ConsentAt)
	}
	// 一次事务、一次码写入；限频读的是签发计数。
	if db.count("AuthCodes.CountRecentByMid") != 1 {
		t.Fatalf("签发限频计数读了 %d 次，期望 1 次", db.count("AuthCodes.CountRecentByMid"))
	}
	if n := len(db.codes); n != 1 {
		t.Fatalf("授权码行数=%d，期望 1", n)
	}
}

func TestIssueAuthorizationCode_ReauthorizationReusesGrantAndClearsSite(t *testing.T) {
	db, s := oauthFixture(t, testScopeR)
	_, first, grant := issuedCode(t, db, s, testScopeR)
	// 用户先撤销：位点与状态都已前移。
	if _, err := NewRevokeAuthorizationLogic(t.Context(), s).RevokeAuthorization(
		&rpc.RevokeAuthorizationReq{
			Target: rpc.RevokeTarget_REVOKE_TARGET_GRANT, AppId: testAppID,
			Mid: testMid, OperatorMid: testMid, Reason: "用户自行解除",
		}); err != nil {
		t.Fatalf("用户撤销自己的授权应成功：%v", err)
	}
	if db.grants[grant.GrantID].RevokedAt == 0 {
		t.Fatal("前提不成立：撤销位点未写下")
	}

	second := issueOK(t, s, testScopeR)
	g := db.grants[second.GrantId]
	if g.GrantID != grant.GrantID {
		t.Fatalf("重新授权新建了 grant 行（%d → %d），会出现并行授权导致撤销漏网",
			grant.GrantID, g.GrantID)
	}
	if g.RevokedAt != 0 || g.Status != model.GrantStatusActive {
		t.Fatalf("重新授权未覆盖历史撤销：revoked_at=%d status=%d", g.RevokedAt, g.Status)
	}
	if g.RevokeReason != "" || g.RevokeOperator != 0 {
		t.Fatalf("撤销审计残留：%q / %d", g.RevokeReason, g.RevokeOperator)
	}
	if codeRowByHash(t, db, second.Code).GrantID != g.GrantID {
		t.Fatal("新码没绑到复用的 grant 上")
	}
	// 两枚码是两行（一次性消费的物理前提），不是覆盖同一行。
	if len(db.codes) != 2 {
		t.Fatalf("授权码行数=%d，期望 2（每枚各一行）", len(db.codes))
	}
	if db.codes[first.CodeID] == nil {
		t.Fatal("旧码行被覆盖而非新增：同一 code 会可被重复兑换")
	}
}

// 回调白名单是精确匹配：前缀/后缀/查询串/端口差异都必须 miss（否则白名单退化成开放重定向池）。
func TestIssueAuthorizationCode_RedirectWhitelistIsExactMatch(t *testing.T) {
	cases := []struct {
		name     string
		redirect string
		want     error
	}{
		{"逐字命中", oauthRedirect, nil},
		{"末尾多斜杠", oauthRedirect + "/", model.ErrRedirectURIMismatch},
		{"白名单是它的前缀", "https://app.example.test/cb/evil", model.ErrRedirectURIMismatch},
		{"换成根路径", "https://app.example.test", model.ErrRedirectURIMismatch},
		{"夹带查询串", oauthRedirect + "?code=1", model.ErrRedirectURIMismatch},
		{"降级为 http", "http://app.example.test/cb", model.ErrRedirectURIMismatch},
		{"换主机", "https://evil.example.test/cb", model.ErrRedirectURIMismatch},
		// 端口差异也算 miss：浏览器把 :8443 当成另一个源，回调到这里等于把码送出门。
		// （http/内网/带 userinfo 这类地址根本进不了白名单，护栏在注册期，见 app_lifecycle_test.go）
		{"换端口", "https://app.example.test:8443/cb", model.ErrRedirectURIMismatch},
		{"缺失回调", "", model.ErrInvalidRedirectURI},
		{"超长回调", "https://app.example.test/cb?" + strings.Repeat("q", oauthMaxURIBytes),
			model.ErrInvalidRedirectURI},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s := oauthFixture(t, testScopeR)
			if c.name == "超长回调" {
				// 白名单本身留一条干净的，确保失败原因是「这条太长」而不是「白名单为空」。
				s.Config.OpenPlatform.MaxRedirectURIBytes = 32
			}
			before := snapshotWrites(db)
			reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
				IssueAuthorizationCode(issueReq(testAppID, testMid, c.redirect, []string{testScopeR}))
			if c.want == nil {
				wantOK(t, reply, err, c.name)
				return
			}
			wantFail(t, reply, err, c.want, c.name)
			wantNoWrites(t, db, before, c.name)
			if len(db.codes) != 0 || len(db.grants) != 0 {
				t.Fatalf("%s：被拒的签发留下了半成品 code=%d grant=%d", c.name, len(db.codes),
					len(db.grants))
			}
		})
	}

	t.Run("白名单为空的应用一律签不出码", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		db.apps[testAppID].RedirectURIs = ""
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrRedirectURIMismatch, "空白名单")
		wantNoWrites(t, db, before, "空白名单")
	})

	t.Run("多条目白名单逐条比对", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		db.apps[testAppID].RedirectURIs = oauthRedirect + "," + oauthRedirect2
		for _, uri := range []string{oauthRedirect, oauthRedirect2} {
			if _, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
				IssueAuthorizationCode(issueReq(testAppID, testMid, uri, []string{testScopeR})); err != nil {
				t.Fatalf("命中白名单第 2 条却被拒：%v", err)
			}
		}
	})
}

// scope 只能收窄：请求集必须同时落在「应用已获批集」和「目录开放集」里。
func TestIssueAuthorizationCode_ScopeMayNotExceedGrantedOrCatalog(t *testing.T) {
	t.Run("请求应用没拿到的 scope", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeW}))
		wantFail(t, reply, err, model.ErrScopeNotGranted, "超出应用获批集")
		wantNoWrites(t, db, before, "超出应用获批集")
	})

	t.Run("审批回收后立刻签不出", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR, testScopeW)
		delete(db.granted[testAppID], testScopeW) // 运营回收写权限（关系行 state 也同步）
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect,
				[]string{testScopeR, testScopeW}))
		wantFail(t, reply, err, model.ErrScopeNotGranted, "已回收的 scope")
		wantNoWrites(t, db, before, "已回收的 scope")
	})

	t.Run("目录缺项按未知处理", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		delete(db.scopes, testScopeR)
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrScopeUnknown, "目录缺项")
		wantNoWrites(t, db, before, "目录缺项")
	})

	t.Run("目录已停用", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		db.scopes[testScopeR].Enabled = 0
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrScopeDisabled, "目录停用")
		wantNoWrites(t, db, before, "目录停用")
	})

	t.Run("写权限未在目录声明同意", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR, testScopeW)
		seedScopeFull(db, testScopeW, model.ScopeAccessWrite, model.ScopeRiskLow, 0, 1)
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeW}))
		wantFail(t, reply, err, model.ErrScopeWriteRequiresConsent, "目录未声明需同意")
		wantNoWrites(t, db, before, "目录未声明需同意")
	})

	t.Run("去重归一后只有一个快照", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		r, err := NewIssueAuthorizationCodeLogic(t.Context(), s).IssueAuthorizationCode(
			issueReq(testAppID, testMid, oauthRedirect,
				[]string{testScopeR, testScopeR, " " + testScopeR + " "}))
		r = wantOK(t, r, err, "重复 scope")
		if len(r.Scope) != 1 || r.Scope[0] != testScopeR {
			t.Fatalf("scope 未去重归一：%v", r.Scope)
		}
		// 入库快照也必须是归一后的单条：带空白或重复项的快照会让 FIND_IN_SET 定点撤销漏判。
		if got := codeRowByHash(t, db, r.Code).Scope; got != testScopeR {
			t.Fatalf("入库 scope=%q，期望归一后的 %q", got, testScopeR)
		}
	})
}

func TestIssueAuthorizationCode_RejectsEveryNonActiveApp(t *testing.T) {
	cases := []struct {
		name   string
		status int32
	}{
		{"待审", model.AppStatusPendingReview},
		{"停用", model.AppStatusSuspended},
		{"驳回", model.AppStatusRejected},
		{"下线", model.AppStatusOffline},
		{"未知状态值", 99},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s := oauthFixture(t, testScopeR)
			db.apps[testAppID].Status = c.status
			before := snapshotWrites(db)
			reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
				IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
			wantFail(t, reply, err, model.ErrApplicationNotActive, c.name)
			wantNoWrites(t, db, before, c.name)
			if len(db.codes) != 0 || len(db.grants) != 0 {
				t.Fatalf("%s：仍签出了凭证 code=%d grant=%d", c.name, len(db.codes), len(db.grants))
			}
		})
	}

	t.Run("应用不存在", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(555, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrAppNotFound, "应用不存在")
		wantNoWrites(t, db, before, "应用不存在")
	})

	t.Run("app_id 非法", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(0, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrInvalidAppID, "app_id 非法")
		wantNoWrites(t, db, before, "app_id 非法")
	})
}

func TestIssueAuthorizationCode_GatesAreSideEffectFree(t *testing.T) {
	noConsent := func(r *rpc.IssueAuthorizationCodeReq) { r.ConsentGiven = false }
	noMid := func(r *rpc.IssueAuthorizationCodeReq) { r.Mid = 0 }
	noScope := func(r *rpc.IssueAuthorizationCodeReq) { r.Scope = nil }
	overlongState := func(r *rpc.IssueAuthorizationCodeReq) {
		r.State = strings.Repeat("s", maxStateRunes+1)
	}
	cases := []struct {
		name   string
		want   error
		mutate func(*rpc.IssueAuthorizationCodeReq)
		tweak  func(t *testing.T, db *store, s *svc.ServiceContext)
	}{
		{name: "用户未同意", want: model.ErrConsentRequired, mutate: noConsent},
		{name: "mid 缺失", want: model.ErrOperatorRequired, mutate: noMid},
		{name: "空 scope", want: errScopeRequiredForIssue, mutate: noScope},
		{name: "scope 字符非法", want: model.ErrScopeUnknown,
			mutate: func(r *rpc.IssueAuthorizationCodeReq) { r.Scope = []string{"bad scope"} }},
		{name: "scope 命中未开放类目", want: model.ErrForbiddenScopeCategory,
			mutate: func(r *rpc.IssueAuthorizationCodeReq) { r.Scope = []string{"member.vip.read"} }},
		{name: "scope 条数超上限", want: model.ErrTooManyScopes,
			mutate: func(r *rpc.IssueAuthorizationCodeReq) { r.Scope = tooManyScopes() }},
		{name: "state 超列宽", want: errStateTooLong, mutate: overlongState},
		{name: "签发限频命中", want: model.ErrRateLimited,
			tweak: func(t *testing.T, db *store, s *svc.ServiceContext) {
				s.Config.OpenPlatform.AuthCodeMaxPerUserPerHour = 2
				for i := 0; i < 2; i++ {
					seedAuthCodeRow(t, db, testAppID, testMid, 1, []string{testScopeR},
						oauthRedirect, "", nowTS())
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s := oauthFixture(t, testScopeR)
			if c.tweak != nil {
				c.tweak(t, db, s)
			}
			in := issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR})
			if c.mutate != nil {
				c.mutate(in)
			}
			codes, grants := len(db.codes), len(db.grants)
			before := snapshotWrites(db)
			reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).IssueAuthorizationCode(in)
			wantFail(t, reply, err, c.want, c.name)
			wantNoWrites(t, db, before, c.name)
			// 计数与数据两条一起看：只查计数的话「写了但回滚」也会被当成通过。
			if len(db.codes) != codes || len(db.grants) != grants {
				t.Fatalf("%s：被拒的签发改变了行集 code %d→%d grant %d→%d", c.name, codes,
					len(db.codes), grants, len(db.grants))
			}
		})
	}

	// 限频读的是「近一小时的签发次数」，配置为 0 时整段跳过：线上默认 30，这里钉住两种分支。
	t.Run("限频配置为 0 时不读计数器", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		s.Config.OpenPlatform.AuthCodeMaxPerUserPerHour = 0
		seedAuthCodeRow(t, db, testAppID, testMid, 1, []string{testScopeR}, oauthRedirect, "", nowTS())
		if _, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR})); err != nil {
			t.Fatalf("限频关闭时不应被拒：%v", err)
		}
		wantCalls(t, db, "AuthCodes.CountRecentByMid", 0, 0, "限频关闭")
	})
}

func TestIssueAuthorizationCode_FailClosedOnConfigAndDependency(t *testing.T) {
	t.Run("缺 pepper 时不签无 pepper 哈希", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		s.Config.Security.CredentialPepper = ""
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "缺 pepper")
		wantNoWrites(t, db, before, "缺 pepper")
	})

	t.Run("AuthCodeTTL 未配置时拒绝签发", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		s.Config.OpenPlatform.AuthCodeTTLSeconds = 0
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, errTTLNotConfigured, "TTL 未配置")
		wantNoWrites(t, db, before, "TTL 未配置")
	})

	t.Run("写令牌耗尽发生在开事务之前", func(t *testing.T) {
		db := newStore()
		seedScopeDir(db, testScopeR, model.ScopeAccessRead)
		app := seedApp(db, testAppID, testOwner, testScopeR)
		app.RedirectURIs = oauthRedirect
		s := limitedSvc(db)
		s.Config.OpenPlatform.MaxRedirectURIBytes = oauthMaxURIBytes
		s.Config.OpenPlatform.AuthCodeMaxPerUserPerHour = oauthCodeRateLine
		before := snapshotWrites(db)
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrRateLimited, "签发被限流")
		wantNoWrites(t, db, before, "签发被限流")
	})

	t.Run("读库故障原样上抛", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		db.failOn("AppScopes.FindGrantedScopes", errFakeDown)
		before := snapshotWrites(db)
		_, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		if err == nil || !errors.Is(err, errFakeDown) {
			t.Fatalf("依赖故障必须报错而不是判成业务拒绝，实际 err=%v", err)
		}
		wantNoWrites(t, db, before, "读获批集失败")
	})
}

func TestIssueAuthorizationCode_WriteFailuresRollBackWholeTransaction(t *testing.T) {
	t.Run("授权码写入失败时 grant 不留孤儿", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		db.failOn("AuthCodes.InsertTx", errFakeDown)
		_, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		if err == nil || !errors.Is(err, errFakeDown) {
			t.Fatalf("应把写库故障上抛，实际 err=%v", err)
		}
		// 计数说「尝试过」，数据说「什么都没留下」：两条一起才排除假绿。
		wantCalls(t, db, "Grants.FindOrCreate", 0, 1, "grant 写入尝试过")
		wantCalls(t, db, "AuthCodes.InsertTx", 0, 1, "授权码写入尝试过")
		if len(db.grants) != 0 || len(db.codes) != 0 {
			t.Fatalf("事务未整体回滚：grant=%d code=%d", len(db.grants), len(db.codes))
		}
	})

	t.Run("哈希撞唯一键时整笔回滚", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		s.AuthCodes = dupHashAuthCodes{AuthCodeModel: s.AuthCodes, db: db}
		reply, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		wantFail(t, reply, err, model.ErrConcurrentUpdate, "uniq_hash 冲突")
		// 绝不允许把别人的授权码当自己签发的：既不返回明文，也不留下半成品。
		if len(db.codes) != 0 {
			t.Fatalf("冲突路径留下了授权码行：%d", len(db.codes))
		}
		if len(db.grants) != 0 {
			t.Fatalf("冲突路径留下了 grant 行：%d", len(db.grants))
		}
	})

	t.Run("grant 复用失败时不签新码", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		db.failOn("Grants.FindOrCreate", errFakeDown)
		_, err := NewIssueAuthorizationCodeLogic(t.Context(), s).
			IssueAuthorizationCode(issueReq(testAppID, testMid, oauthRedirect, []string{testScopeR}))
		if err == nil || !errors.Is(err, errFakeDown) {
			t.Fatalf("grant 写失败必须报错，实际 err=%v", err)
		}
		wantCalls(t, db, "AuthCodes.InsertTx", 0, 0, "grant 失败后不得签发授权码")
		if len(db.codes) != 0 {
			t.Fatalf("出现「有 code 无 grant」的孤儿凭证：%d", len(db.codes))
		}
	})
}

// ---------------------------------------------------------------- 换码

func TestExchangeAuthorizationCode_ReturnsTokenThatMatchesStoredTruth(t *testing.T) {
	db, s := oauthFixture(t, testScopeR, testScopeW)
	scopes := []string{testScopeR, testScopeW}
	plain, row, grant := issuedCode(t, db, s, scopes...)
	before := nowTS()
	// 基线从「换码之前」起算：issuedCode 自己就走过一次事务，绝对计数不是契约。
	txAt, usedAt := db.count("DB.TransactCtx"), db.count("AuthCodes.MarkUsed")

	reply, err := exchange(s, testAppID, plain, oauthRedirect)
	reply = wantOK(t, reply, err, "正常换码")
	tk := reply.Token
	if tk == nil {
		t.Fatal("换码成功却不回 token")
	}

	tok := db.tokens[tk.TokenId]
	if tok == nil {
		t.Fatalf("响应的 token_id=%d 不在库里", tk.TokenId)
	}
	// 响应 ↔ 入库真值。
	if tok.AppID != testAppID || tok.Mid != testMid || tok.GrantID != grant.GrantID {
		t.Fatalf("token 归属错：app=%d mid=%d grant=%d", tok.AppID, tok.Mid, tok.GrantID)
	}
	if tk.GrantId != grant.GrantID {
		t.Fatalf("响应 grant_id=%d，与库里 %d 不一致", tk.GrantId, grant.GrantID)
	}
	if tok.State != model.TokenStateActive {
		t.Fatalf("新签发 token 状态=%d，期望 ACTIVE", tok.State)
	}
	if tok.GrantType != model.GrantTypeAuthorizationCode {
		t.Fatalf("grant_type=%d，期望授权码兑换", tok.GrantType)
	}
	// 换码是「新链」：parent_token_id=0，否则两次独立授权会被串成一条轮换链。
	if tok.ParentTokenID != 0 {
		t.Fatalf("parent_token_id=%d，期望 0", tok.ParentTokenID)
	}
	// scope 只能从码快照继承，一处都不由响应重新组装。
	if tok.Scope != row.Scope {
		t.Fatalf("token 快照 %q 与码快照 %q 漂移（放大权限的入口）", tok.Scope, row.Scope)
	}
	// 逐条对齐（不只比拼接串）：期望集合独立写出，不用被测代码的函数造期望。
	wantScopeList(t, tk.Scope, []string{testScopeW, testScopeR}, "换码响应 scope")
	// 明文只出一次：库里只有 HMAC(pepper, 明文)。
	if tok.AccessHash != mustHash(t, tk.AccessToken) || tok.RefreshHash != mustHash(t, tk.RefreshToken) {
		t.Fatal("入库哈希与 HMAC(pepper, 明文) 口径不符，下次按值定位必然失败")
	}
	if tok.AccessSalt == "" || tok.RefreshSalt == "" {
		t.Fatalf("salt 列不完整：%q/%q", tok.AccessSalt, tok.RefreshSalt)
	}
	mustNoPlaintextStored(t, db, "换码", plain, tk.AccessToken, tk.RefreshToken)

	// 时效来自配置，且与库里时间戳自洽。
	wantWindow(t, tok.AccessExpiresAt, before+s.Config.OpenPlatform.AccessTokenTTLSeconds, 2,
		"access_expires_at")
	wantWindow(t, tok.RefreshExpiresAt, before+s.Config.OpenPlatform.RefreshTokenTTLSeconds, 2,
		"refresh_expires_at")
	if tk.ExpiresIn != tok.AccessExpiresAt-tok.Ctime || tk.RefreshExpiresIn != tok.RefreshExpiresAt-tok.Ctime {
		t.Fatalf("ExpiresIn=%d/%d 与库里的剩余时长不自洽", tk.ExpiresIn, tk.RefreshExpiresIn)
	}
	if tk.TokenType != "Bearer" {
		t.Fatalf("token_type=%q，契约固定 Bearer", tk.TokenType)
	}
	wantWindow(t, tk.IssuedAt, before, 2, "issued_at")

	// 消费位点：一次性消费的物理证据。
	used := codeRowByHash(t, db, plain)
	if used.UsedAt == 0 || used.ConsumedByTokenID != tok.TokenID {
		t.Fatalf("码未被消费：used_at=%d consumed_by=%d", used.UsedAt, used.ConsumedByTokenID)
	}
	if used.ReplayCount != 0 {
		t.Fatalf("首次兑换就被记重放：%d", used.ReplayCount)
	}
	// 链头与审计位点：撤销与轮换都从 current_token_id 出发，回填漏了就会打死新凭证。
	g := db.grants[grant.GrantID]
	if g.CurrentTokenID != tok.TokenID {
		t.Fatalf("链头=%d，期望新 token %d", g.CurrentTokenID, tok.TokenID)
	}
	if g.LastCodeID != used.CodeID {
		t.Fatalf("grant.last_code_id=%d，期望 %d（审计链 grant←code←token）", g.LastCodeID, used.CodeID)
	}
	if g.RotateSeq != 1 {
		t.Fatalf("rotate_seq=%d，期望 1", g.RotateSeq)
	}
	// 一次事务包住「签发 token + 消费码 + 前移链头」：三句必须同生共死。
	wantCalls(t, db, "DB.TransactCtx", txAt, 1, "换码走一个事务")
	wantCalls(t, db, "AuthCodes.MarkUsed", usedAt, 1, "消费一次")
	if len(db.tokens) != 1 {
		t.Fatalf("token 行数=%d，期望 1", len(db.tokens))
	}
}

// 换码期的 redirect_uri 必须与授权时逐字一致；不一致只拒绝，绝不消耗 code。
func TestExchangeAuthorizationCode_RedirectMustEqualAuthorizationTimeValue(t *testing.T) {
	cases := []struct {
		name     string
		redirect string
	}{
		{"换成白名单里的另一条", oauthRedirect2},
		{"追加路径段", oauthRedirect + "/x"},
		{"夹带查询串", oauthRedirect + "?code=steal"},
		{"降级为 http", "http://app.example.test/cb"},
		{"缺失回调", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s := oauthFixture(t, testScopeR)
			db.apps[testAppID].RedirectURIs = oauthRedirect + "," + oauthRedirect2
			plain, _, _ := issuedCode(t, db, s, testScopeR)
			before := snapshotWrites(db)

			reply, err := exchange(s, testAppID, plain, c.redirect)
			wantFail(t, reply, err, model.ErrRedirectURIMismatch, c.name)
			wantNoWrites(t, db, before, c.name)
			// 码没被消耗：否则「猜一次回调地址」就能把用户的授权打死（DoS），
			// 而且用户重试时拿不到第二次机会。
			row := codeRowByHash(t, db, plain)
			if row.UsedAt != 0 || row.ReplayCount != 0 {
				t.Fatalf("回调不一致却消耗了码：used_at=%d replay=%d", row.UsedAt, row.ReplayCount)
			}
			if len(db.tokens) != 0 {
				t.Fatal("回调不一致却签出了 token")
			}
			// 用户带着正确回调再来一次必须还能成功：码仍是活的。
			if _, err := exchange(s, testAppID, plain, oauthRedirect); err != nil {
				t.Fatalf("误拒后正当换码也不通了（码被消耗）：%v", err)
			}
		})
	}
}

func TestExchangeAuthorizationCode_CodeIsSingleUse(t *testing.T) {
	db, s := oauthFixture(t, testScopeR)
	plain, _, grant := issuedCode(t, db, s, testScopeR)
	first, err := exchange(s, testAppID, plain, oauthRedirect)
	first = wantOK(t, first, err, "首次换码")
	consumedAt := codeRowByHash(t, db, plain).UsedAt

	t.Run("重放必须失败且留下告警证据", func(t *testing.T) {
		before := snapshotWrites(db)
		reply, err := exchange(s, testAppID, plain, oauthRedirect)
		wantFail(t, reply, err, model.ErrAuthCodeUsed, "第二次换码")
		// 唯一被允许的写是重放计数（错误路径也要留痕）。
		wantCalls(t, db, "AuthCodes.IncrReplay", before["AuthCodes.IncrReplay"], 1, "重放计数")
		wantCalls(t, db, "AuthCodes.MarkUsed", before["AuthCodes.MarkUsed"], 0, "重放不再消费")
		wantCalls(t, db, "DB.TransactCtx", before["DB.TransactCtx"], 0, "重放开不了事务")
		if n := len(db.tokens); n != 1 {
			t.Fatalf("重放换出了第二代 token：行数=%d", n)
		}
		second := codeRowByHash(t, db, plain)
		if second.ReplayCount != 1 {
			t.Fatalf("replay_count=%d，期望 1（泄露告警的输入）", second.ReplayCount)
		}
		// 首次消费的位点不得被重放覆盖：否则审计链 grant←code←token 断掉。
		if second.UsedAt != consumedAt || second.ConsumedByTokenID != first.Token.TokenId {
			t.Fatalf("重放改写了首次消费位点：used_at=%d consumed_by=%d", second.UsedAt,
				second.ConsumedByTokenID)
		}
		if db.grants[grant.GrantID].CurrentTokenID != first.Token.TokenId {
			t.Fatal("重放前移了链头")
		}
		// 首次签出的凭证依旧可用：重放只做减法，不得牵连既有 token。
		got, err := introspect(s, first.Token.AccessToken, 0, "")
		if err != nil {
			t.Fatalf("回读首次凭证失败：%v", err)
		}
		if !got.Active {
			t.Fatalf("重放把首次签发的 token 也打死了：%s", got.DenyReason)
		}
	})

	t.Run("重放计数写失败不得放行", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		plain, _, _ := issuedCode(t, db, s, testScopeR)
		if _, err := exchange(s, testAppID, plain, oauthRedirect); err != nil {
			t.Fatalf("首次换码应成功：%v", err)
		}
		db.failOn("AuthCodes.IncrReplay", errFakeDown)
		reply, err := exchange(s, testAppID, plain, oauthRedirect)
		// 计数失败只是少一条审计，不能作为「放行」的理由：结论仍是已使用。
		wantFail(t, reply, err, model.ErrAuthCodeUsed, "重放计数失败")
	})
}

func TestExchangeAuthorizationCode_ExpiredCodeRejected(t *testing.T) {
	db, s := oauthFixture(t, testScopeR)
	plain, row, _ := issuedCode(t, db, s, testScopeR)
	// TTL 到点：直接把 expires_at 推到过去（60s 的 TTL 不能靠 sleep 测），
	// 断言的是「过期判定读的是库里那个时间戳」。
	db.codes[row.CodeID].ExpiresAt = nowTS() - 1
	before := snapshotWrites(db)

	reply, err := exchange(s, testAppID, plain, oauthRedirect)
	wantFail(t, reply, err, model.ErrAuthCodeExpired, "过期码")
	wantNoWrites(t, db, before, "过期码")
	if got := codeRowByHash(t, db, plain); got.UsedAt != 0 {
		t.Fatal("过期的码被判成已消费：同一枚码再也换不出东西，等于拒绝服务")
	}
	if len(db.tokens) != 0 {
		t.Fatal("过期码仍签出了 token")
	}
}

// 并发兑换同一枚码：库侧 CAS 是唯一锚点，后到者必须整体回滚。
func TestExchangeAuthorizationCode_ConcurrentExchangeOnlyOneSideWins(t *testing.T) {
	db, s := oauthFixture(t, testScopeR)
	plain, row, grant := issuedCode(t, db, s, testScopeR)
	const racerTokenID = int64(555001)
	// 交错点：logic 已读到「未消费」并签好新 token，正在写 used_at 的瞬间，
	// 并发对手的事务先提交了（同一枚码 → 它的 token，链头也被它前移）。
	db.onHit("AuthCodes.MarkUsed", func() {
		c := db.codes[row.CodeID]
		c.UsedAt = nowTS()
		c.ConsumedByTokenID = racerTokenID
		db.grants[grant.GrantID].CurrentTokenID = racerTokenID
	})

	reply, err := exchange(s, testAppID, plain, oauthRedirect)
	wantFail(t, reply, err, model.ErrAuthCodeUsed, "并发败者")
	// 计数说「尝试过」，数据说「什么都没留下」：两条一起才排除假绿。
	wantCalls(t, db, "Tokens.InsertTx", 0, 1, "败者确实试过签发")
	wantCalls(t, db, "Grants.TouchTokenHead", 0, 0, "CAS 失败后不再前移链头")
	wantCalls(t, db, "Grants.TouchLastCode", 0, 0, "CAS 失败后不再回填审计位点")
	if len(db.tokens) != 0 {
		t.Fatalf("败者的 token 未被回滚，同一枚码换出了两组凭证：%d", len(db.tokens))
	}
	// 败者既没消耗码也没前移链头（对手写的那一行随快照一起回滚，是内存 fake 的已知边界：
	// 真库里由 used_at CAS 与事务隔离保证，这里能证明的是「败者绝不产生第二组凭证」）。
	if g := db.grants[grant.GrantID]; g.CurrentTokenID != 0 || g.LastCodeID != 0 {
		t.Fatalf("败者改动了 grant：current=%d last_code=%d", g.CurrentTokenID, g.LastCodeID)
	}
}

func TestExchangeAuthorizationCode_RevalidatesAppStateAndScopeAtExchangeTime(t *testing.T) {
	// 签发到兑换之间可能发生停用、回收、撤销：这一段必须在换码时重判，
	// 而且三类拒绝都不能消耗码（否则被拒的兑换顺手把用户的授权打死）。
	cases := []struct {
		name  string
		want  error
		tweak func(t *testing.T, db *store, codeID, grantID int64)
	}{
		{"应用签发后被停用", model.ErrApplicationNotActive, func(_ *testing.T, db *store, _ int64, _ int64) {
			db.apps[testAppID].Status = model.AppStatusSuspended
		}},
		{"应用签发后被下线", model.ErrApplicationNotActive, func(_ *testing.T, db *store, _ int64, _ int64) {
			db.apps[testAppID].Status = model.AppStatusOffline
		}},
		{"审批签发后回收 scope", model.ErrScopeNotGranted, func(_ *testing.T, db *store, _ int64, _ int64) {
			delete(db.granted[testAppID], testScopeR)
		}},
		{"码指向不存在的 grant", model.ErrGrantRevoked, func(_ *testing.T, db *store, _ int64, grantID int64) {
			delete(db.grants, grantID) // 数据不一致按已撤销处理，不放行
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s := oauthFixture(t, testScopeR)
			plain, row, grant := issuedCode(t, db, s, testScopeR)
			c.tweak(t, db, row.CodeID, grant.GrantID)
			before := snapshotWrites(db)
			reply, err := exchange(s, testAppID, plain, oauthRedirect)
			wantFail(t, reply, err, c.want, c.name)
			wantNoWrites(t, db, before, c.name)
			if got := codeRowByHash(t, db, plain); got.UsedAt != 0 {
				t.Fatalf("%s：仍消耗了码", c.name)
			}
			if len(db.tokens) != 0 {
				t.Fatalf("%s：仍签出了 token", c.name)
			}
		})
	}

	// 「签发后撤销 → 换码被拒」必须用真撤销路径构造，否则测的不是位点比对本身。
	t.Run("用户签发后撤销授权", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		plain, _, grant := issuedCode(t, db, s, testScopeR)
		if _, err := NewRevokeAuthorizationLogic(t.Context(), s).RevokeAuthorization(
			&rpc.RevokeAuthorizationReq{
				Target: rpc.RevokeTarget_REVOKE_TARGET_GRANT, AppId: testAppID,
				Mid: testMid, OperatorMid: testMid,
			}); err != nil {
			t.Fatalf("撤销应成功：%v", err)
		}
		if db.grants[grant.GrantID].RevokedAt == 0 {
			t.Fatal("前提不成立：撤销位点没写下，本例就没在测位点比对")
		}
		before := snapshotWrites(db)
		reply, err := exchange(s, testAppID, plain, oauthRedirect)
		wantFail(t, reply, err, model.ErrGrantRevoked, "签发后撤销")
		wantNoWrites(t, db, before, "签发后撤销")
		if got := codeRowByHash(t, db, plain); got.UsedAt != 0 {
			t.Fatal("撤销后换码仍消耗了码")
		}
	})

	t.Run("跨应用兑换与不存在的码回同一哨兵", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		plain, _, _ := issuedCode(t, db, s, testScopeR)
		_, byOtherApp := exchange(s, testApp2, plain, oauthRedirect)
		_, unknown := exchange(s, testAppID, mustPlaintext(t), oauthRedirect)
		if byOtherApp == nil || unknown == nil || byOtherApp.Error() != unknown.Error() {
			t.Fatalf("两种失败可区分（%v / %v）：等于一个授权码存在性探测接口", byOtherApp, unknown)
		}
		if !errors.Is(byOtherApp, model.ErrAuthCodeInvalid) {
			t.Fatalf("跨应用兑换应报 ErrAuthCodeInvalid，实际 %v", byOtherApp)
		}
		if got := codeRowByHash(t, db, plain); got.UsedAt != 0 {
			t.Fatal("他人应用的兑换消耗了码")
		}
	})
}

func TestExchangeAuthorizationCode_GatesAndFailClosed(t *testing.T) {
	t.Run("空码与超长码", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		before := snapshotWrites(db)
		reply, err := exchange(s, testAppID, "", oauthRedirect)
		wantFail(t, reply, err, model.ErrAuthCodeInvalid, "空码")
		reply, err = exchange(s, testAppID, strings.Repeat("a", credentialBytes*2+2), oauthRedirect)
		wantFail(t, reply, err, model.ErrAuthCodeInvalid, "超长码")
		wantNoWrites(t, db, before, "码形状非法")
		// 形状门禁发生在算哈希之前：一条读都不该发生。
		wantCalls(t, db, "AuthCodes.FindByHash", 0, 0, "形状非法不查库")
	})

	t.Run("缺 pepper 时不做明文比对", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		plain, _, _ := issuedCode(t, db, s, testScopeR)
		s.Config.Security.CredentialPepper = ""
		before := snapshotWrites(db)
		reply, err := exchange(s, testAppID, plain, oauthRedirect)
		wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "缺 pepper")
		wantNoWrites(t, db, before, "缺 pepper")
		wantCalls(t, db, "AuthCodes.FindByHash", 0, 0, "算不出哈希就不查库")
	})

	t.Run("读库故障原样上抛", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		plain, _, _ := issuedCode(t, db, s, testScopeR)
		db.failOn("AuthCodes.FindByHash", errFakeDown)
		_, err := exchange(s, testAppID, plain, oauthRedirect)
		if err == nil || !errors.Is(err, errFakeDown) {
			t.Fatalf("依赖故障必须报错而不是判成码无效，实际 %v", err)
		}
	})

	t.Run("写令牌耗尽发生在一句写之前", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		plain, _, _ := issuedCode(t, db, s, testScopeR)
		drainWriteBucket(s) // 零容量令牌桶：限流必须发生在任何写之前
		before := snapshotWrites(db)
		reply, err := exchange(s, testAppID, plain, oauthRedirect)
		wantFail(t, reply, err, model.ErrRateLimited, "换码被限流")
		wantNoWrites(t, db, before, "换码被限流")
		// 限流是可重试错误：码必须还在。
		if got := codeRowByHash(t, db, plain); got.UsedAt != 0 {
			t.Fatal("被限流的换码消耗了码，用户重试必然失败")
		}
	})
}

func TestExchangeAuthorizationCode_WriteFailuresRollBackWholeTransaction(t *testing.T) {
	// 三条写语句任一失败，都必须回到「码未消费、无 token、链头未动」：
	// 否则用户手上那枚码作废却又换不出凭证（不可重试的失败），或换出两组凭证。
	for _, op := range []string{"Tokens.InsertTx", "AuthCodes.MarkUsed", "Grants.TouchTokenHead",
		"Grants.TouchLastCode"} {
		t.Run(op+" 失败", func(t *testing.T) {
			db, s := oauthFixture(t, testScopeR)
			plain, _, grant := issuedCode(t, db, s, testScopeR)
			db.failOn(op, errFakeDown)
			_, err := exchange(s, testAppID, plain, oauthRedirect)
			if err == nil || !errors.Is(err, errFakeDown) {
				t.Fatalf("%s 的故障必须上抛，实际 err=%v", op, err)
			}
			if len(db.tokens) != 0 {
				t.Fatalf("%s 失败后仍留下 token 行：%d", op, len(db.tokens))
			}
			if got := codeRowByHash(t, db, plain); got.UsedAt != 0 || got.ConsumedByTokenID != 0 {
				t.Fatalf("%s 失败后码仍被消费：used_at=%d consumed_by=%d", op, got.UsedAt,
					got.ConsumedByTokenID)
			}
			g := db.grants[grant.GrantID]
			if g.CurrentTokenID != 0 || g.LastCodeID != 0 || g.RotateSeq != 0 {
				t.Fatalf("%s 失败后 grant 被前移：%+v", op, g)
			}
		})
	}
}

// ---------------------------------------------------------------- 校验

// oauthTokenScopes 校验/撤销/授权判定共用的凭证前提：一条含读写两个权限点、用户已同意的授权。
var oauthTokenScopes = []string{testScopeR, testScopeW}

// oauthSortedScopes 期望的 scope 顺序：model.JoinScopes 归一后升序，
// 所以期望集合手工写成「publish 在 read 前」，不用被测函数生成期望。
var oauthSortedScopes = []string{testScopeW, testScopeR}

func TestIntrospectToken_ActiveReplyMatchesStoredTruth(t *testing.T) {
	db, s, grant, access, _, tok := oauthTokenFixture(t, oauthTokenScopes, 1)

	reply, err := introspect(s, access, 0, "")
	reply = wantOK(t, reply, err, "按 access 明文校验")
	if !reply.Active {
		t.Fatalf("可用凭证被判失败：deny=%q", reply.DenyReason)
	}
	// 响应字段 == 入库真值（不是「err==nil」就算过）。
	if reply.AppId != tok.AppID || reply.Mid != tok.Mid || reply.TokenId != tok.TokenID ||
		reply.GrantId != tok.GrantID {
		t.Fatalf("响应标识 app=%d mid=%d token=%d grant=%d，库里是 app=%d mid=%d token=%d grant=%d",
			reply.AppId, reply.Mid, reply.TokenId, reply.GrantId,
			tok.AppID, tok.Mid, tok.TokenID, tok.GrantID)
	}
	if reply.GrantId != grant.GrantID {
		t.Fatalf("响应的 grant_id=%d 与凭证所属授权 %d 不一致（撤销位点会比对到另一行）",
			reply.GrantId, grant.GrantID)
	}
	if reply.ExpiresAt != tok.AccessExpiresAt {
		t.Fatalf("expires_at=%d，库里 access_expires_at=%d", reply.ExpiresAt, tok.AccessExpiresAt)
	}
	wantScopeList(t, reply.Scope, oauthSortedScopes, "响应 scope")
	// 判定读的是行里的 scope 快照，而不是 grant 或目录：放大权限的入口就在这里。
	if strings.Join(reply.Scope, ",") != tok.Scope {
		t.Fatalf("响应 scope %v 与快照 %q 不一致", reply.Scope, tok.Scope)
	}
	mustNoPlaintextStored(t, db, "校验", access)

	// 通过时唯一的写是「使用时间」（观测面），且确实落到了行上。
	wantCalls(t, db, "Tokens.TouchUsed", 0, 1, "使用时间回写一次")
	wantWindow(t, db.tokens[tok.TokenID].LastUsedAt, nowTS(), 2, "last_used_at")
	if len(db.tokens) != 1 {
		t.Fatalf("校验凭空多出一代凭证：token 行数=%d", len(db.tokens))
	}
}

func TestIntrospectToken_LocatesByEitherHandleWithPlaintextAhead(t *testing.T) {
	t.Run("只给 token_id 也能判定", func(t *testing.T) {
		_, s, _, _, _, tok := oauthTokenFixture(t, oauthTokenScopes, 1)
		reply, err := introspect(s, "", tok.TokenID, "")
		reply = wantOK(t, reply, err, "按 token_id 校验")
		if !reply.Active || reply.TokenId != tok.TokenID || reply.ExpiresAt != tok.AccessExpiresAt {
			t.Fatalf("按主键定位的结论不对：%+v", reply)
		}
		wantScopeList(t, reply.Scope, oauthSortedScopes, "响应 scope")
	})

	t.Run("明文与 token_id 同时给出时以明文为准", func(t *testing.T) {
		_, s, _, access, _, tok := oauthTokenFixture(t, oauthTokenScopes, 1)
		// token_id 是调用方可能缓存的旧值（甚至别人猜得到的自增数），
		// 明文才是「当场持有」的证据，因此它必须赢。
		reply, err := introspect(s, access, 555000, "")
		reply = wantOK(t, reply, err, "两个定位依据冲突")
		if !reply.Active || reply.TokenId != tok.TokenID {
			t.Fatalf("未按明文定位，结果=%+v", reply)
		}
	})

	t.Run("陌生明文配有效 token_id 必须判失败", func(t *testing.T) {
		db, s, _, _, _, tok := oauthTokenFixture(t, oauthTokenScopes, 1)
		before := snapshotWrites(db)
		reply, err := introspect(s, mustPlaintext(t), tok.TokenID, "")
		if err != nil {
			t.Fatalf("业务不通过不该回 gRPC 错误：%v", err)
		}
		if reply.Active || reply.DenyReason != denyInactive {
			t.Fatalf("结果=%+v，期望 active=false deny=%q", reply, denyInactive)
		}
		// 关键：不能因为 token_id 那行还在就放行——否则主键就成了绕过凭据的通道。
		if reply.TokenId != 0 || reply.AppId != 0 {
			t.Fatalf("凭 token_id 猜出来的标识被回显了：%+v", reply)
		}
		wantNoWrites(t, db, before, "明文不匹配")
	})

	t.Run("两个定位依据都不给是参数错", func(t *testing.T) {
		db, s := oauthFixture(t, testScopeR)
		before := snapshotWrites(db)
		for _, id := range []int64{0, -1} {
			reply, err := introspect(s, "", id, "")
			wantFail(t, reply, err, model.ErrTokenInvalid, "缺少定位依据")
		}
		wantNoWrites(t, db, before, "参数错")
		// 参数门禁在读库之前：不给出任何可被枚举的分支。
		wantCalls(t, db, "Tokens.FindByAccessHash", 0, 0, "参数错不查库")
		wantCalls(t, db, "Tokens.FindByID", 0, 0, "参数错不查库")
	})
}

func TestIntrospectToken_UsageStampIsRateLimitedAndCannotChangeVerdict(t *testing.T) {
	db, s, _, access, _, tok := oauthTokenFixture(t, []string{testScopeR}, 1)
	if _, err := introspect(s, access, 0, ""); err != nil {
		t.Fatalf("首次校验：%v", err)
	}
	wantCalls(t, db, "Tokens.TouchUsed", 0, 1, "首次回写")

	t.Run("60s 内重复校验不再写", func(t *testing.T) {
		before := snapshotWrites(db)
		got, err := introspect(s, access, 0, "")
		got = wantOK(t, got, err, "同窗口重复校验")
		if !got.Active {
			t.Fatalf("重复校验被判失败：%q", got.DenyReason)
		}
		// 校验是每请求热点路径：不限频的话观测写会盖过真实负载。
		wantNoWrites(t, db, before, "重复校验")
	})

	t.Run("距上次满 60s 才再写一次", func(t *testing.T) {
		db.tokens[tok.TokenID].LastUsedAt = nowTS() - touchUsedMinInterval
		before := snapshotWrites(db)
		if _, err := introspect(s, access, 0, ""); err != nil {
			t.Fatalf("校验：%v", err)
		}
		wantCalls(t, db, "Tokens.TouchUsed", before["Tokens.TouchUsed"], 1, "到点再写")
	})

	t.Run("观测写失败只降级不影响结论", func(t *testing.T) {
		db.tokens[tok.TokenID].LastUsedAt = 0
		db.failOn("Tokens.TouchUsed", errFakeDown)
		got, err := introspect(s, access, 0, "")
		got = wantOK(t, got, err, "TouchUsed 故障")
		if !got.Active || got.TokenId != tok.TokenID {
			t.Fatalf("观测写故障改写了结论：%+v", got)
		}
	})
}

// oauthDenyCase 一条「凭证应当不可用」的构造。同一份表既喂 IntrospectToken 也喂
// AuthorizeRequest：两者共用 tokencheck.evaluateAccessToken，对同一库状态必须给出
// 同一个原因码。用一张表跑两个入口，是防漂移最便宜的手段（各写一套判定时必然对不上）。
type oauthDenyCase struct {
	name  string
	scope string // required_scope（空 = 纯 introspect）
	deny  string
	// apply 改的是库里的真行（不是 logic 拿到的副本），构造出
	// 「真撤销/真停用/真过期之后库会长什么样」。返回实际使用的 svc（换主密钥那例会换一个）。
	apply func(t *testing.T, db *store, s *svc.ServiceContext, tok *model.Token,
		grant *model.Grant) *svc.ServiceContext
}

func sameSvc(s *svc.ServiceContext) *svc.ServiceContext { return s }

func oauthDenyCases() []oauthDenyCase {
	return []oauthDenyCase{
		{"行状态=已撤销", "", denyRevoked, func(_ *testing.T, _ *store, s *svc.ServiceContext,
			tok *model.Token, _ *model.Grant) *svc.ServiceContext {
			tok.State = model.TokenStateRevoked
			return sameSvc(s)
		}},
		{"行状态=已轮换（旧代凭证）", "", denyRevoked, func(_ *testing.T, _ *store, s *svc.ServiceContext,
			tok *model.Token, _ *model.Grant) *svc.ServiceContext {
			// 轮换后旧 access 必须立即不可用，否则「已换新」与「旧的还能打」同时成立。
			tok.State = model.TokenStateRotated
			return sameSvc(s)
		}},
		{"行状态=归档为已过期", "", denyExpired, func(_ *testing.T, _ *store, s *svc.ServiceContext,
			tok *model.Token, _ *model.Grant) *svc.ServiceContext {
			tok.State = model.TokenStateExpired
			return sameSvc(s)
		}},
		{"状态仍 ACTIVE 但时间戳已过", "", denyExpired, func(_ *testing.T, _ *store, s *svc.ServiceContext,
			tok *model.Token, _ *model.Grant) *svc.ServiceContext {
			// 过期判定读时间戳而不是状态：归档任务停摆也不能放宽有效期。
			tok.AccessExpiresAt = nowTS() - 1
			return sameSvc(s)
		}},
		{"未知的行状态值", "", denyInactive, func(_ *testing.T, _ *store, s *svc.ServiceContext,
			tok *model.Token, _ *model.Grant) *svc.ServiceContext {
			// 将来新增状态枚举时，判定链默认分支必须保守拒绝而不是放行。
			tok.State = 99
			return sameSvc(s)
		}},
		{"grant 撤销位点晚于签发", "", denyRevoked, func(_ *testing.T, db *store,
			s *svc.ServiceContext, _ *model.Token, grant *model.Grant) *svc.ServiceContext {
			// 双保险的第二道：行状态没动，只前移位点。
			db.grants[grant.GrantID].RevokedAt = nowTS()
			return sameSvc(s)
		}},
		{"token 指向不存在的 grant", "", denyRevoked, func(_ *testing.T, db *store,
			s *svc.ServiceContext, _ *model.Token, grant *model.Grant) *svc.ServiceContext {
			// 数据不一致按「已撤销」处理：拿不到授权归属就谈不上放行。
			delete(db.grants, grant.GrantID)
			return sameSvc(s)
		}},
		{"应用被停用", "", denyAppSuspended, func(_ *testing.T, db *store, s *svc.ServiceContext,
			_ *model.Token, _ *model.Grant) *svc.ServiceContext {
			db.apps[testAppID].Status = model.AppStatusSuspended
			return sameSvc(s)
		}},
		{"应用被下线", "", denyAppSuspended, func(_ *testing.T, db *store, s *svc.ServiceContext,
			_ *model.Token, _ *model.Grant) *svc.ServiceContext {
			db.apps[testAppID].Status = model.AppStatusOffline
			return sameSvc(s)
		}},
		{"应用未审批通过", "", denyAppSuspended, func(_ *testing.T, db *store, s *svc.ServiceContext,
			_ *model.Token, _ *model.Grant) *svc.ServiceContext {
			db.apps[testAppID].Status = model.AppStatusPendingReview
			return sameSvc(s)
		}},
		{"应用行不存在", "", denyInactive, func(_ *testing.T, db *store, s *svc.ServiceContext,
			_ *model.Token, _ *model.Grant) *svc.ServiceContext {
			delete(db.apps, testAppID)
			return sameSvc(s)
		}},
		{"凭证定位不到（库里没有这行）", "", denyInactive, func(_ *testing.T, db *store,
			s *svc.ServiceContext, tok *model.Token, _ *model.Grant) *svc.ServiceContext {
			delete(db.tokens, tok.TokenID)
			return sameSvc(s)
		}},
		{"要求快照里没有的权限点", "video.delete", denyScopeMissing, func(_ *testing.T,
			_ *store, s *svc.ServiceContext, _ *model.Token, _ *model.Grant) *svc.ServiceContext {
			return sameSvc(s)
		}},
		{"权限点在快照但审批已回收", testScopeR, denyScopeMissing, func(_ *testing.T, db *store,
			s *svc.ServiceContext, _ *model.Token, _ *model.Grant) *svc.ServiceContext {
			// 收回授权后此前签发的 token 也必须失效：判定读的是「当前获批集」。
			delete(db.granted[testAppID], testScopeR)
			return sameSvc(s)
		}},
		{"写权限点但用户未同意", testScopeW, denyConsentMissing, func(_ *testing.T, db *store,
			s *svc.ServiceContext, _ *model.Token, grant *model.Grant) *svc.ServiceContext {
			db.grants[grant.GrantID].ConsentGiven = 0
			return sameSvc(s)
		}},
		{"主密钥轮换后同一明文定位不到", "", denyInactive, func(_ *testing.T, db *store,
			_ *svc.ServiceContext, _ *model.Token, _ *model.Grant) *svc.ServiceContext {
			// 换 pepper 等价于「所有已签发凭证即刻作废」：哈希口径变了，按值定位必然落空。
			rotated := oauthSvc(db)
			rotated.Config.Security.CredentialPepper = "rotated-" + testPepper
			return rotated
		}},
	}
}

func TestIntrospectToken_DeniesWithFixedReasonAndLeaksNoIdentifier(t *testing.T) {
	for _, c := range oauthDenyCases() {
		t.Run(c.name, func(t *testing.T) {
			db, s, grant, access, _, tok := oauthTokenFixture(t, oauthTokenScopes, 1)
			s2 := c.apply(t, db, s, tok, grant)
			before := snapshotWrites(db)

			reply, err := introspect(s2, access, 0, c.scope)
			if err != nil {
				t.Fatalf("业务不通过必须是 active=false 而不是 gRPC 错误：%v", err)
			}
			if reply.Active {
				t.Fatal("应判失败却放行了")
			}
			if reply.DenyReason != c.deny {
				t.Fatalf("deny_reason=%q，期望 %q", reply.DenyReason, c.deny)
			}
			// 「存在但无效」的凭证自带一条内部标识映射（谁的哪次授权）。
			// 把它回给任意持票人是净增的泄露面，因此拒绝响应只回结论与原因码。
			if reply.AppId != 0 || reply.Mid != 0 || reply.GrantId != 0 || reply.TokenId != 0 ||
				reply.ExpiresAt != 0 || len(reply.Scope) != 0 {
				t.Fatalf("拒绝响应泄露了标识：%+v", reply)
			}
			wantNoWrites(t, db, before, c.name)
			wantCalls(t, db, "Tokens.TouchUsed", before["Tokens.TouchUsed"], 0, "拒绝不做观测写")
		})
	}
}

func TestIntrospectToken_RequiredScopeJudgedAgainstCurrentTruth(t *testing.T) {
	t.Run("同意位点只管写权限点", func(t *testing.T) {
		db, s, grant, access, _, _ := oauthTokenFixture(t, oauthTokenScopes, 1)
		for _, sc := range oauthSortedScopes {
			got, err := introspect(s, access, 0, sc)
			got = wantOK(t, got, err, "已同意时要求 "+sc)
			if !got.Active {
				t.Fatalf("有同意位点却拒绝 %s：%+v", sc, got)
			}
		}
		// 抹掉同意位点：读权限点必须仍然可用（否则同意门禁会打死只读接入），
		// 写权限点必须立刻不可用。
		db.grants[grant.GrantID].ConsentGiven = 0
		got, err := introspect(s, access, 0, testScopeR)
		got = wantOK(t, got, err, "无同意时的读权限点")
		if !got.Active {
			t.Fatalf("读权限点被同意门禁打死：%+v", got)
		}
		got, err = introspect(s, access, 0, testScopeW)
		got = wantOK(t, got, err, "无同意时的写权限点")
		if got.Active || got.DenyReason != denyConsentMissing {
			t.Fatalf("写权限点缺同意却放行：%+v", got)
		}
	})

	t.Run("同意门禁读的是目录当前定义", func(t *testing.T) {
		db, s, _, access, _, _ := oauthTokenFixture(t, []string{testScopeW}, 0)
		got, err := introspect(s, access, 0, testScopeW)
		got = wantOK(t, got, err, "目录为写权限且未同意")
		if got.Active || got.DenyReason != denyConsentMissing {
			t.Fatalf("未同意的写权限被放行：%+v", got)
		}
		// 目录把该权限点降级为读：同意门禁随之不再适用（判定读的是当前定义，
		// 不是签发时快照，也不是「以前算过同意」）。
		db.scopes[testScopeW].Access = model.ScopeAccessRead
		got, err = introspect(s, access, 0, testScopeW)
		got = wantOK(t, got, err, "目录降级为读权限")
		if !got.Active {
			t.Fatalf("目录降级后仍要求同意：%+v", got)
		}
	})

	t.Run("api_code 不参与 scope 判定", func(t *testing.T) {
		db, s, _, access, _, _ := oauthTokenFixture(t, oauthTokenScopes, 1)
		before := snapshotWrites(db)
		got, err := NewIntrospectTokenLogic(context.Background(), s).IntrospectToken(
			&rpc.IntrospectTokenReq{AccessToken: access, ApiCode: testAPIC})
		got = wantOK(t, got, err, "带 api_code")
		if !got.Active {
			t.Fatalf("带 api_code 改变了结论：%+v", got)
		}
		// 仓库没有「接口 → 权限点」目录，服务端绝不猜一个权限点来判：
		// 因此带 api_code 既不会读配额，也不会做 scope 判定。
		// 成功路径唯一允许的写是 touchTokenUsed 的限频观测写（helpers.go 的 60 秒窗口，
		// 夹具里 last_used_at 在窗口外），除此之外 api_code 不得引入任何业务写。
		if got, was := db.count("Tokens.TouchUsed"), before["Tokens.TouchUsed"]; got != was+1 {
			t.Fatalf("带 api_code 的观测写应恰好 1 次，实际 %d → %d", was, got)
		}
		for _, op := range writeOps {
			if op == "Tokens.TouchUsed" {
				continue
			}
			if db.count(op) != before[op] {
				t.Fatalf("带 api_code 引入了额外写 %s（%d → %d）", op, before[op], db.count(op))
			}
		}
		wantCalls(t, db, "QuotaPolicies.ListCandidates", 0, 0, "introspect 不碰配额")
		wantCalls(t, db, "QuotaUsages.Add", 0, 0, "introspect 不扣配额")
		wantCalls(t, db, "AppScopes.FindGrantedScopes", 0, 0, "无 required_scope 不读获批集")
	})
}

func TestIntrospectToken_DependencyFailureIsErrorNotDeny(t *testing.T) {
	// 依赖故障与「凭证无效」必须可区分：把 DB 抖动判成 inactive，
	// 网关会对一把好凭证停止重试并把用户踢出登录。
	cases := []struct {
		op    string
		scope string
	}{
		{"Tokens.FindByAccessHash", ""},
		{"Tokens.FindByID", ""},
		{"Grants.FindByID", ""},
		{"Apps.FindByID", ""},
		{"AppScopes.FindGrantedScopes", testScopeR},
	}
	for _, c := range cases {
		t.Run(c.op+" 故障", func(t *testing.T) {
			db, s, _, access, _, tok := oauthTokenFixture(t, oauthTokenScopes, 1)
			before := snapshotWrites(db)
			// 按定位方式选入口：Tokens.FindByID 只在 token_id 分支与 grant 分支之后才走到，
			// 直接注入到那一层即可。
			db.failOn(c.op, errFakeDown)
			id := int64(0)
			token := access
			if c.op == "Tokens.FindByID" {
				token, id = "", tok.TokenID
			}
			_, err := introspect(s, token, id, c.scope)
			if err == nil || !errors.Is(err, errFakeDown) {
				t.Fatalf("%s 的故障必须上抛，实际 err=%v", c.op, err)
			}
			wantNoWrites(t, db, before, c.op)
		})
	}

	t.Run("缺 pepper 时不做任何明文比对", func(t *testing.T) {
		db, s, _, access, _, _ := oauthTokenFixture(t, oauthTokenScopes, 1)
		s.Config.Security.CredentialPepper = ""
		before := snapshotWrites(db)
		reply, err := introspect(s, access, 0, "")
		wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "缺 pepper")
		wantNoWrites(t, db, before, "缺 pepper")
		// 算不出哈希就不查库：不做「拿明文直接比」这种降级。
		wantCalls(t, db, "Tokens.FindByAccessHash", 0, 0, "缺 pepper 不查库")
	})

	t.Run("权限点目录读故障仍然不放行", func(t *testing.T) {
		db, s, _, access, _, _ := oauthTokenFixture(t, oauthTokenScopes, 1)
		db.failOn("Scopes.FindByScope", errFakeDown)
		reply, err := introspect(s, access, 0, testScopeW)
		// 口径不精确处：目录读故障被折叠成 consent_missing（见交付报告的观察项），
		// 但方向是 fail closed —— 这里钉住的是「绝不因为读不到目录就放行」。
		if err == nil {
			if reply.Active || reply.DenyReason != denyConsentMissing {
				t.Fatalf("目录故障被放行或换了原因码：%+v", reply)
			}
			return
		}
		if !errors.Is(err, errFakeDown) {
			t.Fatalf("目录故障必须原样上抛，实际 %v", err)
		}
	})
}

func TestIntrospectToken_CacheSettingCannotWeakenJudgment(t *testing.T) {
	// 配置里的定位缓存只缓存 access_hash→token_id，不缓存结论；本用例钉住
	// 「把 IntrospectCacheSeconds 调到线上默认值也不会让已撤销凭证重新可用」。
	// 单测禁止连 Redis（ServiceContext.Cache 是具体类型 *redis.Redis），
	// 因此可证的分支是「缓存不可用时回落 DB 真值」；命中缓存的路径属剩余缺口。
	db, s, grant, access, _, tok := oauthTokenFixture(t, oauthTokenScopes, 1)
	s.Config.OpenPlatform.IntrospectCacheSeconds = 300
	if s.Cache != nil {
		t.Fatal("前提不成立：本用例要求无 Redis 的装配")
	}

	if _, err := revoke(s, &rpc.RevokeAuthorizationReq{
		Target: rpc.RevokeTarget_REVOKE_TARGET_GRANT, AppId: testAppID, Mid: testMid,
		OperatorMid: testMid,
	}); err != nil {
		t.Fatalf("撤销：%v", err)
	}
	if db.grants[grant.GrantID].RevokedAt == 0 {
		t.Fatal("前提不成立：撤销位点没写下")
	}
	for _, byID := range []bool{false, true} {
		label, token, id := "按明文", access, int64(0)
		if byID {
			label, token, id = "按主键", "", tok.TokenID
		}
		reply, err := introspect(s, token, id, "")
		if err != nil {
			t.Fatalf("%s：撤销后校验报错：%v", label, err)
		}
		if reply.Active || reply.DenyReason != denyRevoked {
			t.Fatalf("%s：缓存开着却仍判通过或换了原因码：%+v", label, reply)
		}
	}
}

// ---------------------------------------------------------------- 撤销

// revoke 撤销入口（三种目标由调用方填）。
func revoke(s *svc.ServiceContext, in *rpc.RevokeAuthorizationReq) (
	*rpc.RevokeAuthorizationReply, error) {
	return NewRevokeAuthorizationLogic(context.Background(), s).RevokeAuthorization(in)
}

// revokeByUser 用户撤销自己的授权 / 单个凭证。
func revokeByUser(s *svc.ServiceContext, target rpc.RevokeTarget, appID, mid int64,
	tokenID int64, hint string) (*rpc.RevokeAuthorizationReply, error) {
	return revoke(s, &rpc.RevokeAuthorizationReq{
		Target: target, AppId: appID, Mid: mid, TokenId: tokenID, TokenHint: hint,
		OperatorMid: mid,
	})
}

// grantRevokedRow 取库里的授权行，并断言撤销位点确实写下（否则后面的断言在测空气）。
func grantRevokedRow(t *testing.T, db *store, grantID int64) *model.Grant {
	t.Helper()
	g := db.grants[grantID]
	if g == nil {
		t.Fatalf("grant %d 不在库里", grantID)
	}
	if g.RevokedAt == 0 {
		t.Fatalf("grant %d 的撤销位点没写下", grantID)
	}
	return g
}

// countDeliveries 该事件名的投递任务数（撤销通知是否重发的唯一证据）。
func countDeliveries(db *store, eventID string) int {
	n := 0
	for _, d := range db.dels {
		if d.EventID == eventID {
			n++
		}
	}
	return n
}

// requireTokenState 断言某代凭证的行状态。
func requireTokenState(t *testing.T, db *store, tokenID int64, want int32, label string) {
	t.Helper()
	tok := db.tokens[tokenID]
	if tok == nil {
		t.Fatalf("%s：token %d 不在库里", label, tokenID)
	}
	if tok.State != want {
		t.Fatalf("%s：token %d 状态=%d，期望 %d", label, tokenID, tok.State, want)
	}
}

// ---------------------------------------------------------------- 校验
