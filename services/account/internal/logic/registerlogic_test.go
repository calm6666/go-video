package logic

// registerlogic_test.go 覆盖 Register（repository/login.go:308-362）。
//
// 被测判定链：注册标识归一（TrimSpace+ToLower）→ 验证码（仅在带了 code 时校验）→
// 标识唯一性反查（username/phone/email 三类凭证依次查）→ mid 生成 →
// 「事务」内写 account + account_credential + account_secret → 成功后立刻签发登录态
// （写会话表 + 回填 ak_/rk_ 缓存）→ 落注册日志。
//
// 结论性断言：
//   - 主键与顺序：账号/凭证/密钥/会话/日志各在第几步写、拿到的自增 ID 是谁；
//   - 事务边界只覆盖 account_secret（已知缺口 1）：两个失败用例直接证明
//     账号主表/凭证行在回滚后仍然留在库里；
//   - 注册成功不消费验证码（已知缺口 8）；手机号不带验证码也能注册（已知缺口 12）；
//   - 密码零校验（已知缺口 13）：空密码能注册、也能用空密码登录。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callRegister(t *testing.T, e *env, in *rpc.RegisterReq) (*rpc.RegisterReply, error) {
	t.Helper()
	return NewRegisterLogic(context.Background(), e.svcCtx).Register(in)
}

