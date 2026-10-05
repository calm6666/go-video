package logic

// checkcapturelogic_test.go 覆盖 CheckCapture（repository/login.go:498-500 → checkCapture:477-495）。
//
// 被测判定链（顺序本身就是要紧结论）：
//   读 cap_err_<biz>_<target> → 超限则 Del cap_code 并直接判定「错误次数过多」
//   → 读 cap_code_<biz>_<target>（读不到即「无效」）→ 与 strconv.FormatInt(服务端码,10)
//   比对入参 TrimSpace 后的串 → 不等则 Incr cap_err（首次补 86400 TTL）并判定「验证码错误」。
//
// 钉住的关键事实：
//   - 错误计数判定**先于**取码比对，所以超限后连正确码也一律拒（用例专门拿正确码走一遍）；
//   - 码比对走**十进制规范化**：存的是整数，"0123456" 对上 123456 必须算错；
//   - 未命中码（没发过/已过期）判「无效」且**不**计入错误次数（否则一次网络抖动就把号码锁死）；
//   - 缺口 C：校验成功**不消费**验证码（checkCapture 无 Del cap_code），10 分钟窗口内可无限重放，
//     CaptureLogin/ResetPassword 之外的调用方（网关独立校验路由）尤其如此；
//   - 缺口 D：captureMaxErr=3 但判定是 `errTimes > 3`，实际允许错过 4 次，第 5 次才锁；
//   - 缺口 E：Redis 读故障被 GetInt 吞成 miss，于是整片验证码在故障期一律报「验证码无效」，
//     与「真的没发过」在报文上不可区分；
//   - CheckCapture 是纯校验：从不写 account_login_log / account_capture_log，也不落任何库。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callCheckCapture(t *testing.T, e *env, biz int32, target, code string) (*rpc.DelCacheReply, error) {
	t.Helper()
	return NewCheckCaptureLogic(context.Background(), e.svcCtx).CheckCapture(
		&rpc.CheckCaptureReq{Biz: biz, Target: target, CaptureCode: code})
}

const checkPhone = "13800138000"

// TestCheckCaptureCorrectCodePassesButIsNotConsumed 正确码：只读两个 key、成功应答；
// 但 cap_code 仍在（缺口 C：可重放），也没有任何写库/写日志。
func TestCheckCaptureCorrectCodePassesButIsNotConsumed(t *testing.T) {
	const biz = model.CaptureBizLogin
	e := newEnv(t)
	st := e.st
	seedCapture(st, biz, checkPhone, "123456")
	st.log.reset()

	reply, err := callCheckCapture(t, e, int32(biz), checkPhone, "123456")
	wantNoErr(t, "正确验证码", err)
	wantProto(t, "校验应答", "reply", reply, &rpc.DelCacheReply{})
	wantOps(t, "校验调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(biz, checkPhone),
		"cache.GetInt:" + capCodeKey(biz, checkPhone),
	})
	if v, ok := st.cache.intOf(capCodeKey(biz, checkPhone)); !ok || v != 123456 {
		t.Errorf("校验成功后 cap_code = %d(存在=%v), want 仍是 123456（现状：不消费，缺口 C）", v, ok)
	}
	wantNoOpsWith(t, "校验成功路径", e.ops(0), "cache.Del")
	wantNoOpsWith(t, "校验成功路径", e.ops(0), "cache.Incr")
	wantNoOpsWith(t, "校验不写任何审计", e.ops(0), "loginlog.")
	wantNoOpsWith(t, "校验不写任何审计", e.ops(0), "capturelog.")
	wantNoOpsWith(t, "校验不查库", e.ops(0), "cred.")
	wantEQ(t, "校验不落库", "loginlog 行数", st.loginLog.count(), 0)
	wantEQ(t, "校验不落库", "capturelog 行数", st.captureLog.count(), 0)

	// 入参首尾空格被 TrimSpace：同一份码带空格仍算对（对照下面的补零用例）
	st.log.reset()
	_, err = callCheckCapture(t, e, int32(biz), checkPhone, "  123456 ")
	wantNoErr(t, "带空格的正确验证码", err)

	// 补零不等价：服务端存的是整数，比对用 FormatInt，"0123456" 必须算错
	st.log.reset()
	_, err = callCheckCapture(t, e, int32(biz), checkPhone, "0123456")
	wantErrIs(t, "补零的验证码", err, ErrCaptureWrong)
}

