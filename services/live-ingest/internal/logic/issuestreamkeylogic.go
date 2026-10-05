package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IssueStreamKeyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIssueStreamKeyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IssueStreamKeyLogic {
	return &IssueStreamKeyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 签发推流密钥：入库只有哈希与 Secret/Vault 引用，明文只在本响应出现一次
//
// 明文的生命周期：newPlaintextKey 出来的字符串只被三处消费——sha256Hex（入库）、
// keyTailOf（入库的末 4 位辨认串）、buildPublishURL（本响应）。函数返回后不再有任何引用，
// 既没有写日志，也没有进错误文案，更没有落库。
func (l *IssueStreamKeyLogic) IssueStreamKey(in *rpc.IssueStreamKeyReq) (*rpc.IssueStreamKeyReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, model.ErrCdnNotConfigured
	}

	requestID, err := checkRequestID(in.RequestId)
	if err != nil {
		return nil, err
	}
	if err := checkRoomID(in.RoomId); err != nil {
		return nil, err
	}
	if err := checkMid(in.AnchorMid); err != nil {
		return nil, err
	}
	if in.SessionId < 0 {
		return nil, model.ErrInvalidRoomId
	}
	mask, err := protocolMaskOf(in.Protocols)
	if err != nil {
		return nil, err
	}
	ttl, err := issueTtlSeconds(cfg.IssueTtlSeconds, cfg.MaxIssueTtlSeconds, in.TtlSeconds)
	if err != nil {
		return nil, err
	}
	// 推流域名白名单为空时直接拒签：给了密钥却没有可用地址，只会让主播按错地址推流。
	domain, err := publishDomain(l.svcCtx)
	if err != nil {
		return nil, err
	}

	// 幂等首查：uniq_request_id 命中说明这把密钥已经签发过，明文已不可找回。
	existing, err := repo.StreamKey.FindByIdempotencyRequest(l.ctx, requestID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &rpc.IssueStreamKeyReply{
			KeyId:      existing.KeyID,
			StreamName: existing.StreamName,
			Protocols:  maskToProtocols(existing.ProtocolMask),
			ExpireAt:   existing.ExpireAt,
			Replayed:   true,
		}, nil
	}

	maxStreams := maxStreamsPerKey(cfg.MaxStreamsPerKey, in.MaxStreams)
	streamName, err := deriveStreamName(in.RoomId)
	if err != nil {
		return nil, err
	}
	// 引用先于明文构造：构造不出来就整次签发失败，不退化成「只存哈希不留归属」。
	keyRef, err := keyRefFor(streamName, keyVersionFirst)
	if err != nil {
		return nil, err
	}
	plaintext, err := newPlaintextKey(cfg.KeyRandomBytes)
	if err != nil {
		return nil, err
	}

	now := nowUnix()
	keyID, err := repo.StreamKey.Insert(l.ctx, nil, &model.StreamKey{
		StreamName:   streamName,
		KeyHash:      sha256Hex(plaintext),
		KeyRef:       keyRef,
		KeyTail:      keyTailOf(plaintext),
		RoomID:       in.RoomId,
		SessionID:    in.SessionId,
		AnchorMid:    in.AnchorMid,
		ProtocolMask: mask,
		State:        model.KeyStateActive,
		Version:      keyVersionFirst,
		MaxStreams:   maxStreams,
		ExpireAt:     now + ttl,
		RequestID:    requestID,
		TraceID:      sanitizeTraceID(in.TraceId),
	})
	if err != nil {
		if model.IsDuplicate(err) {
			// 与并发签发撞 uniq_request_id：回查首次那一行回放，绝不发第二把密钥。
			replay, findErr := repo.StreamKey.FindByIdempotencyRequest(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if replay == nil {
				// 命中的是 uniq_key_hash 而不是幂等键：随机源出现重复哈希，属真实故障。
				return nil, err
			}
			return &rpc.IssueStreamKeyReply{
				KeyId:      replay.KeyID,
				StreamName: replay.StreamName,
				Protocols:  maskToProtocols(replay.ProtocolMask),
				ExpireAt:   replay.ExpireAt,
				Replayed:   true,
			}, nil
		}
		return nil, err
	}

	return &rpc.IssueStreamKeyReply{
		KeyId:        keyID,
		StreamName:   streamName,
		PlaintextKey: plaintext,
		PublishUrl:   buildPublishURL(domain, streamName, plaintext, mask),
		Protocols:    maskToProtocols(mask),
		ExpireAt:     now + ttl,
	}, nil
}
