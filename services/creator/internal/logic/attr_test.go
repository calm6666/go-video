package logic

// attr_test.go 覆盖 UpAttr（UP 身份属性，缓存→DB→回填，含负缓存）。
//
// 钉住的结论：
//   - from 的合法域是 0..3（与开关域的 0/1 不通用），越界一律 errInvalidFrom，且拒绝前零依赖调用；
//   - 「有行但 is_author=0」和「根本没行」应答都是 0，但**缓存留下的形态不同**（`"0"` vs `"{}"`），
//     前者是一次真实查询的结果，后者是防击穿空标记；两者都不回库直到 TTL 到期；
//   - 脏缓存值不被当成「默认无身份」：Atoi 的错误原样上抛，一次都不回库；
//   - 回填/写空标记失败被忽略（`_ =`），但绝不能留下半成品值；
//   - mid 全程 int64：BIGINT UNSIGNED 的大 mid 不得被截断（与已修的开关截断同一类）。

import (
	"context"
	"strconv"
	"testing"

	"go-video/services/creator/rpc"
)

const (
	attrMid = int64(21)
	atKey0  = "up:attr:21:0" // fakeKeyAttr(attrMid, 0)
)

func TestUpAttrGuardsRejectBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name    string
		mid     int64
		from    int32
		wantErr error
	}{
		{name: "mid=0", mid: 0, from: 0, wantErr: errInvalidMid},
		{name: "mid 负数", mid: -2, from: 1, wantErr: errInvalidMid},
		{name: "from 负数", mid: attrMid, from: -1, wantErr: errInvalidFrom},
		{name: "from=4 越界", mid: attrMid, from: 4, wantErr: errInvalidFrom},
		{name: "from=99 越界", mid: attrMid, from: 99, wantErr: errInvalidFrom},
		{name: "非法 mid 叠加非法 from", mid: 0, from: 7, wantErr: errInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedAttr(attrMid, 0, 1) // 库里有一行真身份：非法入参也不能被它带过去
			before := st.log.snapshot()

			got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
				UpAttr(&rpc.UpAttrReq{Mid: tc.mid, From: tc.from})
			wantErrIs(t, t.Name(), err, tc.wantErr)
			if got != nil {
				t.Errorf("应答 = %#v, want nil", got)
			}
			wantNoCallAfter(t, t.Name(), st.log, before)
		})
	}
}

// TestUpAttrAcceptsFullFromDomain 与开关域对照：from=2/3 在 up_attr 里合法（各自一条独立缓存键），
// 而在 SetUpSwitch/UpSwitch 里必须被拒——两张表的枚举不通用，别把校验抄错。
func TestUpAttrAcceptsFullFromDomain(t *testing.T) {
	st := newStore()
	for from := int32(0); from <= 3; from++ {
		st.seedAttr(attrMid, from, from%2) // 0/1 交替，便于分辨有没有串键
	}
	sc := st.svcCtx()

	for from := int32(0); from <= 3; from++ {
		got, err := NewUpAttrLogic(context.Background(), sc).
			UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: from})
		wantNoErr(t, "from="+strconv.Itoa(int(from)), err)
		wantEQ(t, "from="+strconv.Itoa(int(from))+" 的身份值", "is_author", got.GetIsAuthor(), from%2)
	}
	wantSeq(t, "四个 from 各自一条 key、互不串号", st.log, 0,
		"cache.GetAttr:"+atKey0,
		"up_attr.FindOne:21/0",
		"cache.SetAttr:"+atKey0,
		"cache.GetAttr:up:attr:21:1",
		"up_attr.FindOne:21/1",
		"cache.SetAttr:up:attr:21:1",
		"cache.GetAttr:up:attr:21:2",
		"up_attr.FindOne:21/2",
		"cache.SetAttr:up:attr:21:2",
		"cache.GetAttr:up:attr:21:3",
		"up_attr.FindOne:21/3",
		"cache.SetAttr:up:attr:21:3")
	wantEQ(t, "每行只查一次", "up_attr.FindOne 次数", len(st.attr.findOneCalls), 4)
}

func TestUpAttrMissThenBackfillThenHit(t *testing.T) {
	st := newStore()
	st.seedAttr(attrMid, 0, 1)

	sc := st.svcCtx()
	got, err := NewUpAttrLogic(context.Background(), sc).UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
	wantNoErr(t, "首读", err)
	wantEQ(t, "首读来自 DB", "is_author", got.GetIsAuthor(), int32(1))
	wantSeq(t, "miss→回源→回填", st.log, 0,
		"cache.GetAttr:"+atKey0,
		"up_attr.FindOne:21/0",
		"cache.SetAttr:"+atKey0)
	wantEQ(t, "回填的是裸数字（不是 JSON）", "raw", st.cacheRaw(t, atKey0), "1")
	wantEQ(t, "回填 TTL", "ttl", st.cacheTTL(t, atKey0), fakeTTLAttr)

	before := st.log.snapshot()
	again, err := NewUpAttrLogic(context.Background(), sc).UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
	wantNoErr(t, "二读", err)
	wantEQ(t, "二读来自缓存", "is_author", again.GetIsAuthor(), int32(1))
	wantSeq(t, "二读只碰缓存", st.log, before, "cache.GetAttr:"+atKey0)
}

