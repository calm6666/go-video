package logic

// blacks3logic_test.go 覆盖 Blacks3（logic/blacks3logic.go:29-42 →
// repository.Blacks internal/repository/relation.go:66-79 → SocialGraphClient.Blacks）。
//
// 被测判定链：logic 只交 in.Mid → Repository 在「未注入」时回空 map →
// 「下游报错」时记日志后回空 map → 下游回 nil map 时兜成 map[int64]bool{} → 其余原样透传。
//
// 钉住的事实：
//  1. black_list 一定非 nil（blacks3logic.go:28 的注释兑现了），保证住在
//     relation.go:68/73/76，logic 的 35-37 与 38-40 两个分支都不可达；
//  2. 本方法在 logic 层不可能返回错误；
//  3. 下游 map 的**键与值都原样透出**，repository 不做任何过滤 ——
//     所以「被拉黑者是否也被记成 false」这种脏值会一路带到客户端；
//  4. 生产形状（socialGraph 未注入）恒为空 map 且零调用；
//  5. mid 不校验（含 0）。
//
// 与 Attentions3 的差别只有一处需要单独钉：这里返回 map，所以「空」与「nil」
// 在 JSON/protobuf 上都会渲染成 `{}`，但 Go 侧 range 与 len 行为不同，
// 因此用例显式断言 GetBlackList() != nil。

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go-video/services/account/rpc"
)

func callBlacks3(t *testing.T, e *env, mid int64) (*rpc.BlacksReply, error) {
	t.Helper()
	return NewBlacks3Logic(context.Background(), e.svcCtx).Blacks3(
		&rpc.MidReq{Mid: mid, RealIp: "1.2.3.4"})
}

// TestBlacks3ProductionShapeAlwaysEmpty 未注入 social-graph：恒空 map、零调用。
func TestBlacks3ProductionShapeAlwaysEmpty(t *testing.T) {
	e := newEnv(t, withoutSocialGraph())
	st := e.st
	st.socialGraph.blacks = map[int64]map[int64]bool{70001: {70002: true}}
	st.log.reset()

	reply, err := callBlacks3(t, e, 70001)
	wantNoErr(t, "生产形状的 Blacks3", err)
	if reply == nil {
		t.Fatalf("reply = nil, want 非 nil")
	}
	if reply.GetBlackList() == nil {
		t.Fatalf("black_list = nil, want 非 nil 空 map")
	}
	wantEQ(t, "未注入即空 map", "len", len(reply.GetBlackList()), 0)
	wantOps(t, "未注入不得触任何依赖", e.ops(0), nil)
	wantEQ(t, "关系族入参记账也必须为空", "条数", st.socialGraph.totalCalls(), 0)
}

// TestBlacks3PassesThroughMapIncludingFalseValues 原样透传，含值=false 的脏条目。
func TestBlacks3PassesThroughMapIncludingFalseValues(t *testing.T) {
	const mid = int64(70001)
	want := map[int64]bool{70002: true, 70003: false} // 70003=false 是下游脏值
	e := newEnv(t, withDownstream(mid, Downstream{Blacks: want}))
	st := e.st
	st.log.reset()

	reply, err := callBlacks3(t, e, mid)
	wantNoErr(t, "Blacks3", err)
	if !reflect.DeepEqual(reply.GetBlackList(), want) {
		t.Errorf("black_list = %v, want 原样 %v（repository 不该过滤 false 条目）", reply.GetBlackList(), want)
	}
	// 值=false 的条目仍然带着自己那条键：客户端 `if _, ok := m[x]; ok` 会把它当「在黑名单里」。
	if _, ok := reply.GetBlackList()[70003]; !ok {
		t.Errorf("false 条目被丢掉了，与 repository/relation.go:78 的原样透传不符")
	}
	wantOps(t, "只问下游一次", e.ops(0), []string{"socialGraph.Blacks:70001"})
	got := st.socialGraph.onlyCall("Blacks")
	if !reflect.DeepEqual(got, sgCall{method: "Blacks", mid: mid}) {
		t.Errorf("传给下游的入参 = %+v, want {mid:%d}", got, mid)
	}
}

// TestBlacks3NilDownstreamAnswerBecomesEmptyNotNil 下游「查不到」回 (nil, nil) 时
// 兜成非 nil 空 map，且这件事由 relation.go:75-77 做（直调 Repository 复现）。
func TestBlacks3NilDownstreamAnswerBecomesEmptyNotNil(t *testing.T) {
	e := newEnv(t, withDownstream(70002, Downstream{Blacks: map[int64]bool{1: true}}))
	st := e.st
	st.log.reset()

	reply, err := callBlacks3(t, e, 70001)
	wantNoErr(t, "Blacks3", err)
	if reply.GetBlackList() == nil {
		t.Fatalf("black_list = nil，说明 relation.go:75-77 的 nil 兜底失效了")
	}
	wantEQ(t, "nil 答案兜成空 map", "len", len(reply.GetBlackList()), 0)
	wantOps(t, "仍走过那一次回源", e.ops(0), []string{"socialGraph.Blacks:70001"})

	direct, err := e.st.repo.Blacks(context.Background(), 70001)
	wantNoErr(t, "Repository.Blacks", err)
	if direct.GetBlackList() == nil {
		t.Fatalf("Repository 直接回 nil map，logic 的兜底分支其实可达")
	}
}

// TestBlacks3DownstreamFaultBecomesEmptyNotError 故障被吞成「没有黑名单」，
// 与「确实一个都没拉黑」同形（本批缺口 25 的黑名单版）。
//
// TODO(缺陷)（本批缺口 26）：黑名单是**隐私/安全**开关（隐藏动态、禁止来访），
// relation.go:70-74 把下游故障降级成空 map，等于在 social-graph 抖动期间
// 对所有拉黑者临时解除屏蔽，且调用方拿不到任何失败信号。此处钉住当前行为。
func TestBlacks3DownstreamFaultBecomesEmptyNotError(t *testing.T) {
	const mid = int64(70001)
	boom := errors.New("account/test: social-graph 黑名单失败")
	e := newEnv(t, withDownstream(mid, Downstream{Blacks: map[int64]bool{70002: true}}))
	st := e.st
	st.socialGraph.failWith("Blacks", boom)
	st.log.reset()

	reply, err := callBlacks3(t, e, mid)
	wantNoErr(t, "故障不得外传", err)
	if reply.GetBlackList() == nil {
		t.Fatalf("故障路径的 black_list 竟然为 nil")
	}
	wantEQ(t, "布好的一条被吞成空", "len", len(reply.GetBlackList()), 0)
	wantOps(t, "故障仍走完那一次回源", e.ops(0), []string{"socialGraph.Blacks:70001"})
	wantNotContains(t, "故障原文不得进报文", reply.String(), boom.Error())
}

// TestBlacks3MidZeroNotValidated mid=0 不校验，仍会去问下游。
func TestBlacks3MidZeroNotValidated(t *testing.T) {
	e := newEnv(t, withDownstream(0, Downstream{Blacks: map[int64]bool{70002: true}}))
	st := e.st
	st.log.reset()

	reply, err := callBlacks3(t, e, 0)
	wantNoErr(t, "Blacks3(mid=0)", err)
	wantEQ(t, "mid=0 也照样有答案", "len", len(reply.GetBlackList()), 1)
	wantOps(t, "下游按 mid=0 被问过", e.ops(0), []string{"socialGraph.Blacks:0"})
}
