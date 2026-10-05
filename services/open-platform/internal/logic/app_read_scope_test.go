package logic

// app_read_scope_test.go：应用/scope 读面三法的契约测试
// （ListApplications / ListScopes / GrantApplicationScopes）。
//
// 契约依据：proto:189-202（列表位点与运营全量分支）、proto:263-271（scope 目录与
// granted_state 三值）、proto:273-291（审批的运营身份、reason、幂等键与 rejected 语义）。
//
// 本文件钉住的不变量：
//  1. 列表响应的每个字段都等于入库真值（含派生列 scopes / secret_state），且不含任何凭证材料；
//  2. 游标位点严格有序、页与页不重叠不遗漏，空结果回空数组而不是报错；
//  3. ps 的夹取口径两侧都要钉住：MaxPageSize 本身允许、+1 才拒，未传走 PageSize 默认值；
//  4. 目录读面：停用项在 only_enabled=false 时必须可见（授权页要能解释「已下线」），
//     未指定 app_id 时 granted_state 恒 0，已回收的关系行必须回到 0 而不是「还持有」；
//  5. 审批只认「目录里存在且 enabled=1」的 scope：未知/停用一律进 rejected 且不落库；
//     整次调用的身份门禁是 operator_mid>0（proto:279），不是请求里的任何布尔位；
//  6. 授予/回收都是对 uniq (app_id, scope) 同一行的原地更新：重复审批不产生第二行；
//  7. 回收只处理「确实已获批」的 scope，无效回收整次零副作用且不前移 app.version；
//  8. 失败路径一律 wantNoWrites + 关系行数不变，成功路径响应的 app_version == 入库 version。

