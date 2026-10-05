package logic

// baselogic_test.go 覆盖 Base 与 Bases（单个/批量基础资料）。
// 判定链全在 repository.BaseInfo / BatchBaseInfo，因此用例一律从 logic 入口进入，
// 用替身轨迹 + 缓存实际内容作断言。
//
// 这里钉住的是本域最贵的四条口径：
//  1. 「查无此人」是**结论**不是错误：mid 有效但库里没有时返回 mid=0 的哨兵结构，
//     并且把这个哨兵**写回缓存**（防击穿）；只有下游真的故障才允许报错。
//  2. 缓存 miss 必须回源 + 回填 TTL=3600；回填内容要能被下一轮命中读出（读写同源）。
//  3. 批量接口部分命中口径：只有 miss 的那几个 mid 进 FindMany，命中与回源结果按 mid
//     合成一张表，库里没有的 mid 补哨兵而非跳过（否则上游按下标取会缺项）。
//  4. 超过 100 个 mid 必须在触缓存之前就被拒。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// baseCachePayload 复刻 repository 未导出的 baseCachePayload / baseCacheValue。
// JSON tag 必须与生产逐字对齐：对不齐时 cached=true 读不出来，命中用例会因为走了
// 回源分支而直接变红（这是刻意的耦合，不是重复定义）。
type baseCachePayload struct {
	Cached bool `json:"cached"`
	baseCacheValue
}

type baseCacheValue struct {
	Mid      int64  `json:"mid"`
	Name     string `json:"name"`
	Sex      int64  `json:"sex"`
	Face     string `json:"face"`
	Sign     string `json:"sign"`
	Rank     int64  `json:"rank"`
	Birthday int64  `json:"birthday"`
}

func TestBaseGuardRejectsBeforeTouchingAnything(t *testing.T) {
	for _, tc := range []struct {
		label string
		mid   int64
	}{
		{"mid=0", 0},
		{"mid 负数", -1},
		{"mid 最小负值", -9223372036854775808},
	} {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			l := NewBaseLogic(context.Background(), e.svcCtx)
			reply, err := l.Base(&rpc.MemberMidReq{Mid: tc.mid, RemoteIp: "10.0.0.1"})
			wantErrIs(t, tc.label, err, repository.ErrRequestErr)
			if reply != nil {
				t.Errorf("%s：拒绝后仍返回了 %+v", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, 0)
		})
	}
}

func TestBaseCacheHitSkipsDB(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmJSON(keyBase(20001), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
		Mid: 20001, Name: "缓存里的名字", Sex: 2, Face: "https://cdn.example.com/cached.png",
		Sign: "缓存签名", Rank: 777777, Birthday: 946684800,
	}})
	// 库存一个**不同**的值：一旦实现走回源，逐字段断言立刻暴露。
	e.st.base.put(&model.UserBase{Mid: 20001, Name: "库里的名字", Sex: 1, Face: "https://db.example.com/db.png",
		Sign: "库签名", Rank: 1, Birthday: 1})

	l := NewBaseLogic(context.Background(), e.svcCtx)
	reply, err := l.Base(&rpc.MemberMidReq{Mid: 20001})
	wantNoErr(t, "缓存命中", err)
	wantEQ(t, "缓存命中", "mid", reply.GetMid(), int64(20001))
	wantEQ(t, "缓存命中", "name 必须来自缓存", reply.GetName(), "缓存里的名字")
	wantEQ(t, "缓存命中", "sex", reply.GetSex(), int64(2))
	wantEQ(t, "缓存命中", "face", reply.GetFace(), "https://cdn.example.com/cached.png")
	wantEQ(t, "缓存命中", "sign", reply.GetSign(), "缓存签名")
	wantEQ(t, "缓存命中", "rank", reply.GetRank(), int64(777777))
	wantEQ(t, "缓存命中", "birthday", reply.GetBirthday(), int64(946684800))
	wantOps(t, "缓存命中", e.ops(0), []string{"cache.GetJSON:bs_20001"})
}

