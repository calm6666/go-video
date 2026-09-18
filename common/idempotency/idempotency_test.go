package idempotency

import (
	"strings"
	"testing"
)

func TestStateParse(t *testing.T) {
	cases := []struct {
		in      string
		want    State
		wantErr bool
	}{
		{"pending", StatePending, false},
		{"PENDING", StatePending, false},
		{" succeeded ", StateSucceeded, false},
		{"failed", StateFailed, false},
		{"", "", true},
		{"unknown", "", true},
	}
	for _, c := range cases {
		got, err := ParseState(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseState(%q) expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseState(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestKeyStableHash(t *testing.T) {
	k1 := NewKey("video.publish", "sub_123", "req_abc")
	k2 := NewKey("video.publish", "sub_123", "req_abc")
	if k1.String() != k2.String() {
		t.Fatalf("hash not stable: %q vs %q", k1.String(), k2.String())
	}
	if len(k1.String()) != 64 {
		t.Fatalf("hash length = %d, want 64 (SHA-256 hex)", len(k1.String()))
	}
}

func TestKeyIgnoresEmptyParts(t *testing.T) {
	k1 := NewKey("a", "b")
	k2 := NewKey("a", "", "b", "")
	if k1.String() != k2.String() {
		t.Errorf("empty parts should be skipped: %q vs %q", k1.String(), k2.String())
	}
}

func TestKeyDistinguishesDifferentInput(t *testing.T) {
	k1 := NewKey("a", "b")
	k2 := NewKey("a", "c")
	if k1.String() == k2.String() {
		t.Error("different inputs should produce different hashes")
	}
}

func TestKeyRaw(t *testing.T) {
	k := NewKey("video.publish", "sub_123")
	if k.Raw() != "video.publish:sub_123" {
		t.Errorf("Raw = %q, want video.publish:sub_123", k.Raw())
	}
}

func TestKeyAllEmpty(t *testing.T) {
	k := NewKey("", "", "")
	if k.String() == "" {
		t.Error("all-empty parts should still produce a hash")
	}
	// 空字符串的 SHA-256 哈希是已知固定值。
	if k.String() != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty input hash = %q, want sha256 of empty string", k.String())
	}
	if k.Raw() != "" {
		t.Errorf("Raw = %q, want empty", k.Raw())
	}
}

func TestGenerateKey(t *testing.T) {
	k := GenerateKey()
	// 应为 26 字符 ULID 或 "idem_" + 26 字符 ULID。
	if !strings.HasPrefix(k, "idem_") && len(k) != 26 {
		t.Fatalf("GenerateKey = %q, want idem_ prefix or 26-char ULID", k)
	}
	// 两次调用应产生不同的 key。
	k2 := GenerateKey()
	if k == k2 {
		t.Fatalf("GenerateKey returned same value twice: %q", k)
	}
}

func TestNewHitAndMiss(t *testing.T) {
	miss := NewMiss()
	if miss.Hit() {
		t.Error("NewMiss should not hit")
	}
	if miss.State() != "" {
		t.Errorf("miss state = %q, want empty", miss.State())
	}
	if miss.Err() != nil {
		t.Error("miss Err should be nil")
	}

	hit := NewHit(StateSucceeded, []byte(`{"status":"ok"}`), 0, "")
	if !hit.Hit() {
		t.Error("NewHit should hit")
	}
	if hit.State() != StateSucceeded {
		t.Errorf("hit state = %q, want succeeded", hit.State())
	}
	if string(hit.Response()) != `{"status":"ok"}` {
		t.Errorf("hit response = %q", string(hit.Response()))
	}
	if hit.Err() != nil {
		t.Error("succeeded hit Err should be nil")
	}
}

func TestHitErrForFailed(t *testing.T) {
	hit := NewHit(StateFailed, nil, 40001, "video quota exceeded")
	if !hit.Hit() {
		t.Fatal("expected hit")
	}
	if hit.ErrCode() != 40001 {
		t.Errorf("ErrCode = %d, want 40001", hit.ErrCode())
	}
	err := hit.Err()
	if err == nil {
		t.Fatal("failed hit should produce error")
	}
	if err.Error() != "video quota exceeded" {
		t.Errorf("Err = %q, want video quota exceeded", err.Error())
	}
}

func TestHitErrForPending(t *testing.T) {
	hit := NewHit(StatePending, nil, 0, "")
	err := hit.Err()
	if err == nil {
		t.Fatal("pending hit should produce error so caller can decide 409")
	}
	if !strings.Contains(err.Error(), "pending") {
		t.Errorf("pending Err should mention state, got %q", err.Error())
	}
}
