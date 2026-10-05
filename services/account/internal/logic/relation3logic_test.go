package logic

// relation3logic_test.go 覆盖 Relation3（logic/relation3logic.go:29-39 →
// repository.Relation internal/repository/relation.go:13-23 → SocialGraphClient.Relation
// 接口，repository.go:66-77）。
//
// 被测判定链：logic 只把 (in.Mid, in.Owner) 交给 Repository → Repository 在
// 「下游未注入」时直接回 Following=false →「下游报错」时记日志后同样回
// Following=false → 只有下游真的答了才透传布尔值。三条路径**都不返回 error**。
//
// 钉住的事实：
//  1. 本方法在 logic 层**不可能**返回错误：relation.go:18-21 把下游错误吞成
//     「未关注」，用例注入故障后照样拿到 (非 nil, nil)；
//  2. 因此 relation3logic.go:31-34（err 分支）与 35-37（nil reply 分支）都是死代码，
//     「reply 一定非 nil」这个保证住在 repository/relation.go:15,20,22，不在 logic；
//     TestRelation3NonNilReplyComesFromRepositoryNotLogic 用直接调 Repository 的
//     三条路径来归属这件事；
//  3. 生产形状：internal/svc/servicecontext.go:35-38 里 socialGraph 只有 TODO、
//     从不赋值，所以生产环境 Relation3 永远回 Following=false 且一步下游都不调；
//  4. (mid, owner) 原样传到下游接口边界，且 mid=0 / owner=0 都不校验。
//
// 覆盖不到的分支（如实声明）：
//   - SocialGraphClient 在本仓**只有接口、没有适配器实现**（repository.go:66-77，
//     全仓无 social_graph_client.go），因此「生产可达」这一侧根本不存在：
//     下面所有 Following=true 都只在替身接线时成立。本文件断言到接口边界为止。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/account/rpc"
)

func callRelation3(t *testing.T, e *env, mid, owner int64) (*rpc.RelationReply, error) {
	t.Helper()
	return NewRelation3Logic(context.Background(), e.svcCtx).Relation3(
		&rpc.RelationReq{Mid: mid, Owner: owner, RealIp: "1.2.3.4"})
}

// TestRelation3ProductionShapeAlwaysFalse 复刻生产装配（social-graph 未注入）：
// 答复恒为「未关注」，并且**一步下游都不许调**——这是「五个关系 RPC 生产恒空」这条
// 缺口的直接证据，也是本用例与下面几条的唯一区别。
func TestRelation3ProductionShapeAlwaysFalse(t *testing.T) {
	e := newEnv(t, withoutSocialGraph())
	st := e.st
	// 布了数据也没用：nil client 分支在查布数据之前就被短路了。
	st.socialGraph.follows = map[int64]map[int64]bool{70001: {70002: true}}
	st.log.reset()

	reply, err := callRelation3(t, e, 70001, 70002)
	wantNoErr(t, "生产形状（未注入 social-graph）的 Relation3", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil（客户端要读 following）")
	}
	wantEQ(t, "未注入即「未关注」", "following", reply.GetFollowing(), false)
	wantOps(t, "未注入不得触任何依赖", e.ops(0), nil)
	wantEQ(t, "关系族入参记账也必须为空", "条数", st.socialGraph.totalCalls(), 0)
}

// TestRelation3FollowsDownstreamAnswer 注入替身后的可达形状：下游说关注就是关注。
// 与上一条合读才能得出「Following=true 只有替身接线时才可能」这个结论。
func TestRelation3FollowsDownstreamAnswer(t *testing.T) {
	const (
		mid   = int64(70001)
		owner = int64(70002)
	)
	e := newEnv(t, withDownstream(mid, Downstream{Relation: map[int64]bool{owner: true}}))
	st := e.st
	st.log.reset()

	reply, err := callRelation3(t, e, mid, owner)
	wantNoErr(t, "Relation3", err)
	wantEQ(t, "下游答 following=true 必须透出", "following", reply.GetFollowing(), true)
	// 轨迹里两个整数操作数之间有一个空格（fmt.Sprint 的规则：相邻都不是字符串时补空格），
	// 所以 (70001,70002) 与 (700017,2) 在字符串上仍可区分；但「切片类参数」不行
	// （owners 走 joinInts，nil 与 []int64{} 都拼成空串），那类保真只能看 sgCall。
	wantOps(t, "只问下游一次", e.ops(0), []string{"socialGraph.Relation:70001 70002"})

	// 布了 owner=70003 而问 70002：必须是 false，不能被「这个 mid 有数据」带偏。
	other, err := callRelation3(t, e, mid, 70003)
	wantNoErr(t, "Relation3（未布的 owner）", err)
	wantEQ(t, "布数据外的 owner 仍是未关注", "following", other.GetFollowing(), false)
}

