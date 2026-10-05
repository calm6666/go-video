package model

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateOverrideKeysAcceptsRegisteredKeys(t *testing.T) {
	cases := []string{
		"",
		"{}",
		`{"diversity_gap":3}`,
		`{"score_weights":{"pred_click":0.4,"pred_finish":0.6}}`,
		`{"source_quota":{"1":0.5,"2":0.5},"cold_start_boost":1.05}`,
		`{"frequency_cap":2}`,
	}
	for _, overrides := range cases {
		if err := ValidateOverrideKeys(overrides, MaxOverridesBytes); err != nil {
			t.Fatalf("ValidateOverrideKeys(%q) = %v, want nil", overrides, err)
		}
	}
}

// TestValidateOverrideKeysRejectsUnregisteredKeys 是 AGENTS.md §7 的守门测试：
// 「手工把某个 aid 推上去」「塞商业化字段」这类需求必须在写库前就被拒绝。
func TestValidateOverrideKeysRejectsUnregisteredKeys(t *testing.T) {
	cases := map[string]string{
		"指定 aid 置顶": `{"boost_aids":[123,456]}`,
		"指定黑名单 aid": `{"block_aids":[7]}`,
		"广告位":       `{"ad_slot":1}`,
		"付费转化目标":    `{"objective_weight":{"pred_payment":1}}`,
		"运营干预文案":    `{"force_reason":"运营要求"}`,
		"混合合法与非法":   `{"diversity_gap":3,"pin_aid":9}`,
		"顶层是数组":     `[{"diversity_gap":3}]`,
		"顶层是字符串":    `"diversity_gap=3"`,
	}
	for name, overrides := range cases {
		err := ValidateOverrideKeys(overrides, MaxOverridesBytes)
		if err == nil {
			t.Fatalf("%s: ValidateOverrideKeys(%q) = nil, want error", name, overrides)
		}
		if !errors.Is(err, ErrInvalidOverrideKey) {
			t.Fatalf("%s: ValidateOverrideKeys(%q) = %v, want ErrInvalidOverrideKey", name, overrides, err)
		}
	}
}

func TestValidateOverrideKeysReportsRejectedKeyName(t *testing.T) {
	err := ValidateOverrideKeys(`{"pin_aid":9,"boost_aids":[1],"diversity_gap":2}`, MaxOverridesBytes)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "boost_aids,pin_aid") {
		t.Fatalf("error must list rejected keys in stable order, got %q", msg)
	}
	if strings.Contains(msg, "diversity_gap") {
		t.Fatalf("error must not list accepted keys, got %q", msg)
	}
}

func TestValidateOverrideKeysEnforcesByteCeiling(t *testing.T) {
	// key 合法但体积超限：长度检查先于 JSON 解析，避免用超大报文拖住解析。
	padded := `{"diversity_gap":` + strings.Repeat("9", 64) + `}`
	if err := ValidateOverrideKeys(padded, 32); !errors.Is(err, ErrOverridesTooLarge) {
		t.Fatalf("ValidateOverrideKeys(oversized) = %v, want ErrOverridesTooLarge", err)
	}
	if err := ValidateOverrideKeys(padded, 0); err != nil {
		t.Fatalf("maxBytes<=0 表示只查 key，得 %v", err)
	}
}

func TestSupportedOverridesAreValidAndUnique(t *testing.T) {
	for key := range SupportedOverrides() {
		if !ValidOverrideKey(key) {
			t.Fatalf("SupportedOverrides 里的 %q 自相矛盾", key)
		}
		if strings.TrimSpace(key) != key || key == "" {
			t.Fatalf("override key %q 必须无首尾空白且非空", key)
		}
		if strings.ContainsAny(key, " ,:{}") {
			t.Fatalf("override key %q 含分隔符，无法用于审计展示", key)
		}
	}
	// 未登记 key 一律 false（含空串与 control 这类业务保留字）。
	for _, key := range []string{"", "control", "aid", "score_weight", "weights", "monetization"} {
		if ValidOverrideKey(key) {
			t.Fatalf("ValidOverrideKey(%q) = true, want false", key)
		}
	}
}
