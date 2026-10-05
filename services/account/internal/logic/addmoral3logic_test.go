package logic

// addmoral3logic_test.go 覆盖 AddMoral3（logic/addmoral3logic.go:31-37 →
// repository.AddMoral internal/repository/exp_moral.go:28-39 → cache.DelCache
// internal/repository/cache.go:287-296）。
//
// 与 AddExp3 同族但入参不同：MoralReq 的操作者字段叫 oper（不是 ExpReq 的
// operater/operate 两段），第五段是 remark。本文件因此独立存在。
//
// 钉住的事实：
//  1. 四段业务入参（mid/moral/oper/reason/remark）逐字保真，顺序不错位；
//  2. addmoral3logic.go:28-29 的注释声明「delta=moral*100、Operator 默认"系统"、
//     RewardType/PunishmentType 由 user-profile 落地」—— account 侧确实**一点都没加工**：
//     moral=1 到下游仍是 1（不是 100），oper 空串仍是空串（不会被填成"系统"）。
//     这是职责边界哨兵：如果哪天有人在本服务里补上 *100 或默认值，本用例会红，
//     从而暴露「两侧都算一遍」的双计费风险；
//  3. moral 允许为负/为零，account 不做上下界校验（连 mid=0 都不校验）；
//  4. 写成功 → 只删该 mid 的四个缓存键、不回温（exp_moral.go:37 与 :21 同形，
//     都不走 cache_delay.go 的 reWarm）；写失败 → 一步缓存都不碰；
//  5. MoralReq.real_ip 同样无人使用（本批缺口 28，与 AddExp3 同因）。
//
// 覆盖不到的分支（如实声明）：user-profile 适配器内部（RPC 字段映射与 *100 规则是否
// 真在那一侧落地）不可离线覆盖，本文件只替到 UserProfileClient 接口边界。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/rpc"
)

func callAddMoral3(t *testing.T, e *env, in *rpc.MoralReq) (*rpc.MoralReply, error) {
	t.Helper()
	return NewAddMoral3Logic(context.Background(), e.svcCtx).AddMoral3(in)
}

// TestAddMoral3ForwardsAllFourArgsVerbatim 入参保真（含负值、小数、空串、零值）。
func TestAddMoral3ForwardsAllFourArgsVerbatim(t *testing.T) {
	const ip = "198.51.100.7"
	cases := []struct {
		name string
		in   *rpc.MoralReq
	}{
		{"满值", &rpc.MoralReq{Mid: 70001, Moral: 3, Oper: "admin:1001", Reason: "举报成立", Remark: "工单 88", RealIp: ip}},
		{"扣减为负", &rpc.MoralReq{Mid: 70001, Moral: -100, Oper: "系统", Reason: "违规处罚", Remark: "自动", RealIp: ip}},
		{"三段字符串全空", &rpc.MoralReq{Mid: 70001, Moral: 1, RealIp: ip}},
		{"只有 remark", &rpc.MoralReq{Mid: 70001, Moral: 1, Remark: "  空格保留  ", RealIp: ip}},
		{"mid 与 moral 都是零值", &rpc.MoralReq{Oper: "admin", Reason: "r", Remark: "m", RealIp: ip}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstreamWrite())
			st := e.st
			st.log.reset()

			reply, err := callAddMoral3(t, e, tc.in)
			wantNoErr(t, "AddMoral3", err)
			if reply == nil {
				t.Fatalf("reply = nil, want 非 nil 空答复")
			}
			wantProto(t, "成功答复是空消息", "MoralReply", reply, &rpc.MoralReply{})
			wantOps(t, "先写下游再失效缓存", e.ops(0),
				[]string{"userProfile.AddMoral:" + itoa(tc.in.Mid), "cache.DelCache:" + delCacheKeysInDeleteOrder(tc.in.Mid)})

			got := st.userProfile.moralWriteAt(0)
			want := moralWrite{Mid: tc.in.Mid, Moral: tc.in.Moral, Oper: tc.in.Oper, Reason: tc.in.Reason, Remark: tc.in.Remark}
			if got != want {
				t.Errorf("下游收到的入参 = %+v, want %+v", got, want)
			}
			wantCount(t, "只写一次下游", e.ops(0), "userProfile.AddMoral:", 1)
			if _, m := st.userProfile.writeStats(); m != 1 {
				t.Errorf("AddMoral 记账 = %d 条, want 1", m)
			}
			// 本批缺口 28：real_ip 全程无人使用。
			wantNotContains(t, "real_ip 不得泄漏到轨迹", strings.Join(e.ops(0), "|"), ip)
		})
	}
}

