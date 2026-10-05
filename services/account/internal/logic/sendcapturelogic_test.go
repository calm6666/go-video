package logic

// sendcapturelogic_test.go 覆盖 SendCapture（repository/login.go:451-474）。
//
// 被测判定链：target 归一（只 TrimSpace，不小写化）→ 读当日发送次数 cap_times_<biz>_<target>
// → 超限则只落一条失败发送记录 → 生成 6 位验证码写 cap_code_<biz>_<target>（TTL 600）
// → Incr 次数（首次补 TTL 86400）→ Del cap_err_<biz>_<target>（重发解锁错误计数）
// → 落一条 status=0 的发送记录。
//
// 钉住的关键事实：
//   - SetInt 故障必须**中止并原样外传**（后续 Incr/Del/发送记录一步都不许发生）：
//     缓存写不进去就等于验证码根本没发出去，却照样计数、照样审计成功，会放大故障；
//   - 空 target 零调用零记录（与 PasswordLogin 的「空标识也留审计」相反）；
//   - 生成的验证码只进 Redis，**绝不进响应体、也绝不进发送记录**（用例从缓存回读真实
//     明文，再去响应/审计文本里找它，找不到才是对的）；
//   - biz 只是 key 命名空间，仓库不校验 biz 取值（未知 biz 照样发码）；
//   - 缺口 A：限额判定用 `times > captureMaxSend`（login.go:460），注释写的是
//     「单日最大 5 次」，实际放行到第 6 次，第 7 次才拒；
//   - 缺口 B：SetInt 失败的路径**不写任何发送记录**，被拒/失败的发码在审计面留空洞。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callSendCapture(t *testing.T, e *env, biz int32, target, ip string) (*rpc.DelCacheReply, error) {
	t.Helper()
	return NewSendCaptureLogic(context.Background(), e.svcCtx).SendCapture(
		&rpc.SendCaptureReq{Biz: biz, Target: target, Ip: ip})
}

// sendCaptureFirstOps 是「当日首次发码」的完整副作用序列（key 由 biz/target 派生，
// 前缀或参数顺序漂移即红）。
func sendCaptureFirstOps(biz int8, target string) []string {
	return []string{
		"cache.GetInt:" + capTimesKey(biz, target),
		"cache.SetInt:" + capCodeKey(biz, target) + "/600",
		"cache.Incr:" + capTimesKey(biz, target),
		"cache.GetInt:" + capTimesKey(biz, target),
		"cache.Expire:" + capTimesKey(biz, target) + "/86400",
		"cache.Del:" + capErrKey(biz, target),
		fmt.Sprintf("capturelog.Add:1/%d/%s/0", biz, target),
	}
}

// TestSendCaptureHappyPathStoresCodeAndRecordsSend 首次发码：码进 Redis（TTL 600）、
// 次数计数补上日 TTL、错误计数被清、落一条 status=0 的发送记录。
func TestSendCaptureHappyPathStoresCodeAndRecordsSend(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const (
		biz    = model.CaptureBizLogin
		target = "13800138000"
	)
	now := nowUnix()

	reply, err := callSendCapture(t, e, int32(biz), target, "1.2.3.4")
	wantNoErr(t, "首次发码", err)
	wantProto(t, "发码应答", "reply", reply, &rpc.DelCacheReply{})
	wantOps(t, "首次发码调用序列", e.ops(0), sendCaptureFirstOps(biz, target))

	code, ok := st.cache.intOf(capCodeKey(biz, target))
	if !ok {
		t.Fatalf("cap_code key 没写上：%s", capCodeKey(biz, target))
	}
	// 生成口径：UnixNano()%900000 + 100000 ⇒ 恒为 6 位十进制（网关按 6 位定长校验）
	if code < 100000 || code > 999999 {
		t.Errorf("验证码 = %d, want 落在 [100000, 999999]", code)
	}
	if ttl, hit := st.cache.ttlOf(capCodeKey(biz, target)); !hit || ttl != 600 {
		t.Errorf("验证码 TTL = %d(存在=%v), want 600（captureCodeTTL）", ttl, hit)
	}
	if n, hit := st.cache.intOf(capTimesKey(biz, target)); !hit || n != 1 {
		t.Errorf("发送次数 = %d(存在=%v), want 1", n, hit)
	}
	if ttl, hit := st.cache.ttlOf(capTimesKey(biz, target)); !hit || ttl != 86400 {
		t.Errorf("发送次数 TTL = %d(存在=%v), want 86400（首次 Incr 后补日 TTL）", ttl, hit)
	}
	if st.cache.has(capErrKey(biz, target)) {
		t.Errorf("错误计数 key 不该被写出来，只该被删")
	}

	row := st.captureLog.only()
	wantEQ(t, "发送记录", "biz", row.Biz, biz)
	wantEQ(t, "发送记录", "target", row.Target, target)
	wantEQ(t, "发送记录", "ip", row.IP, "1.2.3.4")
	wantEQ(t, "发送记录", "status", row.Status, int8(0))
	wantEQ(t, "发送记录", "reason", row.Reason, "")
	wantTSWindow(t, "发送记录", "ctime", row.CTime, now, nowUnix()+5)

	// 明文验证码只许留在 Redis：响应体与审计记录里都不许出现
	wantNotContains(t, "响应不得回显验证码", fmt.Sprintf("%+v", reply), itoa(code))
	wantNotContains(t, "发送记录不得回显验证码", fmt.Sprintf("%+v", row), itoa(code))
	// 发码不查库、不查账号、不触下游
	wantNoOpsWith(t, "发码路径", e.ops(0), "cred.")
	wantNoOpsWith(t, "发码路径", e.ops(0), "account.")
	wantNoOpsWith(t, "发码路径", e.ops(0), "loginlog.")
	wantNoOpsWith(t, "发码路径", e.ops(0), "userProfile.")
}

