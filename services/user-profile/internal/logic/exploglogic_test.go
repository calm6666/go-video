package logic

// exploglogic_test.go 覆盖 ExpLog 与 ExpStat。
//
// ExpLog 的口径在 model.MemberLogModel.FindByMid：只取最近 7 天、status=0、
// 按 ts DESC + id DESC、最多 1000 条，并且 **按 log_type 隔离**（经验 11 / 节操 12）。
// 日志类型串味是最隐蔽的缺陷：两种日志同表，一旦 log_type 传错，用户会在
// 「经验记录」页看到自己的扣分明细。
//
// ExpStat 是纯 Redis 位图读（不触 MySQL），但它**不降级**：GetBit 故障会如实上抛
// （与 BaseInfo/Moral 的「缓存故障吞成 miss」口径相反，见 README 已知缺口）。
// 这里的期望串一律写**字面量**（ea_login_ / ea_shareClick_ / ecoin_），
// 不用 fakes 里的常量拼接——否则生产改了 key 或类型名，用例还会是绿的。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// wantContentEQ 比较日志 content map（wantEQ 受 comparable 约束，map 只能另走这条）。
func wantContentEQ(t *testing.T, label, field string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s：%s 键数 = %d (%v), want %d (%v)", label, field, len(got), keysOf(got), len(want), keysOf(want))
		return
	}
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			t.Errorf("%s：%s 缺键 %q", label, field, k)
			continue
		}
		if g != v {
			t.Errorf("%s：%s[%q] = %q, want %q", label, field, k, g, v)
		}
	}
}

func keysOf(m map[string]string) []string {
	ps := make([]string, 0, len(m))
	for k := range m {
		ps = append(ps, k)
	}
	sort.Strings(ps)
	return ps
}

func TestExpLogProjectsRecentActiveLogsInOrder(t *testing.T) {
	now := time.Now().Unix()
	e := newEnv(t)
	// 三条有效经验日志：两条同刻（靠 id DESC 决出先后），一条更早。
	e.st.logs.put(model.LogTypeExp, &model.UserLog{
		Mid: 50001, IP: "10.1.1.1", TS: now - 100, LogID: "exp-先写的",
		Content: map[string]string{"from_exp": "1", "to_exp": "2", "operater": "运营甲", "reason": "补发登录奖励"},
	}, model.LogStatusActive)
	e.st.logs.put(model.LogTypeExp, &model.UserLog{
		Mid: 50001, IP: "10.1.1.2", TS: now - 100, LogID: "exp-后写的",
		Content: map[string]string{"from_exp": "2", "to_exp": "3", "operater": "运营乙", "reason": "纠错"},
	}, model.LogStatusActive)
	e.st.logs.put(model.LogTypeExp, &model.UserLog{
		Mid: 50001, IP: "10.1.1.3", TS: now - 3600, LogID: "exp-更早的",
		Content: map[string]string{"from_exp": "0", "to_exp": "1"},
	}, model.LogStatusActive)
	// 三条必须**不出现**的：超窗、已撤销、别的日志类型。
	e.st.logs.put(model.LogTypeExp, &model.UserLog{Mid: 50001, TS: now - 8*24*3600, LogID: "exp-超窗"}, model.LogStatusActive)
	e.st.logs.put(model.LogTypeExp, &model.UserLog{Mid: 50001, TS: now - 60, LogID: "exp-已撤销"}, model.LogStatusRevoked)
	e.st.logs.put(model.LogTypeMoral, &model.UserLog{Mid: 50001, TS: now - 60, LogID: "moral-串味的"}, model.LogStatusActive)
	// 别的 mid 的经验日志也不能混进来。
	e.st.logs.put(model.LogTypeExp, &model.UserLog{Mid: 50002, TS: now - 60, LogID: "别人的"}, model.LogStatusActive)

	l := NewExpLogLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpLog(&rpc.MidReq{Mid: 50001})
	wantNoErr(t, "经验日志", err)
	wantOps(t, "经验日志", e.ops(0), []string{"memberLog.FindByMid:11/50001"})

	logs := reply.GetUserLogs()
	wantEQ(t, "经验日志", "条数（超窗/已撤销/他类型/他人均被排除）", len(logs), 3)
	if len(logs) != 3 {
		t.Fatalf("经验日志：条数不对，后续断言无意义")
	}
	wantEQ(t, "经验日志", "第 1 条：同刻后写的排前面（id DESC）", logs[0].GetLogId(), "exp-后写的")
	wantEQ(t, "经验日志", "第 2 条", logs[1].GetLogId(), "exp-先写的")
	wantEQ(t, "经验日志", "第 3 条：时间更晚写入的 ts 更小（ts DESC）", logs[2].GetLogId(), "exp-更早的")

	wantEQ(t, "经验日志", "logs[0].mid", logs[0].GetMid(), int64(50001))
	wantEQ(t, "经验日志", "logs[0].ip", logs[0].GetIp(), "10.1.1.2")
	wantEQ(t, "经验日志", "logs[0].ts", logs[0].GetTs(), now-100)
	wantContentEQ(t, "经验日志", "logs[0].content", logs[0].GetContent(), map[string]string{
		"from_exp": "2", "to_exp": "3", "operater": "运营乙", "reason": "纠错",
	})
	wantEQ(t, "经验日志", "logs[2].ip", logs[2].GetIp(), "10.1.1.3")
	wantEQ(t, "经验日志", "logs[2].ts", logs[2].GetTs(), now-3600)
	wantContentEQ(t, "经验日志", "logs[2].content（只有两个键，不得凭空补字段）", logs[2].GetContent(), map[string]string{
		"from_exp": "0", "to_exp": "1",
	})
}

