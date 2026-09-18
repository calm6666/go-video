package flagvar

import (
	"flag"
	"reflect"
	"testing"
)

// TestSet 验证多次 Set 追加行为，并确认 String 输出按出现顺序连接。
func TestSet(t *testing.T) {
	var s StringVars
	if err := s.Set("a"); err != nil {
		t.Fatalf("set a: %v", err)
	}
	if err := s.Set("b"); err != nil {
		t.Fatalf("set b: %v", err)
	}
	if err := s.Set("c"); err != nil {
		t.Fatalf("set c: %v", err)
	}
	want := StringVars{"a", "b", "c"}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("after Set got %v, want %v", s, want)
	}
	if got := s.String(); got != "a,b,c" {
		t.Errorf("String() = %q, want %q", got, "a,b,c")
	}
}

// TestStringEmpty 验证空切片 String 输出为空字符串。
func TestStringEmpty(t *testing.T) {
	var s StringVars
	if got := s.String(); got != "" {
		t.Errorf("empty String() = %q, want empty", got)
	}
}

// TestStringSingle 验证单元素切片输出无多余分隔符。
func TestStringSingle(t *testing.T) {
	s := StringVars{"only"}
	if got := s.String(); got != "only" {
		t.Errorf("single String() = %q, want %q", got, "only")
	}
}

// TestFlagVar 验证通过 flag.Var 注册后解析命令行参数的端到端行为。
func TestFlagVar(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var s StringVars
	fs.Var(&s, "group", "group names")
	if err := fs.Parse([]string{"-group", "g1", "-group", "g2", "-group", "g3"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := StringVars{"g1", "g2", "g3"}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("flag var got %v, want %v", s, want)
	}
}

// TestFlagVarEmpty 验证不传 flag 时切片为空且 String 输出空字符串。
func TestFlagVarEmpty(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var s StringVars
	fs.Var(&s, "group", "group names")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s) != 0 {
		t.Errorf("expect empty slice, got %v", s)
	}
	if got := s.String(); got != "" {
		t.Errorf("empty String() = %q, want empty", got)
	}
}
