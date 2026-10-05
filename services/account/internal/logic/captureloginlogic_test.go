package logic

// captureloginlogic_test.go 覆盖 CaptureLogin（repository/login.go:288-305）。
//
// 被测判定链：checkCapture(biz=1 登录, **req.Account 原样**, req.CaptureCode)
// → findMidByAccount(**小写化+TrimSpace 之后**的 Account，按用户名/手机/邮箱三类依次反查)
// → issueSession（落 account_session + 回填 ak_/rk_ 缓存）→ 写登录日志。
//
// 钉住的关键事实：
//   - 验证码校验**先于**账号反查：码不对时连 cred 都不查，会话与缓存零副作用；
//   - 全程不读 account_secret（验证码路径与口令无关），也不读 account 主表；
//   - 入参 LoginType / Password 都不参与判定（LoginType=1 也照样走验证码）；
//   - 失败日志一律 mid=0（还没确定是谁）、login_type=2、reason 为领域错误原文；
//   - 缺口 C（登录侧实锤）：校验成功不删 cap_code，同一份码在 10 分钟内可反复换会话；
//   - 缺口 F：cap_code 的 key 用**未归一**的 Account，反查却用归一后的串——
//     客户端多带一个空格就永远「验证码无效」，而同一份标识去掉空格又能正常登录。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callCaptureLogin(t *testing.T, e *env, in *rpc.LoginReq) (*rpc.LoginReply, error) {
	t.Helper()
	return NewCaptureLoginLogic(context.Background(), e.svcCtx).CaptureLogin(in)
}

const capturePhone = "13800138000"

// captureLoginCheckOps 验证码那一步的两次读（key 派生与 login.go:81-91 一致）。
func captureLoginCheckOps(target string) []string {
	return []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizLogin, target),
		"cache.GetInt:" + capCodeKey(model.CaptureBizLogin, target),
	}
}

// TestCaptureLoginHappyPathIssuesSessionLikePasswordLogin 正确码 + 已注册手机号：
// 两次读码 → 两类凭证反查（用户名先查、手机在第 2 类命中）→ 签发会话 → 两条缓存 → 一条成功日志。
func TestCaptureLoginHappyPathIssuesSessionLikePasswordLogin(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	// 刻意用含非 hex 数字（7）的验证码：下面的「应答不得回显验证码」要拿它去
	// 在 hex 形态的 token/csrf 里找，用 123456 会因为随机 hex 里真出现该串而偶发误红。
	seedCapture(st, model.CaptureBizLogin, capturePhone, "177777")
	st.log.reset()
	now := nowUnix()

	reply, err := callCaptureLogin(t, e, &rpc.LoginReq{
		Account: capturePhone, CaptureCode: "177777",
		LoginType: int32(model.LoginTypeCapture), Ip: "1.2.3.4", Device: "android", Buvid: "BV777",
	})
	wantNoErr(t, "验证码登录", err)
	wantOps(t, "验证码登录调用序列", e.ops(0), append(captureLoginCheckOps(capturePhone),
		"cred.FindMidsByIdentifiers:1/"+capturePhone,
		"cred.FindMidsByIdentifiers:2/"+capturePhone,
		"session.Insert:1/"+itoa(mid),
		"cache.SetJSON:ak_"+reply.Token+"/600",
		"cache.SetJSON:rk_"+reply.RefreshToken+"/600",
		"loginlog.Add:1/"+itoa(mid)+"/2/0",
	))

	wantEQ(t, "应答", "mid", reply.Mid, mid)
	wantHexLen(t, "签发", "token", reply.Token, 64)
	wantHexLen(t, "签发", "refresh_token", reply.RefreshToken, 64)
	wantHexLen(t, "签发", "csrf", reply.Csrf, 32)
	wantTSWindow(t, "签发", "expires", reply.Expires, now+30*86400, nowUnix()+30*86400+5)

	s := sessionOf(t, e, reply.Token)
	wantEQ(t, "会话落库", "mid", s.Mid, mid)
	wantEQ(t, "会话落库", "status", s.Status, model.SessionStatusActive)
	wantEQ(t, "会话落库", "csrf", s.Csrf, reply.Csrf)
	wantEQ(t, "会话落库", "create_ip", s.CreateIP, "1.2.3.4")
	wantEQ(t, "会话落库", "device", s.Device, "android")
	wantEQ(t, "会话落库", "buvid", s.Buvid, "BV777")
	wantTSWindow(t, "会话落库", "refresh_expires", s.RefreshExpires, now+90*86400, nowUnix()+90*86400+5)

	ll := st.loginLog.only()
	wantEQ(t, "成功日志", "mid", ll.Mid, mid)
	wantEQ(t, "成功日志", "login_type", ll.LoginType, model.LoginTypeCapture)
	wantEQ(t, "成功日志", "status", ll.Status, model.LoginStatusOK)
	wantEQ(t, "成功日志", "reason", ll.Reason, "")
	wantNotContains(t, "应答不得回显验证码", fmt.Sprintf("%+v", reply), "177777")

	// 验证码路径与口令无关：一次都不读密钥
	wantNoOpsWith(t, "验证码登录", e.ops(0), "secret.")
	wantNoOpsWith(t, "验证码登录", e.ops(0), "account.FindOne")
	wantNoOpsWith(t, "验证码登录", e.ops(0), "userProfile.")
	wantNoOpsWith(t, "验证码登录", e.ops(0), "socialGraph.")
}