// TestSendCaptureEmptyTargetZeroCallsNoRecord 空/纯空格 target：不做任何 Redis 调用，
// 也不落发送记录，直接返回「标识不存在」。
func TestSendCaptureEmptyTargetZeroCallsNoRecord(t *testing.T) {
	for _, target := range []string{"", "   ", "\t"} {
		t.Run(fmt.Sprintf("target=%q", target), func(t *testing.T) {
			e := newEnv(t)
			reply, err := callSendCapture(t, e, int32(model.CaptureBizLogin), target, "9.9.9.9")
			wantErrIs(t, "空 target 发码", err, ErrLoginAccountNotExist)
			if reply != nil {
				t.Errorf("响应 = %+v, want nil", reply)
			}
			wantOps(t, "空 target 不得触任何依赖", e.ops(0), []string{})
			wantEQ(t, "空 target 不落发送记录", "capturelog 行数", e.st.captureLog.count(), 0)
		})
	}
}

// TestSendCaptureTrimsTargetButKeepsCase 归一口径：只 TrimSpace、**不**小写化——
// 发到大写标识的码落在原始大小写的 key 上（这是缺口 11 的上半段：ResetPassword
// 会把 account 小写化后再拼 key，见 resetpasswordlogic_test.go）。
func TestSendCaptureTrimsTargetButKeepsCase(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const padded = "  Bob@Example.com  "
	biz := model.CaptureBizRecovery

	reply, err := callSendCapture(t, e, int32(biz), padded, "1.2.3.4")
	wantNoErr(t, "带空格的大写标识发码", err)
	wantProto(t, "发码应答", "reply", reply, &rpc.DelCacheReply{})
	wantOps(t, "key 用的是去掉首尾空格的原样标识", e.ops(0)[:2], []string{
		"cache.GetInt:" + capTimesKey(biz, "Bob@Example.com"),
		"cache.SetInt:" + capCodeKey(biz, "Bob@Example.com") + "/600",
	})
	wantEQ(t, "发送记录里的 target 也是去空格原样", "target",
		st.captureLog.only().Target, "Bob@Example.com")
	if _, ok := st.cache.intOf(capCodeKey(biz, "Bob@Example.com")); !ok {
		t.Errorf("验证码没落在大小写原样的 key 上")
	}
	if st.cache.has(capCodeKey(biz, "bob@example.com")) {
		t.Errorf("实现不该顺手把标识小写化（小写化是 ResetPassword 那侧做的）")
	}
}

// TestSendCaptureAbortsOnCacheWriteFailure SetInt 故障：错误原样外传，
// 计数/清错误计数/发送记录**一步都不许发生**（缺口 B：失败发码不留审计）。
func TestSendCaptureAbortsOnCacheWriteFailure(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const (
		biz    = model.CaptureBizLogin
		target = "13800138000"
	)
	boom := errors.New("account/redis: write down")
	st.cache.failWith("SetInt", boom)

	reply, err := callSendCapture(t, e, int32(biz), target, "1.2.3.4")
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样外传 %v（不许吞成领域错误）", err, boom)
	}
	wantOps(t, "缓存写失败即中止", e.ops(0), []string{
		"cache.GetInt:" + capTimesKey(biz, target),
		"cache.SetInt:" + capCodeKey(biz, target) + "/600",
	})
	if st.cache.has(capTimesKey(biz, target)) {
		t.Errorf("缓存写失败仍计数了发送次数（限流计数会把一次失败的发码算进去）")
	}
	if st.cache.has(capCodeKey(biz, target)) {
		t.Errorf("写失败却留下了验证码 key")
	}
	wantEQ(t, "写失败不落发送记录（缺口 B：审计空洞）", "capturelog 行数", st.captureLog.count(), 0)
}

