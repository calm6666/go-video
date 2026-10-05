package logic

// 应用生命周期四法的契约测试：注册（幂等 + 明文只出一次）、改资料/推状态（通道分离 + CAS）、
// 详情（归属门禁 + 脱敏投影）、列表（分页边界 + 批量投影不产生 N+1）。

import (
	"fmt"
	"strings"
	"testing"

	"go-video/common/ratelimit"
	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

const testRedirect = "https://app.example.test/cb"

// drainWriteBucket 把写令牌桶换成零容量（与 limitedSvc 同一口径）：
// 用来证明限流发生在任何写之前，而不是写完了才拒绝。
func drainWriteBucket(s *svc.ServiceContext) {
	s.WriteLimiter = ratelimit.NewTokenBucket(0, 0)
}

// appFixture 一个 ACTIVE 应用 + 一个已声明的读 scope 目录项。
func appFixture(t *testing.T) (*store, *svc.ServiceContext) {
	t.Helper()
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	seedApp(db, testAppID, testOwner, testScopeR)
	db.apps[testAppID].RedirectURIs = testRedirect
	return db, newTestSvc(db)
}

// mustNoPlaintextStored 断言明文凭证在整张内存库里一处都找不到。
// 这是「HMAC 入库」唯一能被证明的形式：不是「测试没去读它」，而是「全表扫过也没有」。
func mustNoPlaintextStored(t *testing.T, db *store, label string, plains ...string) {
	t.Helper()
	var dump strings.Builder
	for _, a := range db.apps {
		dump.WriteString(fmt.Sprintf("%+v", *a))
	}
	for _, s := range db.secrets {
		dump.WriteString(fmt.Sprintf("%+v", *s))
	}
	for _, tk := range db.tokens {
		dump.WriteString(fmt.Sprintf("%+v", *tk))
	}
	for _, c := range db.codes {
		dump.WriteString(fmt.Sprintf("%+v", *c))
	}
	for _, e := range db.eps {
		dump.WriteString(fmt.Sprintf("%+v", *e))
	}
	for _, d := range db.dels {
		dump.WriteString(fmt.Sprintf("%+v", *d))
	}
	text := dump.String()
	for _, plain := range plains {
		if plain == "" {
			t.Fatalf("%s：明文凭证为空串，断言无意义", label)
		}
		if strings.Contains(text, plain) {
			t.Fatalf("%s：明文凭证落库了（长度 %d）", label, len(plain))
		}
	}
}

// mustNoPlaintextInReply 断言读侧响应体不含任何凭证材料（salt/hash/明文）。
func mustNoPlaintextInReply(t *testing.T, reply any, label string, banned ...string) {
	t.Helper()
	text := fmt.Sprintf("%+v", reply)
	for _, b := range banned {
		if b != "" && strings.Contains(text, b) {
			t.Fatalf("%s：响应泄露凭证材料 %q", label, b)
		}
	}
}

func registerReq() *rpc.RegisterApplicationReq {
	return &rpc.RegisterApplicationReq{
		Name:         "demo-app",
		Description:  "d",
		OwnerMid:     testOwner,
		RedirectUris: []string{testRedirect},
		Scopes:       []string{testScopeR},
		ClientToken:  "client-token-1",
	}
}

// ---------------------------------------------------------------- 注册

func TestRegisterApplication_IssuesSecretOnceAndStaysPendingReview(t *testing.T) {
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	s := newTestSvc(db)

	before := snapshotWrites(db)
	reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(registerReq())
	if reply == nil {
		t.Fatalf("注册应成功，实际 reply=nil err=%v", err)
	}
	reply = wantOK(t, reply, err, "正常注册")
	// 应用行与密钥行必须落在同一个事务里：两条独立写会留下「有密钥无应用」的孤儿凭证。
	wantCalls(t, db, "DB.TransactCtx", before["DB.TransactCtx"], 1, "注册走一个事务")
	wantCalls(t, db, "Apps.InsertTx", before["Apps.InsertTx"], 1, "应用行写一次")
	wantCalls(t, db, "Secrets.InsertTx", before["Secrets.InsertTx"], 1, "密钥行写一次")
	wantCalls(t, db, "AppScopes.Request", before["AppScopes.Request"], 1, "scope 申请写一次")

	if reply.Replayed {
		t.Fatal("首次注册不得标记为重放")
	}
	if len(reply.ClientSecret) != credentialBytes*2 {
		t.Fatalf("明文密钥长度=%d，期望 %d", len(reply.ClientSecret), credentialBytes*2)
	}
	app := reply.App
	if app == nil {
		t.Fatal("响应缺应用投影")
	}
	if app.Status != rpc.AppStatus_APP_STATUS_PENDING_REVIEW {
		t.Fatalf("新应用状态=%v，期望待审", app.Status)
	}
	if app.SecretState != rpc.SecretState_SECRET_STATE_CONFIGURED {
		t.Fatalf("密钥状态=%v，期望 CONFIGURED（首把密钥已随应用同事务落库）", app.SecretState)
	}
	if app.AppKey == "" || !strings.HasPrefix(app.AppKey, "opk_") {
		t.Fatalf("app_key=%q 形态不对", app.AppKey)
	}
	// 明文只出一次：库里只有 salt+HMAC(pepper, salt||明文)。
	secret, ok := db.secrets[1]
	if !ok || len(db.secrets) != 1 {
		t.Fatalf("应恰好落一把密钥，实际 %d 把", len(db.secrets))
	}
	if secret.Hash == reply.ClientSecret || secret.Salt == reply.ClientSecret {
		t.Fatal("密钥列被明文覆盖")
	}
	if want, _ := model.HashSecret(testPepper, secret.Salt, reply.ClientSecret); secret.Hash != want {
		t.Fatal("入库哈希与「HMAC(pepper, salt||明文)」口径不符，下次验签必然对不上")
	}
	mustNoPlaintextStored(t, db, "注册", reply.ClientSecret)

	// 申请的 scope 只是「待审」，绝不等于已获批：授权链读的是 granted 视图。
	if got := db.appscopes[app.AppId][testScopeR]; got == nil ||
		got.State != model.AppScopePending {
		t.Fatalf("scope 申请行状态=%+v，期望 PENDING", got)
	}
	if db.granted[app.AppId][testScopeR] {
		t.Fatal("申请的 scope 被当成已获批，最小权限边界失守")
	}
	// 幂等键落库（它是重放判定的唯一锚点），但回调白名单按规范化后的值存。
	if fresh := db.apps[app.AppId]; fresh.RegisterToken != "client-token-1" ||
		fresh.RedirectURIs != testRedirect || fresh.OwnerMid != testOwner {
		t.Fatalf("入库行与入参不符：%+v", fresh)
	}
	if app.SecretRotatedAt == 0 {
		t.Fatal("密钥概览应给出最近签发时间（列表/详情的 SecretState 同源）")
	}
}

func TestRegisterApplication_SecretValidDaysConfigDrivesExpiry(t *testing.T) {
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	s := newTestSvc(db)
	s.Config.OpenPlatform.SecretValidDays = 30

	req := registerReq()
	req.Scopes = nil
	reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(req)
	reply = wantOK(t, reply, err, "配置了密钥有效期")
	now := nowTS()
	if left := reply.SecretExpiresAt - now; left < 29*86400 || left > 30*86400 {
		t.Fatalf("SecretExpiresAt-now=%d，期望约 30 天", left)
	}
	if db.secrets[1].ExpiresAt != reply.SecretExpiresAt {
		t.Fatal("响应回显的有效期与入库值不一致")
	}

	// 未配置有效期时必须回 0（长期有效），而不是伪造一个远期时间戳。
	db2 := newStore()
	seedScopeDir(db2, testScopeR, model.ScopeAccessRead)
	r2, err := NewRegisterApplicationLogic(t.Context(), newTestSvc(db2)).RegisterApplication(req)
	r2 = wantOK(t, r2, err, "未配置有效期")
	if r2.SecretExpiresAt != 0 || db2.secrets[1].ExpiresAt != 0 {
		t.Fatalf("未配置有效期却回了 %d，应为 0", r2.SecretExpiresAt)
	}
}

func TestRegisterApplication_ReplayNeverReturnsSecret(t *testing.T) {
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	app := seedApp(db, testAppID, testOwner, testScopeR)
	app.RegisterToken = "client-token-1"
	app.Status = model.AppStatusPendingReview
	seedSecret(t, db, testAppID, "plain-secret-once")

	before := snapshotWrites(db)
	reply, err := NewRegisterApplicationLogic(t.Context(), newTestSvc(db)).
		RegisterApplication(registerReq())
	reply = wantOK(t, reply, err, "同 client_token 重放")

	if !reply.Replayed {
		t.Fatal("命中 client_token 必须标记 replayed")
	}
	if reply.ClientSecret != "" {
		t.Fatal("重放响应不得带回密钥明文（库里只有哈希，物理上也回不出来）")
	}
	if reply.App.GetAppId() != testAppID {
		t.Fatalf("重放应回既有 app_id=%d，实际 %d", testAppID, reply.App.GetAppId())
	}
	if len(db.secrets) != 1 {
		t.Fatalf("重放不得再签一把密钥，实际 %d 把", len(db.secrets))
	}
	wantNoWrites(t, db, before, "注册重放")
}

func TestRegisterApplication_GateFailuresAreSideEffectFree(t *testing.T) {
	longName := strings.Repeat("名", maxAppNameRunes+1)
	longDesc := strings.Repeat("述", maxAppDescRunes+1)
	longToken := strings.Repeat("t", maxClientTokenRunes+1)
	manyScopes := make([]string, 0, maxScopeCountPerReq+1)
	for i := 0; i <= maxScopeCountPerReq; i++ {
		manyScopes = append(manyScopes, fmt.Sprintf("video.read%d", i))
	}

	cases := []struct {
		name  string
		in    *rpc.RegisterApplicationReq
		want  error
		tweak func(db *store, s *svc.ServiceContext)
	}{
		{name: "应用名缺失", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Name = "   "
			return r
		}(), want: errNameRequired},
		{name: "应用名超列宽", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Name = longName
			return r
		}(), want: errNameTooLong},
		{name: "简介超列宽", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Description = longDesc
			return r
		}(), want: errDescriptionTooLong},
		{name: "归属 mid 缺失", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.OwnerMid = 0
			return r
		}(), want: model.ErrOwnerRequired},
		{name: "幂等键缺失", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.ClientToken = ""
			return r
		}(), want: model.ErrClientTokenRequired},
		{name: "幂等键超长", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.ClientToken = longToken
			return r
		}(), want: model.ErrClientTokenRequired},
		{name: "回调非 https", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.RedirectUris = []string{"http://app.example.test/cb"}
			return r
		}(), want: model.ErrInvalidRedirectURI},
		{name: "回调指向内网主机名", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.RedirectUris = []string{"https://api.internal/cb"}
			return r
		}(), want: model.ErrInvalidRedirectURI},
		{name: "回调回环字面 IP", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.RedirectUris = []string{"https://127.0.0.1/cb"}
			return r
		}(), want: model.ErrInvalidRedirectURI},
		{name: "回调夹带 userinfo", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.RedirectUris = []string{"https://u:p@app.example.test/cb"}
			return r
		}(), want: model.ErrInvalidRedirectURI},
		{name: "回调条数超配置上限", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.RedirectUris = []string{"https://a.example.test/cb", "https://b.example.test/cb",
				"https://c.example.test/cb"}
			return r
		}(), want: model.ErrTooManyRedirectURIs,
			tweak: func(_ *store, s *svc.ServiceContext) { s.Config.OpenPlatform.MaxRedirectURIs = 2 }},
		{name: "scope 字符合法性", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Scopes = []string{"bad scope"}
			return r
		}(), want: model.ErrScopeUnknown},
		{name: "scope 条数超上限", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Scopes = manyScopes
			return r
		}(), want: model.ErrTooManyScopes},
		{name: "红线类目", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Scopes = []string{"member.vip.read"}
			return r
		}(), want: model.ErrForbiddenScopeCategory},
		{name: "目录里不存在", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Scopes = []string{"video.not-exist"}
			return r
		}(), want: model.ErrScopeUnknown},
		{name: "目录已停用", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Scopes = []string{"video.disabled"}
			return r
		}(), want: model.ErrScopeDisabled,
			tweak: func(db *store, _ *svc.ServiceContext) {
				seedScopeFull(db, "video.disabled", model.ScopeAccessRead, model.ScopeRiskLow, 0, 0)
			}},
		{name: "写 scope 未声明用户同意", in: func() *rpc.RegisterApplicationReq {
			r := registerReq()
			r.Scopes = []string{testScopeW}
			return r
		}(), want: model.ErrScopeWriteRequiresConsent,
			tweak: func(db *store, _ *svc.ServiceContext) {
				seedScopeDir(db, testScopeW, model.ScopeAccessWrite)
			}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			seedScopeDir(db, testScopeR, model.ScopeAccessRead)
			s := newTestSvc(db)
			if tc.tweak != nil {
				tc.tweak(db, s)
			}
			before := snapshotWrites(db)
			reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(tc.in)
			wantFail(t, reply, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			if len(db.apps) != 0 || len(db.secrets) != 0 || len(db.appscopes) != 0 {
				t.Fatalf("%s：门禁失败仍写了行 app=%d secret=%d", tc.name, len(db.apps), len(db.secrets))
			}
		})
	}
}

