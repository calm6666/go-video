package logic

// 本文件是 logic 包的手写扩展（策略与转换），不是 goctl 生成产物。

import (
	"errors"
	"strconv"

	"go-video/services/playback/internal/signurl"
	"go-video/services/playback/model"
	"go-video/services/playback/rpc"
)

// VerifyPlaybackToken 的拒绝原因（对外稳定字符串，客户端与 CDN 日志据此排障）。
// 回源校验失败用 allow=false + reason 表达，而不是 gRPC 错误：
// 边缘节点需要明确的"拒绝回源"响应，不能把拒绝和故障混成一类。
const (
	denySessionNotFound = "session_not_found" // 会话不存在
	denySessionExpired  = "session_expired"   // 会话已过期
	denySessionRevoked  = "session_revoked"   // 会话已撤销
	denyBadAuthFormat   = "bad_auth_format"   // auth_key 格式非法
	denySignMismatch    = "sign_mismatch"     // 签名不匹配（私钥轮换或串被篡改）
	denyURIMismatch     = "uri_mismatch"      // auth_key 与会话记录的 URI 不一致
	denySignerDisabled  = "signer_disabled"   // 服务端签名配置异常
)

// clampExpireAt 计算播放授权的过期时间点。
//
//	expireAt = min(now + ttl, windowEnd)
//
// windowEnd 为 rights 返回的版权窗口结束时间，0 表示不受窗口约束（UGC）。
// 授权永不超过版权窗口是 AGENTS.md §8 的要求：窗口结束后不应再有可回源的地址。
// 若窗口已经结束（windowEnd <= now）则拒绝签发。
func clampExpireAt(now, ttl, windowEnd int64) (int64, error) {
	if ttl <= 0 {
		return 0, signurl.ErrInvalidTTL
	}
	expireAt := now + ttl
	if windowEnd > 0 && windowEnd < expireAt {
		expireAt = windowEnd
	}
	if expireAt <= now {
		return 0, model.ErrCopyrightWindowUnavailable
	}
	return expireAt, nil
}

// contentTypeValue 校验并转换内容类型枚举，返回落库用的 int32。
func contentTypeValue(ct rpc.ContentType) (int32, error) {
	switch ct {
	case rpc.ContentType_CONTENT_TYPE_UGC:
		return model.ContentTypeUGC, nil
	case rpc.ContentType_CONTENT_TYPE_PGC:
		return model.ContentTypePGC, nil
	default:
		return 0, model.ErrInvalidContentType
	}
}

// platformValue 校验并转换客户端平台枚举，返回落库用的 int32。
func platformValue(p rpc.Platform) (int32, error) {
	switch p {
	case rpc.Platform_PLATFORM_ANDROID, rpc.Platform_PLATFORM_IOS,
		rpc.Platform_PLATFORM_HARMONY, rpc.Platform_PLATFORM_DESKTOP:
		return int32(p), nil
	default:
		// 小程序等其它端不在本期范围（AGENTS.md §1/§6），拒绝未知平台而不是写 0。
		return 0, model.ErrInvalidPlatform
	}
}

// signError 把签名器的协议错误翻译成播放域错误，保证对外错误码稳定：
// 私钥缺失（ErrPrivateKeyRequired）原样透出，属于服务端配置错误；
// 签名器被禁用则是启动配置矛盾，归并为 model.ErrSignerMisconfigured。
func signError(err error) error {
	if errors.Is(err, signurl.ErrAuthKeyDisabled) {
		return model.ErrSignerMisconfigured
	}
	return err
}

// sessionInfo 把会话领域对象投影为 RPC 响应体。
func sessionInfo(s *model.PlaybackSession) *rpc.PlaybackSessionInfo {
	if s == nil {
		return nil
	}
	return &rpc.PlaybackSessionInfo{
		SessionId:   s.SessionId,
		ContentType: s.ContentType,
		ContentId:   s.ContentId,
		Vid:         s.Vid,
		Mid:         s.Mid,
		Platform:    s.Platform,
		AppVersion:  s.AppVersion,
		Region:      s.Region,
		ObjectKey:   s.ObjectKey,
		Uri:         s.Uri,
		RequestId:   s.RequestId,
		ExpireAt:    s.ExpireAt,
		State:       s.State,
		TraceId:     s.TraceId,
		Ctime:       s.Ctime,
		Mtime:       s.Mtime,
	}
}

// progressInfo 把进度领域对象投影为 RPC 响应体。
func progressInfo(p *model.PlaybackProgress) *rpc.PlaybackProgressInfo {
	if p == nil {
		return nil
	}
	return &rpc.PlaybackProgressInfo{
		Id:          p.ID,
		SessionId:   p.SessionId,
		ContentType: p.ContentType,
		ContentId:   p.ContentId,
		Vid:         p.Vid,
		Mid:         p.Mid,
		PositionMs:  p.PositionMs,
		DurationMs:  p.DurationMs,
		BufferCount: p.BufferCount,
		AvgBitrate:  p.AvgBitrate,
		LastError:   p.LastError,
		Ctime:       p.Ctime,
		Mtime:       p.Mtime,
	}
}

// uidOf 返回 auth_key 的 uid 段（会话归属的 mid；游客为 0）。
// CDN 侧只把它当日志标识，不参与鉴权判定。
func uidOf(mid int64) string {
	if mid <= 0 {
		return "0"
	}
	return strconv.FormatInt(mid, 10)
}
