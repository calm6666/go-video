package logic

// loginlogslogic_test.go 覆盖 LoginLogs（repository/login.go:620-638）。
//
// 被测判定链：loginLog.FindByMid(mid, **入参 limit 原样**) → 逐列投影成 rpc.LoginLog。
// 排序与截断都发生在 model 层（model/login_log.go:98-115：WHERE mid=?
// ORDER BY ctime DESC, id DESC LIMIT ?，并在 limit<=0 时取 20、limit>100 时压到 100），
// 替身按同一语义复刻（fakes_test.go:fakeLoginLogModel.FindByMid），所以本文件的
// 「行数」断言覆盖的是契约语义而不是 SQL 文本；「limit 原样下发」则由调用轨迹精确钉住。
//
// 钉住的关键事实：
//   - 布数据用 put（不补 now），ctime 才能互不相同 —— 否则 DESC/ASC 方向不可证；
//   - 排序是 ctime DESC，同秒再按 id DESC（用例用两行同 ctime、不同 id 卡住 tiebreak）；
//   - 投影里的 ts 是**登录时间 ts 列**，不是 ctime（两者布成不同值，取错即红）；
//   - 没有任何过滤/脱敏：mid=0 也照样查，reason 原文（含底层错误文本）照原样回传调用方；
//   - 缺口 H：表里有 buvid 列、契约 rpc.LoginLog 没有，于是设备指纹在 RPC 侧永久丢失
//     （用例断言 buvid 值确实不在应答里，把这个「查得到却传不出」的现状钉住）；
//   - 查不到是结论不是错误（空列表 + nil error）；DB 故障则原样外传、应答为 nil。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callLoginLogs(t *testing.T, e *env, mid int64, limit int32) (*rpc.LoginLogsReply, error) {
	t.Helper()
	return NewLoginLogsLogic(context.Background(), e.svcCtx).LoginLogs(
		&rpc.LoginLogsReq{Mid: mid, Limit: limit})
}

// logRow 布一行日志：ctime/id 决定排序，ts 独立可辨（见文件头口径说明）。
func logRow(mid int64, ctime, ts int64, loginType, status int8, reason, ip, device, buvid string) *model.AccountLoginLog {
	return &model.AccountLoginLog{
		Mid: mid, LoginType: loginType, Status: status, Reason: reason,
		IP: ip, Device: device, Buvid: buvid, TS: ts, CTime: ctime,
	}
}

func deviceSeq(logs []*rpc.LoginLog) []string {
	out := make([]string, 0, len(logs))
	for _, l := range logs {
		out = append(out, l.Device)
	}
	return out
}

// tsSeqStr 把 ts 列拼成一个可比较的串（顺序即返回顺序，方向错了立刻红）。
func tsSeqStr(logs []*rpc.LoginLog) string {
	out := make([]int64, 0, len(logs))
	for _, l := range logs {
		out = append(out, l.Ts)
	}
	return joinInts(out)
}