func TestRegisterApplication_FailClosedOnDownstream(t *testing.T) {
	cases := []struct {
		name string
		fail string
		want error
	}{
		{name: "幂等位点读不到", fail: "Apps.FindByRegisterToken", want: errFakeDown},
		{name: "app_key 预检读不到", fail: "Apps.FindByAppKey", want: errFakeDown},
		{name: "scope 目录读不到", fail: "Scopes.FindByScopes", want: errFakeDown},
		{name: "应用写入故障", fail: "Apps.InsertTx", want: errFakeDown},
		{name: "密钥投影读不到", fail: "Secrets.SummariesByApps", want: errFakeDown},
		{name: "获批 scope 投影读不到", fail: "AppScopes.ListGranted", want: errFakeDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			seedScopeDir(db, testScopeR, model.ScopeAccessRead)
			s := newTestSvc(db)
			db.failOn(tc.fail, errFakeDown)
			reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(registerReq())
			if err == nil || !strings.Contains(err.Error(), tc.want.Error()) {
				t.Fatalf("%s：应报 %v，实际 err=%v", tc.name, tc.want, err)
			}
			if reply != nil {
				t.Fatalf("%s：故障路径不得带回响应体：%+v", tc.name, reply)
			}
		})
	}

	t.Run("未配置 pepper 一票否决签发能力", func(t *testing.T) {
		db := newStore()
		seedScopeDir(db, testScopeR, model.ScopeAccessRead)
		s := newTestSvc(db)
		s.Config.Security.CredentialPepper = ""
		before := snapshotWrites(db)
		reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(registerReq())
		wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "无 pepper")
		wantNoWrites(t, db, before, "无 pepper")
	})

	t.Run("写令牌耗尽发生在落库之前", func(t *testing.T) {
		db := newStore()
		seedScopeDir(db, testScopeR, model.ScopeAccessRead)
		before := snapshotWrites(db)
		reply, err := NewRegisterApplicationLogic(t.Context(), limitedSvc(db)).
			RegisterApplication(registerReq())
		wantFail(t, reply, err, model.ErrRateLimited, "注册被限流")
		wantNoWrites(t, db, before, "注册被限流")
	})
}

