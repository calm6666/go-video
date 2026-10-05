package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ContactResolver 把“接收人”换算成供应商可用的受控投递标识。
//
// 隐私与契约约束：
//   - 本服务不存明文手机号/邮箱，也不接受调用方在模板参数里传明文号码；
//   - push 的标识由客户端上报的设备 token 引用（target_ref / device_id）直接透传；
//   - sms/email 的真实号码属于 account 域。account.v1 当前只提供 tel_status/email_status
//     绑定状态，没有“按 mid 取联系方式”的 RPC（见 README 缺口章节），
//     因此在契约补齐前，未携带 target_ref 的 sms/email 请求必须显式失败，
//     由投递调度器把任务转 retry/dead_letter —— 绝不能伪造发送成功。
type ContactResolver interface {
	// Resolve 返回供应商可用的投递标识；无法解析时返回显式错误。
	Resolve(ctx context.Context, channel string, mid int64, targetRef string) (string, error)
}

// passthroughResolver 是本期唯一实现：只信任调用方给出的受控标识。
type passthroughResolver struct{}

// NewPassthroughResolver 构造受控标识透传解析器。
func NewPassthroughResolver() ContactResolver { return passthroughResolver{} }

// Resolve 实现 ContactResolver。
func (passthroughResolver) Resolve(_ context.Context, channel string, mid int64, targetRef string) (string, error) {
	ref := strings.TrimSpace(targetRef)
	if ref != "" {
		if strings.ContainsAny(ref, "\r\n") || len(ref) > 128 {
			return "", fmt.Errorf("%w: target_ref malformed", ErrInvalidConfig)
		}
		return ref, nil
	}
	switch channel {
	case ChannelSMS, ChannelEmail:
		if mid <= 0 {
			return "", fmt.Errorf("%w: channel=%s requires target_ref or mid", ErrInvalidConfig, channel)
		}
		return "", fmt.Errorf("%w: channel=%s mid=%d", ErrContactNotWired, channel, mid)
	case ChannelPush:
		return "", fmt.Errorf("%w: channel=push requires client reported device token target_ref", ErrInvalidConfig)
	default:
		return "", fmt.Errorf("%w: %q", ErrUnsupportedChannel, channel)
	}
}

// ErrNoRecipients 请求未携带任何接收人。
var ErrNoRecipients = errors.New("notification/provider: no recipients")
