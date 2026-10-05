package logic

// realnametelcapturelogic_test.go 覆盖 RealnameTelCapture（logic/realnametelcapturelogic.go:25
// → repository/realname.go:323）。
//
// 这个方法**只写 Redis**：一次验证码发放全程 0 次 MySQL 调用、0 个事务、0 个事件，
// 所以「存储形态 vs 出口形态」的断言面就是缓存键值本身 + 日志 + 应答三处。
// 本文件把三件事钉住：
//   1. 配额口径是 `times > 5`（realname.go:334）——24 小时内放行的是**第 1..6 次**，
//      第 7 次才拒；拒的时候一次写都不做（连错误计数都不清）。
//   2. 验证码是 6 位随机数（realname.go:337 rand.Intn(900000)+100000），
//      它是**一次性凭据**：应答里没有、MySQL 里没有，唯一藏身处是
//      realname_cap_code_<mid>（TTL 600s）——而 realname.go:339 把它明文写进了 INFO 日志，
//      这是本批最高价值的隐私结论，见 TestRealnameTelCaptureCodeIsLoggedInPlaintext。
//   3. 发送次数为负（手改/半写）时先归零再放行（realname.go:328-333）；
//      发新码会把「验证码错误次数」清零（:346）——重复申请等于给爆破窗口续期。
//
// 其余口径：
//   - 没有 mid 守卫（logic 层与 repository 层都不查 mid），mid=0 照样发码；
//   - setCaptureCode/setCaptureTimes 走的是**无返回值的** cache.SetInt，
//     写失败被彻底吞（接口仍报成功、验证码其实没落库）；
//   - Incr / Del 的错误则原样上抛（realname.go:343、:346），此时码其实已经在缓存里活着。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/logx/logtest"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/rpc"
)

// cacheOnlyOps 断言一次发码只碰缓存（发码不落任何 MySQL 表，是本方法的边界事实）。
func cacheOnlyOps(t *testing.T, label string, ops []string) {
	t.Helper()
	for _, op := range ops {
		if !strings.HasPrefix(op, "cache.") {
			t.Errorf("%s：出现非缓存调用 %q（发码路径不应触库）", label, op)
		}
	}
}

func TestRealnameTelCaptureColdStartSequence(t *testing.T) {
	const mid = int64(51001)
	e := newEnv(t)

	reply, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "首次发码", err)
	// 出口形态：EmptyReply 一个字段都没有，验证码不出接口（调用方只能靠短信/Redis 拿到它）。
	wantEQ(t, "首次发码", "应答无字段", reply.String(), "")

	wantOps(t, "首次发码序列", e.ops(0), []string{
		"cache.GetInt:realname_cap_times_51001",       // miss → -1（cache.go:235）
		"cache.SetInt:realname_cap_times_51001/86400", // 负数先归零（realname.go:329）
		"cache.SetInt:realname_cap_code_51001/600",    // 验证码 TTL 10 分钟
		"cache.Incr:realname_cap_times_51001",         // 计数 +1
		"cache.GetInt:realname_cap_times_51001",       // 判断是否首次，补 TTL（cache.go:255）
		"cache.Expire:realname_cap_times_51001/86400", // 24 小时窗口从首发起算
		"cache.Del:realname_cap_err_times_51001",      // 错误次数清零
	})
	cacheOnlyOps(t, "首次发码", e.ops(0))

	code, ok := e.st.cache.intOf(keyCapCode(mid))
	wantEQ(t, "首次发码", "验证码已落缓存", ok, true)
	if code < 100000 || code > 999999 {
		t.Errorf("首次发码：验证码应是 6 位，实际=%d", code)
	}
	wantEQ(t, "首次发码", "验证码 TTL", e.st.cache.ttls[keyCapCode(mid)], 600)
	times, ok := e.st.cache.intOf(keyCapTimes(mid))
	wantEQ(t, "首次发码", "发送次数已计到 1", ok && times == 1, true)
	wantEQ(t, "首次发码", "发送次数 TTL", e.st.cache.ttls[keyCapTimes(mid)], 86400)
	if _, ok := e.st.cache.intOf(keyCapErr(mid)); ok {
		t.Errorf("首次发码：错误次数键应被删除，实际仍存值")
	}
	// 事务/事件/落库全为零：发码不留任何服务端台账（事后无法审计谁在什么时间要过码）。
	wantEQ(t, "首次发码", "事务次数", e.st.conn.transactions, 0)
	wantEQ(t, "首次发码", "Outbox 行数", e.st.outbox.count(), 0)
}

