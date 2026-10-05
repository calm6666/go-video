package esclient

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestPlanAliasSwitch_ExpectedCurrentMismatch 是两个运维并发切换时的关键保护：
// 后者不能把前者刚上线的索引静默摘掉。
func TestPlanAliasSwitch_ExpectedCurrentMismatch(t *testing.T) {
	_, err := PlanAliasSwitch("go_video_content", "go_video_content_v1_200", []string{"go_video_content_v1_100"}, "go_video_content_v1_999")
	if !errors.Is(err, ErrAliasStateMismatch) {
		t.Fatalf("expected_current 不一致必须返回 ErrAliasStateMismatch，实际 %v", err)
	}
}

func TestPlanAliasSwitch_FirstMount(t *testing.T) {
	// 别名尚无指向：只接受 expected_current 为空的「首次建立」。
	plan, err := PlanAliasSwitch("go_video_content", "go_video_content_v1_1", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Noop {
		t.Fatal("首次挂载不是 noop")
	}
	if len(plan.Actions) != 1 || plan.Actions[0].Kind != "add" {
		t.Fatalf("首次挂载动作异常: %+v", plan.Actions)
	}

	if _, err := PlanAliasSwitch("go_video_content", "go_video_content_v1_1", nil, "go_video_content_v1_0"); err == nil {
		t.Fatal("别名无指向却给了 expected_current，必须拒绝")
	}
}

func TestPlanAliasSwitch_Noop(t *testing.T) {
	plan, err := PlanAliasSwitch("a", "a_v1_2", []string{"a_v1_2"}, "a_v1_2")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Noop || len(plan.Actions) != 0 {
		t.Fatalf("已指向目标时应 Noop 且无动作: %+v", plan)
	}
	if plan.PreviousIndex != "a_v1_2" {
		t.Fatalf("Noop 也要回填 PreviousIndex 供日志: %+v", plan)
	}
}

// TestPlanAliasSwitch_AddIsLastAction _aliases 单次调用内按顺序执行，
// remove 在前、add 在后才不会让查询侧看到别名空窗（零停机的核心）。
func TestPlanAliasSwitch_AddIsLastAction(t *testing.T) {
	current := []string{"a_v1_2", "a_v1_1"} // 故意乱序 + 多指向
	plan, err := PlanAliasSwitch("a", "a_v1_3", current, "a_v1_1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Noop {
		t.Fatal("需要切换，不应 Noop")
	}
	last := plan.Actions[len(plan.Actions)-1]
	if last.Kind != "add" || last.Index != "a_v1_3" {
		t.Fatalf("最后一个动作必须是 add 目标索引: %+v", plan.Actions)
	}
	for _, a := range plan.Actions[:len(plan.Actions)-1] {
		if a.Kind != "remove" {
			t.Fatalf("add 之前只允许 remove: %+v", plan.Actions)
		}
	}
	// 多指向时全部摘掉，并记入 Replaced。
	if len(plan.Replaced) != 2 {
		t.Fatalf("Replaced = %v, want 2 项", plan.Replaced)
	}
	if plan.PreviousIndex != "a_v1_1" {
		t.Fatalf("PreviousIndex 取字典序首个，实际 %q", plan.PreviousIndex)
	}
}

func TestPlanAliasSwitch_DedupAndEmptyEntries(t *testing.T) {
	plan, err := PlanAliasSwitch("a", "a_v2", []string{"a_v1", "", "a_v1", "a_v1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// 去重后只有 1 个指向 → 1 remove + 1 add。
	if len(plan.Actions) != 2 {
		t.Fatalf("动作数 = %d, want 2: %+v", len(plan.Actions), plan.Actions)
	}
}

func TestPlanAliasSwitch_RequiresAliasAndTarget(t *testing.T) {
	for _, c := range [][2]string{{"", "idx"}, {"alias", ""}} {
		if _, err := PlanAliasSwitch(c[0], c[1], nil, ""); err == nil {
			t.Fatalf("alias=%q target=%q 必须报错", c[0], c[1])
		}
	}
}

func TestAliasActionMarshalJSON(t *testing.T) {
	raw, err := json.Marshal(AliasAction{Kind: "add", Index: "i1", Alias: "a1"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"add":{"alias":"a1","index":"i1"}}` {
		t.Fatalf("动作体格式异常: %s", raw)
	}
	for _, a := range []AliasAction{
		{Kind: "replace", Index: "i", Alias: "a"},
		{Kind: "add", Index: "", Alias: "a"},
		{Kind: "add", Index: "i", Alias: ""},
	} {
		if _, err := json.Marshal(a); err == nil {
			t.Fatalf("非法动作必须报错: %+v", a)
		}
	}
}

// TestAliasActionsBody 复核实际发给 /_aliases 的整体结构：
// {"actions":[...]}，顺序即执行顺序。
func TestAliasActionsBody(t *testing.T) {
	plan, err := PlanAliasSwitch("a", "a_v2", []string{"a_v1"}, "a_v1")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := marshalNoHTMLEscape(map[string]interface{}{"actions": plan.Actions})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"actions":[{"remove":{"alias":"a","index":"a_v1"}},{"add":{"alias":"a","index":"a_v2"}}]}`
	if string(raw) != want {
		t.Fatalf("_aliases 请求体不符:\n got %s\nwant %s", raw, want)
	}
}
