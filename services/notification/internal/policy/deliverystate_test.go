package policy

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/notification/model"
)

// 投递/事件状态机：非法迁移必须被拒，终态不得被复活（多实例并发下的事实源）。

func TestCanTransitDeliveryLegal(t *testing.T) {
	legal := [][2]int32{
		{model.DeliveryStatePending, model.DeliveryStateSent},
		{model.DeliveryStatePending, model.DeliveryStateRetry},
		{model.DeliveryStatePending, model.DeliveryStateFailed},
		{model.DeliveryStatePending, model.DeliveryStateDeadLetter},
		{model.DeliveryStatePending, model.DeliveryStateSuppressed},
		{model.DeliveryStateRetry, model.DeliveryStateSent},
		{model.DeliveryStateRetry, model.DeliveryStateRetry}, // 幂等重放
		{model.DeliveryStateRetry, model.DeliveryStateDeadLetter},
		{model.DeliveryStateDeadLetter, model.DeliveryStatePending}, // 运营 RetryDeadLetter
		// 同状态幂等只对 pending/retry 开放
		{model.DeliveryStatePending, model.DeliveryStatePending},
	}
	for _, tr := range legal {
		if !CanTransitDelivery(tr[0], tr[1]) {
			t.Errorf("合法迁移被拒: %d -> %d", tr[0], tr[1])
		}
	}
}

func TestCanTransitDeliveryIllegal(t *testing.T) {
	illegal := [][2]int32{
		{model.DeliveryStateSent, model.DeliveryStateRetry},      // 已受理不得回退
		{model.DeliveryStateSent, model.DeliveryStatePending},    // 终态不得复活 -> 重复推送
		{model.DeliveryStateSent, model.DeliveryStateSuppressed}, // 终态不得改写
		{model.DeliveryStateSent, model.DeliveryStateDeadLetter},
		{model.DeliveryStateFailed, model.DeliveryStateSent},
		{model.DeliveryStateSuppressed, model.DeliveryStateSent},    // 未调用供应商不得伪造成已发送
		{model.DeliveryStateSuppressed, model.DeliveryStatePending}, // 拦截即终态
		{model.DeliveryStateRetry, model.DeliveryStateSuppressed},   // 静默期由 held(retry) 表达
		{model.DeliveryStateDeadLetter, model.DeliveryStateRetry},   // 只能回 pending，由 Dispatch 推进
		{model.DeliveryStateSent, model.DeliveryStateSent},          // 终态同状态不属于幂等重放
	}
	for _, tr := range illegal {
		if CanTransitDelivery(tr[0], tr[1]) {
			t.Errorf("非法迁移被放行: %d -> %d", tr[0], tr[1])
		}
		if err := MustTransit(tr[0], tr[1], "DLV-test"); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("MustTransit(%d,%d) 应报 ErrIllegalTransition, got %v", tr[0], tr[1], err)
		}
	}
	// 合法迁移不得报错
	if err := MustTransit(model.DeliveryStatePending, model.DeliveryStateSent, "DLV-1"); err != nil {
		t.Errorf("合法迁移被 MustTransit 拒绝: %v", err)
	}
	if !strings.Contains(MustTransit(model.DeliveryStateSent, model.DeliveryStateRetry, "DLV-xyz").Error(), "DLV-xyz") {
		t.Error("错误信息缺少 delivery_id 上下文")
	}
}

