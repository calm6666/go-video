package logic

// RefreshAccessToken 的行为测试：轮换 + 一次性使用 + 重放检测 + scope 只收窄。
//
// 契约依据：proto:328-340「旧 refresh 立即置 ROTATED；旧值再被使用即判定重放并撤销整条
// grant（保守失效）；scope 允许收窄不允许扩大」，proto:39 停用应用一律拒绝。

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

// refreshFixture：ACTIVE 应用 + 已获批两个 scope（读+写）+ 一条同意过的授权 + 一代 token。
func refreshFixture(t *testing.T, scopes []string, consent int8) (
	*store, *svc.ServiceContext, *model.Grant, string, string, *model.Token) {
	t.Helper()
	db := newStore()
	seedApp(db, testAppID, testOwner, scopes...)
	seedScopeDir(db, testScopeR, model.ScopeAccessRead)
	seedScopeDir(db, testScopeW, model.ScopeAccessWrite)
	grant := seedGrant(db, testAppID, testMid, scopes, consent)
	access, refresh, tok := seedToken(t, db, grant, scopes, 10, 3600, 2592000)
	return db, newTestSvc(db), grant, access, refresh, tok
}

func TestRefreshAccessToken_RotatesChainAndKeepsPlaintextOutOfStorage(t *testing.T) {
	db, s, grant, _, refresh, old := refreshFixture(t, []string{testScopeR, testScopeW}, 1)
	before := snapshotWrites(db)

	reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
		RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
	reply = wantOK(t, reply, err, "正常轮换")
	if !reply.Rotated {
		t.Fatalf("Rotated=false，轮换必须显式声明")
	}
	set := reply.Token
	if set == nil || set.AccessToken == "" || set.RefreshToken == "" {
		t.Fatalf("TokenSet 缺明文：%+v", set)
	}
	if set.TokenId == old.TokenID {
		t.Fatalf("回显了旧 token_id")
	}
	if set.GrantId != grant.GrantID {
		t.Fatalf("GrantId=%d，期望 %d", set.GrantId, grant.GrantID)
	}
	// 空 Scope 表示继承旧快照（proto:333）。快照串是规范化的升序集，断言只看成员不看顺序。
	if len(set.Scope) != 2 || !model.ContainsScope(set.Scope, testScopeR) ||
		!model.ContainsScope(set.Scope, testScopeW) {
		t.Fatalf("继承的 scope 快照异常：%v", set.Scope)
	}

	// 旧行：立即 ROTATED（一次性使用），并留下轮换链父指针。
	if db.tokens[old.TokenID].State != model.TokenStateRotated {
		t.Fatalf("旧 refresh 未置 ROTATED：state=%d", db.tokens[old.TokenID].State)
	}
	if db.tokens[old.TokenID].RotatedAt == 0 {
		t.Fatalf("旧行缺 rotated_at，重放线索不可审计")
	}
	// 新行：parent 指向被轮换的旧行（链上任一代可回溯）。
	newRow := db.tokens[set.TokenId]
	if newRow == nil || newRow.ParentTokenID != old.TokenID {
		t.Fatalf("新 token 行未挂上父指针：%+v", newRow)
	}
	if newRow.State != model.TokenStateActive || newRow.GrantType != model.GrantTypeRefreshToken {
		t.Fatalf("新 token 行形态异常：state=%d grant_type=%d", newRow.State, newRow.GrantType)
	}
	// 库里只有哈希：响应里的两个明文都能对上，且明文本身不出现在任何列。
	if newRow.AccessHash != mustHash(t, set.AccessToken) || newRow.RefreshHash != mustHash(t, set.RefreshToken) {
		t.Fatalf("落库哈希与返回明文不匹配：客户端拿到的 token 校验必失败")
	}
	if newRow.AccessSalt == set.AccessToken || newRow.RefreshSalt == set.RefreshToken {
		t.Fatalf("salt 列疑似写入明文")
	}
	// 链头前移：撤销位点/并发防护依赖 current_token_id。
	g := db.grants[grant.GrantID]
	if g.CurrentTokenID != set.TokenId {
		t.Fatalf("grant.current_token_id=%d，期望 %d", g.CurrentTokenID, set.TokenId)
	}
	if g.RotateSeq != 1 {
		t.Fatalf("grant.rotate_seq=%d，期望 1", g.RotateSeq)
	}
	wantCalls(t, db, "Tokens.InsertTx", before["Tokens.InsertTx"], 1, "轮换")
	wantCalls(t, db, "Tokens.MarkRotated", before["Tokens.MarkRotated"], 1, "轮换")
	wantCalls(t, db, "Grants.TouchTokenHead", before["Grants.TouchTokenHead"], 1, "轮换")
	// 旧 access 的定位缓存失效是可选优化，但撤销链不能顺手打 token 表。
	wantCalls(t, db, "Tokens.RevokeByGrant", before["Tokens.RevokeByGrant"], 0, "正常轮换不得撤销 grant")
	wantCalls(t, db, "Grants.MarkRevoked", before["Grants.MarkRevoked"], 0, "正常轮换不得撤销 grant")
}