// TestCaptureLoginWrongCodeHasNoSessionSideEffects 码错：ErrCaptureWrong 必须在反查账号**之前**
// 就把链路掐断（cred/secret/session 一步都不许发生），审计记在 mid=0 上。
func TestCaptureLoginWrongCodeHasNoSessionSideEffects(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, model.CaptureBizLogin, capturePhone, "123456")
	st.log.reset()

	reply, err := callCaptureLogin(t, e, &rpc.LoginReq{
		Account: capturePhone, CaptureCode: "000000", Password: "S3cret!", Ip: "9.9.9.9",
	})
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	wantErrIs(t, "错误验证码登录", err, ErrCaptureWrong)
	wantOps(t, "错误码的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizLogin, capturePhone),
		"cache.GetInt:" + capCodeKey(model.CaptureBizLogin, capturePhone),
		"cache.Incr:" + capErrKey(model.CaptureBizLogin, capturePhone),
		"cache.GetInt:" + capErrKey(model.CaptureBizLogin, capturePhone),
		"cache.Expire:" + capErrKey(model.CaptureBizLogin, capturePhone) + "/86400",
		"loginlog.Add:1/0/2/1",
	})
	wantEQ(t, "错误码不得签发会话", "session 行数", st.session.count(), 0)
	wantNoOpsWith(t, "错误码路径", e.ops(0), "cred.")
	wantNoOpsWith(t, "错误码路径", e.ops(0), "secret.")
	wantNoOpsWith(t, "错误码路径", e.ops(0), "cache.SetJSON")
	ll := st.loginLog.only()
	wantEQ(t, "错误码日志的 mid（身份尚未确定）", "mid", ll.Mid, int64(0))
	wantEQ(t, "错误码日志的 login_type", "login_type", ll.LoginType, model.LoginTypeCapture)
	wantEQ(t, "错误码日志的 reason", "reason", ll.Reason, "capture code wrong")
	wantNotContains(t, "失败日志不得含口令", ll.Reason, "S3cret!")

	// 边界对：计数到 1 仍远未到锁死线，同一份正确码随后照样能登进去
	st.log.reset()
	ok, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456"})
	wantNoErr(t, "一次猜错之后用正确码登录", err)
	wantEQ(t, "随后登录拿到的 mid", "mid", ok.Mid, int64(70001))
	wantOps(t, "恢复后的调用序列", e.ops(0), append(captureLoginCheckOps(capturePhone),
		"cred.FindMidsByIdentifiers:1/"+capturePhone,
		"cred.FindMidsByIdentifiers:2/"+capturePhone,
		"session.Insert:1/70001",
		"cache.SetJSON:ak_"+ok.Token+"/600",
		"cache.SetJSON:rk_"+ok.RefreshToken+"/600",
		"loginlog.Add:2/70001/2/0",
	))
}

