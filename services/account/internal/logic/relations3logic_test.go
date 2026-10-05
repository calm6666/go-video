package logic

// relations3logic_test.go 覆盖 Relations3（logic/relations3logic.go:29-42 →
// repository.Relations internal/repository/relation.go:27-47 → SocialGraphClient.Relations）。
//
// 被测判定链：logic 把 (in.Mid, in.Owners) 交给 Repository → Repository 先按 owners
// 建好 map（relation.go:28）→ 问一次下游 → 逐个 owner 取值，取不到就补 Following=false。
// 因此「每个 owner 都有条目」这件事由 repository 保证，与下游有没有答、答得对不对无关。
//
// 钉住的事实：
//  1. logic 永不可能拿到 error（relation.go:36-38 把下游错误吞成逐条默认值），
//     所以 relations3logic.go:32-34 与 38-40 都是死代码；
//  2. relations 这个 map 一定非 nil，且键集恰为 owners 的**去重集**（重复 owner 会被
//     map 合并，条目数少于入参条数）；
//  3. owners 为 nil 与为 []int64{} 在响应上完全同形（都是空 map）——字符串轨迹里两者
//     也拼成同一个串，所以入参保真必须看结构化的 sgCall.list（本文件用它区分 nil/空）；
//  4. 生产形状：socialGraph 未注入 → 逐条补 false、且一步下游都不调；
//  5. mid=0 不校验（会真的拿 mid=0 去问下游）。
//
// 覆盖不到的分支（如实声明）：
//   - SocialGraphClient 只有接口没有适配器实现（repository.go:66-77），
//     「下游真答 following=true」只在替身接线时可达。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/account/rpc"
)

func callRelations3(t *testing.T, e *env, mid int64, owners []int64) (*rpc.RelationsReply, error) {
	t.Helper()
	return NewRelations3Logic(context.Background(), e.svcCtx).Relations3(
		&rpc.RelationsReq{Mid: mid, Owners: owners, RealIp: "1.2.3.4"})
}

// followingOf 从答复里取一个 owner 的 following（缺键直接 Fatal：
// 「每个 owner 都有条目」是本方法唯一对外承诺，缺了就是契约违约）。
func followingOf(t *testing.T, reply *rpc.RelationsReply, owner int64) bool {
	t.Helper()
	rel, ok := reply.GetRelations()[owner]
	if !ok {
		t.Fatalf("owners 里的 %d 在 relations 中没有条目（当前键集 %v）", owner, mapKeysOf(reply.GetRelations()))
	}
	if rel == nil {
		t.Fatalf("relations[%d] = nil，客户端没法读它的 following", owner)
	}
	return rel.GetFollowing()
}