func TestRefreshAccessToken_ScopeMayOnlyNarrow(t *testing.T) {
	t.Run("收窄成功", func(t *testing.T) {
		db, s, _, _, refresh, _ := refreshFixture(t, []string{testScopeR, testScopeW}, 1)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{
				AppId: testAppID, RefreshToken: refresh, Scope: []string{testScopeR}})
		reply = wantOK(t, reply, err, "收窄")
		if len(reply.Token.Scope) != 1 || reply.Token.Scope[0] != testScopeR {
			t.Fatalf("收窄后 scope=%v", reply.Token.Scope)
		}
		if db.tokens[reply.Token.TokenId].Scope != testScopeR {
			t.Fatalf("落库快照与回显不一致：%q", db.tokens[reply.Token.TokenId].Scope)
		}
	})

	t.Run("扩大被拒", func(t *testing.T) {
		db, s, _, _, refresh, _ := refreshFixture(t, []string{testScopeR}, 1)
		seedScopeDir(db, testScopeW, model.ScopeAccessWrite)
		before := snapshotWrites(db)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{
				AppId: testAppID, RefreshToken: refresh, Scope: []string{testScopeR, testScopeW}})
		wantFail(t, reply, err, model.ErrScopeNarrowingDenied, "扩大 scope")
		wantNoWrites(t, db, before, "扩大 scope")
	})

	t.Run("审批已回收的 scope 不得借刷新复活", func(t *testing.T) {
		db, s, _, _, refresh, old := refreshFixture(t, []string{testScopeR, testScopeW}, 1)
		delete(db.granted[testAppID], testScopeW) // 运营回收了写权限
		before := snapshotWrites(db)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		wantFail(t, reply, err, model.ErrScopeNotGranted, "继承已回收的 scope")
		wantNoWrites(t, db, before, "继承已回收的 scope")
		if db.tokens[old.TokenID].State != model.TokenStateActive {
			t.Fatalf("被拒的刷新把旧行改脏了")
		}
	})

	t.Run("写权限缺用户同意时被拒", func(t *testing.T) {
		// grant.consent_given=0 + 快照含写 scope：轮换不得把「用户没同意过」的写权限续下去。
		db, s, _, _, refresh, _ := refreshFixture(t, []string{testScopeW}, 0)
		before := snapshotWrites(db)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		wantFail(t, reply, err, model.ErrScopeWriteRequiresConsent, "写 scope 缺同意")
		wantNoWrites(t, db, before, "写 scope 缺同意")
	})

	t.Run("非法 scope 标识被拒", func(t *testing.T) {
		db, s, _, _, refresh, _ := refreshFixture(t, []string{testScopeR}, 1)
		before := snapshotWrites(db)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{
				AppId: testAppID, RefreshToken: refresh, Scope: []string{"bad scope"}})
		wantFail(t, reply, err, model.ErrScopeUnknown, "非法 scope")
		wantNoWrites(t, db, before, "非法 scope")
	})
}