// TestLoginLogsProjectsEveryContractColumn 逐列投影：mid/ip/ts/login_type/status/reason/device
// 都必须原样落到 rpc.LoginLog；ts 取的是 ts 列而不是 ctime 列。
func TestLoginLogsProjectsEveryContractColumn(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const mid = int64(70001)
	st.loginLog.put(logRow(mid, 1700000100, 1700000999, model.LoginTypeCapture, model.LoginStatusFail,
		"capture code wrong", "1.2.3.4", "android", "BV-SEED-1"))
	st.log.reset()

	reply, err := callLoginLogs(t, e, mid, 10)
	wantNoErr(t, "登录日志查询", err)
	wantOps(t, "只有一次查库", e.ops(0), []string{"loginlog.FindByMid:" + itoa(mid) + "/10"})
	wantEQ(t, "条数", "logs 条数", len(reply.Logs), 1)

	l := reply.Logs[0]
	wantEQ(t, "投影", "mid", l.Mid, mid)
	wantEQ(t, "投影", "ip", l.Ip, "1.2.3.4")
	wantEQ(t, "投影取 ts 列而不是 ctime 列", "ts", l.Ts, int64(1700000999))
	wantEQ(t, "投影", "login_type", l.LoginType, int32(model.LoginTypeCapture))
	wantEQ(t, "投影", "status", l.Status, int32(model.LoginStatusFail))
	wantEQ(t, "投影", "reason", l.Reason, "capture code wrong")
	wantEQ(t, "投影", "device", l.Device, "android")
	// 缺口 H：account_login_log.buvid 有值，但 rpc.LoginLog 没有该字段 ⇒ 设备指纹传不出去
	if got := st.loginLog.rowsOf(mid)[0].Buvid; got != "BV-SEED-1" {
		t.Fatalf("布景破坏：库存 buvid = %q", got)
	}
	wantNotContains(t, "现状：应答里没有 buvid 载体（缺口 H）", l.String(), "BV-SEED-1")
	wantNoOpsWith(t, "日志查询只读一张表", e.ops(0), "cache.")
	wantNoOpsWith(t, "日志查询只读一张表", e.ops(0), "cred.")
	wantNoOpsWith(t, "日志查询只读一张表", e.ops(0), "session.")
	wantNoOpsWith(t, "日志查询只读一张表", e.ops(0), "userProfile.")
}

// TestLoginLogsOrderIsCtimeDescThenIdDesc 排序方向与同秒 tiebreak：
// 布数据时刻意让 ctime 与插入序不同（A100/B300/C200/D300），
// 于是 ASC 会得到 d1,d3,d2,d4，而「ctime DESC, id ASC」会得到 d2,d4,d3,d1，
// 只有「ctime DESC, id DESC」才是 d4,d2,d3,d1。
func TestLoginLogsOrderIsCtimeDescThenIdDesc(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const mid = int64(70001)
	st.loginLog.put(logRow(mid, 100, 1000, model.LoginTypePassword, model.LoginStatusOK, "", "1.1.1.1", "d1", "b1"))
	st.loginLog.put(logRow(mid, 300, 3000, model.LoginTypePassword, model.LoginStatusOK, "", "2.2.2.2", "d2", "b2"))
	st.loginLog.put(logRow(mid, 200, 2000, model.LoginTypePassword, model.LoginStatusOK, "", "3.3.3.3", "d3", "b3"))
	st.loginLog.put(logRow(mid, 300, 3001, model.LoginTypePassword, model.LoginStatusOK, "", "4.4.4.4", "d4", "b4"))
	st.log.reset()

	reply, err := callLoginLogs(t, e, mid, 10)
	wantNoErr(t, "排序查询", err)
	wantEQ(t, "条数", "logs 条数", len(reply.Logs), 4)
	wantOps(t, "设备序（同 ctime 的行按 id 倒序）", deviceSeq(reply.Logs), []string{"d4", "d2", "d3", "d1"})
	wantEQ(t, "ts 序列（ctime DESC 方向）", "ts 序列", tsSeqStr(reply.Logs), "3001,3000,2000,1000")
	// ip 与 mid 不得串到别的行上
	for i, l := range reply.Logs {
		wantEQ(t, "第 "+itoa(int64(i+1))+" 行的 mid", "mid", l.Mid, mid)
	}
}