// TestUpAttrCacheWinsOverDB 说明读侧以缓存为准：库里已经改成 1，缓存里还是 0 时应答 0。
// 本服务没有写 up_attr 的 RPC，所以这条差异只能靠 TTL（1h）收敛（缺陷 D5 的同一类）。
func TestUpAttrCacheWinsOverDB(t *testing.T) {
	st := newStore()
	st.seedAttr(attrMid, 0, 1)
	st.warmAttr(attrMid, 0, "0")

	got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
		UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
	wantNoErr(t, "命中", err)
	wantEQ(t, "缓存值优先", "is_author", got.GetIsAuthor(), int32(0))
	wantSeq(t, "命中不回库", st.log, 0, "cache.GetAttr:"+atKey0)
}

// TestUpAttrZeroRowAndMissingRowLeaveDifferentArtifacts 是本文件最要紧的分辨性用例：
// 两条路径应答都是 0，但库里有没有行、缓存里落什么完全不同。
func TestUpAttrZeroRowAndMissingRowLeaveDifferentArtifacts(t *testing.T) {
	t.Run("有行且 is_author=0", func(t *testing.T) {
		st := newStore()
		st.seedAttr(attrMid, 0, 0)

		got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
			UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
		wantNoErr(t, "零值行", err)
		wantEQ(t, "应答", "is_author", got.GetIsAuthor(), int32(0))
		wantSeq(t, "零值行也回填真实值", st.log, 0,
			"cache.GetAttr:"+atKey0,
			"up_attr.FindOne:21/0",
			"cache.SetAttr:"+atKey0)
		wantEQ(t, "落的是查询结果", "raw", st.cacheRaw(t, atKey0), "0")
		wantMethodCount(t, "走的是 SetAttr 而不是空标记", st.log, "cache.SetAttr", 1)
		wantMethodCount(t, "不写空标记", st.log, "cache.SetAttrEmpty", 0)
	})

	t.Run("根本没行", func(t *testing.T) {
		st := newStore()

		got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
			UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
		wantNoErr(t, "无行", err)
		wantEQ(t, "无行应答 0 且不报错（查无此人 ≠ 故障）", "is_author", got.GetIsAuthor(), int32(0))
		wantSeq(t, "无行写空标记", st.log, 0,
			"cache.GetAttr:"+atKey0,
			"up_attr.FindOne:21/0",
			"cache.SetAttrEmpty:"+atKey0)
		wantEQ(t, "落的是防击穿空标记", "raw", st.cacheRaw(t, atKey0), fakeEmptyMark)
		wantEQ(t, "空标记 TTL", "ttl", st.cacheTTL(t, atKey0), fakeTTLAttr)

		// 负缓存生效：二读不回库。
		before := st.log.snapshot()
		if _, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
			UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0}); err != nil {
			t.Fatalf("二读：%v", err)
		}
		wantSeq(t, "空标记即命中", st.log, before, "cache.GetAttr:"+atKey0)
	})
}

// TestUpAttrNegativeCacheHidesLateRow pin「无行→空标记」的代价：
// 运营面在回填之后插入真实身份行，读侧仍要等满 1h 才看到 is_author=1。
// 钩子必须真的触发（checkRaces），否则这条结论是空转。
func TestUpAttrNegativeCacheHidesLateRow(t *testing.T) {
	st := newStore()
	st.raceBefore("cache.SetAttrEmpty", func() {
		st.seedAttr(attrMid, 0, 1) // 就在要写空标记的这一瞬间，运营面给这个 mid 开了身份
	})

	got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
		UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
	wantNoErr(t, "首读", err)
	wantEQ(t, "pin：本次应答仍是 0", "is_author", got.GetIsAuthor(), int32(0))
	wantEQ(t, "库侧现在真有行（对照）", "up_attr 行数", len(st.attr.rows), 1)
	wantEQ(t, "pin 缺陷：空标记把「查无此人」又钉回缓存", "raw", st.cacheRaw(t, atKey0), fakeEmptyMark)

	again, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
		UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
	wantNoErr(t, "二读", err)
	wantEQ(t, "pin 缺陷：真实身份被空标记挡住", "is_author", again.GetIsAuthor(), int32(0))
	wantMethodCount(t, "二读不回库", st.log, "up_attr.FindOne", 1)
	wantEQ(t, "滞后上限就是 TTL", "ttl", st.cacheTTL(t, atKey0), fakeTTLAttr)
	st.checkRaces(t)
}

