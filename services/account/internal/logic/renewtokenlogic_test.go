package logic

// renewtokenlogic_test.go 覆盖 RenewToken（repository/login.go:422-446）。
//
// 被测判定链：refresh 非空 → 按 refresh 查会话（**只查 DB，不查缓存**）→
// 校验 status 与 refresh_expires → 生成新 token/csrf 与新 expires →
// UpdateToken 落库 → 删旧 ak_ 缓存 → 回填 ak_/rk_ 缓存。
//
// 钉住的关键事实：
//   - UpdateToken 不轮换 refresh_token / refresh_expires，也没有重放检测（缺口 4）：
//     同一个 refresh 可以被一直刷到 90 天期满；
//   - 刷新过程不写任何登录日志（审计面空洞，缺口 9c）；
//   - UpdateToken 失败时旧 token 缓存原封不动（缺口 7 同源）；
//   - 删旧 ak_ 缓存的错误被 `_ =` 吞掉 → 旧 token 在 600 秒内仍然可用。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callRenewToken(t *testing.T, e *env, refresh string) (*rpc.RenewTokenReply, error) {
	t.Helper()
	return NewRenewTokenLogic(context.Background(), e.svcCtx).RenewToken(
		&rpc.RenewTokenReq{RefreshToken: refresh, Ip: "1.2.3.4"})
}

// TestRenewTokenHappyPath 正常轮换：五步顺序、DB 行只改 token/csrf/expires、
// 旧 ak_ 缓存删除、新 ak_ 与 rk_ 缓存回填。
func TestRenewTokenHappyPath(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	oldToken, refresh := sess.Token, sess.RefreshToken
	oldRow := sessionOf(t, e, oldToken)
	now := nowUnix()

	reply, err := callRenewToken(t, e, refresh)
	wantNoErr(t, "刷新 token", err)
	wantOps(t, "刷新调用序列", e.ops(0), []string{
		"session.FindByRefresh:" + refresh,
		"session.UpdateToken:" + itoa(oldRow.ID),
		"cache.Del:ak_" + oldToken,
		"cache.SetJSON:ak_" + reply.Token + "/600",
		"cache.SetJSON:rk_" + refresh + "/600",
	})
	wantHexLen(t, "刷新签发", "token", reply.Token, 64)
	wantHexLen(t, "刷新签发", "csrf", reply.Csrf, 32)
	if reply.Token == oldToken {
		t.Errorf("新 token 与旧的相同：%s…", reply.Token[:8])
	}
	wantTSWindow(t, "刷新后的 expires", "expires", reply.Expires, now+30*86400, nowUnix()+30*86400+5)

	// DB：同一行被就地更新（不新增会话行）
	wantEQ(t, "刷新不得新增会话行", "session 行数", st.session.count(), 1)
	newRow := sessionOf(t, e, reply.Token)
	wantEQ(t, "轮换发生在同一行", "id", newRow.ID, oldRow.ID)
	wantEQ(t, "轮换后的 mid", "mid", newRow.Mid, mid)
	wantEQ(t, "轮换后的 csrf", "csrf", newRow.Csrf, reply.Csrf)
	wantEQ(t, "轮换后的 expires", "expires", newRow.Expires, reply.Expires)
	wantEQ(t, "轮换后 status 仍为有效", "status", newRow.Status, model.SessionStatusActive)
	// 缺口 4：refresh 完全不轮换（token/refresh 一一对应，泄露后无法通过轮换发现）
	wantEQ(t, "refresh_token 未轮换（缺口 4）", "refresh_token", newRow.RefreshToken, refresh)
	wantEQ(t, "refresh_expires 未顺延（缺口 4）", "refresh_expires", newRow.RefreshExpires, oldRow.RefreshExpires)

	// 缓存：旧的 ak_ 被删、新的 ak_ 与（同 key 的）rk_ 被刷新
	if st.cache.has("ak_" + oldToken) {
		t.Errorf("旧 ak_ 缓存仍在，必须已被删除")
	}
	var ak model.AccountSession
	if !st.cache.jsonOf("ak_"+reply.Token, &ak) {
		t.Fatalf("新 ak_ 缓存未回填")
	}
	wantEQ(t, "新 ak_ 缓存内容", "token", ak.Token, reply.Token)
	wantEQ(t, "新 ak_ 缓存内容", "expires", ak.Expires, reply.Expires)
	var rk model.AccountSession
	if !st.cache.jsonOf("rk_"+refresh, &rk) {
		t.Fatalf("rk_ 缓存未回填")
	}
	wantEQ(t, "rk_ 缓存指向新 token", "token", rk.Token, reply.Token)

	// 刷新不写任何审计日志（缺口 9c）：整条序列里一步 loginlog 都没有
	wantNoOpsWith(t, "刷新链路", e.ops(0), "loginlog.Add")
	wantEQ(t, "刷新不落登录日志（缺口 9c，只剩 login() 那一条）", "loginlog 行数", st.loginLog.count(), 1)

	// 新 token 立即可用于鉴权，且是缓存命中（不再回源）
	st.log.reset()
	info, err := callTokenInfo(t, e, reply.Token)
	wantNoErr(t, "新 token 校验", err)
	wantEQ(t, "新 token 已登录", "is_login", info.IsLogin, true)
	wantOps(t, "新 token 走缓存命中", e.ops(0), []string{"cache.GetJSON:ak_" + reply.Token})
}

