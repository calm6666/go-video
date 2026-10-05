package repository

import (
	"encoding/base64"
	"errors"
	"testing"

	"go-video/services/search-query/model"
)

func TestOffsetCursorRoundTrip(t *testing.T) {
	const fp = "abc123"
	cursor, err := EncodeOffsetCursor(60, fp)
	if err != nil {
		t.Fatalf("EncodeOffsetCursor: %v", err)
	}
	if cursor == "" {
		t.Fatal("cursor must not be empty for a non-zero offset")
	}
	// 游标必须是不透明文本（base64url 无填充），客户端不得解析它。
	if _, err := base64.RawURLEncoding.DecodeString(cursor); err != nil {
		t.Fatalf("cursor is not base64url(no padding): %v", err)
	}
	off, err := DecodeOffsetCursor(cursor, fp)
	if err != nil {
		t.Fatalf("DecodeOffsetCursor: %v", err)
	}
	if off != 60 {
		t.Fatalf("offset = %d, want 60", off)
	}
}

func TestDecodeOffsetCursorFirstPage(t *testing.T) {
	off, err := DecodeOffsetCursor("", "whatever")
	if err != nil {
		t.Fatalf("empty cursor must be treated as first page, got %v", err)
	}
	if off != 0 {
		t.Errorf("offset = %d, want 0", off)
	}
}

func TestDecodeOffsetCursorRejectsMismatchedQuery(t *testing.T) {
	cursor, err := EncodeOffsetCursor(30, "fp-a")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// 翻页途中改了筛选条件（指纹变化）：必须报错，而不是静默串页。
	if _, err := DecodeOffsetCursor(cursor, "fp-b"); !errors.Is(err, model.ErrCursorMismatch) {
		t.Fatalf("err = %v, want ErrCursorMismatch", err)
	}
}

func TestDecodeOffsetCursorRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"not base64":            "!!!not-base64!!!",
		"base64 of garbage":     base64.RawURLEncoding.EncodeToString([]byte("hello")),
		"json but not a cursor": base64.RawURLEncoding.EncodeToString([]byte(`{"foo":1}`)),
	}
	for name, cursor := range cases {
		if _, err := DecodeOffsetCursor(cursor, "fp"); err == nil {
			t.Errorf("%s: must be rejected", name)
		} else if !errors.Is(err, model.ErrInvalidCursor) && !errors.Is(err, ErrUnsupportedCursorVersion) {
			t.Errorf("%s: err = %v, want ErrInvalidCursor/unsupported version", name, err)
		}
	}
}

func TestDecodeOffsetCursorRejectsNegativeOffset(t *testing.T) {
	payload := offsetCursorPayload{V: cursorVersion, Off: -10, Fing: "fp"}
	bs, err := encodeCursorJSON(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := DecodeOffsetCursor(bs, "fp"); !errors.Is(err, model.ErrInvalidCursor) {
		t.Fatalf("err = %v, want ErrInvalidCursor", err)
	}
}

func TestCursorVersionMustBeChecked(t *testing.T) {
	// 滚动发布期间的未来版本游标：显式报版本错误，不能当成首页静默重放。
	payload := offsetCursorPayload{V: cursorVersion + 1, Off: 30, Fing: "fp"}
	bs, err := encodeCursorJSON(payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := DecodeOffsetCursor(bs, "fp"); !errors.Is(err, ErrUnsupportedCursorVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedCursorVersion", err)
	}

	keyset := keysetCursorPayload{V: cursorVersion + 1, T: 1, I: 1}
	kbs, err := encodeCursorJSON(keyset)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, _, err := DecodeKeysetCursor(kbs); !errors.Is(err, ErrUnsupportedCursorVersion) {
		t.Fatalf("keyset err = %v, want ErrUnsupportedCursorVersion", err)
	}
}

func TestKeysetCursorRoundTrip(t *testing.T) {
	cursor, err := EncodeKeysetCursor(1700000000, 4242)
	if err != nil {
		t.Fatalf("EncodeKeysetCursor: %v", err)
	}
	mtime, id, err := DecodeKeysetCursor(cursor)
	if err != nil {
		t.Fatalf("DecodeKeysetCursor: %v", err)
	}
	if mtime != 1700000000 || id != 4242 {
		t.Fatalf("decoded (%d,%d), want (1700000000,4242)", mtime, id)
	}
	if mtime, id, err := DecodeKeysetCursor(""); err != nil || mtime != 0 || id != 0 {
		t.Fatalf("empty keyset cursor = (%d,%d,%v), want first page", mtime, id, err)
	}
	if _, _, err := DecodeKeysetCursor("@@@"); !errors.Is(err, model.ErrInvalidCursor) {
		t.Fatalf("err = %v, want ErrInvalidCursor", err)
	}
}

func TestPageToOffset(t *testing.T) {
	cases := []struct {
		pn, ps int32
		want   int64
	}{
		{0, 30, 0}, // 未传 pn 视为首页
		{1, 30, 0},
		{2, 30, 30},
		{4, 20, 60},
	}
	for _, c := range cases {
		got, err := PageToOffset(c.pn, c.ps)
		if err != nil {
			t.Fatalf("PageToOffset(%d,%d): %v", c.pn, c.ps, err)
		}
		if got != c.want {
			t.Errorf("PageToOffset(%d,%d) = %d, want %d", c.pn, c.ps, got, c.want)
		}
	}
	if _, err := PageToOffset(1, 0); !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("ps=0 err = %v, want ErrInvalidPage", err)
	}
	if _, err := PageToOffset(-1, 30); !errors.Is(err, model.ErrInvalidPage) {
		t.Errorf("pn=-1 err = %v, want ErrInvalidPage", err)
	}
}
