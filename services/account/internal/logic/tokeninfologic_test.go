package logic

// tokeninfologic_test.go 覆盖 TokenInfo（repository/login.go:389-398 + sessionByToken:220-240）。
//
// 被测判定链：token 非空 → 读 ak_ 缓存 → 缓存命中即**短路**（有效就直接返回、
// 无效/过期就直接判未登录，一律不回源）→ 缓存未命中/坏值/读故障才回源 DB →
// DB 行需 status=0 且未过期 → 回填 ak_/rk_ 缓存。
//
// 钉住的关键事实：
//   - 「无效 token」是结论不是错误：只有 ErrSessionRevoked 会被软化成为 is_login=false；
//   - DB 故障必须外传（网关据此区分「没登录」与「鉴权不可用」）；
//   - 全链路从不读 account 主表 → 封禁账号的既有 token 继续有效（缺口 14）；
//   - 缓存短路意味着「缓存说活着就活着、缓存说死了就死了」，批量吊销只能等 600 秒
//     （缺口 7），而 Revoke 落库失败时会因缓存被删而回源复活（缺口 6）。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callTokenInfo(t *testing.T, e *env, token string) (*rpc.GetTokenInfoReply, error) {
	t.Helper()
	return NewTokenInfoLogic(context.Background(), e.svcCtx).TokenInfo(
		&rpc.GetTokenInfoReq{Token: token, Buvid: "BV123"})
}

// TestTokenInfoCacheHitShortCircuitsDB 缓存命中：一步都不许多走（这正是 600 秒窗口的来源）。
func TestTokenInfoCacheHitShortCircuitsDB(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.log.reset()

	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "token 校验", err)
	wantEQ(t, "已登录", "is_login", info.IsLogin, true)
	wantEQ(t, "mid", "mid", info.Mid, mid)
	wantEQ(t, "csrf", "csrf", info.Csrf, sess.Csrf)
	wantEQ(t, "expires", "expires", info.Expires, sess.Expires)
	wantOps(t, "缓存命中只该有一步", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})
	wantNoOpsWith(t, "缓存命中路径", e.ops(0), "session.FindByToken")
	wantNoOpsWith(t, "缓存命中路径", e.ops(0), "account.FindOne")
}

// TestTokenInfoEmptyTokenIsNotErrorButNoCall 入参校验边界：空 token 判未登录，
// 且不发任何 Redis/DB 调用（网关每请求都会带空 token 打这里，必须零成本）。
func TestTokenInfoEmptyTokenIsNotErrorButNoCall(t *testing.T) {
	e := newEnv(t)
	info, err := callTokenInfo(t, e, "")
	wantNoErr(t, "空 token", err)
	wantEQ(t, "空 token 判未登录", "is_login", info.IsLogin, false)
	wantEQ(t, "空 token 的 mid", "mid", info.Mid, int64(0))
	wantEQ(t, "空 token 的 csrf", "csrf", info.Csrf, "")
	wantEQ(t, "空 token 不得产生任何调用", "ops", len(e.ops(0)), 0)
}

// TestTokenInfoCacheMissFallsBackToDBAndRewarms 缓存未命中 → 回源 → 回填两条缓存。
func TestTokenInfoCacheMissFallsBackToDBAndRewarms(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.cache.forget("ak_" + sess.Token)
	st.cache.forget("rk_" + sess.RefreshToken)
	st.log.reset()

	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "回源校验", err)
	wantEQ(t, "回源后已登录", "is_login", info.IsLogin, true)
	wantEQ(t, "回源后的 mid", "mid", info.Mid, mid)
	wantOps(t, "回源+回填的调用序列", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
		"cache.SetJSON:ak_" + sess.Token + "/600",
		"cache.SetJSON:rk_" + sess.RefreshToken + "/600",
	})
	// 回填后的缓存条目 TTL 重新拿满 600
	if ttl, ok := st.cache.ttlOf("ak_" + sess.Token); !ok || ttl != 600 {
		t.Errorf("回填的 ak_ TTL = %d(存在=%v), want 600", ttl, ok)
	}
	// 第二次调用即缓存命中
	st.log.reset()
	_, err = callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "第二次校验", err)
	wantOps(t, "第二次只该读缓存", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})
}

// TestTokenInfoUnknownTokenIsNotError 不存在的 token：判未登录而不是报错。
func TestTokenInfoUnknownTokenIsNotError(t *testing.T) {
	e := newEnv(t)
	seedUser(t, e.st, "alice", "S3cret!", model.CredentialTypeUsername)
	e.st.log.reset()

	info, err := callTokenInfo(t, e, "nonexistent-token")
	wantNoErr(t, "未知 token", err)
	wantEQ(t, "未知 token 判未登录", "is_login", info.IsLogin, false)
	wantOps(t, "未知 token 的调用序列", e.ops(0), []string{
		"cache.GetJSON:ak_nonexistent-token",
		"session.FindByToken:nonexistent-token",
	})
	wantNoOpsWith(t, "未知 token 路径", e.ops(0), "cache.SetJSON")
}