// TestRealnameTelCaptureSendQuotaBoundary 钉住配额的**开闭区间**：判定是 times > 5，
// 所以 24 小时内第 6 次仍放行、第 7 次才拒；且被拒时一次写都不发生（不清错误次数、
// 不覆盖已下发的码）——否则「频繁要码」会把受害者手里的码刷掉。
func TestRealnameTelCaptureSendQuotaBoundary(t *testing.T) {
	cases := []struct {
		startTimes int64
		wantOK     bool
		wantTimes  int64
	}{
		{0, true, 1}, {1, true, 2}, {4, true, 5}, {5, true, 6}, // 放行到第 6 次
		{6, false, 6}, {99, false, 99}, // 第 7 次起拒
	}
	for _, tc := range cases {
		label := fmt.Sprintf("已发%d次", tc.startTimes)
		t.Run(label, func(t *testing.T) {
			const mid = int64(51002)
			e := newEnv(t)
			e.st.cache.warmInt(keyCapTimes(mid), tc.startTimes)
			e.st.cache.warmInt(keyCapErr(mid), 3) // 已有 3 次输错
			e.st.cache.warmInt(keyCapCode(mid), 654321)
			e.st.log.reset()

			reply, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
			if !tc.wantOK {
				wantErrIs(t, label, err, repository.ErrRealnameCaptureSendTooMany)
				wantEQ(t, label, "应答为 nil", reply == nil, true)
				// 拒绝只花了一次读：配额判定发生在任何写之前。
				wantOps(t, label, e.ops(0), []string{"cache.GetInt:realname_cap_times_51002"})
				// 手里那个码、以及错误次数计数，都不因「被拒的要码请求」而变化。
				got, _ := e.st.cache.intOf(keyCapCode(mid))
				wantEQ(t, label, "旧验证码未被覆盖", got, 654321)
				gotErr, _ := e.st.cache.intOf(keyCapErr(mid))
				wantEQ(t, label, "错误次数未被清零", gotErr, 3)
				gotTimes, _ := e.st.cache.intOf(keyCapTimes(mid))
				wantEQ(t, label, "计数不再增长（拒绝不消耗配额）", gotTimes, tc.startTimes)
				return
			}
			wantNoErr(t, label, err)
			// Incr 后只在值 <=1 时补 Expire（cache.go:255）：24 小时窗口固定从**首次**发码起算，
			// 不会因为持续要码而顺延。已发 0 次这一格正好跨过那条边界，单独断言。
			wantTTLTopUp := tc.wantTimes <= 1
			ops := []string{
				"cache.GetInt:realname_cap_times_51002",
				"cache.SetInt:realname_cap_code_51002/600",
				"cache.Incr:realname_cap_times_51002",
				"cache.GetInt:realname_cap_times_51002",
			}
			if wantTTLTopUp {
				ops = append(ops, "cache.Expire:realname_cap_times_51002/86400")
			}
			ops = append(ops, "cache.Del:realname_cap_err_times_51002")
			wantOps(t, label, e.ops(0), ops)
			gotTimes, _ := e.st.cache.intOf(keyCapTimes(mid))
			wantEQ(t, label, "计数推进", gotTimes, tc.wantTimes)
			_, hasTTL := e.st.cache.ttls[keyCapTimes(mid)]
			wantEQ(t, label, "是否补了 24h TTL", hasTTL, wantTTLTopUp)
		})
	}
}

// TestRealnameTelCaptureNegativeCounterIsResetThenSends 覆盖「计数被写成负数」的自愈分支：
// captureTimes miss 返回 -1（cache.go:239），运维手工 SET 成负数也会走同一条路。
// 现状是**先归零、再照常发码**，也就是负值不会锁死，反而被洗白成 0 起步。
func TestRealnameTelCaptureNegativeCounterIsResetThenSends(t *testing.T) {
	const mid = int64(51003)
	e := newEnv(t)
	e.st.cache.warmInt(keyCapTimes(mid), -3)
	e.st.log.reset()

	_, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "负计数自愈", err)
	wantOps(t, "负计数自愈", e.ops(0), []string{
		"cache.GetInt:realname_cap_times_51003",
		"cache.SetInt:realname_cap_times_51003/86400", // 归零，且这次带上 24h TTL
		"cache.SetInt:realname_cap_code_51003/600",
		"cache.Incr:realname_cap_times_51003",
		"cache.GetInt:realname_cap_times_51003",
		"cache.Expire:realname_cap_times_51003/86400",
		"cache.Del:realname_cap_err_times_51003",
	})
	got, _ := e.st.cache.intOf(keyCapTimes(mid))
	wantEQ(t, "负计数自愈", "负三被洗成 1（配额重新满格）", got, 1)
}