// TestSendCaptureSecondSendDoesNotRefreshTimesTTL 第二次发码：Incr 后回读值为 2，
// 因此**不再**补 TTL（否则当日窗口会被无限续期，日限流形同虚设）。
func TestSendCaptureSecondSendDoesNotRefreshTimesTTL(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const (
		biz    = model.CaptureBizLogin
		target = "13800138000"
	)
	// 首次发码作为布景：结束后清零轨迹（纪律 4）
	_, err := callSendCapture(t, e, int32(biz), target, "1.1.1.1")
	wantNoErr(t, "第一次发码", err)
	st.log.reset()

	_, err = callSendCapture(t, e, int32(biz), target, "1.1.1.1")
	wantNoErr(t, "第二次发码", err)
	wantOps(t, "第二次发码序列", e.ops(0), []string{
		"cache.GetInt:" + capTimesKey(biz, target),
		"cache.SetInt:" + capCodeKey(biz, target) + "/600",
		"cache.Incr:" + capTimesKey(biz, target),
		"cache.GetInt:" + capTimesKey(biz, target),
		"cache.Del:" + capErrKey(biz, target),
		fmt.Sprintf("capturelog.Add:2/%d/%s/0", biz, target),
	})
	wantCount(t, "第二次不再补日 TTL", e.ops(0), "cache.Expire", 0)
	n, ok := st.cache.intOf(capTimesKey(biz, target))
	if !ok || n != 2 {
		t.Errorf("发送次数 = %d(存在=%v), want 2", n, ok)
	}
	wantEQ(t, "两次发码各落一条记录", "capturelog 行数", st.captureLog.count(), 2)
	wantEQ(t, "同 biz/target 的记录条数", "rowsFor", len(st.captureLog.rowsFor(biz, target)), 2)
}

// TestSendCaptureDailyLimitOffByOne 缺口 A 的边界对：captureMaxSend=5、注释写「>5 拒绝」，
// 判定却是 `times > captureMaxSend`，于是 times=5（已发 5 次）时第 6 次仍放行，
// 第 7 次才拒。用例分别布 times=5 / times=6，把「放行 6 次」这件事钉死。
func TestSendCaptureDailyLimitOffByOne(t *testing.T) {
	const (
		biz    = model.CaptureBizLogin
		target = "13800138000"
	)
	t.Run("已计 5 次时第 6 次仍放行", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		st.cache.warmInt(capTimesKey(biz, target), 5)
		_, err := callSendCapture(t, e, int32(biz), target, "1.1.1.1")
		wantNoErr(t, "times=5 仍发码（captureMaxSend 判定为 >，非 >=）", err)
		wantOps(t, "放行路径", e.ops(0), []string{
			"cache.GetInt:" + capTimesKey(biz, target),
			"cache.SetInt:" + capCodeKey(biz, target) + "/600",
			"cache.Incr:" + capTimesKey(biz, target),
			"cache.GetInt:" + capTimesKey(biz, target),
			"cache.Del:" + capErrKey(biz, target),
			fmt.Sprintf("capturelog.Add:1/%d/%s/0", biz, target),
		})
	})

	t.Run("已计 6 次时拒绝并留失败记录", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		st.cache.warmInt(capTimesKey(biz, target), 6)
		reply, err := callSendCapture(t, e, int32(biz), target, "2.2.2.2")
		wantErrIs(t, "times=6 拒绝发码", err, ErrCaptureSendTooMany)
		if reply != nil {
			t.Errorf("响应 = %+v, want nil", reply)
		}
		wantOps(t, "拒绝路径", e.ops(0), []string{
			"cache.GetInt:" + capTimesKey(biz, target),
			fmt.Sprintf("capturelog.Add:1/%d/%s/1", biz, target),
		})
		wantNoOpsWith(t, "拒绝路径不得写码", e.ops(0), "cache.SetInt")
		wantNoOpsWith(t, "拒绝路径不得再计数", e.ops(0), "cache.Incr")
		row := st.captureLog.only()
		wantEQ(t, "拒绝记录的 status", "status", row.Status, int8(1))
		wantEQ(t, "拒绝记录的 reason", "reason", row.Reason, "capture send too many")
		wantEQ(t, "拒绝记录的 ip", "ip", row.IP, "2.2.2.2")
	})

	t.Run("连续发码到第 7 次才拒", func(t *testing.T) {
		e := newEnv(t)
		var errs []error
		for i := 0; i < 7; i++ {
			_, err := callSendCapture(t, e, int32(biz), target, "3.3.3.3")
			errs = append(errs, err)
		}
		for i := 0; i < 6; i++ {
			wantNoErr(t, fmt.Sprintf("第 %d 次发码", i+1), errs[i])
		}
		wantErrIs(t, "第 7 次发码", errs[6], ErrCaptureSendTooMany)
		wantEQ(t, "6 次成功 + 1 次拒绝都落记录", "capturelog 行数", e.st.captureLog.count(), 7)
	})
}