// TestTokenInfoDBRevokedRowIsNotError 已吊销的 DB 行（缓存已过期）→ 未登录，不报错，
// 而且**不回填缓存**（不给废 token 续命）。
func TestTokenInfoDBRevokedRowIsNotError(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.session.mutate(sess.Token, func(r *model.AccountSession) { r.Status = model.SessionStatusRevoked })
	st.cache.forget("ak_" + sess.Token)
	st.cache.forget("rk_" + sess.RefreshToken)
	st.log.reset()

	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "已吊销 token", err)
	wantEQ(t, "已吊销判未登录", "is_login", info.IsLogin, false)
	wantOps(t, "已吊销的调用序列", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
	})
	wantCount(t, "废 token 不得回填缓存", e.ops(0), "cache.SetJSON", 0)
}

// TestTokenInfoDBExpiredRowIsNotError 过期（但 status 仍为 0）的 DB 行 → 未登录。
// 时间边界取「刚好过期」这一格：判定用的是 `>=`，所以 expires == now 即失效。
func TestTokenInfoDBExpiredRowIsNotError(t *testing.T) {
	cases := []struct {
		name    string
		expires func() int64
		want    bool
	}{
		{"还有 1 秒", func() int64 { return nowUnix() + 60 }, true},
		{"刚好到点", func() int64 { return nowUnix() }, false},
		{"昨天就到期", func() int64 { return nowUnix() - 86400 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
			sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
			st.session.mutate(sess.Token, func(r *model.AccountSession) { r.Expires = tc.expires() })
			st.cache.forget("ak_" + sess.Token)
			st.cache.forget("rk_" + sess.RefreshToken)
			st.log.reset()

			info, err := callTokenInfo(t, e, sess.Token)
			wantNoErr(t, "过期边界校验", err)
			wantEQ(t, "is_login", "值", info.IsLogin, tc.want)
			if !tc.want {
				wantCount(t, "过期 token 不得回填缓存", e.ops(0), "cache.SetJSON", 0)
			}
		})
	}
}

// TestTokenInfoCachedExpiredShortCircuitsWithoutDB 缓存里的行已过期 → 直接判未登录，
// **不回源**：即使 DB 行后来被续期（例如后台延长了有效期），缓存说了算。
// 这条正是「批量吊销只能等 600 秒」的同一处短路（缺口 7）。
func TestTokenInfoCachedExpiredShortCircuitsWithoutDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.AccountSession)
	}{
		{"缓存里已过期", func(r *model.AccountSession) { r.Expires = nowUnix() - 1 }},
		{"缓存里已吊销", func(r *model.AccountSession) { r.Status = model.SessionStatusRevoked }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
			sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
			// 只把**缓存**改坏（DB 行仍是有效且未过期）
			var cached model.AccountSession
			if !st.cache.jsonOf("ak_"+sess.Token, &cached) {
				t.Fatalf("前置条件破坏：ak_ 缓存不在")
			}
			tc.mutate(&cached)
			st.cache.warmJSON("ak_"+sess.Token, &cached)
			st.log.reset()

			info, err := callTokenInfo(t, e, sess.Token)
			wantNoErr(t, "坏缓存的校验", err)
			wantEQ(t, "缓存说死了就是死了", "is_login", info.IsLogin, false)
			wantOps(t, "缓存短路（不回源）的调用序列", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})
			wantNoOpsWith(t, "缓存短路路径", e.ops(0), "session.FindByToken")
			// 对照：DB 行其实还有效 —— 差异完全由缓存窗口造成
			if row := st.session.byToken(sess.Token); row == nil || row.Status != model.SessionStatusActive {
				t.Fatalf("对照前提破坏：DB 行应仍有效，实为 %+v", row)
			}
		})
	}
}

// TestTokenInfoCachedWrongTokenFallsBackToDB 缓存值与 key 对不上（串号/坏值）→ 按未命中回源，
// 不能把别人的会话当成本次 token 的结论。
func TestTokenInfoCachedWrongTokenFallsBackToDB(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	var foreign model.AccountSession
	if !st.cache.jsonOf("ak_"+sess.Token, &foreign) {
		t.Fatalf("前置条件破坏")
	}
	foreign.Token = "some-other-token"
	st.cache.warmJSON("ak_"+sess.Token, &foreign)
	st.log.reset()

	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "串号缓存的校验", err)
	wantEQ(t, "回源后仍判已登录", "is_login", info.IsLogin, true)
	wantEQ(t, "回源后的 mid", "mid", info.Mid, mid)
	wantOps(t, "串号缓存的回源序列", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
		"cache.SetJSON:ak_" + sess.Token + "/600",
		"cache.SetJSON:rk_" + sess.RefreshToken + "/600",
	})
	// 回源把正确值覆盖回缓存，下一次即恢复短路
	var after model.AccountSession
	if !st.cache.jsonOf("ak_"+sess.Token, &after) || after.Token != sess.Token {
		t.Errorf("回填后 ak_ 缓存的 token = %q, want %s", after.Token, sess.Token)
	}
}

