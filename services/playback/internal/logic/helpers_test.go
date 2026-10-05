package logic

import (
	"errors"
	"testing"

	"go-video/services/playback/internal/signurl"
	"go-video/services/playback/model"
	"go-video/services/playback/rpc"
)

func TestClampExpireAt(t *testing.T) {
	const now = int64(1_800_000_000)
	cases := []struct {
		name      string
		ttl       int64
		windowEnd int64
		want      int64
		wantErr   error
	}{
		{"UGC 不受窗口约束", 1800, 0, now + 1800, nil},
		{"窗口早于 TTL 时以窗口封顶", 1800, now + 60, now + 60, nil},
		{"窗口晚于 TTL 时以 TTL 封顶", 1800, now + 7200, now + 1800, nil},
		{"窗口已结束", 1800, now, 0, model.ErrCopyrightWindowUnavailable},
		{"窗口已在过去", 1800, now - 10, 0, model.ErrCopyrightWindowUnavailable},
		{"TTL 非正数", 0, 0, 0, signurl.ErrInvalidTTL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clampExpireAt(now, tc.ttl, tc.windowEnd)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("clampExpireAt() error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("clampExpireAt() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestContentTypeValue(t *testing.T) {
	cases := []struct {
		in      rpc.ContentType
		want    int32
		wantErr bool
	}{
		{rpc.ContentType_CONTENT_TYPE_UGC, model.ContentTypeUGC, false},
		{rpc.ContentType_CONTENT_TYPE_PGC, model.ContentTypePGC, false},
		{rpc.ContentType_CONTENT_TYPE_UNSPECIFIED, 0, true},
		{rpc.ContentType(99), 0, true},
	}
	for _, tc := range cases {
		got, err := contentTypeValue(tc.in)
		if tc.wantErr && err == nil {
			t.Fatalf("contentTypeValue(%v) expected error", tc.in)
		}
		if !tc.wantErr && (err != nil || got != tc.want) {
			t.Fatalf("contentTypeValue(%v) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestPlatformValueRejectsUnknown(t *testing.T) {
	for _, p := range []rpc.Platform{
		rpc.Platform_PLATFORM_ANDROID, rpc.Platform_PLATFORM_IOS,
		rpc.Platform_PLATFORM_HARMONY, rpc.Platform_PLATFORM_DESKTOP,
	} {
		got, err := platformValue(p)
		if err != nil || got != int32(p) {
			t.Fatalf("platformValue(%v) = %d, %v", p, got, err)
		}
	}
	// 未指定与未知平台（例如小程序）必须显式拒绝，而不是落库 0。
	if _, err := platformValue(rpc.Platform_PLATFORM_UNSPECIFIED); !errors.Is(err, model.ErrInvalidPlatform) {
		t.Fatalf("platformValue(unspecified) error = %v, want %v", err, model.ErrInvalidPlatform)
	}
	if _, err := platformValue(rpc.Platform(88)); !errors.Is(err, model.ErrInvalidPlatform) {
		t.Fatalf("platformValue(unknown) error = %v, want %v", err, model.ErrInvalidPlatform)
	}
}

func TestSignErrorMapsDisabledToDomainError(t *testing.T) {
	if got := signError(signurl.ErrAuthKeyDisabled); !errors.Is(got, model.ErrSignerMisconfigured) {
		t.Fatalf("signError(disabled) = %v, want %v", got, model.ErrSignerMisconfigured)
	}
	// 私钥缺失保持原错误透出：运维需要一眼看出是配置问题。
	if got := signError(signurl.ErrPrivateKeyRequired); !errors.Is(got, signurl.ErrPrivateKeyRequired) {
		t.Fatalf("signError(no key) = %v, want ErrPrivateKeyRequired", got)
	}
	if got := signError(nil); got != nil {
		t.Fatalf("signError(nil) = %v, want nil", got)
	}
}

func TestAuthDenyReasonIsStable(t *testing.T) {
	cases := map[error]string{
		signurl.ErrMalformedAuthKey:   denyBadAuthFormat,
		signurl.ErrAuthKeyExpired:     denySessionExpired,
		signurl.ErrSignatureMismatch:  denySignMismatch,
		signurl.ErrAuthKeyDisabled:    denySignerDisabled,
		signurl.ErrPrivateKeyRequired: denySignerDisabled,
	}
	for err, want := range cases {
		if got := authDenyReason(err); got != want {
			t.Fatalf("authDenyReason(%v) = %q, want %q", err, got, want)
		}
	}
	if got := authDenyReason(errors.New("boom")); got != denySignMismatch {
		t.Fatalf("authDenyReason(unknown) = %q, want %q", got, denySignMismatch)
	}
}

func TestCheckSameRequest(t *testing.T) {
	in := &rpc.GetPlaybackTokenReq{
		ContentType: rpc.ContentType_CONTENT_TYPE_UGC,
		ContentId:   100,
		Mid:         7,
	}
	s := &model.PlaybackSession{ContentType: model.ContentTypeUGC, ContentId: 100, Mid: 7}
	if err := checkSameRequest(s, in); err != nil {
		t.Fatalf("checkSameRequest() error = %v, want nil", err)
	}
	// 幂等键被串用到别的用户/内容上必须拒绝。
	other := &rpc.GetPlaybackTokenReq{ContentType: rpc.ContentType_CONTENT_TYPE_UGC, ContentId: 100, Mid: 8}
	if err := checkSameRequest(s, other); !errors.Is(err, model.ErrSessionIDConflict) {
		t.Fatalf("checkSameRequest(other mid) error = %v, want %v", err, model.ErrSessionIDConflict)
	}
	otherContent := &rpc.GetPlaybackTokenReq{ContentType: rpc.ContentType_CONTENT_TYPE_PGC, ContentId: 200, Mid: 7}
	if err := checkSameRequest(s, otherContent); !errors.Is(err, model.ErrSessionIDConflict) {
		t.Fatalf("checkSameRequest(other content) error = %v, want %v", err, model.ErrSessionIDConflict)
	}
}

func TestUidOf(t *testing.T) {
	if got := uidOf(0); got != "0" {
		t.Fatalf("uidOf(0) = %q, want 0", got)
	}
	if got := uidOf(-5); got != "0" {
		t.Fatalf("uidOf(-5) = %q, want 0", got)
	}
	if got := uidOf(42); got != "42" {
		t.Fatalf("uidOf(42) = %q, want 42", got)
	}
}