func mapKeysOf(m map[int64]*rpc.RelationReply) []int64 {
	var ks []int64
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestRelations3ProductionShapePadsEveryOwner 生产形状（未注入 social-graph）：
// 每个 owner 都拿到 Following=false，且不触任何依赖。
// 这条与下一条一起证明「批量关系的形状」与「批量关系的答案」在生产里分别是
// 「齐全」与「全否」——形状对了，答案永远错。
func TestRelations3ProductionShapePadsEveryOwner(t *testing.T) {
	e := newEnv(t, withoutSocialGraph())
	st := e.st
	st.socialGraph.follows = map[int64]map[int64]bool{70001: {70002: true}}
	st.log.reset()

	reply, err := callRelations3(t, e, 70001, []int64{70002, 70003})
	wantNoErr(t, "生产形状的 Relations3", err)
	wantEQ(t, "未注入也要补齐两个条目", "条目数", len(reply.GetRelations()), 2)
	wantEQ(t, "布好的 true 在生产里读不到", "70002", followingOf(t, reply, 70002), false)
	wantEQ(t, "没布的 owner 同为 false", "70003", followingOf(t, reply, 70003), false)
	wantOps(t, "未注入不得触任何依赖", e.ops(0), nil)
	wantEQ(t, "关系族入参记账也必须为空", "条数", st.socialGraph.totalCalls(), 0)
}

// TestRelations3PassesThroughDownstreamAnswers 注入替身：true/false 各自照答，
// 布数据外的 owner 补 false（不是漏键）。
func TestRelations3PassesThroughDownstreamAnswers(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t,
		withDownstream(mid, Downstream{Relation: map[int64]bool{70002: true}}),
		withDownstream(70009, Downstream{Relation: map[int64]bool{70002: true}}),
	)
	st := e.st
	st.log.reset()

	reply, err := callRelations3(t, e, mid, []int64{70002, 70003})
	wantNoErr(t, "Relations3", err)
	wantEQ(t, "下游说关注", "70002", followingOf(t, reply, 70002), true)
	wantEQ(t, "下游没这个 owner → 补 false", "70003", followingOf(t, reply, 70003), false)
	// 只问下游一次（批量语义），且入参是请求里那个 owners 原样。
	wantOps(t, "批量只回源一次", e.ops(0), []string{"socialGraph.Relations:7000170002,70003"})
	wantEQ(t, "回源次数", "条数", len(st.socialGraph.callsOf("Relations")), 1)
	// 别的 mid 的布数据不参与本次判定。
	_, err = callRelations3(t, e, 70009, []int64{70002})
	wantNoErr(t, "Relations3（另一个 mid）", err)

	got := st.socialGraph.callsOf("Relations")
	if !reflect.DeepEqual(got[1], sgCall{method: "Relations", mid: 70009, list: []int64{70002}}) {
		t.Errorf("第二次的入参记账 = %+v, want {mid:70009 list:[70002]}", got[1])
	}
}

// TestRelations3OwnersArgFidelity nil / 空 / 重复 / 顺序 四种 owners 的入参保真。
// 字符串轨迹把 nil 与 []int64{} 都拼成 "70001"，只有 sgCall.list 能区分，
// 所以这里两类断言都要有：轨迹钉住「调用次数与形态」，记账钉住「参数原样」。
func TestRelations3OwnersArgFidelity(t *testing.T) {
	cases := []struct {
		name   string
		owners []int64
		// wantLen 是去重后的条目数；wantNilList 区分记账里该是 nil 还是空切片。
		wantLen     int
		wantNilList bool
	}{
		{"nil owners", nil, 0, true},
		{"空切片 owners", []int64{}, 0, false},
		{"重复 owners 被 map 合并", []int64{70002, 70002, 70003}, 2, false},
		{"顺序倒置仍逐项补齐", []int64{70003, 70002}, 2, false},
		{"含 0 值 owner", []int64{0}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstream(70001, Downstream{
				Relation: map[int64]bool{70002: true, 70003: true},
			}))
			st := e.st
			st.log.reset()

			reply, err := callRelations3(t, e, 70001, tc.owners)
			wantNoErr(t, "Relations3", err)
			if reply == nil || reply.GetRelations() == nil {
				t.Fatalf("relations = %v, want 非 nil（logic 的兜底分支本不该被走到）", reply.GetRelations())
			}
			wantEQ(t, "条目数 = owners 去重集大小", "len", len(reply.GetRelations()), tc.wantLen)
			for _, owner := range tc.owners {
				if owner == 70002 || owner == 70003 {
					if !followingOf(t, reply, owner) {
						t.Errorf("布了数据的 owner %d 应为 true", owner)
					}
					continue
				}
				if followingOf(t, reply, owner) {
					t.Errorf("没布数据的 owner %d 竟为 true", owner)
				}
			}
			got := st.socialGraph.onlyCall("Relations")
			want := sgCall{method: "Relations", mid: 70001, list: tc.owners}
			if tc.wantNilList {
				if got.list != nil {
					t.Errorf("nil owners 被换成了 %v，logic 没有原样传", got.list)
				}
				return
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("传给下游的入参 = %+v, want %+v", got, want)
			}
		})
	}
}

