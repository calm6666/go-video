package logic

// upswitch_test.go 覆盖 SetUpSwitch / UpSwitch 两个方法（关注弹窗开关的读写闭环）。
//
// 钉住的结论：
//   - 写后再读必须读回同一个值，而且这一「读回」必须是**真的回库**（不能只回显入参）；
//   - 开关枚举（from / state）越界一律拒，且拒绝发生在任何依赖调用之前、不留半行数据；
//   - mid 非法（<=0）不得凭空插出一行「看起来合法」的开关；
//   - 缓存方向只有「读回填 + 写失效」：失效必须是 Upsert 之后 DEL，顺序反过来就是脏读；
//   - 缓存/DB 报错原样 errors.Is 透传，绝不伪装成「查无此人」的默认关闭。
//
// 缺陷登记见 fakes_test.go 文件头（本文件复现 D1、D2，并锁住已修的 mid 截断问题）。

import (
	"context"
	"testing"

	"go-video/services/creator/rpc"
)

const (
	switchMid = int64(101)
	swKey0    = "up:sw:101:0"
	swKey1    = "up:sw:101:1"
)

// --- SetUpSwitch ---

func TestSetUpSwitchGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name     string
		mid      int64
		from     int32
		state    int32
		wantErr  error
		wantFrag string
	}{
		{name: "mid=0", mid: 0, from: 0, state: 1, wantErr: errInvalidMid},
		{name: "mid 负数", mid: -7, from: 0, state: 1, wantErr: errInvalidMid},
		{name: "from 负数", mid: switchMid, from: -1, state: 1, wantErr: errInvalidSwitchFrom},
		{name: "from=2 越界", mid: switchMid, from: 2, state: 1, wantErr: errInvalidSwitchFrom},
		// 注意 from=2/3 在 UpAttr 里是合法枚举，在开关域里必须被拒：两张表的枚举不通用。
		{name: "state 负数", mid: switchMid, from: 0, state: -1, wantErr: errInvalidSwitchState},
		{name: "state=2 越界", mid: switchMid, from: 0, state: 2, wantErr: errInvalidSwitchState},
		{name: "非法 mid 叠加非法 from", mid: 0, from: 9, state: 1, wantErr: errInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			before := st.log.snapshot()

			resp, err := NewSetUpSwitchLogic(context.Background(), st.svcCtx()).
				SetUpSwitch(&rpc.UpSwitchReq{Mid: tc.mid, From: tc.from, State: tc.state})

			wantErrIs(t, t.Name(), err, tc.wantErr)
			if resp != nil {
				t.Errorf("%s：拒绝时应答 = %#v, want nil", t.Name(), resp)
			}
			// 守卫拒绝后一次依赖调用都不许发生（既不触库也不触缓存）。
			wantNoCallAfter(t, t.Name(), st.log, before)
			// 爆炸半径：库里一行都不该有（含别的表）。
			if got := st.counts(); got != (storeCounts{}) {
				t.Errorf("%s：守卫拒绝后仍有库存 residue %+v", t.Name(), got)
			}
		})
	}
}

func TestSetUpSwitchThenUpSwitchReadsBackFromDB(t *testing.T) {
	st := newStore()
	l := NewSetUpSwitchLogic(context.Background(), st.svcCtx())

	resp, err := l.SetUpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0, State: 1})
	wantNoErr(t, "SetUpSwitch", err)
	if resp == nil {
		t.Fatalf("SetUpSwitch：应答 = nil, want 非 nil EmptyReply")
	}

	// 顺序就是结论：先落库，再失效缓存（反过来就是「读到旧值又被回填」的脏窗口）。
	wantSeq(t, "写路径", st.log, 0,
		"up_switch.Upsert:101/0/1",
		"cache.DelSwitch:"+swKey0)

	// 库里那一行真的等于请求值（不是靠应答回显）。
	wantEQ(t, "落库后", "up_switch.state", st.switchRow(t, switchMid, 0).State, int32(1))
	if st.cacheHas(swKey0) {
		t.Errorf("写后缓存 key %s 仍在，DelSwitch 没生效", swKey0)
	}

	// 读侧必须回源，读到的值来自 DB 行本身。
	before := st.log.snapshot()
	got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "UpSwitch", err)
	wantEQ(t, "写后读回", "state", got.GetState(), int32(1))
	wantSeq(t, "读路径", st.log, before,
		"cache.GetSwitch:"+swKey0,
		"up_switch.FindOne:101/0",
		"cache.SetSwitch:"+swKey0)
}

