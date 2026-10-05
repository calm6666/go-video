package logic

// addexp3logic_test.go 覆盖 AddExp3（logic/addexp3logic.go:30-36 →
// repository.AddExp internal/repository/exp_moral.go:12-23 → cache.DelCache
// internal/repository/cache.go:287-296）。
//
// 被测判定链：nil 下游 → ErrNotImplemented（一次调用都不发）；下游写失败 → 原始错误
// 上抛且**不失效缓存**；下游写成功 → 把 mid/exp/operater/operate/reason 五个入参
// 原样交给 user-profile，随后静默失效该 mid 的四个缓存键（`_ =` 丢弃错误）。
//
// 钉住的事实：
//  1. 入参逐字保真：mid/exp（含负数、极小值）/三段字符串（含空串）都不加工、不裁剪、
//     不填默认值 —— logic 的 5 个实参顺序与 ExpReq 字段顺序一致，Operate 与 Reason
//     互换即红；
//  2. ExpReq.real_ip 在这条链路上**无人使用**（本批缺口 28）：logic 不读，
//     UserProfileClient.AddExp 签名里没有它，缓存键里也没有它，所以审计侧只能看到
//     「某个 mid 的经验变了 N」而看不到来源 IP —— 与同批 AddMoral3 同形；
//  3. 与 DelCache（RPC 方法）不同，这里的失效是 repository.AddExp 内联调
//     cache.DelCache（exp_moral.go:21），**只删不回温**（不走 cache_delay.go 的 reWarm），
//     于是四个键删完后读侧要各自回源一次；
//  4. 失败与成功在答复形状上可区分：失败时 logic 回 (nil, err)，成功时回 (非 nil 空
//     ExpReply, nil)，且 err 是原始错误（未被 fmt.Errorf 包裹掉 errors.Is 链）；
//  5. 不校验 mid/exp：mid=0、exp=0 照样打到下游并照删缓存。
//
// 覆盖不到的分支（如实声明）：
//   - user-profile 适配器内部（internal/repository/userprofile_client.go 的 grpc 调用与
//     字段映射）仍不可离线覆盖，本文件只替到 UserProfileClient 接口边界。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/rpc"
)

func callAddExp3(t *testing.T, e *env, in *rpc.ExpReq) (*rpc.ExpReply, error) {
	t.Helper()
	return NewAddExp3Logic(context.Background(), e.svcCtx).AddExp3(in)
}

// expDelOps 是 exp_moral.go:21 那次内联失效在替身上的唯一轨迹（只删、不回温）。
func expDelOps(mid int64) string {
	return "cache.DelCache:" + delCacheKeysInDeleteOrder(mid)
}

// TestAddExp3ForwardsAllFiveArgsVerbatim 入参保真：ExpReq 的五个业务字段一字不动地
// 落到 user-profile.AddExp 的形参上（含负数、小数、空串、零值）。
func TestAddExp3ForwardsAllFiveArgsVerbatim(t *testing.T) {
	const ip = "203.0.113.9"
	cases := []struct {
		name string
		in   *rpc.ExpReq
	}{
		{"满值", &rpc.ExpReq{Mid: 70001, Exp: 12.5, Operater: "admin:1001", Operate: "daily_sign", Reason: "签到奖励", RealIp: ip}},
		{"扣减为负", &rpc.ExpReq{Mid: 70001, Exp: -30, Operater: "系统", Operate: "punishment", Reason: "违规扣分", RealIp: ip}},
		{"三段字符串全空", &rpc.ExpReq{Mid: 70001, Exp: 1, RealIp: ip}},
		{"只有 reason", &rpc.ExpReq{Mid: 70001, Exp: 1, Reason: "  前后空格要保留  ", RealIp: ip}},
		{"mid 与 exp 都是零值", &rpc.ExpReq{Operater: "admin", Operate: "manual", Reason: "补发", RealIp: ip}},
		{"极小/极大 exp", &rpc.ExpReq{Mid: 70002, Exp: 5e-324, Operater: "a", Operate: "b", Reason: "c", RealIp: ip}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstreamWrite())
			st := e.st
			st.log.reset()

			reply, err := callAddExp3(t, e, tc.in)
			wantNoErr(t, "AddExp3", err)
			if reply == nil {
				t.Fatalf("reply = nil, want 非 nil 空答复")
			}
			wantProto(t, "成功答复是空消息", "ExpReply", reply, &rpc.ExpReply{})

			want := []string{"userProfile.AddExp:" + itoa(tc.in.Mid), expDelOps(tc.in.Mid)}
			// mid=0 也照发（不校验），键名是 i3_0 等。
			wantOps(t, "先写下游再失效缓存", e.ops(0), want)

			got := st.userProfile.expWriteAt(0)
			wantWrite := expWrite{Mid: tc.in.Mid, Exp: tc.in.Exp, Operater: tc.in.Operater, Operate: tc.in.Operate, Reason: tc.in.Reason}
			if got != wantWrite {
				t.Errorf("下游收到的入参 = %+v, want %+v", got, wantWrite)
			}
			// 本批缺口 28：real_ip 全程无人使用 —— 既不在下游入参里，也不在轨迹里。
			wantNotContains(t, "real_ip 不得泄漏到缓存键/轨迹", strings.Join(e.ops(0), "|"), ip)
			wantCount(t, "只写一次下游", e.ops(0), "userProfile.AddExp:", 1)
			if n, _ := st.userProfile.writeStats(); n != 1 {
				t.Errorf("AddExp 记账 = %d 条, want 1（不得重复写）", n)
			}
		})
	}
}