import (
	"fmt"
	"strings"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

// 分页夹取口径的两侧都以此为准（与 newTestSvc 的 OpenPlatformConf 同值）。
const (
	testDefaultPS = 20 // config.OpenPlatform.PageSize
	testMaxPS     = 50 // config.OpenPlatform.MaxPageSize
)

// 目录用的另外三个权限点：高风险、写但未声明需用户同意、已停用。
// 命名刻意避开 model.forbiddenScopeTokens 的词根，否则正例会被红线门禁先拦掉。
const (
	scopeHighRisk  = "account.close"
	scopeNoConsent = "draft.write"
	scopeDisabled  = "legacy.read"
)

// appRow 落一行状态/时间可控的应用：seedApp 把 mtime 一律刷成 now，
// 只靠它无法验证「按 (mtime, app_id) 倒序」这条位点语义。
func appRow(db *store, appID, ownerMid int64, name string, status int32, mtime int64) *model.Application {
	app := seedStatusApp(db, appID, ownerMid, status)
	app.Name = name
	app.Mtime = mtime
	app.Ctime = mtime - 100
	return app
}

// scopeFixture：ACTIVE 应用 + 四种形态的权限点目录（读开放 / 写需同意 / 高风险 / 已停用）。
func scopeFixture(t *testing.T, granted ...string) (*store, *svc.ServiceContext) {
	t.Helper()
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	seedWriteScope(db, testScopeW)
	seedScopeFull(db, scopeHighRisk, model.ScopeAccessWrite, model.ScopeRiskHigh, 1, 1)
	seedScopeFull(db, scopeDisabled, model.ScopeAccessRead, model.ScopeRiskLow, 0, 0)
	db.scopes[scopeDisabled].DisableReason = "能力已下线，等待重新评审"
	seedApp(db, testAppID, testOwner, granted...)
	return db, newTestSvc(db)
}

func grantScopesReq(grant, revoke []string) *rpc.GrantApplicationScopesReq {
	return &rpc.GrantApplicationScopesReq{
		AppId: testAppID, Grant: grant, Revoke: revoke,
		OperatorMid: testMid, Reason: "运营审批", IdempotencyKey: "grant-key-1",
	}
}

func callListScopes(t *testing.T, s *svc.ServiceContext, in *rpc.ListScopesReq) *rpc.ListScopesReply {
	t.Helper()
	reply, err := NewListScopesLogic(t.Context(), s).ListScopes(in)
	return wantOK(t, reply, err, "ListScopes")
}

// scopeStates 把目录响应折成 scope -> granted_state，便于逐条断言。
func scopeStates(reply *rpc.ListScopesReply) map[string]int32 {
	out := make(map[string]int32, len(reply.GetList()))
	for _, sc := range reply.GetList() {
		out[sc.GetScope()] = sc.GetGrantedState()
	}
	return out
}

// ---------------------------------------------------------------- 应用列表

func TestListApplications_ProjectionFieldsMatchStoredRows(t *testing.T) {
	db := newStore()
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	seedScopeDir(db, testScopeW, model.ScopeAccessWrite)
	now := nowTS()

	live := appRow(db, testAppID, testOwner, "有生效密钥", model.AppStatusActive, now-10)
	live.Description = "带简介"
	live.RedirectURIs = testRedirect + ",https://alt.example.test/cb"
	live.Version = 7
	// 获批集与密钥都刻意用非默认值，否则投影写反方向也能自证通过。
	db.granted[testAppID] = map[string]bool{testScopeR: true, testScopeW: true}
	liveSec := seedSecret(t, db, testAppID, "plain-list-secret")

	// 应用 ID 一律避开 testApp2：seedStatusApp 是按 app_id 覆盖写，撞号会让两条断言
	// 数到彼此的行数上去，「别人的应用不得混入」这条就白测了。
	revoked := appRow(db, testAppID+100, testOwner, "密钥已全部吊销", model.AppStatusSuspended, now-20)
	hist := seedSecret(t, db, revoked.AppID, "plain-old-secret")
	hist.Status = model.SecretStatusHistory

	unset := appRow(db, testAppID+101, testOwner, "从未签发密钥", model.AppStatusPendingReview, now-30)

	// 别人家的应用：owner 分支一条都看不到，运营分支必须能看到（跨归属全量）。
	foreign := appRow(db, testApp2, testOwner+7, "别人的应用", model.AppStatusActive, now-5)

	s := newTestSvc(db)
	lg := NewListApplicationsLogic(t.Context(), s)

	reply, err := lg.ListApplications(&rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: testMaxPS})
	reply = wantOK(t, reply, err, "owner 列表")
	if len(reply.GetList()) != 3 {
		t.Fatalf("n=%d，期望 3（别人的应用不得混入）", len(reply.GetList()))
	}

	want := []*model.Application{live, revoked, unset} // (mtime, app_id) 倒序
	for i, info := range reply.GetList() {
		row := want[i]
		if info.GetAppId() != row.AppID || info.GetName() != row.Name ||
			info.GetDescription() != row.Description || info.GetOwnerMid() != row.OwnerMid ||
			info.GetStatus() != appStatusToRPC(row.Status) || info.GetVersion() != row.Version ||
			info.GetCtime() != row.Ctime || info.GetMtime() != row.Mtime {
			t.Fatalf("第 %d 行投影与入库真值不符：实得 app_id=%d name=%q status=%d version=%d ctime=%d mtime=%d，期望 %d/%q/%d/%d/%d/%d",
				i, info.GetAppId(), info.GetName(), info.GetStatus(), info.GetVersion(),
				info.GetCtime(), info.GetMtime(),
				row.AppID, row.Name, row.Status, row.Version, row.Ctime, row.Mtime)
		}
	}
	// 获批集来自批量读（op_app_scope.go 的 GrantedByApps：ORDER BY app_id, scope ASC），
	// 因此是字典升序；投影不得重排，否则同一次列表的相邻两行会给出不同的 scope 序。
	if got := reply.GetList()[0].GetScopes(); len(got) != 2 ||
		got[0] != testScopeW || got[1] != testScopeR {
		t.Fatalf("获批 scope 投影=%v，期望按批量读的字典升序两条", got)
	}
	if got := reply.GetList()[0].GetRedirectUris(); len(got) != 2 ||
		got[1] != "https://alt.example.test/cb" {
		t.Fatalf("回调白名单投影=%v，期望按列拆成两条", got)
	}
	if reply.GetList()[1].GetScopes() != nil {
		t.Fatalf("无获批 scope 却回了 %v（空集应为 nil）", reply.GetList()[1].GetScopes())
	}
	if reply.GetList()[0].GetSecretState() != rpc.SecretState_SECRET_STATE_CONFIGURED {
		t.Fatal("存在生效密钥却未回 CONFIGURED")
	}
	if reply.GetList()[0].GetSecretRotatedAt() != liveSec.Ctime {
		t.Fatalf("secret_rotated_at=%d 与密钥行 ctime=%d 不一致",
			reply.GetList()[0].GetSecretRotatedAt(), liveSec.Ctime)
	}
	if reply.GetList()[1].GetSecretState() != rpc.SecretState_SECRET_STATE_REVOKED {
		t.Fatal("密钥全部置历史却未回 REVOKED（客户端会误以为还能签名）")
	}
	if reply.GetList()[2].GetSecretState() != rpc.SecretState_SECRET_STATE_UNSET {
		t.Fatal("从未签发密钥却回了非 UNSET 状态")
	}
	// 脱敏：列表响应不得出现明文、salt 或入库哈希（它们合起来就是可验证的凭证材料）。
	mustNoPlaintextInReply(t, reply, "owner 列表",
		"plain-list-secret", "plain-old-secret", liveSec.Salt, liveSec.Hash, hist.Salt, hist.Hash)

	all, err := lg.ListApplications(&rpc.ListApplicationsReq{Operator: true, Ps: testMaxPS})
	all = wantOK(t, all, err, "运营全量")
	if len(all.GetList()) != 4 {
		t.Fatalf("运营全量 n=%d，期望 4", len(all.GetList()))
	}
	if all.GetList()[0].GetAppId() != foreign.AppID {
		t.Fatalf("运营全量未按 (mtime, app_id) 倒序：首行 app_id=%d，期望 %d",
			all.GetList()[0].GetAppId(), foreign.AppID)
	}

	// 状态过滤 + 跨归属：只回 SUSPENDED 的那一条。
	onlySuspended, err := lg.ListApplications(&rpc.ListApplicationsReq{
		Operator: true, Status: rpc.AppStatus_APP_STATUS_SUSPENDED, Ps: testMaxPS})
	onlySuspended = wantOK(t, onlySuspended, err, "按 SUSPENDED 过滤")
	if len(onlySuspended.GetList()) != 1 || onlySuspended.GetList()[0].GetAppId() != revoked.AppID {
		t.Fatalf("状态过滤结果=%+v，期望只有 app_id=%d", onlySuspended.GetList(), revoked.AppID)
	}

	// 空结果：空数组 + 空游标 + has_more=false，而不是报错或留下上一轮位点。
	empty, err := lg.ListApplications(&rpc.ListApplicationsReq{OwnerMid: testOwner + 999, Ps: 10})
	empty = wantOK(t, empty, err, "无数据 owner")
	if len(empty.GetList()) != 0 || empty.GetNextCursor() != "" || empty.GetHasMore() {
		t.Fatalf("空结果形态：n=%d cursor=%q has_more=%t", len(empty.GetList()),
			empty.GetNextCursor(), empty.GetHasMore())
	}
}