// TestRelations3DownstreamFaultStillPadsDefaults 下游故障时不报错、条目照样齐全，
// 但全部退化为 false（与 Relation3 的缺口 25 同形：错误与「确实没关注」不可区分）。
func TestRelations3DownstreamFaultStillPadsDefaults(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: social-graph 批量接口失败")
	e := newEnv(t, withDownstream(mid, Downstream{Relation: map[int64]bool{70002: true}}))
	st := e.st
	st.socialGraph.failWith("Relations", boom)
	st.log.reset()

	reply, err := callRelations3(t, e, mid, []int64{70002, 70003})
	wantNoErr(t, "故障路径不得把错误外传", err)
	wantEQ(t, "故障不减少条目", "len", len(reply.GetRelations()), 2)
	wantEQ(t, "true 被故障吞成 false", "70002", followingOf(t, reply, 70002), false)
	wantOps(t, "故障仍走完那一次回源", e.ops(0), []string{"socialGraph.Relations:7000170002,70003"})
	wantNotContains(t, "故障原文不得进报文", reply.String(), boom.Error())
}

// TestRelations3OtherMidDataNeverLeaks 布在别的 mid 上的关注关系不得串到这里：
// 替身对未布的 mid 回 (nil, nil)（适配器「查不到」口径），repository 逐项补 false。
func TestRelations3OtherMidDataNeverLeaks(t *testing.T) {
	e := newEnv(t, withDownstream(70001, Downstream{Relation: map[int64]bool{70002: true}}))
	st := e.st
	st.log.reset()

	reply, err := callRelations3(t, e, 88888, []int64{70002})
	wantNoErr(t, "Relations3（查无此 mid）", err)
	wantEQ(t, "查不到的 mid 只补默认值", "70002", followingOf(t, reply, 70002), false)
	wantEQ(t, "查不到也要回非 nil map 且只一项", "len", len(reply.GetRelations()), 1)
	wantOps(t, "仍然按 mid 问过下游", e.ops(0), []string{"socialGraph.Relations:8888870002"})
}

// TestRelations3NonNilGuaranteeLivesInRepository 归属保证：直接调 Repository 的
// 三种装配形状都得到非 nil relations，logic 的 `if reply.Relations == nil` 兜底
// （relations3logic.go:38-40）不可达。
func TestRelations3NonNilGuaranteeLivesInRepository(t *testing.T) {
	t.Run("未注入", func(t *testing.T) {
		e := newEnv(t, withoutSocialGraph())
		reply, err := e.st.repo.Relations(context.Background(), 70001, []int64{70002})
		wantNoErr(t, "Repository.Relations", err)
		if reply.GetRelations() == nil {
			t.Fatalf("Repository 回了 nil map")
		}
	})
	t.Run("下游返回 nil map", func(t *testing.T) {
		e := newEnv(t) // follows 未布 → (nil, errDownstreamWired)
		reply, err := e.st.repo.Relations(context.Background(), 70001, []int64{70002, 70003})
		wantNoErr(t, "Repository.Relations", err)
		if reply.GetRelations() == nil {
			t.Fatalf("Repository 回了 nil map")
		}
		wantEQ(t, "nil map 也补齐两条", "len", len(reply.GetRelations()), 2)
	})
	t.Run("已接线且成功", func(t *testing.T) {
		e := newEnv(t, withDownstream(70001, Downstream{Relation: map[int64]bool{70002: true}}))
		reply, err := e.st.repo.Relations(context.Background(), 70001, nil)
		wantNoErr(t, "Repository.Relations", err)
		if reply.GetRelations() == nil {
			t.Fatalf("Repository 回了 nil map")
		}
		wantEQ(t, "owners 为空则 map 为空", "len", len(reply.GetRelations()), 0)
	})
}