func TestBaseCacheMissReadsThroughAndBackfills(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 20002, Name: "星野_Aya", Sex: 2,
		Face: "https://i.example.com/face/aya.png", Sign: "日更一只", Rank: 12345, Birthday: 946684800})

	l := NewBaseLogic(context.Background(), e.svcCtx)
	reply, err := l.Base(&rpc.MemberMidReq{Mid: 20002})
	wantNoErr(t, "回源", err)
	wantOps(t, "回源", e.ops(0), []string{
		"cache.GetJSON:bs_20002",
		"base.FindOne:20002",
		"cache.SetJSON:bs_20002/3600",
	})
	wantEQ(t, "回源", "mid", reply.GetMid(), int64(20002))
	wantEQ(t, "回源", "name", reply.GetName(), "星野_Aya")
	wantEQ(t, "回源", "sex", reply.GetSex(), int64(2))
	wantEQ(t, "回源", "face", reply.GetFace(), "https://i.example.com/face/aya.png")
	wantEQ(t, "回源", "sign", reply.GetSign(), "日更一只")
	wantEQ(t, "回源", "rank", reply.GetRank(), int64(12345))
	wantEQ(t, "回源", "birthday", reply.GetBirthday(), int64(946684800))

	var cached baseCachePayload
	if !e.st.cache.jsonOf(keyBase(20002), &cached) {
		t.Fatal("回源后没有写入缓存")
	}
	wantEQ(t, "回源", "回填 cached", cached.Cached, true)
	wantEQ(t, "回源", "回填 name", cached.Name, "星野_Aya")
	wantEQ(t, "回源", "回填 sex", cached.Sex, int64(2))
	wantEQ(t, "回源", "回填 face", cached.Face, "https://i.example.com/face/aya.png")
	wantEQ(t, "回源", "回填 sign", cached.Sign, "日更一只")
	wantEQ(t, "回源", "回填 rank", cached.Rank, int64(12345))
	wantEQ(t, "回源", "回填 birthday", cached.Birthday, int64(946684800))

	// 回填必须真的可被命中读出：第二次调用只许碰缓存。
	before := e.st.log.snapshot()
	_, err = l.Base(&rpc.MemberMidReq{Mid: 20002})
	wantNoErr(t, "二次读取", err)
	wantOps(t, "二次读取", e.ops(before), []string{"cache.GetJSON:bs_20002"})
}

func TestBaseMissingUserReturnsSentinelAndCachesIt(t *testing.T) {
	e := newEnv(t) // 库里没有 20003

	l := NewBaseLogic(context.Background(), e.svcCtx)
	reply, err := l.Base(&rpc.MemberMidReq{Mid: 20003})
	wantNoErr(t, "查无此人", err)
	wantEQ(t, "查无此人", "mid 哨兵", reply.GetMid(), int64(0))
	wantEQ(t, "查无此人", "name", reply.GetName(), "")
	wantEQ(t, "查无此人", "sex", reply.GetSex(), int64(0))
	wantEQ(t, "查无此人", "face", reply.GetFace(), "")
	wantEQ(t, "查无此人", "sign", reply.GetSign(), "")
	wantEQ(t, "查无此人", "rank", reply.GetRank(), int64(0))
	wantEQ(t, "查无此人", "birthday", reply.GetBirthday(), int64(0))
	wantOps(t, "查无此人", e.ops(0), []string{
		"cache.GetJSON:bs_20003",
		"base.FindOne:20003",
		"cache.SetJSON:bs_20003/3600",
	})
	var cached baseCachePayload
	if !e.st.cache.jsonOf(keyBase(20003), &cached) {
		t.Fatal("防击穿哨兵没有写入缓存")
	}
	wantEQ(t, "查无此人", "哨兵 cached=true", cached.Cached, true)
	wantEQ(t, "查无此人", "哨兵 mid", cached.Mid, int64(0))
}

func TestBaseCacheFaultDegradesToDBWithoutError(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 20004, Name: "降级也要给我", Sex: 1, Rank: 5000, Birthday: -28800})
	boom := errors.New("redis down")
	e.st.cache.failWith("GetJSON", boom)

	l := NewBaseLogic(context.Background(), e.svcCtx)
	reply, err := l.Base(&rpc.MemberMidReq{Mid: 20004})
	wantNoErr(t, "缓存故障降级", err)
	wantEQ(t, "缓存故障降级", "name", reply.GetName(), "降级也要给我")
	wantOps(t, "缓存故障降级", e.ops(0), []string{
		"cache.GetJSON:bs_20004",
		"base.FindOne:20004",
	})
	// 缓存已被判定为不可用，不得再尝试回填（cacheOK=false）。
	if n := e.st.log.countPrefix("cache.Set"); n != 0 {
		t.Errorf("缓存故障降级：回填次数 = %d, want 0", n)
	}
}