// TestLoginLogsLimitIsForwardedVerbatim repository 不做任何兜底/夹紧：
// 三条轨迹分别显示 0、-7、500 被**原样**下发到 model（夹紧只发生在 model 层）。
func TestLoginLogsLimitIsForwardedVerbatim(t *testing.T) {
	cases := []struct {
		name  string
		limit int32
	}{
		{"零", 0},
		{"负数", -7},
		{"超过存储行数", 500},
		{"恰好一行", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			const mid = int64(70001)
			st.loginLog.put(logRow(mid, 100, 1000, model.LoginTypePassword, model.LoginStatusOK, "", "1.1.1.1", "d1", "b1"))
			st.loginLog.put(logRow(mid, 200, 2000, model.LoginTypePassword, model.LoginStatusOK, "", "2.2.2.2", "d2", "b2"))
			st.loginLog.put(logRow(mid, 300, 3000, model.LoginTypePassword, model.LoginStatusOK, "", "3.3.3.3", "d3", "b3"))
			st.log.reset()

			reply, err := callLoginLogs(t, e, mid, tc.limit)
			wantNoErr(t, "limit 下发", err)
			wantOps(t, "limit 原样进 model", e.ops(0),
				[]string{"loginlog.FindByMid:" + itoa(mid) + "/" + itoa(int64(tc.limit))})
			if reply == nil {
				t.Fatalf("应答 = nil, want 一条 LoginLogsReply")
			}
		})
	}
}

// TestLoginLogsRowCountsFollowModelClamp 行数口径（语义在 model/login_log.go:99-104，
// 替身同口径实现，所以这三档断言证明的是「默认 20 / 上限 100 / 不足不补位」这套契约语义）：
//   - limit=0 与 limit=-7 → 默认 20 条（布 21 行时确实只回 20）；
//   - limit=2 → 只回最新 2 条，且顺序仍是 DESC；
//   - limit=500 而库里只有 3 行 → 回 3 行（不补空、不报错）。
func TestLoginLogsRowCountsFollowModelClamp(t *testing.T) {
	const mid = int64(70001)
	build := func(t *testing.T, rows int) *env {
		e := newEnv(t)
		for i := 1; i <= rows; i++ {
			// ctime 随插入序递增，因此最新的是最后一行（device=dN）
			e.st.loginLog.put(logRow(mid, int64(100+i), int64(1000+i),
				model.LoginTypePassword, model.LoginStatusOK, "", "9.9.9.9", "d"+itoa(int64(i)), "b"))
		}
		e.st.log.reset()
		return e
	}

	t.Run("limit=0 时默认 20 条", func(t *testing.T) {
		e := build(t, 21)
		reply, err := callLoginLogs(t, e, mid, 0)
		wantNoErr(t, "limit=0", err)
		wantEQ(t, "布了 21 行只回 20 行", "logs 条数", len(reply.Logs), 20)
		wantEQ(t, "首行仍是最新一条", "device", reply.Logs[0].Device, "d21")
		wantEQ(t, "末行是第 2 新的记录（第 1 新之后的第 20 条）", "device", reply.Logs[19].Device, "d2")
	})

	t.Run("limit=-7 同样落到默认 20", func(t *testing.T) {
		e := build(t, 21)
		reply, err := callLoginLogs(t, e, mid, -7)
		wantNoErr(t, "limit=-7", err)
		wantEQ(t, "负数不报错、按默认 20 截断", "logs 条数", len(reply.Logs), 20)
	})

	t.Run("limit=2 只回最新两条且顺序不变", func(t *testing.T) {
		e := build(t, 5)
		reply, err := callLoginLogs(t, e, mid, 2)
		wantNoErr(t, "limit=2", err)
		wantEQ(t, "条数", "logs 条数", len(reply.Logs), 2)
		wantOps(t, "设备序", deviceSeq(reply.Logs), []string{"d5", "d4"})
	})

	t.Run("limit 大于库存行数不补位", func(t *testing.T) {
		e := build(t, 3)
		reply, err := callLoginLogs(t, e, mid, 500)
		wantNoErr(t, "limit=500", err)
		wantEQ(t, "有几行回几行", "logs 条数", len(reply.Logs), 3)
		wantOps(t, "设备序", deviceSeq(reply.Logs), []string{"d3", "d2", "d1"})
	})
}