// TestDeliverySourceStates：带守卫的 UPDATE 依赖这份源状态集合，
// 少了会漏更新、多了会覆盖终态。
func TestDeliverySourceStates(t *testing.T) {
	cases := []struct {
		to   int32
		want []int32
	}{
		{model.DeliveryStateSent, []int32{model.DeliveryStatePending, model.DeliveryStateRetry}},
		{model.DeliveryStateDeadLetter, []int32{model.DeliveryStatePending, model.DeliveryStateRetry}},
		{model.DeliveryStateSuppressed, []int32{model.DeliveryStatePending}},
		{model.DeliveryStatePending, []int32{model.DeliveryStateDeadLetter}},
		{model.DeliveryStateFailed, []int32{model.DeliveryStatePending, model.DeliveryStateRetry}},
	}
	for _, c := range cases {
		got := DeliverySourceStates(c.to)
		if len(got) != len(c.want) {
			t.Errorf("to=%d 源状态集 = %v, want %v", c.to, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("to=%d 源状态集 = %v, want %v", c.to, got, c.want)
			}
		}
		// 升序：便于拼 SQL。
		for i := 1; i < len(got); i++ {
			if got[i-1] >= got[i] {
				t.Errorf("源状态集未升序: %v", got)
			}
		}
		// 自反性：目标状态不得出现在自己的源状态集里（retry 的幂等重放除外，见下条断言）。
		if c.to != model.DeliveryStateRetry && stateIn(got, c.to) {
			t.Errorf("to=%d 的源状态集包含自身，终态可能被自己覆盖", c.to)
		}
	}
	// retry 允许从 retry 进入（退避重放与领取窗口）。
	if !stateIn(DeliverySourceStates(model.DeliveryStateRetry), model.DeliveryStateRetry) {
		t.Error("retry 必须允许从 retry 进入")
	}
	// sent/死信等终态永远不能从 sent 迁移出去。
	for _, to := range []int32{model.DeliveryStatePending, model.DeliveryStateRetry, model.DeliveryStateSuppressed} {
		if stateIn(DeliverySourceStates(to), model.DeliveryStateSent) {
			t.Errorf("to=%d 的源状态集包含 sent，会改写已受理回执", to)
		}
	}
}

func TestEventStateMachine(t *testing.T) {
	legal := [][2]int32{
		{model.EventStateReceived, model.EventStateProcessing},
		{model.EventStateReceived, model.EventStateDeadLetter},
		{model.EventStateProcessing, model.EventStateSucceeded},
		{model.EventStateProcessing, model.EventStateRetry},
		{model.EventStateProcessing, model.EventStateDeadLetter},
		{model.EventStateRetry, model.EventStateProcessing},
		{model.EventStateRetry, model.EventStateRetry},           // 再次排队
		{model.EventStateDeadLetter, model.EventStateProcessing}, // 运营重投
	}
	for _, tr := range legal {
		if !CanTransitEvent(tr[0], tr[1]) {
			t.Errorf("合法事件迁移被拒: %d -> %d", tr[0], tr[1])
		}
	}
	illegal := [][2]int32{
		{model.EventStateSucceeded, model.EventStateProcessing}, // 重复消息不得把成功改回处理中
		{model.EventStateSucceeded, model.EventStateRetry},
		{model.EventStateSucceeded, model.EventStateDeadLetter},
		{model.EventStateProcessing, model.EventStateReceived},
		{model.EventStateDeadLetter, model.EventStateSucceeded}, // 必须重走 processing
	}
	for _, tr := range illegal {
		if CanTransitEvent(tr[0], tr[1]) {
			t.Errorf("非法事件迁移被放行: %d -> %d", tr[0], tr[1])
		}
	}
	// succeeded 是终态：任何目标状态的源集合里都不该出现它。
	for _, to := range []int32{model.EventStateProcessing, model.EventStateRetry, model.EventStateDeadLetter} {
		if stateIn(EventSourceStates(to), model.EventStateSucceeded) {
			t.Errorf("to=%d 的源集合包含 succeeded", to)
		}
	}
	if !stateIn(EventSourceStates(model.EventStateProcessing), model.EventStateReceived) ||
		!stateIn(EventSourceStates(model.EventStateProcessing), model.EventStateRetry) {
		t.Errorf("processing 的源集合缺少 received/retry: %v", EventSourceStates(model.EventStateProcessing))
	}
}

// TestPriorityConstants：优先级常量与 rpc 枚举必须一致，
// 否则“高优先越过免打扰”会作用在错误的值上。
func TestPriorityConstants(t *testing.T) {
	if PriorityHigh != int32(3) || PriorityNormal != int32(2) || PriorityLow != int32(1) {
		t.Fatalf("优先级常量漂移: low=%d normal=%d high=%d", PriorityLow, PriorityNormal, PriorityHigh)
	}
	// 0 表示未指定（由调用方回落为普通），按注释属于合法值。
	if !IsValidPriority(0) || !IsValidPriority(PriorityLow) || !IsValidPriority(PriorityHigh) {
		t.Error("IsValidPriority 判定异常")
	}
	if IsValidPriority(4) || IsValidPriority(-1) {
		t.Error("越界优先级必须拒绝")
	}
}

func stateIn(states []int32, want int32) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}
