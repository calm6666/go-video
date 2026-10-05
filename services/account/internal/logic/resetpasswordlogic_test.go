package logic

// resetpasswordlogic_test.go 覆盖 ResetPassword（repository/login.go:556-589 → logic/resetpasswordlogic.go:28-34）。
//
// 被测判定链：account 归一化（TrimSpace+ToLower）→ checkCapture(biz=3, account) →
// findMidByAccount（用户名/手机/邮箱三种凭证依次反查）→ 新密码非空 →
// 事务内 [MarkHistory → Insert 新密钥] → RevokeAll(mid) → Del 验证码。
//
// 钉住的关键事实：
//   - 判定顺序是「先验证码、后账号存在性」：验证码对了才发现账号不存在（缺口 9b 同源），
//     且此时验证码不消费；
//   - checkCapture 用 `errTimes > 3` 而非 `>=` → 第 4 次错误才封，第 5 次才报「次数过多」；
//     超限那一步会把验证码删掉（只删 code，不删 err 计数）；
//   - 验证码只在**全流程成功**时才消费：事务失败、新密码为空都会留下可重用的码（缺口 8 同源）；
//   - RevokeAll 与 Del 的错误都被 `_ =` 吞掉（缺口 5 / 7）；
//   - 全程不写任何登录/验证码日志，ip 入参丢弃（缺口 9d / 15）；
//   - ResetPassword 把 account 小写化后再拼 Redis key，而 SendCapture 只 TrimSpace
//     （login.go:452 vs login.go:557）→ 邮箱大小写不一致时**永远校验不过**（缺口 11）；
//   - 与 SetPassword 共用 uk_mid_type_status，第一次重置之后就再也重置不了（缺口 3）。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callResetPassword(t *testing.T, e *env, account, code, newPwd, ip string) (*rpc.DelCacheReply, error) {
	t.Helper()
	return NewResetPasswordLogic(context.Background(), e.svcCtx).ResetPassword(
		&rpc.ResetPasswordReq{Account: account, CaptureCode: code, NewPassword: newPwd, Ip: ip})
}

// recoveryBiz 直接取生产的枚举值，不写死 3：key 派生与 biz 口径漂移时用例即红。
var recoveryBiz = model.CaptureBizRecovery

// TestResetPasswordHappyPath 正常重置：八步顺序（读错误计数 → 读验证码 → 两次凭证反查 →
// 置历史 → 插新密钥 → 全量吊销 → 删验证码）、新密码可用、旧会话全吊销、验证码被消费。
func TestResetPasswordHappyPath(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, recoveryBiz, phone, "246810")
	sess := login(t, e, phone, "S3cret!", "1.2.3.4", "pc")
	nextID := st.secret.peekID()
	st.log.reset()
	now := nowUnix()

	reply, err := callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	wantNoErr(t, "重置密码", err)
	if reply == nil {
		t.Fatalf("重置成功必须返回非 nil reply")
	}
	wantOps(t, "重置调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capCodeKey(recoveryBiz, phone),
		"cred.FindMidsByIdentifiers:1/" + phone,
		"cred.FindMidsByIdentifiers:2/" + phone,
		"secret.MarkHistory:" + itoa(mid) + "/1",
		"secret.Insert:" + itoa(nextID+1) + "/" + itoa(mid) + "/0",
		"session.RevokeAll:" + itoa(mid),
		"cache.Del:" + capCodeKey(recoveryBiz, phone),
	})
	opened, rolledBack := st.conn.txStats()
	wantEQ(t, "重置开一个事务", "事务数", opened, 1)
	wantEQ(t, "重置不应回滚", "回滚数", rolledBack, 0)

	// 密钥轮换：旧行转历史、新行生效且 salt/hash 口径正确
	wantEQ(t, "重置后密钥行数", "行数", st.secret.count(), 2)
	wantEQ(t, "原密钥转历史", "status", st.secret.rowsOf(mid)[0].Status, model.SecretStatusHistory)
	fresh := activeSecret(t, st, mid)
	wantHexLen(t, "重置后的新密钥", "salt", fresh.Salt, 32)
	wantEQ(t, "新 hash = MD5(pwd+盐+salt)", "hash", fresh.Hash, seedHash("Reset#1", fresh.Salt))
	wantTSWindow(t, "新密钥时间戳", "ctime", fresh.CTime, now, nowUnix()+5)

	// 会话全吊销（DB 口径），但缓存一步都没动（缺口 7 与 SetPassword 同源）
	wantEQ(t, "重置后该 mid 无有效会话", "active", st.session.activeFor(mid), 0)
	wantEQ(t, "重置后原会话行仍有效（DB 已吊销）", "status",
		sessionOf(t, e, sess.Token).Status, model.SessionStatusRevoked)
	wantNoOpsWith(t, "重置链路", e.ops(0), "cache.SetJSON")

	// 验证码消费掉了，错误计数却不清零（下一次尝试仍带着这次的 0 次，无害；
	// 但反过来证明实现没有「成功后重置计数」这一步）
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, phone)); ok {
		t.Errorf("重置成功后验证码必须被删除")
	}

	// 零审计：找回改密既没有登录日志也没有验证码日志，ip 彻底丢弃（缺口 9d/15）
	wantNoOpsWith(t, "重置链路", e.ops(0), "loginlog.Add")
	wantNoOpsWith(t, "重置链路", e.ops(0), "capturelog.Add")
	wantEQ(t, "重置不落登录日志", "loginlog 行数", st.loginLog.count(), 1) // 只剩 login() 那条
	wantEQ(t, "重置不落验证码日志（缺口 15：ip 无人记录）", "capturelog 行数", st.captureLog.count(), 0)

	// 新密码可登录、旧密码失效
	if _, err := callPasswordLogin(t, e, &rpc.LoginReq{Account: phone, Password: "S3cret!",
		LoginType: int32(model.LoginTypePassword), Ip: "1.2.3.4"}); !errors.Is(err, ErrLoginPasswordWrong) {
		t.Errorf("旧密码 = %v, want ErrLoginPasswordWrong", err)
	}
	if got := login(t, e, phone, "Reset#1", "1.2.3.4", "pc"); got.Mid != mid {
		t.Errorf("新密码登录 mid = %d, want %d", got.Mid, mid)
	}
}