func TestRegisterApplication_SecretWriteFailureRollsBackWholeApp(t *testing.T) {
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	s := newTestSvc(db)
	db.failOn("Secrets.InsertTx", errFakeDown)

	reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(registerReq())
	if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
		t.Fatalf("密钥写入失败应整体报错，实际 err=%v", err)
	}
	if reply != nil {
		t.Fatal("失败路径不得带回响应体")
	}
	// 计数说「尝试过」，数据说「什么都没留下」：两条一起才排除假绿。
	wantCalls(t, db, "Apps.InsertTx", 0, 1, "应用行写入尝试过")
	if len(db.apps) != 0 || len(db.secrets) != 0 {
		t.Fatalf("事务必须整体回滚，实际 app=%d secret=%d", len(db.apps), len(db.secrets))
	}
}

func TestRegisterApplication_ConcurrentSameClientTokenAbortsBeforeSecret(t *testing.T) {
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	s := newTestSvc(db)
	// 交错：本事务的应用写入即将执行时，另一个请求用同一 client_token 先提交了。
	db.onHit("Apps.InsertTx", func() {
		now := nowTS()
		db.apps[testApp2] = &model.Application{AppID: testApp2, AppKey: "opk_racer", Name: "racer",
			OwnerMid: testOwner, Status: model.AppStatusPendingReview, Version: 1,
			RegisterToken: "client-token-1", Ctime: now, Mtime: now}
	})

	reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(registerReq())
	wantFail(t, reply, err, model.ErrConcurrentUpdate, "并发同幂等键")
	if db.count("Secrets.InsertTx") != 0 {
		t.Fatal("应用行未创建就签密钥，会留下「有密钥无应用」的孤儿凭证")
	}
}