func TestListApplications_CursorWalkIsOrderedAndDisjoint(t *testing.T) {
	db := newStore()
	now := nowTS()
	// 五行同一 owner，mtime 两两错开；末两行刻意同 mtime，用 app_id 作二级位点键。
	base := testAppID + 100 // 与 testApp2 错开，见上面的撞号说明
	for i := 0; i < 3; i++ {
		appRow(db, base+int64(i), testOwner, fmt.Sprintf("app-%d", i),
			model.AppStatusActive, now-int64(100*(i+1)))
	}
	tieA, tieB := base+10, base+11
	for _, id := range []int64{tieA, tieB} {
		appRow(db, id, testOwner, fmt.Sprintf("tie-%d", id), model.AppStatusActive, now-500)
	}
	// 另一个 owner 插在中间（mtime 更大），验证翻页不会串门。
	appRow(db, testApp2, testOwner+1, "别人家", model.AppStatusActive, now-50)
	s := newTestSvc(db)
	lg := NewListApplicationsLogic(t.Context(), s)

	var walk []int64
	cursor := ""
	for page := 0; page < 10; page++ {
		reply, err := lg.ListApplications(&rpc.ListApplicationsReq{
			OwnerMid: testOwner, Ps: 2, Cursor: cursor})
		reply = wantOK(t, reply, err, fmt.Sprintf("第 %d 页", page+1))
		if len(reply.GetList()) > 2 {
			t.Fatalf("第 %d 页 n=%d 超过 ps=2", page+1, len(reply.GetList()))
		}
		for _, info := range reply.GetList() {
			if info.GetOwnerMid() != testOwner {
				t.Fatalf("翻页过程混入他人应用 app_id=%d", info.GetAppId())
			}
			walk = append(walk, info.GetAppId())
		}
		if !reply.GetHasMore() {
			break
		}
		if reply.GetNextCursor() == "" {
			t.Fatal("has_more=true 却不给位点，调用方无法续页")
		}
		cursor = reply.GetNextCursor()
	}
	if len(walk) != 5 {
		t.Fatalf("翻页共取 %d 行，期望 5 行（有遗漏或翻不完）", len(walk))
	}
	seen := map[int64]bool{}
	for _, id := range walk {
		if seen[id] {
			t.Fatalf("app_id=%d 在两页里重复出现（游标位点语义破了）", id)
		}
		seen[id] = true
	}
	// 期望序：mtime 倒序，同 mtime 时 app_id 倒序。
	want := []int64{base, base + 1, base + 2, tieB, tieA}
	for i := range want {
		if walk[i] != want[i] {
			t.Fatalf("第 %d 行 app_id=%d，期望 %d（排序不稳定会让上游短缓存拿到错页）",
				i, walk[i], want[i])
		}
	}
}

func TestListApplications_PageSizeClampFollowsConfig(t *testing.T) {
	db := newStore()
	now := nowTS()
	// 21 行：正好跨过 PageSize=20 这条默认值边界。
	for i := 0; i < 21; i++ {
		appRow(db, testAppID+int64(i), testOwner, fmt.Sprintf("app-%02d", i),
			model.AppStatusActive, now-int64(i))
	}
	s := newTestSvc(db)
	lg := NewListApplicationsLogic(t.Context(), s)
	before := snapshotWrites(db)

	// 未传 ps 走配置默认值 20（既不是 1、也不是「不限」）。
	def, err := lg.ListApplications(&rpc.ListApplicationsReq{OwnerMid: testOwner})
	def = wantOK(t, def, err, "默认页大小")
	if len(def.GetList()) != testDefaultPS || !def.GetHasMore() {
		t.Fatalf("默认页 n=%d has_more=%t，期望 %d/true",
			len(def.GetList()), def.GetHasMore(), testDefaultPS)
	}

	// 上限本身允许、+1 才拒：只测一侧的话 off-by-one 能悄悄通过。
	edge, err := lg.ListApplications(&rpc.ListApplicationsReq{Operator: true, Ps: testMaxPS})
	edge = wantOK(t, edge, err, "ps 取上限")
	if len(edge.GetList()) != 21 {
		t.Fatalf("ps=%d 时 n=%d，期望全量 21", testMaxPS, len(edge.GetList()))
	}
	over, err := lg.ListApplications(&rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: testMaxPS + 1})
	wantFail(t, over, err, model.ErrPsTooLarge, "ps 超上限")
	neg, err := lg.ListApplications(&rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: -1})
	wantFail(t, neg, err, model.ErrInvalidPage, "ps 负数")

	// 运营分支不要求 owner_mid（全量扫描的身份位由请求里的 operator 承载，见
	// listapplicationslogic.go:48-50 的注释）；开发者分支必须给 owner_mid，
	// 否则「猜一个 owner」就成了合法查询。
	noOwner, err := lg.ListApplications(&rpc.ListApplicationsReq{Ps: 10})
	wantFail(t, noOwner, err, model.ErrOwnerRequired, "开发者分支缺 owner_mid")
	wantNoWrites(t, db, before, "列表读面零副作用")
}

func TestListApplications_FailClosedOnDependency(t *testing.T) {
	cases := []struct {
		name     string
		fail     string
		operator bool
	}{
		{name: "主查询失败", fail: "Apps.ListByOwner"},
		{name: "运营全量失败", fail: "Apps.ListAll", operator: true},
		{name: "获批 scope 批量读失败", fail: "AppScopes.GrantedByApps"},
		{name: "密钥概览批量读失败", fail: "Secrets.SummariesByApps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, s := scopeFixture(t, testScopeR)
			before := snapshotWrites(db)
			db.failOn(tc.fail, errFakeDown)
			in := &rpc.ListApplicationsReq{OwnerMid: testOwner, Ps: 10, Operator: tc.operator}
			reply, err := NewListApplicationsLogic(t.Context(), s).ListApplications(in)
			if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
				t.Fatalf("%s 必须整次失败，实际 err=%v", tc.fail, err)
			}
			if reply != nil {
				t.Fatalf("故障路径不得带回缺派生列的半份列表：%+v", reply)
			}
			wantNoWrites(t, db, before, tc.fail)
		})
	}
}

