package logic

// setexplogic_test.go 覆盖 SetExp（直接设置经验值，注释自称「仅运营」）。
//
// 这是这批写方法里**唯一真带前置守卫**的一个：Repository.SetExp 先 checkExpMember
// （BaseInfo → mid<1 → ErrRequestErr；base.Mid==0 → ErrMemberNotExist；
// base.Rank < 10000 → ErrUserNoMember），再读旧经验、写目标值、写经验日志、失效 exp_。
// 守卫必须发生在碰 exp_ 之前——所以守卫表用例会断言「轨迹里没有任何 exp. 调用」。
//
// 链路事实（决定断言形状）：
//   - 目标是**绝对赋值**不是增量：target = int64(count × model.ExpMulti)，count 是 float64，
//     所以小数被**向零截断**，且 count<0 会算出负 target（DDL `exp` BIGINT UNSIGNED 会拒）；
//   - 与 UpdateExp 的口径差异：UpdateExp 有 `count == 0 → 直接 return nil`，
//     SetExp **没有**，count=0 会真的把经验清零并留下一条日志；
//   - addExpLog 的错误被 logx.Errorf 吞掉（返回值被丢弃）→ 经验改了但审计丢了仍是成功；
//   - 整条链路**不在事务里**（无 TransactCtx），也**不发 Outbox 事件**；
//   - Repository.exp() 会先把**旧值** SetInt 进 exp_<mid>（TTL 86400），
//     最后才 delExpCache 删掉它：一旦删除失败，缓存里躺着的就是这次写入之前的旧经验。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// memberMid 是绝大多数用例共用的会员账号（rank=10000 恰好过闸门）。
func memberBase(mid, rank int64) *model.UserBase {
	return &model.UserBase{Mid: mid, Name: "会员" + itoa(mid), Rank: rank, Birthday: model.DefaultTime}
}

