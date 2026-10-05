package logic

// setranklogic_test.go 覆盖 SetRank（设置排名/会员位）。
//
// 本方法是这批里**权限语义最重**的一个写入口：user_base.rank 在本服务里不只是排序位，
// repository.checkExpMember 用 `rank >= 10000` 当「是否会员」的唯一判据，
// 而 SetRank 对调用方身份、rank 值域、rank>=10000 这个语义跳变**一律不设约束**。
// 所以这里除了共用不变量（wantSetBaseWritten），重点钉两条：
//  1. 值域：DDL 是 `rank` BIGINT UNSIGNED DEFAULT 5000，但 logic/repository 层零校验，
//     负数、0、极大值都原样透传到 model（真实 MySQL 对负数会报 1264，替身按 int64 存，
//     故这里断言的是「本层不校验、参数原样下传」而不是「库能存下负数」）；
//  2. 跨方法后果：一次 SetRank 就能把普通账号升成会员，进而让 SetExp 从
//     ErrUserNoMember 变成成功——这是「运营位写入口没有调用者身份约束」的可执行证据。

import (
	"context"
	"errors"
	"math"
	"testing"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/model"
	"go-video/services/user-profile/rpc"
)

// TestRankGuardTableIsPinnedAsNoGuard 钉「SetRank 无任何前置守卫」。
//
// TODO(缺陷) 应拒未拒：
//   - mid <= 0：给不存在的账号凭空补一行 user_base；
//   - rank < 0：DDL UNSIGNED，本层不校验，只能等数据库报错；
//   - rank = 0：SetBase 里有「0 → DefaultRank」的归一，SetRank 没有，口径不一致；
//   - 无调用者身份约束：任何能打到这个 RPC 的人都能改会员位（见
//     TestRankUpgradeMakesUserEligibleForSetExp）；
//   - remote_ip 契约里有但完全不用，升会员不留任何来源审计。
func TestRankGuardTableIsPinnedAsNoGuard(t *testing.T) {
	cases := []struct {
		label string
		mid   int64
		rank  int64
	}{
		{"mid=0 照样写", 0, 1},
		{"mid 负数照样写", -9, 1},
		{"rank=0 不归一（与 SetBase 的 0→5000 口径不一致）", 32001, 0},
		{"rank 负数原样透传", 32002, -1},
		{"rank=9999 恰好是会员阈值的下方", 32003, 9999},
		{"rank=10000 恰好是会员阈值", 32004, 10000},
		{"int64 上限不裁剪", 32005, math.MaxInt64},
		{"int64 下限不裁剪", 32006, math.MinInt64},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			l := NewSetRankLogic(context.Background(), e.svcCtx)
			reply, err := l.SetRank(&rpc.UpdateRankReq{Mid: tc.mid, Rank: tc.rank, RemoteIp: "10.0.0.2"})
			wantNoErr(t, tc.label, err)
			if reply == nil {
				t.Fatalf("%s：成功时 reply 必须非 nil", tc.label)
			}
			wantSetBaseWritten(t, tc.label, e, tc.mid, "SetRank", repository.ActUpdatePersonInfo, 1)
			row := e.st.base.get(tc.mid)
			if row == nil {
				t.Fatalf("%s：user_base 没有落库", tc.label)
			}
			wantEQ(t, tc.label, "rank 原样入库（本层零校验）", row.Rank, tc.rank)
		})
	}
}

// TestRankUpgradeMakesUserEligibleForSetExp 是「运营位写入口无身份约束」的行为哨兵：
// 同一个 mid，SetRank 之前 SetExp 必失败（ErrUserNoMember），SetRank 之后立刻成功。
// rank 的 10000 这条线是本服务里唯一区分「普通用户 / 会员」的东西，
// 而它能被任意调用方一次 RPC 改写，等价于把会员资格发给了自己。
func TestRankUpgradeMakesUserEligibleForSetExp(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 32100, Name: "普通用户", Rank: model.DefaultRank})
	e.st.exp.put(32100, 0)

	// 升位前：rank=5000 < 10000，SetExp 必须被拒。
	sl := NewSetExpLogic(context.Background(), e.svcCtx)
	_, err := sl.SetExp(&rpc.AddExpReq{Mid: 32100, Count: 100})
	if !errors.Is(err, repository.ErrUserNoMember) {
		t.Fatalf("升位前 SetExp err = %v, want ErrUserNoMember", err)
	}
	wantEQ(t, "升位前", "经验值未被改动", e.st.exp.value(32100), int64(0))

	// 一次 SetRank 就把闸门打开。
	e.st.log.reset()
	rl := NewSetRankLogic(context.Background(), e.svcCtx)
	_, err = rl.SetRank(&rpc.UpdateRankReq{Mid: 32100, Rank: 99999})
	wantNoErr(t, "自行升会员位", err)
	wantSetBaseWritten(t, "自行升会员位", e, 32100, "SetRank", repository.ActUpdatePersonInfo, 1)

	e.st.log.reset()
	_, err = sl.SetExp(&rpc.AddExpReq{Mid: 32100, Count: 100, Operate: "运营补发", Reason: "无"})
	wantNoErr(t, "升位后 SetExp（同一个人、同一次调用序列）", err)
	wantEQ(t, "升位后", "经验值按 ×100 落库", e.st.exp.value(32100), int64(10000))
}