// ---------------------------------------------------------------- scope 目录

func TestListScopes_CatalogVisibilityAndStableOrder(t *testing.T) {
	db, s := scopeFixture(t, testScopeR)
	// 写权限但未声明需要用户同意：目录读面照实回声明，不替运营「美化」成需要同意。
	seedScopeFull(db, scopeNoConsent, model.ScopeAccessWrite, model.ScopeRiskMedium, 0, 1)

	full := callListScopes(t, s, &rpc.ListScopesReq{})
	wantOrder := []string{scopeHighRisk, scopeNoConsent, scopeDisabled, testScopeW, testScopeR}
	if len(full.GetList()) != len(wantOrder) {
		t.Fatalf("全量目录 n=%d，期望 %d", len(full.GetList()), len(wantOrder))
	}
	for i, sc := range full.GetList() {
		if sc.GetScope() != wantOrder[i] {
			t.Fatalf("目录第 %d 项=%s，期望 %s（排序不稳定会让上游短缓存永不命中）",
				i, sc.GetScope(), wantOrder[i])
		}
		row := db.scopes[sc.GetScope()]
		if sc.GetDisplayName() != row.DisplayName ||
			sc.GetAccess() != rpc.ScopeAccess(row.Access) ||
			sc.GetRiskLevel() != rpc.ScopeRiskLevel(row.RiskLevel) ||
			sc.GetRequiresUserConsent() != (row.RequiresUserConsent == 1) ||
			sc.GetEnabled() != (row.Enabled == 1) || sc.GetReason() != row.DisableReason {
			t.Fatalf("%s 投影与目录行不符：实得 display=%q access=%d risk=%d consent=%t enabled=%t reason=%q",
				sc.GetScope(), sc.GetDisplayName(), sc.GetAccess(), sc.GetRiskLevel(),
				sc.GetRequiresUserConsent(), sc.GetEnabled(), sc.GetReason())
		}
	}
	// 停用项必须可见且带原因：授权页要能解释「这个能力已下线」。
	disabled := full.GetList()[2]
	if disabled.GetScope() != scopeDisabled || disabled.GetEnabled() ||
		disabled.GetReason() != "能力已下线，等待重新评审" {
		t.Fatalf("停用项形态异常：%+v", disabled)
	}

	enabled := callListScopes(t, s, &rpc.ListScopesReq{OnlyEnabled: true})
	if len(enabled.GetList()) != 4 {
		t.Fatalf("only_enabled 目录 n=%d，期望 4", len(enabled.GetList()))
	}
	for _, sc := range enabled.GetList() {
		if sc.GetScope() == scopeDisabled {
			t.Fatal("only_enabled=true 仍返回停用项")
		}
		if !sc.GetEnabled() {
			t.Fatalf("%s 在 only_enabled 结果里 enabled=false", sc.GetScope())
		}
	}
	// 同样输入两次调用逐位相同（结果会被上游短缓存）。
	again := callListScopes(t, s, &rpc.ListScopesReq{})
	for i := range full.GetList() {
		if full.GetList()[i].GetScope() != again.GetList()[i].GetScope() {
			t.Fatalf("第 %d 项两次调用不同：%s vs %s", i,
				full.GetList()[i].GetScope(), again.GetList()[i].GetScope())
		}
	}
}

func TestListScopes_GrantedStateReflectsApprovalRows(t *testing.T) {
	db, s := scopeFixture(t, testScopeR) // video.read 已获批
	// 三值断言需要「待审批」这一档，因此目录里得有它；scopeFixture 只播三种形态。
	seedScopeFull(db, scopeNoConsent, model.ScopeAccessWrite, model.ScopeRiskMedium, 0, 1)
	now := nowTS()
	db.appScopeRows(testAppID)[scopeNoConsent] = &model.AppScope{
		ID: db.next("appscope"), AppID: testAppID, Scope: scopeNoConsent,
		State: model.AppScopePending, RequestedBy: testOwner, Ctime: now, Mtime: now}
	// 已回收：审计事实在关系行里，但对外必须回到「未申请」，否则开发者无法重新申请。
	db.appScopeRows(testAppID)[scopeDisabled] = &model.AppScope{
		ID: db.next("appscope"), AppID: testAppID, Scope: scopeDisabled,
		State: model.AppScopeRevoked, Operator: testMid, Reason: "违规回收", Ctime: now, Mtime: now}

	byApp := callListScopes(t, s, &rpc.ListScopesReq{AppId: testAppID, OnlyEnabled: true})
	states := scopeStates(byApp)
	want := map[string]int32{testScopeR: 2, scopeNoConsent: 1, scopeHighRisk: 0, testScopeW: 0}
	for sc, exp := range want {
		got, ok := states[sc]
		if !ok {
			t.Fatalf("目录缺 scope %s", sc)
		}
		if got != exp {
			t.Fatalf("%s 的 granted_state=%d，期望 %d", sc, got, exp)
		}
	}
	if _, ok := states[scopeDisabled]; ok {
		t.Fatal("only_enabled=true 时停用项仍出现")
	}

	// 不带 app_id：目录对「哪个应用获批了什么」保持中立，全部回 0。
	neutral := callListScopes(t, s, &rpc.ListScopesReq{OnlyEnabled: true})
	for _, sc := range neutral.GetList() {
		if sc.GetGrantedState() != 0 {
			t.Fatalf("%s 未指定 app_id 却回了获批状态 %d", sc.GetScope(), sc.GetGrantedState())
		}
	}

	before := snapshotWrites(db)
	lg := NewListScopesLogic(t.Context(), s)
	// 应用不存在必须报错：回空列表会被授权页渲染成「该应用无权限点」，属于误导。
	reply, err := lg.ListScopes(&rpc.ListScopesReq{AppId: 555})
	wantFail(t, reply, err, model.ErrAppNotFound, "应用不存在")
	neg, err := lg.ListScopes(&rpc.ListScopesReq{AppId: -1})
	wantFail(t, neg, err, model.ErrInvalidAppID, "负的 app_id")
	wantNoWrites(t, db, before, "scope 目录失败路径")
}