func TestRegisterApplication_ReplyVersionMatchesStoredRow(t *testing.T) {
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	s := newTestSvc(db)

	reply, err := NewRegisterApplicationLogic(t.Context(), s).RegisterApplication(registerReq())
	reply = wantOK(t, reply, err, "正常注册")

	stored := db.apps[reply.App.GetAppId()]
	// 回归护栏（缺陷于 2026-09-22 修掉）：早先 registerapplicationlogic.go 直接投影事务里那个
	// 未经回读的 app 结构，而 op_app.go 的 INSERT 硬编码 version=1 —— 响应里的 version 恒为 0。
	// version 是 UpdateApplication 的乐观锁入参（ApplicationInfo.version 注释即「乐观锁版本」），
	// 且 applyProfile 对 expected_version<=0 直接回 ErrConcurrentUpdate，
	// 因此照本响应走「注册→改资料」的调用方第一步必失败。现在提交后回读真值再投影。
	// fake 的 insert 用 `row := *app` 复制后再置 version=1，所以本断言不会被指针别名糊过去。
	if reply.App.GetVersion() != stored.Version {
		t.Fatalf("响应 version=%d 与入库真值 version=%d 不一致（注册响应的乐观锁令牌不可用）",
			reply.App.GetVersion(), stored.Version)
	}
}

// ---------------------------------------------------------------- 改资料 / 推状态

func TestUpdateApplication_OwnerProfileCASBumpsVersion(t *testing.T) {
	db, s := appFixture(t)
	sec := seedSecret(t, db, testAppID, "plain-secret-once")
	// 内存库里应用行是指针，改后会一起变：版本必须按改前的值快照。
	beforeApp := *db.apps[testAppID]

	reply, err := NewUpdateApplicationLogic(t.Context(), s).UpdateApplication(
		&rpc.UpdateApplicationReq{
			AppId: testAppID, Name: "新名字", Description: "新简介",
			ExpectedVersion: beforeApp.Version, OperatorMid: testOwner,
		})
	reply = wantOK(t, reply, err, "owner 改资料")

	if !reply.Changed {
		t.Fatal("改了字段却回 changed=false")
	}
	fresh := db.apps[testAppID]
	if fresh.Name != "新名字" || fresh.Description != "新简介" {
		t.Fatalf("入库值未更新：%+v", fresh)
	}
	if fresh.Version != beforeApp.Version+1 {
		t.Fatalf("版本未自增：%d → %d", beforeApp.Version, fresh.Version)
	}
	if reply.App.GetVersion() != fresh.Version {
		t.Fatalf("响应 version=%d 与入库真值 %d 不一致（调用方拿不到下一轮的乐观锁令牌）",
			reply.App.GetVersion(), fresh.Version)
	}
	// 空列表表示「不修改」：把白名单清空等于断掉用户自己的撤销路径。
	if fresh.RedirectURIs != testRedirect || reply.App.GetRedirectUris()[0] != testRedirect {
		t.Fatalf("未传的回调白名单被改写：%q", fresh.RedirectURIs)
	}
	if fresh.Status != model.AppStatusActive {
		t.Fatal("开发者改资料不得推进状态")
	}
	// 脱敏：改资料响应既不带明文，也不带 salt / 入库哈希（它们合起来就是可验证的凭证材料）。
	mustNoPlaintextInReply(t, reply, "改资料响应", "plain-secret-once", sec.Salt, sec.Hash)
}