// TestRealnameTelCaptureRepeatOverwritesCodeAndResetsBruteForceCounter
// 重复提交：同一个 key 被覆盖（不发第二份、不追加），
// 且每次发码都把「验证码错误次数」清零 —— 攻击者可先用正确流程拿码，
// 再靠反复要码给猜测窗口续期（发码配额 6 次/24h，每轮可再猜 4 次）。
func TestRealnameTelCaptureRepeatOverwritesCodeAndResetsBruteForceCounter(t *testing.T) {
	const mid = int64(51004)
	e := newEnv(t)

	l := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx)
	_, err := l.RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "第一次发码", err)
	first, ok := e.st.cache.intOf(keyCapCode(mid))
	wantEQ(t, "第一次发码", "第一个码在库", ok, true)

	e.st.cache.warmInt(keyCapErr(mid), 3) // 模拟已输错 3 次
	e.st.log.reset()
	_, err = l.RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "第二次发码", err)

	second, ok := e.st.cache.intOf(keyCapCode(mid))
	wantEQ(t, "第二次发码", "第二个码在库", ok, true)
	// 终态只有一个键值，且历史上确实发生过两次写 → 覆盖而非并存。
	wantEQ(t, "第二次发码", "验证码 key 数量", e.st.log.countPrefix("cache.SetInt:realname_cap_code_"), 1)
	wantEQ(t, "第二次发码", "累计写次数（含被覆盖的那次）", len(e.st.cache.writes[keyCapCode(mid)]), 2)
	wantEQ(t, "第二次发码", "终态等于最后一次写", e.st.cache.writes[keyCapCode(mid)][1], fmt.Sprint(second))
	// 两次随机码相同（概率 1e-6）不会削弱本用例：要钉的是「覆盖」而非并存，
	// 上面的两次写入 + 终态等于末次已经把它锁住了。
	if first == second {
		t.Logf("两次随机码撞成同一个（概率 1e-6）：%d", first)
	}
	gotErr, ok := e.st.cache.intOf(keyCapErr(mid))
	wantEQ(t, "第二次发码", "错误次数计数已被删除", ok, false)
	wantEQ(t, "第二次发码", "计数不残留旧值", gotErr, 0)
	gotTimes, _ := e.st.cache.intOf(keyCapTimes(mid))
	wantEQ(t, "第二次发码", "发送次数 2", gotTimes, 2)
}

// TestRealnameTelCaptureCodeIsLoggedInPlaintext 本批最高价值结论（隐私红线 AGENTS.md §7）：
// realname.go:339 用 logx.Infof 把**一次性验证码原文**打进群 INFO 日志
// （"send capture mid=%d code=%06d (sms not integrated)"）。
// 任何有日志读权限的人（含日志采集链路、SLS/ELK 保留期内的所有人）都能凭 mid
// 直接完成实名手机验证 —— 等价于绕过第二步凭据。
// 修法方向：日志只留 mid 与「已下发」事实（或留摘要），绝不带 code；
// 真实下发接入 notification 后这行开发态日志必须删除。
//
// 缺陷：services/user-profile/internal/repository/realname.go:339 ——
// 一次性凭据明文进 INFO 日志，日志无脱敏 —— 删除该字段或改为摘要。
// ⚠ 改生产代码前不要动本用例的期望。
func TestRealnameTelCaptureCodeIsLoggedInPlaintext(t *testing.T) {
	const mid = int64(51005)
	e := newEnv(t)
	logs := logtest.NewCollector(t)

	reply, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "发码日志审计", err)
	code, ok := e.st.cache.intOf(keyCapCode(mid))
	wantEQ(t, "发码日志审计", "验证码已落缓存", ok, true)

	// 先证明捕获通道在工作（否则下面的现状断言会白过）。
	needle := fmt.Sprintf("send capture mid=%d code=%06d", mid, code)
	if !strings.Contains(logs.String(), "send capture") {
		t.Fatalf("日志捕获通道失效，没收到发码那行：%q", logs.String())
	}
	// 现状哨兵：明文码出现在日志里。生产修掉这行后，本断言应变红并驱动用例改成 wantNoPII。
	if !strings.Contains(logs.String(), needle) {
		t.Errorf("期望现状是验证码明文进 INFO 日志（缺陷已登记 README），未找到 %q，实际=%q", needle, logs.String())
	}
	// 与之相对的两个出口必须是干净的：应答不带码、MySQL 一行都不写。
	wantNoPII(t, "发码日志审计", []string{fmt.Sprint(code)}, map[string]string{
		"reply": reply.String(),
	})
	cacheOnlyOps(t, "发码日志审计", e.ops(0))
	wantEQ(t, "发码日志审计", "无任何 SQL 落库", len(e.st.realname.rows)+len(e.st.apply.rows)+len(e.st.logs.rows), 0)
}

