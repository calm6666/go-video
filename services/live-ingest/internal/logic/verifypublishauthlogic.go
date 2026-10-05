package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type VerifyPublishAuthLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVerifyPublishAuthLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VerifyPublishAuthLogic {
	return &VerifyPublishAuthLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 接入鉴权（RTMP/SRT/WebRTC 入口在建连时调用）：比对哈希、协议、有效期、配额，可选建档 IDLE 流
//
// 明文纪律：入参 plaintext_key 只被 sha256Hex 消费一次，随后即被丢弃。
// 本函数任何分支都不把入参键写库、不写日志、不拼进错误文案；
// 「密钥不存在」与「密钥与流标识不匹配」返回同一个 reason，避免被枚举。
//
// 建档不是状态迁移：IDLE 流以 seq=0 落库，首个 live.state.v1 事件是 IDLE→PUBLISHING（seq=1）。
func (l *VerifyPublishAuthLogic) VerifyPublishAuth(in *rpc.VerifyPublishAuthReq) (*rpc.VerifyPublishAuthReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}

	streamName, ok := normalizeStreamName(in.StreamName)
	if !ok {
		// 非法流标识不进入查库：连索引都不碰，避免把垃圾输入变成一次全表最坏情况的查询。
		return nil, model.ErrPublishDenied
	}
	protocol := int32(in.Protocol)
	mask, valid := model.ProtocolMask(protocol)
	if !valid {
		return nil, model.ErrInvalidProtocol
	}
	if in.RoomId < 0 || in.CreateStream && in.RequestId == "" {
		// create_stream=true 时幂等键是「重放不重复建档」的唯一依据，缺了就拒。
		return nil, model.ErrIdempotencyKeyRequired
	}

	var requestID string
	if in.CreateStream {
		var err error
		if requestID, err = checkRequestID(in.RequestId); err != nil {
			return nil, err
		}
		// 建档幂等首查：uniq_publish_request 命中就回放同一 stream_id。
		// 放在查密钥之前：重放请求即使密钥已被吊销，也必须拿到当时建档的那条流。
		existing, err := repo.Stream.FindByPublishRequest(l.ctx, requestID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return &rpc.VerifyPublishAuthReply{
				Allowed: true, Reason: "replayed", KeyId: existing.KeyID, RoomId: existing.RoomID,
				SessionId: existing.SessionID, AnchorMid: existing.AnchorMid, StreamId: existing.StreamID,
				StreamState: rpcStreamState(existing.State), ServerTime: nowUnix(), Replayed: true,
			}, nil
		}
	}

	if in.PlaintextKey == "" {
		return deniedPublishReply(reasonKeyNotFound), nil
	}
	k, err := repo.StreamKey.FindByKeyHash(l.ctx, sha256Hex(in.PlaintextKey))
	if err != nil {
		return nil, err
	}
	if k == nil || k.StreamName != streamName {
		// 两种情况故意同码：不让「这把密钥存在但不归这个流」被探测出来。
		return deniedPublishReply(reasonKeyNotFound), nil
	}

	now := nowUnix()
	denied := func(reason string) (*rpc.VerifyPublishAuthReply, error) {
		return publishDenial(k, reason, now), nil
	}

	if k.RoomID != in.RoomId {
		// 房间与密钥绑定不一致：可能是主播拿错房间密钥，也可能是入口拼错，都不放行。
		return denied(reasonRoomMismatch)
	}
	if k.ProtocolMask&mask == 0 {
		return denied(reasonProtocolDenied)
	}
	if !model.AuthAllowedKeyState(k.State) {
		if k.State == model.KeyStateRevoked {
			return denied(reasonKeyRevoked)
		}
		return denied(reasonKeyExpired)
	}
	if k.ExpireAt > 0 && k.ExpireAt <= now {
		return denied(reasonKeyExpired)
	}
	if k.State == model.KeyStateRotating && k.GraceUntil <= now {
		// 宽限期已过：旧密钥只能等扫描器置 RETIRED，鉴权这边先拒。
		return denied(reasonKeyExpired)
	}

	active, err := repo.StreamKey.CountActiveStreams(l.ctx, k.KeyID)
	if err != nil {
		return nil, err
	}
	if active >= int64(k.MaxStreams) {
		if !in.CreateStream {
			// 纯探测：结构化回 reason，入口可据此给主播「同时推流数已达上限」的提示。
			return publishDenial(k, reasonQuotaExceeded, now), nil
		}
		return nil, model.ErrStreamQuotaExceeded
	}
	if !in.CreateStream {
		// 只做校验不建档。
		return &rpc.VerifyPublishAuthReply{
			Allowed: true, KeyId: k.KeyID, RoomId: k.RoomID, SessionId: k.SessionID, AnchorMid: k.AnchorMid,
			KeyExpireAt: k.ExpireAt, ServerTime: now,
		}, nil
	}

	streamID, err := newStreamID()
	if err != nil {
		return nil, err
	}
	var created *model.Stream
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		// 活跃指针必须在事务内占：两个并发建档只能有一个拿到它。
		claimed, err := repo.StreamKey.ClaimActiveStream(ctx, tx, k.KeyID, streamID)
		if err != nil {
			return err
		}
		if !claimed {
			// 指针已被占用：配额判定只是近似，这里才是「一把密钥一条活流」的硬闸门。
			return model.ErrStreamQuotaExceeded
		}
		row := &model.Stream{
			StreamID: streamID, KeyID: k.KeyID, StreamName: k.StreamName, RoomID: k.RoomID,
			SessionID: k.SessionID, AnchorMid: k.AnchorMid, Protocol: protocol,
			State: model.StreamStateIdle, Seq: 0, PublishRequestID: requestID,
			TraceID: sanitizeTraceID(in.TraceId),
		}
		if err := repo.Stream.Insert(ctx, tx, row); err != nil {
			return err
		}
		created = row
		return nil
	})
	if err != nil {
		if model.IsDuplicate(err) {
			// uniq_publish_request 被并发抢先：回放对手建出来的那条流，绝不建第二条。
			existing, findErr := repo.Stream.FindByPublishRequest(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if existing == nil {
				return nil, err
			}
			return &rpc.VerifyPublishAuthReply{
				Allowed: true, Reason: "replayed", KeyId: existing.KeyID, RoomId: existing.RoomID,
				SessionId: existing.SessionID, AnchorMid: existing.AnchorMid, StreamId: existing.StreamID,
				StreamState: rpcStreamState(existing.State), KeyExpireAt: k.ExpireAt,
				ServerTime: nowUnix(), Replayed: true,
			}, nil
		}
		return nil, err
	}

	return &rpc.VerifyPublishAuthReply{
		Allowed: true, KeyId: k.KeyID, RoomId: k.RoomID, SessionId: k.SessionID, AnchorMid: k.AnchorMid,
		StreamId: created.StreamID, StreamState: rpcStreamState(created.State),
		KeyExpireAt: k.ExpireAt, ServerTime: now,
	}, nil
}

// publishDenial 组装拒绝响应：带上密钥归属信息但不带任何凭据材料，
// 入口据此回一句人话即可，无需知道是哪一步没通过（reason 已给出稳定口径）。
func publishDenial(k *model.StreamKey, reason string, now int64) *rpc.VerifyPublishAuthReply {
	return &rpc.VerifyPublishAuthReply{
		Allowed: false, Reason: reason, KeyId: k.KeyID, RoomId: k.RoomID, SessionId: k.SessionID,
		AnchorMid: k.AnchorMid, KeyExpireAt: k.ExpireAt, ServerTime: now,
	}
}

func deniedPublishReply(reason string) *rpc.VerifyPublishAuthReply {
	return &rpc.VerifyPublishAuthReply{Allowed: false, Reason: reason, ServerTime: nowUnix()}
}