func TestUpdateApplication_ChannelSeparationAndGates(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.UpdateApplicationReq
		want  error
		tweak func(db *store, s *svc.ServiceContext)
	}{
		{name: "app 不存在", in: &rpc.UpdateApplicationReq{AppId: 555, Name: "x",
			ExpectedVersion: 1, OperatorMid: testOwner}, want: model.ErrAppNotFound},
		{name: "app_id 非法", in: &rpc.UpdateApplicationReq{AppId: 0, Name: "x",
			ExpectedVersion: 1, OperatorMid: testOwner}, want: model.ErrInvalidAppID},
		{name: "开发者不得自批状态", in: &rpc.UpdateApplicationReq{AppId: testAppID,
			TargetStatus: rpc.AppStatus_APP_STATUS_ACTIVE, OperatorMid: testOwner},
			want: model.ErrOwnerRequired},
		{name: "非归属者改资料", in: &rpc.UpdateApplicationReq{AppId: testAppID, Name: "x",
			ExpectedVersion: 1, OperatorMid: testMid}, want: model.ErrOwnerRequired},
		{name: "运营通道未给目标状态", in: &rpc.UpdateApplicationReq{AppId: testAppID,
			IsOperator: true, OperatorMid: testMid, Reason: "r"}, want: errTargetStatusRequired},
		{name: "运营不得顺手改资料", in: &rpc.UpdateApplicationReq{AppId: testAppID,
			IsOperator: true, OperatorMid: testMid, Reason: "r", Name: "运营改的名",
			TargetStatus: rpc.AppStatus_APP_STATUS_SUSPENDED}, want: model.ErrOwnerRequired},
		{name: "匿名运营", in: &rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true,
			TargetStatus: rpc.AppStatus_APP_STATUS_SUSPENDED, Reason: "r"},
			want: model.ErrOperatorRequired},
		{name: "状态变更无原因", in: &rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true,
			OperatorMid: testMid, TargetStatus: rpc.AppStatus_APP_STATUS_SUSPENDED},
			want: errReasonRequired},
		{name: "原因超长", in: &rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true,
			OperatorMid: testMid, TargetStatus: rpc.AppStatus_APP_STATUS_SUSPENDED,
			Reason: strings.Repeat("因", maxReasonRunes+1)}, want: errReasonTooLong},
		{name: "目标状态未定义", in: &rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true,
			OperatorMid: testMid, Reason: "r", TargetStatus: 9}, want: errInvalidTargetStatus},
		{name: "非法状态迁移", in: &rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true,
			OperatorMid: testMid, Reason: "r",
			TargetStatus: rpc.AppStatus_APP_STATUS_PENDING_REVIEW},
			want: model.ErrInvalidStateTransition},
		{name: "乐观锁版本未传", in: &rpc.UpdateApplicationReq{AppId: testAppID, Name: "x",
			OperatorMid: testOwner}, want: model.ErrConcurrentUpdate},
		{name: "应用名超长", in: &rpc.UpdateApplicationReq{AppId: testAppID,
			Name: strings.Repeat("名", maxAppNameRunes+1), ExpectedVersion: 1,
			OperatorMid: testOwner}, want: errNameTooLong},
		{name: "简介超长", in: &rpc.UpdateApplicationReq{AppId: testAppID,
			Description: strings.Repeat("述", maxAppDescRunes+1), ExpectedVersion: 1,
			OperatorMid: testOwner}, want: errDescriptionTooLong},
		{name: "脏回调地址", in: &rpc.UpdateApplicationReq{AppId: testAppID, ExpectedVersion: 1,
			OperatorMid: testOwner, RedirectUris: []string{"https://10.0.0.8/cb"}},
			want: model.ErrInvalidRedirectURI},
		{name: "被限流", in: &rpc.UpdateApplicationReq{AppId: testAppID, Name: "x",
			ExpectedVersion: 1, OperatorMid: testOwner}, want: model.ErrRateLimited,
			tweak: func(_ *store, s *svc.ServiceContext) { drainWriteBucket(s) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, s := appFixture(t)
			if tc.tweak != nil {
				tc.tweak(db, s)
			}
			before := snapshotWrites(db)
			beforeApp := *db.apps[testAppID]
			reply, err := NewUpdateApplicationLogic(t.Context(), s).UpdateApplication(tc.in)
			wantFail(t, reply, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			if after := db.apps[testAppID]; *after != beforeApp {
				t.Fatalf("%s：门禁失败仍改了应用行\n前：%+v\n后：%+v", tc.name, beforeApp, *after)
			}
		})
	}
}

func TestUpdateApplication_StaleVersionLeavesRowUntouched(t *testing.T) {
	db, s := appFixture(t)
	before := snapshotWrites(db)

	reply, err := NewUpdateApplicationLogic(t.Context(), s).UpdateApplication(
		&rpc.UpdateApplicationReq{AppId: testAppID, Name: "过期写", ExpectedVersion: 999,
			OperatorMid: testOwner})
	wantFail(t, reply, err, model.ErrConcurrentUpdate, "版本已落后")
	// CAS 确实发生了（计数+1），但库里那一行没被改（数据态）——两条一起才不是假绿。
	wantCalls(t, db, "Apps.UpdateProfile", before["Apps.UpdateProfile"], 1, "CAS 尝试一次")
	if db.apps[testAppID].Name == "过期写" || db.apps[testAppID].Version != 1 {
		t.Fatalf("CAS 未命中却改了行：%+v", *db.apps[testAppID])
	}
}

func TestUpdateApplication_OfflineIsTerminal(t *testing.T) {
	db := newStore()
	seedStatusApp(db, testAppID, testOwner, model.AppStatusOffline)
	s := newTestSvc(db)

	for _, tc := range []struct {
		name string
		to   rpc.AppStatus
	}{
		{"终态不可复活", rpc.AppStatus_APP_STATUS_ACTIVE},
		{"终态不可再停用", rpc.AppStatus_APP_STATUS_SUSPENDED},
		{"同态也不放行（离线必须只发生一次）", rpc.AppStatus_APP_STATUS_OFFLINE},
	} {
		before := snapshotWrites(db)
		reply, err := NewUpdateApplicationLogic(t.Context(), s).UpdateApplication(
			&rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true, OperatorMid: testMid,
				Reason: "r", TargetStatus: tc.to})
		wantFail(t, reply, err, model.ErrInvalidStateTransition, tc.name)
		wantNoWrites(t, db, before, tc.name)
	}
}