// TestSetExpGuardTable 守卫表：三类拒绝都必须在**触达经验之前**发生。
func TestSetExpGuardTable(t *testing.T) {
	t.Run("mid 非法：ErrRequestErr 且一步都不许走", func(t *testing.T) {
		for _, mid := range []int64{0, -1, -99999} {
			e := newEnv(t)
			l := NewSetExpLogic(context.Background(), e.svcCtx)
			reply, err := l.SetExp(&rpc.AddExpReq{Mid: mid, Count: 100, Operate: "补发", Reason: "r", Ip: "1.2.3.4"})
			if !errors.Is(err, repository.ErrRequestErr) {
				t.Errorf("mid=%d err = %v, want ErrRequestErr", mid, err)
			}
			if reply != nil {
				t.Errorf("mid=%d reply = %+v, want nil", mid, reply)
			}
			wantNoCall(t, "mid 非法", e.st, 0)
		}
	})

	t.Run("账号不存在：ErrMemberNotExist 且不碰经验表", func(t *testing.T) {
		e := newEnv(t)
		l := NewSetExpLogic(context.Background(), e.svcCtx)
		_, err := l.SetExp(&rpc.AddExpReq{Mid: 35001, Count: 100})
		if !errors.Is(err, repository.ErrMemberNotExist) {
			t.Fatalf("账号不存在 err = %v, want ErrMemberNotExist", err)
		}
		// 守卫前的读路径本身会回填一条「mid=0 空资料」缓存（防击穿），这是既有设计；
		// 关键断言是：经验表一次都没被读或写。
		wantOps(t, "账号不存在", e.ops(0), []string{
			"cache.GetJSON:bs_35001",
			"base.FindOne:35001",
			"cache.SetJSON:bs_35001/3600",
		})
		wantNoOpsWith(t, "账号不存在", e.ops(0), "exp.")
		wantEQ(t, "账号不存在", "member_log 行数", e.st.logs.count(), 0)
	})

	t.Run("非会员：ErrUserNoMember 且不碰经验表", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(memberBase(35002, model.DefaultRank)) // 5000 < 10000
		e.st.exp.put(35002, 5555)
		l := NewSetExpLogic(context.Background(), e.svcCtx)
		_, err := l.SetExp(&rpc.AddExpReq{Mid: 35002, Count: 100})
		if !errors.Is(err, repository.ErrUserNoMember) {
			t.Fatalf("非会员 err = %v, want ErrUserNoMember", err)
		}
		wantNoOpsWith(t, "非会员", e.ops(0), "exp.")
		wantEQ(t, "非会员", "经验值未被改动", e.st.exp.value(35002), int64(5555))
		wantEQ(t, "非会员", "member_log 行数", e.st.logs.count(), 0)
	})

	t.Run("rank 阈值边界：9999 拒 / 10000 放行", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(memberBase(35003, 9999))
		e.st.base.put(memberBase(35004, 10000))
		l := NewSetExpLogic(context.Background(), e.svcCtx)

		_, err := l.SetExp(&rpc.AddExpReq{Mid: 35003, Count: 1})
		if !errors.Is(err, repository.ErrUserNoMember) {
			t.Errorf("rank=9999 err = %v, want ErrUserNoMember（阈值是 >= 10000）", err)
		}
		wantNoOpsWith(t, "rank=9999", e.ops(0), "exp.")

		e.st.log.reset()
		_, err = l.SetExp(&rpc.AddExpReq{Mid: 35004, Count: 1})
		wantNoErr(t, "rank=10000", err)
		if ops := e.ops(0); len(ops) == 0 || ops[len(ops)-1] != "cache.Del:exp_35004" {
			t.Errorf("rank=10000：调用序列 = %v, want 以 cache.Del:exp_35004 收尾", ops)
		}
	})

	t.Run("守卫读的是 bs_ 缓存里的 rank（缓存脏则闸门跟着脏）", func(t *testing.T) {
		// checkExpMember 走 BaseInfo，所以 bs_ 命中时**根本不看库里的 rank**。
		// 结合 SetRank 的「Del 失败被吞」缺陷，这构成一条真实的越权路径：
		// 降权写库成功、缓存失效失败 → 24h 内仍能按旧的高 rank 通过闸门。
		e := newEnv(t)
		e.st.base.put(memberBase(35005, model.DefaultRank)) // 库里 5000：非会员
		e.st.cache.warmJSON(keyBase(35005), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
			Mid: 35005, Rank: 20000,
		}})
		l := NewSetExpLogic(context.Background(), e.svcCtx)
		_, err := l.SetExp(&rpc.AddExpReq{Mid: 35005, Count: 7})
		wantNoErr(t, "缓存脏 rank 越过会员闸门", err)
		wantOps(t, "缓存脏 rank 越过会员闸门", e.ops(0), []string{
			"cache.GetJSON:bs_35005", // 命中 → 不回源，库里 rank=5000 从未被看
			"cache.GetInt:exp_35005",
			"exp.FindOne:35005",
			"cache.SetInt:exp_35005/86400",
			"exp.Set:35005/700",
			"memberLog.Add:11/35005/1",
			"cache.Del:exp_35005",
		})
	})
}

// TestSetExpSetsAbsoluteTargetWithTruncation 钉 count 的换算口径：
// 绝对赋值 + ×100 + 向零截断；并和 UpdateExp 的「count=0 早返回」做对照。
func TestSetExpSetsAbsoluteTargetWithTruncation(t *testing.T) {
	cases := []struct {
		label    string
		count    float64
		wantRows int64 // 写入 exp 列的值
	}{
		{"整百单位：12345 分 → 1234500", 12345, 1234500},
		{"1 分 → 100", 1, 100},
		{"小数 0.5 分 → 50", 0.5, 50},
		{"小数 1.999 向零截断 → 199", 1.999, 199},
		{"count=0 真的清零（UpdateExp 在这里会早返回）", 0, 0},
		{"count=0.001 截断成 0", 0.001, 0},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			e.st.base.put(memberBase(35100, 10000))
			e.st.exp.put(35100, 999999) // 旧值刻意与目标值不同，防「断言刚写入的值」
			e.st.cache.warmInt(keyExp(35100), 999999)

			l := NewSetExpLogic(context.Background(), e.svcCtx)
			_, err := l.SetExp(&rpc.AddExpReq{Mid: 35100, Count: tc.count, Operate: "补发", Reason: "运营调整"})
			wantNoErr(t, tc.label, err)

			// 命中缓存时不再回源读经验：序列里没有 exp.FindOne。
			wantOps(t, tc.label, e.ops(0), []string{
				"cache.GetJSON:bs_35100",
				"base.FindOne:35100",
				"cache.SetJSON:bs_35100/3600",
				"cache.GetInt:exp_35100",
				"exp.Set:35100/" + itoa(tc.wantRows),
				"memberLog.Add:11/35100/1",
				"cache.Del:exp_35100",
			})
			wantEQ(t, tc.label, "exp 列被绝对赋值", e.st.exp.value(35100), tc.wantRows)
		})
	}

	// 对照：UpdateExp 的 count=0 什么都不做（两个经验写入口对 0 的口径不一致）。
	e := newEnv(t)
	e.st.base.put(memberBase(35101, 10000))
	e.st.exp.put(35101, 12345)
	_, err := NewUpdateExpLogic(context.Background(), e.svcCtx).UpdateExp(&rpc.AddExpReq{Mid: 35101, Count: 0})
	wantNoErr(t, "对照 UpdateExp count=0", err)
	wantOps(t, "对照 UpdateExp count=0", e.ops(0), []string{
		"cache.GetJSON:bs_35101",
		"base.FindOne:35101",
		"cache.SetJSON:bs_35101/3600",
	})
	wantEQ(t, "对照 UpdateExp count=0", "经验值没被动", e.st.exp.value(35101), int64(12345))
	wantEQ(t, "对照 UpdateExp count=0", "日志行数", e.st.logs.count(), 0)
}