// TestRegisterUsernameHappyPath 正常路径：三步反查 + 三表落库 + 会话签发 + 缓存回填 + 成功日志，
// 顺序与步数逐一钉死。
func TestRegisterUsernameHappyPath(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := nowUnix()

	reply, err := callRegister(t, e, &rpc.RegisterReq{Account: "alice", Password: "S3cret!", Ip: "1.2.3.4"})
	wantNoErr(t, "用户名注册", err)
	if reply == nil || reply.Login == nil {
		t.Fatalf("注册响应残缺：%+v", reply)
	}
	mid := reply.Mid
	if mid <= 0 {
		t.Fatalf("mid = %d, want 正整数（idgen 生成）", mid)
	}

	wantOps(t, "注册调用序列", e.ops(0), []string{
		// 标识唯一性：三类凭证依次反查，用户名未命中所以查满三类
		"cred.FindMidsByIdentifiers:1/alice",
		"cred.FindMidsByIdentifiers:2/alice",
		"cred.FindMidsByIdentifiers:3/alice",
		"account.Insert:" + itoa(mid),
		"cred.Insert:" + itoa(mid) + "/1/alice",
		"secret.Insert:1/" + itoa(mid) + "/0",
		"session.Insert:1/" + itoa(mid),
		"cache.SetJSON:ak_" + reply.Login.Token + "/600",
		"cache.SetJSON:rk_" + reply.Login.RefreshToken + "/600",
		"loginlog.Add:1/" + itoa(mid) + "/3/0",
	})

	// 账号主表
	acc := st.account.get(mid)
	if acc == nil {
		t.Fatalf("account 表没有 mid=%d 的行", mid)
	}
	wantEQ(t, "注册落库", "status", acc.Status, int32(0))
	wantEQ(t, "注册落库", "is_tourist", acc.IsTourist, int32(0))
	wantEQ(t, "注册落库", "reg_ip", acc.RegIP, "1.2.3.4")
	wantTSWindow(t, "注册落库", "created_at", acc.CreatedAt, now, nowUnix()+5)

	// 凭证表：类型按标识形态推断为「用户名」，标识按归一后的小写落库
	crs := st.cred.rowsFor(mid)
	if len(crs) != 1 {
		t.Fatalf("account_credential 行数 = %d, want 1", len(crs))
	}
	wantEQ(t, "注册凭证", "credential_type", crs[0].CredentialType, model.CredentialTypeUsername)
	wantEQ(t, "注册凭证", "identifier", crs[0].Identifier, "alice")
	wantEQ(t, "注册凭证", "status", crs[0].Status, int8(0))
	// 替身口径纪律：生产 SQL 直接把入参 created_at 写进列（注册路径传 0），
	// model 层不补 now —— 替身若「好心」补上，这里就会红。
	wantEQ(t, "注册凭证 created_at 取入参值（model 不补 now）", "created_at", crs[0].CreatedAt, int64(0))

	// 密钥表：新盐随机、哈希与 saltPwd 同口径
	secs := st.secret.rowsOf(mid)
	if len(secs) != 1 {
		t.Fatalf("account_secret 行数 = %d, want 1", len(secs))
	}
	wantEQ(t, "注册密钥", "secret_type", secs[0].SecretType, model.SecretTypePassword)
	wantEQ(t, "注册密钥", "status", secs[0].Status, model.SecretStatusActive)
	wantHexLen(t, "注册密钥", "salt", secs[0].Salt, 32)
	wantHexLen(t, "注册密钥", "hash", secs[0].Hash, 32)
	wantEQ(t, "注册密钥哈希口径", "hash", secs[0].Hash, seedHash("S3cret!", secs[0].Salt))

	// 会话表 + 缓存
	sessions := st.session.rowsOf(mid)
	if len(sessions) != 1 {
		t.Fatalf("account_session 行数 = %d, want 1", len(sessions))
	}
	s := sessions[0]
	wantEQ(t, "注册即登录", "token", s.Token, reply.Login.Token)
	wantEQ(t, "注册即登录", "refresh_token", s.RefreshToken, reply.Login.RefreshToken)
	wantEQ(t, "注册即登录", "csrf", s.Csrf, reply.Login.Csrf)
	wantEQ(t, "注册即登录", "status", s.Status, model.SessionStatusActive)
	wantEQ(t, "注册即登录", "create_ip", s.CreateIP, "1.2.3.4")
	wantEQ(t, "注册即登录（Register 不传 device/buvid）", "device", s.Device, "")
	wantHexLen(t, "注册签发", "token", reply.Login.Token, 64)
	wantHexLen(t, "注册签发", "refresh_token", reply.Login.RefreshToken, 64)
	wantHexLen(t, "注册签发", "csrf", reply.Login.Csrf, 32)
	wantTSWindow(t, "注册签发", "expires", reply.Login.Expires, now+30*86400, nowUnix()+30*86400+5)
	wantTSWindow(t, "注册签发", "refresh_expires", s.RefreshExpires, now+90*86400, nowUnix()+90*86400+5)

	var cached model.AccountSession
	if !st.cache.jsonOf("ak_"+reply.Login.Token, &cached) {
		t.Fatalf("缓存里没有 ak_%s…", reply.Login.Token[:8])
	}
	wantEQ(t, "ak_ 缓存回读", "mid", cached.Mid, mid)
	wantEQ(t, "ak_ 缓存回读", "csrf", cached.Csrf, reply.Login.Csrf)
	if ttl, ok := st.cache.ttlOf("ak_" + reply.Login.Token); !ok || ttl != 600 {
		t.Errorf("ak_ 缓存 TTL = %d(ok=%v), want 600（tokenCacheTTL）", ttl, ok)
	}
	// 密码与哈希绝不进缓存（缓存里只有会话 JSON）
	wantNotContains(t, "缓存不得含密码明文", st.cache.allText(), "S3cret!")
	wantNotContains(t, "缓存不得含密码哈希", st.cache.allText(), secs[0].Hash)
	wantNotContains(t, "缓存不得含盐", st.cache.allText(), secs[0].Salt)

	// 注册日志：type=3、status=0、reason 空
	ll := st.loginLog.only()
	wantEQ(t, "注册日志", "mid", ll.Mid, mid)
	wantEQ(t, "注册日志", "login_type", ll.LoginType, model.LoginTypeRegister)
	wantEQ(t, "注册日志", "status", ll.Status, model.LoginStatusOK)
	wantEQ(t, "注册日志", "reason", ll.Reason, "")
	wantEQ(t, "注册日志", "ip", ll.IP, "1.2.3.4")

	// 注册链路不得触下游 RPC
	wantNoOpsWith(t, "注册链路", e.ops(0), "userProfile.")
	wantNoOpsWith(t, "注册链路", e.ops(0), "socialGraph.")
}