// TestRelation3PassesArgsUntouched mid/owner 原样交付下游接口，含 0 值与相邻值。
// 结构化的 sgCall 是唯一能区分 (1,12) 与 (11,2) 的记账面。
func TestRelation3PassesArgsUntouched(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mid, owner int64
	}{
		{"正常对", 70001, 70002},
		{"mid 为 0（不校验）", 0, 70002},
		{"owner 为 0（不校验）", 70001, 0},
		{"两者都是 0", 0, 0},
		{"小值对 A", 1, 12},
		{"小值对 B（与 A 的 mid/owner 交换）", 12, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			st.log.reset()

			_, err := callRelation3(t, e, tc.mid, tc.owner)
			wantNoErr(t, "Relation3", err)
			got := st.socialGraph.onlyCall("Relation")
			if !reflect.DeepEqual(got, sgCall{method: "Relation", mid: tc.mid, owner: tc.owner}) {
				t.Errorf("传给下游的入参 = %+v, want {mid:%d owner:%d}", got, tc.mid, tc.owner)
			}
		})
	}
}

// TestRelation3DownstreamFaultBecomesFalseNotError 下游报错被 repository 吞成
// 「未关注」：客户端拿到的是 200 + following=false，而不是一次失败。
//
// TODO(缺陷)（本批缺口 25）：relation.go:18-21 只 logx.Errorf 就把错误转成业务否值，
// 于是 social-graph 宕机与「确实没关注」在响应上完全同形。网关若按 following 决定
// 「能否发私信/订阅」，一次下游抖动就会把已关注用户判成未关注，且调用方无从发现。
// 此处钉住**当前**行为。
func TestRelation3DownstreamFaultBecomesFalseNotError(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: social-graph 不可用")
	e := newEnv(t, withDownstream(mid, Downstream{Relation: map[int64]bool{70002: true}}))
	st := e.st
	st.socialGraph.failWith("Relation", boom)
	st.log.reset()

	reply, err := callRelation3(t, e, mid, 70002)
	wantNoErr(t, "下游故障的 Relation3 必须降级不报错", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil")
	}
	wantEQ(t, "布好的 true 被故障吞成 false", "following", reply.GetFollowing(), false)
	wantOps(t, "故障仍走完那一次下游调用", e.ops(0), []string{"socialGraph.Relation:70001 70002"})
	// 错误既没进 error 也没进报文：只剩服务端日志（TestMain 里 logx.Disable 了）。
	wantNotContains(t, "下游错误原文不得外传给客户端（缺口 10 同形）", reply.String(), boom.Error())
}

// TestRelation3NonNilReplyComesFromRepositoryNotLogic 归属「nil→非 nil」这条保证：
// relation3logic.go:35-37 的兜底永远走不到，因为 Repository 的三条 return
// （relation.go:15/20/22）本身就是非 nil。直接调 Repository 三种装配形状来证明，
// 而不是只在 logic 上观察（logic 上观察到的非 nil 无法区分是哪一层给的）。
func TestRelation3NonNilReplyComesFromRepositoryNotLogic(t *testing.T) {
	const mid = int64(70001)
	t.Run("未注入", func(t *testing.T) {
		e := newEnv(t, withoutSocialGraph())
		reply, err := e.st.repo.Relation(context.Background(), mid, 70002)
		wantNoErr(t, "Repository.Relation", err)
		if reply == nil {
			t.Fatalf("Repository 竟然回了 nil reply，logic 的兜底分支就变成可达的了")
		}
	})
	t.Run("已接线且故障", func(t *testing.T) {
		e := newEnv(t, withDownstream(mid, Downstream{Relation: map[int64]bool{70002: true}}))
		e.st.socialGraph.failWith("Relation", errors.New("account/test: 故障"))
		reply, err := e.st.repo.Relation(context.Background(), mid, 70002)
		wantNoErr(t, "Repository.Relation（故障）", err)
		if reply == nil {
			t.Fatalf("Repository 竟然回了 nil reply")
		}
	})
	t.Run("已接线且成功", func(t *testing.T) {
		e := newEnv(t, withDownstream(mid, Downstream{Relation: map[int64]bool{70002: true}}))
		reply, err := e.st.repo.Relation(context.Background(), mid, 70002)
		wantNoErr(t, "Repository.Relation（正常）", err)
		if reply == nil {
			t.Fatalf("Repository 竟然回了 nil reply")
		}
		wantEQ(t, "直调 Repository 也拿得到 true", "following", reply.GetFollowing(), true)
	})
}
