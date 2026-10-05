package logic

// logoutlogic_test.go 覆盖 Logout（repository/login.go:376-386 → logic/logoutlogic.go:28-34）。
//
// 被测判定链：按 token 查库（**只查库，不读缓存**）→ 命中才 Revoke(token) →
// 删 ak_/rk_ 两条缓存 → 无论吊销成败一律返回成功。
//
// 钉住的关键事实：
//   - 未知/空 token 是「成功 + 零副作用」，调用方无法区分「登出了」与「什么都没做」；
//   - Revoke 的错误被 `_ =` 吞掉（login.go:382）→ 库里的行仍是 status=0，
//     缓存虽然删了，下一次 TokenInfo 回源会把会话**重新灌回缓存**，token 原地复活（缺口 6）；
//   - delSessionCache 两次 Del 的错误也被吞（login.go:214-217）→ 缓存里的有效会话
//     继续短路鉴权 600 秒（缺口 7）；
//   - 缓存里存在但库里没有的 token（RenewToken 吞掉 Del 后必然出现的中间态）
//     登出是彻底的空操作，登不掉；
//   - 登出不写任何登录日志（缺口 9 系列）。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callLogout(t *testing.T, e *env, token string) (*rpc.DelCacheReply, error) {
	t.Helper()
	return NewLogoutLogic(context.Background(), e.svcCtx).Logout(&rpc.LogoutReq{Token: token})
}

// TestLogoutHappyPath 正常登出：四步顺序（先库后缓存、先吊销后删缓存）、
// 同一行就地吊销、两条缓存都清、登出后 token 与 refresh 双双失效。
func TestLogoutHappyPath(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	row := sessionOf(t, e, sess.Token)
	now := nowUnix()

	reply, err := callLogout(t, e, sess.Token)
	wantNoErr(t, "登出", err)
	if reply == nil {
		t.Fatalf("登出成功必须返回非 nil reply")
	}
	wantOps(t, "登出调用序列", e.ops(0), []string{
		"session.FindByToken:" + sess.Token,
		"session.Revoke:" + sess.Token,
		"cache.Del:ak_" + sess.Token,
		"cache.Del:rk_" + sess.RefreshToken,
	})
	// 吊销判定不信任缓存：整条序列里一次 cache.GetJSON 都没有
	wantNoOpsWith(t, "登出序列", e.ops(0), "cache.GetJSON")

	// 就地吊销，不删行、不新增行
	wantEQ(t, "登出不得删除会话行", "session 行数", st.session.count(), 1)
	after := st.session.get(row.ID)
	if after == nil {
		t.Fatalf("登出把会话行删掉了，account_session 必须只改 status")
	}
	wantEQ(t, "登出后的 status", "status", after.Status, model.SessionStatusRevoked)
	wantEQ(t, "登出只改 status/mtime", "mid", after.Mid, mid)
	wantEQ(t, "登出只改 status/mtime", "token", after.Token, sess.Token)
	wantEQ(t, "登出只改 status/mtime", "refresh_token", after.RefreshToken, sess.RefreshToken)
	wantEQ(t, "登出只改 status/mtime", "expires", after.Expires, row.Expires)
	wantTSWindow(t, "登出刷新 mtime", "mtime", after.MTime, now, nowUnix()+5)

	// 两条缓存都清掉，且没有回填
	if st.cache.has("ak_"+sess.Token) || st.cache.has("rk_"+sess.RefreshToken) {
		t.Errorf("登出后 ak_/rk_ 缓存仍在，必须都被删除")
	}
	st.log.reset()
	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "登出后校验 token", err)
	wantEQ(t, "登出后未登录", "is_login", info.IsLogin, false)
	wantOps(t, "登出后鉴权序列（回源一次、不回填）", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
	})
	// 不写任何审计（缺口 9）
	wantNoOpsWith(t, "登出链路", e.ops(0), "loginlog.Add")
	wantEQ(t, "登出不落登录日志", "loginlog 行数", st.loginLog.count(), 1) // 只剩 login() 那条
}

// TestLogoutUnknownTokenIsSilentSuccess 「登出一个不存在的 token」是成功 + 只有一次查库：
// 既没有任何吊销，也没有任何缓存动作，调用方拿到的响应与真登出完全一致。
func TestLogoutUnknownTokenIsSilentSuccess(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.log.reset()

	reply, err := callLogout(t, e, "ffffffffffffffffffffffffffffffff")
	wantNoErr(t, "未知 token 登出（不存在不算错误）", err)
	if reply == nil {
		t.Fatalf("未知 token 也必须返回非 nil reply")
	}
	wantOps(t, "未知 token 的调用序列", e.ops(0), []string{"session.FindByToken:ffffffffffffffffffffffffffffffff"})
	wantNoOpsWith(t, "未知 token 路径", e.ops(0), "session.Revoke")
	wantNoOpsWith(t, "未知 token 路径", e.ops(0), "cache.")
}