// TestLoginLogsFiltersByMidAndTreatsEmptyAsConclusion WHERE mid 生效 + 空结果是结论：
// 陌生 mid（含 mid=0）回**空列表**且无错误，应答对象非 nil。
func TestLoginLogsFiltersByMidAndTreatsEmptyAsConclusion(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.loginLog.put(logRow(70001, 100, 1000, model.LoginTypePassword, model.LoginStatusOK, "", "1.1.1.1", "d1", "b1"))
	st.loginLog.put(logRow(70002, 200, 2000, model.LoginTypePassword, model.LoginStatusOK, "", "2.2.2.2", "d2", "b2"))
	st.log.reset()

	reply, err := callLoginLogs(t, e, 70002, 10)
	wantNoErr(t, "按 mid 查询", err)
	wantEQ(t, "只回本 mid 的行", "logs 条数", len(reply.Logs), 1)
	wantEQ(t, "本 mid 的行内容", "device", reply.Logs[0].Device, "d2")
	wantEQ(t, "本 mid 的行内容", "mid", reply.Logs[0].Mid, int64(70002))

	st.log.reset()
	empty, err := callLoginLogs(t, e, 999999, 10)
	wantNoErr(t, "从未登录过的 mid 是结论不是错误", err)
	if empty == nil {
		t.Fatalf("应答 = nil, want 一条空列表的 LoginLogsReply")
	}
	wantEQ(t, "空结果条数", "logs 条数", len(empty.Logs), 0)
	if empty.Logs == nil {
		t.Errorf("logs = nil, want 非 nil 空列表（RPC 侧按 len 判定，不该让调用方处理 nil）")
	}
	wantOps(t, "空结果也照样查库（没有 mid 合法性短路）", e.ops(0),
		[]string{"loginlog.FindByMid:999999/10"})

	// mid=0 同样不校验：等价于查「没有归属」的那批审计行
	st.loginLog.put(logRow(0, 300, 3000, model.LoginTypePassword, model.LoginStatusFail,
		"account not exist", "8.8.8.8", "unknown", ""))
	st.log.reset()
	zero, err := callLoginLogs(t, e, 0, 10)
	wantNoErr(t, "mid=0 查询", err)
	wantEQ(t, "mid=0 能查到匿名失败记录", "logs 条数", len(zero.Logs), 1)
	wantEQ(t, "匿名记录内容", "reason", zero.Logs[0].Reason, "account not exist")
}

// TestLoginLogsFaultPropagatesRaw 查库故障：错误原样外传、应答为 nil，
// 不许被折成空列表（那会让「审计面查不到」看起来像「用户从没登录过」）。
func TestLoginLogsFaultPropagatesRaw(t *testing.T) {
	e := newEnv(t)
	st := e.st
	boom := errors.New("account/db: login log lookup down")
	st.loginLog.failWith("FindByMid", boom)

	reply, err := callLoginLogs(t, e, 70001, 10)
	if reply != nil {
		t.Errorf("应答 = %+v, want nil（故障绝不能伪装成空列表）", reply)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样外传 %v", err, boom)
	}
	wantOps(t, "故障路径只查一次库", e.ops(0), []string{"loginlog.FindByMid:70001/10"})
}

// TestLoginLogsNoSanitisationOfReason reason 列不做任何脱敏或截断：
// 底层 SQL/驱动错误原文会被完整回传给调用方（缺口 10 的读侧同类，网关若直接透传
// 就把内部错误文本交给了终端）。
func TestLoginLogsNoSanitisationOfReason(t *testing.T) {
	e := newEnv(t)
	st := e.st
	const raw = "Error 1062: Duplicate entry 'tok' for key 'account_session.uk_token' at row 1"
	st.loginLog.put(logRow(70001, 100, 1000, model.LoginTypeRegister, model.LoginStatusFail,
		raw, "1.1.1.1", "d1", "b1"))
	st.log.reset()

	reply, err := callLoginLogs(t, e, 70001, 10)
	wantNoErr(t, "原始错误文本回传", err)
	wantEQ(t, "reason 一字不改地外传", "reason", reply.Logs[0].Reason, raw)
}