func TestListScopes_EmptyCatalogAndForbiddenCategory(t *testing.T) {
	t.Run("目录为空回空数组而不是报错", func(t *testing.T) {
		db, s := scopeFixture(t)
		for k := range db.scopes {
			delete(db.scopes, k)
		}
		reply, err := NewListScopesLogic(t.Context(), s).ListScopes(&rpc.ListScopesReq{})
		reply = wantOK(t, reply, err, "空目录")
		if len(reply.GetList()) != 0 {
			t.Fatalf("空目录却回了 %d 项", len(reply.GetList()))
		}
	})

	t.Run("红线类目让整次调用失败", func(t *testing.T) {
		db, s := scopeFixture(t)
		// 有人手工改表塞进未开放类目（AGENTS.md §1）：整次失败而不是偷偷过滤那一条。
		db.scopes["member.read"] = &model.Scope{Scope: "member.read", DisplayName: "会员",
			Access: model.ScopeAccessRead, RiskLevel: model.ScopeRiskLow, Enabled: 1}
		before := snapshotWrites(db)
		reply, err := NewListScopesLogic(t.Context(), s).ListScopes(&rpc.ListScopesReq{})
		wantFail(t, reply, err, model.ErrForbiddenScopeCategory, "目录含未开放类目")
		wantNoWrites(t, db, before, "目录含未开放类目")
	})
}

// ---------------------------------------------------------------- scope 审批

func TestGrantApplicationScopes_GrantsOnlyEnabledCatalogScopes(t *testing.T) {
	db, s := scopeFixture(t)
	app := db.apps[testAppID]
	before := snapshotWrites(db)

	// 混合批次：一条开放、一条目录里没有、一条已停用。
	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq([]string{testScopeR, "nope.read", scopeDisabled}, nil))
	reply = wantOK(t, reply, err, "混合批次审批")

	// grantSet 已升序去重，因此 accepted/rejected 的顺序都是确定的。
	if len(reply.GetGranted()) != 1 || reply.GetGranted()[0] != testScopeR {
		t.Fatalf("granted=%v，期望只有 %s", reply.GetGranted(), testScopeR)
	}
	if want := []string{scopeDisabled, "nope.read"}; !equalStrings(reply.GetRejected(), want) {
		t.Fatalf("rejected=%v，期望 %v（未知与停用都必须显式回给运营）", reply.GetRejected(), want)
	}
	if reply.GetReplayed() {
		t.Fatal("首次审批不得标记为重放")
	}

	row := db.appscopes[testAppID][testScopeR]
	if row == nil {
		t.Fatal("已授予的 scope 没落审批关系行")
	}
	if row.State != model.AppScopeGranted || row.Operator != testMid || row.Reason != "运营审批" {
		t.Fatalf("关系行审计列不符真值：state=%d operator=%d reason=%q",
			row.State, row.Operator, row.Reason)
	}
	if !db.granted[testAppID][testScopeR] {
		t.Fatal("获批视图未同步，Introspect 侧会立刻读不到该 scope")
	}
	// 被拒的两条不得留下任何痕迹：一半落库的审批结论无法与工单对账。
	for _, sc := range []string{scopeDisabled, "nope.read"} {
		if _, ok := db.appscopes[testAppID][sc]; ok {
			t.Fatalf("被拒 scope %s 仍落了关系行", sc)
		}
		if db.granted[testAppID][sc] {
			t.Fatalf("被拒 scope %s 出现在获批视图里", sc)
		}
	}
	// 回归护栏：app_version 必须等于入库 version（下游以它作投影失效依据）。
	if reply.GetAppVersion() != app.Version {
		t.Fatalf("响应 app_version=%d 与入库 version=%d 不一致", reply.GetAppVersion(), app.Version)
	}
	if app.Version != 2 {
		t.Fatalf("审批后 version=%d，期望从 1 前移到 2", app.Version)
	}
	wantCalls(t, db, "DB.TransactCtx", before["DB.TransactCtx"], 1, "审批走一个事务")
	wantCalls(t, db, "AppScopes.Grant", before["AppScopes.Grant"], 1, "授予写一次")
	wantCalls(t, db, "AppScopes.Revoke", before["AppScopes.Revoke"], 1, "回收算子同事务内调用")
	wantCalls(t, db, "Apps.NextVersion", before["Apps.NextVersion"], 1, "版本前移一次")
}

func TestGrantApplicationScopes_AllRejectedWritesNothing(t *testing.T) {
	db, s := scopeFixture(t)
	before := snapshotWrites(db)

	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq([]string{scopeDisabled, "nope.read"}, nil))
	reply = wantOK(t, reply, err, "全部被目录拒掉")
	if len(reply.GetGranted()) != 0 || len(reply.GetRejected()) != 2 {
		t.Fatalf("granted=%v rejected=%v", reply.GetGranted(), reply.GetRejected())
	}
	// 全拒时既不写关系行也不前移版本：版本前移会让既有校验链路白白冷启动一次。
	wantNoWrites(t, db, before, "全部被拒")
	if reply.GetAppVersion() != db.apps[testAppID].Version || db.apps[testAppID].Version != 1 {
		t.Fatalf("version=%d 响应=%d，期望保持 1", db.apps[testAppID].Version, reply.GetAppVersion())
	}
	if len(db.appscopes[testAppID]) != 0 {
		t.Fatalf("被拒批次却落了 %d 行关系", len(db.appscopes[testAppID]))
	}
}