// TestTokenInfoCachedGarbageTreatedAsMiss 缓存里是非法 JSON → 反序列化错误按「未命中」回源
// （与 CacheInfo 那套「吞成 miss」的口径一致，但 GetJSON 是把错误交出去、由调用方判 miss）。
func TestTokenInfoCachedGarbageTreatedAsMiss(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.cache.warmRaw("ak_"+sess.Token, "{not json")
	st.log.reset()

	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "坏 JSON 的校验", err)
	wantEQ(t, "坏值回源后判已登录", "is_login", info.IsLogin, true)
	wantEQ(t, "回源后的 mid", "mid", info.Mid, mid)
	wantOps(t, "坏值回源的调用序列", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
		"cache.SetJSON:ak_" + sess.Token + "/600",
		"cache.SetJSON:rk_" + sess.RefreshToken + "/600",
	})
}

// TestTokenInfoRedisReadFaultDegradesToDB Redis 读故障不会把鉴权打死：GetJSON 报错被当作
// 「未命中」，直接回源 DB（写回缓存同样失败但不影响结论）。
func TestTokenInfoRedisReadFaultDegradesToDB(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.cache.forget("ak_" + sess.Token)
	st.cache.forget("rk_" + sess.RefreshToken)
	boom := errors.New("redis down")
	st.cache.failWith("GetJSON", boom)
	st.cache.failWith("SetJSON", boom)
	st.log.reset()

	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "Redis 故障下的鉴权", err)
	wantEQ(t, "Redis 故障仍能靠 DB 判定", "is_login", info.IsLogin, true)
	wantEQ(t, "回源拿到的 mid", "mid", info.Mid, mid)
	wantOps(t, "Redis 故障的调用序列", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
		"cache.SetJSON:ak_" + sess.Token + "/600",
		"cache.SetJSON:rk_" + sess.RefreshToken + "/600",
	})
	// SetJSON 的签名里没有 error：写失败只记日志，鉴权结论不受影响（缓存确实没写进去）
	if st.cache.has("ak_" + sess.Token) {
		t.Errorf("SetJSON 注入故障后不该存在 ak_ 缓存（说明替身把故障当成功了）")
	}
}

// TestTokenInfoDBFaultPropagatesAsError DB 故障必须外传：网关要能区分
// 「用户没登录」与「鉴权服务不可用」，这里不能被软化成 is_login=false。
func TestTokenInfoDBFaultPropagatesAsError(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	boom := errors.New("account/db: session lookup down")
	st.session.failWith("FindByToken", boom)
	st.cache.forget("ak_" + sess.Token)
	st.log.reset()

	reply, err := callTokenInfo(t, e, sess.Token)
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v（DB 故障不得软化为未登录）", err, boom)
	}
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	wantOps(t, "DB 故障的调用序列", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
	})
}

// TestTokenInfoNeverConsultsAccountTable 缺口 14 的鉴权侧：TokenInfo 只信会话表，
// 账号被封禁后既有 token 在 30 天内仍然一路畅通。
func TestTokenInfoNeverConsultsAccountTable(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	acc := st.account.get(mid)
	acc.Status = 1 // 封禁
	st.account.put(acc)
	st.log.reset()

	info, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "封禁账号的 token 校验", err)
	wantEQ(t, "封禁账号的 token 仍判已登录（缺口 14）", "is_login", info.IsLogin, true)
	wantNoOpsWith(t, "鉴权链路从不查账号主表", e.ops(0), "account.FindOne")

	// 缓存过期回源后结论一样（不是缓存造成的假象）
	st.cache.forget("ak_" + sess.Token)
	st.log.reset()
	info2, err := callTokenInfo(t, e, sess.Token)
	wantNoErr(t, "回源校验", err)
	wantEQ(t, "回源后封禁账号仍判已登录", "is_login", info2.IsLogin, true)
}

// TestTokenInfoDoesNotWriteAnything 鉴权是纯读操作：除了缓存回填之外不得有任何写。
func TestTokenInfoDoesNotWriteAnything(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	sess := login(t, e, "alice", "S3cret!", "1.2.3.4", "pc")
	st.cache.forget("ak_" + sess.Token)
	before := st.session.count()
	st.log.reset()

	if _, err := callTokenInfo(t, e, sess.Token); err != nil {
		t.Fatalf("校验：%v", err)
	}
	wantNoOpsWith(t, "鉴权链路", e.ops(0), "session.Insert")
	wantNoOpsWith(t, "鉴权链路", e.ops(0), "session.UpdateToken")
	wantNoOpsWith(t, "鉴权链路", e.ops(0), "session.Revoke")
	wantNoOpsWith(t, "鉴权链路", e.ops(0), "loginlog.Add")
	wantNoOpsWith(t, "鉴权链路", e.ops(0), "account.UpdateStatus")
	wantEQ(t, "会话行数不变", "session 行数", st.session.count(), before)
}