// TestLogoutEmptyTokenOnlyQueriesDB 入参校验边界：空 token 没有本地短路，
// 照样发一次 token=” 的查库（真实 SQL 同样命中 0 行），然后静默成功。
// 与 RenewToken/TokenInfo 的「空值先短路」形成对照——三个方法口径不一致。
func TestLogoutEmptyTokenOnlyQueriesDB(t *testing.T) {
	e := newEnv(t)
	seedUser(t, e.st, "alice", "S3cret!", model.CredentialTypeUsername)
	e.st.log.reset()

	_, err := callLogout(t, e, "")
	wantNoErr(t, "空 token 登出", err)
	wantOps(t, "空 token 的调用序列", e.ops(0), []string{"session.FindByToken:"})
}

// TestLogoutCannotTouchCacheOnlySession 缓存里有、库里没有的 token（RenewToken 的
// Del 被吞后必然出现的中间态，见 TestRenewTokenCacheDeleteFailureKeepsOldTokenAlive）
// 登不掉：Logout 不读缓存，查库 0 行 → 静默成功 → 缓存条目与鉴权结果原封不动。
func TestLogoutCannotTouchCacheOnlySession(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	ghost := model.AccountSession{
		Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RefreshToken: "rrrr", Mid: mid,
		Csrf: "cccc", Expires: nowUnix() + 3600, Status: model.SessionStatusActive,
	}
	st.cache.warmJSON("ak_"+ghost.Token, &ghost)
	st.log.reset()

	_, err := callLogout(t, e, ghost.Token)
	wantNoErr(t, "登出仅存在于缓存里的 token", err)
	wantOps(t, "缓存态 token 的调用序列", e.ops(0), []string{"session.FindByToken:" + ghost.Token})
	if !st.cache.has("ak_" + ghost.Token) {
		t.Errorf("前置条件破坏：缓存条目本不该被动到")
	}
	st.log.reset()
	info, err := callTokenInfo(t, e, ghost.Token)
	wantNoErr(t, "登出后校验缓存态 token", err)
	wantEQ(t, "只存在于缓存里的 token 登出后仍判定已登录", "is_login", info.IsLogin, true)
}

// TestLogoutRevokeFailureSwallowedResurrectsSession login.go:382 的 `_ =` 吞掉吊销错误：
// 登出仍返回成功，但库里的行还是 status=0，且两条缓存已被删 →
// 下一次 TokenInfo 回源命中有效行，重新回填缓存，token 原地复活（缺口 6 的完整链路）。
func TestLogoutRevokeFailureSwallowedResurrectsSession(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	row := sessionOf(t, e, sess.Token)
	boom := errors.New("account/db: session revoke down")
	st.session.failWith("Revoke", boom)
	st.log.reset()

	_, err := callLogout(t, e, sess.Token)
	wantNoErr(t, "Revoke 故障不得让登出失败（错误被吞）", err)
	wantOps(t, "Revoke 故障下的调用序列", e.ops(0), []string{
		"session.FindByToken:" + sess.Token,
		"session.Revoke:" + sess.Token,
		"cache.Del:ak_" + sess.Token,
		"cache.Del:rk_" + sess.RefreshToken,
	})
	after := st.session.get(row.ID)
	wantEQ(t, "吊销失败后库里的行仍有效（缺口 6）", "status", after.Status, model.SessionStatusActive)

	// 两条缓存确实删过了（见上面的序列），所以「复活」只能来自回源重填 → 影响面被放大
	st.log.reset()
	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "登出失败后回源校验", err)
	wantEQ(t, "登出后 token 仍可用（缺口 6）", "is_login", info.IsLogin, true)
	wantEQ(t, "复活的会话带着真实 mid（可继续调用任何接口）", "mid", info.Mid, mid)
	wantOps(t, "回源 → 重新回填缓存", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
		"cache.SetJSON:ak_" + sess.Token + "/600",
		"cache.SetJSON:rk_" + sess.RefreshToken + "/600",
	})
}

// TestLogoutCacheDeleteFailureKeepsTokenAlive delSessionCache 两次 Del 的错误都被吞：
// 登出成功返回、DB 已吊销，但缓存里还是有效会话 → TokenInfo 命中缓存直接放行，
// 最迟 600 秒后才真正失效（缺口 7）。
func TestLogoutCacheDeleteFailureKeepsTokenAlive(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	row := sessionOf(t, e, sess.Token)
	boom := errors.New("redis down")
	st.cache.failWith("Del", boom)
	st.log.reset()

	_, err := callLogout(t, e, sess.Token)
	wantNoErr(t, "Del 故障不得让登出失败（错误被吞）", err)
	wantOps(t, "Del 故障下的调用序列", e.ops(0), []string{
		"session.FindByToken:" + sess.Token,
		"session.Revoke:" + sess.Token,
		"cache.Del:ak_" + sess.Token,
		"cache.Del:rk_" + sess.RefreshToken,
	})
	wantEQ(t, "缓存故障不影响吊销落库", "status", st.session.get(row.ID).Status, model.SessionStatusRevoked)

	st.log.reset()
	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "登出后校验", err)
	wantEQ(t, "缓存没删掉 → 旧会话仍判定已登录（缺口 7）", "is_login", info.IsLogin, true)
	wantOps(t, "命中的是残留缓存，不回源", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})
}