func TestGrantApplicationScopes_RevokeCascadesToGrantsAndTokens(t *testing.T) {
	db, s := scopeFixture(t, testScopeR, testScopeW)
	grant := seedGrant(db, testAppID, testMid, []string{testScopeR, testScopeW}, 1)
	_, _, tok := seedToken(t, db, grant, []string{testScopeR, testScopeW}, 10, 3600, 2592000)
	// 另一个应用持有同名 scope：定点撤销必须只命中目标应用。
	other := seedApp(db, testApp2, testOwner, testScopeW)
	otherGrant := seedGrant(db, testApp2, other.OwnerMid, []string{testScopeW}, 1)
	_, _, otherTok := seedToken(t, db, otherGrant, []string{testScopeW}, 10, 3600, 2592000)
	before := snapshotWrites(db)

	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq(nil, []string{testScopeW}))
	reply = wantOK(t, reply, err, "回收一个已获批 scope")
	if !equalStrings(reply.GetRevoked(), []string{testScopeW}) {
		t.Fatalf("revoked=%v，期望只有 %s", reply.GetRevoked(), testScopeW)
	}

	// 回收是集合化 UPDATE（一次覆盖全部受影响行），不是逐 grant 遍历：
	// 逐条遍历会让「回收立即生效」的时间窗随数据量线性变长。
	wantCalls(t, db, "Tokens.RevokeByAppWithScopes", before["Tokens.RevokeByAppWithScopes"], 1,
		"token 定点撤销一次")
	wantCalls(t, db, "Grants.RevokeByAppWithScopes", before["Grants.RevokeByAppWithScopes"], 1,
		"grant 定点撤销一次")

	if tok.State != model.TokenStateRevoked {
		t.Fatalf("命中被回收 scope 的 token state=%d，期望 REVOKED", tok.State)
	}
	if otherTok.State != model.TokenStateActive {
		t.Fatal("回收波及了另一个应用的 token")
	}
	if grant.Status != model.GrantStatusRevoked || grant.RevokedAt == 0 {
		t.Fatalf("grant 撤销位点未落：status=%d revoked_at=%d", grant.Status, grant.RevokedAt)
	}
	if otherGrant.RevokedAt != 0 {
		t.Fatal("另一个应用的 grant 被连带撤销")
	}
	revokedRow := db.appscopes[testAppID][testScopeW]
	if revokedRow.State != model.AppScopeRevoked || revokedRow.Operator != testMid ||
		revokedRow.Reason != "运营审批" {
		t.Fatalf("回收关系行未保留审计：state=%d operator=%d reason=%q",
			revokedRow.State, revokedRow.Operator, revokedRow.Reason)
	}
	// 未被回收的 scope 必须还在：整应用一刀切等于把回收做成了停用应用。
	if !db.granted[testAppID][testScopeR] {
		t.Fatal("保留 scope 却从获批视图消失")
	}
	if db.granted[testAppID][testScopeW] {
		t.Fatal("已回收 scope 仍在获批视图里（回收不会立即生效）")
	}
	if reply.GetAppVersion() != db.apps[testAppID].Version {
		t.Fatalf("响应 app_version=%d 与入库 version=%d 不一致",
			reply.GetAppVersion(), db.apps[testAppID].Version)
	}
}

func TestGrantApplicationScopes_RevokeNeverGrantedIsSideEffectFree(t *testing.T) {
	db, s := scopeFixture(t, testScopeR)
	grant := seedGrant(db, testAppID, testMid, []string{testScopeR}, 1)
	_, _, tok := seedToken(t, db, grant, []string{testScopeR}, 10, 3600, 2592000)
	before := snapshotWrites(db)

	// 回收一个从未获批的 scope：不破坏任何东西，但也不能静默「成功」。
	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq(nil, []string{testScopeW}))
	reply = wantOK(t, reply, err, "无效回收")
	if len(reply.GetRevoked()) != 0 {
		t.Fatalf("revoked=%v，期望空（没有对应已获批 scope 的回收不算生效）", reply.GetRevoked())
	}
	wantNoWrites(t, db, before, "无效回收")
	if reply.GetAppVersion() != 1 || db.apps[testAppID].Version != 1 {
		t.Fatalf("无效回收却前移了版本：%d/%d", reply.GetAppVersion(), db.apps[testAppID].Version)
	}
	if tok.State != model.TokenStateActive {
		t.Fatal("无效回收波及了 token")
	}
	if _, ok := db.appscopes[testAppID][testScopeW]; ok {
		t.Fatal("无效回收留下了从未获批过的关系行")
	}
}

func TestGrantApplicationScopes_EffectiveAndIneffectiveRevoke(t *testing.T) {
	db, s := scopeFixture(t, testScopeR)
	// 回收列表里一条已获批、一条从未获批：只有前者进 revoked 回执。
	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq([]string{testScopeW}, []string{testScopeR, "never.applied"}))
	reply = wantOK(t, reply, err, "混合授予与回收")
	if !equalStrings(reply.GetRevoked(), []string{testScopeR}) {
		t.Fatalf("revoked=%v，期望只有 %s", reply.GetRevoked(), testScopeR)
	}
	if !equalStrings(reply.GetGranted(), []string{testScopeW}) {
		t.Fatalf("granted=%v，期望 %s", reply.GetGranted(), testScopeW)
	}
	if db.granted[testAppID][testScopeR] || !db.granted[testAppID][testScopeW] {
		t.Fatalf("获批视图未收敛：%+v", db.granted[testAppID])
	}
	if reply.GetAppVersion() != db.apps[testAppID].Version {
		t.Fatal("响应版本与入库不一致")
	}
}