// TestCheckCaptureWrongCodeIncrementsErrCounterOncePerAttempt 错误码：ErrCaptureWrong、
// 计数 +1、只在**首次**补 86400 TTL，且码不删（还能继续试）。
func TestCheckCaptureWrongCodeIncrementsErrCounterOncePerAttempt(t *testing.T) {
	const biz = model.CaptureBizLogin
	e := newEnv(t)
	st := e.st
	seedCapture(st, biz, checkPhone, "123456")
	st.log.reset()

	reply, err := callCheckCapture(t, e, int32(biz), checkPhone, "000000")
	if reply != nil {
		t.Errorf("响应 = %+v, want nil", reply)
	}
	wantErrIs(t, "错误验证码", err, ErrCaptureWrong)
	wantOps(t, "首次猜错的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(biz, checkPhone),
		"cache.GetInt:" + capCodeKey(biz, checkPhone),
		"cache.Incr:" + capErrKey(biz, checkPhone),
		"cache.GetInt:" + capErrKey(biz, checkPhone),
		"cache.Expire:" + capErrKey(biz, checkPhone) + "/86400",
	})
	if v, ok := st.cache.intOf(capErrKey(biz, checkPhone)); !ok || v != 1 {
		t.Errorf("cap_err = %d(存在=%v), want 1", v, ok)
	}
	if _, ok := st.cache.intOf(capCodeKey(biz, checkPhone)); !ok {
		t.Errorf("猜错不得删除验证码（用户还能再试）")
	}

	// 第二次猜错：计数到 2，且**不再**补 TTL（否则错误窗口被每次猜错无限续期）
	st.log.reset()
	_, err = callCheckCapture(t, e, int32(biz), checkPhone, "000001")
	wantErrIs(t, "第二次猜错", err, ErrCaptureWrong)
	wantOps(t, "第二次猜错的调用序列", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(biz, checkPhone),
		"cache.GetInt:" + capCodeKey(biz, checkPhone),
		"cache.Incr:" + capErrKey(biz, checkPhone),
		"cache.GetInt:" + capErrKey(biz, checkPhone),
	})
	wantCount(t, "TTL 只在首次补", e.ops(0), "cache.Expire", 0)
	if v, _ := st.cache.intOf(capErrKey(biz, checkPhone)); v != 2 {
		t.Errorf("cap_err = %d, want 2", v)
	}
}

// TestCheckCaptureMissingOrExpiredCodeIsInvalid 没发过 / 已过期：判「无效」，
// 而且**不**计入错误次数（一次 Redis 抖动不该把号码锁一天）。
func TestCheckCaptureMissingOrExpiredCodeIsInvalid(t *testing.T) {
	const biz = model.CaptureBizLogin
	t.Run("从未发送", func(t *testing.T) {
		e := newEnv(t)
		reply, err := callCheckCapture(t, e, int32(biz), checkPhone, "123456")
		if reply != nil {
			t.Errorf("响应 = %+v, want nil", reply)
		}
		wantErrIs(t, "没有验证码时校验", err, ErrCaptureInvalid)
		wantOps(t, "无效码的调用序列", e.ops(0), []string{
			"cache.GetInt:" + capErrKey(biz, checkPhone),
			"cache.GetInt:" + capCodeKey(biz, checkPhone),
		})
		wantNoOpsWith(t, "无效码不得计数", e.ops(0), "cache.Incr")
		wantNoOpsWith(t, "无效码不得计数", e.ops(0), "cache.Expire")
	})

	t.Run("TTL 到期（key 消失）", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedCapture(st, biz, checkPhone, "123456")
		st.cache.forget(capCodeKey(biz, checkPhone)) // 复刻 Redis 到期
		st.log.reset()

		_, err := callCheckCapture(t, e, int32(biz), checkPhone, "123456")
		wantErrIs(t, "过期验证码", err, ErrCaptureInvalid)
		wantOps(t, "过期验证码的调用序列", e.ops(0), []string{
			"cache.GetInt:" + capErrKey(biz, checkPhone),
			"cache.GetInt:" + capCodeKey(biz, checkPhone),
		})
		wantNoOpsWith(t, "过期路径", e.ops(0), "cache.Incr")
	})

	t.Run("非数字的缓存值按未命中处理", func(t *testing.T) {
		// GetInt 对 ParseInt 失败返回 (0,false)，与生产 cache.go:341-344 同口径
		e := newEnv(t)
		st := e.st
		st.cache.warmRawStr(capCodeKey(biz, checkPhone), "not-a-code")
		st.log.reset()
		_, err := callCheckCapture(t, e, int32(biz), checkPhone, "123456")
		wantErrIs(t, "坏值验证码", err, ErrCaptureInvalid)
		wantNoOpsWith(t, "坏值路径不得写缓存", e.ops(0), "cache.SetInt")
		wantNoOpsWith(t, "坏值路径不得写缓存", e.ops(0), "cache.Del")
	})
}