// TestRegisterLowercasesAndTrimsIdentifier 归一口径：首尾空格去掉、大小写折叠后才做
// 唯一性反查与落库（否则同一标识能被注册两次）。
func TestRegisterLowercasesAndTrimsIdentifier(t *testing.T) {
	e := newEnv(t)
	st := e.st

	reply, err := callRegister(t, e, &rpc.RegisterReq{Account: "  Alice@Example.COM  ", Password: "S3cret!"})
	wantNoErr(t, "邮箱注册", err)
	wantOps(t, "标识归一后的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice@example.com",
		"cred.FindMidsByIdentifiers:2/alice@example.com",
		"cred.FindMidsByIdentifiers:3/alice@example.com",
		"account.Insert:" + itoa(reply.Mid),
		"cred.Insert:" + itoa(reply.Mid) + "/3/alice@example.com",
		"secret.Insert:1/" + itoa(reply.Mid) + "/0",
		"session.Insert:1/" + itoa(reply.Mid),
		"cache.SetJSON:ak_" + reply.Login.Token + "/600",
		"cache.SetJSON:rk_" + reply.Login.RefreshToken + "/600",
		"loginlog.Add:1/" + itoa(reply.Mid) + "/3/0",
	})
	crs := st.cred.rowsFor(reply.Mid)
	if len(crs) != 1 {
		t.Fatalf("凭证行数 = %d, want 1", len(crs))
	}
	wantEQ(t, "邮箱标识落库必须小写", "identifier", crs[0].Identifier, "alice@example.com")
	wantEQ(t, "标识形态推断", "credential_type", crs[0].CredentialType, model.CredentialTypeEmail)
}

// TestRegisterEmptyAccountTouchesNothing 入参校验边界：空/纯空格标识在反查之前就返回，
// 一条 SQL 都不该发（区别于 PasswordLogin：后者失败也要落审计日志）。
func TestRegisterEmptyAccountTouchesNothing(t *testing.T) {
	for _, acct := range []string{"", "   ", "\t\n"} {
		t.Run("account="+strings.TrimSpace(acct), func(t *testing.T) {
			e := newEnv(t)
			_, err := callRegister(t, e, &rpc.RegisterReq{Account: acct, Password: "S3cret!"})
			wantErrIs(t, "空标识注册", err, ErrLoginAccountNotExist)
			wantEQ(t, "空标识注册不得产生任何调用", "ops", len(e.ops(0)), 0)
			wantEQ(t, "空标识注册不得落账号", "account 行数", e.st.account.count(), 0)
			wantEQ(t, "空标识注册不得落日志", "loginlog 行数", e.st.loginLog.count(), 0)
		})
	}
}

// TestRegisterRejectsExistingAccount 已存在标识：只查不写，错误映射为领域错误，
// 且不产生第二条账号/凭证/会话（唯一性反查在写之前）。
func TestRegisterRejectsExistingAccount(t *testing.T) {
	e := newEnv(t)
	st := e.st
	mid := seedUser(t, st, "alice", "OldPass!", model.CredentialTypeUsername)
	st.log.reset()

	_, err := callRegister(t, e, &rpc.RegisterReq{Account: "ALICE", Password: "S3cret!", Ip: "1.1.1.1"})
	wantErrIs(t, "重复标识注册", err, ErrAccountExists)
	wantOps(t, "重复注册的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/alice",
	})
	wantEQ(t, "重复注册不得再落账号", "account 行数", st.account.count(), 1)
	wantEQ(t, "重复注册不得再落凭证", "cred 行数", st.cred.count(), 1)
	wantEQ(t, "重复注册不得再落密钥", "secret 行数", st.secret.count(), 1)
	wantEQ(t, "重复注册不得签发会话", "session 行数", st.session.count(), 0)
	// 撞在唯一性检查上的注册不写审计日志（与事务失败分支不同，见缺口 9）
	wantEQ(t, "重复注册不写登录日志", "loginlog 行数", st.loginLog.count(), 0)
	acc := st.account.get(mid)
	if acc == nil {
		t.Fatalf("原账号被影响：mid=%d 的行不见了", mid)
	}
}

