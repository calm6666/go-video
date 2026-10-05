package logic

// passwordloginlogic_test.go 覆盖 PasswordLogin（repository/login.go:259-285）。
//
// 被测判定链：标识反查（三类凭证依次查，命中即停）→ 密码解密（RSA 或明文）→
// 取当前生效密钥并比对 MD5(pwd+salt) → 签发会话（落 account_session + 回填 ak_/rk_ 缓存）→
// 落登录日志（成功 reason 空、失败 reason 为错误原文）。
//
// 钉住的关键事实：
//   - 缓存不参与登录（口令校验只走 DB），日志在会话之后写；
//   - 全链路**从不读 account 主表** → 封禁/注销中的账号照样能拿到 token（缺口 14）；
//   - 凭证反查不带 status 过滤 → 已解绑标识仍能登录（缺口 2）；
//   - 密码错误既无次数限制也无锁定（缺口 9a）；
//   - RSA 解密失败把底层 crypto 错误原样外传并写进审计日志（缺口 10）；
//   - secret.FindActive 的 DB 故障直接返回且不写任何日志（缺口 9b）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callPasswordLogin(t *testing.T, e *env, in *rpc.LoginReq) (*rpc.LoginReply, error) {
	t.Helper()
	return NewPasswordLoginLogic(context.Background(), e.svcCtx).PasswordLogin(in)
}

// TestPasswordLoginHappyPath 正常路径：一步反查、一次密钥读取、一条会话、两条缓存、一条成功日志。
func TestPasswordLoginHappyPath(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.log.reset()
	now := nowUnix()

	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{
		Account: "alice", Password: "S3cret!", LoginType: int32(model.LoginTypePassword),
		Ip: "1.2.3.4", Device: "pc", Buvid: "BV123",
	})
	wantNoErr(t, "密码登录", err)
	wantEQ(t, "登录返回", "mid", reply.Mid, mid)
	wantOps(t, "登录调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"secret.FindActive:" + itoa(mid) + "/1",
		"session.Insert:1/" + itoa(mid),
		"cache.SetJSON:ak_" + reply.Token + "/600",
		"cache.SetJSON:rk_" + reply.RefreshToken + "/600",
		"loginlog.Add:1/" + itoa(mid) + "/1/0",
	})
	wantHexLen(t, "签发", "token", reply.Token, 64)
	wantHexLen(t, "签发", "refresh_token", reply.RefreshToken, 64)
	wantHexLen(t, "签发", "csrf", reply.Csrf, 32)
	wantTSWindow(t, "签发", "expires", reply.Expires, now+30*86400, nowUnix()+30*86400+5)

	s := sessionOf(t, e, reply.Token)
	wantEQ(t, "会话落库", "mid", s.Mid, mid)
	wantEQ(t, "会话落库", "status", s.Status, model.SessionStatusActive)
	wantEQ(t, "会话落库", "csrf", s.Csrf, reply.Csrf)
	wantEQ(t, "会话落库", "create_ip", s.CreateIP, "1.2.3.4")
	wantEQ(t, "会话落库", "device", s.Device, "pc")
	wantEQ(t, "会话落库", "buvid", s.Buvid, "BV123")
	wantTSWindow(t, "会话落库", "ctime", s.CTime, now, nowUnix()+5)

	// 两条缓存都是同一份会话，TTL = tokenCacheTTL(600)
	var ak model.AccountSession
	if !st.cache.jsonOf("ak_"+reply.Token, &ak) {
		t.Fatalf("ak_ 缓存未写入")
	}
	wantEQ(t, "ak_ 缓存", "refresh_token", ak.RefreshToken, reply.RefreshToken)
	var rk model.AccountSession
	if !st.cache.jsonOf("rk_"+reply.RefreshToken, &rk) {
		t.Fatalf("rk_ 缓存未写入")
	}
	wantEQ(t, "rk_ 缓存", "token", rk.Token, reply.Token)
	for _, k := range []string{"ak_" + reply.Token, "rk_" + reply.RefreshToken} {
		if ttl, ok := st.cache.ttlOf(k); !ok || ttl != 600 {
			t.Errorf("%s TTL = %d(存在=%v), want 600", k, ttl, ok)
		}
	}

	ll := st.loginLog.only()
	wantEQ(t, "成功日志", "mid", ll.Mid, mid)
	wantEQ(t, "成功日志", "login_type", ll.LoginType, model.LoginTypePassword)
	wantEQ(t, "成功日志", "status", ll.Status, model.LoginStatusOK)
	wantEQ(t, "成功日志", "reason", ll.Reason, "")
	wantEQ(t, "成功日志", "buvid", ll.Buvid, "BV123")

	// 口令与哈希不得进缓存、也不得进日志
	secs := st.secret.rowsOf(mid)
	wantNotContains(t, "缓存不得含哈希", st.cache.allText(), secs[0].Hash)
	wantNotContains(t, "日志不得含密码", ll.Reason, "S3cret!")
	wantNoOpsWith(t, "登录链路", e.ops(0), "userProfile.")
	wantNoOpsWith(t, "登录链路", e.ops(0), "socialGraph.")
}