// TestAddExp3OperateAndReasonAreNotSwapped 专门盯实参顺序：Operate/Reason 都是字符串，
// 换位后编译仍然通过，只有按字段名比对才能发现。
func TestAddExp3OperateAndReasonAreNotSwapped(t *testing.T) {
	e := newEnv(t, withDownstreamWrite())
	st := e.st
	st.log.reset()

	_, err := callAddExp3(t, e, &rpc.ExpReq{Mid: 70001, Exp: 1, Operater: "OPERATER", Operate: "OPERATE", Reason: "REASON"})
	wantNoErr(t, "AddExp3", err)
	got := st.userProfile.expWriteAt(0)
	if got.Operater != "OPERATER" || got.Operate != "OPERATE" || got.Reason != "REASON" {
		t.Errorf("三段字符串错位：operater=%q operate=%q reason=%q", got.Operater, got.Operate, got.Reason)
	}
}

// TestAddExp3InvalidatesOnlyFourKeysAndNeverRewarms 与 DelCache（RPC）的关键差异：
// exp_moral.go:21 直接调 cache.DelCache，删完就走，没有 reWarm 的十步回温。
func TestAddExp3InvalidatesOnlyFourKeysAndNeverRewarms(t *testing.T) {
	const (
		mid   = int64(70001)
		other = int64(70002)
	)
	e := newEnv(t, withDownstreamWrite())
	st := e.st
	for _, key := range []string{infoKey(mid), cardKey(mid), vipKey(mid), profileKey(mid)} {
		st.cache.warmMsg(key, &rpc.Info{Mid: mid, Name: "旧昵称"}, 999)
	}
	for _, key := range []string{infoKey(other), cardKey(other), vipKey(other), profileKey(other)} {
		st.cache.warmMsg(key, &rpc.Info{Mid: other, Name: "别人的昵称"}, 999)
	}
	st.log.reset()

	_, err := callAddExp3(t, e, &rpc.ExpReq{Mid: mid, Exp: 1, Operater: "o", Operate: "a", Reason: "r"})
	wantNoErr(t, "AddExp3", err)

	wantOps(t, "整条链路只有两步：写下游 + 删键（无回温）", e.ops(0),
		[]string{"userProfile.AddExp:" + itoa(mid), expDelOps(mid)})
	for _, key := range []string{infoKey(mid), cardKey(mid), vipKey(mid), profileKey(mid)} {
		if st.cache.has(key) {
			t.Errorf("%s 没被删掉", key)
		}
	}
	for _, key := range []string{infoKey(other), cardKey(other), vipKey(other), profileKey(other)} {
		if !st.cache.has(key) {
			t.Errorf("%s 被误删（失效范围越界）", key)
		}
	}
}

// TestAddExp3DownstreamFailureSkipsInvalidation 下游写失败：错误原样上抛、答复为 nil、
// 且一次缓存都不删（缓存里还是旧资料，但调用方会重试，这是正确行为）。
func TestAddExp3DownstreamFailureSkipsInvalidation(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: user-profile 写经验失败")

	e := newEnv(t, withDownstreamWrite())
	st := e.st
	st.cache.warmMsg(infoKey(mid), &rpc.Info{Mid: mid, Name: "旧昵称"}, 999)
	st.userProfile.failWith("AddExp", boom)
	st.log.reset()

	reply, err := callAddExp3(t, e, &rpc.ExpReq{Mid: mid, Exp: 1})
	if !errors.Is(err, boom) {
		t.Errorf("AddExp3 错误 = %v, want 原始故障 %v", err, boom)
	}
	if reply != nil {
		t.Errorf("失败时 reply = %v, want nil（logic 失败不得回空答复）", reply)
	}
	wantOps(t, "写失败后不得删缓存", e.ops(0), []string{"userProfile.AddExp:" + itoa(mid)})
	if _, ok := msgAs[*rpc.Info](st.cache, infoKey(mid)); !ok {
		t.Errorf("前提已变：%s 竟然被删了", infoKey(mid))
	}
}