func TestUpAttrCorruptCacheValueFailsLoud(t *testing.T) {
	for _, raw := range []string{"yes", "2 3", "[1]", "1.0"} {
		t.Run(raw, func(t *testing.T) {
			st := newStore()
			st.seedAttr(attrMid, 0, 1)
			st.warmAttr(attrMid, 0, raw)

			got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
				UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
			if err == nil {
				t.Fatalf("脏缓存 %q 被当成正常应答：%#v", raw, got)
			}
			if got != nil {
				t.Errorf("应答 = %#v, want nil", got)
			}
			// 关键：脏值不会被伪装成「无身份」，也不静默回库（cache.go:143-146 直接 return err）。
			wantSeq(t, "轨迹止于缓存", st.log, 0, "cache.GetAttr:"+atKey0)
		})
	}
}

func TestUpAttrCacheFailureIsNotDowngraded(t *testing.T) {
	st := newStore()
	st.seedAttr(attrMid, 0, 1)
	st.cache.failWith("GetAttr", errFakeRedis)

	got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
		UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
	wantErrIs(t, "缓存故障原样透传", err, errFakeRedis)
	if got != nil {
		t.Errorf("应答 = %#v, want nil（不能降级成 is_author=0）", got)
	}
	wantSeq(t, "缓存故障时不回库", st.log, 0, "cache.GetAttr:"+atKey0)
}

func TestUpAttrDBFailureWrapped(t *testing.T) {
	st := newStore()
	st.attr.failWith("FindOne", errFakeDB)

	got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
		UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
	wantErrIs(t, "DB 失败透传", err, errFakeDB)
	wantErrContains(t, "错误归属", err, "UpAttr FindOne")
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	// 一次抖动不能被钉成 1h 的「无身份」：既不能写真实值也不能写空标记。
	wantSeq(t, "失败后不得回填", st.log, 0,
		"cache.GetAttr:"+atKey0,
		"up_attr.FindOne:21/0")
	if st.cacheHas(atKey0) {
		t.Errorf("DB 失败却写了缓存")
	}
}

// TestUpAttrBackfillFailuresIgnored 覆盖两个 `_ =` 分支（repository.go:204、207）。
func TestUpAttrBackfillFailuresIgnored(t *testing.T) {
	t.Run("有行→SetAttr 失败", func(t *testing.T) {
		st := newStore()
		st.seedAttr(attrMid, 0, 1)
		st.cache.failWith("SetAttr", errFakeRedis)

		got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
			UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
		wantNoErr(t, "回填失败被忽略", err)
		wantEQ(t, "应答仍是 DB 值", "is_author", got.GetIsAuthor(), int32(1))
		if st.cacheHas(atKey0) {
			t.Errorf("SetAttr 失败了却留下值，会让后续读走半成品")
		}
	})

	t.Run("无行→SetAttrEmpty 失败", func(t *testing.T) {
		st := newStore()
		st.cache.failWith("SetAttrEmpty", errFakeRedis)

		got, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
			UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0})
		wantNoErr(t, "写空标记失败被忽略", err)
		wantEQ(t, "应答仍是「无身份」", "is_author", got.GetIsAuthor(), int32(0))
		if st.cacheHas(atKey0) {
			t.Errorf("SetAttrEmpty 失败了却留下空标记")
		}
		// 没写成缓存＝下一次读还要回库（性能代价，但不会读到错值）。
		before := st.log.snapshot()
		if _, err := NewUpAttrLogic(context.Background(), st.svcCtx()).
			UpAttr(&rpc.UpAttrReq{Mid: attrMid, From: 0}); err != nil {
			t.Fatalf("二读：%v", err)
		}
		wantSeq(t, "二读重新回源", st.log, before,
			"cache.GetAttr:"+atKey0,
			"up_attr.FindOne:21/0",
			"cache.SetAttrEmpty:"+atKey0)
	})
}

// TestUpAttrKeepsFullInt64Mid 是 mid 截断这一类问题在 up_attr 侧的哨兵：
// 读路径必须把 BIGINT UNSIGNED 的大 mid 原样交给 SQL 与缓存键。
func TestUpAttrKeepsFullInt64Mid(t *testing.T) {
	bigMid := int64(5_000_000_000)
	truncated := int64(int32(bigMid)) // 若哪天有人再加一次 int32 转换，就会落到这个号上
	st := newStore()
	st.seedAttr(truncated, 0, 0)
	st.seedAttr(bigMid, 0, 1)
	sc := st.svcCtx()

	got, err := NewUpAttrLogic(context.Background(), sc).
		UpAttr(&rpc.UpAttrReq{Mid: bigMid, From: 0})
	wantNoErr(t, "大 mid", err)
	wantEQ(t, "大 mid 读到自己那一行", "is_author", got.GetIsAuthor(), int32(1))
	wantSeq(t, "SQL 参数与缓存键都是完整 mid", st.log, 0,
		"cache.GetAttr:up:attr:5000000000:0",
		"up_attr.FindOne:5000000000/0",
		"cache.SetAttr:up:attr:5000000000:0")
	wantEQ(t, "无辜的截断号未被牵连", "is_author", st.attr.rows[0].IsAuthor, int32(0))
	if st.cacheHas(fakeKeyAttr(truncated, 0)) {
		t.Errorf("截断号的缓存键被动过")
	}
}