func TestRankOnlyTouchesTheRankColumn(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{
		Mid: 32200, Name: "小张", Sex: 1, Face: "https://cdn.example.com/b.png",
		Sign: "签名", Rank: 5000, Birthday: 946684800,
	})
	e.st.cache.warmJSON(keyBase(32200), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{
		Mid: 32200, Rank: 5000,
	}})

	l := NewSetRankLogic(context.Background(), e.svcCtx)
	_, err := l.SetRank(&rpc.UpdateRankReq{Mid: 32200, Rank: 777, RemoteIp: "203.0.113.8"})
	wantNoErr(t, "改 rank", err)
	wantSetBaseWritten(t, "改 rank", e, 32200, "SetRank", repository.ActUpdatePersonInfo, 1)

	row := e.st.base.get(32200)
	wantEQ(t, "改 rank", "rank", row.Rank, int64(777))
	wantEQ(t, "改 rank", "name 不许被顺手改", row.Name, "小张")
	wantEQ(t, "改 rank", "sex 不许被顺手改", row.Sex, int64(1))
	wantEQ(t, "改 rank", "face 不许被顺手改", row.Face, "https://cdn.example.com/b.png")
	wantEQ(t, "改 rank", "sign 不许被顺手改", row.Sign, "签名")
	wantEQ(t, "改 rank", "birthday 不许被顺手改", row.Birthday, int64(946684800))

	// 失效范围：rank 变了只删 bs_<mid>，会员/经验/节操缓存不属于本方法的失效集合。
	e.st.log.reset()
	bl := NewBaseLogic(context.Background(), e.svcCtx)
	base, err := bl.Base(&rpc.MemberMidReq{Mid: 32200})
	wantNoErr(t, "改 rank 后读回", err)
	wantOps(t, "改 rank 后读回", e.ops(0), []string{
		"cache.GetJSON:bs_32200",
		"base.FindOne:32200",
		"cache.SetJSON:bs_32200/3600",
	})
	wantEQ(t, "改 rank 后读回", "rank", base.GetRank(), int64(777))
}

func TestRankCreatesMissingRowWithDefaults(t *testing.T) {
	e := newEnv(t)
	l := NewSetRankLogic(context.Background(), e.svcCtx)
	_, err := l.SetRank(&rpc.UpdateRankReq{Mid: 32300, Rank: 10000})
	wantNoErr(t, "给不存在的 mid 设 rank", err)
	wantSetBaseWritten(t, "给不存在的 mid 设 rank", e, 32300, "SetRank", repository.ActUpdatePersonInfo, 1)
	row := e.st.base.get(32300)
	if row == nil {
		t.Fatal("给不存在的 mid 设 rank：没有补建行")
	}
	wantEQ(t, "补建行", "rank", row.Rank, int64(10000))
	wantEQ(t, "补建行", "name 空", row.Name, "")
	wantEQ(t, "补建行", "birthday 取 DDL 默认 -28800", row.Birthday, int64(model.DefaultTime))

	// 后果：一个 account 侧不存在的 mid，靠 SetRank 就能拿到「会员」身份。
	e.st.log.reset()
	if err := e.st.repo.SetExp(context.Background(), 32300, 1, "运营", "无", ""); err != nil {
		t.Errorf("补建行后的 SetExp err = %v, want nil（rank=10000 已过会员闸门）", err)
	}
}