// TestRenewTokenEmptyRefreshTouchessNothing 入参校验边界：空 refresh 直接返回领域错误，
// 一次调用都不发。
func TestRenewTokenEmptyRefreshTouchessNothing(t *testing.T) {
	e := newEnv(t)
	_, err := callRenewToken(t, e, "")
	wantErrIs(t, "空 refresh", err, ErrSessionRevoked)
	wantEQ(t, "空 refresh 不得产生任何调用", "ops", len(e.ops(0)), 0)
}

// TestRenewTokenUnknownRefresh 未知 refresh：只查库一次即返回领域错误，
// 不签发、不动缓存、不写日志。
func TestRenewTokenUnknownRefresh(t *testing.T) {
	e := newEnv(t)
	seedUser(t, e.st, "alice", "S3cret!", model.CredentialTypeUsername)
	e.st.log.reset()

	_, err := callRenewToken(t, e, "deadbeef")
	wantErrIs(t, "未知 refresh", err, ErrSessionRevoked)
	wantOps(t, "未知 refresh 的调用序列", e.ops(0), []string{"session.FindByRefresh:deadbeef"})
	wantNoOpsWith(t, "未知 refresh 路径", e.ops(0), "session.UpdateToken")
	wantNoOpsWith(t, "未知 refresh 路径", e.ops(0), "cache.")
}

// TestRenewTokenRejectsRevokedSession 已吊销（登出/改密后）的会话不能再刷：
// 校验的是 DB 行的 status，与缓存无关。
func TestRenewTokenRejectsRevokedSession(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.session.mutate(sess.Token, func(r *model.AccountSession) { r.Status = model.SessionStatusRevoked })
	st.log.reset()

	_, err := callRenewToken(t, e, sess.RefreshToken)
	wantErrIs(t, "已吊销会话", err, ErrSessionRevoked)
	wantOps(t, "已吊销的调用序列", e.ops(0), []string{
		"session.FindByRefresh:" + sess.RefreshToken,
	})
	wantNoOpsWith(t, "已吊销路径", e.ops(0), "session.UpdateToken")
	// 缓存里仍是有效会话（rk_ 未清理），但吊销判定以 DB 为准 → 说明刷新链路不信任缓存
	if !st.cache.has("rk_" + sess.RefreshToken) {
		t.Errorf("前置条件破坏：rk_ 缓存本应还在")
	}
}

// TestRenewTokenRejectsExpiredRefresh refresh 过期后必须拒绝（时间窗用 DB 行判定）。
func TestRenewTokenRejectsExpiredRefresh(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.session.mutate(sess.Token, func(r *model.AccountSession) { r.RefreshExpires = nowUnix() - 1 })
	st.log.reset()

	_, err := callRenewToken(t, e, sess.RefreshToken)
	wantErrIs(t, "refresh 已过期", err, ErrSessionRevoked)
	wantEQ(t, "过期后不得更新会话", "UpdateToken 次数",
		countPrefix(e.ops(0), "session.UpdateToken"), 0)
}

// TestRenewTokenLookupFaultPropagates 查库故障：错误原样外传（不降级成领域错误），
// 于是网关看到的是 500 而不是「未登录」。
func TestRenewTokenLookupFaultPropagates(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	boom := errors.New("account/db: session lookup down")
	st.session.failWith("FindByRefresh", boom)
	st.log.reset()

	_, err := callRenewToken(t, e, sess.RefreshToken)
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	wantOps(t, "查库故障的调用序列", e.ops(0), []string{"session.FindByRefresh:" + sess.RefreshToken})
	wantNoOpsWith(t, "查库故障路径", e.ops(0), "cache.")
}

// TestRenewTokenUpdateFailureLeavesOldTokenValid 轮换写库失败时错误外传，
// 且**旧 token 缓存一步都没清**（Del 在 UpdateToken 之后）→ 旧 token 继续有效。
func TestRenewTokenUpdateFailureLeavesOldTokenValid(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	boom := errors.New("account/db: session update down")
	st.session.failWith("UpdateToken", boom)
	sessionID := sessionOf(t, e, sess.Token).ID
	st.log.reset()

	_, err := callRenewToken(t, e, sess.RefreshToken)
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	wantOps(t, "UpdateToken 失败的调用序列", e.ops(0), []string{
		"session.FindByRefresh:" + sess.RefreshToken,
		"session.UpdateToken:" + itoa(sessionID),
	})
	wantNoOpsWith(t, "UpdateToken 失败路径", e.ops(0), "cache.Del")
	wantNoOpsWith(t, "UpdateToken 失败路径", e.ops(0), "cache.SetJSON")
	// 旧 token 仍能鉴权（缓存未动、DB 未动）
	st.log.reset()
	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "轮换失败后旧 token 校验", err)
	wantEQ(t, "轮换失败后旧 token 仍有效", "is_login", info.IsLogin, true)
}

