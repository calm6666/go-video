package policy

import (
	"testing"

	"go-video/services/danmaku/model"
)

// TestCanTransitionLegalTable 逐条校验合法迁移表。
// 期望值在这里显式重写一遍，是为了让「改表必须同时改测试」，
// 避免误删迁移路径后测试跟着实现一起漂移。
func TestCanTransitionLegalTable(t *testing.T) {
	legal := [][2]int32{
		{model.StatePending, model.StateNormal},
		{model.StatePending, model.StateFolded},
		{model.StatePending, model.StateRejected},
		{model.StatePending, model.StateDeleted},
		{model.StateNormal, model.StatePending},
		{model.StateNormal, model.StateFolded},
		{model.StateNormal, model.StateDeleted},
		{model.StateNormal, model.StateRejected}, // 机审放行后人审驳回：直接拉回屏蔽池
		{model.StateFolded, model.StatePending},
		{model.StateFolded, model.StateNormal},
		{model.StateFolded, model.StateRejected},
		{model.StateFolded, model.StateDeleted},
		{model.StateRejected, model.StatePending},
		{model.StateRejected, model.StateNormal},
		{model.StateRejected, model.StateDeleted},
	}
	for _, tr := range legal {
		if !CanTransition(tr[0], tr[1]) {
			t.Fatalf("迁移 %d → %d 应为合法", tr[0], tr[1])
		}
	}
}

func TestCanTransitionIllegal(t *testing.T) {
	illegal := [][2]int32{
		{model.StateDeleted, model.StateNormal},  // 已删除是终态，不得复活
		{model.StateDeleted, model.StatePending}, // 已删除是终态
		{model.StateDeleted, model.StateDeleted},
		{model.StateRejected, model.StateFolded},    // 驳回态只能经复核回到待审/正常，不能直接折叠
		{model.StatePending, model.StatePending},    // 同状态自迁移交给调用方幂等处理
		{model.StateNormal, model.StateNormal},      // 同上：正常态重复下发 PASS 不做迁移
		{model.StateFolded, model.StateFolded},      // 折叠态重复 REVIEW 不迁移
		{model.StateRejected, model.StateRejected},  // 驳回态重复 REJECT 不迁移
		{-1, model.StateNormal},                     // 未知起始状态
		{model.StateNormal, 99},                     // 未知目标状态
		{model.StateNormal, model.StatePending + 4}, // 越界状态
	}
	for _, tr := range illegal {
		if CanTransition(tr[0], tr[1]) {
			t.Fatalf("迁移 %d → %d 应为非法", tr[0], tr[1])
		}
	}
}

// TestDeletedIsTerminal 已删除弹幕不得迁出到任何状态（AGENTS.md §8 审计要求）。
func TestDeletedIsTerminal(t *testing.T) {
	for _, to := range States() {
		if CanTransition(model.StateDeleted, to) {
			t.Fatalf("StateDeleted 必须是终态，却允许迁移到 %d", to)
		}
	}
}

// TestStatesCoversTable States 必须与迁移表 key 集合一致，
// 否则新增状态会绕开审核判定。
func TestStatesCoversTable(t *testing.T) {
	seen := make(map[int32]bool, len(States()))
	for _, s := range States() {
		if seen[s] {
			t.Fatalf("States 返回重复状态 %d", s)
		}
		seen[s] = true
		if _, ok := legalTransitions[s]; !ok {
			t.Fatalf("状态 %d 缺少迁移表条目", s)
		}
	}
	if len(seen) != len(legalTransitions) {
		t.Fatalf("States 数量 %d 与迁移表 %d 不一致", len(seen), len(legalTransitions))
	}
}

func TestTargetForVerdict(t *testing.T) {
	cases := []struct {
		name      string
		verdict   int32
		wantState int32
		wantPool  int32
		wantOK    bool
	}{
		{"通过", VerdictPass, model.StateNormal, model.PoolNormal, true},
		{"转人审", VerdictReview, model.StatePending, model.PoolReview, true},
		{"拒绝", VerdictReject, model.StateRejected, model.PoolBlock, true},
		{"未指定", 0, 0, 0, false},
		{"未知结论", 42, 0, 0, false},
		{"负值", -1, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, p, ok := TargetForVerdict(c.verdict)
			if ok != c.wantOK || s != c.wantState || p != c.wantPool {
				t.Fatalf("TargetForVerdict(%d) = (%d, %d, %v), want (%d, %d, %v)",
					c.verdict, s, p, ok, c.wantState, c.wantPool, c.wantOK)
			}
		})
	}
}

// TestOnlyPassMakesPublic 机审/人审结论里只有 PASS 能把弹幕放进普通池，
// 这是 AGENTS.md §8「机审结论落库前不得对所有人可见」的硬约束。
func TestOnlyPassMakesPublic(t *testing.T) {
	for _, v := range []int32{0, VerdictReview, VerdictReject, 99} {
		s, p, ok := TargetForVerdict(v)
		if ok && s == model.StateNormal && p == model.PoolNormal {
			t.Fatalf("结论 %d 不应让弹幕对所有人可见", v)
		}
	}
}

// TestVerdictTargetsAreReachable 合法结论映射出的目标必须能从弹幕当前态到达，
// 否则 ApplyModerationResult 会永久卡在非法迁移上。
func TestVerdictTargetsAreReachable(t *testing.T) {
	for _, from := range []int32{model.StatePending, model.StateFolded, model.StateNormal} {
		for _, v := range []int32{VerdictPass, VerdictReview, VerdictReject} {
			to, _, ok := TargetForVerdict(v)
			if !ok {
				t.Fatalf("verdict %d 应合法", v)
			}
			if from == to {
				continue // 同状态由 logic 按幂等处理
			}
			if !CanTransition(from, to) {
				t.Fatalf("审核结论 %d 从状态 %d 无法落到 %d", v, from, to)
			}
		}
	}
}

func TestInitialState(t *testing.T) {
	cases := []struct {
		name       string
		blocked    bool
		machineRev bool
		wantState  int32
		wantPool   int32
	}{
		{"命中屏蔽词：折叠进屏蔽池", true, true, model.StateFolded, model.PoolBlock},
		{"命中屏蔽词且机审关闭：仍不进普通池", true, false, model.StateFolded, model.PoolBlock},
		{"未命中且机审开启：待审进审核池", false, true, model.StatePending, model.PoolReview},
		{"未命中且机审关闭：直接正常池", false, false, model.StateNormal, model.PoolNormal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, p := InitialState(c.blocked, c.machineRev)
			if s != c.wantState || p != c.wantPool {
				t.Fatalf("InitialState(%v, %v) = (%d, %d), want (%d, %d)",
					c.blocked, c.machineRev, s, p, c.wantState, c.wantPool)
			}
			// §8 门禁：除「机审关闭 + 未命中」外，落库初始态一律不可对所有人可见。
			public := s == model.StateNormal && p == model.PoolNormal
			if public == (c.blocked || c.machineRev) {
				t.Fatalf("InitialState(%v, %v) 可见性与 §8 门禁不一致", c.blocked, c.machineRev)
			}
		})
	}
}