// TestSetExpNegativeCountIsNotGuarded 钉「count 不校验正负」。
// TODO(缺陷)：DDL `exp` BIGINT UNSIGNED，负 target 在真库是 1264 越界；
// 本层既不拒绝也不裁剪，参数原样乘 100 后下传（替身按 int64 存，
// 所以这里断言的是「下传给 model 的值就是负数」这一步的事实）。
func TestSetExpNegativeCountIsNotGuarded(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(memberBase(35200, 10000))
	e.st.exp.put(35200, 5000)

	l := NewSetExpLogic(context.Background(), e.svcCtx)
	_, err := l.SetExp(&rpc.AddExpReq{Mid: 35200, Count: -30, Operate: "扣回", Reason: "违规"})
	wantNoErr(t, "负 count", err)
	wantOps(t, "负 count", e.ops(0), []string{
		"cache.GetJSON:bs_35200",
		"base.FindOne:35200",
		"cache.SetJSON:bs_35200/3600",
		"cache.GetInt:exp_35200",
		"exp.FindOne:35200",
		"cache.SetInt:exp_35200/86400",
		"exp.Set:35200/-3000", // 负值原样下传：model 层没做任何钳制
		"memberLog.Add:11/35200/1",
		"cache.Del:exp_35200",
	})
	row := e.st.logs.row(0)
	c := decodeLogContent(t, "负 count", row)
	wantEQ(t, "负 count", "日志 to_exp 记的是截断后的负数", c["to_exp"], "-30")
}

// TestSetExpLogRowIsFullAudit 一次成功变更必须留下一行完整的经验日志：
// log_type=11、mid、log_id 是 uuid、ts 落在调用窗口、ip 走列、content 键集合精确。
func TestSetExpLogRowIsFullAudit(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(memberBase(35300, 10000))
	e.st.exp.put(35300, 123400) // 旧值 1234 分

	before := time.Now().Unix() - 1
	l := NewSetExpLogic(context.Background(), e.svcCtx)
	_, err := l.SetExp(&rpc.AddExpReq{Mid: 35300, Count: 5678, Operate: "手工调整", Reason: "活动补发", Ip: "203.0.113.11"})
	wantNoErr(t, "经验日志", err)
	after := time.Now().Unix() + 1

	wantEQ(t, "经验日志", "一次变更恰好一行", e.st.logs.count(), 1)
	row := e.st.logs.row(0)
	wantEQ(t, "经验日志", "log_type（11 经验变更）", int(row.LogType), int(model.LogTypeExp))
	wantEQ(t, "经验日志", "mid", row.Mid, int64(35300))
	wantUUIDLike(t, "经验日志", "log_id", row.LogID)
	wantTSWindow(t, "经验日志", "ts", row.TS, before, after)
	wantEQ(t, "经验日志", "ip 落列", row.IP, "203.0.113.11")
	wantEQ(t, "经验日志", "status（日志状态恒为有效，Add 里硬编码）", int(row.Status), int(model.LogStatusActive))

	c := decodeLogContent(t, "经验日志", row)
	wantKeysExact(t, "经验日志", "content 键集合（operater 是既有拼写，改名即断链）",
		c, []string{"from_exp", "reason", "to_exp", "operater"})
	wantEQ(t, "经验日志", "from_exp 是换算回分的旧值", c["from_exp"], "1234")
	wantEQ(t, "经验日志", "to_exp 是新值", c["to_exp"], "5678")
	wantEQ(t, "经验日志", "operater", c["operater"], "手工调整")
	wantEQ(t, "经验日志", "reason", c["reason"], "活动补发")

	// 整条链路既不在事务里，也不发事件。
	wantEQ(t, "经验日志", "事务次数（SetExp 全程无事务）", e.st.conn.transactions, 0)
	wantEQ(t, "经验日志", "Outbox 行数（经验变更不通知下游）", e.st.outbox.count(), 0)
}