// TestRealnameTelCaptureSwallowsCodeWriteFailure 钉住「写缓存失败被吞」：
// cache.SetInt 没有返回值（repository/cache.go:229 只能 return nil），
// 于是 Redis 写不进去时接口仍报成功，但库里根本没有码 ——
// 用户收了「成功」应答却永远过不了校验，且服务端没有任何错误信号。
func TestRealnameTelCaptureSwallowsCodeWriteFailure(t *testing.T) {
	const mid = int64(51006)
	e := newEnv(t)
	e.st.cache.failWith("SetInt", errors.New("redis down"))
	logs := logtest.NewCollector(t)

	reply, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
	wantNoErr(t, "写码失败被吞", err)
	wantEQ(t, "写码失败被吞", "应答仍非空", reply != nil, true)
	if _, ok := e.st.cache.intOf(keyCapCode(mid)); ok {
		t.Errorf("写码失败被吞：库里竟仍有验证码（替身未模拟 SetInt 无返回值的语义）")
	}
	// 发送计数照样 +1（Incr 是另一条命令）：配额被一次「什么都没发出去」的请求吃掉。
	times, ok := e.st.cache.intOf(keyCapTimes(mid))
	wantEQ(t, "写码失败被吞", "计数仍被消耗", ok && times == 1, true)
	// 错误日志里没有可诊断的原因（生产只在 go-zero 内部吞掉），调用方无从分辨。
	if strings.Contains(logs.String(), "redis down") {
		t.Errorf("写码失败被吞：现状不应有业务侧错误日志，实际=%q", logs.String())
	}
	wantOps(t, "写码失败被吞", e.ops(0), []string{
		"cache.GetInt:realname_cap_times_51006",
		"cache.SetInt:realname_cap_times_51006/86400",
		"cache.SetInt:realname_cap_code_51006/600",
		"cache.Incr:realname_cap_times_51006",
		"cache.GetInt:realname_cap_times_51006",
		"cache.Expire:realname_cap_times_51006/86400",
		"cache.Del:realname_cap_err_times_51006",
	})
}

