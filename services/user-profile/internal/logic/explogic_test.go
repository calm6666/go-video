package logic

// explogic_test.go 覆盖 Exp 与 Level（经验值 → 等级换算的两条读路径）。
// 换算全在 model.BuildLevel，取数全在 repository.exp，因此本文件同时钉住两件事：
//  1. Exp 与 Level 的**唯一**差别是 now_exp 是否回填（sexp 标志）；等级、下界、下一级
//     阈值必须一致。把 sexp 传错是这类接口最典型的越权/漏字段故障。
//  2. 阈值边界必须逐个钉死：经验入库单位是「分 × 100」（ExpMulti），
//     199 分仍是 1 级、200 分才升 2 级；满级（28800 分）时 next_exp 必须是 -1；
//     负经验（缓存被写坏时会出现）按 Go 的向零截断落级，不得 panic、不得变成满级。
//
// 另外钉住读侧不变量：缓存命中一律不回源；miss 必须回填 TTL=86400；
// 经验表没有这行时按 0 处理（0 是合法值，不是「不存在」，因此不做防击穿哨兵）。

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

func TestExpCacheHitSkipsDB(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmInt(keyExp(40001), 20000) // 200 分 → 2 级
	e.st.exp.put(40001, 99999999)            // 库里的值差得远：一旦走回源，等级就变

	l := NewExpLogic(context.Background(), e.svcCtx)
	reply, err := l.Exp(&rpc.MidReq{Mid: 40001})
	wantNoErr(t, "经验缓存命中", err)
	wantEQ(t, "经验缓存命中", "cur", reply.GetCur(), int32(2))
	wantEQ(t, "经验缓存命中", "min", reply.GetMin(), int32(200))
	wantEQ(t, "经验缓存命中", "now_exp", reply.GetNowExp(), int32(200))
	wantEQ(t, "经验缓存命中", "next_exp", reply.GetNextExp(), int32(1500))
	wantOps(t, "经验缓存命中", e.ops(0), []string{"cache.GetInt:exp_40001"})
}

func TestExpCacheMissReadsThroughAndBackfills(t *testing.T) {
	e := newEnv(t)
	e.st.exp.put(40002, 450000) // 4500 分 → 4 级下界

	l := NewExpLogic(context.Background(), e.svcCtx)
	reply, err := l.Exp(&rpc.MidReq{Mid: 40002})
	wantNoErr(t, "经验回源", err)
	wantOps(t, "经验回源", e.ops(0), []string{
		"cache.GetInt:exp_40002",
		"exp.FindOne:40002",
		"cache.SetInt:exp_40002/86400",
	})
	wantEQ(t, "经验回源", "cur", reply.GetCur(), int32(4))
	wantEQ(t, "经验回源", "min", reply.GetMin(), int32(4500))
	wantEQ(t, "经验回源", "now_exp", reply.GetNowExp(), int32(4500))
	wantEQ(t, "经验回源", "next_exp", reply.GetNextExp(), int32(10800))
	got, ok := e.st.cache.intOf(keyExp(40002))
	wantEQ(t, "经验回源", "回填命中", ok, true)
	wantEQ(t, "经验回源", "回填值", got, int64(450000))
}

func TestExpWithoutRowIsZeroLevelNotMissing(t *testing.T) {
	e := newEnv(t) // user_exp 无此 mid，缓存也无

	l := NewExpLogic(context.Background(), e.svcCtx)
	reply, err := l.Exp(&rpc.MidReq{Mid: 40003})
	wantNoErr(t, "无经验记录", err)
	wantOps(t, "无经验记录", e.ops(0), []string{
		"cache.GetInt:exp_40003",
		"exp.FindOne:40003",
		"cache.SetInt:exp_40003/86400",
	})
	wantEQ(t, "无经验记录", "cur", reply.GetCur(), int32(0))
	wantEQ(t, "无经验记录", "min", reply.GetMin(), int32(0))
	wantEQ(t, "无经验记录", "now_exp", reply.GetNowExp(), int32(0))
	wantEQ(t, "无经验记录", "next_exp", reply.GetNextExp(), int32(1))
	// 0 是合法经验值，回填后必须能命中（本服务对经验不做防击穿哨兵）。
	got, ok := e.st.cache.intOf(keyExp(40003))
	wantEQ(t, "无经验记录", "回填了 0", ok, true)
	wantEQ(t, "无经验记录", "回填值", got, int64(0))
}

