package logic

// morallogic_test.go 覆盖 Moral 与 MoralLog。
//
// Moral 的节操值单位是 1/100（基准 7000 = 70.00，上限 10000），
// 「查无节操记录」必须回落成基准值 7000 且**不是错误**——新注册用户没有这一行，
// 把它做成错误会让所有资料页在注册后第一时间报错。
// 同时钉住它与 BaseInfo 的两处口径差异：
//  1. 命中判定靠 payload.mid != 0（没有 Cached 哨兵），所以回填的默认值必须带上请求 mid，
//     否则永远命中不了、每次都打库。
//  2. 缓存读故障时它**照样尝试回填**（BaseInfo 会跳过回填），这条差异是事实，先钉住。
//
// MoralLog 与 ExpLog 同表不同 log_type（节操 12 / 经验 11），串味会让用户在
// 「节操记录」里看到经验补发，这里显式互查一次。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func TestMoralCacheHitSkipsDB(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmJSON(keyMoral(90001), model.UserMoral{
		Mid: 90001, Moral: 8888, Added: 111, Deducted: 222, LastRecoverDate: 1700000000,
	})
	// 库里放一个差得远的值：走回源就红。
	e.st.moral.put(&model.UserMoral{Mid: 90001, Moral: 100, Added: 999999, Deducted: 999999, LastRecoverDate: 1})

	l := NewMoralLogic(context.Background(), e.svcCtx)
	reply, err := l.Moral(&rpc.MemberMidReq{Mid: 90001})
	wantNoErr(t, "节操缓存命中", err)
	wantEQ(t, "节操缓存命中", "mid", reply.GetMid(), int64(90001))
	wantEQ(t, "节操缓存命中", "moral", reply.GetMoral(), int64(8888))
	wantEQ(t, "节操缓存命中", "added", reply.GetAdded(), int64(111))
	wantEQ(t, "节操缓存命中", "deducted", reply.GetDeducted(), int64(222))
	wantEQ(t, "节操缓存命中", "last_recover_date", reply.GetLastRecoverDate(), int64(1700000000))
	wantOps(t, "节操缓存命中", e.ops(0), []string{"cache.GetJSON:moral_90001"})
}

func TestMoralCacheIsTrustedBlindlyEvenForAnotherMid(t *testing.T) {
	// 命中判定只看 payload.mid != 0，不校验它等于请求的 mid。
	// 钉住现状：缓存被写坏/串 key 时，接口会把别人的节操值原样端出去。
	e := newEnv(t)
	e.st.cache.warmJSON(keyMoral(90002), model.UserMoral{Mid: 90999, Moral: 1234})
	e.st.moral.put(&model.UserMoral{Mid: 90002, Moral: 7000})

	l := NewMoralLogic(context.Background(), e.svcCtx)
	reply, err := l.Moral(&rpc.MemberMidReq{Mid: 90002})
	wantNoErr(t, "缓存 mid 与请求 mid 不一致", err)
	wantEQ(t, "缓存 mid 与请求 mid 不一致", "透传缓存里的 mid", reply.GetMid(), int64(90999))
	wantEQ(t, "缓存 mid 与请求 mid 不一致", "透传缓存里的 moral", reply.GetMoral(), int64(1234))
	wantOps(t, "缓存 mid 与请求 mid 不一致", e.ops(0), []string{"cache.GetJSON:moral_90002"})
}

func TestMoralCacheMissReadsThroughAndBackfills(t *testing.T) {
	e := newEnv(t)
	e.st.moral.put(&model.UserMoral{Mid: 90003, Moral: 3456, Added: 500, Deducted: 4044, LastRecoverDate: 1690000000})

	l := NewMoralLogic(context.Background(), e.svcCtx)
	reply, err := l.Moral(&rpc.MemberMidReq{Mid: 90003})
	wantNoErr(t, "节操回源", err)
	wantOps(t, "节操回源", e.ops(0), []string{
		"cache.GetJSON:moral_90003",
		"moral.FindOne:90003",
		"cache.SetJSON:moral_90003/3600",
	})
	wantEQ(t, "节操回源", "mid", reply.GetMid(), int64(90003))
	wantEQ(t, "节操回源", "moral", reply.GetMoral(), int64(3456))
	wantEQ(t, "节操回源", "added", reply.GetAdded(), int64(500))
	wantEQ(t, "节操回源", "deducted", reply.GetDeducted(), int64(4044))
	wantEQ(t, "节操回源", "last_recover_date", reply.GetLastRecoverDate(), int64(1690000000))

	var cached model.UserMoral
	if !e.st.cache.jsonOf(keyMoral(90003), &cached) {
		t.Fatal("节操回源后没有回填缓存")
	}
	wantEQ(t, "节操回源", "回填 moral", cached.Moral, int64(3456))
	wantEQ(t, "节操回源", "回填 mid", cached.Mid, int64(90003))
	wantEQ(t, "节操回源", "回填 TTL", e.st.cache.ttls[keyMoral(90003)], 3600)
}