// TestAddMoral3AppliesNoBusinessRules account 侧不做 *100、不填默认操作者
// （addmoral3logic.go:26-28 把规则归属写给了 user-profile）。
// 反向哨兵：在本服务里加 `moral*100` 或 `if oper == "" { oper = "系统" }` 即红。
func TestAddMoral3AppliesNoBusinessRules(t *testing.T) {
	e := newEnv(t, withDownstreamWrite())
	st := e.st
	st.log.reset()

	_, err := callAddMoral3(t, e, &rpc.MoralReq{Mid: 70001, Moral: 1, Reason: "r"})
	wantNoErr(t, "AddMoral3", err)
	got := st.userProfile.moralWriteAt(0)
	if got.Moral != 1 {
		t.Errorf("moral = %v, want 原样 1（本服务不得乘 100）", got.Moral)
	}
	if got.Oper != "" {
		t.Errorf("oper = %q, want 原样空串（本服务不得代填默认操作者）", got.Oper)
	}
	if got.Remark != "" {
		t.Errorf("remark = %q, want 原样空串", got.Remark)
	}
}

// TestAddMoral3OperAndReasonAndRemarkAreNotSwapped oper/reason/remark 三段同为字符串，
// 换位照样编译得过，只能按字段名比对。
func TestAddMoral3OperAndReasonAndRemarkAreNotSwapped(t *testing.T) {
	e := newEnv(t, withDownstreamWrite())
	st := e.st
	st.log.reset()

	_, err := callAddMoral3(t, e, &rpc.MoralReq{Mid: 70001, Moral: 1, Oper: "OPER", Reason: "REASON", Remark: "REMARK"})
	wantNoErr(t, "AddMoral3", err)
	got := st.userProfile.moralWriteAt(0)
	if got.Oper != "OPER" || got.Reason != "REASON" || got.Remark != "REMARK" {
		t.Errorf("三段字符串错位：oper=%q reason=%q remark=%q", got.Oper, got.Reason, got.Remark)
	}
}

// TestAddMoral3DownstreamFailureSkipsInvalidation 写失败：原始错误上抛、答复为 nil、
// 四个键一个不删。
func TestAddMoral3DownstreamFailureSkipsInvalidation(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: user-profile 写道德失败")

	e := newEnv(t, withDownstreamWrite())
	st := e.st
	st.cache.warmMsg(profileKey(mid), &rpc.Profile{Mid: mid, Name: "旧资料"}, 999)
	st.userProfile.failWith("AddMoral", boom)
	st.log.reset()

	reply, err := callAddMoral3(t, e, &rpc.MoralReq{Mid: mid, Moral: 1})
	if !errors.Is(err, boom) {
		t.Errorf("AddMoral3 错误 = %v, want 原始故障 %v", err, boom)
	}
	if reply != nil {
		t.Errorf("失败时 reply = %v, want nil", reply)
	}
	wantOps(t, "写失败后不得删缓存", e.ops(0), []string{"userProfile.AddMoral:" + itoa(mid)})
	wantNoOpsWith(t, "写失败后不得触缓存", e.ops(0), "cache.")
	if _, ok := msgAs[*rpc.Profile](st.cache, profileKey(mid)); !ok {
		t.Errorf("前提已变：%s 竟然被删了", profileKey(mid))
	}
}