// TestRegisterPhoneWithoutCaptureIsAccepted 已知缺口 12：代码注释写「手机/邮箱注册要求验证码」，
// 但实现只在 CaptureCode 非空时才校验 → 任何人可用他人手机号直接注册。
func TestRegisterPhoneWithoutCaptureIsAccepted(t *testing.T) {
	e := newEnv(t)
	st := e.st

	reply, err := callRegister(t, e, &rpc.RegisterReq{Account: "13800138000", Password: "S3cret!", Ip: "1.2.3.4"})
	wantNoErr(t, "手机号无验证码注册（缺口 12 的现状）", err)
	if reply == nil || reply.Mid <= 0 {
		t.Fatalf("手机号无验证码竟然没注册成功：%+v", reply)
	}
	wantNoOpsWith(t, "手机号注册（无验证码）", e.ops(0), "cap_code_2_13800138000")
	wantNoOpsWith(t, "手机号注册（无验证码）", e.ops(0), "cap_err_2_13800138000")
	crs := st.cred.rowsFor(reply.Mid)
	if len(crs) != 1 {
		t.Fatalf("凭证行数 = %d, want 1", len(crs))
	}
	wantEQ(t, "11 位 1 开头按手机号入库", "credential_type", crs[0].CredentialType, model.CredentialTypePhone)

	// 对照：同一手机号带正确验证码时确实会走校验 → 缺的是「强制要求」而不是形态判断。
	e2 := newEnv(t)
	seedCapture(e2.st, model.CaptureBizRegister, "13800138000", "123456")
	_, err = callRegister(t, e2, &rpc.RegisterReq{
		Account: "13800138000", Password: "S3cret!", CaptureCode: "123456", Ip: "1.2.3.4",
	})
	wantNoErr(t, "带正确验证码的手机号注册", err)
	wantCount(t, "带验证码注册必须走一次验证码读取", e2.ops(0),
		"cache.GetInt:"+capCodeKey(model.CaptureBizRegister, "13800138000"), 1)
}

// TestRegisterWrongCaptureIncrementsErrCounter 验证码错误分支：错误计数 +1、首次补 TTL，
// 且完全没有走到写库（注册在验证码这一步就该止步）。
func TestRegisterWrongCaptureIncrementsErrCounter(t *testing.T) {
	e := newEnv(t)
	st := e.st
	target := "bob@example.com"
	seedCapture(st, model.CaptureBizRegister, target, "123456")
	st.log.reset()

	_, err := callRegister(t, e, &rpc.RegisterReq{
		Account: target, Password: "S3cret!", CaptureCode: "000000", Ip: "1.2.3.4",
	})
	wantErrIs(t, "验证码错误", err, ErrCaptureWrong)
	wantOps(t, "验证码错误的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizRegister, target),
		"cache.GetInt:" + capCodeKey(model.CaptureBizRegister, target),
		"cache.Incr:" + capErrKey(model.CaptureBizRegister, target),
		"cache.GetInt:" + capErrKey(model.CaptureBizRegister, target),
		"cache.Expire:" + capErrKey(model.CaptureBizRegister, target) + "/86400",
	})
	n, ok := st.cache.intOf(capErrKey(model.CaptureBizRegister, target))
	wantEQ(t, "错误计数回读", "存在", ok, true)
	wantEQ(t, "错误计数", "cap_err", n, int64(1))
	if ttl, hit := st.cache.ttlOf(capErrKey(model.CaptureBizRegister, target)); !hit || ttl != 86400 {
		t.Errorf("cap_err TTL = %d(hit=%v), want 86400", ttl, hit)
	}
	wantEQ(t, "验证码错误不得落账号", "account 行数", st.account.count(), 0)
	wantEQ(t, "验证码错误不得写登录日志", "loginlog 行数", st.loginLog.count(), 0)
}