// 一次性使用 + 重放可检测：proto:334-336 的保守失效。
func TestRefreshAccessToken_ReuseRevokesWholeGrant(t *testing.T) {
	db, s, grant, access, refresh, old := refreshFixture(t, []string{testScopeR}, 1)
	seedEndpoint(db, testAppID, model.WebhookEventGrantRevoked)
	logic := NewRefreshAccessTokenLogic(context.Background(), s)

	first, err := logic.RefreshAccessToken(&rpc.RefreshAccessTokenReq{
		AppId: testAppID, RefreshToken: refresh})
	first = wantOK(t, first, err, "首次轮换")
	// 用新签发的 refresh 还能继续用。
	second, err := logic.RefreshAccessToken(&rpc.RefreshAccessTokenReq{
		AppId: testAppID, RefreshToken: first.Token.RefreshToken})
	second = wantOK(t, second, err, "用新 refresh 继续轮换")

	before := snapshotWrites(db)
	// 重放已被轮换掉的旧值：唯一安全处置是让整条 grant 失效。
	third, err := logic.RefreshAccessToken(&rpc.RefreshAccessTokenReq{
		AppId: testAppID, RefreshToken: refresh})
	wantFail(t, third, err, model.ErrRefreshReused, "重放旧 refresh")

	g := db.grants[grant.GrantID]
	if g.RevokedAt == 0 || g.Status != model.GrantStatusRevoked {
		t.Fatalf("重放后 grant 位点未前移：revoked_at=%d status=%d", g.RevokedAt, g.Status)
	}
	if g.RevokeOperator != 0 {
		t.Fatalf("系统触发的撤销必须记 operator=0，实际 %d", g.RevokeOperator)
	}
	if g.RevokeReason == "" {
		t.Fatalf("重放处置未留原因，审计断链")
	}
	// 双保险的第一道：整条 grant 上所有行（含 ROTATED 链）都被标记。
	checked := 0
	for _, tok := range db.tokens {
		if tok.GrantID != grant.GrantID {
			continue
		}
		if tok.State != model.TokenStateRevoked {
			t.Fatalf("grant 下 token_id=%d 未被作废：state=%d", tok.TokenID, tok.State)
		}
		checked++
	}
	// 原始那把 + 两次轮换签发的 = 3 行；少于 3 说明上面的循环是空转。
	if checked < 3 || db.tokens[old.TokenID].State != model.TokenStateRevoked {
		t.Fatalf("链上被清除的行数=%d，原始 token_id=%d", checked, old.TokenID)
	}
	wantCalls(t, db, "Grants.MarkRevoked", before["Grants.MarkRevoked"], 1, "重放处置")
	wantCalls(t, db, "Tokens.RevokeByGrant", before["Tokens.RevokeByGrant"], 1, "重放处置")
	// 通知订阅方（GRANT_REVOKED），事件正文只带 ID 与原因，不含凭证。
	if len(db.dels) != 1 {
		t.Fatalf("GRANT_REVOKED 投递任务数=%d，期望 1", len(db.dels))
	}
	if db.dels[0].EventID != "grant-revoked:"+strconv.FormatInt(grant.GrantID, 10) {
		t.Fatalf("事件 ID=%q，期望按 grant 去重", db.dels[0].EventID)
	}
	// 重放处置之后再拿新签的那把 refresh 也必须被拒（整条链已死）。
	after, err := logic.RefreshAccessToken(&rpc.RefreshAccessTokenReq{
		AppId: testAppID, RefreshToken: second.Token.RefreshToken})
	wantFail(t, after, err, model.ErrTokenRevoked, "重放后新 refresh 也不可用")

	// access 侧同样立即失效：Introspect 与 Authorize 共用判定链，位点比对必须命中。
	in := NewIntrospectTokenLogic(context.Background(), s)
	res, err := in.IntrospectToken(&rpc.IntrospectTokenReq{AccessToken: access})
	res = wantOK(t, res, err, "introspect 重放后的旧 access")
	if res.Active || res.DenyReason != denyRevoked {
		t.Fatalf("重放处置后旧 access 仍可用水：active=%t deny=%q", res.Active, res.DenyReason)
	}
}

