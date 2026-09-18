package idgen

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

const ulidLen = 26

func TestULIDFormat(t *testing.T) {
	id, err := ULID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != ulidLen {
		t.Fatalf("len = %d, want %d", len(id), ulidLen)
	}
	if _, err := ulid.Parse(id); err != nil {
		t.Fatalf("not a valid ULID: %v", err)
	}
}

func TestULIDMonotonic(t *testing.T) {
	prev, err := ULID()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		next, err := ULID()
		if err != nil {
			t.Fatal(err)
		}
		if next <= prev {
			t.Fatalf("iteration %d: next %q <= prev %q (non-monotonic)", i, next, prev)
		}
		prev = next
	}
}

func TestULIDUniqueness(t *testing.T) {
	const n = 5000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id, err := ULID()
		if err != nil {
			t.Fatal(err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ULID at iteration %d: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestMustULID(t *testing.T) {
	id := MustULID()
	if len(id) != ulidLen {
		t.Fatalf("len = %d, want %d", len(id), ulidLen)
	}
}

func TestShort(t *testing.T) {
	t.Run("basic", func(t *testing.T) {
		s, err := Short(16)
		if err != nil {
			t.Fatal(err)
		}
		// 16 字节 -> Base64URL 不带填充时为 ceil(16*4/3) = 22 字符。
		if len(s) != 22 {
			t.Fatalf("len = %d, want 22", len(s))
		}
		if strings.Contains(s, "=") {
			t.Fatalf("Short should be unpadded, got %q", s)
		}
	})
	t.Run("uniqueness", func(t *testing.T) {
		seen := make(map[string]struct{}, 100)
		for i := 0; i < 100; i++ {
			s, err := Short(8)
			if err != nil {
				t.Fatal(err)
			}
			if _, dup := seen[s]; dup {
				t.Fatalf("duplicate Short at iteration %d", i)
			}
			seen[s] = struct{}{}
		}
	})
	t.Run("non-positive rejected", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			if _, err := Short(n); err == nil {
				t.Errorf("Short(%d) expected error", n)
			}
		}
	})
}

func TestPrefixed(t *testing.T) {
	id, err := Prefixed("evt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "evt_") {
		t.Fatalf("Prefixed missing underscore: %q", id)
	}
	suffix := strings.TrimPrefix(id, "evt_")
	if len(suffix) != ulidLen {
		t.Fatalf("suffix len = %d, want %d", len(suffix), ulidLen)
	}
	if _, err := ulid.Parse(suffix); err != nil {
		t.Fatalf("suffix not valid ULID: %v", err)
	}
}

func TestSetDefault(t *testing.T) {
	defer SetDefault(nil) // 恢复 crypto/rand 默认实现

	// 注入一个由确定性熵源 reader 支撑的生成器。
	// oklog/ulid 要求熵源每次提供 10 字节，无限 reader 可满足该要求。
	deterministic := NewGenerator(newInfiniteReader(0xAB))
	SetDefault(deterministic)

	id1, err := ULID()
	if err != nil {
		t.Fatal(err)
	}
	id2, err := ULID()
	if err != nil {
		t.Fatal(err)
	}
	// 使用 Monotonic 熵源时，连续两次调用产生不同 ID
	//（熵自增或毫秒推进）。
	if id1 == id2 {
		t.Fatalf("expected unique IDs, got %q twice", id1)
	}
}

type infiniteReader struct {
	b byte
}

func newInfiniteReader(b byte) *infiniteReader { return &infiniteReader{b: b} }

func (r *infiniteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}
