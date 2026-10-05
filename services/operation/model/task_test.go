package model

// 任务/步骤状态机的纯逻辑用例（对应 AGENTS.md §8「状态只能通过合法迁移推进」）。
// 期望值在测试里手写成表，而不是从 taskTransitions 反推，
// 这样实现表一旦被放宽（例如给终态加出边）就会失败。

import "testing"

// 手写状态机规格：from -> 允许迁移到的状态集合。
var wantTaskTransitions = map[string][]string{
	TaskStatePending:   {TaskStateRunning, TaskStateCanceled},
	TaskStateRunning:   {TaskStateSucceeded, TaskStatePartial, TaskStateFailed, TaskStateCanceled},
	TaskStateSucceeded: nil,
	TaskStatePartial:   nil,
	TaskStateFailed:    nil,
	TaskStateCanceled:  nil,
}

var wantStepTransitions = map[string][]string{
	StepStatePending:   {StepStateRunning, StepStateCanceled, StepStateFailed},
	StepStateRunning:   {StepStateSucceeded, StepStateFailed, StepStateCanceled},
	StepStateSucceeded: nil,
	StepStateFailed:    nil,
	StepStateCanceled:  nil,
}

func allTaskStates() []string {
	out := make([]string, 0, len(wantTaskTransitions))
	for s := range wantTaskTransitions {
		out = append(out, s)
	}
	return out
}

func allStepStates() []string {
	out := make([]string, 0, len(wantStepTransitions))
	for s := range wantStepTransitions {
		out = append(out, s)
	}
	return out
}

func assertTransitionTable(t *testing.T, name string,
	spec map[string][]string, states []string, can func(from, to string) bool,
) {
	t.Helper()
	known := map[string]bool{}
	for _, s := range states {
		known[s] = true
	}
	for _, from := range states {
		allowed := map[string]bool{}
		for _, to := range spec[from] {
			allowed[to] = true
		}
		for _, to := range states {
			got := can(from, to)
			if want := allowed[to]; got != want {
				t.Fatalf("%s transition %s -> %s = %v, want %v", name, from, to, got, want)
			}
		}
		// 自迁移一律非法：重复提交同一状态说明推进者丢了互斥，必须放弃而不是覆盖。
		if can(from, from) {
			t.Fatalf("%s %s -> %s must be rejected", name, from, from)
		}
	}
	// 未知/脏数据状态（含空串与大小写差异）既不能作为起点，也不能作为终点。
	for _, bad := range []string{"", " unknown ", "PENDING", "pending "} {
		if known[bad] {
			continue
		}
		for _, s := range states {
			if can(bad, s) {
				t.Fatalf("%s from unknown state %q to %q must be rejected", name, bad, s)
			}
			if can(s, bad) {
				t.Fatalf("%s from %q to unknown state %q must be rejected", name, s, bad)
			}
		}
	}
}

func TestTaskStateMachineMatrix(t *testing.T) {
	assertTransitionTable(t, "task", wantTaskTransitions, allTaskStates(), CanTaskTransition)
}

func TestStepStateMachineMatrix(t *testing.T) {
	assertTransitionTable(t, "step", wantStepTransitions, allStepStates(), CanStepTransition)
}

func TestTaskFinalStateHasNoOutgoingEdge(t *testing.T) {
	for _, s := range allTaskStates() {
		final := IsTaskFinalState(s)
		if want := len(wantTaskTransitions[s]) == 0; final != want {
			t.Fatalf("IsTaskFinalState(%s) = %v, but out-degree %d", s, final, len(wantTaskTransitions[s]))
		}
		if final {
			for _, to := range allTaskStates() {
				if CanTaskTransition(s, to) {
					t.Fatalf("final task state %s must not move to %s", s, to)
				}
			}
		}
	}
	// 脏状态既非终态也不可迁移：调用方据此拒绝写入，不会把未知值当 pending。
	if IsTaskFinalState("weird") || CanTaskTransition("weird", TaskStateCanceled) {
		t.Fatal("unknown state must be neither final nor transitable")
	}
	if IsTaskFinalState(TaskStatePending) || IsTaskFinalState(TaskStateRunning) {
		t.Fatal("pending/running are not final states")
	}
}

func TestResolveTaskFinalState(t *testing.T) {
	cases := []struct {
		name                     string
		total, succeeded, failed int32
		want                     string
	}{
		{"all ok", 3, 3, 0, TaskStateSucceeded},
		{"all failed", 3, 0, 3, TaskStateFailed},
		{"mixed", 3, 2, 1, TaskStatePartial},
		{"single ok", 1, 1, 0, TaskStateSucceeded},
		{"single failed", 1, 0, 1, TaskStateFailed},
		// 有步骤被取消时 succeeded+failed < total：不能谎报全绿，收敛为 partial。
		{"canceled steps leave gap", 4, 2, 1, TaskStatePartial},
		{"no failure but not all done", 4, 3, 0, TaskStatePartial},
		{"all canceled", 2, 0, 0, TaskStatePartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveTaskFinalState(c.total, c.succeeded, c.failed); got != c.want {
				t.Fatalf("ResolveTaskFinalState(%d,%d,%d) = %s, want %s", c.total, c.succeeded, c.failed, got, c.want)
			}
		})
	}
	// 推导结果必须是终态且可从 running 抵达，否则 settleTask 会卡在无法收敛。
	for _, c := range cases {
		final := ResolveTaskFinalState(c.total, c.succeeded, c.failed)
		if !IsTaskFinalState(final) {
			t.Fatalf("%s is not a final state", final)
		}
		if !CanTaskTransition(TaskStateRunning, final) {
			t.Fatalf("running -> %s must be a legal transition", final)
		}
	}
}

func TestValidTaskType(t *testing.T) {
	for _, ok := range []string{
		TaskTypeBatchOfflineSubmission, TaskTypeBatchOfflineEpisode,
		TaskTypeBatchExpireWindow, TaskTypeBatchRejectAppeal,
	} {
		if !ValidTaskType(ok) {
			t.Fatalf("task type %q must be accepted", ok)
		}
	}
	// 未知类型（含大小写、前缀相近、空串）必须拒绝：它们没有映射的下游 RPC。
	for _, bad := range []string{"", "batch_offline", "BATCH_OFFLINE_SUBMISSION", "batch_pay_refund", "batch_offline_submission "} {
		if ValidTaskType(bad) {
			t.Fatalf("task type %q must be rejected", bad)
		}
	}
}