// TestSetExpLogFailureIsSwallowed 行为哨兵：addExpLog 的 error 被 logx.Errorf 吞掉。
// 结果 = 经验已经改了、审计日志一行没有、接口仍返回成功 → 事后无法对账。
func TestSetExpLogFailureIsSwallowed(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(memberBase(35400, 10000))
	e.st.exp.put(35400, 0)
	boom := errors.New("insert into member_log: dead lock")
	e.st.logs.failWith("Add", boom)

	l := NewSetExpLogic(context.Background(), e.svcCtx)
	reply, err := l.SetExp(&rpc.AddExpReq{Mid: 35400, Count: 100, Operate: "补发", Reason: "r"})
	if err != nil {
		t.Fatalf("日志写失败却被传出：%v（与本结论「被吞」不符，请同步更新用例与 README）", err)
	}
	if reply == nil {
		t.Fatal("日志写失败：成功时 reply 必须非 nil")
	}
	wantOps(t, "日志写失败被吞", e.ops(0), []string{
		"cache.GetJSON:bs_35400",
		"base.FindOne:35400",
		"cache.SetJSON:bs_35400/3600",
		"cache.GetInt:exp_35400",
		"exp.FindOne:35400",
		"cache.SetInt:exp_35400/86400",
		"exp.Set:35400/10000",
		"memberLog.Add:11/35400/1",
		"cache.Del:exp_35400",
	})
	wantEQ(t, "日志写失败被吞", "经验已经改了", e.st.exp.value(35400), int64(10000))
	wantEQ(t, "日志写失败被吞", "member_log 行数（审计丢了）", e.st.logs.count(), 0)
}

// TestSetExpCacheSequenceAndInvalidationScope 失效范围：只删 exp_<mid>，
// bs_ / moral_ 一个都不许动；同时钉住「先 SetInt 旧值、最后才 Del」这个尴尬顺序。
func TestSetExpCacheSequenceAndInvalidationScope(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(memberBase(35500, 10000))
	e.st.exp.put(35500, 4200)
	e.st.cache.warmJSON(keyMoral(35500), model.UserMoral{Mid: 35500, Moral: model.DefaultMoral})

	l := NewSetExpLogic(context.Background(), e.svcCtx)
	_, err := l.SetExp(&rpc.AddExpReq{Mid: 35500, Count: 900, Operate: "o", Reason: "r"})
	wantNoErr(t, "失效范围", err)

	wantEQ(t, "失效范围", "cache.Del 次数（只有 exp_ 这一个 key）", e.st.log.countPrefix("cache.Del:"), 1)
	wantOps(t, "失效范围", e.ops(0), []string{
		"cache.GetJSON:bs_35500",
		"base.FindOne:35500",
		"cache.SetJSON:bs_35500/3600",
		"cache.GetInt:exp_35500",
		"exp.FindOne:35500",
		"cache.SetInt:exp_35500/86400", // 旧值 4200 先被塞进缓存
		"exp.Set:35500/90000",
		"memberLog.Add:11/35500/1",
		"cache.Del:exp_35500", // 才被删掉
	})
	if _, ok := e.st.cache.jsons[keyMoral(35500)]; !ok {
		t.Error("失效范围：moral_35500 被顺手删了")
	}
	if _, ok := e.st.cache.jsons[keyBase(35500)]; !ok {
		t.Error("失效范围：SetExp 顺手把 bs_35500 删了（BaseInfo 的回填被抹掉了）")
	}
	if _, ok := e.st.cache.strs[keyExp(35500)]; ok {
		t.Error("失效范围：exp_35500 还在，失效没生效")
	}

	// 写后读：经验缓存 miss → 回源拿到新值 → 按 86400 回填。
	e.st.log.reset()
	el := NewExpLogic(context.Background(), e.svcCtx)
	got, err := el.Exp(&rpc.MidReq{Mid: 35500})
	wantNoErr(t, "SetExp 后读经验", err)
	wantOps(t, "SetExp 后读经验", e.ops(0), []string{
		"cache.GetInt:exp_35500",
		"exp.FindOne:35500",
		"cache.SetInt:exp_35500/86400",
	})
	// 90000 = 900 分：等级 2（本级起点 200 分），当前经验 900 分，下一级 1500 分。
	wantEQ(t, "SetExp 后读经验", "cur 等级", int64(got.GetCur()), int64(2))
	wantEQ(t, "SetExp 后读经验", "min 本级起点（分）", int64(got.GetMin()), int64(200))
	wantEQ(t, "SetExp 后读经验", "now_exp", int64(got.GetNowExp()), int64(900))
	wantEQ(t, "SetExp 后读经验", "next_exp", int64(got.GetNextExp()), int64(1500))
}

