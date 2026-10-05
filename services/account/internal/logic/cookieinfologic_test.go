package logic

// cookieinfologic_test.go 覆盖 CookieInfo（repository/login.go:402-419 → sessionByToken:220-240）。
//
// 被测判定链：按 ';' 切 cookie、每段 TrimSpace 后用 SplitN("=",2) 找**第一个**名为
// SESSDATA 的项 → sessionByToken（先读 ak_<token> 缓存，命中还要校验 cached.Token 与
// 状态/过期；未命中或坏值才回源 account_session，回源成功后回填 ak_/rk_ 两条缓存）。
//
// 钉住的关键事实：
//   - 「未登录」是**结论不是错误**：ErrSessionRevoked 被翻译成 is_login=false 且 err=nil，
//     而底层存储故障必须原样外传、绝不许也被折成 is_login=false（两条用例分别钉死）；
//   - 空 token（没有 SESSDATA 项 / 名字大小写不对 / 值为空）在触 Redis **之前**就短路，
//     因此调用序列为空——这是「解析没解出来」的唯一可观测证据；
//   - SESSDATA 的 key 名区分大小写（kv[0] == "SESSDATA"），网关传 'sessdata=' 恒判未登录；
//   - 缓存里的会话过期/已吊销时**不回源**（直接判未登录），但缓存值与 key 上的 token
//     不一致时会回源并回填；
//   - 缺口 G：吊销只写 DB（SetPassword/ResetPassword 的 RevokeAll）不删 ak_ 缓存，
//     于是改密后最长 600s 内旧 cookie 仍判「已登录」。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callCookieInfo(t *testing.T, e *env, cookie string) (*rpc.GetCookieInfoReply, error) {
	t.Helper()
	return NewCookieInfoLogic(context.Background(), e.svcCtx).CookieInfo(&rpc.GetCookieInfoReq{Cookie: cookie})
}

// TestCookieInfoValidCookieFromCacheReportsSession 有效 cookie + 缓存命中：
// 只读一次 ak_ 缓存就给出完整会话字段，绝不回源 DB。
func TestCookieInfoValidCookieFromCacheReportsSession(t *testing.T) {
	e := newEnv(t)
	seedUser(t, e.st, capturePhone, "S3cret!", model.CredentialTypePhone)
	sess := login(t, e, capturePhone, "S3cret!", "1.2.3.4", "pc")

	reply, err := callCookieInfo(t, e, "SESSDATA="+sess.Token+";sid="+sess.Csrf)
	wantNoErr(t, "有效 cookie 校验", err)
	wantEQ(t, "已登录判定", "is_login", reply.IsLogin, true)
	wantEQ(t, "会话字段", "mid", reply.Mid, sess.Mid)
	wantEQ(t, "会话字段", "csrf", reply.Csrf, sess.Csrf)
	wantEQ(t, "会话字段", "expires", reply.Expires, sess.Expires)
	wantOps(t, "缓存命中不回源", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})
	wantNoOpsWith(t, "cookie 校验", e.ops(0), "session.FindByToken")
	wantNoOpsWith(t, "cookie 校验", e.ops(0), "loginlog.")
	wantNoOpsWith(t, "cookie 校验", e.ops(0), "userProfile.")
}

// TestCookieInfoCookieParsingTable cookie 文本的解析口径：
// 段间空格、前后缀项都能取到 SESSDATA；小写键名、无 '='、值为空、缺项都取不到，
// 而取不到 token 时**一次依赖都不碰**（ops 为空序列）。
func TestCookieInfoCookieParsingTable(t *testing.T) {
	e := newEnv(t)
	seedUser(t, e.st, capturePhone, "S3cret!", model.CredentialTypePhone)
	sess := login(t, e, capturePhone, "S3cret!", "1.2.3.4", "pc")
	tok := sess.Token

	cases := []struct {
		name     string
		cookie   string
		wantLive bool
		wantOps  []string
	}{
		{"裸一项", "SESSDATA=" + tok, true, []string{"cache.GetJSON:ak_" + tok}},
		{"段间有空格", "sid=x;  SESSDATA=" + tok, true, []string{"cache.GetJSON:ak_" + tok}},
		{"后面还有别的项", "a=1;SESSDATA=" + tok + ";b=2", true, []string{"cache.GetJSON:ak_" + tok}},
		{"分号开头", ";SESSDATA=" + tok, true, []string{"cache.GetJSON:ak_" + tok}},
		{"键名小写", "sessdata=" + tok, false, []string{}},
		{"没有等号", "SESSDATA", false, []string{}},
		{"值为空", "SESSDATA=", false, []string{}},
		{"空串", "", false, []string{}},
		{"只有别的项", "sid=abc;other=1", false, []string{}},
		{
			// 同名多项只取第一个：第二个才是有效 token，于是判未登录
			name:     "重复项取第一个",
			cookie:   "SESSDATA=nosuchtoken;SESSDATA=" + tok,
			wantLive: false,
			wantOps: []string{
				"cache.GetJSON:ak_nosuchtoken",
				"session.FindByToken:nosuchtoken",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.st.log.reset()
			reply, err := callCookieInfo(t, e, tc.cookie)
			wantNoErr(t, "cookie 校验必须把未登录当结论返回", err)
			if reply == nil {
				t.Fatalf("应答 = nil, want 一条 GetCookieInfoReply")
			}
			wantEQ(t, "is_login", "is_login", reply.IsLogin, tc.wantLive)
			wantOps(t, "调用序列", e.ops(0), tc.wantOps)
			if !tc.wantLive {
				wantEQ(t, "未登录时不回显任何会话字段", "mid", reply.Mid, int64(0))
				wantEQ(t, "未登录时不回显任何会话字段", "csrf", reply.Csrf, "")
			}
		})
	}
}

