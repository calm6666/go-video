package repository

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"go-video/services/inbox/model"
)

// cursor_test.go 覆盖收件箱翻页游标：必须是稳定、不透明、可拒绝伪造的输入，
// 因为客户端直接回传该串（docs/api-and-events.md §2 的 ps/cursor 约定）。

func TestCursorRoundTrip(t *testing.T) {
	cases := []Cursor{
		{Time: 1700000000, ID: 1},
		{Time: 1700000000, ID: 987654321},
		{Time: 1, ID: 1},
		{Time: 0, ID: 42}, // 同一秒批量投递：ctime 相同、只靠 id 决胜
		{Time: 9007199254740993, ID: 1},
	}
	for _, want := range cases {
		s := EncodeCursor(want)
		if s == "" {
			t.Fatalf("非零游标编码不应为空串：%+v", want)
		}
		got, err := DecodeCursor(s)
		if err != nil {
			t.Fatalf("解码 %q 失败: %v", s, err)
		}
		if got != want {
			t.Fatalf("游标往返不一致：%+v -> %q -> %+v", want, s, got)
		}
	}
}

func TestEncodeCursorZeroIsEmpty(t *testing.T) {
	if got := EncodeCursor(Cursor{}); got != "" {
		t.Fatalf("零值游标应编码为空串（第一页），得到 %q", got)
	}
	got, err := DecodeCursor("")
	if err != nil || got != (Cursor{}) {
		t.Fatalf("空串应解码为零游标，得到 %+v err=%v", got, err)
	}
}

func TestCursorIsURLSafeAndOpaque(t *testing.T) {
	s := EncodeCursor(Cursor{Time: 1700000000, ID: 12})
	if strings.ContainsAny(s, "+/=") {
		t.Fatalf("游标必须是 URL 安全的 RawURLEncoding，得到 %q", s)
	}
	// 明文可解码只说明格式简单；对外仍作为不透明串，客户端不得解析（服务端可换版本）。
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("游标不是合法 RawURLEncoding: %v", err)
	}
	if string(raw) != "1700000000:12" {
		t.Fatalf("游标载荷格式变化需要评审，得到 %q", string(raw))
	}
}

func TestDecodeCursorRejectsMalformed(t *testing.T) {
	bad := []struct {
		name  string
		input string
	}{
		{"非 base64", "!!!not-base64!!!"},
		{"缺少分隔符", base64.RawURLEncoding.EncodeToString([]byte("1700000000"))},
		{"三段", base64.RawURLEncoding.EncodeToString([]byte("1700000000:12:34"))},
		{"时间非数字", base64.RawURLEncoding.EncodeToString([]byte("abc:12"))},
		{"ID 非数字", base64.RawURLEncoding.EncodeToString([]byte("1700000000:xy"))},
		{"负时间", base64.RawURLEncoding.EncodeToString([]byte("-1:12"))},
		{"负 ID", base64.RawURLEncoding.EncodeToString([]byte("1700000000:-12"))},
		{"空载荷", ""}, // 空串合法：表示第一页，见下面单独断言
	}
	for _, c := range bad[:len(bad)-1] {
		if _, err := DecodeCursor(c.input); !isCursorError(err) {
			t.Fatalf("%s：期望 ErrInvalidCursor，得到 %v", c.name, err)
		}
	}
	if _, err := DecodeCursor(""); err != nil {
		t.Fatalf("空游标表示第一页，不应报错，得到 %v", err)
	}
}

func TestDecodeCursorRejectsOverflow(t *testing.T) {
	// 超出 int64 的字符串必须被拒绝，不能回绕成负游标（会跳过或重复整页）。
	huge := base64.RawURLEncoding.EncodeToString([]byte("99999999999999999999:1"))
	if _, err := DecodeCursor(huge); !isCursorError(err) {
		t.Fatalf("溢出游标期望 ErrInvalidCursor，得到 %v", err)
	}
}

func TestCursorOrderingIsStrictlyDecreasing(t *testing.T) {
	// 同一秒内的两条消息：游标必须推进到第二条之后，翻页不重不漏。
	first := Cursor{Time: 1700000000, ID: 20}
	second := Cursor{Time: 1700000000, ID: 19}
	if EncodeCursor(first) == EncodeCursor(second) {
		t.Fatal("同一秒不同 id 的游标不得相同")
	}
	if !(second.Time < first.Time || (second.Time == first.Time && second.ID < first.ID)) {
		t.Fatal("游标 (ctime,id) 必须单调递减才能用于 DESC 分页")
	}
}

func isCursorError(err error) bool {
	return errors.Is(err, model.ErrInvalidCursor)
}
