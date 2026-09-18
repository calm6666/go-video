package validation

import (
	"strings"
	"testing"
)

func TestNormalizePage(t *testing.T) {
	cases := []struct {
		page, pageSize, max int
		wantPage, wantSize  int
	}{
		{0, 0, 50, 1, 20},
		{-1, -5, 50, 1, 20},
		{1, 10, 50, 1, 10},
		{3, 100, 50, 3, 50}, // 截断到 max
		{2, 30, 0, 2, 30},   // max=0 回退到 50；30 保持不变
		{2, 60, 0, 2, 50},   // max=0 回退到 50；60 截断到 50
	}
	for _, c := range cases {
		got := NormalizePage(c.page, c.pageSize, c.max)
		if got.Page != c.wantPage || got.PageSize != c.wantSize {
			t.Errorf("NormalizePage(%d,%d,%d) = %+v, want page=%d size=%d", c.page, c.pageSize, c.max, got, c.wantPage, c.wantSize)
		}
	}
}

func TestPageOffset(t *testing.T) {
	p := NormalizePage(3, 20, 50)
	if p.Offset() != 40 {
		t.Errorf("Offset = %d, want 40", p.Offset())
	}
	if (NormalizePage(0, 0, 50)).Offset() != 0 {
		t.Error("page 1 offset should be 0")
	}
}

func TestNormalizeCursor(t *testing.T) {
	c := NormalizeCursor("", 0, 100)
	if c.Limit != 20 {
		t.Errorf("default limit = %d, want 20", c.Limit)
	}
	c = NormalizeCursor("abc", 500, 100)
	if c.Limit != 100 {
		t.Errorf("clamped limit = %d, want 100", c.Limit)
	}
	c = NormalizeCursor("abc", 50, 0)
	if c.Limit != 50 {
		t.Errorf("maxLimit=0 should fall back to 100; 50 stays 50, got %d", c.Limit)
	}
	if c.Value != "abc" {
		t.Errorf("cursor value lost: %q", c.Value)
	}
}

func TestValidateStringLength(t *testing.T) {
	cases := []struct {
		s        string
		min, max int
		wantErr  bool
	}{
		{"hello", 1, 10, false},
		{"hello", 6, 10, true},
		{"hello", 1, 4, true},
		{"你好世界", 1, 4, false}, // 4 个 rune
		{"你好世界", 1, 3, true},  // 4 个 rune 超过 3
		{"", 0, 0, false},     // 无限制
	}
	for _, c := range cases {
		err := ValidateStringLength(c.s, c.min, c.max)
		if c.wantErr && err == nil {
			t.Errorf("ValidateStringLength(%q, %d, %d) expected error", c.s, c.min, c.max)
		}
		if !c.wantErr && err != nil {
			t.Errorf("ValidateStringLength(%q, %d, %d) unexpected error: %v", c.s, c.min, c.max, err)
		}
	}
}

func TestValidateByteLength(t *testing.T) {
	if err := ValidateByteLength("hello", 1, 10); err != nil {
		t.Fatal(err)
	}
	// "你好" 在 UTF-8 中占 6 字节。
	if err := ValidateByteLength("你好", 1, 5); err == nil {
		t.Error("expected byte length error for 6-byte string with max 5")
	}
}

func TestValidateEnum(t *testing.T) {
	allowed := []string{"newest", "oldest", "hottest"}
	if err := ValidateEnum("newest", allowed); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnum("Newest", allowed); err == nil {
		t.Error("case-sensitive: Newest should not match newest")
	}
	err := ValidateEnum("unknown", allowed)
	if err == nil {
		t.Fatal("expected error for unknown enum value")
	}
	if !strings.Contains(err.Error(), "unknown") || !strings.Contains(err.Error(), "newest") {
		t.Errorf("error message should include value and allowed set, got %q", err.Error())
	}
}

func TestValidateSliceLength(t *testing.T) {
	if err := ValidateSliceLength([]int{1, 2, 3}, 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSliceLength([]int{}, 1, 5); err == nil {
		t.Error("empty slice below min should fail")
	}
	if err := ValidateSliceLength([]int{1, 2, 3, 4, 5, 6}, 1, 5); err == nil {
		t.Error("slice over max should fail")
	}
	if err := ValidateSliceLength("hello", 1, 5); err != nil {
		t.Errorf("string should be sliceable, got %v", err)
	}
	if err := ValidateSliceLength(42, 1, 5); err == nil {
		t.Error("int should not be sliceable")
	}
	if err := ValidateSliceLength(nil, 1, 5); err == nil {
		t.Error("nil should fail")
	}
}

func TestValidateMapSize(t *testing.T) {
	m := map[string]any{"a": 1, "b": 2}
	if err := ValidateMapSize(m, 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMapSize(m, 3, 5); err == nil {
		t.Error("map below min should fail")
	}
	if err := ValidateMapSize(m, 1, 1); err == nil {
		t.Error("map over max should fail")
	}
}

func TestValidateNonEmptyString(t *testing.T) {
	if err := ValidateNonEmptyString("title", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNonEmptyString("title", "   "); err == nil {
		t.Error("whitespace-only should fail")
	}
	err := ValidateNonEmptyString("title", "")
	if err == nil {
		t.Fatal("expected error for empty")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("error should mention field name, got %q", err.Error())
	}
}

func TestValidatePositiveInt(t *testing.T) {
	if err := ValidatePositiveInt("count", 1); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []int64{0, -1, -100} {
		if err := ValidatePositiveInt("count", bad); err == nil {
			t.Errorf("expected error for count=%d", bad)
		}
	}
}

func TestValidateRange(t *testing.T) {
	cases := []struct {
		value, min, max int64
		wantErr         bool
	}{
		{5, 1, 10, false},
		{0, 1, 10, true},
		{11, 1, 10, true},
		{1, 1, 1, false},
		{5, 10, 1, true}, // 非法范围（min > max）
	}
	for _, c := range cases {
		err := ValidateRange("age", c.value, c.min, c.max)
		if c.wantErr && err == nil {
			t.Errorf("ValidateRange(%d, %d, %d) expected error", c.value, c.min, c.max)
		}
		if !c.wantErr && err != nil {
			t.Errorf("ValidateRange(%d, %d, %d) unexpected error: %v", c.value, c.min, c.max, err)
		}
	}
}