// TestAddExp3WithoutUserProfileIsNotImplemented 未注入下游（servicecontext.go:30-33 的
// etcd/target 都为空时就是这一形状）：ErrNotImplemented 且一次下游调用都不发。
// 反向哨兵：把 nil 检查挪到调用之后就会红。
func TestAddExp3WithoutUserProfileIsNotImplemented(t *testing.T) {
	t.Run("未注入下游", func(t *testing.T) {
		e := newEnv(t, withoutUserProfile())
		st := e.st
		st.log.reset()

		reply, err := callAddExp3(t, e, &rpc.ExpReq{Mid: 70001, Exp: 1})
		wantErrIs(t, "AddExp3", err, repository.ErrNotImplemented)
		if reply != nil {
			t.Errorf("reply = %v, want nil", reply)
		}
		wantOps(t, "nil 检查必须早于任何调用", e.ops(0), nil)
		// 区分于替身的响铃：报的必须是 repository 自己那一条。
		wantNotContains(t, "不得把替身信号当错误外传", errText(err), errDownstreamWired.Error())
	})
	// 「写侧已接线但整个 client 没注入」：仍然只报 ErrNotImplemented，
	// 证明错误来自 repository 的 nil 检查而不是替身。
	t.Run("写侧接线也不能越过 nil 检查", func(t *testing.T) {
		e := newEnv(t, withoutUserProfile(), withDownstreamWrite())
		st := e.st
		st.log.reset()

		_, err := callAddExp3(t, e, &rpc.ExpReq{Mid: 70001, Exp: 1})
		wantErrIs(t, "AddExp3", err, repository.ErrNotImplemented)
		wantOps(t, "零调用", e.ops(0), nil)
		if exp, moral := st.userProfile.writeStats(); exp != 0 || moral != 0 {
			t.Errorf("写侧记账 = exp %d/moral %d, want 0/0", exp, moral)
		}
	})
}

// TestAddExp3CacheInvalidationFailureIsSilent 写成功了但缓存失效失败：错误被
// `_ = r.cache.DelCache(...)`（exp_moral.go:21）丢掉，客户端拿到成功。
//
// TODO(缺陷)（本批缺口 27 的同族，exp_moral.go:21 与 :37 各一处）：与 DelCache 的
// logic 层吞错一样，这里连「记进 errs 再交给上层」的机会都没有。
// 此处钉住**当前**行为，不改生产代码。
func TestAddExp3CacheInvalidationFailureIsSilent(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: redis DEL 失败")

	// 先证明替身确实会把失败上报给 repository（否则「被丢掉」无从谈起）。
	e0 := newRawStore(withDownstreamWrite())
	e0.cache.failWith("DelCache", boom)
	if errs := e0.cache.DelCache(context.Background(), mid); len(errs) == 0 {
		t.Fatalf("前提已变：cache.DelCache 失败不再返回错误，请复核 cache.go:287-296")
	}

	e := newEnv(t, withDownstreamWrite())
	st := e.st
	st.cache.warmMsg(infoKey(mid), &rpc.Info{Mid: mid, Name: "旧昵称"}, 999)
	st.cache.failWith("DelCache", boom)
	st.log.reset()

	reply, err := callAddExp3(t, e, &rpc.ExpReq{Mid: mid, Exp: 1})
	wantNoErr(t, "缓存失效失败不得外传（被钉住的当前行为）", err)
	wantProto(t, "失效失败与成功的答复同形", "ExpReply", reply, &rpc.ExpReply{})
	wantNotContains(t, "故障原文不得进答复", errText(err), boom.Error())
	wantOps(t, "删键仍被尝试过一次", e.ops(0),
		[]string{"userProfile.AddExp:" + itoa(mid), expDelOps(mid)})
	if _, ok := msgAs[*rpc.Info](st.cache, infoKey(mid)); !ok {
		t.Errorf("前提已变：%s 竟然被删了", infoKey(mid))
	}
}
