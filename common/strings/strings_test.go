package strings

import (
	"errors"
	"strconv"
	"testing"
)

func TestJoinInts(t *testing.T) {
	cases := []struct {
		in   []int64
		want string
	}{
		{nil, ""},
		{[]int64{}, ""},
		{[]int64{1}, "1"},
		{[]int64{1, 2, 3}, "1,2,3"},
		{[]int64{-1, 0, 9223372036854775807}, "-1,0,9223372036854775807"},
	}
	for _, c := range cases {
		if got := JoinInts(c.in); got != c.want {
			t.Errorf("JoinInts(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitInts(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, err := SplitInts("")
		if err != nil || got != nil {
			t.Fatalf("got %v, err %v, want nil/nil", got, err)
		}
	})
	t.Run("single", func(t *testing.T) {
		got, err := SplitInts("42")
		if err != nil || len(got) != 1 || got[0] != 42 {
			t.Fatalf("got %v, err %v", got, err)
		}
	})
	t.Run("multi", func(t *testing.T) {
		got, err := SplitInts("1,2,3")
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{1, 2, 3}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})
	t.Run("invalid", func(t *testing.T) {
		_, err := SplitInts("1,abc,3")
		if err == nil {
			t.Fatal("expected error for invalid token, got nil")
		}
		// strconv.ParseInt 在输入非法时返回 *strconv.NumError。
		var numErr *strconv.NumError
		if !errors.As(err, &numErr) {
			t.Logf("err type = %T (不是 strconv.NumError，可接受)", err)
		}
	})
}

func TestJoinStrings(t *testing.T) {
	cases := []struct {
		ss   []string
		sep  string
		want string
	}{
		{nil, ",", ""},
		{[]string{"", ""}, ",", ""},
		{[]string{"a", "", "b"}, ",", "a,b"},
		{[]string{"a", "b", "c"}, "|", "a|b|c"},
	}
	for _, c := range cases {
		if got := JoinStrings(c.ss, c.sep); got != c.want {
			t.Errorf("JoinStrings(%v, %q) = %q, want %q", c.ss, c.sep, got, c.want)
		}
	}
}

func TestDedupStrings(t *testing.T) {
	if got := DedupStrings(nil); got != nil {
		t.Fatalf("DedupStrings(nil) = %v, want nil", got)
	}
	got := DedupStrings([]string{"b", "a", "b", "c", "a"})
	want := []string{"b", "a", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestContainsString(t *testing.T) {
	ss := []string{"alpha", "beta", "gamma"}
	if !ContainsString(ss, "beta") {
		t.Error("expected beta to be found")
	}
	if ContainsString(ss, "delta") {
		t.Error("delta should not be found")
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 0, ""},
		{"hello", 3, "hel"},
		{"hello", 10, "hello"},
		{"你好世界", 2, "你好"},
		{"", 5, ""},
	}
	for _, c := range cases {
		if got := Truncate(c.in, c.max); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}
