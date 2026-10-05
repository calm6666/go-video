package logic

// richrelations3logic_test.go 覆盖 RichRelations3（logic/richrelations3logic.go:29-42 →
// repository.RichRelations internal/repository/relation.go:83-103 →
// SocialGraphClient.RichRelations）。
//
// 被测判定链：logic 把 (in.Owner, in.Mids) 交给 Repository（注意**首参是 owner**，
// 与 Relations3 的首参 mid 相反，串了就查不到东西）→ Repository 按 mids 建 map
// （relation.go:84）→ 问一次下游 → 逐个 mid 取值，取不到补 0。
//
// 钉住的事实：
//  1. 本方法在 logic 层不可能返回错误（relation.go:92-94 吞错），
//     richrelations3logic.go:31-34 与 38-40 都是死代码；
//  2. rich_relations 一定非 nil，键集恰为 mids 的**去重集**；
//     下游多给出的 mid（不在请求里）会被 relation.go:95 的循环丢掉，不透给客户端；
//  3. 「下游真给了 0」与「查不到补 0」在响应上不可区分（值域里没有第三种标记）；
//  4. 生产形状（socialGraph 未注入）恒为「全 0 但键集齐全」，且零调用；
//  5. owner=0 / mids 含 0 都不校验。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/account/rpc"
)

func callRichRelations3(t *testing.T, e *env, owner int64, mids []int64) (*rpc.RichRelationsReply, error) {
	t.Helper()
	return NewRichRelations3Logic(context.Background(), e.svcCtx).RichRelations3(
		&rpc.RichRelationReq{Owner: owner, Mids: mids, RealIp: "1.2.3.4"})
}

// TestRichRelations3ProductionShapePadsZero 未注入 social-graph：每个 mid 都有条目、
// 值恒为 0，且不触任何依赖。
func TestRichRelations3ProductionShapePadsZero(t *testing.T) {
	e := newEnv(t, withoutSocialGraph())
	st := e.st
	st.socialGraph.richs = map[int64]map[int64]int32{70001: {70002: 60}}
	st.log.reset()

	reply, err := callRichRelations3(t, e, 70001, []int64{70002, 70003})
	wantNoErr(t, "生产形状的 RichRelations3", err)
	if reply.GetRichRelations() == nil {
		t.Fatalf("rich_relations = nil, want 非 nil map")
	}
	wantEQ(t, "未注入也补齐两个条目", "len", len(reply.GetRichRelations()), 2)
	wantEQ(t, "布好的 60 在生产里读不到", "70002", reply.GetRichRelations()[70002], int32(0))
	wantEQ(t, "没布的 mid 同为 0", "70003", reply.GetRichRelations()[70003], int32(0))
	wantOps(t, "未注入不得触任何依赖", e.ops(0), nil)
	wantEQ(t, "关系族入参记账也必须为空", "条数", st.socialGraph.totalCalls(), 0)
}

// TestRichRelations3PassesValuesAndDropsUnrequestedMids 值原样透传；
// 下游多给的 mid（没被请求）不得出现在响应里。
func TestRichRelations3PassesValuesAndDropsUnrequestedMids(t *testing.T) {
	const owner = int64(70001)
	e := newEnv(t, withDownstream(owner, Downstream{Rich: map[int64]int32{
		70002: 60, 70003: 0, 999999: 100, // 999999 没被请求
	}}))
	st := e.st
	st.log.reset()

	reply, err := callRichRelations3(t, e, owner, []int64{70002, 70003, 70004})
	wantNoErr(t, "RichRelations3", err)
	wantEQ(t, "下游答的值必须透出", "70002", reply.GetRichRelations()[70002], int32(60))
	wantEQ(t, "未请求的 mid 不得漏进响应", "条目数", len(reply.GetRichRelations()), 3)
	if _, ok := reply.GetRichRelations()[999999]; ok {
		t.Errorf("响应里出现了没被请求的 999999：%v", reply.GetRichRelations())
	}
	// 70003（下游真给 0）、70004（查不到补 0）与「未注入」三者不可区分：
	// 只能靠调用轨迹证明实现确实问过、而不是整片兜 0。
	wantEQ(t, "下游真给的 0 与补的 0 不可区分", "70003", reply.GetRichRelations()[70003], int32(0))
	wantEQ(t, "补出来的 0", "70004", reply.GetRichRelations()[70004], int32(0))
	wantOps(t, "批量只回源一次", e.ops(0), []string{"socialGraph.RichRelations:7000170002,70003,70004"})
	got := st.socialGraph.onlyCall("RichRelations")
	if !reflect.DeepEqual(got, sgCall{method: "RichRelations", owner: owner, list: []int64{70002, 70003, 70004}}) {
		t.Errorf("传给下游的入参 = %+v", got)
	}
}