func TestUpdateApplication_SuspendRevokesTokensAndSuppressesDeliveries(t *testing.T) {
	db, s := appFixture(t)
	grant := seedGrant(db, testAppID, testMid, []string{testScopeR}, 1)
	seedToken(t, db, grant, []string{testScopeR}, 10, 3600, 3600)
	seedToken(t, db, grant, []string{testScopeR}, 5, 3600, 3600)
	ep := seedEndpoint(db, testAppID, model.WebhookEventContentPublishResult)
	pending := seedDelivery(db, testAppID, ep, "evt-1", model.DeliveryStatePending, `{"a":1}`)
	done := seedDelivery(db, testAppID, ep, "evt-2", model.DeliveryStateSuccess, `{"a":2}`)
	delsBefore := len(db.dels)

	reply, err := NewUpdateApplicationLogic(t.Context(), s).UpdateApplication(
		&rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true, OperatorMid: testMid,
			Reason: "违规处置", TargetStatus: rpc.AppStatus_APP_STATUS_SUSPENDED})
	reply = wantOK(t, reply, err, "运营停用")

	if reply.App.GetStatus() != rpc.AppStatus_APP_STATUS_SUSPENDED {
		t.Fatalf("状态未推进：%v", reply.App.GetStatus())
	}
	for _, tok := range db.tokens {
		if tok.State != model.TokenStateRevoked {
			t.Fatalf("停用应用的 token 仍可用：token=%d state=%d", tok.TokenID, tok.State)
		}
	}
	if db.dels[0].State != model.DeliveryStateIgnored || pending.State != model.DeliveryStateIgnored {
		t.Fatalf("端点删除前的在途任务未抑制：state=%d", db.dels[0].State)
	}
	if db.dels[1].State != model.DeliveryStateSuccess && done.State != model.DeliveryStateSuccess {
		t.Fatal("已成功的投递结论被改动：历史审计结论必须不动")
	}
	// 刻意不入队 GRANT_REVOKED：Tokens.RevokeByApp 拿不到 grant_id 集合，
	// 造一条 grant_id=0 的通知比不发更糟（见文件内注释与 README 已知缺口）。
	if len(db.dels) != delsBefore {
		t.Fatalf("停用不应新增投递任务，实际 %d → %d", delsBefore, len(db.dels))
	}
	for _, d := range db.dels {
		if strings.HasPrefix(d.EventID, "grant-revoked:") {
			t.Fatalf("停用路径入队了 grant 级通知：%s", d.EventID)
		}
	}
}

func TestUpdateApplication_CredentialRevokeFailureKeepsStatusAdvanced(t *testing.T) {
	db, s := appFixture(t)
	grant := seedGrant(db, testAppID, testMid, []string{testScopeR}, 1)
	seedToken(t, db, grant, []string{testScopeR}, 10, 3600, 3600)
	ep := seedEndpoint(db, testAppID, model.WebhookEventContentPublishResult)
	seedDelivery(db, testAppID, ep, "evt-1", model.DeliveryStatePending, `{"a":1}`)
	db.failOn("Tokens.RevokeByApp", errFakeDown)

	reply, err := NewUpdateApplicationLogic(t.Context(), s).UpdateApplication(
		&rpc.UpdateApplicationReq{AppId: testAppID, IsOperator: true, OperatorMid: testMid,
			Reason: "r", TargetStatus: rpc.AppStatus_APP_STATUS_SUSPENDED})
	reply = wantOK(t, reply, err, "吊销故障时状态仍已推进")

	// 顺序正确性：状态是真值，它先落库且不在事务里，所以吊销失败不会把应用留在「可用」态。
	if db.apps[testAppID].Status != model.AppStatusSuspended {
		t.Fatal("状态未推进，等于停用没生效")
	}
	if db.tokens[1].State == model.TokenStateRevoked {
		t.Fatal("事务回滚后 token 不该被改（半途写入更危险）")
	}
	if db.dels[0].State != model.DeliveryStatePending {
		t.Fatal("同一事务里的抑制未随吊销一起回滚")
	}
}

// ---------------------------------------------------------------- 详情

