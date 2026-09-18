package ecode

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelMessages(t *testing.T) {
	if OK.Message() != "ok" {
		t.Errorf("OK.Message = %q, want ok", OK.Message())
	}
	if ServerErr.Message() != "internal error" {
		t.Errorf("ServerErr.Message = %q, want internal error", ServerErr.Message())
	}
}

func TestRegisterAndMessage(t *testing.T) {
	Register(map[int]string{
		10001: "user not found",
		10002: "password mismatch",
	})
	if got := Int(10001).Message(); got != "user not found" {
		t.Errorf("message = %q, want user not found", got)
	}
	if got := Int(99999).Message(); got != "99999" {
		t.Errorf("unknown code message = %q, want decimal fallback", got)
	}
}

func TestNewDedup(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate New")
		}
	}()
	_ = New(20001)
	_ = New(20001) // panics
}

func TestNewRejectsNonPositive(t *testing.T) {
	for _, bad := range []int{0, -1, -100} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("expected panic for New(%d)", bad)
				}
			}()
			_ = New(bad)
		}()
	}
}

func TestStringParsing(t *testing.T) {
	if String("") != OK {
		t.Error("empty string should be OK")
	}
	if String("50000") != ServerErr {
		t.Error("50000 should equal ServerErr")
	}
	if String("not-a-number") != ServerErr {
		t.Error("non-numeric should fall back to ServerErr")
	}
}

func TestCodeErrorAndMessage(t *testing.T) {
	c := New(30001)
	if c.Error() != "30001" {
		t.Errorf("Error() = %q, want 30001", c.Error())
	}
	Register(map[int]string{30001: "custom"})
	if c.Message() != "custom" {
		t.Errorf("Message() = %q, want custom", c.Message())
	}
}

func TestCauseFromCodeValue(t *testing.T) {
	c := New(40001)
	if got := Cause(c); got.Code() != 40001 {
		t.Errorf("Cause(Code) = %d, want 40001", got.Code())
	}
}

func TestCauseFromErrorPtr(t *testing.T) {
	c := New(40002)
	err := c.Err("detail1", 42)
	got := Cause(err)
	if got.Code() != 40002 {
		t.Errorf("Cause(*Error) = %d, want 40002", got.Code())
	}
	details := err.Details()
	if len(details) != 2 || details[0] != "detail1" || details[1] != 42 {
		t.Errorf("Details = %v, want [detail1 42]", details)
	}
}

func TestCauseFromWrappedError(t *testing.T) {
	c := New(40003)
	wrapped := fmt.Errorf("outer: %w", c)
	got := Cause(wrapped)
	if got.Code() != 40003 {
		t.Errorf("Cause(wrapped) = %d, want 40003", got.Code())
	}
}

func TestCauseFromUnrelatedError(t *testing.T) {
	if Cause(errors.New("plain")) != ServerErr {
		t.Error("plain error should map to ServerErr")
	}
	if Cause(nil) != OK {
		t.Error("nil error should map to OK")
	}
}

func TestEqualError(t *testing.T) {
	c := New(50001)
	if !EqualError(c, c.Err()) {
		t.Error("EqualError should match own Err()")
	}
	if EqualError(c, errors.New("unrelated")) {
		t.Error("EqualError should not match unrelated")
	}
	if !EqualError(OK, nil) {
		t.Error("EqualError(OK, nil) should be true")
	}
}

func TestErrorWrap(t *testing.T) {
	c := New(60001)
	inner := errors.New("db connection lost")
	err := c.Err().Wrap(inner)
	if !errors.Is(err, inner) {
		t.Error("errors.Is should find inner cause via Unwrap")
	}
	if Cause(err).Code() != 60001 {
		t.Error("Cause should still report 60001")
	}
}

func TestNilError(t *testing.T) {
	var e *Error
	if e.Error() != OK.Message() {
		t.Error("nil *Error.Error should return OK message")
	}
	if e.Code() != OK {
		t.Error("nil *Error.Code should return OK")
	}
}