func TestBaseDBFailurePropagatesAndCachesNothing(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("dial mysql: refused")
	e.st.base.failWith("FindOne", boom)

	l := NewBaseLogic(context.Background(), e.svcCtx)
	reply, err := l.Base(&rpc.MemberMidReq{Mid: 20005})
	wantErrIs(t, "库故障", err, boom)
	if reply != nil {
		t.Errorf("库故障：不应返回半成品 reply，got %+v", reply)
	}
	wantOps(t, "库故障", e.ops(0), []string{"cache.GetJSON:bs_20005", "base.FindOne:20005"})
}

func TestBasesGuards(t *testing.T) {
	t.Run("超过 100 个 mid 直接拒绝", func(t *testing.T) {
		e := newEnv(t)
		mids := make([]int64, 101)
		for i := range mids {
			mids[i] = int64(i + 1)
		}
		l := NewBasesLogic(context.Background(), e.svcCtx)
		reply, err := l.Bases(&rpc.MemberMidsReq{Mids: mids})
		wantErrIs(t, "101 个 mid", err, repository.ErrMemberOverLimit)
		if reply != nil {
			t.Errorf("101 个 mid：拒绝后仍返回 %+v", reply)
		}
		wantNoCall(t, "101 个 mid", e.st, 0)
	})

	t.Run("刚好 100 个放行", func(t *testing.T) {
		e := newEnv(t)
		mids := make([]int64, 100)
		for i := range mids {
			mids[i] = int64(i + 1)
			e.st.cache.warmJSON(keyBase(mids[i]), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: mids[i], Name: "全员命中"}})
		}
		l := NewBasesLogic(context.Background(), e.svcCtx)
		reply, err := l.Bases(&rpc.MemberMidsReq{Mids: mids})
		wantNoErr(t, "100 个 mid", err)
		wantEQ(t, "100 个 mid", "条数", len(reply.GetBaseInfos()), 100)
		wantEQ(t, "100 个 mid", "mid=100 的名字", reply.GetBaseInfos()[100].GetName(), "全员命中")
		if n := e.st.log.countPrefix("base.FindMany"); n != 0 {
			t.Errorf("100 个 mid：全员命中时仍回源 %d 次", n)
		}
		wantEQ(t, "100 个 mid", "缓存读次数", e.st.log.countPrefix("cache.GetJSON"), 100)
	})

	t.Run("空列表返回空 map 且零调用", func(t *testing.T) {
		for _, tc := range []struct {
			label string
			mids  []int64
		}{
			{"nil", nil},
			{"空切片", []int64{}},
		} {
			e := newEnv(t)
			l := NewBasesLogic(context.Background(), e.svcCtx)
			reply, err := l.Bases(&rpc.MemberMidsReq{Mids: tc.mids})
			wantNoErr(t, tc.label, err)
			if reply.GetBaseInfos() == nil {
				t.Errorf("%s：BaseInfos 为 nil，上游 range 之外按下标取会 panic 风险", tc.label)
			}
			wantEQ(t, tc.label, "条数", len(reply.GetBaseInfos()), 0)
			wantNoCall(t, tc.label, e.st, 0)
		}
	})
}