// TestResetPasswordTrimsAndLowercasesIdentifier 入参归一化：两侧空格与大小写都归一
// （但归一化只发生在找回侧，见 TestResetPasswordEmailCaseAsymmetry 的不对称缺陷）。
func TestResetPasswordTrimsAndLowercasesIdentifier(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, recoveryBiz, phone, "246810")
	st.log.reset()

	_, err := callResetPassword(t, e, "  "+phone+"  ", "246810", "Reset#1", "1.2.3.4")
	wantNoErr(t, "带空格与 +86 之外的空白标识重置", err)
	wantOps(t, "归一化后使用的 key", e.ops(0)[:2], []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capCodeKey(recoveryBiz, phone),
	})
	wantCount(t, "归一化后仍能反查到账号", e.ops(0), "cred.FindMidsByIdentifiers:2/"+phone, 1)
}

// TestResetPasswordWrongCodeIncrsErrCounterAndTouchesNoDB 验证码不对：ErrCaptureWrong，
// 错误计数 +1 并补 TTL，**不查库、不改密钥、不消费验证码**。
func TestResetPasswordWrongCodeIncrsErrCounterAndTouchesNoDB(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	before := activeSecret(t, st, mid)
	seedCapture(st, recoveryBiz, phone, "246810")
	st.log.reset()

	_, err := callResetPassword(t, e, phone, "000000", "Reset#1", "1.2.3.4")
	wantErrIs(t, "验证码不对", err, ErrCaptureWrong)
	wantOps(t, "验证码错误的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capCodeKey(recoveryBiz, phone),
		"cache.Incr:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.Expire:" + capErrKey(recoveryBiz, phone) + "/86400",
	})
	wantNoOpsWith(t, "验证码错误路径", e.ops(0), "cred.")
	wantNoOpsWith(t, "验证码错误路径", e.ops(0), "secret.")
	if v, ok := st.cache.intOf(capErrKey(recoveryBiz, phone)); !ok || v != 1 {
		t.Errorf("错误计数 = %d(存在=%v), want 1", v, ok)
	}
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, phone)); !ok {
		t.Errorf("验证码错一次就被消费掉了，必须留着让人重试")
	}
	wantEQ(t, "错误码不得改密钥", "hash", activeSecret(t, st, mid).Hash, before.Hash)
	opened, _ := st.conn.txStats()
	wantEQ(t, "错误码不得开事务", "事务数", opened, 0)
}