// TestLogoutLookupFaultPropagates 查库故障原样外传（不降级成领域错误）→
// 网关看到的是 500，而且这一步之后什么都不会执行：会话既没吊销也没删缓存。
func TestLogoutLookupFaultPropagates(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	boom := errors.New("account/db: session lookup down")
	st.session.failWith("FindByToken", boom)
	st.log.reset()

	reply, err := callLogout(t, e, sess.Token)
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	if reply != nil {
		t.Errorf("出错路径必须返回 nil reply，got %+v", reply)
	}
	wantOps(t, "查库故障的调用序列", e.ops(0), []string{"session.FindByToken:" + sess.Token})
	wantNoOpsWith(t, "查库故障路径", e.ops(0), "session.Revoke")
	wantNoOpsWith(t, "查库故障路径", e.ops(0), "cache.")
	// 会话完好无损：可以继续使用，也可以稍后重登出
	st.session.clearFaults()
	st.log.reset()
	_, err = callLogout(t, e, sess.Token)
	wantNoErr(t, "故障恢复后重登出", err)
	wantEQ(t, "重登出后已吊销", "status", sessionOf(t, e, sess.Token).Status, model.SessionStatusRevoked)
}

// TestLogoutRevokesOnlyThisSession 登出的影响面精确到单个 token：
// 同账号的其他会话、其他账号的会话都不受影响。
func TestLogoutRevokesOnlyThisSession(t *testing.T) {
	e := newEnv(t)
	st := e.st
	alice := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	bob := seedUserAt(t, st, 70002, "bob", "S3cret!", model.CredentialTypeUsername)
	first := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	second := login(t, e, "alice", "S3cret!", "1.2.3.5", "pad")
	bobSess := login(t, e, "bob", "S3cret!", "1.2.3.6", "pc")
	st.log.reset()

	_, err := callLogout(t, e, first.Token)
	wantNoErr(t, "登出其中一个会话", err)
	wantEQ(t, "同账号另一会话不受影响", "status", sessionOf(t, e, second.Token).Status, model.SessionStatusActive)
	wantEQ(t, "其他账号会话不受影响", "status", sessionOf(t, e, bobSess.Token).Status, model.SessionStatusActive)
	wantEQ(t, "alice 剩余有效会话数", "active", st.session.activeFor(alice), 1)
	wantEQ(t, "bob 的有效会话数", "active", st.session.activeFor(bob), 1)
	wantEQ(t, "登出不新增会话行", "session 行数", st.session.count(), 3)
}

// TestLogoutReplayIsIdempotent 重放登出：Revoke 的 SQL 带 `AND status = 0`，
// 已吊销行不再命中 → 第二次登出的 mtime 必须原封不动（把 mtime 布成哨兵值来证伪
// 「无条件刷新」），且两条缓存的 Del 仍会重发（幂等但有副作用）。
func TestLogoutReplayIsIdempotent(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	_, err := callLogout(t, e, sess.Token)
	wantNoErr(t, "首次登出", err)
	row := sessionOf(t, e, sess.Token)
	st.session.mutate(sess.Token, func(r *model.AccountSession) { r.MTime = 12345 })
	st.log.reset()

	_, err = callLogout(t, e, sess.Token)
	wantNoErr(t, "重放登出", err)
	wantOps(t, "重放登出的调用序列", e.ops(0), []string{
		"session.FindByToken:" + sess.Token,
		"session.Revoke:" + sess.Token,
		"cache.Del:ak_" + sess.Token,
		"cache.Del:rk_" + sess.RefreshToken,
	})
	after := st.session.get(row.ID)
	wantEQ(t, "重放不得刷新 mtime（WHERE 带 status=0）", "mtime", after.MTime, int64(12345))
	wantEQ(t, "重放后 status 仍为已吊销", "status", after.Status, model.SessionStatusRevoked)
}

// TestLogoutAlsoKillsRefreshToken 登出按 token 吊销整行 → 同一行的 refresh 一起失效：
// 这是「一处吊销覆盖两条凭证」的正向结论（对比 RenewToken 不轮换 refresh，缺口 4）。
func TestLogoutAlsoKillsRefreshToken(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	_, err := callLogout(t, e, sess.Token)
	wantNoErr(t, "登出", err)
	st.log.reset()

	reply, err := callRenewToken(t, e, sess.RefreshToken)
	wantErrIs(t, "登出后用 refresh 刷新", err, ErrSessionRevoked)
	if reply != nil {
		t.Errorf("登出后刷新不得签发新 token：%+v", reply)
	}
	wantOps(t, "登出后刷新的调用序列", e.ops(0), []string{"session.FindByRefresh:" + sess.RefreshToken})
	wantCount(t, "登出后刷新不落库", e.ops(0), "session.UpdateToken", 0)
}