func TestGetApplication_ProjectionAndOwnership(t *testing.T) {
	db, s := appFixture(t)
	secret := seedSecret(t, db, testAppID, "plain-secret-once")
	seedApp(db, testApp2, testOwner+1, testScopeR)

	t.Run("owner 按 app_id 看自己", func(t *testing.T) {
		reply, err := NewGetApplicationLogic(t.Context(), s).GetApplication(
			&rpc.GetApplicationReq{AppId: testAppID, CallerMid: testOwner})
		reply = wantOK(t, reply, err, "owner 查询")
		if reply.App.GetAppId() != testAppID || reply.App.GetOwnerMid() != testOwner {
			t.Fatalf("投影错行：%+v", reply.App)
		}
		if reply.App.GetSecretState() != rpc.SecretState_SECRET_STATE_CONFIGURED {
			t.Fatalf("SecretState=%v，有生效密钥时应为 CONFIGURED", reply.App.GetSecretState())
		}
		if got := reply.App.GetScopes(); len(got) != 1 || got[0] != testScopeR {
			t.Fatalf("scopes 投影=%v，期望来自获批视图", got)
		}
		// 脱敏：详情既不回显明文，也不回显 salt/hash/register_token。
		mustNoPlaintextInReply(t, reply, "详情响应",
			"plain-secret-once", secret.Salt, secret.Hash)
		if strings.Contains(fmt.Sprintf("%+v", reply.App), "client-token") {
			t.Fatal("响应含 register_token（幂等键等同凭证）")
		}
	})

	t.Run("按 app_key 查", func(t *testing.T) {
		reply, err := NewGetApplicationLogic(t.Context(), s).GetApplication(
			&rpc.GetApplicationReq{AppKey: "opk_" + fmt.Sprint(testAppID), CallerMid: testOwner})
		reply = wantOK(t, reply, err, "app_key 查询")
		if reply.App.GetAppId() != testAppID {
			t.Fatalf("按 key 定位错行：%d", reply.App.GetAppId())
		}
	})

	t.Run("两者都给时以 app_id 为准", func(t *testing.T) {
		before := db.count("Apps.FindByAppKey")
		reply, err := NewGetApplicationLogic(t.Context(), s).GetApplication(
			&rpc.GetApplicationReq{AppId: testApp2, AppKey: "opk_" + fmt.Sprint(testAppID),
				CallerMid: testOwner + 1})
		reply = wantOK(t, reply, err, "app_id 优先")
		wantCalls(t, db, "Apps.FindByAppKey", before, 0, "给了 app_id 就不该再按 key 定位")
		if reply.App.GetAppId() != testApp2 {
			t.Fatalf("app_key 覆盖了 app_id：%d", reply.App.GetAppId())
		}
	})

	t.Run("越权与探测", func(t *testing.T) {
		before := snapshotWrites(db)
		reply, err := NewGetApplicationLogic(t.Context(), s).GetApplication(
			&rpc.GetApplicationReq{AppId: testAppID, CallerMid: testMid})
		wantFail(t, reply, err, model.ErrOwnerRequired, "非 owner 非运营")
		reply, err = NewGetApplicationLogic(t.Context(), s).GetApplication(
			&rpc.GetApplicationReq{AppId: 4242, CallerMid: testOwner})
		wantFail(t, reply, err, model.ErrAppNotFound, "不存在")
		reply, err = NewGetApplicationLogic(t.Context(), s).GetApplication(
			&rpc.GetApplicationReq{CallerMid: testOwner})
		wantFail(t, reply, err, model.ErrInvalidAppID, "两个定位键都没给")
		reply, err = NewGetApplicationLogic(t.Context(), s).GetApplication(
			&rpc.GetApplicationReq{AppId: testAppID, Operator: true})
		wantFail(t, reply, err, model.ErrOwnerRequired, "匿名运营不得看")
		wantNoWrites(t, db, before, "详情读侧")
	})

	t.Run("非 ACTIVE 应用只有运营可见", func(t *testing.T) {
		db2 := newStore()
		seedStatusApp(db2, testAppID, testOwner, model.AppStatusPendingReview)
		s2 := newTestSvc(db2)
		reply, err := NewGetApplicationLogic(t.Context(), s2).GetApplication(
			&rpc.GetApplicationReq{AppId: testAppID, CallerMid: testOwner})
		wantFail(t, reply, err, model.ErrAppNotFound, "owner 也看不到待审身份")
		reply, err = NewGetApplicationLogic(t.Context(), s2).GetApplication(
			&rpc.GetApplicationReq{AppId: testAppID, CallerMid: testMid, Operator: true})
		reply = wantOK(t, reply, err, "运营看全状态")
		if reply.App.GetStatus() != rpc.AppStatus_APP_STATUS_PENDING_REVIEW {
			t.Fatalf("运营视图状态=%v", reply.App.GetStatus())
		}
	})
}

func TestGetApplication_FailClosedOnDownstream(t *testing.T) {
	for _, op := range []string{"Apps.FindByID", "Apps.FindByAppKey", "AppScopes.ListGranted",
		"Secrets.SummariesByApps"} {
		t.Run(op, func(t *testing.T) {
			db, s := appFixture(t)
			db.failOn(op, errFakeDown)
			var reply *rpc.GetApplicationReply
			var err error
			if op == "Apps.FindByAppKey" {
				reply, err = NewGetApplicationLogic(t.Context(), s).GetApplication(
					&rpc.GetApplicationReq{AppKey: "opk_9001", CallerMid: testOwner})
			} else {
				reply, err = NewGetApplicationLogic(t.Context(), s).GetApplication(
					&rpc.GetApplicationReq{AppId: testAppID, CallerMid: testOwner})
			}
			if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
				t.Fatalf("%s 故障应报错，实际 err=%v", op, err)
			}
			if reply != nil {
				t.Fatalf("%s 故障不得带回半个投影：%+v", op, reply)
			}
		})
	}
}

// ---------------------------------------------------------------- 列表