// TestSetUpSwitchNonEchoProof 用「库里是 0、请求写 1 后立刻把库改成 0」反证读侧不是回显：
// 读值只可能来自那一行，行变了读值就跟着变。
func TestSetUpSwitchReadValueComesFromRowNotEcho(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 1)
	st.warmSwitch(switchMid, 0, "1") // 先让读侧命中缓存，确认命中路径回的是缓存值

	sc := st.svcCtx()
	got, err := NewUpSwitchLogic(context.Background(), sc).UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "首次读", err)
	wantEQ(t, "缓存命中", "state", got.GetState(), int32(1))

	// 把缓存改成 0（模拟「库里已经改了、缓存还是旧值」以外的另一半事实：库里是 0）。
	if err := st.sw.Upsert(context.Background(), switchMid, 0, 0); err != nil {
		t.Fatalf("直改库存行失败：%v", err)
	}
	st.warmSwitch(switchMid, 0, "0")
	got, err = NewUpSwitchLogic(context.Background(), sc).UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "第二次读", err)
	wantEQ(t, "读值跟随存储", "state", got.GetState(), int32(0))

	// 走一次真实的写→读闭环，确认写进去的值被原样读回。
	if _, err := NewSetUpSwitchLogic(context.Background(), sc).
		SetUpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0, State: 1}); err != nil {
		t.Fatalf("SetUpSwitch：%v", err)
	}
	got, err = NewUpSwitchLogic(context.Background(), sc).UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "写后读", err)
	wantEQ(t, "写后读回", "state", got.GetState(), int32(1))
	wantEQ(t, "写后库值", "up_switch.state", st.switchRow(t, switchMid, 0).State, int32(1))
}

func TestSetUpSwitchOverwritesExistingRowAndKeepsOneRow(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 0)
	before := st.counts()

	if _, err := NewSetUpSwitchLogic(context.Background(), st.svcCtx()).
		SetUpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0, State: 1}); err != nil {
		t.Fatalf("SetUpSwitch：%v", err)
	}

	after := st.counts()
	wantEQ(t, "行数差（Upsert 不得插第二行）", "up_switch 增量", after.sw-before.sw, 0)
	wantEQ(t, "库值", "state", st.switchRow(t, switchMid, 0).State, int32(1))
}

func TestSetUpSwitchIsIsolatedPerFrom(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 1) // 播放器关注开关：已打开
	sc := st.svcCtx()

	if _, err := NewSetUpSwitchLogic(context.Background(), sc).
		SetUpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 1, State: 0}); err != nil {
		t.Fatalf("SetUpSwitch(from=1)：%v", err)
	}

	// 失效只删自己那一条 key（此刻日志里只有这两条，读侧断言放在其后）。
	wantSeq(t, "写路径只碰 from=1 的 key", st.log, 0,
		"up_switch.Upsert:101/1/0",
		"cache.DelSwitch:"+swKey1)

	// 同一 mid 的另一路开关不得被带写（PK 是 (mid, from)）。
	wantEQ(t, "from=0 未被牵连", "state", st.switchRow(t, switchMid, 0).State, int32(1))
	wantEQ(t, "from=1 已写入", "state", st.switchRow(t, switchMid, 1).State, int32(0))
	got, err := NewUpSwitchLogic(context.Background(), sc).UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "UpSwitch(from=0)", err)
	wantEQ(t, "读回 from=0", "state", got.GetState(), int32(1))
}

func TestSetUpSwitchIsIdempotentOnRepeat(t *testing.T) {
	st := newStore()
	sc := st.svcCtx()
	l := NewSetUpSwitchLogic(context.Background(), sc)
	in := &rpc.UpSwitchReq{Mid: switchMid, From: 0, State: 1}

	first, err := l.SetUpSwitch(in)
	wantNoErr(t, "首次提交", err)
	before := st.log.snapshot()
	second, err := l.SetUpSwitch(in)
	wantNoErr(t, "重复提交", err)
	if second == nil {
		t.Fatalf("重复提交应答 = nil, want 与首次同形的 EmptyReply")
	}
	wantEQ(t, "两次应答同为空消息", "第二次应答", second.String(), first.String())

	// 重复提交的依赖轨迹与首次完全一致（没有「第二次改成 no-op」的分支），
	// 且库里仍只有一行、值不变——幂等是「结果一致」，不是「少发 SQL」。
	wantSeq(t, "重复提交", st.log, before,
		"up_switch.Upsert:101/0/1",
		"cache.DelSwitch:"+swKey0)
	wantEQ(t, "行数", "up_switch 行数", len(st.sw.rows), 1)
	wantEQ(t, "库值", "state", st.switchRow(t, switchMid, 0).State, int32(1))
}