func TestRankDownstreamFailures(t *testing.T) {
	t.Run("单列写失败：不发事件、不失效缓存", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 32400, Rank: 5000})
		e.st.cache.warmJSON(keyBase(32400), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 32400, Rank: 5000}})
		boom := errors.New("Error 1264: Out of range value for column 'rank'")
		e.st.base.failWith("SetRank", boom)

		l := NewSetRankLogic(context.Background(), e.svcCtx)
		reply, err := l.SetRank(&rpc.UpdateRankReq{Mid: 32400, Rank: -1})
		wantErrIs(t, "单列写失败", err, boom)
		if reply != nil {
			t.Errorf("单列写失败：reply = %+v, want nil", reply)
		}
		wantOps(t, "单列写失败", e.ops(0), []string{"base.SetRank:32400"})
		wantEQ(t, "单列写失败", "事务次数", e.st.conn.transactions, 1)
		wantEQ(t, "单列写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "单列写失败", "事件行数", e.st.outbox.count(), 0)
		if _, ok := e.st.cache.jsons[keyBase(32400)]; !ok {
			t.Error("单列写失败：缓存被顺手删了（失败不该扩大影响面）")
		}
	})

	t.Run("Outbox 写失败：错误传出且不失效缓存", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 32401, Rank: 5000})
		boom := errors.New("insert into member_outbox: boom")
		e.st.outbox.failWith("Insert", boom)

		l := NewSetRankLogic(context.Background(), e.svcCtx)
		_, err := l.SetRank(&rpc.UpdateRankReq{Mid: 32401, Rank: 10000})
		wantErrIs(t, "Outbox 写失败", err, boom)
		wantOps(t, "Outbox 写失败", e.ops(0), []string{
			"base.SetRank:32401",
			"outbox.Insert:user.profile.updated/32401/1",
		})
		wantEQ(t, "Outbox 写失败", "事务回滚", e.st.conn.rolledBack, 1)
		wantEQ(t, "Outbox 写失败", "事件行数（同事务回滚不留行）", e.st.outbox.count(), 0)
	})

	t.Run("缓存失效失败被吞：接口成功但脏缓存继续吐旧 rank", func(t *testing.T) {
		e := newEnv(t)
		e.st.base.put(&model.UserBase{Mid: 32402, Rank: 5000})
		e.st.cache.warmJSON(keyBase(32402), baseCachePayload{Cached: true, baseCacheValue: baseCacheValue{Mid: 32402, Rank: 5000}})
		e.st.cache.failWith("Del", errors.New("del bs_32402: redis down"))

		l := NewSetRankLogic(context.Background(), e.svcCtx)
		reply, err := l.SetRank(&rpc.UpdateRankReq{Mid: 32402, Rank: 10000})
		wantNoErr(t, "失效失败", err)
		if reply == nil {
			t.Fatal("失效失败：成功时 reply 必须非 nil")
		}
		wantOps(t, "失效失败", e.ops(0), []string{
			"base.SetRank:32402",
			"outbox.Insert:user.profile.updated/32402/1",
			"cache.Del:bs_32402",
		})
		wantEQ(t, "失效失败", "库里已是新值", e.st.base.get(32402).Rank, int64(10000))
		var stale baseCachePayload
		if !e.st.cache.jsonOf(keyBase(32402), &stale) {
			t.Fatal("失效失败：缓存竟然没了（说明 Del 的错误被替身当成成功执行）")
		}
		wantEQ(t, "失效失败", "缓存仍是旧 rank=5000（会员位脏读窗口 = TTL 3600s）", stale.Rank, int64(5000))

		e.st.log.reset()
		bl := NewBaseLogic(context.Background(), e.svcCtx)
		base, err := bl.Base(&rpc.MemberMidReq{Mid: 32402})
		wantNoErr(t, "失效失败后读回", err)
		wantOps(t, "失效失败后读回", e.ops(0), []string{"cache.GetJSON:bs_32402"})
		wantEQ(t, "失效失败后读回", "脏读持续性：还在吐未升位前的 rank", base.GetRank(), int64(5000))
	})
}

// TestRankCarriesPersonInfoActionAndNoAuditFields 断言 rank 变更走 updatePersonInfo，
// 且 payload 不含 rank 值 / IP / 操作人——升会员位这件事在事件流里看不出升了什么、谁升的。
func TestRankCarriesPersonInfoActionAndNoAuditFields(t *testing.T) {
	e := newEnv(t)
	e.st.base.put(&model.UserBase{Mid: 32500, Rank: 5000})

	l := NewSetRankLogic(context.Background(), e.svcCtx)
	_, err := l.SetRank(&rpc.UpdateRankReq{Mid: 32500, Rank: 99999, RemoteIp: "203.0.113.99"})
	wantNoErr(t, "rank 事件", err)
	wantSetBaseWritten(t, "rank 事件", e, 32500, "SetRank", repository.ActUpdatePersonInfo, 1)

	_, pl := decodeOutboxView(t, "rank 事件", e.st.outbox.row(0))
	wantEQ(t, "rank 事件", "payload 键数量", len(pl), 2)
	if _, ok := pl["rank"]; ok {
		t.Errorf("rank 事件：payload 出现了 rank = %v，与本结论不符", pl["rank"])
	}
	if _, ok := pl["ip"]; ok {
		t.Errorf("rank 事件：payload 出现了 ip = %v，与本结论不符", pl["ip"])
	}
	wantEQ(t, "rank 事件", "member_log 行数（升会员位不留服务端变更日志）", e.st.logs.count(), 0)
}