func TestExpLogEmptyResultIsNonNilSlice(t *testing.T) {
	e := newEnv(t)
	l := NewExpLogLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpLog(&rpc.MidReq{Mid: 50009})
	wantNoErr(t, "无日志", err)
	if reply.GetUserLogs() == nil {
		t.Error("无日志：UserLogs 为 nil，上游 range 之外做 len 判断虽安全但口径不一致")
	}
	wantEQ(t, "无日志", "条数", len(reply.GetUserLogs()), 0)
	wantOps(t, "无日志", e.ops(0), []string{"memberLog.FindByMid:11/50009"})
}

func TestExpLogSkipsRowWithUnparsableContent(t *testing.T) {
	now := time.Now().Unix()
	e := newEnv(t)
	good := e.st.logs.put(model.LogTypeExp, &model.UserLog{
		Mid: 50011, TS: now - 10, LogID: "good", Content: map[string]string{"to_exp": "7"},
	}, model.LogStatusActive)
	bad := e.st.logs.put(model.LogTypeExp, &model.UserLog{
		Mid: 50011, TS: now - 5, LogID: "bad", Content: map[string]string{"to_exp": "8"},
	}, model.LogStatusActive)
	bad.Content = `{"to_exp":` // 半截 JSON：真实实现是 continue，不是报错
	if good.ID == 0 {
		t.Fatal("布景失败")
	}

	l := NewExpLogLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpLog(&rpc.MidReq{Mid: 50011})
	wantNoErr(t, "坏 JSON 行", err)
	logs := reply.GetUserLogs()
	wantEQ(t, "坏 JSON 行", "条数（坏行跳过而非整体报错）", len(logs), 1)
	if len(logs) == 1 {
		wantEQ(t, "坏 JSON 行", "留下的那条", logs[0].GetLogId(), "good")
	}
}

func TestExpLogCapsAt1000Rows(t *testing.T) {
	now := time.Now().Unix()
	e := newEnv(t)
	for i := 0; i < 1005; i++ {
		e.st.logs.put(model.LogTypeExp, &model.UserLog{
			Mid: 50013, TS: now - int64(i), LogID: "log-" + strconv.Itoa(i),
			Content: map[string]string{"seq": strconv.Itoa(i)},
		}, model.LogStatusActive)
	}
	l := NewExpLogLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpLog(&rpc.MidReq{Mid: 50013})
	wantNoErr(t, "日志上限", err)
	logs := reply.GetUserLogs()
	wantEQ(t, "日志上限", "条数（LIMIT 1000）", len(logs), 1000)
	if len(logs) == 1000 {
		wantEQ(t, "日志上限", "第一条是最新写入的", logs[0].GetLogId(), "log-0")
		wantEQ(t, "日志上限", "最后一条", logs[999].GetLogId(), "log-999")
	}
	wantEQ(t, "日志上限", "只查一次库", e.st.log.countPrefix("memberLog.FindByMid"), 1)
}

func TestExpLogDBFailurePropagates(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("select from member_log: too many connections")
	e.st.logs.failWith("FindByMid", boom)

	l := NewExpLogLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpLog(&rpc.MidReq{Mid: 50015})
	wantErrIs(t, "日志库故障", err, boom)
	if reply != nil {
		t.Errorf("日志库故障：reply = %+v, want nil", reply)
	}
	wantOps(t, "日志库故障", e.ops(0), []string{"memberLog.FindByMid:11/50015"})
}

// === ExpStat ===

// statOps 拼出 ExpStat 期望的完整调用序列（key 与类型名一律字面量）。
func statOps(day, mid int64) []string {
	shard := mid / 10000
	offset := mid % 10000
	bit := func(tp string) string {
		return fmt.Sprintf("cache.GetBit:ea_%s_%d_%d/%d", tp, day, shard, offset)
	}
	return []string{
		bit("login"),
		bit("watch"),
		bit("shareClick"),
		fmt.Sprintf("cache.GetInt:ecoin_%d_%d", day, mid),
	}
}