func TestRefreshAccessToken_RejectsInvalidCredentials(t *testing.T) {
	cases := []struct {
		name  string
		appID int64
		token string
		want  error
		tweak func(db *store, tok *model.Token, grant *model.Grant)
	}{
		{"app_id 非法", 0, "", model.ErrInvalidAppID, nil},
		{"未知 refresh", testAppID, mustPlaintext(t), model.ErrTokenInvalid, nil},
		{"超长 refresh 明文", testAppID, string(make([]byte, credentialBytes*2+2)),
			model.ErrTokenInvalid, nil},
		{"refresh 属于别的应用", testAppID, "", model.ErrTokenInvalid, nil},
		{"已撤销", testAppID, "", model.ErrTokenRevoked,
			func(db *store, tok *model.Token, _ *model.Grant) {
				tok.State = model.TokenStateRevoked
			}},
		{"归档为过期", testAppID, "", model.ErrTokenExpired,
			func(db *store, tok *model.Token, _ *model.Grant) {
				tok.State = model.TokenStateExpired
			}},
		{"时间到点但状态未归档", testAppID, "", model.ErrTokenExpired,
			func(db *store, tok *model.Token, _ *model.Grant) {
				tok.RefreshExpiresAt = nowTS() - 1
			}},
		{"未知状态值按不可用处理", testAppID, "", model.ErrTokenInvalid,
			func(db *store, tok *model.Token, _ *model.Grant) {
				tok.State = 99
			}},
		{"grant 消失（数据不一致）保守拒绝", testAppID, "", model.ErrGrantRevoked,
			func(db *store, _ *model.Token, grant *model.Grant) {
				delete(db.grants, grant.GrantID)
			}},
		{"撤销位点命中", testAppID, "", model.ErrGrantRevoked,
			func(db *store, tok *model.Token, grant *model.Grant) {
				grant.RevokedAt = tok.Ctime // Ctime <= 位点 → 该代之前签发的全部作废
			}},
		{"应用停用", testAppID, "", model.ErrApplicationNotActive,
			func(db *store, _ *model.Token, _ *model.Grant) {
				db.apps[testAppID].Status = model.AppStatusSuspended
			}},
		{"应用下线", testAppID, "", model.ErrApplicationNotActive,
			func(db *store, _ *model.Token, _ *model.Grant) {
				db.apps[testAppID].Status = model.AppStatusOffline
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, s, grant, _, own, tok := refreshFixture(t, []string{testScopeR}, 1)
			// 默认必须拿本次会话自己那把 refresh：状态/位点类用例要先按值反查到同一行，
			// 换一把随机明文会在第 2 步就把后面所有分支短路成 ErrTokenInvalid（假绿）。
			plain := own
			if c.name == "refresh 属于别的应用" {
				other := seedApp(db, testApp2, testOwner, testScopeR)
				g2 := seedGrant(db, other.AppID, testMid, []string{testScopeR}, 1)
				_, plain, _ = seedToken(t, db, g2, []string{testScopeR}, 5, 3600, 2592000)
			}
			if c.tweak != nil {
				c.tweak(db, tok, grant)
			}
			if c.token != "" {
				plain = c.token
			}
			before := snapshotWrites(db)
			reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
				RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: c.appID, RefreshToken: plain})
			wantFail(t, reply, err, c.want, c.name)
			wantNoWrites(t, db, before, c.name)
		})
	}
}