func TestRealnameTelCaptureDownstreamErrorsPropagateVerbatim(t *testing.T) {
	t.Run("Incr 失败：错误原样上抛，但码已经发出去了", func(t *testing.T) {
		const mid = int64(51007)
		e := newEnv(t)
		boom := errors.New("redis: INCR unavailable")
		e.st.cache.failWith("Incr", boom)

		reply, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
		wantEQ(t, "Incr 失败", "错误原样（不换成哨兵）", errors.Is(err, boom), true)
		wantEQ(t, "Incr 失败", "应答为 nil", reply == nil, true)
		// 半状态：码在库里、计数没涨 → 配额可被无限次要码绕过（Incr 持续失败时）。
		if _, ok := e.st.cache.intOf(keyCapCode(mid)); !ok {
			t.Errorf("Incr 失败：期望验证码已写入（写码在计数之前）")
		}
		times, _ := e.st.cache.intOf(keyCapTimes(mid))
		wantEQ(t, "Incr 失败", "计数停在 0", times, 0)
		wantOps(t, "Incr 失败", e.ops(0), []string{
			"cache.GetInt:realname_cap_times_51007",
			"cache.SetInt:realname_cap_times_51007/86400",
			"cache.SetInt:realname_cap_code_51007/600",
			"cache.Incr:realname_cap_times_51007",
		})
	})

	t.Run("Del 失败：错误上抛，码与计数都已生效", func(t *testing.T) {
		const mid = int64(51008)
		e := newEnv(t)
		boom := errors.New("redis: DEL unavailable")
		e.st.cache.failWith("Del", boom)
		e.st.cache.warmInt(keyCapTimes(mid), 2)
		e.st.cache.warmInt(keyCapErr(mid), 3)
		e.st.log.reset()

		reply, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
		wantEQ(t, "Del 失败", "错误原样", errors.Is(err, boom), true)
		wantEQ(t, "Del 失败", "应答为 nil", reply == nil, true)
		// 新码已覆盖、计数已 +1，只有错误计数没清 —— 报给调用方的失败其实是「部分成功」。
		times, _ := e.st.cache.intOf(keyCapTimes(mid))
		wantEQ(t, "Del 失败", "计数已推进", times, 3)
		gotErr, _ := e.st.cache.intOf(keyCapErr(mid))
		wantEQ(t, "Del 失败", "错误次数仍为 3", gotErr, 3)
		wantOps(t, "Del 失败", e.ops(0), []string{
			"cache.GetInt:realname_cap_times_51008",
			"cache.SetInt:realname_cap_code_51008/600",
			"cache.Incr:realname_cap_times_51008",
			"cache.GetInt:realname_cap_times_51008",
			"cache.Del:realname_cap_err_times_51008",
		})
	})

	t.Run("配额读失败：不降级放行", func(t *testing.T) {
		// cache.GetInt 在替身里没有错误分支（与真实实现一致：miss 而非 error），
		// 因此配额判定的失败只可能是「值读不到 → 按 -1 处理 → 归零放行」。
		// 本用例把这条降级路径钉成事实：坏值（非整数）等同于配额清零。
		const mid = int64(51009)
		e := newEnv(t)
		e.st.cache.strs[keyCapTimes(mid)] = "不是数字"
		e.st.log.reset()

		_, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: mid})
		wantNoErr(t, "坏计数值", err)
		wantOps(t, "坏计数值", e.ops(0), []string{
			"cache.GetInt:realname_cap_times_51009",
			"cache.SetInt:realname_cap_times_51009/86400",
			"cache.SetInt:realname_cap_code_51009/600",
			"cache.Incr:realname_cap_times_51009",
			"cache.GetInt:realname_cap_times_51009",
			"cache.Expire:realname_cap_times_51009/86400",
			"cache.Del:realname_cap_err_times_51009",
		})
		times, _ := e.st.cache.intOf(keyCapTimes(mid))
		wantEQ(t, "坏计数值", "脏值被归零后重计", times, 1)
	})
}

// TestRealnameTelCaptureHasNoMidGuard mid 非法也不拒：整个方法不查会员是否存在，
// 只按 mid 拼缓存键。于是 mid=0 / 负数也会占一个发码桶并写出真码，
// 而 Realname 链路的校验侧（RealnameTelCaptureCheck）同样只认 mid ——
// 未登录/越权调用方可为**任意他人 mid** 反复要码，把对方手里的码覆盖掉。
func TestRealnameTelCaptureHasNoMidGuard(t *testing.T) {
	cases := []struct {
		label string
		mid   int64
		key   string
	}{
		{"mid=0", 0, "realname_cap_code_0"},
		{"mid 为负", -7, "realname_cap_code_-7"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			reply, err := NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(&rpc.MemberMidReq{Mid: tc.mid})
			wantNoErr(t, tc.label, err)
			wantEQ(t, tc.label, "仍返回 EmptyReply", reply.String(), "")
			code, ok := e.st.cache.intOf(tc.key)
			wantEQ(t, tc.label, "非法 mid 照样发码", ok, true)
			if code < 100000 || code > 999999 {
				t.Errorf("%s：验证码应是 6 位，实际=%d", tc.label, code)
			}
			// 全程不查会员：既没有 base.FindOne，也没有 realname.FindOne。
			wantNoOpsWith(t, tc.label, e.ops(0), "base.")
			wantNoOpsWith(t, tc.label, e.ops(0), "realname.")
			wantNoOpsWith(t, tc.label, e.ops(0), "apply.")
		})
	}

	// 对照：入参为 nil 时 panic 在生成器之前，属于 RPC 层不会发生的形态，
	// 这里只钉「logic 层不做 nil 校验」这一事实，避免误以为有守卫。
	t.Run("in 为 nil 直接 panic", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Errorf("期望 in=nil 触发 panic（logic 层无 nil 守卫）")
			}
		}()
		e := newEnv(t)
		_, _ = NewRealnameTelCaptureLogic(context.Background(), e.svcCtx).RealnameTelCapture(nil)
	})
}
