package esclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
)

// AliasAction 别名变更动作（POST /_aliases 的 actions 元素）。
type AliasAction struct {
	Kind  string // add / remove
	Index string // 物理索引名
	Alias string // 别名
}

// MarshalJSON 输出 {"add":{"index":..,"alias":..}} 形式。
func (a AliasAction) MarshalJSON() ([]byte, error) {
	if a.Kind != "add" && a.Kind != "remove" {
		return nil, fmt.Errorf("esclient: invalid alias action kind %q", a.Kind)
	}
	if a.Index == "" || a.Alias == "" {
		return nil, fmt.Errorf("esclient: alias action %s requires index and alias", a.Kind)
	}
	return json.Marshal(map[string]interface{}{
		a.Kind: map[string]string{"index": a.Index, "alias": a.Alias},
	})
}

// SwitchPlan 别名切换计划。
type SwitchPlan struct {
	Actions       []AliasAction // 需要在一次 _aliases 调用中原子执行的动作
	PreviousIndex string        // 切换前指向（多指向时取字典序首个，用于日志与登记）
	Noop          bool          // true 表示已经是目标状态，无需请求
	Replaced      []string      // 被摘掉的索引
}

// PlanAliasSwitch 生成零停机别名切换计划并做乐观校验。
//
// 语义：
//   - current 是 OpenSearch 中别名当前真实指向（由 AliasTargets 读取）；
//   - expectedCurrent 非空时必须出现在 current 中，否则返回 ErrAliasMismatch，
//     避免两个操作者同时切换时后者把前者刚上线的索引静默摘掉；
//   - expectedCurrent 为空表示调用方接受「首次建立别名」（current 必须为空）；
//   - 目标已在 current 中且没有其它指向 → Noop=true；
//   - 生成的 actions 同时包含 remove 与 add，_aliases 单次调用是原子的，
//     因此查询侧（search-query）不会看到别名指向为空的瞬间。
func PlanAliasSwitch(alias, target string, current []string, expectedCurrent string) (*SwitchPlan, error) {
	if alias == "" || target == "" {
		return nil, fmt.Errorf("esclient: alias and target index are required")
	}

	dedup := make(map[string]struct{}, len(current))
	normalized := make([]string, 0, len(current))
	for _, c := range current {
		if c == "" {
			continue
		}
		if _, ok := dedup[c]; ok {
			continue
		}
		dedup[c] = struct{}{}
		normalized = append(normalized, c)
	}
	sort.Strings(normalized)

	plan := &SwitchPlan{}

	if expectedCurrent != "" {
		if _, ok := dedup[expectedCurrent]; !ok {
			return nil, fmt.Errorf("%w: expected=%s actual=%v", ErrAliasStateMismatch, expectedCurrent, normalized)
		}
	}
	if len(normalized) == 0 {
		if expectedCurrent != "" {
			return nil, fmt.Errorf("%w: expected=%s but alias has no index", ErrAliasStateMismatch, expectedCurrent)
		}
		plan.Actions = []AliasAction{{Kind: "add", Index: target, Alias: alias}}
		return plan, nil
	}

	if len(normalized) == 1 && normalized[0] == target {
		plan.Noop = true
		plan.PreviousIndex = target
		return plan, nil
	}

	plan.PreviousIndex = normalized[0]
	for _, idx := range normalized {
		if idx == target {
			continue
		}
		plan.Actions = append(plan.Actions, AliasAction{Kind: "remove", Index: idx, Alias: alias})
		plan.Replaced = append(plan.Replaced, idx)
	}
	// add 放最后：即使服务端按顺序执行，也不会出现别名短暂无指向。
	plan.Actions = append(plan.Actions, AliasAction{Kind: "add", Index: target, Alias: alias})
	return plan, nil
}

// ErrAliasStateMismatch 别名实际指向与 expected_current 不一致。
var ErrAliasStateMismatch = errors.New("esclient: alias state mismatch with expected_current")

// AliasTargets 读取别名指向的物理索引列表。
func (c *httpClient) AliasTargets(ctx context.Context, alias string) ([]string, error) {
	path := "/_alias/" + url.PathEscape(alias)
	var raw map[string]json.RawMessage
	if err := c.request(ctx, http.MethodGet, path, nil, nil, "", true, &raw); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, alias)
		}
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for k := range raw {
		// 响应里同时可能包含其它以点开头的系统条目，只保留真实索引名。
		if k == "" {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// ApplyAliasActions 原子执行别名变更。
// 注意：_aliases 不重试——重复执行 remove 会因指向已变而报错，交给上层重新读取状态。
func (c *httpClient) ApplyAliasActions(ctx context.Context, actions []AliasAction) error {
	if err := c.guardWrite(); err != nil {
		return err
	}
	if len(actions) == 0 {
		return nil
	}
	body, err := marshalNoHTMLEscape(map[string]interface{}{"actions": actions})
	if err != nil {
		return fmt.Errorf("esclient: marshal alias actions: %w", err)
	}
	return c.request(ctx, http.MethodPost, "/_aliases", nil, body, "application/json", false, nil)
}