// TestRenewTokenCacheDeleteFailureKeepsOldTokenAlive 删旧缓存的错误被 `_ =` 吞掉：
// 刷新照样成功返回，但旧 token 在 tokenCacheTTL(600s) 内仍然可用（缺口 7 的实锤）。
func TestRenewTokenCacheDeleteFailureKeepsOldTokenAlive(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	boom := errors.New("redis down")
	st.cache.failWith("Del", boom)
	sessionID := sessionOf(t, e, sess.Token).ID
	st.log.reset()

	reply, err := callRenewToken(t, e, sess.RefreshToken)
	wantNoErr(t, "Del 故障不得让刷新失败（错误被吞）", err)
	wantOps(t, "Del 故障下的调用序列", e.ops(0), []string{
		"session.FindByRefresh:" + sess.RefreshToken,
		"session.UpdateToken:" + itoa(sessionID),
		"cache.Del:ak_" + sess.Token,
		"cache.SetJSON:ak_" + reply.Token + "/600",
		"cache.SetJSON:rk_" + sess.RefreshToken + "/600",
	})
	// DB 里旧 token 已经不存在了……
	if st.session.byToken(sess.Token) != nil {
		t.Errorf("前置条件破坏：DB 里不该还能按旧 token 查到行")
	}
	// ……但缓存里还在，所以旧 token 仍然判定为已登录（同一 mid，可继续调用任何接口）
	st.log.reset()
	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "旧 token 校验", err)
	wantEQ(t, "旧 token 在缓存里复活（缺口 7）", "is_login", info.IsLogin, true)
	wantOps(t, "旧 token 走的是缓存命中，不回源", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})

	// 新 token 同样有效：一次刷新后有两个可用 token
	info2, err := callTokenInfo(t, e, reply.Token)
	wantNoErr(t, "新 token 校验", err)
	wantEQ(t, "新 token 有效", "is_login", info2.IsLogin, true)
}

// TestRenewTokenReplayableIndefinitely 缺口 4 的可复现证明：refresh 不轮换 + 无重放检测
// → 同一条 refresh 可以连续刷 3 次，每次都拿到新 token 且都能用。
// 一旦 refresh 泄露，攻击者与受害者可长期并存，且无法通过「重放即失效」发现。
func TestRenewTokenReplayableIndefinitely(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.log.reset()

	tokens := []string{sess.Token}
	for i := 0; i < 3; i++ {
		reply, err := callRenewToken(t, e, sess.RefreshToken)
		wantNoErr(t, "同一 refresh 重放刷新", err)
		for _, prev := range tokens {
			if reply.Token == prev {
				t.Fatalf("第 %d 次重放拿到了重复 token", i+1)
			}
		}
		tokens = append(tokens, reply.Token)
		row := sessionOf(t, e, reply.Token)
		wantEQ(t, "始终是同一行", "id", row.ID, int64(1))
		wantEQ(t, "始终是同一个 mid", "mid", row.Mid, mid)
		wantEQ(t, "refresh 未轮换", "refresh_token", row.RefreshToken, sess.RefreshToken)
	}
	wantCount(t, "3 次重放 = 3 次 UpdateToken", e.ops(0), "session.UpdateToken", 3)
	wantEQ(t, "3 次重放后仍只有 1 个会话", "session 行数", st.session.count(), 1)
	wantCount(t, "3 次重放落 3 条审计都没有（缺口 9c）", e.ops(0), "loginlog.Add", 0)
}

// TestRenewTokenHonoursConfiguredTokenTTL 配置项 TokenTTLDays 进了刷新口径。
func TestRenewTokenHonoursConfiguredTokenTTL(t *testing.T) {
	e := newEnv(t, withTokenTTL(3))
	seedUser(t, e.st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	e.st.log.reset()
	now := nowUnix()

	reply, err := callRenewToken(t, e, sess.RefreshToken)
	wantNoErr(t, "刷新", err)
	wantTSWindow(t, "配置化 token 有效期", "expires", reply.Expires, now+3*86400, nowUnix()+3*86400+5)
}

// TestRenewTokenUsesAccessTTLForNewCacheEntry 刷新后回填的两条缓存都重新拿满 600 秒
// （不是沿用旧条目剩余时间）——这决定了「缓存里能活多久」。
func TestRenewTokenUsesAccessTTLForNewCacheEntry(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.log.reset()

	reply, err := callRenewToken(t, e, sess.RefreshToken)
	wantNoErr(t, "刷新", err)
	for _, k := range []string{"ak_" + reply.Token, "rk_" + sess.RefreshToken} {
		if ttl, ok := st.cache.ttlOf(k); !ok || ttl != 600 {
			t.Errorf("%s TTL = %d(存在=%v), want 600", k, ttl, ok)
		}
	}
}