// TestResetPasswordMissingCodeIsInvalid 没发过验证码（key 不存在）→ ErrCaptureInvalid，
// 只读两个缓存 key，连库都不碰。
func TestResetPasswordMissingCodeIsInvalid(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "13800138000", "S3cret!", model.CredentialTypePhone)
	st.log.reset()

	_, err := callResetPassword(t, e, "13800138000", "246810", "Reset#1", "1.2.3.4")
	wantErrIs(t, "未发送验证码", err, ErrCaptureInvalid)
	wantOps(t, "无验证码的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, "13800138000"),
		"cache.GetInt:" + capCodeKey(recoveryBiz, "13800138000"),
	})
}

// TestResetPasswordErrTooManyBurnsTheCode 错误次数超限（`> 3`，即第 4 次错误起）：
// 直接返回 ErrCaptureErrTooMany，并顺手把验证码删掉（此后重发才能继续）。
// 注意这一步不 Incr，所以计数停在触发值。
func TestResetPasswordErrTooManyBurnsTheCode(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	before := activeSecret(t, st, mid)
	seedCapture(st, recoveryBiz, phone, "246810")
	st.cache.warmInt(capErrKey(recoveryBiz, phone), 4)
	st.log.reset()

	_, err := callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	wantErrIs(t, "错误次数超限", err, ErrCaptureErrTooMany)
	wantOps(t, "超限的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.Del:" + capCodeKey(recoveryBiz, phone),
	})
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, phone)); ok {
		t.Errorf("超限后验证码必须被删除（即便它本来就是对的）")
	}
	if v, ok := st.cache.intOf(capErrKey(recoveryBiz, phone)); !ok || v != 4 {
		t.Errorf("错误计数 = %d(存在=%v), want 停在 4（超限不 Incr）", v, ok)
	}
	wantEQ(t, "超限不得改密钥", "行数", st.secret.count(), 1)
	wantEQ(t, "超限不得吊销会话", "RevokeAll 次数", countPrefix(e.ops(0), "session.RevokeAll"), 0)
	wantEQ(t, "超限后原密码仍可用", "hash", activeSecret(t, st, mid).Hash, before.Hash)
}

// TestResetPasswordChecksCaptureBeforeAccountExistence 判定顺序：验证码先于账号存在性。
// 用一个「验证码正确、账号不存在」的输入钉住：走满了三次凭证反查才报账号不存在，
// 而且这期间的验证码读取次数、账号探测都可用于枚举（枚举面），且验证码不消费。
func TestResetPasswordChecksCaptureBeforeAccountExistence(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const ghost = "13900139000"
	seedUser(t, st, "13800138000", "S3cret!", model.CredentialTypePhone)
	seedCapture(st, recoveryBiz, ghost, "246810")
	st.log.reset()

	_, err := callResetPassword(t, e, ghost, "246810", "Reset#1", "1.2.3.4")
	wantErrIs(t, "验证码对但账号不存在", err, ErrLoginAccountNotExist)
	wantOps(t, "账号不存在前的完整判定链", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, ghost),
		"cache.GetInt:" + capCodeKey(recoveryBiz, ghost),
		"cred.FindMidsByIdentifiers:1/" + ghost,
		"cred.FindMidsByIdentifiers:2/" + ghost,
		"cred.FindMidsByIdentifiers:3/" + ghost,
	})
	wantNoOpsWith(t, "账号不存在路径", e.ops(0), "secret.")
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, ghost)); !ok {
		t.Errorf("账号不存在时验证码被消费了，必须留着")
	}
}

// TestResetPasswordEmptyAccountDegradesToCaptureInvalid 入参校验边界：空标识没有专门的
// 「请输入账号」错误，而是被 checkCapture 抢先判成「验证码无效」——用户拿到误导性的提示。
func TestResetPasswordEmptyAccountDegradesToCaptureInvalid(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedUser(t, st, "13800138000", "S3cret!", model.CredentialTypePhone)
	seedCapture(st, recoveryBiz, "", "246810")
	st.log.reset()

	_, err := callResetPassword(t, e, "   ", "246810", "Reset#1", "1.2.3.4")
	wantErrIs(t, "空标识（但 key 存在时对上了 cap_code_3_）", err, ErrLoginAccountNotExist)
	wantOps(t, "空标识的调用序列（findMidByAccount 对空串直接短路，不查库）", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, ""),
		"cache.GetInt:" + capCodeKey(recoveryBiz, ""),
	})

	// 正常线上态（没有 cap_code_3_ 这个 key）下退化成 ErrCaptureInvalid
	e2 := newEnv(t)
	seedUser(t, e2.st, "13800138000", "S3cret!", model.CredentialTypePhone)
	e2.st.log.reset()
	_, err = callResetPassword(t, e2, "", "246810", "Reset#1", "1.2.3.4")
	wantErrIs(t, "空标识 + 无验证码时报的是验证码错误（误导）", err, ErrCaptureInvalid)
	wantOps(t, "空标识 + 无验证码的调用序列", e2.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, ""),
		"cache.GetInt:" + capCodeKey(recoveryBiz, ""),
	})
}