func TestExpStatReadsThreeBitsThenCoin(t *testing.T) {
	day := int64(time.Now().Day())
	const mid = int64(12345678) // shard=1234、offset=5678，两个数都不一样才测得出顺序
	e := newEnv(t)
	e.st.cache.warmBit(fmt.Sprintf("ea_login_%d_1234", day), 5678, true)
	e.st.cache.warmBit(fmt.Sprintf("ea_shareClick_%d_1234", day), 5678, true)
	e.st.cache.warmInt(fmt.Sprintf("ecoin_%d_%d", day, mid), 7)

	l := NewExpStatLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpStat(&rpc.MidReq{Mid: mid})
	wantNoErr(t, "当日统计", err)
	wantOps(t, "当日统计", e.ops(0), statOps(day, mid))
	wantEQ(t, "当日统计", "login", reply.GetLogin(), true)
	wantEQ(t, "当日统计", "watch 未置位必须是 false", reply.GetWatch(), false)
	wantEQ(t, "当日统计", "share", reply.GetShare(), true)
	wantEQ(t, "当日统计", "coin", reply.GetCoin(), int64(7))
	// 纯缓存读：一颗 MySQL 都不许碰。
	for _, p := range []string{"base.", "exp.", "moral.", "flag.", "memberLog.", "monitor."} {
		if n := e.st.log.countPrefix(p); n != 0 {
			t.Errorf("当日统计：触碰了 %s* 共 %d 次，want 0", p, n)
		}
	}
}

func TestExpStatEmptyCacheIsAllZeroNotError(t *testing.T) {
	day := int64(time.Now().Day())
	e := newEnv(t)
	l := NewExpStatLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpStat(&rpc.MidReq{Mid: 4242})
	wantNoErr(t, "统计空缓存", err)
	wantEQ(t, "统计空缓存", "login", reply.GetLogin(), false)
	wantEQ(t, "统计空缓存", "watch", reply.GetWatch(), false)
	wantEQ(t, "统计空缓存", "share", reply.GetShare(), false)
	wantEQ(t, "统计空缓存", "coin", reply.GetCoin(), int64(0))
	wantOps(t, "统计空缓存", e.ops(0), statOps(day, 4242))
	// 只读不写：ExpStat 不得顺手补键。
	for _, p := range []string{"cache.Set", "cache.Del", "cache.Incr", "cache.Expire"} {
		if n := e.st.log.countPrefix(p); n != 0 {
			t.Errorf("统计空缓存：%s 发生了 %d 次，want 0", p, n)
		}
	}
}

func TestExpStatShardAndOffsetSplit(t *testing.T) {
	day := int64(time.Now().Day())
	for _, tc := range []struct {
		mid           int64
		shard, offset int64
	}{
		{1, 0, 1},
		{9999, 0, 9999},
		{10000, 1, 0},
		{10001, 1, 1},
		{12345678, 1234, 5678},
	} {
		e := newEnv(t)
		l := NewExpStatLogic(context.Background(), e.svcCtx)
		if _, err := l.ExpStat(&rpc.MidReq{Mid: tc.mid}); err != nil {
			t.Fatalf("mid=%d：%v", tc.mid, err)
		}
		wantEQ(t, "分片", "shard", tc.mid/10000, tc.shard)
		wantEQ(t, "分片", "offset", tc.mid%10000, tc.offset)
		wantOps(t, fmt.Sprintf("mid=%d", tc.mid), e.ops(0), statOps(day, tc.mid))
	}
}

func TestExpStatBitFailurePropagatesWithoutDegrading(t *testing.T) {
	day := int64(time.Now().Day())
	e := newEnv(t)
	boom := errors.New("redis GETBIT: READONLY")
	e.st.cache.failWith("GetBit", boom)

	l := NewExpStatLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpStat(&rpc.MidReq{Mid: 12345678})
	wantErrIs(t, "位图读故障", err, boom)
	if reply != nil {
		t.Errorf("位图读故障：reply = %+v, want nil", reply)
	}
	// 第一个位图就失败：不得继续读后两个位、也不得降级成「全 0 成功」。
	wantOps(t, "位图读故障", e.ops(0), []string{fmt.Sprintf("cache.GetBit:ea_login_%d_1234/5678", day)})
}

func TestExpStatCoinStaysZeroWhenOnlyCoinMissing(t *testing.T) {
	day := int64(time.Now().Day())
	const mid = int64(22222)
	e := newEnv(t)
	e.st.cache.warmBit(fmt.Sprintf("ea_watch_%d_2", day), 2222, true)

	l := NewExpStatLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpStat(&rpc.MidReq{Mid: mid})
	wantNoErr(t, "投币数缺失", err)
	wantOps(t, "投币数缺失", e.ops(0), statOps(day, mid))
	wantEQ(t, "投币数缺失", "watch", reply.GetWatch(), true)
	wantEQ(t, "投币数缺失", "login", reply.GetLogin(), false)
	wantEQ(t, "投币数缺失", "coin 缺失补 0 而非报错", reply.GetCoin(), int64(0))
}

func TestExpStatDoesNotGuardNonPositiveMid(t *testing.T) {
	// 与 Exp/Level 同样没有 mid 守卫：0 会去读 ea_login_<day>_0 的第 0 位。
	day := int64(time.Now().Day())
	e := newEnv(t)
	l := NewExpStatLogic(context.Background(), e.svcCtx)
	reply, err := l.ExpStat(&rpc.MidReq{Mid: 0})
	wantNoErr(t, "mid=0", err)
	wantEQ(t, "mid=0", "login", reply.GetLogin(), false)
	wantOps(t, "mid=0", e.ops(0), statOps(day, 0))
}