// TestCheckCaptureErrThresholdIsStrictlyGreaterThan 缺口 D 的边界对：errTimes=3 仍比对码
// （所以第 4 次猜错照样是「验证码错误」），errTimes=4 才进超限分支，
// 且超限分支**根本不读 cap_code**——正确码也一样被拒，只留一次 Del。
func TestCheckCaptureErrThresholdIsStrictlyGreaterThan(t *testing.T) {
	const biz = model.CaptureBizLogin
	code := "123456"

	t.Run("errTimes=3 仍在比对", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedCapture(st, biz, checkPhone, code)
		st.cache.warmInt(capErrKey(biz, checkPhone), 3)
		st.log.reset()

		_, err := callCheckCapture(t, e, int32(biz), checkPhone, "000000")
		wantErrIs(t, "第 4 次猜错仍是「验证码错误」（阈值判定为 >3）", err, ErrCaptureWrong)
		wantOps(t, "阈值内仍读码并计数", e.ops(0), []string{
			"cache.GetInt:" + capErrKey(biz, checkPhone),
			"cache.GetInt:" + capCodeKey(biz, checkPhone),
			"cache.Incr:" + capErrKey(biz, checkPhone),
			"cache.GetInt:" + capErrKey(biz, checkPhone),
		})
		if v, _ := st.cache.intOf(capErrKey(biz, checkPhone)); v != 4 {
			t.Errorf("cap_err = %d, want 4", v)
		}
	})

	t.Run("errTimes=4 直接判超限并销毁验证码", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedCapture(st, biz, checkPhone, code)
		st.cache.warmInt(capErrKey(biz, checkPhone), 4)
		st.log.reset()

		// 关键：**给正确码**也照样拒 —— 证明超限判定先于取码比对
		_, err := callCheckCapture(t, e, int32(biz), checkPhone, code)
		wantErrIs(t, "超限后正确码也被拒", err, ErrCaptureErrTooMany)
		wantOps(t, "超限分支不读 cap_code", e.ops(0), []string{
			"cache.GetInt:" + capErrKey(biz, checkPhone),
			"cache.Del:" + capCodeKey(biz, checkPhone),
		})
		if _, ok := st.cache.intOf(capCodeKey(biz, checkPhone)); ok {
			t.Errorf("超限后验证码必须被销毁")
		}

		// 之后无论给什么都判超限（码已没了，计数仍 >3 所以仍是「超限」而非「无效」）
		st.log.reset()
		_, err = callCheckCapture(t, e, int32(biz), checkPhone, code)
		wantErrIs(t, "超限状态一直保持", err, ErrCaptureErrTooMany)
		wantCount(t, "超限分支不再递增计数", e.ops(0), "cache.Incr", 0)
	})

	t.Run("重发把错误计数清零后同一码恢复可用", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedCapture(st, biz, checkPhone, code)
		st.cache.warmInt(capErrKey(biz, checkPhone), 9)
		wantNoErr(t, "重发验证码", st.repo.SendCapture(context.Background(), int32(biz), checkPhone, "1.1.1.1"))
		newCode, ok := st.cache.intOf(capCodeKey(biz, checkPhone))
		if !ok {
			t.Fatalf("重发后没有新验证码")
		}
		st.log.reset()

		_, err := callCheckCapture(t, e, int32(biz), checkPhone, itoa(newCode))
		wantNoErr(t, "重发后用新码校验（超限状态已被 SendCapture 解除）", err)
		wantOps(t, "解除后的校验序列", e.ops(0), []string{
			"cache.GetInt:" + capErrKey(biz, checkPhone),
			"cache.GetInt:" + capCodeKey(biz, checkPhone),
		})
	})
}

// TestCheckCaptureFourWrongAttemptsAllowedBeforeLockout 缺口 D 的用户视角实锤：
// 连错 4 次都还是「验证码错误」，第 5 次即使输对也是「次数过多」——
// 而注释与文档口径是「最多错 3 次」。
func TestCheckCaptureFourWrongAttemptsAllowedBeforeLockout(t *testing.T) {
	const biz = model.CaptureBizLogin
	e := newEnv(t)
	st := e.st
	seedCapture(st, biz, checkPhone, "123456")
	st.log.reset()

	for i := 1; i <= 4; i++ {
		_, err := callCheckCapture(t, e, int32(biz), checkPhone, "000000")
		wantErrIs(t, "第 N 次猜错", err, ErrCaptureWrong)
	}
	wantCount(t, "4 次猜错 = 4 次计数", e.ops(0), "cache.Incr:"+capErrKey(biz, checkPhone), 4)
	_, err := callCheckCapture(t, e, int32(biz), checkPhone, "123456")
	wantErrIs(t, "第 5 次即使正确也被锁", err, ErrCaptureErrTooMany)
	if _, ok := st.cache.intOf(capCodeKey(biz, checkPhone)); ok {
		t.Errorf("锁定后验证码必须已被销毁")
	}
}