// TestResetPasswordEmptyNewPasswordKeepsCode 新密码为空：ErrPasswordRequired，
// 事务与吊销都不发生，而且验证码**没被消费**——补齐密码后可直接用同一条码重试。
func TestResetPasswordEmptyNewPasswordKeepsCode(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	before := activeSecret(t, st, mid)
	seedCapture(st, recoveryBiz, phone, "246810")
	st.cache.warmInt(capErrKey(recoveryBiz, phone), 2) // 已错过两次
	st.log.reset()

	_, err := callResetPassword(t, e, phone, "246810", "", "1.2.3.4")
	wantErrIs(t, "空新密码", err, ErrPasswordRequired)
	wantOps(t, "空新密码的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capCodeKey(recoveryBiz, phone),
		"cred.FindMidsByIdentifiers:1/" + phone,
		"cred.FindMidsByIdentifiers:2/" + phone,
	})
	opened, _ := st.conn.txStats()
	wantEQ(t, "空新密码不得开事务", "事务数", opened, 0)
	wantEQ(t, "空新密码不得改密钥", "hash", activeSecret(t, st, mid).Hash, before.Hash)
	wantEQ(t, "空新密码不得吊销会话", "RevokeAll 次数", countPrefix(e.ops(0), "session.RevokeAll"), 0)
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, phone)); !ok {
		t.Errorf("失败路径把验证码消费了，重试就得重新收码")
	}

	// 同一条码确实还能用（错误计数也不会因成功而清零——这里已 2 次，仍 < 阈值 4）
	_, err = callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	wantNoErr(t, "补上新密码后用同一条码重试", err)
	wantEQ(t, "重试后密钥已轮换", "行数", st.secret.count(), 2)
}

// TestResetPasswordReplayNeedsNewCode 重放保护：成功后验证码被删，
// 拿同一条码再重置一次 → ErrCaptureInvalid，且一步写库都没有。
func TestResetPasswordReplayNeedsNewCode(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, recoveryBiz, phone, "246810")
	_, err := callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	wantNoErr(t, "首次重置", err)
	first := activeSecret(t, st, mid)
	st.log.reset()

	_, err = callResetPassword(t, e, phone, "246810", "Reset#2", "1.2.3.4")
	wantErrIs(t, "重放已消费的验证码", err, ErrCaptureInvalid)
	wantOps(t, "重放的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capCodeKey(recoveryBiz, phone),
	})
	wantNoOpsWith(t, "重放路径", e.ops(0), "secret.")
	wantNoOpsWith(t, "重放路径", e.ops(0), "session.")
	wantEQ(t, "重放后密钥仍是第一次那把", "hash", activeSecret(t, st, mid).Hash, first.Hash)
	wantEQ(t, "重放不新增密钥行", "行数", st.secret.count(), 2)
}