func TestGrantApplicationScopes_GatesAreSideEffectFree(t *testing.T) {
	many := make([]string, 0, maxScopeCountPerReq+1)
	for i := 0; i <= maxScopeCountPerReq; i++ {
		many = append(many, fmt.Sprintf("scope-%02d.read", i))
	}
	cases := []struct {
		name string
		in   *rpc.GrantApplicationScopesReq
		want error
	}{
		{name: "匿名运营", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{testScopeW}, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrOperatorRequired},
		{name: "负 operator_mid", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{testScopeW}, OperatorMid: -1, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrOperatorRequired},
		{name: "应用不存在", in: &rpc.GrantApplicationScopesReq{AppId: 555,
			Grant: []string{testScopeW}, OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrAppNotFound},
		{name: "app_id 为 0", in: &rpc.GrantApplicationScopesReq{Grant: []string{testScopeW},
			OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"}, want: model.ErrInvalidAppID},
		{name: "审批无原因", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{testScopeW}, OperatorMid: testMid, IdempotencyKey: "k"},
			want: errReasonRequired},
		{name: "原因超长", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{testScopeW}, OperatorMid: testMid, IdempotencyKey: "k",
			Reason: strings.Repeat("因", maxReasonRunes+1)}, want: errReasonTooLong},
		{name: "缺幂等键", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{testScopeW}, OperatorMid: testMid, Reason: "r"},
			want: model.ErrIdempotencyKeyRequired},
		{name: "幂等键超长", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{testScopeW}, OperatorMid: testMid, Reason: "r",
			IdempotencyKey: strings.Repeat("k", maxIdempotencyRunes+1)},
			want: model.ErrIdempotencyKeyRequired},
		{name: "授予与回收都为空", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: errGrantOrRevokeRequired},
		{name: "同一 scope 既授又回", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{testScopeW}, Revoke: []string{testScopeW},
			OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrInvalidStateTransition},
		{name: "scope 字符非法", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{"bad scope"}, OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrScopeUnknown},
		{name: "授予未开放类目", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{"member.read"}, OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrForbiddenScopeCategory},
		{name: "回收未开放类目", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Revoke: []string{"order.read"}, OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrForbiddenScopeCategory},
		{name: "scope 条数超限", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: many, OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrTooManyScopes},
		{name: "高风险与其它同批", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{scopeHighRisk, testScopeW}, OperatorMid: testMid, Reason: "r",
			IdempotencyKey: "k"}, want: errHighRiskScopeBatch},
		{name: "两条高风险同批", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{scopeHighRisk, "account.cancel"}, OperatorMid: testMid, Reason: "r",
			IdempotencyKey: "k"}, want: errHighRiskScopeBatch},
		{name: "写 scope 未声明用户同意", in: &rpc.GrantApplicationScopesReq{AppId: testAppID,
			Grant: []string{scopeNoConsent}, OperatorMid: testMid, Reason: "r", IdempotencyKey: "k"},
			want: model.ErrScopeWriteRequiresConsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, s := scopeFixture(t, testScopeR)
			seedScopeFull(db, scopeNoConsent, model.ScopeAccessWrite, model.ScopeRiskLow, 0, 1)
			seedScopeFull(db, "account.cancel", model.ScopeAccessWrite, model.ScopeRiskHigh, 1, 1)
			before := snapshotWrites(db)
			reply, err := NewGrantApplicationScopesLogic(t.Context(), s).
				GrantApplicationScopes(tc.in)
			wantFail(t, reply, err, tc.want, tc.name)
			wantNoWrites(t, db, before, tc.name)
			if v := db.apps[testAppID].Version; v != 1 {
				t.Fatalf("门禁失败却前移了版本：%d", v)
			}
			if n := len(db.appscopes[testAppID]); n != 1 {
				t.Fatalf("门禁失败却改动了关系行：%d 行（只剩种子那条）", n)
			}
		})
	}
}

func TestGrantApplicationScopes_HighRiskMustBeSolo(t *testing.T) {
	db, s := scopeFixture(t)
	before := snapshotWrites(db)
	// 单条高风险可过：门禁是「不可批量」，不是「高风险一律拒」。
	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq([]string{scopeHighRisk}, nil))
	reply = wantOK(t, reply, err, "单条高风险审批")
	if !equalStrings(reply.GetGranted(), []string{scopeHighRisk}) {
		t.Fatalf("granted=%v", reply.GetGranted())
	}
	wantCalls(t, db, "AppScopes.Grant", before["AppScopes.Grant"], 1, "高风险单条写一次")
	if db.appscopes[testAppID][scopeHighRisk].State != model.AppScopeGranted {
		t.Fatal("高风险 scope 未落获批状态")
	}
}

func TestGrantApplicationScopes_RepeatSameKeyKeepsSingleRow(t *testing.T) {
	db, s := scopeFixture(t)
	lg := NewGrantApplicationScopesLogic(t.Context(), s)
	req := grantScopesReq([]string{testScopeW}, nil)

	first, err := lg.GrantApplicationScopes(req)
	first = wantOK(t, first, err, "首次授予")
	rowID := db.appscopes[testAppID][testScopeW].ID
	v1 := db.apps[testAppID].Version

	// 本包测试的 Cache 恒为 nil（newTestSvc），幂等键按 idem.go 的口径降级为
	// 「按当前状态重算」：结论必须收敛，且绝不允许出现第二行。
	second, err := lg.GrantApplicationScopes(req)
	second = wantOK(t, second, err, "同键重算")
	if second.GetReplayed() {
		t.Fatal("缓存缺失时不该声称命中重放（那是把降级说成命中）")
	}
	rows := db.appscopes[testAppID]
	if len(rows) != 1 {
		t.Fatalf("uniq (app_id, scope) 却出现 %d 行：%+v", len(rows), rows)
	}
	if rows[testScopeW].ID != rowID {
		t.Fatalf("重复授予换了主键（%d → %d），原地 UPDATE 语义破了", rowID, rows[testScopeW].ID)
	}
	if rows[testScopeW].State != model.AppScopeGranted {
		t.Fatal("重复授予把获批状态改掉了")
	}
	if second.GetAppVersion() != db.apps[testAppID].Version || second.GetAppVersion() <= v1 {
		t.Fatalf("重算路径版本推进异常：%d → %d/%d", v1, second.GetAppVersion(),
			db.apps[testAppID].Version)
	}
}