// TestRegisterTooManyCaptureErrors 错误次数超过 captureMaxErr=3 后验证码直接作废并删 key。
func TestRegisterTooManyCaptureErrors(t *testing.T) {
	e := newEnv(t)
	st := e.st
	target := "carol@example.com"
	seedCapture(st, model.CaptureBizRegister, target, "123456")
	st.cache.warmInt(capErrKey(model.CaptureBizRegister, target), 4)
	st.log.reset()

	_, err := callRegister(t, e, &rpc.RegisterReq{
		Account: target, Password: "S3cret!", CaptureCode: "123456", // 连正确码也拒绝
	})
	wantErrIs(t, "错误次数超限", err, ErrCaptureErrTooMany)
	wantOps(t, "错误次数超限的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizRegister, target),
		"cache.Del:" + capCodeKey(model.CaptureBizRegister, target),
	})
	if st.cache.has(capCodeKey(model.CaptureBizRegister, target)) {
		t.Errorf("超限后验证码 key 仍在缓存里，必须已删除")
	}
	wantNoOpsWith(t, "超限路径", e.ops(0), "account.Insert")
}

// TestRegisterMissingCaptureIsInvalid 验证码不存在（未发送/已过期）→ ErrCaptureInvalid，
// 不能退化成「跳过校验」。
func TestRegisterMissingCaptureIsInvalid(t *testing.T) {
	e := newEnv(t)
	_, err := callRegister(t, e, &rpc.RegisterReq{
		Account: "dave@example.com", Password: "S3cret!", CaptureCode: "123456",
	})
	wantErrIs(t, "未发送验证码", err, ErrCaptureInvalid)
	wantEQ(t, "未发送验证码不得落账号", "account 行数", e.st.account.count(), 0)
	wantNoOpsWith(t, "未发送验证码", e.ops(0), "account.Insert")
}

// TestRegisterDoesNotConsumeCaptureCode 已知缺口 8：注册成功后不删 cap_code_2_*，
// 与 ResetPassword（成功后 Del）不对称 → 10 分钟内同一条验证码可重复用于注册。
func TestRegisterDoesNotConsumeCaptureCode(t *testing.T) {
	e := newEnv(t)
	st := e.st
	target := "erin@example.com"
	seedCapture(st, model.CaptureBizRegister, target, "123456")
	st.log.reset()

	if _, err := callRegister(t, e, &rpc.RegisterReq{
		Account: target, Password: "S3cret!", CaptureCode: "123456", Ip: "1.2.3.4",
	}); err != nil {
		t.Fatalf("首次带验证码注册：%v", err)
	}
	wantNoOpsWith(t, "注册成功后", e.ops(0), "cache.Del:"+capCodeKey(model.CaptureBizRegister, target))
	if v, ok := st.cache.intOf(capCodeKey(model.CaptureBizRegister, target)); !ok || v != 123456 {
		t.Fatalf("注册后验证码 = %d(存在=%v), want 123456 仍在缓存", v, ok)
	}

	// 重放：同一条验证码再次提交 → 卡在「标识已存在」，说明验证码这一步第二次仍被接受
	// （若注册成功即消费，这里应该先报 ErrCaptureInvalid）。
	st.log.reset()
	_, err := callRegister(t, e, &rpc.RegisterReq{
		Account: target, Password: "Other123!", CaptureCode: "123456", Ip: "1.2.3.4",
	})
	wantErrIs(t, "同一条验证码重放注册", err, ErrAccountExists)
	wantOps(t, "重放时的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizRegister, target),
		"cache.GetInt:" + capCodeKey(model.CaptureBizRegister, target),
		"cred.FindMidsByIdentifiers:1/" + target,
		"cred.FindMidsByIdentifiers:2/" + target,
		"cred.FindMidsByIdentifiers:3/" + target,
	})
	wantEQ(t, "重放不得再多一个账号", "account 行数", st.account.count(), 1)
	wantEQ(t, "重放不得再多签发会话", "session 行数", st.session.count(), 1)
}