// TestCaptureLoginMissingCodeStillWritesAudit 没发过码（或已过期）：ErrCaptureInvalid，
// 零副作用，但仍留一条 mid=0 的失败审计（暴力试码必须可见）。
func TestCaptureLoginMissingCodeStillWritesAudit(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	st.log.reset()

	_, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456", Ip: "8.8.8.8"})
	wantErrIs(t, "未发送验证码就登录", err, ErrCaptureInvalid)
	wantOps(t, "无码路径的调用序列", e.ops(0), append(captureLoginCheckOps(capturePhone),
		"loginlog.Add:1/0/2/1"))
	wantEQ(t, "无码不得签发会话", "session 行数", st.session.count(), 0)
	if st.cache.has(capErrKey(model.CaptureBizLogin, capturePhone)) {
		t.Errorf("没有验证码时不许累计错误次数（一次 Redis 抖动/误输不该把号码锁一天）")
	}
	wantEQ(t, "无码审计的 reason", "reason", st.loginLog.only().Reason, "capture code invalid")
}

// TestCaptureLoginCodeValidButAccountUnknown 码对、标识查不到：ErrLoginAccountNotExist，
// 三类凭证查满才认输；验证码**不消费**（缺口 C 的放大面：攻击者可以用同一个有效码
// 反复枚举标识，直到 10 分钟 TTL 自然到期）。
func TestCaptureLoginCodeValidButAccountUnknown(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "13900139000", "S3cret!", model.CredentialTypePhone)
	seedCapture(st, model.CaptureBizLogin, capturePhone, "123456")
	st.log.reset()

	reply, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456"})
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	wantErrIs(t, "码对但标识不存在", err, ErrLoginAccountNotExist)
	wantOps(t, "三类凭证查满才认输", e.ops(0), append(captureLoginCheckOps(capturePhone),
		"cred.FindMidsByIdentifiers:1/"+capturePhone,
		"cred.FindMidsByIdentifiers:2/"+capturePhone,
		"cred.FindMidsByIdentifiers:3/"+capturePhone,
		"loginlog.Add:1/0/2/1",
	))
	wantEQ(t, "查不到账号不得签发会话", "session 行数", st.session.count(), 0)
	if v, ok := st.cache.intOf(capCodeKey(model.CaptureBizLogin, capturePhone)); !ok || v != 123456 {
		t.Errorf("验证码应仍未被消费（现状：%d 存在=%v，缺口 C）", v, ok)
	}
	wantEQ(t, "审计 reason 是标识不存在", "reason", st.loginLog.only().Reason, "account not exist")
}

// TestCaptureLoginTooManyErrorsShortCircuitsEverything 超限：Del 掉码、不反查账号、
// 不签会话，审计 reason 为超限原文。
func TestCaptureLoginTooManyErrorsShortCircuitsEverything(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, model.CaptureBizLogin, capturePhone, "123456")
	st.cache.warmInt(capErrKey(model.CaptureBizLogin, capturePhone), 4)
	st.log.reset()

	_, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456"})
	wantErrIs(t, "超限后即使码正确也拒绝", err, ErrCaptureErrTooMany)
	wantOps(t, "超限路径的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizLogin, capturePhone),
		"cache.Del:" + capCodeKey(model.CaptureBizLogin, capturePhone),
		"loginlog.Add:1/0/2/1",
	})
	wantNoOpsWith(t, "超限路径", e.ops(0), "cred.")
	wantEQ(t, "超限不得签发会话", "session 行数", st.session.count(), 0)
	if st.cache.has(capCodeKey(model.CaptureBizLogin, capturePhone)) {
		t.Errorf("超限后验证码必须已被销毁")
	}
	wantEQ(t, "超限审计 reason", "reason", st.loginLog.only().Reason, "capture code error too many")
}