func TestExpDBFailurePropagatesAndCachesNothing(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("select from user_exp: connection reset")
	e.st.exp.failWith("FindOne", boom)

	l := NewExpLogic(context.Background(), e.svcCtx)
	reply, err := l.Exp(&rpc.MidReq{Mid: 40004})
	wantErrIs(t, "经验库故障", err, boom)
	if reply != nil {
		t.Errorf("经验库故障：reply = %+v, want nil", reply)
	}
	wantOps(t, "经验库故障", e.ops(0), []string{"cache.GetInt:exp_40004", "exp.FindOne:40004"})
}

// === Exp / Level 换算边界（钉死 BuildLevel 的每个阈值与 sexp 差别） ===

func TestExpAndLevelLevelBoundaries(t *testing.T) {
	cases := []struct {
		stored              int64
		cur, min, now, next int32
	}{
		{0, 0, 0, 0, 1},                    // 0 分
		{99, 0, 0, 0, 1},                   // 99 分：不足 1 分，仍 0 级
		{100, 1, 1, 1, 200},                // 1 分：升 1 级下界
		{19999, 1, 1, 199, 200},            // 199 分：差 1 分升 2 级
		{20000, 2, 200, 200, 1500},         // 200 分：升 2 级
		{150000, 3, 1500, 1500, 4500},      // 1500 分：升 3 级
		{450000, 4, 4500, 4500, 10800},     // 4500 分：升 4 级
		{1080000, 5, 10800, 10800, 28800},  /* 10800 分：升 5 级 */
		{2880000, 6, 28800, 28800, -1},     // 28800 分：满级，next = -1
		{300000000, 6, 28800, 3000000, -1}, // 300 万分：满级后 now_exp 继续涨
		{-99, 0, 0, 0, 1},                  // 向零截断：不足 1 分
		{-150, 0, 0, -1, 1},                // 负经验（缓存被写坏）：落 0 级、now=-1
		{-1000000, 0, 0, -10000, 1},        // -10000 分：不得变成满级
	}
	for _, tc := range cases {
		tc := tc
		label := strconv.FormatInt(tc.stored, 10) + " 入库经验"
		t.Run("Exp/"+label, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmInt(keyExp(40010), tc.stored)
			l := NewExpLogic(context.Background(), e.svcCtx)
			reply, err := l.Exp(&rpc.MidReq{Mid: 40010})
			wantNoErr(t, label, err)
			wantEQ(t, label, "cur", reply.GetCur(), tc.cur)
			wantEQ(t, label, "min", reply.GetMin(), tc.min)
			wantEQ(t, label, "now_exp", reply.GetNowExp(), tc.now)
			wantEQ(t, label, "next_exp", reply.GetNextExp(), tc.next)
			wantOps(t, label, e.ops(0), []string{"cache.GetInt:exp_40010"})
		})
		t.Run("Level/"+label, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmInt(keyExp(40010), tc.stored)
			l := NewLevelLogic(context.Background(), e.svcCtx)
			reply, err := l.Level(&rpc.MidReq{Mid: 40010})
			wantNoErr(t, label, err)
			wantEQ(t, label, "cur", reply.GetCur(), tc.cur)
			wantEQ(t, label, "min", reply.GetMin(), tc.min)
			// Level 的 sexp=false：now_exp 必须恒为 0，哪怕经验读到了。
			wantEQ(t, label, "now_exp 必须被抹平", reply.GetNowExp(), int32(0))
			wantEQ(t, label, "next_exp", reply.GetNextExp(), tc.next)
			wantOps(t, label, e.ops(0), []string{"cache.GetInt:exp_40010"})
		})
	}
}