// TestRegisterTransactionOnlyRollsBackSecretWrites 已知缺口 1（最要紧的一条）：
// Register 的「事务」只覆盖 account_secret —— account/account_credential 的 model 接口
// 根本没有 tx 形参（model/account.go:36、model/account_credential.go:50），
// 走的是 conn.ExecCtx，因此密钥写入失败时前两步不会回滚 → 留下没有密钥的孤儿账号+凭证。
func TestRegisterTransactionOnlyRollsBackSecretWrites(t *testing.T) {
	e := newEnv(t)
	st := e.st
	boom := errors.New("Error 1062: Duplicate entry '9-x-0' for key 'account_secret.uk_mid_type_status'")
	st.secret.failWith("Insert", boom)
	st.log.reset()

	_, err := callRegister(t, e, &rpc.RegisterReq{Account: "frank", Password: "S3cret!", Ip: "1.2.3.4"})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v（密钥写入失败必须原样外传）", err, boom)
	}
	opened, rolled := st.conn.txStats()
	wantEQ(t, "事务", "开启次数", opened, 1)
	wantEQ(t, "事务", "回滚次数", rolled, 1)
	// 唯一被事务保护的一步撤销了
	wantEQ(t, "回滚后", "account_secret 行数", st.secret.count(), 0)
	// 但这两步不在事务里：孤儿账号 + 孤儿凭证留在了库里
	wantEQ(t, "回滚后账号主表（缺口 1：未回滚）", "account 行数", st.account.count(), 1)
	wantEQ(t, "回滚后凭证表（缺口 1：未回滚）", "cred 行数", st.cred.count(), 1)
	mids := st.account.mids()
	if len(mids) != 1 {
		t.Fatalf("account 主表异常：%v", mids)
	}
	orphanMid := mids[0]
	wantOps(t, "失败路径的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/frank",
		"cred.FindMidsByIdentifiers:2/frank",
		"cred.FindMidsByIdentifiers:3/frank",
		"account.Insert:" + itoa(orphanMid),
		"cred.Insert:" + itoa(orphanMid) + "/1/frank",
		"secret.Insert:1/" + itoa(orphanMid) + "/0",
		"loginlog.Add:1/" + itoa(orphanMid) + "/3/1",
	})
	// 只有密钥那一步真的拿到了事务会话
	wantEQ(t, "只有 secretModel.Insert 走事务会话", "tx 次数", st.secret.txCount("Insert"), 1)
	wantEQ(t, "密钥写之后没有第 4 次事务登记", "tx 次数(MarkHistory)", st.secret.txCount("MarkHistory"), 0)

	ll := st.loginLog.only()
	wantEQ(t, "注册失败日志", "status", ll.Status, model.LoginStatusFail)
	wantEQ(t, "注册失败日志", "login_type", ll.LoginType, model.LoginTypeRegister)
	wantEQ(t, "注册失败日志", "reason", ll.Reason, boom.Error())
	wantEQ(t, "失败不得签发会话", "session 行数", st.session.count(), 0)
	wantNoOpsWith(t, "失败路径", e.ops(0), "cache.SetJSON:ak_")

	// 后果：孤儿凭证占住了 frank 这个标识 —— 用户重试注册永远拿到 already exists，
	// 而反查得到的 mid 又没有密钥，密码登录必然「password wrong」，账号彻底废掉。
	st.secret.failWith("Insert", nil) // 撤掉故障，让重试能走完
	st.log.reset()
	_, err = callRegister(t, e, &rpc.RegisterReq{Account: "frank", Password: "S3cret!"})
	wantErrIs(t, "重试注册（孤儿凭证占位）", err, ErrAccountExists)
	_, err = NewPasswordLoginLogic(context.Background(), e.svcCtx).PasswordLogin(&rpc.LoginReq{
		Account: "frank", Password: "S3cret!", LoginType: int32(model.LoginTypePassword),
	})
	wantErrIs(t, "孤儿账号用原密码登录", err, ErrLoginPasswordWrong)
	wantEQ(t, "孤儿账号在库里仍存在（缺口 1 的持久影响）", "account 行数", st.account.count(), 1)
	wantEQ(t, "孤儿账号没有密钥", "secret 行数", st.secret.count(), 0)
}