func TestMoralWithoutRowFallsBackToBaselineAndStaysCacheable(t *testing.T) {
	e := newEnv(t) // user_moral 无此 mid：新用户

	l := NewMoralLogic(context.Background(), e.svcCtx)
	reply, err := l.Moral(&rpc.MemberMidReq{Mid: 90004})
	wantNoErr(t, "无节操记录", err)
	wantOps(t, "无节操记录", e.ops(0), []string{
		"cache.GetJSON:moral_90004",
		"moral.FindOne:90004",
		"cache.SetJSON:moral_90004/3600",
	})
	wantEQ(t, "无节操记录", "回落基准值 7000", reply.GetMoral(), int64(model.DefaultMoral))
	wantEQ(t, "无节操记录", "mid 必须是请求的 mid", reply.GetMid(), int64(90004))
	wantEQ(t, "无节操记录", "added", reply.GetAdded(), int64(0))
	wantEQ(t, "无节操记录", "deducted", reply.GetDeducted(), int64(0))
	wantEQ(t, "无节操记录", "last_recover_date", reply.GetLastRecoverDate(), int64(0))

	// 默认值缓存必须**真的能命中**（命中判定看 mid != 0），否则每次都打库。
	before := e.st.log.snapshot()
	again, err := l.Moral(&rpc.MemberMidReq{Mid: 90004})
	wantNoErr(t, "默认值二次读取", err)
	wantOps(t, "默认值二次读取", e.ops(before), []string{"cache.GetJSON:moral_90004"})
	wantEQ(t, "默认值二次读取", "moral 与首次一致", again.GetMoral(), int64(model.DefaultMoral))
	wantEQ(t, "默认值二次读取", "mid 与首次一致", again.GetMid(), int64(90004))
}

func TestMoralCacheFaultStillReadsDBAndStillTriesBackfill(t *testing.T) {
	e := newEnv(t)
	e.st.moral.put(&model.UserMoral{Mid: 90005, Moral: 9990, Added: 10, Deducted: 5})
	e.st.cache.failWith("GetJSON", errors.New("redis down"))

	l := NewMoralLogic(context.Background(), e.svcCtx)
	reply, err := l.Moral(&rpc.MemberMidReq{Mid: 90005})
	wantNoErr(t, "缓存故障降级", err)
	wantEQ(t, "缓存故障降级", "moral", reply.GetMoral(), int64(9990))
	// 与 BaseInfo 不同：Moral 不记 cacheOK，故障后仍会尝试写回（口径如实钉住）。
	wantOps(t, "缓存故障降级", e.ops(0), []string{
		"cache.GetJSON:moral_90005",
		"moral.FindOne:90005",
		"cache.SetJSON:moral_90005/3600",
	})
}

func TestMoralDBFailurePropagates(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("select from user_moral: boom")
	e.st.moral.failWith("FindOne", boom)

	l := NewMoralLogic(context.Background(), e.svcCtx)
	reply, err := l.Moral(&rpc.MemberMidReq{Mid: 90006})
	wantErrIs(t, "节操库故障", err, boom)
	if reply != nil {
		t.Errorf("节操库故障：reply = %+v, want nil", reply)
	}
	wantOps(t, "节操库故障", e.ops(0), []string{"cache.GetJSON:moral_90006", "moral.FindOne:90006"})
}

func TestMoralDoesNotGuardNonPositiveMid(t *testing.T) {
	// 与 Exp/Level 一样没有 mid 守卫：mid=0 会被当成「无记录」并返回基准 7000。
	// 上游拿 mid=0 去问节操，会收到一个看起来合法的 70.00（缺陷登记）。
	e := newEnv(t)
	l := NewMoralLogic(context.Background(), e.svcCtx)
	reply, err := l.Moral(&rpc.MemberMidReq{Mid: 0})
	wantNoErr(t, "mid=0", err)
	wantEQ(t, "mid=0", "基准值冒充合法节操", reply.GetMoral(), int64(model.DefaultMoral))
	wantEQ(t, "mid=0", "mid", reply.GetMid(), int64(0))
	wantOps(t, "mid=0", e.ops(0), []string{
		"cache.GetJSON:moral_0",
		"moral.FindOne:0",
		"cache.SetJSON:moral_0/3600",
	})
}