func TestRefreshAccessToken_ConcurrentRotationRollsBack(t *testing.T) {
	t.Run("同值二次到达优先判重放", func(t *testing.T) {
		db, s, _, _, refresh, tok := refreshFixture(t, []string{testScopeR}, 1)
		// 并发对手已把这一行轮换掉：本把 refresh 再次到达时，重放证据优先于 CAS 结论。
		db.tokens[tok.TokenID].State = model.TokenStateRotated
		db.tokens[tok.TokenID].RotatedAt = nowTS() - 1
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		wantFail(t, reply, err, model.ErrRefreshReused, "重放路径优先于 CAS")
	})

	t.Run("链头已被前移", func(t *testing.T) {
		db, s, grant, _, refresh, tok := refreshFixture(t, []string{testScopeR}, 1)
		// 交错必须发生在「logic 步骤 4 读到链头之后、事务内 CAS 之前」：
		// 开跑前直接改库里的手柄没有区分度（logic 读到的就是改后的值，CAS 自然命中）。
		db.onHit("Tokens.MarkRotated", func() {
			db.grants[grant.GrantID].CurrentTokenID = 555555
		})
		_, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		if err == nil || !errors.Is(err, model.ErrConcurrentUpdate) {
			t.Fatalf("并发防护失效：TouchTokenHead applied=false 被当成成功（err=%v）", err)
		}
		// 回滚必须彻底：旧行仍是 ACTIVE、链头回到原值，且没有多出第二代 token。
		if db.tokens[tok.TokenID].State != model.TokenStateActive {
			t.Fatalf("回滚不彻底：旧行 state=%d", db.tokens[tok.TokenID].State)
		}
		if len(db.tokens) != 1 {
			t.Fatalf("回滚不彻底：token 行数=%d", len(db.tokens))
		}
		if db.grants[grant.GrantID].CurrentTokenID == 555555 {
			t.Fatalf("回滚不彻底：并发前移的链头没被还原")
		}
	})

	t.Run("缺 pepper 时 fail closed", func(t *testing.T) {
		db, s, _, _, refresh, _ := refreshFixture(t, []string{testScopeR}, 1)
		s.Config.Security.CredentialPepper = ""
		before := snapshotWrites(db)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		wantFail(t, reply, err, model.ErrSecretVerificationUnavailable, "缺 pepper")
		wantNoWrites(t, db, before, "缺 pepper")
	})

	t.Run("TTL 未配置时不签发异常凭证", func(t *testing.T) {
		db, s, _, _, refresh, tok := refreshFixture(t, []string{testScopeR}, 1)
		s.Config.OpenPlatform.AccessTokenTTLSeconds = 0
		before := snapshotWrites(db)
		_, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		if err == nil {
			t.Fatalf("TTL 未配置时必须回错，不能签发「立刻过期」的凭证")
		}
		wantNoWrites(t, db, before, "TTL 未配置")
		if db.tokens[tok.TokenID].State != model.TokenStateActive {
			t.Fatalf("被拒的签发改脏了旧行")
		}
	})

	t.Run("写令牌耗尽时零副作用", func(t *testing.T) {
		db, _, _, _, refresh, tok := refreshFixture(t, []string{testScopeR}, 1)
		s := limitedSvc(db)
		before := snapshotWrites(db)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		wantFail(t, reply, err, model.ErrRateLimited, "限流")
		wantNoWrites(t, db, before, "限流")
		if db.tokens[tok.TokenID].State != model.TokenStateActive {
			t.Fatalf("被限流的请求改脏了旧行")
		}
	})

	t.Run("重放处置失败不改变拒绝结论", func(t *testing.T) {
		db, s, _, _, refresh, tok := refreshFixture(t, []string{testScopeR}, 1)
		tok.State = model.TokenStateRotated
		tok.RotatedAt = nowTS() - 5
		db.failOn("Grants.MarkRevoked", errFakeDown)
		reply, err := NewRefreshAccessTokenLogic(context.Background(), s).
			RefreshAccessToken(&rpc.RefreshAccessTokenReq{AppId: testAppID, RefreshToken: refresh})
		wantFail(t, reply, err, model.ErrRefreshReused, "处置失败仍必须拒绝")
	})
}