// TestRegisterConcurrentSameCredentialLeavesOrphanAccount 缺口 1 的真实触发场景：
// 并发注册同一标识时，后到者在唯一性反查之后、cred.Insert 之前被对手抢先，
// 于是 cred.Insert 撞 1062，而它自己的 account 行已经提交。
func TestRegisterConcurrentSameCredentialLeavesOrphanAccount(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cred.failWith("Insert", dupErr("account_credential.uk_type_identifier", "grace"))
	st.log.reset()

	_, err := callRegister(t, e, &rpc.RegisterReq{Account: "grace", Password: "S3cret!"})
	wantErr(t, "并发注册撞唯一键", err)
	if !strings.Contains(err.Error(), "Duplicate entry") {
		t.Errorf("错误 = %v, want MySQL 1062 原文外传", err)
	}
	_, rolled := st.conn.txStats()
	wantEQ(t, "事务已回滚", "回滚次数", rolled, 1)
	wantEQ(t, "凭证未落库（本步就是失败点）", "cred 行数", st.cred.count(), 0)
	wantEQ(t, "密钥未落库", "secret 行数", st.secret.count(), 0)
	// 缺陷：account 主表行留在了库里
	wantEQ(t, "回滚后账号主表（缺口 1）", "account 行数", st.account.count(), 1)
	wantNoOpsWith(t, "撞唯一键路径", e.ops(0), "session.Insert")
}

// TestRegisterCredentialFaultPropagatesWithoutAuditLog 反查阶段的 DB 故障：
// 错误原样外传，但**不写任何登录日志**（审计覆盖不对称，见 README 缺口 9）。
func TestRegisterCredentialFaultPropagatesWithoutAuditLog(t *testing.T) {
	e := newEnv(t)
	st := e.st
	boom := errors.New("account/db: credential lookup down")
	st.cred.failWith("FindMidsByIdentifiers", boom)
	st.log.reset()

	_, err := callRegister(t, e, &rpc.RegisterReq{Account: "henry", Password: "S3cret!"})
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	wantEQ(t, "反查故障不得落账号", "account 行数", st.account.count(), 0)
	wantEQ(t, "反查故障不写登录日志（缺口 9）", "loginlog 行数", st.loginLog.count(), 0)
	wantOps(t, "反查故障的调用序列", e.ops(0), []string{
		"cred.FindMidsByIdentifiers:1/henry",
	})
}

// TestRegisterSessionInsertFailureKeepsAccountButSkipsLoginLog 事务成功但会话签发失败：
// 账号已可用（三表齐备），但既不返回登录态也不写失败日志（缺口 9 的另一面）。
func TestRegisterSessionInsertFailureKeepsAccountButSkipsLoginLog(t *testing.T) {
	e := newEnv(t)
	st := e.st
	boom := errors.New("account/db: session insert down")
	st.session.failWith("Insert", boom)
	st.log.reset()

	reply, err := callRegister(t, e, &rpc.RegisterReq{Account: "iris", Password: "S3cret!", Ip: "5.6.7.8"})
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want %v", err, boom)
	}
	if reply != nil {
		t.Errorf("会话签发失败时响应 = %+v, want nil", reply)
	}
	wantEQ(t, "注册本身已成功", "account 行数", st.account.count(), 1)
	wantEQ(t, "注册本身已成功", "secret 行数", st.secret.count(), 1)
	wantEQ(t, "会话未落库", "session 行数", st.session.count(), 0)
	wantNoOpsWith(t, "会话失败路径", e.ops(0), "cache.SetJSON:ak_")
	wantEQ(t, "issueSession 失败不写注册日志（缺口 9）", "loginlog 行数", st.loginLog.count(), 0)
	// 日志写入本身失败也只是记日志，不影响注册结果
	e2 := newEnv(t)
	e2.st.loginLog.failWith("Add", errors.New("login log down"))
	reply2, err2 := callRegister(t, e2, &rpc.RegisterReq{Account: "iris", Password: "S3cret!"})
	wantNoErr(t, "登录日志故障不得影响注册", err2)
	if reply2 == nil || reply2.Login == nil {
		t.Fatalf("登录日志故障时注册响应残缺：%+v", reply2)
	}
}

