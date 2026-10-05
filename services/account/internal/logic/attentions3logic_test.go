package logic

// attentions3logic_test.go 覆盖 Attentions3（logic/attentions3logic.go:29-42 →
// repository.Attentions internal/repository/relation.go:50-63 → SocialGraphClient.Attentions）。
//
// 被测判定链：logic 只交 in.Mid → Repository 在「未注入」时直接回空列表 →
// 「下游报错」时记日志后回空列表 → 下游回 nil 时兜成 []int64{} → 其余原样透传。
//
// 钉住的事实：
//  1. attentions 一定非 nil（attentions3logic.go:28 的注释兑现了），但兜底动作发生在
//     relation.go:52/57/60，logic 的 35-37 与 38-40 两个分支都不可达；
//  2. 本方法在 logic 层不可能返回错误；
//  3. 下游返回的顺序与重复**原样透出**（repository 不排序、不去重），
//     所以「关注列表按关注时间倒序」这类语义完全由 social-graph 负责，account 不校正；
//  4. 生产形状（socialGraph 未注入）恒为空列表且零调用；
//  5. mid=0 不校验。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/account/rpc"
)

func callAttentions3(t *testing.T, e *env, mid int64) (*rpc.AttentionsReply, error) {
	t.Helper()
	return NewAttentions3Logic(context.Background(), e.svcCtx).Attentions3(
		&rpc.MidReq{Mid: mid, RealIp: "1.2.3.4"})
}

// TestAttentions3ProductionShapeAlwaysEmpty 未注入 social-graph：恒空列表、零调用。
func TestAttentions3ProductionShapeAlwaysEmpty(t *testing.T) {
	e := newEnv(t, withoutSocialGraph())
	st := e.st
	st.socialGraph.attentions = map[int64][]int64{70001: {70002}}
	st.log.reset()

	reply, err := callAttentions3(t, e, 70001)
	wantNoErr(t, "生产形状的 Attentions3", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil")
	}
	if reply.GetAttentions() == nil {
		t.Fatalf("attentions = nil, want 非 nil 空切片（客户端要 range 它）")
	}
	wantEQ(t, "未注入即空列表", "len", len(reply.GetAttentions()), 0)
	wantOps(t, "未注入不得触任何依赖", e.ops(0), nil)
	wantEQ(t, "关系族入参记账也必须为空", "条数", st.socialGraph.totalCalls(), 0)
}

// TestAttentions3KeepsDownstreamOrderAndDuplicates 注入替身：原样透出，不排序不去重。
// 反向哨兵：把布数据顺序故意写成「非升序 + 含重复」，若实现做过排序/去重就会被发现。
func TestAttentions3KeepsDownstreamOrderAndDuplicates(t *testing.T) {
	const mid = int64(70001)
	want := []int64{90003, 70002, 90001, 70002} // 既逆序又含重复
	e := newEnv(t, withDownstream(mid, Downstream{Attentions: want}))
	st := e.st
	st.log.reset()

	reply, err := callAttentions3(t, e, mid)
	wantNoErr(t, "Attentions3", err)
	if !reflect.DeepEqual(reply.GetAttentions(), want) {
		t.Errorf("attentions = %v, want 原样 %v", reply.GetAttentions(), want)
	}
	wantOps(t, "只问下游一次", e.ops(0), []string{"socialGraph.Attentions:70001"})
	got := st.socialGraph.onlyCall("Attentions")
	if !reflect.DeepEqual(got, sgCall{method: "Attentions", mid: mid}) {
		t.Errorf("传给下游的入参 = %+v, want {mid:%d}", got, mid)
	}
	// 别的 mid 的数据不串味。
	st.log.reset()
	other, err := callAttentions3(t, e, 88888)
	wantNoErr(t, "Attentions3（查无此 mid）", err)
	if len(other.GetAttentions()) != 0 {
		t.Errorf("把 70001 的关注列表串到了 88888：%v", other.GetAttentions())
	}
	wantOps(t, "仍按 mid 问过下游", e.ops(0), []string{"socialGraph.Attentions:88888"})
}

// TestAttentions3NilDownstreamAnswerBecomesEmptyNotNil 下游「查不到」回 (nil, nil) 时，
// 非 nil 的保证住在 relation.go:59-61（直调 Repository 即可看到），不在 logic。
func TestAttentions3NilDownstreamAnswerBecomesEmptyNotNil(t *testing.T) {
	e := newEnv(t, withDownstream(70002, Downstream{Attentions: []int64{1}}))
	st := e.st
	st.log.reset()

	reply, err := callAttentions3(t, e, 70001) // 布数据的是别人，问的是空档 mid
	wantNoErr(t, "Attentions3", err)
	if reply.GetAttentions() == nil {
		t.Fatalf("attentions = nil，说明 repository 的 nil 兜底（relation.go:59-61）失效了")
	}
	wantEQ(t, "nil 答案兜成空列表", "len", len(reply.GetAttentions()), 0)
	wantOps(t, "仍走过那一次回源", e.ops(0), []string{"socialGraph.Attentions:70001"})

	direct, err := e.st.repo.Attentions(context.Background(), 70001)
	wantNoErr(t, "Repository.Attentions", err)
	if direct.GetAttentions() == nil {
		t.Fatalf("Repository 直接回 nil 列表，logic 的兜底分支其实可达")
	}
}

// TestAttentions3DownstreamFaultBecomesEmptyNotError 下游故障被吞成「没有关注」：
// 与「确实一个都没关注」在响应上完全同形（本批缺口 25 的批量版）。
func TestAttentions3DownstreamFaultBecomesEmptyNotError(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: social-graph 关注列表失败")
	e := newEnv(t, withDownstream(mid, Downstream{Attentions: []int64{70002, 70003}}))
	st := e.st
	st.socialGraph.failWith("Attentions", boom)
	st.log.reset()

	reply, err := callAttentions3(t, e, mid)
	wantNoErr(t, "故障不得外传", err)
	if reply.GetAttentions() == nil {
		t.Fatalf("故障路径的 attentions 竟然为 nil")
	}
	wantEQ(t, "布好的两条被吞成空", "len", len(reply.GetAttentions()), 0)
	wantOps(t, "故障仍走完那一次回源", e.ops(0), []string{"socialGraph.Attentions:70001"})
	wantNotContains(t, "故障原文不得进报文", reply.String(), boom.Error())
}

// TestAttentions3MidZeroNotValidated mid=0 不校验，且真的拿 0 去问下游、
// 缓存键与站点级匿名数据无关（这里只是把「不校验」钉住）。
func TestAttentions3MidZeroNotValidated(t *testing.T) {
	e := newEnv(t, withDownstream(0, Downstream{Attentions: []int64{70002}}))
	st := e.st
	st.log.reset()

	reply, err := callAttentions3(t, e, 0)
	wantNoErr(t, "Attentions3(mid=0)", err)
	wantEQ(t, "mid=0 也照样有答案", "len", len(reply.GetAttentions()), 1)
	wantOps(t, "下游按 mid=0 被问过", e.ops(0), []string{"socialGraph.Attentions:0"})
}