// TestSetUpSwitchKeepsFullInt64Mid 是已修缺陷的回归锁（fakes_test.go 文件头）：
// 旧代码把 int64 mid 截成 int32，mid > 2^31-1 时写进另一个号的行、失效另一个号的 key，
// 而读侧用的是完整 mid → 用户改完开关读回旧值，还顺手改了别人的开关。
func TestSetUpSwitchKeepsFullInt64Mid(t *testing.T) {
	// 取一个 > 2^32 的号：截断后仍是正数（5e9-2^32=705032704），
	// 这样「无辜的另一行」才可能真的存在，缺陷后果才可观察。
	bigMid := int64(5_000_000_000)
	truncated := int64(int32(bigMid)) // 旧代码的作为；这里用变量而不是常量，才不会被编译期挡下
	if truncated >= bigMid || truncated <= 0 {
		t.Fatalf("用例前提破了：截断值 %d 应为一个不同正数", truncated)
	}
	st := newStore()
	st.seedSwitch(truncated, 0, 0) // 无辜的另一行（截断后的号恰好有主）
	sc := st.svcCtx()

	if _, err := NewSetUpSwitchLogic(context.Background(), sc).
		SetUpSwitch(&rpc.UpSwitchReq{Mid: bigMid, From: 0, State: 1}); err != nil {
		t.Fatalf("SetUpSwitch：%v", err)
	}

	wantSeq(t, "写路径", st.log, 0,
		"up_switch.Upsert:5000000000/0/1",
		"cache.DelSwitch:up:sw:5000000000:0")
	wantEQ(t, "大 mid 已落自己的行", "state", st.switchRow(t, bigMid, 0).State, int32(1))
	wantEQ(t, "截断号那一行不得被牵连", "state", st.switchRow(t, truncated, 0).State, int32(0))

	got, err := NewUpSwitchLogic(context.Background(), sc).UpSwitch(&rpc.UpSwitchReq{Mid: bigMid, From: 0})
	wantNoErr(t, "UpSwitch", err)
	wantEQ(t, "写后读回", "state", got.GetState(), int32(1))
}

func TestSetUpSwitchDBFailurePropagatesAndSkipsInvalidation(t *testing.T) {
	st := newStore()
	st.sw.failWith("Upsert", errFakeDB)
	before := st.log.snapshot()

	resp, err := NewSetUpSwitchLogic(context.Background(), st.svcCtx()).
		SetUpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0, State: 1})

	wantErrIs(t, "DB 失败", err, errFakeDB)
	wantErrContains(t, "错误归属", err, "SetUpSwitch Upsert")
	if resp != nil {
		t.Errorf("应答 = %#v, want nil", resp)
	}
	// 写失败绝不能失效缓存：否则「缓存已空 + 库未改」会让下一次读回到旧值并被重新缓存，
	// 更糟的是掩盖真实故障。
	wantSeq(t, "DB 失败后", st.log, before, "up_switch.Upsert:101/0/1")
	wantEQ(t, "残留行数", "up_switch 行数", len(st.sw.rows), 0)
}

// TestSetUpSwitchCacheInvalidateFailureStillWritesDB pin 缺陷 D2：
// Upsert 已成功、DelSwitch 失败时整体回错。行为是「调用方看到失败，库里已是新值」，
// 客户端重试同一请求即可自愈（重复 Upsert 不改变结果），所以不改成吞错误。
func TestSetUpSwitchCacheInvalidateFailureStillWritesDB(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 0)
	// 旧值由**生产读路径**回填进来（而不是布景），这样 TTL 是代码真选的 86400。
	if _, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0}); err != nil {
		t.Fatalf("预热读：%v", err)
	}
	st.cache.failWith("DelSwitch", errFakeRedis)
	afterWarm := st.log.snapshot()

	_, err := NewSetUpSwitchLogic(context.Background(), st.svcCtx()).
		SetUpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0, State: 1})
	wantErrIs(t, "失效失败仍回错", err, errFakeRedis)

	wantSeq(t, "轨迹", st.log, afterWarm,
		"up_switch.Upsert:101/0/1",
		"cache.DelSwitch:"+swKey0)
	wantEQ(t, "DB 已经写成新值", "state", st.switchRow(t, switchMid, 0).State, int32(1))
	wantEQ(t, "缓存仍是旧值（D2 的后果）", "raw", st.cacheRaw(t, swKey0), "0")
	// 于是读侧被旧缓存挡住，最长 24h（TTL 见 repository/cache.go:28）。
	got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "降级读", err)
	wantEQ(t, "pin 当前行为：读到旧值", "state", got.GetState(), int32(0))
	wantMethodCount(t, "旧值命中就不回库", st.log, "up_switch.FindOne", 1)
	wantEQ(t, "脏值存活时长上限", "ttl", st.cacheTTL(t, swKey0), fakeTTLSwitch)
}