// TestCaptureLoginDoesNotConsumeCodeOnSuccess 缺口 C 的登录侧实锤：登录成功之后
// 同一份码立刻还能再登录一次（会话数翻倍），10 分钟窗口内可无限重放。
func TestCaptureLoginDoesNotConsumeCodeOnSuccess(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, model.CaptureBizLogin, capturePhone, "123456")
	st.log.reset()

	first, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456"})
	wantNoErr(t, "首次验证码登录", err)
	st.log.reset()

	second, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456"})
	wantNoErr(t, "用同一份码重放登录（现状：不消费）", err)
	wantOps(t, "重放路径与首次同构（没有 Del cap_code）", e.ops(0), append(captureLoginCheckOps(capturePhone),
		"cred.FindMidsByIdentifiers:1/"+capturePhone,
		"cred.FindMidsByIdentifiers:2/"+capturePhone,
		"session.Insert:2/"+itoa(mid),
		"cache.SetJSON:ak_"+second.Token+"/600",
		"cache.SetJSON:rk_"+second.RefreshToken+"/600",
		"loginlog.Add:2/"+itoa(mid)+"/2/0",
	))
	wantEQ(t, "两个会话是两条独立记录", "session 行数", st.session.count(), 2)
	if first.Token == second.Token {
		t.Errorf("两次登录拿到了同一个 token：%s", first.Token)
	}
	// 对照：真正会消费码的是 ResetPassword（成功后 Del cap_code_3_*，见 resetpasswordlogic_test.go），
	// 说明「消费」这件事在本仓库是逐方法手工加的，登录路径漏了。
	if v, ok := st.cache.intOf(capCodeKey(model.CaptureBizLogin, capturePhone)); !ok || v != 123456 {
		t.Errorf("前置条件破坏：cap_code = %d(存在=%v)", v, ok)
	}
}

// TestCaptureLoginUsesRawAccountForCaptureKey 缺口 F 的边界对：验证码 key 用原样 Account，
// 标识反查却先 TrimSpace+小写化——带空格的同一标识会「验证码无效」，去空格才登得上。
func TestCaptureLoginUsesRawAccountForCaptureKey(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, model.CaptureBizLogin, capturePhone, "123456")
	st.log.reset()

	_, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: " " + capturePhone, CaptureCode: "123456"})
	wantErrIs(t, "Account 多带一个空格", err, ErrCaptureInvalid)
	wantOps(t, "查的是带空格的 key", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizLogin, " "+capturePhone),
		"cache.GetInt:" + capCodeKey(model.CaptureBizLogin, " "+capturePhone),
		"loginlog.Add:1/0/2/1",
	})
	wantNoOpsWith(t, "码没对上就不该去反查账号", e.ops(0), "cred.")

	st.log.reset()
	ok, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456"})
	wantNoErr(t, "去掉空格后同一份码立即可用", err)
	wantEQ(t, "登录 mid", "mid", ok.Mid, mid)
}

// TestCaptureLoginIgnoresLoginTypeAndPassword 判定只看验证码：
// LoginType 传成 1（密码）、Password 传错口令，都照样走验证码并成功。
func TestCaptureLoginIgnoresLoginTypeAndPassword(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, model.CaptureBizLogin, capturePhone, "123456")
	st.log.reset()

	reply, err := callCaptureLogin(t, e, &rpc.LoginReq{
		Account: capturePhone, Password: "totally-wrong", CaptureCode: "123456",
		LoginType: int32(model.LoginTypePassword),
	})
	wantNoErr(t, "LoginType=1 仍走验证码通道", err)
	wantEQ(t, "mid", "mid", reply.Mid, mid)
	wantNoOpsWith(t, "验证码通道绝不比对口令", e.ops(0), "secret.")
	wantNoOpsWith(t, "验证码通道绝不比对口令", e.ops(0), "loginlog.Add:1/"+itoa(mid)+"/1/")
	// 日志写的是「验证码」而不是入参的 LoginType=1（login_type 由实现决定，不信客户端）
	wantEQ(t, "日志里的登录方式", "login_type", st.loginLog.only().LoginType, model.LoginTypeCapture)
}

