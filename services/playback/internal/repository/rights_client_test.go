package repository

import (
	"context"
	"errors"
	"testing"

	"go-video/services/playback/model"
	rightsrpc "go-video/services/rights/rpc"
)

// TestToRightsContentType 锁定 playback 与 rights 之间的内容类型映射。
//
// 两边枚举编号刻意不同（playback 1=UGC/2=PGC，rights 1=PGC/2=UGC），
// 一旦有人图省事改成 int32 直转，PGC 就会被当成 UGC 跳过版权校验（AGENTS.md §5）。
// 该测试就是这条越权的护栏。
func TestToRightsContentType(t *testing.T) {
	cases := []struct {
		name string
		in   int32
		want rightsrpc.ContentType
	}{
		{"playback UGC(1) -> rights UGC(2)", model.ContentTypeUGC, rightsrpc.ContentType_CONTENT_TYPE_UGC},
		{"playback PGC(2) -> rights PGC(1)", model.ContentTypePGC, rightsrpc.ContentType_CONTENT_TYPE_PGC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toRightsContentType(tc.in)
			if err != nil {
				t.Fatalf("toRightsContentType(%d) err = %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("toRightsContentType(%d) = %d, want %d", tc.in, got, tc.want)
			}
			// 显式断言"不能直转"：映射结果与入参数值必须不同
			if int32(got) == tc.in {
				t.Errorf("映射后数值未变化（%d），说明两边编号被误改成一致或做了直转", tc.in)
			}
		})
	}
}

// TestToRightsContentTypeRejectsUnknown 未注册的内容类型必须报错而不是落到 UNSPECIFIED 后继续调用。
func TestToRightsContentTypeRejectsUnknown(t *testing.T) {
	for _, in := range []int32{0, 3, -1, 99} {
		got, err := toRightsContentType(in)
		if !errors.Is(err, model.ErrInvalidContentType) {
			t.Errorf("toRightsContentType(%d) err = %v, want ErrInvalidContentType", in, err)
		}
		if got != rightsrpc.ContentType_CONTENT_TYPE_UNSPECIFIED {
			t.Errorf("toRightsContentType(%d) = %d, want UNSPECIFIED", in, got)
		}
	}
}

// TestNewUnavailableRightsNeverSucceeds rights 未配置时必须显式失败，
// 绝不返回 (true, ...)，避免 PGC 在无版权校验的情况下拿到签名地址。
func TestNewUnavailableRightsNeverSucceeds(t *testing.T) {
	checker := NewUnavailableRights()
	if checker == nil {
		t.Fatal("NewUnavailableRights() = nil")
	}
	for _, ct := range []int32{model.ContentTypeUGC, model.ContentTypePGC} {
		playable, endTime, err := checker.CheckPlayable(context.Background(), 1001, ct, "cn")
		if !errors.Is(err, model.ErrRightsUnavailable) {
			t.Errorf("content_type=%d err = %v, want ErrRightsUnavailable", ct, err)
		}
		if playable {
			t.Errorf("content_type=%d playable = true，禁止伪造成功", ct)
		}
		if endTime != 0 {
			t.Errorf("content_type=%d endTime = %d, want 0", ct, endTime)
		}
	}
}

// TestCheckPlayableRejectsBadContentTypeBeforeRPC 真实客户端在发起 RPC 前先校验类型，
// 未知类型不得把 UNSPECIFIED 发给 rights。
func TestCheckPlayableRejectsBadContentTypeBeforeRPC(t *testing.T) {
	c := &rightsClient{cli: nil} // cli 为空：一旦真的发起 RPC 就会 panic，测试立即失败
	playable, endTime, err := c.CheckPlayable(context.Background(), 1, 0, "cn")
	if !errors.Is(err, model.ErrInvalidContentType) {
		t.Errorf("err = %v, want ErrInvalidContentType", err)
	}
	if playable || endTime != 0 {
		t.Errorf("playable/endTime = %v/%d, want false/0", playable, endTime)
	}
}