// --- UpSwitch ---

func TestUpSwitchGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name    string
		mid     int64
		from    int32
		wantErr error
	}{
		{name: "mid=0", mid: 0, from: 0, wantErr: errInvalidMid},
		{name: "mid 负数", mid: -1, from: 1, wantErr: errInvalidMid},
		{name: "from 负数", mid: switchMid, from: -1, wantErr: errInvalidSwitchFrom},
		{name: "from=2 越界", mid: switchMid, from: 2, wantErr: errInvalidSwitchFrom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedSwitch(switchMid, 0, 1)
			before := st.log.snapshot()

			resp, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
				UpSwitch(&rpc.UpSwitchReq{Mid: tc.mid, From: tc.from, State: 7}) // state 只写侧用，读侧必须忽略
			wantErrIs(t, t.Name(), err, tc.wantErr)
			if resp != nil {
				t.Errorf("应答 = %#v, want nil", resp)
			}
			wantNoCallAfter(t, t.Name(), st.log, before)
		})
	}
}

func TestUpSwitchCacheHitSkipsDB(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 1)
	st.warmSwitch(switchMid, 0, "0") // 缓存说「关」，库说「开」：读侧以缓存为准（一致性由写路径失效保证）

	got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "UpSwitch", err)
	wantEQ(t, "缓存值优先", "state", got.GetState(), int32(0))
	wantSeq(t, "命中路径", st.log, 0, "cache.GetSwitch:"+swKey0)
	wantMethodCount(t, "命中不得回库", st.log, "up_switch.FindOne", 0)
}

// TestUpSwitchCorruptCacheValueFailsLoud pin：缓存里的脏值不会被当成「默认关闭」，
// 也不会静默降级回库——错误原样上抛（AGENTS.md §5「不得把故障伪装成查无此人」）。
func TestUpSwitchCorruptCacheValueFailsLoud(t *testing.T) {
	for _, raw := range []string{"on", "2 3", "[1]"} {
		t.Run(raw, func(t *testing.T) {
			st := newStore()
			st.seedSwitch(switchMid, 0, 1)
			st.warmSwitch(switchMid, 0, raw)

			got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
				UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
			if err == nil {
				t.Fatalf("脏缓存 %q 被当成正常应答：%#v", raw, got)
			}
			wantMethodCount(t, "脏值不回库", st.log, "up_switch.FindOne", 0)
			wantSeq(t, "轨迹", st.log, 0, "cache.GetSwitch:"+swKey0)
		})
	}
}

func TestUpSwitchCacheFailureIsNotDowngraded(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 1)
	st.cache.failWith("GetSwitch", errFakeRedis)

	got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantErrIs(t, "缓存故障原样透传", err, errFakeRedis)
	if got != nil {
		t.Errorf("应答 = %#v, want nil（不能降级成 state=0）", got)
	}
	// repository.go:215-218 直接 return 0, err：一次都不回库。
	wantMethodCount(t, "缓存故障时不回库", st.log, "up_switch.FindOne", 0)
}

// TestUpSwitchMissingRowIsClosedAndNotCached pin repository.go:226-229 的注释事实：
// 「不存在视为默认关闭，不缓存」。所以每次读都回库——没有负缓存，也就不会把
// 「后来才被写开的开关」钉死成关闭。
func TestUpSwitchMissingRowIsClosedAndNotCached(t *testing.T) {
	st := newStore()
	sc := st.svcCtx()

	for i := 1; i <= 2; i++ {
		got, err := NewUpSwitchLogic(context.Background(), sc).
			UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
		wantNoErr(t, "UpSwitch", err)
		wantEQ(t, "无行默认关闭", "state", got.GetState(), int32(0))
	}
	wantSeq(t, "两次都回库", st.log, 0,
		"cache.GetSwitch:"+swKey0,
		"up_switch.FindOne:101/0",
		"cache.GetSwitch:"+swKey0,
		"up_switch.FindOne:101/0")
	if st.cacheHas(swKey0) {
		t.Errorf("无行路径写了缓存，与 repository.go:226-229 的口径冲突")
	}
}