func TestBasesPartialHitOnlyBackfillsMisses(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmJSON(keyBase(30001), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
		Mid: 30001, Name: "命中项", Sex: 2, Rank: 111, Birthday: 111,
	}})
	e.st.base.put(&model.UserBase{Mid: 30002, Name: "回源项", Sex: 1, Face: "https://i.example.com/2.png",
		Sign: "签名2", Rank: 222, Birthday: 222})
	// 30003 缓存与库里都没有 → 必须补哨兵而不是跳过。

	l := NewBasesLogic(context.Background(), e.svcCtx)
	reply, err := l.Bases(&rpc.MemberMidsReq{Mids: []int64{30001, 30002, 30003}})
	wantNoErr(t, "部分命中", err)
	wantOps(t, "部分命中", e.ops(0), []string{
		"cache.GetJSON:bs_30001",
		"cache.GetJSON:bs_30002",
		"cache.GetJSON:bs_30003",
		"base.FindMany:30002,30003",
		"cache.SetJSON:bs_30002/3600",
		"cache.SetJSON:bs_30003/3600",
	})
	wantInt64EQ(t, "部分命中", "FindMany 只拿到 miss", e.st.base.lastMany, []int64{30002, 30003})

	bases := reply.GetBaseInfos()
	wantEQ(t, "部分命中", "条数", len(bases), 3)
	wantEQ(t, "部分命中", "30001 名字", bases[30001].GetName(), "命中项")
	wantEQ(t, "部分命中", "30001 rank", bases[30001].GetRank(), int64(111))
	wantEQ(t, "部分命中", "30002 名字", bases[30002].GetName(), "回源项")
	wantEQ(t, "部分命中", "30002 face", bases[30002].GetFace(), "https://i.example.com/2.png")
	wantEQ(t, "部分命中", "30002 sign", bases[30002].GetSign(), "签名2")
	wantEQ(t, "部分命中", "30002 sex", bases[30002].GetSex(), int64(1))
	wantEQ(t, "部分命中", "30002 birthday", bases[30002].GetBirthday(), int64(222))
	missing, ok := bases[30003]
	if !ok {
		t.Fatal("库里没有的 mid 被跳过了：上游按下标取会缺项")
	}
	wantEQ(t, "部分命中", "30003 哨兵 mid", missing.GetMid(), int64(0))
	wantEQ(t, "部分命中", "30003 哨兵 name", missing.GetName(), "")

	var cached baseCachePayload
	if !e.st.cache.jsonOf(keyBase(30002), &cached) {
		t.Fatal("回源项没有回填缓存")
	}
	wantEQ(t, "部分命中", "回填 30002 name", cached.Name, "回源项")
}

func TestBasesDropsPartialResultOnDBFailure(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmJSON(keyBase(30101), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 30101, Name: "已命中"}})
	boom := errors.New("select from user_base: dead lock")
	e.st.base.failWith("FindMany", boom)

	l := NewBasesLogic(context.Background(), e.svcCtx)
	reply, err := l.Bases(&rpc.MemberMidsReq{Mids: []int64{30101, 30102}})
	wantErrIs(t, "批量回源失败", err, boom)
	// 生产口径：BatchBaseInfo 会把**已经命中的那部分**连同错误一起返回，
	// 但 Bases 只透传错误、丢弃 map（见 README 已知缺口）。
	if reply != nil {
		t.Errorf("批量回源失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "批量回源失败", e.ops(0), []string{
		"cache.GetJSON:bs_30101",
		"cache.GetJSON:bs_30102",
		"base.FindMany:30102",
	})
}

func TestBasesKeepsDuplicateMidsAsOneEntry(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 30201, Name: "重复项", Sex: 1, Rank: 5000, Birthday: -28800})

	l := NewBasesLogic(context.Background(), e.svcCtx)
	reply, err := l.Bases(&rpc.MemberMidsReq{Mids: []int64{30201, 30201, 30201}})
	wantNoErr(t, "重复 mid", err)
	wantEQ(t, "重复 mid", "结果条数（map 天然去重）", len(reply.GetBaseInfos()), 1)
	wantEQ(t, "重复 mid", "name", reply.GetBaseInfos()[30201].GetName(), "重复项")
	// 请求侧不去重：三次缓存读、FindMany 收到三个相同 mid、三次回填都如实发生。
	wantOps(t, "重复 mid", e.ops(0), []string{
		"cache.GetJSON:bs_30201",
		"cache.GetJSON:bs_30201",
		"cache.GetJSON:bs_30201",
		"base.FindMany:30201,30201,30201",
		"cache.SetJSON:bs_30201/3600",
		"cache.SetJSON:bs_30201/3600",
		"cache.SetJSON:bs_30201/3600",
	})
}

func TestBasesDoesNotInventMidZeroGuard(t *testing.T) {
	// BatchBaseInfo 对 mid 取值零校验：0 与负数会照原样拼 key 进 SQL。
	// 本用例钉住现状；若将来加了校验，这里必须同步改（而不是悄悄放宽）。
	e := newEnv(t)
	l := NewBasesLogic(context.Background(), e.svcCtx)
	reply, err := l.Bases(&rpc.MemberMidsReq{Mids: []int64{0, -5}})
	wantNoErr(t, "非法 mid 列表", err)
	wantOps(t, "非法 mid 列表", e.ops(0), []string{
		"cache.GetJSON:bs_0",
		"cache.GetJSON:bs_-5",
		"base.FindMany:0,-5",
		"cache.SetJSON:bs_0/3600",
		"cache.SetJSON:bs_-5/3600",
	})
	wantEQ(t, "非法 mid 列表", "条数", len(reply.GetBaseInfos()), 2)
}
