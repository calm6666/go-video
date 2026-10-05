package logic

import (
	"context"
	"errors"
	"time"

	"go-video/common/idgen"
	"go-video/services/playback/internal/signurl"
	"go-video/services/playback/internal/svc"
	"go-video/services/playback/model"
	"go-video/services/playback/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetPlaybackTokenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetPlaybackTokenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPlaybackTokenLogic {
	return &GetPlaybackTokenLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetPlaybackToken 校验播放资格、创建播放会话并返回短期防盗链地址。
//
// 版权校验口径（AGENTS.md §5/§8）：
//   - PGC（电影/电视剧/番剧的一集）必须调用 rights 的 CheckPlayable，
//     窗口不存在/过期/撤权/地区不匹配一律返回 ErrCopyrightWindowUnavailable；
//     rights 不可用时同样拒绝签发，绝不退化为"默认可播"。
//   - UGC 稿件不调用 rights：整片版权内容不允许通过普通投稿发布（§1），
//     UGC 的可播放性由稿件自身的发布状态与可见范围决定，那是 video 服务的领域，
//     由 gateway/app 在调用本方法前完成校验（见 services/playback/README.md）。
//     playback 只负责"给谁、在什么时间窗内、能取哪个对象"这件事。
//
// 幂等：request_id 上有唯一索引，同一 request_id 重放返回同一 session_id 与同一
// 授权到期时间（auth_key 的随机串会重新生成，但有效期不延长）。
func (l *GetPlaybackTokenLogic) GetPlaybackToken(in *rpc.GetPlaybackTokenReq) (*rpc.GetPlaybackTokenReply, error) {
	contentType, err := contentTypeValue(in.GetContentType())
	if err != nil {
		return nil, err
	}
	if in.GetContentId() <= 0 {
		return nil, model.ErrInvalidContentID
	}
	platform, err := platformValue(in.GetPlatform())
	if err != nil {
		return nil, err
	}
	if in.GetRequestId() == "" {
		return nil, model.ErrMissingRequestID
	}
	uri, err := signurl.NormalizeURI(in.GetObjectKey())
	if err != nil {
		return nil, model.ErrInvalidObjectKey
	}
	if contentType == model.ContentTypePGC && in.GetRegion() == "" {
		return nil, model.ErrMissingRegion
	}

	now := time.Now().Unix()

	// PGC 必须过版权窗口；窗口结束时间用于给授权封顶。
	var windowEnd int64
	if contentType == model.ContentTypePGC {
		playable, end, err := l.svcCtx.Rights.CheckPlayable(l.ctx, in.GetContentId(), contentType, in.GetRegion())
		if err != nil {
			l.Errorf("playback/GetPlaybackToken rights check content_id=%d region=%s err=%v",
				in.GetContentId(), in.GetRegion(), err)
			return nil, err
		}
		if !playable {
			l.Infof("playback/GetPlaybackToken denied by rights window: content_id=%d region=%s mid=%d",
				in.GetContentId(), in.GetRegion(), in.GetMid())
			return nil, model.ErrCopyrightWindowUnavailable
		}
		windowEnd = end
	}

	expireAt, err := clampExpireAt(now, l.svcCtx.Signer.TTL(), windowEnd)
	if err != nil {
		return nil, err
	}

	// 幂等重放：同一 request_id 直接复用已创建的会话。
	existing, err := l.svcCtx.Repository.FindSessionByRequest(l.ctx, in.GetRequestId())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if err := checkSameRequest(existing, in); err != nil {
			return nil, err
		}
		if existing.State == model.SessionStateRevoked {
			return nil, model.ErrSessionRevoked
		}
		if existing.ExpireAt <= now {
			return nil, model.ErrSessionExpired
		}
		return l.reply(existing.SessionId, existing.Uri, existing.Mid, existing.ExpireAt)
	}

	sessionId, err := idgen.ULID()
	if err != nil {
		return nil, err
	}
	s := &model.PlaybackSession{
		SessionId:   sessionId,
		ContentType: contentType,
		ContentId:   in.GetContentId(),
		Vid:         in.GetVid(),
		Mid:         in.GetMid(),
		Platform:    platform,
		AppVersion:  in.GetAppVersion(),
		Region:      in.GetRegion(),
		ObjectKey:   in.GetObjectKey(),
		Uri:         uri,
		RequestId:   in.GetRequestId(),
		ExpireAt:    expireAt,
		State:       model.SessionStateActive,
		TraceId:     in.GetTraceId(),
		Ctime:       now,
		Mtime:       now,
	}
	if err := l.svcCtx.Repository.CreateSession(l.ctx, s); err != nil {
		if errors.Is(err, model.ErrDuplicateRequest) {
			// 并发重放：另一个请求已经插入同一 request_id，改为复用它的会话。
			raced, ferr := l.svcCtx.Repository.FindSessionByRequest(l.ctx, in.GetRequestId())
			if ferr != nil {
				return nil, ferr
			}
			if raced != nil {
				return l.reply(raced.SessionId, raced.Uri, raced.Mid, raced.ExpireAt)
			}
		}
		l.Errorf("playback/GetPlaybackToken create session content_id=%d mid=%d err=%v",
			in.GetContentId(), in.GetMid(), err)
		return nil, err
	}
	// 新会话立刻进缓存，避免首个分片回源就打穿数据库。
	if err := l.svcCtx.Repository.CacheSession(l.ctx, s, now); err != nil {
		l.Errorf("playback/GetPlaybackToken cache session %s err=%v", sessionId, err)
	}
	return l.reply(s.SessionId, s.Uri, s.Mid, s.ExpireAt)
}

// reply 对会话的 URI 签名并组装响应。签名失败（私钥未配置等）返回明确错误，
// 不返回未签名地址（AGENTS.md §6）。
func (l *GetPlaybackTokenLogic) reply(sessionId, uri string, mid, expireAt int64) (*rpc.GetPlaybackTokenReply, error) {
	authKey, err := l.svcCtx.Signer.Sign(uri, uidOf(mid), expireAt, "")
	if err != nil {
		return nil, signError(err)
	}
	if !l.svcCtx.Signer.Signed() {
		// 仅 dev/test 会走到这里：签名关闭时地址没有防盗链保护。
		l.Infof("playback/GetPlaybackToken auth_key 未启用（dev/test），返回未签名地址 session=%s", sessionId)
	}
	return &rpc.GetPlaybackTokenReply{
		SessionId: sessionId,
		PlayUrl:   l.svcCtx.Signer.BuildURL(uri, authKey),
		AuthKey:   authKey,
		ExpireAt:  expireAt,
		// ttl 供客户端与网关做缓存；签名地址不应长期缓存（docs/api-and-events.md §1.1）。
		Ttl:   expireAt - time.Now().Unix(),
		KeyId: l.svcCtx.Signer.KeyID(),
	}, nil
}

// checkSameRequest 校验幂等重放的请求体与原请求一致，防止 request_id 被串用到
// 另一个用户或另一个内容上。
func checkSameRequest(s *model.PlaybackSession, in *rpc.GetPlaybackTokenReq) error {
	if s.Mid != in.GetMid() || s.ContentType != int32(in.GetContentType()) || s.ContentId != in.GetContentId() {
		return model.ErrSessionIDConflict
	}
	return nil
}