// TestRegisterDoesNotValidatePassword 已知缺口 13：Register 对密码零校验（SetPassword
// 至少挡了空密码），空密码能注册成功，而且能拿空密码登录进来。
func TestRegisterDoesNotValidatePassword(t *testing.T) {
	e := newEnv(t)
	st := e.st

	reply, err := callRegister(t, e, &rpc.RegisterReq{Account: "ivan", Password: ""})
	wantNoErr(t, "空密码注册（缺口 13 的现状）", err)
	secs := st.secret.rowsOf(reply.Mid)
	if len(secs) != 1 {
		t.Fatalf("密钥行数 = %d, want 1", len(secs))
	}
	wantEQ(t, "空密码也被按同算法哈希落库", "hash", secs[0].Hash, seedHash("", secs[0].Salt))

	// 后果：任何人用空密码即可登录这个账号
	st.log.reset()
	sess, err := NewPasswordLoginLogic(context.Background(), e.svcCtx).PasswordLogin(&rpc.LoginReq{
		Account: "ivan", Password: "", LoginType: int32(model.LoginTypePassword),
	})
	wantNoErr(t, "空密码登录", err)
	wantEQ(t, "空密码登录拿到同一个 mid", "mid", sess.Mid, reply.Mid)
	wantEQ(t, "注册 + 空密码登录共 2 个会话", "session 行数", st.session.count(), 2)
}

// TestRegisterHonoursConfiguredTokenTTL 配置项 TokenTTLDays 必须真的进了签发口径
// （RefreshTTLDays 未覆盖时走 90 天默认）。
func TestRegisterHonoursConfiguredTokenTTL(t *testing.T) {
	e := newEnv(t, withTokenTTL(7))
	now := nowUnix()

	reply, err := callRegister(t, e, &rpc.RegisterReq{Account: "judy", Password: "S3cret!"})
	wantNoErr(t, "注册", err)
	wantTSWindow(t, "配置化 token 有效期", "expires", reply.Login.Expires, now+7*86400, nowUnix()+7*86400+5)
	s := sessionOf(t, e, reply.Login.Token)
	wantEQ(t, "token 有效期", "expires", s.Expires, reply.Login.Expires)
	wantTSWindow(t, "refresh 走默认 90 天", "refresh_expires", s.RefreshExpires,
		now+90*86400, nowUnix()+90*86400+5)
}

// TestRegisterWithRSAConfiguredRejectsUndecryptablePassword 生产配了 RSA 密钥时，
// 明文/坏 base64 密码在写库之前就被拒绝（错误原文外传，见缺口 10）。
func TestRegisterWithRSAConfiguredRejectsUndecryptablePassword(t *testing.T) {
	e := newEnv(t, withPassportRSA())
	st := e.st
	st.log.reset()

	_, err := callRegister(t, e, &rpc.RegisterReq{Account: "kate", Password: "not base64 at all !!!"})
	wantErr(t, "RSA 模式下明文密码注册", err)
	if !strings.Contains(err.Error(), "base64") {
		t.Errorf("错误 = %v, want 底层 base64 错误原文（缺口 10：未映射成领域错误）", err)
	}
	wantEQ(t, "解密失败不得落账号", "account 行数", st.account.count(), 0)
	wantEQ(t, "解密失败不得写注册日志", "loginlog 行数", st.loginLog.count(), 0)
	wantNoOpsWith(t, "解密失败路径", e.ops(0), "account.Insert")

	// 对照：用配对公钥加密后即可注册，且落库哈希按明文计算
	st.log.reset()
	cipher, encErr := testPassportRSA.CardEncrypt([]byte("S3cret!"))
	wantNoErr(t, "测试侧 RSA 加密", encErr)
	reply, err := callRegister(t, e, &rpc.RegisterReq{Account: "kate", Password: string(cipher)})
	wantNoErr(t, "RSA 密文注册", err)
	secs := st.secret.rowsOf(reply.Mid)
	if len(secs) != 1 {
		t.Fatalf("密钥行数 = %d, want 1", len(secs))
	}
	wantEQ(t, "落库哈希按解密后的明文计算", "hash", secs[0].Hash, seedHash("S3cret!", secs[0].Salt))
}

// sessionOf 按 token 回读会话表行（找不到直接 Fatal）。
func sessionOf(t *testing.T, e *env, token string) *model.AccountSession {
	t.Helper()
	s := e.st.session.byToken(token)
	if s == nil {
		t.Fatalf("account_session 里没有 token=%s… 的行", token[:8])
	}
	return s
}
