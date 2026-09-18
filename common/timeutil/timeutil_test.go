package timeutil

import (
	"context"
	"errors"
	"testing"
	stdtime "time"
)

func TestTimeScan(t *testing.T) {
	t.Run("time.Time", func(t *testing.T) {
		var ts Time
		src := stdtime.Unix(1700000000, 0).UTC()
		if err := ts.Scan(src); err != nil {
			t.Fatal(err)
		}
		if int64(ts) != 1700000000 {
			t.Fatalf("got %d, want 1700000000", ts)
		}
	})
	t.Run("string", func(t *testing.T) {
		var ts Time
		if err := ts.Scan("1700000000"); err != nil {
			t.Fatal(err)
		}
		if int64(ts) != 1700000000 {
			t.Fatalf("got %d", ts)
		}
	})
	t.Run("empty string", func(t *testing.T) {
		var ts Time
		if err := ts.Scan(""); err != nil {
			t.Fatal(err)
		}
		if ts != 0 {
			t.Fatalf("got %d, want 0", ts)
		}
	})
	t.Run("[]byte", func(t *testing.T) {
		var ts Time
		if err := ts.Scan([]byte("123")); err != nil {
			t.Fatal(err)
		}
		if int64(ts) != 123 {
			t.Fatalf("got %d", ts)
		}
	})
	t.Run("int64", func(t *testing.T) {
		var ts Time
		if err := ts.Scan(int64(42)); err != nil {
			t.Fatal(err)
		}
		if int64(ts) != 42 {
			t.Fatalf("got %d", ts)
		}
	})
	t.Run("nil", func(t *testing.T) {
		var ts Time
		if err := ts.Scan(nil); err != nil {
			t.Fatal(err)
		}
		if ts != 0 {
			t.Fatalf("got %d, want 0", ts)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		var ts Time
		err := ts.Scan(3.14)
		if err == nil {
			t.Fatal("expected error for float64")
		}
		if !errors.Is(err, err) { // sanity check on error presence
			t.Fatal("err should be non-nil")
		}
	})
}

func TestTimeValueAndTime(t *testing.T) {
	ts := Time(1700000000)
	v, err := ts.Value()
	if err != nil {
		t.Fatal(err)
	}
	tt, ok := v.(stdtime.Time)
	if !ok {
		t.Fatalf("Value returned %T, want time.Time", v)
	}
	if tt.Unix() != 1700000000 {
		t.Fatalf("got unix %d", tt.Unix())
	}
	if ts.Time().Unix() != 1700000000 {
		t.Fatalf("Time() unix mismatch")
	}
}

func TestDurationUnmarshalText(t *testing.T) {
	cases := []struct {
		in      string
		want    Duration
		wantErr bool
	}{
		{"", 0, false},
		{"500ms", Duration(stdtime.Millisecond * 500), false},
		{"1s", Duration(stdtime.Second), false},
		{"1m30s", Duration(stdtime.Second * 90), false},
		{"abc", 0, true},
	}
	for _, c := range cases {
		var d Duration
		err := d.UnmarshalText([]byte(c.in))
		if c.wantErr {
			if err == nil {
				t.Errorf("UnmarshalText(%q) expected error, got nil", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("UnmarshalText(%q) unexpected error: %v", c.in, err)
			continue
		}
		if d != c.want {
			t.Errorf("UnmarshalText(%q) = %v, want %v", c.in, d, c.want)
		}
	}
}

func TestDurationShrink(t *testing.T) {
	d := Duration(stdtime.Second)

	t.Run("no deadline uses d", func(t *testing.T) {
		ctx := context.Background()
		shrink, sctx, cancel := d.Shrink(ctx)
		defer cancel()
		if shrink != d {
			t.Fatalf("shrink = %v, want %v", shrink, d)
		}
		_, hasDeadline := sctx.Deadline()
		if !hasDeadline {
			t.Fatal("child ctx should have a deadline")
		}
	})

	t.Run("shorter parent deadline wins", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*stdtime.Millisecond)
		defer cancel()
		shrink, _, scancel := d.Shrink(ctx)
		defer scancel()
		if shrink >= d {
			t.Fatalf("shrink = %v, should be smaller than %v", shrink, d)
		}
	})
}

func TestNowAndSetNowFunc(t *testing.T) {
	fake := stdtime.Unix(9999999999, 0)
	SetNowFunc(func() stdtime.Time { return fake })
	defer SetNowFunc(nil)

	if got := Now(); !got.Equal(fake) {
		t.Fatalf("Now() = %v, want %v", got, fake)
	}
	if NowMilli() != fake.UnixMilli() {
		t.Fatalf("NowMilli mismatch")
	}
}

func TestParseAndFormatRFC3339(t *testing.T) {
	s := "2026-08-24T10:00:00Z"
	tt, err := ParseRFC3339(s)
	if err != nil {
		t.Fatal(err)
	}
	if out := FormatRFC3339(tt); out != s {
		t.Errorf("round trip = %q, want %q", out, s)
	}
	if FormatRFC3339(stdtime.Time{}) != "" {
		t.Error("zero time should format to empty string")
	}
	if _, err := ParseRFC3339("not-a-time"); err == nil {
		t.Error("expected error for invalid input")
	}
}