func TestUpSwitchBackfillsAndThenServesFromCache(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 1)
	sc := st.svcCtx()

	got, err := NewUpSwitchLogic(context.Background(), sc).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "首读", err)
	wantEQ(t, "首读来自 DB", "state", got.GetState(), int32(1))
	wantEQ(t, "回填的原始值", "raw", st.cacheRaw(t, swKey0), "1")
	wantEQ(t, "回填 TTL", "ttl", st.cacheTTL(t, swKey0), fakeTTLSwitch)

	before := st.log.snapshot()
	got, err = NewUpSwitchLogic(context.Background(), sc).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "二读", err)
	wantEQ(t, "二读来自缓存", "state", got.GetState(), int32(1))
	wantSeq(t, "二读序列", st.log, before, "cache.GetSwitch:"+swKey0)
}

func TestUpSwitchDBFailureWrapped(t *testing.T) {
	st := newStore()
	st.sw.failWith("FindOne", errFakeDB)

	got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantErrIs(t, "DB 失败透传", err, errFakeDB)
	wantErrContains(t, "错误归属", err, "UpSwitch FindOne")
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	wantSeq(t, "失败后不得回填", st.log, 0,
		"cache.GetSwitch:"+swKey0,
		"up_switch.FindOne:101/0")
	if st.cacheHas(swKey0) {
		t.Errorf("DB 失败却写了缓存，会把故障结果钉成 24h")
	}
}

func TestUpSwitchBackfillFailureStillAnswersDBValue(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 1)
	st.cache.failWith("SetSwitch", errFakeRedis)

	got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "回填失败被忽略（repository.go:230 是 `_ =`）", err)
	wantEQ(t, "应答仍是 DB 值", "state", got.GetState(), int32(1))
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetSwitch:"+swKey0,
		"up_switch.FindOne:101/0",
		"cache.SetSwitch:"+swKey0)
	if st.cacheHas(swKey0) {
		t.Errorf("SetSwitch 失败了却留下缓存值，会让后续读走脏值")
	}
}

// TestUpSwitchStaleBackfillRacePinsOldValue 复现缺陷 D1：
// 读侧「FindOne 拿到旧值」与「SetSwitch 回填」之间被一次完整的写插队，
// 回填就把已被覆盖的旧值重新钉进缓存（TTL 24h），此后读侧一直返回旧值。
// 钩子没触发即判失败（st.checkRaces），所以这条轨迹是真的交错过。
func TestUpSwitchStaleBackfillRacePinsOldValue(t *testing.T) {
	st := newStore()
	st.seedSwitch(switchMid, 0, 0) // 库里先是「关闭」
	st.raceBefore("cache.SetSwitch", func() {
		// 就在要回填的这一瞬间，用户自己把开关打开了（走的就是生产写路径）。
		st.mustSetUpSwitch(t, switchMid, 0, 1)
	})

	got, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "被插队的读", err)
	wantEQ(t, "pin：本次读到插队前的旧值（可接受）", "state", got.GetState(), int32(0))

	// 交错后的事实：库里是 1，缓存却被回填成 0。
	wantSeq(t, "交错轨迹", st.log, 0,
		"cache.GetSwitch:"+swKey0,
		"up_switch.FindOne:101/0",
		"up_switch.Upsert:101/0/1",
		"cache.DelSwitch:"+swKey0,
		"cache.SetSwitch:"+swKey0)
	wantEQ(t, "库值", "up_switch.state", st.switchRow(t, switchMid, 0).State, int32(1))
	wantEQ(t, "pin 缺陷：回填把旧值又写回缓存", "raw", st.cacheRaw(t, swKey0), "0")

	// 后果：下一次读命中脏缓存，用户改了开关却读不回来，且最长持续 24h。
	again, err := NewUpSwitchLogic(context.Background(), st.svcCtx()).
		UpSwitch(&rpc.UpSwitchReq{Mid: switchMid, From: 0})
	wantNoErr(t, "第二次读", err)
	wantEQ(t, "pin 缺陷：读到旧值", "state", again.GetState(), int32(0))
	wantMethodCount(t, "第二次读不回库（脏缓存挡在中间）", st.log, "up_switch.FindOne", 1)
	wantEQ(t, "脏值存活时长上限", "ttl", st.cacheTTL(t, swKey0), fakeTTLSwitch)
	st.checkRaces(t)
}