// TestAddMoral3WithoutUserProfileIsNotImplemented 未注入下游（servicecontext.go:30-33
// 的 etcd/target 都为空时即此形状）：ErrNotImplemented 且零调用。
func TestAddMoral3WithoutUserProfileIsNotImplemented(t *testing.T) {
	e := newEnv(t, withoutUserProfile(), withDownstreamWrite())
	st := e.st
	st.log.reset()

	reply, err := callAddMoral3(t, e, &rpc.MoralReq{Mid: 70001, Moral: 1})
	wantErrIs(t, "AddMoral3", err, repository.ErrNotImplemented)
	if reply != nil {
		t.Errorf("reply = %v, want nil", reply)
	}
	wantOps(t, "nil 检查早于任何调用", e.ops(0), nil)
	if exp, moral := st.userProfile.writeStats(); exp != 0 || moral != 0 {
		t.Errorf("写侧记账 = exp %d/moral %d, want 0/0（两条链路互不串台）", exp, moral)
	}
}

// TestAddMoral3CacheInvalidationFailureIsSilent 写成功、失效失败：错误被
// `_ = r.cache.DelCache(...)`（exp_moral.go:37）丢掉，客户端拿到成功答复。
//
// TODO(缺陷)（本批缺口 27 的同族，exp_moral.go:21 与 :37 各一处）：钉住**当前**行为。
func TestAddMoral3CacheInvalidationFailureIsSilent(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: redis DEL 失败")

	e0 := newRawStore(withDownstreamWrite())
	e0.cache.failWith("DelCache", boom)
	if errs := e0.cache.DelCache(context.Background(), mid); len(errs) == 0 {
		t.Fatalf("前提已变：cache.DelCache 失败不再返回错误，请复核 cache.go:287-296")
	}

	e := newEnv(t, withDownstreamWrite())
	st := e.st
	st.cache.warmMsg(profileKey(mid), &rpc.Profile{Mid: mid, Name: "旧资料"}, 999)
	st.cache.failWith("DelCache", boom)
	st.log.reset()

	reply, err := callAddMoral3(t, e, &rpc.MoralReq{Mid: mid, Moral: -1, Oper: "系统", Reason: "处罚"})
	wantNoErr(t, "缓存失效失败不得外传（被钉住的当前行为）", err)
	wantProto(t, "失效失败与成功的答复同形", "MoralReply", reply, &rpc.MoralReply{})
	wantOps(t, "删键仍被尝试过一次且不回温", e.ops(0),
		[]string{"userProfile.AddMoral:" + itoa(mid), "cache.DelCache:" + delCacheKeysInDeleteOrder(mid)})
	if _, ok := msgAs[*rpc.Profile](st.cache, profileKey(mid)); !ok {
		t.Errorf("前提已变：%s 竟然被删了（旧道德值本该留在缓存里）", profileKey(mid))
	}
}

// TestAddMoral3InvalidatesSameKeysAsAddExp 两条链路的失效面完全一致（都是
// i3_/c3_/v3_/p3_ 四键、都不回温）：差别只在下游方法与入参。
func TestAddMoral3InvalidatesSameKeysAsAddExp(t *testing.T) {
	const mid = int64(70001)
	exp := newEnv(t, withDownstreamWrite())
	mor := newEnv(t, withDownstreamWrite())
	exp.st.log.reset()
	mor.st.log.reset()

	_, err := callAddExp3(t, exp, &rpc.ExpReq{Mid: mid, Exp: 1})
	wantNoErr(t, "AddExp3", err)
	_, err = callAddMoral3(t, mor, &rpc.MoralReq{Mid: mid, Moral: 1})
	wantNoErr(t, "AddMoral3", err)

	expOps, morOps := exp.ops(0), mor.ops(0)
	if len(expOps) != 2 || len(morOps) != 2 {
		t.Fatalf("序列长度变了：exp %q / moral %q", expOps, morOps)
	}
	if expOps[1] != morOps[1] {
		t.Errorf("失效序列不一致：%q vs %q", expOps[1], morOps[1])
	}
	wantNoOpsWith(t, "两条链路都不该回温", expOps, "cache.Cache")
	wantNoOpsWith(t, "两条链路都不该回温", morOps, "cache.Cache")
}