// TestCheckCaptureBizIsPartOfKeySpace biz 只当 key 命名空间：别的 biz 拿同一份码校验必然无效，
// 未知 biz（99）也不报错、只是查自己的 key。
func TestCheckCaptureBizIsPartOfKeySpace(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedCapture(st, model.CaptureBizLogin, checkPhone, "123456")
	st.log.reset()

	_, err := callCheckCapture(t, e, int32(model.CaptureBizRegister), checkPhone, "123456")
	wantErrIs(t, "跨 biz 复用验证码", err, ErrCaptureInvalid)
	wantOps(t, "查的是注册 biz 的 key", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(model.CaptureBizRegister, checkPhone),
		"cache.GetInt:" + capCodeKey(model.CaptureBizRegister, checkPhone),
	})

	// 未知 biz：没有白名单校验，只是落到没人写过的 key 上 ⇒ 无效
	st.log.reset()
	_, err = callCheckCapture(t, e, 99, checkPhone, "123456")
	wantErrIs(t, "未知 biz 校验", err, ErrCaptureInvalid)
	wantOps(t, "未知 biz 的 key", e.ops(0), []string{
		"cache.GetInt:cap_err_99_" + checkPhone,
		"cache.GetInt:cap_code_99_" + checkPhone,
	})

	// 对照：同一 biz 的 key 上确实有码，校验就过（证明上面两次失败是 key 空间不同，不是入参被拒）
	st.log.reset()
	_, err = callCheckCapture(t, e, int32(model.CaptureBizLogin), checkPhone, "123456")
	wantNoErr(t, "同 biz 校验", err)
}

// TestCheckCaptureNoTargetValidation 入参零校验：空 target 也能成一个合法的 key 空间，
// 只要 cap_code_1_ 上有人布过码就判通过（发码侧同样只 TrimSpace，见 sendcapture 用例）。
func TestCheckCaptureNoTargetValidation(t *testing.T) {
	const biz = model.CaptureBizRecovery
	e := newEnv(t)
	st := e.st
	seedCapture(st, biz, "", "246810")
	st.log.reset()

	_, err := callCheckCapture(t, e, int32(biz), "", "246810")
	wantNoErr(t, "空 target 的校验（仓库不校验接收方形态）", err)
	wantOps(t, "空 target 就是 key 的一部分", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(biz, ""),
		"cache.GetInt:" + capCodeKey(biz, ""),
	})

	// 空码同样不短路：走完整两次 GetInt 后判「无效」（不是「未填验证码」这类参数错误）
	st.log.reset()
	_, err = callCheckCapture(t, e, int32(biz), checkPhone, "")
	wantErrIs(t, "空验证码", err, ErrCaptureInvalid)
	wantOps(t, "空码也照样查库", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(biz, checkPhone),
		"cache.GetInt:" + capCodeKey(biz, checkPhone),
	})
}

// TestCheckCaptureRedisOutageIsSwallowedAsInvalid 缺口 E：GetInt 故障被吞成 miss，
// 于是 Redis 一片故障时所有校验都回「验证码无效」，既不报底层错误、也不计数。
func TestCheckCaptureRedisOutageIsSwallowedAsInvalid(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const biz = model.CaptureBizLogin
	seedCapture(st, biz, checkPhone, "123456")
	boom := errors.New("account/redis: connection refused")
	st.cache.failWith("GetInt", boom)
	st.log.reset()

	_, err := callCheckCapture(t, e, int32(biz), checkPhone, "123456")
	if errors.Is(err, boom) {
		t.Fatalf("底层故障被外传了（与生产口径不符）：%v", err)
	}
	wantErrIs(t, "Redis 故障期的校验", err, ErrCaptureInvalid)
	wantOps(t, "两次读都吞成 miss", e.ops(0), []string{
		"cache.GetInt:" + capErrKey(biz, checkPhone),
		"cache.GetInt:" + capCodeKey(biz, checkPhone),
	})
	wantNoOpsWith(t, "故障路径不得写任何 key", e.ops(0), "cache.Del")
	wantNoOpsWith(t, "故障路径不得写任何 key", e.ops(0), "cache.Incr")
}