// TestResetPasswordTxFailureRollsBackAndKeepsCode 事务内 Insert 故障：
// 错误原样外传、MarkHistory 被回滚、会话不吊销、验证码仍可重用。
func TestResetPasswordTxFailureRollsBackAndKeepsCode(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	before := activeSecret(t, st, mid)
	seedCapture(st, recoveryBiz, phone, "246810")
	sess := login(t, e, phone, "S3cret!", "1.2.3.4", "pc")
	nextID := st.secret.peekID()
	boom := errors.New("account/db: secret insert down")
	st.secret.failWith("Insert", boom)
	st.log.reset()

	reply, err := callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	if reply != nil {
		t.Errorf("出错路径必须返回 nil reply，got %+v", reply)
	}
	wantOps(t, "Insert 故障的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capCodeKey(recoveryBiz, phone),
		"cred.FindMidsByIdentifiers:1/" + phone,
		"cred.FindMidsByIdentifiers:2/" + phone,
		"secret.MarkHistory:" + itoa(mid) + "/1",
		"secret.Insert:" + itoa(nextID+1) + "/" + itoa(mid) + "/0",
	})
	wantNoOpsWith(t, "Insert 故障路径", e.ops(0), "session.RevokeAll")
	wantNoOpsWith(t, "Insert 故障路径", e.ops(0), "cache.Del")
	_, rolledBack := st.conn.txStats()
	wantEQ(t, "Insert 失败必须回滚", "回滚数", rolledBack, 1)
	wantEQ(t, "回滚后只剩原密钥行", "行数", st.secret.count(), 1)
	wantEQ(t, "回滚后原密钥仍生效", "status", activeSecret(t, st, mid).Status, model.SecretStatusActive)
	wantEQ(t, "回滚后 hash 不变", "hash", activeSecret(t, st, mid).Hash, before.Hash)
	wantEQ(t, "失败后会话仍有效", "active", st.session.activeFor(mid), 1)
	wantEQ(t, "失败后 token 仍可用", "is_login", mustTokenInfo(t, e, sess.Token).IsLogin, true)
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, phone)); !ok {
		t.Errorf("事务失败后验证码必须留着（否则用户得重新收码）")
	}
}

// TestResetPasswordRevokeAllFailureSwallowed login.go:586 的 `_ =`：
// 密码已经改了但吊销失败被吞 → 返回成功、旧会话全部存活（缺口 5）。
func TestResetPasswordRevokeAllFailureSwallowed(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	sess := login(t, e, phone, "S3cret!", "1.2.3.4", "pc")
	boom := errors.New("account/db: revoke all down")
	st.session.failWith("RevokeAll", boom)
	seedCapture(st, recoveryBiz, phone, "246810")
	st.log.reset()

	_, err := callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	wantNoErr(t, "RevokeAll 故障不得让重置失败（错误被吞）", err)
	wantCount(t, "吊销那一步确实尝试过", e.ops(0), "session.RevokeAll:"+itoa(mid), 1)
	wantEQ(t, "重置成功但旧会话仍有效（缺口 5）", "active", st.session.activeFor(mid), 1)
	wantEQ(t, "旧 token 仍判定已登录", "is_login", mustTokenInfo(t, e, sess.Token).IsLogin, true)
	fresh := activeSecret(t, st, mid)
	wantEQ(t, "新密码已经生效", "hash", fresh.Hash, seedHash("Reset#1", fresh.Salt))
	wantCount(t, "尽管吊销失败，验证码照样被消费", e.ops(0), "cache.Del:"+capCodeKey(recoveryBiz, phone), 1)
}

// TestResetPasswordCacheDelFailureKeepsCodeUsable login.go:587 的 `_ =`：
// 消费验证码的 Del 失败被吞 → 重置返回成功，但同一条码还在，可再重置一次
// （下一次会撞缺口 3，所以真正的危害面是「同一把码可反复尝试覆盖密钥」）。
func TestResetPasswordCacheDelFailureKeepsCodeUsable(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, recoveryBiz, phone, "246810")
	boom := errors.New("redis down")
	st.cache.failWith("Del", boom)
	st.log.reset()

	_, err := callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	wantNoErr(t, "Del 故障不得让重置失败（错误被吞）", err)
	got, ok := st.cache.intOf(capCodeKey(recoveryBiz, phone))
	if !ok || got != 246810 {
		t.Errorf("Del 失败后验证码 = %d(存在=%v), want 仍是 246810", got, ok)
	}
	wantEQ(t, "Del 故障不影响改密落库", "行数", st.secret.count(), 2)
	fresh := activeSecret(t, st, mid)
	wantEQ(t, "Del 故障下新密码确实已生效", "hash", fresh.Hash, seedHash("Reset#1", fresh.Salt))
}