func TestSetExpDownstreamFailures(t *testing.T) {
	t.Run("会员校验读库失败：错误传出、经验一动没动", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(memberBase(35600, 10000))
		e.st.exp.put(35600, 700)
		boom := errors.New("dial tcp 10.0.0.7:3306: connect: connection refused")
		e.st.base.failWith("FindOne", boom)

		l := NewSetExpLogic(context.Background(), e.svcCtx)
		reply, err := l.SetExp(&rpc.AddExpReq{Mid: 35600, Count: 100})
		wantErrIs(t, "校验读库失败", err, boom)
		if reply != nil {
			t.Errorf("校验读库失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "校验读库失败", e.ops(0), []string{"cache.GetJSON:bs_35600", "base.FindOne:35600"})
		wantEQ(t, "校验读库失败", "经验值未动", e.st.exp.value(35600), int64(700))
		wantEQ(t, "校验读库失败", "日志行数", e.st.logs.count(), 0)
	})

	t.Run("读旧经验失败：不写新值、不留日志、不失效", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(memberBase(35601, 10000))
		boom := errors.New("select exp from user_exp: boom")
		e.st.exp.failWith("FindOne", boom)

		l := NewSetExpLogic(context.Background(), e.svcCtx)
		_, err := l.SetExp(&rpc.AddExpReq{Mid: 35601, Count: 100})
		wantErrIs(t, "读旧经验失败", err, boom)
		wantOps(t, "读旧经验失败", e.ops(0), []string{
			"cache.GetJSON:bs_35601",
			"base.FindOne:35601",
			"cache.SetJSON:bs_35601/3600",
			"cache.GetInt:exp_35601",
			"exp.FindOne:35601",
		})
		wantNoOpsWith(t, "读旧经验失败", e.ops(0), "exp.Set")
		wantNoOpsWith(t, "读旧经验失败", e.ops(0), "cache.Del")
		wantEQ(t, "读旧经验失败", "member_log 行数（没有半截日志）", e.st.logs.count(), 0)
	})

	t.Run("目标值写失败：不留日志、不失效（缓存里是刚塞进去的旧值）", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(memberBase(35602, 10000))
		e.st.exp.put(35602, 800)
		boom := errors.New("Error 1264: Out of range value for column 'exp'")
		e.st.exp.failWith("Set", boom)

		l := NewSetExpLogic(context.Background(), e.svcCtx)
		_, err := l.SetExp(&rpc.AddExpReq{Mid: 35602, Count: 100})
		wantErrIs(t, "目标值写失败", err, boom)
		wantOps(t, "目标值写失败", e.ops(0), []string{
			"cache.GetJSON:bs_35602",
			"base.FindOne:35602",
			"cache.SetJSON:bs_35602/3600",
			"cache.GetInt:exp_35602",
			"exp.FindOne:35602",
			"cache.SetInt:exp_35602/86400",
			"exp.Set:35602/10000",
		})
		wantEQ(t, "目标值写失败", "经验值未动", e.st.exp.value(35602), int64(800))
		wantEQ(t, "目标值写失败", "member_log 行数（无半截日志）", e.st.logs.count(), 0)
		// 这条失败路径的额外代价：exp_ 里留着本次写入前的旧值 + 24h TTL，
		// 且因为提前 return，delExpCache 根本没执行——脏读窗口是满的 86400s。
		v, ok := e.st.cache.intOf(keyExp(35602))
		if !ok {
			t.Fatal("目标值写失败：exp_ 缓存不见了（与本结论不符）")
		}
		wantEQ(t, "目标值写失败", "缓存里是旧经验", v, int64(800))
	})

	t.Run("失效失败：错误如实传出（与 setBaseTx 家族相反），但库已改、缓存仍脏", func(t *testing.T) {
		// 三条写侧链路的失效失败口径**互不相同**，这是本次测试钉出来的事实：
		//   - setBaseTx（SetRank/SetSex/SetName/SetSign/SetBirthday/SetFace）：吞掉，返回成功；
		//   - UpdateMoral / BatchUpdateMoral：吞掉，返回成功；
		//   - SetExp / UpdateExp：`return r.delExpCache(...)` —— 原样传出。
		// SetExp 传出错误本身是对的（失败不该假装成功），但它**没有事务**：
		// 于是调用方看到一个错误、按可重试去重试，而经验值其实早就落库了。
		e := newEnv(t)
		e.st.base.put(memberBase(35603, 10000))
		e.st.exp.put(35603, 900)
		boom := errors.New("del exp_35603: redis down")
		e.st.cache.failWith("Del", boom)

		l := NewSetExpLogic(context.Background(), e.svcCtx)
		reply, err := l.SetExp(&rpc.AddExpReq{Mid: 35603, Count: 100, Operate: "补发", Reason: "活动"})
		wantErrIs(t, "失效失败", err, boom)
		if reply != nil {
			t.Errorf("失效失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "失效失败", e.ops(0), []string{
			"cache.GetJSON:bs_35603",
			"base.FindOne:35603",
			"cache.SetJSON:bs_35603/3600",
			"cache.GetInt:exp_35603",
			"exp.FindOne:35603",
			"cache.SetInt:exp_35603/86400",
			"exp.Set:35603/10000",
			"memberLog.Add:11/35603/1",
			"cache.Del:exp_35603",
		})
		wantEQ(t, "失效失败", "库里已经改成新值（错误发生在写之后，回不去）", e.st.exp.value(35603), int64(10000))
		wantEQ(t, "失效失败", "审计日志已经留下一行", e.st.logs.count(), 1)
		v, ok := e.st.cache.intOf(keyExp(35603))
		if !ok {
			t.Fatal("失效失败：exp_ 缓存不见了（说明 Del 的错误被替身当成成功执行）")
		}
		wantEQ(t, "失效失败", "缓存仍是旧经验 900", v, int64(900))
		wantEQ(t, "失效失败", "脏值 TTL", e.st.cache.ttls[keyExp(35603)], 86400)

		// 重放后果（半成功 + 无幂等键）：第二次调用读到的是自己刚塞进去的旧值，
		// 于是日志里的 from_exp 变成上一次的 to_exp，审计链出现「900→10000、10000→10000」的假账。
		e.st.log.reset()
		_, err = l.SetExp(&rpc.AddExpReq{Mid: 35603, Count: 100, Operate: "补发", Reason: "活动"})
		wantErrIs(t, "失效失败后重放", err, boom)
		wantOps(t, "失效失败后重放", e.ops(0), []string{
			"cache.GetJSON:bs_35603", // 命中回填的 bs_，不再回源
			"cache.GetInt:exp_35603", // 命中脏值 900，不回源
			"exp.Set:35603/10000",
			"memberLog.Add:11/35603/2",
			"cache.Del:exp_35603",
		})
		wantEQ(t, "失效失败后重放", "重放又留下一行日志（member_log 无幂等约束）", e.st.logs.count(), 2)
		first := decodeLogContent(t, "重放第 1 条日志", e.st.logs.row(0))
		second := decodeLogContent(t, "重放第 2 条日志", e.st.logs.row(1))
		wantEQ(t, "失效失败后重放", "第 1 条 from_exp", first["from_exp"], "9")
		wantEQ(t, "失效失败后重放", "第 1 条 to_exp", first["to_exp"], "100")
		wantEQ(t, "失效失败后重放", "第 2 条 from_exp（读的是脏缓存，仍是 9）", second["from_exp"], "9")
	})
}