// TestCookieInfoUnknownTokenIsConclusionNotError 缓存与 DB 都查不到的 token：
// is_login=false 且 err=nil —— 这是「结论」，把它做成错误会让网关把鉴权降级成 5xx。
func TestCookieInfoUnknownTokenIsConclusionNotError(t *testing.T) {
	e := newEnv(t)
	reply, err := callCookieInfo(t, e, "SESSDATA=ghosttoken")
	wantNoErr(t, "陌生 token", err)
	if errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("ErrSessionRevoked 被外传了（与实现不符）：%v", err)
	}
	wantEQ(t, "陌生 token 的判定", "is_login", reply.IsLogin, false)
	wantOps(t, "先缓存后回源", e.ops(0), []string{
		"cache.GetJSON:ak_ghosttoken",
		"session.FindByToken:ghosttoken",
	})
}

// TestCookieInfoExpiredOrRevokedCacheValueDoesNotFallBack 缓存里那条会话已过期/已吊销：
// 直接判未登录，**不再**回源 DB（用例断言 session.FindByToken 一次都没出现）。
func TestCookieInfoExpiredOrRevokedCacheValueDoesNotFallBack(t *testing.T) {
	const tok = "tok-window"
	cases := []struct {
		name   string
		status int8
		expiry int64
	}{
		{"缓存里已过期", model.SessionStatusActive, nowUnix() - 1},
		{"缓存里已吊销", model.SessionStatusRevoked, nowUnix() + 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			st.cache.warmJSON("ak_"+tok, &model.AccountSession{
				Token: tok, Mid: 70001, Csrf: "csrf-seed", Status: tc.status, Expires: tc.expiry,
			})
			// DB 里同一 token 是一行**有效**会话：证明结论来自缓存那条，而不是回源结果
			st.session.put(&model.AccountSession{
				Token: tok, RefreshToken: "rk-seed", Mid: 70001, Csrf: "csrf-seed",
				Expires: nowUnix() + 86400, RefreshExpires: nowUnix() + 9*86400,
				Status: model.SessionStatusActive,
			})
			st.log.reset()

			reply, err := callCookieInfo(t, e, "SESSDATA="+tok)
			wantNoErr(t, "过期/吊销会话的判定", err)
			wantEQ(t, "is_login", "is_login", reply.IsLogin, false)
			wantOps(t, "命中缓存即出结论", e.ops(0), []string{"cache.GetJSON:ak_" + tok})
			wantNoOpsWith(t, "过期/吊销不得回源", e.ops(0), "session.FindByToken")
		})
	}
}

// TestCookieInfoCachedTokenMismatchFallsBackAndBackfills 缓存值与 key 上的 token 不一致
// （线上等价于「键被别的会话写过/串了」）：必须回源核对，成功后按 600s 重新回填两条缓存。
func TestCookieInfoCachedTokenMismatchFallsBackAndBackfills(t *testing.T) {
	const tok = "tok-mismatch"
	e := newEnv(t)
	st := e.st
	st.cache.warmJSON("ak_"+tok, &model.AccountSession{
		Token: "somebody-elses-token", Mid: 999999, Csrf: "wrong-csrf",
		Status: model.SessionStatusActive, Expires: nowUnix() + 86400,
	})
	st.session.put(&model.AccountSession{
		Token: tok, RefreshToken: "rk-real", Mid: 70001, Csrf: "csrf-real",
		Expires: nowUnix() + 86400, RefreshExpires: nowUnix() + 9*86400,
		Status: model.SessionStatusActive,
	})
	st.log.reset()

	reply, err := callCookieInfo(t, e, "SESSDATA="+tok)
	wantNoErr(t, "串值后的 cookie 校验", err)
	wantEQ(t, "以 DB 为准判定已登录", "is_login", reply.IsLogin, true)
	wantEQ(t, "mid 来自 DB 行而不是缓存串值", "mid", reply.Mid, int64(70001))
	wantEQ(t, "csrf 来自 DB 行", "csrf", reply.Csrf, "csrf-real")
	wantOps(t, "回源 + 回填两条缓存", e.ops(0), []string{
		"cache.GetJSON:ak_" + tok,
		"session.FindByToken:" + tok,
		"cache.SetJSON:ak_" + tok + "/600",
		"cache.SetJSON:rk_rk-real/600",
	})
	var ak model.AccountSession
	if !st.cache.jsonOf("ak_"+tok, &ak) {
		t.Fatalf("ak_ 缓存没有被子写回")
	}
	wantEQ(t, "写回的缓存值 token 与 key 一致", "token", ak.Token, tok)
}

