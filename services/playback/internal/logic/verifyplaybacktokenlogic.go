package logic

import (
	"context"
	"errors"
	"time"

	"go-video/services/playback/internal/signurl"
	"go-video/services/playback/internal/svc"
	"go-video/services/playback/model"
	"go-video/services/playback/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type VerifyPlaybackTokenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVerifyPlaybackTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VerifyPlaybackTokenLogic {
	return &VerifyPlaybackTokenLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// VerifyPlaybackToken 供 CDN 边缘/网关回源校验授权串与会话是否仍然有效。
//
// 判定顺序（任一不满足即 allow=false，并给出稳定 reason）：
//
//  1. 会话存在；
//  2. 会话未被撤销；
//  3. 请求资源与会话签发时绑定的 URI 一致（auth_key 不能挪用到别的对象）；
//  4. auth_key 签名正确且未过期（EnableAuthKey=false 时跳过，仅 dev/test）；
//  5. 会话 expire_at 未到期，到期时顺带把 state 推进为 expired。
//
// 首次放行时累加一次播放计数（Redis SETNX 去重）；计数失败只记日志，
// 不影响放行——播放可用性优先于统计完整性。
func (l *VerifyPlaybackTokenLogic) VerifyPlaybackToken(in *rpc.VerifyPlaybackTokenReq) (*rpc.VerifyPlaybackTokenReply, error) {
	if in.GetSessionId() == "" {
		return nil, model.ErrMissingSessionID
	}
	uri, err := signurl.NormalizeURI(in.GetUri())
	if err != nil {
		return nil, model.ErrInvalidObjectKey
	}
	now := time.Now().Unix()

	s, err := l.svcCtx.Repository.FindSession(l.ctx, in.GetSessionId(), now)
	if err != nil {
		l.Errorf("playback/VerifyPlaybackToken find session %s err=%v", in.GetSessionId(), err)
		return nil, err
	}
	if s == nil {
		l.Infof("playback/VerifyPlaybackToken session_not_found %s", in.GetSessionId())
		return denied(denySessionNotFound, 0, 0), nil
	}
	if s.State == model.SessionStateRevoked {
		return denied(denySessionRevoked, s.ExpireAt, 0), nil
	}
	if uri != s.Uri {
		// 会话绑定的对象与请求不一致：签名被挪用，直接拒绝。
		l.Infof("playback/VerifyPlaybackToken uri_mismatch session=%s want=%s got=%s", s.SessionId, s.Uri, uri)
		return denied(denyURIMismatch, s.ExpireAt, 0), nil
	}
	if l.svcCtx.Signer.Signed() {
		if _, err := l.svcCtx.Signer.Verify(uri, in.GetAuthKey(), now); err != nil {
			return denied(authDenyReason(err), s.ExpireAt, 0), nil
		}
	} else {
		l.Infof("playback/VerifyPlaybackToken auth_key 未启用（dev/test），仅校验会话有效期 session=%s", s.SessionId)
	}
	if s.ExpireAt <= now {
		if err := l.svcCtx.Repository.MarkSessionExpired(l.ctx, s.SessionId, now); err != nil {
			l.Errorf("playback/VerifyPlaybackToken mark expired %s err=%v", s.SessionId, err)
		}
		return denied(denySessionExpired, s.ExpireAt, 0), nil
	}

	count, err := l.svcCtx.Repository.CountPlayOnce(l.ctx, s, now)
	if err != nil {
		// 计数是旁路能力：Redis 故障时放行播放并告警，不返回错误。
		l.Errorf("playback/VerifyPlaybackToken count play session=%s content=%d/%d err=%v",
			s.SessionId, s.ContentType, s.ContentId, err)
	}
	return &rpc.VerifyPlaybackTokenReply{Allow: true, ExpireAt: s.ExpireAt, PlayCount: count}, nil
}

// denied 组装拒绝响应。deny_reason 是面向客户端与 CDN 日志的稳定枚举字符串，
// 不携带 Token、私钥或 SQL 等敏感信息（AGENTS.md §6）。
func denied(reason string, expireAt, playCount int64) *rpc.VerifyPlaybackTokenReply {
	return &rpc.VerifyPlaybackTokenReply{
		Allow:      false,
		DenyReason: reason,
		ExpireAt:   expireAt,
		PlayCount:  playCount,
	}
}

// authDenyReason 把签名校验错误映射为稳定 reason。
func authDenyReason(err error) string {
	switch {
	case errors.Is(err, signurl.ErrMalformedAuthKey):
		return denyBadAuthFormat
	case errors.Is(err, signurl.ErrAuthKeyExpired):
		return denySessionExpired
	case errors.Is(err, signurl.ErrSignatureMismatch):
		return denySignMismatch
	case errors.Is(err, signurl.ErrAuthKeyDisabled), errors.Is(err, signurl.ErrPrivateKeyRequired):
		// 服务端签名配置异常：宁可拒绝回源，也不放行无法验证的地址。
		return denySignerDisabled
	default:
		return denySignMismatch
	}
}