// TestPasswordLoginIdentifierTypeLookupOrder 标识形态与反查顺序：手机号在第 2 类命中
// （用户名先查），邮箱要查满三类；命中即停，不再往下查。
func TestPasswordLoginIdentifierTypeLookupOrder(t *testing.T) {
	cases := []struct {
		name    string
		account string
		cred    int8
		lookups []string
	}{
		{
			name: "手机号", account: "13800138000", cred: model.CredentialTypePhone,
			lookups: []string{
				"cred.FindMidsByIdentifiers:1/13800138000",
				"cred.FindMidsByIdentifiers:2/13800138000",
			},
		},
		{
			name: "邮箱", account: "alice@example.com", cred: model.CredentialTypeEmail,
			lookups: []string{
				"cred.FindMidsByIdentifiers:1/alice@example.com",
				"cred.FindMidsByIdentifiers:2/alice@example.com",
				"cred.FindMidsByIdentifiers:3/alice@example.com",
			},
		},
		{
			name: "用户名", account: "alice", cred: model.CredentialTypeUsername,
			lookups: []string{"cred.FindMidsByIdentifiers:1/alice"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			mid := seedUser(t, e.st, tc.account, "S3cret!", tc.cred)
			e.st.log.reset()

			reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: tc.account, Password: "S3cret!"})
			wantNoErr(t, "登录", err)
			wantEQ(t, "命中的 mid", "mid", reply.Mid, mid)
			wantOps(t, "反查到取密钥这一段", e.ops(0)[:len(tc.lookups)+1],
				append(append([]string{}, tc.lookups...), "secret.FindActive:"+itoa(mid)+"/1"))
			wantCount(t, "命中即停，不再查下一类凭证", e.ops(0), "cred.FindMidsByIdentifiers", len(tc.lookups))
		})
	}
}

// TestPasswordLoginTrimsAndFoldsCase 入参归一：首尾空格与大小写不影响命中，
// 反查键一律用小写（SQL 侧是 LOWER(identifier) IN (?)）。
func TestPasswordLoginTrimsAndFoldsCase(t *testing.T) {
	e := newEnv(t)
	mid := seedUser(t, e.st, "Alice", "S3cret!", model.CredentialTypeUsername)
	e.st.log.reset()

	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "  ALICE ", Password: "S3cret!"})
	wantNoErr(t, "大小写+空格归一的登录", err)
	wantEQ(t, "归一后命中同一 mid", "mid", reply.Mid, mid)
	wantOps(t, "归一后的反查键", e.ops(0)[:1], []string{"cred.FindMidsByIdentifiers:1/alice"})
}