// TestResetPasswordEmailCaseAsymmetry 缺口 11 的端到端实锤：SendCapture 只对 target 做
// TrimSpace（login.go:452），ResetPassword 却把 account 小写化后再拼 key（login.go:557）——
// 用真实 SendCapture 发一封「Bob@Example.com」的找回码，再用同样的输入重置必然失败，
// 而且错误提示是「验证码无效」，用户侧完全无法自救。
func TestResetPasswordEmailCaseAsymmetry(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const mixed = "Bob@Example.com"
	mid := seedUser(t, st, strings.ToLower(mixed), "S3cret!", model.CredentialTypeEmail)
	wantNoErr(t, "发送找回验证码（大写邮箱）", st.repo.SendCapture(context.Background(), int32(recoveryBiz), mixed, "1.2.3.4"))
	code, ok := st.cache.intOf(capCodeKey(recoveryBiz, mixed))
	if !ok {
		t.Fatalf("SendCapture 没有把验证码写到 %s", capCodeKey(recoveryBiz, mixed))
	}
	st.log.reset()

	_, err := callResetPassword(t, e, mixed, itoa(code), "Reset#1", "1.2.3.4")
	wantErrIs(t, "大写邮箱发码 + 同样输入重置", err, ErrCaptureInvalid)
	wantOps(t, "查询的是小写 key", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, strings.ToLower(mixed)),
		"cache.GetInt:" + capCodeKey(recoveryBiz, strings.ToLower(mixed)),
	})
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, mixed)); !ok {
		t.Errorf("前置条件破坏：验证码仍留在原始大小写的 key 上，谁也读不到")
	}
	wantEQ(t, "失败路径不得改密钥", "行数", st.secret.count(), 1)
	wantNoOpsWith(t, "大写邮箱失败路径", e.ops(0), "secret.")

	// 反过来：用小写标识去重置能过（缺陷只在「发码时用了大写」这一步暴露）
	st.cache.warmInt(capCodeKey(recoveryBiz, strings.ToLower(mixed)), code)
	st.log.reset()
	_, err = callResetPassword(t, e, strings.ToLower(mixed), itoa(code), "Reset#1", "1.2.3.4")
	wantNoErr(t, "用小写标识重置", err)
	wantEQ(t, "小写路径改密成功", "行数", st.secret.count(), 2)
	fresh := activeSecret(t, st, mid)
	wantEQ(t, "小写路径写入的就是那把新密码", "hash", fresh.Hash, seedHash("Reset#1", fresh.Salt))
}

// TestResetPasswordSecondResetAlwaysFails 缺口 3 跨方法复现：第一次重置留下一条
// status=1 的历史行，之后任何一次重置（或 SetPassword）都在 MarkHistory 上撞 1062。
// 用户视角：改过一次密码后，「忘记密码」通道永久报废。
func TestResetPasswordSecondResetAlwaysFails(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	mid := seedUser(t, st, phone, "S3cret!", model.CredentialTypePhone)
	seedCapture(st, recoveryBiz, phone, "246810")
	_, err := callResetPassword(t, e, phone, "246810", "Reset#1", "1.2.3.4")
	wantNoErr(t, "第一次重置", err)
	first := activeSecret(t, st, mid)
	sess := login(t, e, phone, "Reset#1", "1.2.3.4", "pc")

	seedCapture(st, recoveryBiz, phone, "135790")
	st.log.reset()

	_, err = callResetPassword(t, e, phone, "135790", "Reset#2", "1.2.3.4")
	wantErr(t, "第二次重置必须失败（缺口 3）", err)
	if !strings.Contains(errText(err), "1062") || !strings.Contains(errText(err), "uk_mid_type_status") {
		t.Errorf("第二次重置的错误 = %v, want 唯一键 1062（uk_mid_type_status）", err)
	}
	wantOps(t, "第二次重置的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(recoveryBiz, phone),
		"cache.GetInt:" + capCodeKey(recoveryBiz, phone),
		"cred.FindMidsByIdentifiers:1/" + phone,
		"cred.FindMidsByIdentifiers:2/" + phone,
		"secret.MarkHistory:" + itoa(mid) + "/1",
	})
	_, rolledBack := st.conn.txStats()
	wantEQ(t, "第二次整段回滚", "回滚数", rolledBack, 1)
	wantEQ(t, "回滚后密钥行数不变", "行数", st.secret.count(), 2)
	wantEQ(t, "生效密钥仍是第一次那把", "hash", activeSecret(t, st, mid).Hash, first.Hash)
	wantCount(t, "失败路径不吊销会话", e.ops(0), "session.RevokeAll", 0)
	wantEQ(t, "失败后新会话仍有效", "status", sessionOf(t, e, sess.Token).Status, model.SessionStatusActive)
	// 失败路径也没消费掉那条新验证码（用户可以无限次重放同一个码，每次都以 1062 告终）
	if _, ok := st.cache.intOf(capCodeKey(recoveryBiz, phone)); !ok {
		t.Errorf("缺口 3 的连带效应：失败的重置不消费验证码")
	}
}