// === MoralLog ===

func TestMoralLogQueriesOnlyMoralType(t *testing.T) {
	now := time.Now().Unix()
	e := newEnv(t)
	e.st.logs.put(model.LogTypeMoral, &model.UserLog{
		Mid: 90101, IP: "10.9.9.9", TS: now - 20, LogID: "moral-1",
		Content: map[string]string{
			"from_moral": "7000", "to_moral": "6500", "origin": "2",
			"status": "0", "mid": "90101", "remark": "人工备注", "operater": "审核丙", "reason": "违规弹幕",
		},
	}, model.LogStatusActive)
	e.st.logs.put(model.LogTypeMoral, &model.UserLog{
		Mid: 90101, IP: "10.9.9.8", TS: now - 10, LogID: "moral-2",
		Content: map[string]string{"from_moral": "6500", "to_moral": "7000", "origin": "5"},
	}, model.LogStatusActive)
	// 同一个人的经验日志绝不能出现在节操记录里。
	e.st.logs.put(model.LogTypeExp, &model.UserLog{
		Mid: 90101, TS: now - 5, LogID: "exp-混进来就完蛋", Content: map[string]string{"to_exp": "1"},
	}, model.LogStatusActive)

	l := NewMoralLogLogic(context.Background(), e.svcCtx)
	reply, err := l.MoralLog(&rpc.MemberMidReq{Mid: 90101})
	wantNoErr(t, "节操日志", err)
	wantOps(t, "节操日志", e.ops(0), []string{"memberLog.FindByMid:12/90101"})
	logs := reply.GetUserLogs()
	wantEQ(t, "节操日志", "条数（log_type=12 隔离）", len(logs), 2)
	if len(logs) != 2 {
		t.Fatalf("节操日志：条数不对，后续断言无意义")
	}
	wantEQ(t, "节操日志", "第 1 条（ts DESC）", logs[0].GetLogId(), "moral-2")
	wantEQ(t, "节操日志", "第 2 条", logs[1].GetLogId(), "moral-1")
	wantEQ(t, "节操日志", "logs[1].mid", logs[1].GetMid(), int64(90101))
	wantEQ(t, "节操日志", "logs[1].ip", logs[1].GetIp(), "10.9.9.9")
	wantEQ(t, "节操日志", "logs[1].ts", logs[1].GetTs(), now-20)
	wantContentEQ(t, "节操日志", "logs[1].content 逐键", logs[1].GetContent(), map[string]string{
		"from_moral": "7000", "to_moral": "6500", "origin": "2",
		"status": "0", "mid": "90101", "remark": "人工备注", "operater": "审核丙", "reason": "违规弹幕",
	})
}

func TestMoralLogEmptyAndFailure(t *testing.T) {
	t.Run("无记录返回空列表不报错", func(t *testing.T) {
		e := newEnv(t)
		l := NewMoralLogLogic(context.Background(), e.svcCtx)
		reply, err := l.MoralLog(&rpc.MemberMidReq{Mid: 90109})
		wantNoErr(t, "无节操日志", err)
		if reply.GetUserLogs() == nil {
			t.Error("无节操日志：UserLogs 为 nil")
		}
		wantEQ(t, "无节操日志", "条数", len(reply.GetUserLogs()), 0)
		wantOps(t, "无节操日志", e.ops(0), []string{"memberLog.FindByMid:12/90109"})
	})

	t.Run("库故障必须报错", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("member_log 超时")
		e.st.logs.failWith("FindByMid", boom)
		l := NewMoralLogLogic(context.Background(), e.svcCtx)
		reply, err := l.MoralLog(&rpc.MemberMidReq{Mid: 90110})
		wantErrIs(t, "节操日志库故障", err, boom)
		if reply != nil {
			t.Errorf("节操日志库故障：reply = %+v, want nil", reply)
		}
	})

	t.Run("mid=0 无守卫直查库", func(t *testing.T) {
		e := newEnv(t)
		l := NewMoralLogLogic(context.Background(), e.svcCtx)
		if _, err := l.MoralLog(&rpc.MemberMidReq{Mid: 0}); err != nil {
			t.Fatalf("mid=0：%v", err)
		}
		wantOps(t, "mid=0", e.ops(0), []string{"memberLog.FindByMid:12/0"})
	})
}