func TestListApplications_PagingAndOwnership(t *testing.T) {
	db := newStore()
	// 自己的三行刻意避开 testApp2（=9002，别人的应用），否则 seedApp 会覆盖它、
	// 越权断言与总数都失去意义。
	for i := int64(0); i < 3; i++ {
		seedApp(db, testAppID+100+i, testOwner)
	}
	seedApp(db, testApp2, testOwner+7)
	s := newTestSvc(db)

	reply, err := NewListApplicationsLogic(t.Context(), s).ListApplications(
		&rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: 2})
	reply = wantOK(t, reply, err, "第一页")
	if len(reply.List) != 2 || !reply.HasMore {
		t.Fatalf("第一页 n=%d has_more=%t，期望 2/true（多取一条判位点）", len(reply.List), reply.HasMore)
	}
	for _, a := range reply.List {
		if a.GetOwnerMid() != testOwner {
			t.Fatalf("列表混入他人应用 app_id=%d owner=%d", a.GetAppId(), a.GetOwnerMid())
		}
	}
	seen := map[int64]bool{reply.List[0].GetAppId(): true, reply.List[1].GetAppId(): true}

	reply2, err := NewListApplicationsLogic(t.Context(), s).ListApplications(
		&rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: 2, Cursor: reply.NextCursor})
	reply2 = wantOK(t, reply2, err, "第二页")
	if reply2.HasMore {
		t.Fatal("3 条数据 ps=2 的第二页不该还有下一页")
	}
	if len(reply2.List) != 1 {
		t.Fatalf("第二页 n=%d，期望 1（不遗漏）", len(reply2.List))
	}
	last := reply2.List[0].GetAppId()
	if seen[last] {
		t.Fatalf("第二页与第一页重复 app_id=%d（游标位点语义破了）", last)
	}
	if last == testApp2 {
		t.Fatal("越权：别人的应用出现在自己的列表里")
	}

	// 运营全量分支按状态过滤：未知状态值不能被当成「不过滤」。
	all, err := NewListApplicationsLogic(t.Context(), s).ListApplications(
		&rpc.ListApplicationsReq{Operator: true, Ps: 50})
	all = wantOK(t, all, err, "运营全量")
	if len(all.List) != 4 {
		t.Fatalf("全量 n=%d，期望 4", len(all.List))
	}
	onlyOffline, err := NewListApplicationsLogic(t.Context(), s).ListApplications(
		&rpc.ListApplicationsReq{Operator: true, Status: rpc.AppStatus_APP_STATUS_OFFLINE})
	onlyOffline = wantOK(t, onlyOffline, err, "按 OFFLINE 过滤")
	if len(onlyOffline.List) != 0 {
		t.Fatalf("无一条 OFFLINE，却返回 %d 行", len(onlyOffline.List))
	}
}

func TestListApplications_PageSizeAndFilterGates(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListApplicationsReq
		want error
	}{
		{name: "负 ps", in: &rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: -1},
			want: model.ErrInvalidPage},
		{name: "ps 超上限", in: &rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: 51},
			want: model.ErrPsTooLarge},
		{name: "非法游标", in: &rpc.ListApplicationsReq{OwnerMid: testOwner, Cursor: "@@"},
			want: model.ErrInvalidCursor},
		{name: "游标缺位点", in: &rpc.ListApplicationsReq{OwnerMid: testOwner,
			Cursor: "MTIz"}, want: model.ErrInvalidCursor},
		{name: "未定义状态过滤器", in: &rpc.ListApplicationsReq{OwnerMid: testOwner, Status: 9},
			want: errInvalidAppStatusFilter},
		{name: "开发者未给 owner_mid", in: &rpc.ListApplicationsReq{Ps: 10},
			want: model.ErrOwnerRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, s := appFixture(t)
			before := snapshotWrites(db)
			reply, err := NewListApplicationsLogic(t.Context(), s).ListApplications(tc.in)
			wantFail(t, reply, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
		})
	}

	t.Run("ps=0 用配置默认值而不是 1", func(t *testing.T) {
		db, s := appFixture(t)
		// 三行同主数据：只有一行时「裁到 1」这个断言无法失败。
		seedApp(db, testAppID+1, testOwner)
		seedApp(db, testAppID+2, testOwner)
		s.Config.OpenPlatform.PageSize = 1
		reply, err := NewListApplicationsLogic(t.Context(), s).ListApplications(
			&rpc.ListApplicationsReq{OwnerMid: testOwner})
		reply = wantOK(t, reply, err, "默认页大小")
		if len(reply.List) != 1 {
			t.Fatalf("n=%d，期望按配置默认值裁到 1", len(reply.List))
		}
		if !reply.HasMore {
			t.Fatal("三行数据裁到 1 时必须还有下一页")
		}
	})

	t.Run("PageSize 未配置时报错而不是放行全表", func(t *testing.T) {
		_, s := appFixture(t)
		s.Config.OpenPlatform.PageSize = 0
		reply, err := NewListApplicationsLogic(t.Context(), s).ListApplications(
			&rpc.ListApplicationsReq{OwnerMid: testOwner})
		wantFail(t, reply, err, model.ErrInvalidPage, "未配置默认页大小")
	})
}

func TestListApplications_DerivedColumnsUseBatchQueries(t *testing.T) {
	db := newStore()
	for i := int64(0); i < 3; i++ {
		seedApp(db, testAppID+i, testOwner, testScopeR)
		seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	}
	s := newTestSvc(db)

	gBefore := db.count("AppScopes.GrantedByApps")
	sBefore := db.count("Secrets.SummariesByApps")
	reply, err := NewListApplicationsLogic(t.Context(), s).ListApplications(
		&rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: 50})
	reply = wantOK(t, reply, err, "三行列表")
	if len(reply.List) != 3 {
		t.Fatalf("n=%d，期望 3", len(reply.List))
	}
	// 派生列必须批量取：逐行 ListGranted/CountActive 会把一次列表放大成 N 次查询。
	wantCalls(t, db, "AppScopes.GrantedByApps", gBefore, 1, "获批 scope 批量一次")
	wantCalls(t, db, "Secrets.SummariesByApps", sBefore, 1, "密钥概览批量一次")
	for _, a := range reply.List {
		if got := a.GetScopes(); len(got) != 1 || got[0] != testScopeR {
			t.Fatalf("app_id=%d 的 scopes 投影=%v", a.GetAppId(), got)
		}
	}
}