// TestRichRelations3OwnerIsFirstArg 首参是 owner 而不是 mid：
// 把关系布在 70002 名下、以 owner=70001 请求，只能拿到全 0。
// 这条与 Relations3（首参 mid）成对读，才看得出两个方法的首参语义**相反**。
func TestRichRelations3OwnerIsFirstArg(t *testing.T) {
	e := newEnv(t, withDownstream(70002, Downstream{Rich: map[int64]int32{70001: 60}}))
	st := e.st
	st.log.reset()

	reply, err := callRichRelations3(t, e, 70001, []int64{70001})
	wantNoErr(t, "RichRelations3", err)
	wantEQ(t, "布在别人 owner 名下的值取不到", "70001", reply.GetRichRelations()[70001], int32(0))
	wantOps(t, "下游按 owner=70001 被问过", e.ops(0), []string{"socialGraph.RichRelations:7000170001"})
}

// TestRichRelations3MidsArgFidelity nil / 空 / 重复 / 含 0 的 mids 入参保真
// （字符串轨迹无法区分 nil 与空切片，必须看结构化记账）。
func TestRichRelations3MidsArgFidelity(t *testing.T) {
	cases := []struct {
		name        string
		mids        []int64
		wantLen     int
		wantNilList bool
	}{
		{"nil mids", nil, 0, true},
		{"空切片 mids", []int64{}, 0, false},
		{"重复 mids 被 map 合并", []int64{70002, 70002}, 1, false},
		{"含 0 值 mid", []int64{0}, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstream(70001, Downstream{Rich: map[int64]int32{70002: 60, 0: 7}}))
			st := e.st
			st.log.reset()

			reply, err := callRichRelations3(t, e, 70001, tc.mids)
			wantNoErr(t, "RichRelations3", err)
			if reply.GetRichRelations() == nil {
				t.Fatalf("rich_relations = nil, want 非 nil")
			}
			wantEQ(t, "条目数 = mids 去重集大小", "len", len(reply.GetRichRelations()), tc.wantLen)
			for _, mid := range tc.mids {
				v, ok := reply.GetRichRelations()[mid]
				if !ok {
					t.Fatalf("mids 里的 %d 没有条目", mid)
				}
				if mid != 70002 {
					continue // 只有它布了非 0 值
				}
				wantEQ(t, "布好的值透出", itoa(mid), v, int32(60))
			}
			got := st.socialGraph.onlyCall("RichRelations")
			if tc.wantNilList {
				if got.list != nil {
					t.Errorf("nil mids 被换成了 %v", got.list)
				}
				return
			}
			if !reflect.DeepEqual(got, sgCall{method: "RichRelations", owner: 70001, list: tc.mids}) {
				t.Errorf("传给下游的入参 = %+v, want list %v", got, tc.mids)
			}
		})
	}
}

// TestRichRelations3DownstreamFaultStillPadsZero 故障被吞成全 0：
// 与「亲密度真的都是 0」不可区分（本批缺口 25 的富关系版）。
func TestRichRelations3DownstreamFaultStillPadsZero(t *testing.T) {
	const owner = int64(70001)
	boom := errors.New("account/test: social-graph 富关系失败")
	e := newEnv(t, withDownstream(owner, Downstream{Rich: map[int64]int32{70002: 60}}))
	st := e.st
	st.socialGraph.failWith("RichRelations", boom)
	st.log.reset()

	reply, err := callRichRelations3(t, e, owner, []int64{70002})
	wantNoErr(t, "故障不得外传", err)
	wantEQ(t, "故障不减少条目", "len", len(reply.GetRichRelations()), 1)
	wantEQ(t, "60 被吞成 0", "70002", reply.GetRichRelations()[70002], int32(0))
	wantOps(t, "故障仍走完那一次回源", e.ops(0), []string{"socialGraph.RichRelations:7000170002"})
	wantNotContains(t, "故障原文不得进报文", reply.String(), boom.Error())
}

// TestRichRelations3NonNilGuaranteeLivesInRepository 归属：非 nil map 与逐条补齐
// 都由 relation.go:84,87/97-100 完成，直调 Repository 三种装配形状都成立。
func TestRichRelations3NonNilGuaranteeLivesInRepository(t *testing.T) {
	t.Run("未注入", func(t *testing.T) {
		e := newEnv(t, withoutSocialGraph())
		reply, err := e.st.repo.RichRelations(context.Background(), 70001, []int64{70002})
		wantNoErr(t, "Repository.RichRelations", err)
		if reply.GetRichRelations() == nil {
			t.Fatalf("Repository 回了 nil map")
		}
	})
	t.Run("下游返回 nil map", func(t *testing.T) {
		e := newEnv(t) // richs 未布 → (nil, errDownstreamWired)
		reply, err := e.st.repo.RichRelations(context.Background(), 70001, []int64{70002, 70003})
		wantNoErr(t, "Repository.RichRelations", err)
		if reply.GetRichRelations() == nil {
			t.Fatalf("Repository 回了 nil map")
		}
		wantEQ(t, "nil map 也补齐两条", "len", len(reply.GetRichRelations()), 2)
	})
	t.Run("已接线且成功", func(t *testing.T) {
		e := newEnv(t, withDownstream(70001, Downstream{Rich: map[int64]int32{70002: 60}}))
		reply, err := e.st.repo.RichRelations(context.Background(), 70001, nil)
		wantNoErr(t, "Repository.RichRelations", err)
		if reply.GetRichRelations() == nil {
			t.Fatalf("Repository 回了 nil map")
		}
		wantEQ(t, "mids 为空则 map 为空", "len", len(reply.GetRichRelations()), 0)
	})
}