// TestPasswordLoginUnknownAccountWritesFailLog 凭证不存在：三类查满后返回领域错误，
// 并且审计日志记在 mid=0 上（缺口：不知道是谁，但必须留下尝试痕迹）。
func TestPasswordLoginUnknownAccountWritesFailLog(t *testing.T) {
	e := newEnv(t)
	seedUser(t, e.st, "someoneelse", "S3cret!", model.CredentialTypeUsername)
	e.st.log.reset()

	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "ghost", Password: "S3cret!", Ip: "8.8.8.8"})
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	wantErrIs(t, "标识不存在", err, ErrLoginAccountNotExist)
	wantOps(t, "未命中的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/ghost",
		"cred.FindMidsByIdentifiers:2/ghost",
		"cred.FindMidsByIdentifiers:3/ghost",
		"loginlog.Add:1/0/1/1",
	})
	ll := e.st.loginLog.only()
	wantEQ(t, "未命中日志的 mid", "mid", ll.Mid, int64(0))
	wantEQ(t, "未命中日志的 reason", "reason", ll.Reason, "account not exist")
	wantEQ(t, "未命中日志的 ip", "ip", ll.IP, "8.8.8.8")
	wantEQ(t, "未命中不得签发会话", "session 行数", e.st.session.count(), 0)
	wantNoOpsWith(t, "未命中路径", e.ops(0), "secret.FindActive")
}

// TestPasswordLoginEmptyAccount 入参校验边界：空/纯空格标识不做任何反查，
// 但仍会落一条失败日志（与 Register 的「零调用零日志」相反）。
func TestPasswordLoginEmptyAccount(t *testing.T) {
	for _, acct := range []string{"", "  ", "\t"} {
		t.Run("account="+strings.TrimSpace(acct), func(t *testing.T) {
			e := newEnv(t)
			_, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: acct, Password: "S3cret!"})
			wantErrIs(t, "空标识登录", err, ErrLoginAccountNotExist)
			wantOps(t, "空标识的调用序列", e.ops(0), []string{"loginlog.Add:1/0/1/1"})
		})
	}
}

// TestPasswordLoginWrongPassword 密码错误：错误信息不区分「没有密码」与「密码不对」，
// 也不回显任何口令片段。
func TestPasswordLoginWrongPassword(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.log.reset()

	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "wrong-pass"})
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	wantErrIs(t, "密码错误", err, ErrLoginPasswordWrong)
	wantOps(t, "密码错误的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"secret.FindActive:" + itoa(mid) + "/1",
		"loginlog.Add:1/" + itoa(mid) + "/1/1",
	})
	ll := st.loginLog.only()
	wantEQ(t, "失败日志 reason", "reason", ll.Reason, "password wrong")
	wantNotContains(t, "失败日志不得含入参口令", ll.Reason, "wrong-pass")
	wantEQ(t, "密码错误不得签发会话", "session 行数", st.session.count(), 0)
	wantNoOpsWith(t, "密码错误路径", e.ops(0), "session.Insert")
	wantNoOpsWith(t, "密码错误路径", e.ops(0), "cache.SetJSON")

	// 空密码同样是「password wrong」（不能因为哈希算得出就放行）
	st.log.reset()
	_, err = callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: ""})
	wantErrIs(t, "空密码登录已有口令的账号", err, ErrLoginPasswordWrong)
}

// TestPasswordLoginAccountWithoutSecretIsPasswordWrong 账号存在但从未设置密码：
// 走的是同一条「password wrong」分支（不泄露「该账号未设密码」）。
func TestPasswordLoginAccountWithoutSecretIsPasswordWrong(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := int64(70002)
	st.account.put(&model.Account{Mid: mid, Status: 0})
	st.cred.put(&model.AccountCredential{
		Mid: mid, CredentialType: model.CredentialTypeUsername, Identifier: "nostatus", Status: 0,
	})
	st.log.reset()

	_, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "nostatus", Password: "anything"})
	wantErrIs(t, "无密钥账号登录", err, ErrLoginPasswordWrong)
	ll := st.loginLog.only()
	wantEQ(t, "无密钥账号的失败原因与密码错误一致", "reason", ll.Reason, "password wrong")
}