func TestLevelReadsThroughAndBackfillsLikeExp(t *testing.T) {
	e := newEnv(t)
	e.st.exp.put(40020, 1080000) // 10800 分 → 5 级

	l := NewLevelLogic(context.Background(), e.svcCtx)
	reply, err := l.Level(&rpc.MidReq{Mid: 40020})
	wantNoErr(t, "等级回源", err)
	wantOps(t, "等级回源", e.ops(0), []string{
		"cache.GetInt:exp_40020",
		"exp.FindOne:40020",
		"cache.SetInt:exp_40020/86400",
	})
	wantEQ(t, "等级回源", "cur", reply.GetCur(), int32(5))
	wantEQ(t, "等级回源", "min", reply.GetMin(), int32(10800))
	wantEQ(t, "等级回源", "now_exp", reply.GetNowExp(), int32(0))
	wantEQ(t, "等级回源", "next_exp", reply.GetNextExp(), int32(28800))
}

func TestLevelDBFailurePropagates(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("user_exp 表不存在")
	e.st.exp.failWith("FindOne", boom)

	l := NewLevelLogic(context.Background(), e.svcCtx)
	reply, err := l.Level(&rpc.MidReq{Mid: 40021})
	wantErrIs(t, "等级库故障", err, boom)
	if reply != nil {
		t.Errorf("等级库故障：reply = %+v, want nil", reply)
	}
	wantOps(t, "等级库故障", e.ops(0), []string{"cache.GetInt:exp_40021", "exp.FindOne:40021"})
}

// Exp / Level 与 Base 不同：mid<=0 没有守卫，会一路打到 SQL。
// 钉住现状（见 README 已知缺口），将来加守卫必须同步改这里。
func TestExpAndLevelDoNotGuardNonPositiveMid(t *testing.T) {
	for _, mid := range []int64{0, -7} {
		label := "mid=" + strconv.FormatInt(mid, 10)

		e := newEnv(t)
		el := NewExpLogic(context.Background(), e.svcCtx)
		reply, err := el.Exp(&rpc.MidReq{Mid: mid})
		wantNoErr(t, "Exp "+label, err)
		wantEQ(t, "Exp "+label, "cur", reply.GetCur(), int32(0))
		wantOps(t, "Exp "+label, e.ops(0), []string{
			"cache.GetInt:exp_" + strconv.FormatInt(mid, 10),
			"exp.FindOne:" + strconv.FormatInt(mid, 10),
			"cache.SetInt:exp_" + strconv.FormatInt(mid, 10) + "/86400",
		})

		e2 := newEnv(t)
		ll := NewLevelLogic(context.Background(), e2.svcCtx)
		if _, err = ll.Level(&rpc.MidReq{Mid: mid}); err != nil {
			t.Errorf("Level %s：%v", label, err)
		}
		wantOps(t, "Level "+label, e2.ops(0), []string{
			"cache.GetInt:exp_" + strconv.FormatInt(mid, 10),
			"exp.FindOne:" + strconv.FormatInt(mid, 10),
			"cache.SetInt:exp_" + strconv.FormatInt(mid, 10) + "/86400",
		})
	}
}

// 经验缓存与基础资料缓存互不串门：key 前缀不同，写坏一个不得影响另一个。
func TestExpCacheKeyIsIsolatedFromBase(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 40030, Name: "只看基础资料", Rank: 5000, Birthday: -28800})
	e.st.exp.put(40030, 20000)

	el := NewExpLogic(context.Background(), e.svcCtx)
	if _, err := el.Exp(&rpc.MidReq{Mid: 40030}); err != nil {
		t.Fatalf("Exp：%v", err)
	}
	wantOps(t, "经验与基础资料隔离", e.ops(0), []string{
		"cache.GetInt:exp_40030",
		"exp.FindOne:40030",
		"cache.SetInt:exp_40030/86400",
	})
	if n := e.st.log.countPrefix("cache.GetJSON"); n != 0 {
		t.Errorf("经验与基础资料隔离：读到了 JSON 缓存 %d 次", n)
	}
	if v := e.st.cache.ttls[keyExp(40030)]; v != 86400 {
		t.Errorf("经验 TTL = %d, want 86400", v)
	}
}