// TestSendCaptureClearsErrCounterAndIsNamespacedByBiz 重发解锁 + key 命名空间：
// 清掉同 biz/target 的 cap_err，但**不碰**别的 biz、别的 target 的码与计数。
func TestSendCaptureClearsErrCounterAndIsNamespacedByBiz(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const phone = "13800138000"
	const otherTarget = "13900139000"
	// 登录场景已错过 4 次、并有一条旧码
	st.cache.warmInt(capErrKey(model.CaptureBizLogin, phone), 4)
	seedCapture(st, model.CaptureBizLogin, phone, "111111")
	// 其它 biz / 其它 target 的布景不许被波及
	st.cache.warmInt(capErrKey(model.CaptureBizRegister, phone), 2)
	seedCapture(st, model.CaptureBizRegister, phone, "222222")
	seedCapture(st, model.CaptureBizLogin, otherTarget, "333333")
	st.log.reset()

	_, err := callSendCapture(t, e, int32(model.CaptureBizLogin), phone, "4.4.4.4")
	wantNoErr(t, "重发登录码", err)
	wantOps(t, "重发只动本 biz/target 的三个 key", e.ops(0), sendCaptureFirstOps(model.CaptureBizLogin, phone))

	if st.cache.has(capErrKey(model.CaptureBizLogin, phone)) {
		t.Errorf("重发没有清掉登录场景的错误计数，验证码解锁不了")
	}
	newCode, ok := st.cache.intOf(capCodeKey(model.CaptureBizLogin, phone))
	if !ok || newCode == 111111 {
		t.Errorf("旧验证码未被新码覆盖（现值 %d 存在=%v）", newCode, ok)
	}
	if v, ok := st.cache.intOf(capErrKey(model.CaptureBizRegister, phone)); !ok || v != 2 {
		t.Errorf("注册场景的错误计数被波及（%d 存在=%v）", v, ok)
	}
	if v, ok := st.cache.intOf(capCodeKey(model.CaptureBizRegister, phone)); !ok || v != 222222 {
		t.Errorf("注册场景的验证码被波及（%d 存在=%v）", v, ok)
	}
	if v, ok := st.cache.intOf(capCodeKey(model.CaptureBizLogin, otherTarget)); !ok || v != 333333 {
		t.Errorf("别的接收方的验证码被波及（%d 存在=%v）", v, ok)
	}
}

// TestSendCaptureUnknownBizIsJustANamespace 仓库不校验 biz 取值（无白名单、无枚举判定），
// 未知 biz 照样发码，只是落在自己的 key 与记录行上。
func TestSendCaptureUnknownBizIsJustANamespace(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const target = "13800138000"
	_, err := callSendCapture(t, e, 99, target, "5.5.5.5")
	wantNoErr(t, "未知 biz 发码（仓库不做 biz 白名单）", err)
	wantOps(t, "未知 biz 的 key 与记录都带 99", e.ops(0), sendCaptureFirstOps(99, target))
	row := st.captureLog.only()
	wantEQ(t, "记录行的 biz 原样落库", "biz", row.Biz, int8(99))
}

// TestSendCaptureBizIsTruncatedToInt8InAudit Redis key 用 int32 原值拼、审计列是 int8：
// biz=300 时两者对不上（300 → int8 截成 44），按 biz 统计发码量的运营口径会串台。
// 这里把「key 用 300、列值用 44」这对现状钉住，而不是断言它应该怎么做。
func TestSendCaptureBizIsTruncatedToInt8InAudit(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const target = "13800138000"
	_, err := callSendCapture(t, e, 300, target, "5.5.5.5")
	wantNoErr(t, "biz=300 发码", err)
	wantOps(t, "Redis key 里是 int32 原值 300", e.ops(0), []string{
		"cache.GetInt:cap_times_300_" + target,
		"cache.SetInt:cap_code_300_" + target + "/600",
		"cache.Incr:cap_times_300_" + target,
		"cache.GetInt:cap_times_300_" + target,
		"cache.Expire:cap_times_300_" + target + "/86400",
		"cache.Del:cap_err_300_" + target,
		// 审计轨迹里的 biz 已经是截断后的 int8 值（44），与 key 上的 300 不同
		"capturelog.Add:1/44/" + target + "/0",
	})
	wantEQ(t, "审计列被截成 int8(300)=44", "biz", st.captureLog.only().Biz, int8(44))
}