// TestPasswordLoginNoFailureLockout 缺口 9a：密码登录完全没有失败计数/锁定
// （只有验证码有 cap_err_*），连错 5 次后仍能立刻登对。
func TestPasswordLoginNoFailureLockout(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.log.reset()

	for i := 0; i < 5; i++ {
		_, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "guess"})
		wantErrIs(t, "第 N 次猜密码", err, ErrLoginPasswordWrong)
	}
	wantNoOpsWith(t, "密码登录路径不得出现任何计数", e.ops(0), "cache.Incr")
	wantNoOpsWith(t, "密码登录路径不得出现任何计数", e.ops(0), "cap_err")
	wantCount(t, "每次猜错都要留一条失败日志", e.ops(0), "loginlog.Add", 5)
	wantEQ(t, "失败日志条数", "loginlog 行数", st.loginLog.count(), 5)

	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!"})
	wantNoErr(t, "连错 5 次之后仍能登对（无锁定）", err)
	wantEQ(t, "第 6 次登录的 mid", "mid", reply.Mid, mid)
	wantEQ(t, "5 次失败 + 5 次会话写入前的会话数", "session 行数", st.session.count(), 1)
}

// TestPasswordLoginIgnoresLoginTypeField 入参 LoginType 不参与判定：
// 网关把 login_type 传成 2（验证码）也照样按密码校验走，capture_code 被彻底忽略。
func TestPasswordLoginIgnoresLoginTypeField(t *testing.T) {
	e := newEnv(t)
	seedUser(t, e.st, "alice", "S3cret!", model.CredentialTypeUsername)
	e.st.log.reset()

	_, err := callPasswordLogin(t, e, &rpc.LoginReq{
		Account: "alice", Password: "S3cret!", LoginType: int32(model.LoginTypeCapture), CaptureCode: "000000",
	})
	wantNoErr(t, "LoginType=2 仍走密码校验", err)
	wantNoOpsWith(t, "PasswordLogin", e.ops(0), "cap_code")
	wantNoOpsWith(t, "PasswordLogin", e.ops(0), "cache.GetInt")
}

// TestPasswordLoginNeverChecksAccountStatus 缺口 14（安全面最要紧的一条）：
// 登录链路从不读 account 主表，封禁(status=1) 与注销中(status=2) 的账号照样拿到 token。
func TestPasswordLoginNeverChecksAccountStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int32
	}{
		{"封禁", 1},
		{"注销中", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
			acc := st.account.get(mid)
			acc.Status = tc.status
			st.account.put(acc)
			st.log.reset()

			reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!"})
			wantNoErr(t, tc.name+"账号仍能登录（缺口 14）", err)
			wantEQ(t, "登录拿到的 mid", "mid", reply.Mid, mid)
			wantNoOpsWith(t, "登录链路从不查账号主表", e.ops(0), "account.FindOne")
			wantEQ(t, "被封禁账号仍签发了会话", "session 行数", st.session.count(), 1)
			// 该 token 在鉴权侧同样有效（TokenInfo 也不查主表，见 tokeninfologic_test.go）
			info, err := NewTokenInfoLogic(context.Background(), e.svcCtx).TokenInfo(
				&rpc.GetTokenInfoReq{Token: reply.Token})
			wantNoErr(t, "封禁账号的 token 校验", err)
			wantEQ(t, "封禁账号的 token 仍判定为已登录（缺口 14 放大面）", "is_login", info.IsLogin, true)
		})
	}
}