func TestGrantApplicationScopes_ConcurrentCommitKeepsSingleRow(t *testing.T) {
	db, s := scopeFixture(t, testScopeR)
	const racerID = 777
	// 交错：本事务即将写 scope 行时，另一路运营对同一 scope 的批次先提交了。
	db.onHit("AppScopes.Grant", func() {
		now := nowTS()
		db.appScopeRows(testAppID)[testScopeW] = &model.AppScope{
			ID: racerID, AppID: testAppID, Scope: testScopeW, State: model.AppScopeGranted,
			Operator: testMid + 1, Reason: "先提交的批次", Ctime: now, Mtime: now}
	})

	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq([]string{testScopeW}, nil))
	reply = wantOK(t, reply, err, "并发交错授予")

	rows := db.appscopes[testAppID]
	if len(rows) != 2 { // 种子的 video.read + 本次的 video.publish
		t.Fatalf("关系行数=%d，期望 2（并发不得造出第三行）", len(rows))
	}
	if rows[testScopeW].ID != racerID {
		t.Fatalf("唯一键命中后新增了第二行（id=%d），原地更新语义破了", rows[testScopeW].ID)
	}
	// 后写覆盖前写的审计列：最终结论必须与本次请求一致，否则审计追不到人。
	if rows[testScopeW].Operator != testMid || rows[testScopeW].Reason != "运营审批" {
		t.Fatalf("并发后的审计列未收敛：operator=%d reason=%q",
			rows[testScopeW].Operator, rows[testScopeW].Reason)
	}
	if reply.GetAppVersion() != db.apps[testAppID].Version {
		t.Fatal("响应版本与入库不一致")
	}
}

func TestGrantApplicationScopes_TransactionFailureRollsBackGrant(t *testing.T) {
	db, s := scopeFixture(t, testScopeR)
	grant := seedGrant(db, testAppID, testMid, []string{testScopeR}, 1)
	_, _, tok := seedToken(t, db, grant, []string{testScopeR}, 10, 3600, 2592000)
	// 事务里第二个写算子失败：同事务的授予必须整体回滚。
	db.failOn("AppScopes.Revoke", errFakeDown)
	before := snapshotWrites(db)

	reply, err := NewGrantApplicationScopesLogic(t.Context(), s).GrantApplicationScopes(
		grantScopesReq([]string{testScopeW}, nil))
	if err == nil || !strings.Contains(err.Error(), errFakeDown.Error()) {
		t.Fatalf("事务内写失败必须整次报错，实际 err=%v", err)
	}
	if reply != nil {
		t.Fatalf("失败路径不得带回结论：%+v", reply)
	}
	// 计数说「尝试过」，数据说「什么都没留下」：两条一起才排除假绿。
	wantCalls(t, db, "AppScopes.Grant", before["AppScopes.Grant"], 1, "授予算子被调用过")
	if _, ok := db.appscopes[testAppID][testScopeW]; ok {
		t.Fatal("事务回滚后仍留下新授予的关系行")
	}
	if db.granted[testAppID][testScopeW] {
		t.Fatal("事务回滚后获批视图仍含新 scope")
	}
	// 回滚只撤销本次事务的写：种子行必须还在，否则 restore 把库抹平了却看不出来。
	if db.appscopes[testAppID][testScopeR].State != model.AppScopeGranted {
		t.Fatal("回滚把种子审批行也弄丢了")
	}
	if tok.State != model.TokenStateActive {
		t.Fatal("回滚波及了无关 token")
	}
	wantCalls(t, db, "Apps.NextVersion", before["Apps.NextVersion"], 0,
		"事务未提交就前移版本会留下「版本说已变更、审批还是旧的」的假失效")
	if v := db.apps[testAppID].Version; v != 1 {
		t.Fatalf("事务失败后 version=%d，期望 1", v)
	}
}

func TestGrantApplicationScopes_WriteRateLimitBeforeAnyWrite(t *testing.T) {
	db, s := scopeFixture(t)
	// 同一份请求在正常令牌桶下先过一遍：既给出正对照，也证明下面的拒绝只可能来自限流。
	ok, err := NewGrantApplicationScopesLogic(t.Context(), s).
		GrantApplicationScopes(grantScopesReq([]string{testScopeW}, nil))
	ok = wantOK(t, ok, err, "令牌桶正常时同一请求可过")
	if ok.GetAppVersion() != 2 {
		t.Fatalf("正对照版本=%d，期望 2", ok.GetAppVersion())
	}
	before := snapshotWrites(db)

	reply, err := NewGrantApplicationScopesLogic(t.Context(), limitedSvc(db)).
		GrantApplicationScopes(grantScopesReq([]string{testScopeR}, nil))
	wantFail(t, reply, err, model.ErrRateLimited, "审批被限流")
	wantNoWrites(t, db, before, "审批被限流")
	if v := db.apps[testAppID].Version; v != 2 {
		t.Fatalf("限流路径却改动了版本：%d", v)
	}
}

// equalStrings 逐位比较（nil 与空切片都算「无元素」，回执断言不关心底层表示）。
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