// TestCookieInfoDBMissIsConclusionWithoutBackfill DB 查不到就出结论，不写任何缓存
// （否则一个陌生 token 就能往 Redis 里塞键）。
func TestCookieInfoDBMissIsConclusionWithoutBackfill(t *testing.T) {
	e := newEnv(t)
	reply, err := callCookieInfo(t, e, "SESSDATA=never-seen")
	wantNoErr(t, "陌生 token", err)
	wantEQ(t, "is_login", "is_login", reply.IsLogin, false)
	wantCount(t, "未命中不得回填缓存", e.ops(0), "cache.SetJSON", 0)
	if e.st.cache.has("ak_never-seen") {
		t.Errorf("为陌生 token 写了缓存键")
	}
}

// TestCookieInfoStorageFailureIsNotSilentlyNotLoggedIn 存储故障：错误必须原样外传、
// 应答为 nil —— 绝不允许被折成 is_login=false（那是把 DB 抖动当成「用户没登录」，
// 网关会据此把已登录用户踢下线）。
func TestCookieInfoStorageFailureIsNotSilentlyNotLoggedIn(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const tok = "tok-db-down"
	boom := errors.New("account/db: session lookup down")
	// 缓存先按 miss 处理，把链路逼到回源那一步
	st.session.failWith("FindByToken", boom)
	st.log.reset()

	reply, err := callCookieInfo(t, e, "SESSDATA="+tok)
	if reply != nil {
		t.Errorf("应答 = %+v, want nil（故障绝不能伪装成「未登录」）", reply)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样外传 %v", err, boom)
	}
	if errors.Is(err, ErrSessionRevoked) {
		t.Errorf("底层故障被折成了 ErrSessionRevoked：%v", err)
	}
	wantOps(t, "故障路径的调用序列", e.ops(0), []string{
		"cache.GetJSON:ak_" + tok,
		"session.FindByToken:" + tok,
	})

	// 对照：同一 token 在 DB 恢复后确实查得到 ⇒ 上面的失败是故障而不是「没登录」
	st.session.clearFaults()
	st.session.put(&model.AccountSession{
		Token: tok, RefreshToken: "rk-recover", Mid: 70001, Csrf: "c",
		Expires: nowUnix() + 86400, RefreshExpires: nowUnix() + 9*86400,
		Status: model.SessionStatusActive,
	})
	st.cache.forget("ak_" + tok)
	st.log.reset()
	ok, err := callCookieInfo(t, e, "SESSDATA="+tok)
	wantNoErr(t, "依赖恢复后重查", err)
	wantEQ(t, "恢复后的判定", "is_login", ok.IsLogin, true)
}

// TestCookieInfoRevokeAllLeavesCachedSessionValid 缺口 G：RevokeAll（改密/重置密码走的批量吊销）
// 只 UPDATE account_session，不删 ak_ 缓存；CookieInfo 读的是缓存那条 status=0 的旧值，
// 于是被吊销的 cookie 在 600s 窗口内仍然判定「已登录」。
func TestCookieInfoRevokeAllLeavesCachedSessionValid(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	sess := login(t, e, capturePhone, "S3cret!", "1.2.3.4", "pc")
	// 静默把 DB 行置为已吊销（等价于 RevokeAll 之后的库内状态），缓存里的旧副本保持原样
	st.session.mutate(sess.Token, func(s *model.AccountSession) { s.Status = model.SessionStatusRevoked })
	st.log.reset()

	reply, err := callCookieInfo(t, e, "SESSDATA="+sess.Token)
	wantNoErr(t, "已吊销会话的 cookie 校验", err)
	wantEQ(t, "现状：吊销后缓存窗口内仍判已登录（缺口 G）", "is_login", reply.IsLogin, true)
	wantOps(t, "只读缓存，没看见 DB 的吊销状态", e.ops(0), []string{"cache.GetJSON:ak_" + sess.Token})

	// 边界对：把缓存也清掉（等价于窗口过期或 Logout 删键），同一条 cookie 立刻判未登录
	st.cache.forget("ak_" + sess.Token)
	st.log.reset()
	after, err := callCookieInfo(t, e, "SESSDATA="+sess.Token)
	wantNoErr(t, "缓存失效后的 cookie 校验", err)
	wantEQ(t, "缓存缺失后以 DB 状态为准", "is_login", after.IsLogin, false)
	wantOps(t, "回源后不再回填（未通过有效性判定）", e.ops(0), []string{
		"cache.GetJSON:ak_" + sess.Token,
		"session.FindByToken:" + sess.Token,
	})
}