// TestPasswordLoginUnboundCredentialStillWorks 缺口 2：FindMidsByIdentifiers 的 SQL
// 没有 status = 0 过滤（model/account_credential.go:103），所以已解绑的标识仍反查到 mid。
func TestPasswordLoginUnboundCredentialStillWorks(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	// 把凭证置为「已解绑」（DDL 注释要求反查过滤掉这类行）
	st.cred.setStatus(mid, model.CredentialTypeUsername, 1)
	st.log.reset()

	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!"})
	wantNoErr(t, "已解绑标识仍能登录（缺口 2 的现状）", err)
	wantEQ(t, "解绑后仍能拿到原 mid", "mid", reply.Mid, mid)
	wantNoOpsWith(t, "解绑判定根本不存在", e.ops(0), "account.FindOne")
}

// TestPasswordLoginSecretLookupFaultPropagatesWithoutLog 缺口 9b：
// secret.FindActive 故障时错误直接外传，**不写任何登录日志**（审计面出现空洞）。
func TestPasswordLoginSecretLookupFaultPropagatesWithoutLog(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.secret.failWith("FindActive", errors.New("account/db: secret lookup down"))
	st.log.reset()

	_, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!"})
	wantErr(t, "密钥查询故障", err)
	if !strings.Contains(err.Error(), "secret lookup down") {
		t.Errorf("错误 = %v, want 原样外传底层错误", err)
	}
	wantOps(t, "故障路径的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"secret.FindActive:70001/1",
	})
	wantEQ(t, "密钥故障不写登录日志（缺口 9b）", "loginlog 行数", st.loginLog.count(), 0)
	wantEQ(t, "密钥故障不得签发会话", "session 行数", st.session.count(), 0)
}

// TestPasswordLoginCredentialLookupFaultLeaksRawErrorToClientAndAudit 反查阶段的 DB 故障：
// 错误原样外传给 gRPC 客户端（与「标识不存在」共用同一条 addLoginLog 分支），
// 于是底层故障文本被写进审计日志的 reason 字段（缺口 10 同源：不做错误映射）。
// 注意这与 Register 不同——Register 的同一分支不写日志（见 registerlogic_test.go）。
func TestPasswordLoginCredentialLookupFaultLeaksRawErrorToClientAndAudit(t *testing.T) {
	e := newEnv(t)
	st := e.st
	boom := errors.New("account/db: cred lookup down")
	st.cred.failWith("FindMidsByIdentifiers", boom)
	st.log.reset()

	_, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!", Ip: "1.1.1.1"})
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v（原样外传）", err, boom)
	}
	wantOps(t, "反查故障的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"loginlog.Add:1/0/1/1",
	})
	ll := st.loginLog.only()
	wantEQ(t, "反查故障也会写日志（与 Register 相反）", "reason", ll.Reason, boom.Error())
	wantEQ(t, "反查故障日志的 mid", "mid", ll.Mid, int64(0))
	wantEQ(t, "反查故障不得签发会话", "session 行数", st.session.count(), 0)
}

// TestPasswordLoginSessionInsertFailureLogsErrorText 会话落库失败：错误外传、
// 失败日志的 reason 是底层错误原文（缺口 10 同类：不映射成领域错误）。
func TestPasswordLoginSessionInsertFailureLogsErrorText(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	boom := errors.New("Error 1062: Duplicate entry 'tok' for key 'account_session.uk_token'")
	st.session.failWith("Insert", boom)
	st.log.reset()

	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!"})
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	wantOps(t, "会话写失败的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"secret.FindActive:" + itoa(mid) + "/1",
		"session.Insert:1/" + itoa(mid),
		"loginlog.Add:1/" + itoa(mid) + "/1/1",
	})
	wantEQ(t, "失败日志 reason 是底层错误原文", "reason", st.loginLog.only().Reason, boom.Error())
	wantNoOpsWith(t, "会话写失败", e.ops(0), "cache.SetJSON")
}