// TestCaptureLoginAcceptsAnyIdentifierShape 不校验标识形态：用户名标识只要与
// cap_code_1_<标识> 对得上就能登录（验证码通道并不限定手机号）。
func TestCaptureLoginAcceptsAnyIdentifierShape(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const account = "alice"
	mid := seedUser(t, st, account, "S3cret!", model.CredentialTypeUsername)
	seedCapture(st, model.CaptureBizLogin, account, "654321")
	st.log.reset()

	reply, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: account, CaptureCode: "654321"})
	wantNoErr(t, "用户名标识的验证码登录", err)
	wantEQ(t, "mid", "mid", reply.Mid, mid)
	wantOps(t, "用户名在第 1 类凭证即命中", e.ops(0)[:3], []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizLogin, account),
		"cache.GetInt:" + capCodeKey(model.CaptureBizLogin, account),
		"cred.FindMidsByIdentifiers:1/alice",
	})
}

// TestCaptureLoginSessionInsertFailureAuditAndNoCache 会话落库失败：底层错误原样外传、
// 审计 reason 是原始错误文本（缺口 10 同源），且不写任何会话缓存。
func TestCaptureLoginSessionInsertFailureAuditAndNoCache(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, model.CaptureBizLogin, capturePhone, "123456")
	boom := errors.New("Error 1406: Data too long for column device")
	st.session.failWith("Insert", boom)
	st.log.reset()

	reply, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: capturePhone, CaptureCode: "123456"})
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样外传 %v", err, boom)
	}
	wantOps(t, "会话写失败的调用序列", e.ops(0), append(captureLoginCheckOps(capturePhone),
		"cred.FindMidsByIdentifiers:1/"+capturePhone,
		"cred.FindMidsByIdentifiers:2/"+capturePhone,
		"session.Insert:1/"+itoa(mid),
		"loginlog.Add:1/"+itoa(mid)+"/2/1",
	))
	wantNoOpsWith(t, "会话写失败不得回填缓存", e.ops(0), "cache.SetJSON")
	wantEQ(t, "失败审计的 mid（此时已知是谁）", "mid", st.loginLog.only().Mid, mid)
	wantEQ(t, "失败审计 reason 是底层错误原文", "reason", st.loginLog.only().Reason, boom.Error())
	wantEQ(t, "失败审计的登录方式", "login_type", st.loginLog.only().LoginType, model.LoginTypeCapture)
}

// TestCaptureLoginEmptyAccountIsInvalidCode 空标识：不短路成参数错误，而是按
// cap_code_1_（空前缀）这个 key 走一遍校验，失败同样落审计。
func TestCaptureLoginEmptyAccountIsInvalidCode(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, capturePhone, "S3cret!", model.CredentialTypePhone)
	st.log.reset()

	_, err := callCaptureLogin(t, e, &rpc.LoginReq{Account: "", CaptureCode: "123456"})
	wantErrIs(t, "空标识的验证码登录", err, ErrCaptureInvalid)
	wantOps(t, "空标识就是 key 的一部分，不做参数校验", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizLogin, ""),
		"cache.GetInt:" + capCodeKey(model.CaptureBizLogin, ""),
		"loginlog.Add:1/0/2/1",
	})
	wantEQ(t, "空标识不得签发会话", "session 行数", st.session.count(), 0)
}