// TestPasswordLoginRepeatedLoginsIssueIndependentSessions 重放：同一条口令重复登录
// 每次都是一个新会话（无去重、无会话上限），且 token 互不相同。
func TestPasswordLoginRepeatedLoginsIssueIndependentSessions(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.log.reset()

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: "S3cret!", Device: "d"})
		wantNoErr(t, "重复登录", err)
		if seen[reply.Token] {
			t.Fatalf("第 %d 次登录拿到了重复 token %s…", i+1, reply.Token[:8])
		}
		seen[reply.Token] = true
		wantEQ(t, "每次登录都落一条会话", "session 行数", st.session.count(), i+1)
		wantEQ(t, "每次登录都落一条日志", "loginlog 行数", st.loginLog.count(), i+1)
	}
	wantCount(t, "3 次登录写 6 条会话缓存", e.ops(0), "cache.SetJSON:ak_", 3)
	wantCount(t, "3 次登录写 6 条会话缓存", e.ops(0), "cache.SetJSON:rk_", 3)
	// 三个会话都各自有效
	for tok := range seen {
		info, err := NewTokenInfoLogic(context.Background(), e.svcCtx).TokenInfo(&rpc.GetTokenInfoReq{Token: tok})
		wantNoErr(t, "token 校验", err)
		wantEQ(t, "重复登录签发的 token 都有效", "is_login", info.IsLogin, true)
	}
}

// TestPasswordLoginWithRSAWrongKeyLeaksCryptoError 缺口 10：生产配了 RSA 时，
// 密码解不开就把底层 crypto/base64 错误原样返回给 gRPC 客户端，并写进审计日志。
func TestPasswordLoginWithRSAWrongKeyLeaksCryptoError(t *testing.T) {
	e := newEnv(t, withPassportRSA())
	st := e.st
	// 用配对的公钥登记口令：先按明文注册一条密钥（RSA 分支之外手工布）
	mid := seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.log.reset()

	// 用另一把不配对的公钥加密 → 私钥解不开
	bad := encryptWithOtherKey(t, "S3cret!")
	reply, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: bad})
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	wantErr(t, "错配密钥的密码登录", err)
	if errors.Is(err, ErrLoginPasswordWrong) {
		t.Errorf("错配密钥被映射成了领域错误，与实现不符（应原样外传）：%v", err)
	}
	wantNotContains(t, "错误原文不得回显口令", err.Error(), "S3cret!")
	wantOps(t, "解密失败的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"loginlog.Add:1/" + itoa(mid) + "/1/1",
	})
	wantEQ(t, "解密失败不得读密钥", "secret 行数被读", countPrefix(e.ops(0), "secret.FindActive"), 0)
	wantEQ(t, "解密失败不签发会话", "session 行数", st.session.count(), 0)
	// 审计日志里存的是原始 crypto 错误文本（不是 "password wrong"）
	if r := st.loginLog.only().Reason; r == "password wrong" || r == "" {
		t.Errorf("失败日志 reason = %q, want 底层解密错误原文（缺口 10）", r)
	}

	// 对照：配对公钥加密的密文可正常登录
	st.log.reset()
	cipher, encErr := testPassportRSA.CardEncrypt([]byte("S3cret!"))
	wantNoErr(t, "测试侧 RSA 加密", encErr)
	ok, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: string(cipher)})
	wantNoErr(t, "RSA 密文登录", err)
	wantEQ(t, "RSA 密文登录的 mid", "mid", ok.Mid, mid)
}

// TestPasswordLoginWithRSAStripsEmptyPassword RSA 分支下空密码短路（不解密、不报错），
// 因此落到「password wrong」而不是 crypto 错误。
func TestPasswordLoginWithRSAStripsEmptyPassword(t *testing.T) {
	e := newEnv(t, withPassportRSA())
	st := e.st
	seedUser(t, st, "alice", "S3cret!", model.CredentialTypeUsername)
	st.log.reset()

	_, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: "alice", Password: ""})
	wantErrIs(t, "RSA 分支下的空密码", err, ErrLoginPasswordWrong)
	wantOps(t, "空密码不调解密器也不查密钥", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
		"secret.FindActive:70001/1",
		"loginlog.Add:1/70001/1/1",
	})
}
