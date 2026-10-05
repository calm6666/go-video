package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RotateStreamKeyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRotateStreamKeyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RotateStreamKeyLogic {
	return &RotateStreamKeyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 轮转密钥：新密钥生效、旧密钥进入宽限期，重连不中断
//
// 并发纪律：事务内先 LockByID 锁旧密钥行，再「插新行 + LinkRotation 改旧行」，
// 两条写同事务提交，因此不会出现「新密钥已可用但旧密钥还 ACTIVE」的双活窗口；
// LinkRotation 只接受当前为 ACTIVE 的旧密钥，第二次轮转会在 CAS 上失败并回滚。
//
// force 的口径（见 README「与契约的偏差」）：轮转从不停止进行中的流——旧密钥在宽限期内
// 仍可重连，停流是 RevokeStreamKey/CloseStream 的职责。force 只影响回显的提示文案。
func (l *RotateStreamKeyLogic) RotateStreamKey(in *rpc.RotateStreamKeyReq) (*rpc.RotateStreamKeyReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}

	requestID, err := checkRequestID(in.RequestId)
	if err != nil {
		return nil, err
	}
	if in.KeyId <= 0 {
		return nil, model.ErrInvalidKeyId
	}
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}

	// 幂等首查：新密钥行带 request_id，命中即回放元数据（明文同样不可找回）。
	existing, err := repo.StreamKey.FindByIdempotencyRequest(l.ctx, requestID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		// 新密钥行的 grace_until 是 0：宽限截止属于被替换的旧密钥，回查它才能如实回显。
		var replayGrace int64
		if existing.PrevKeyID > 0 {
			prev, err := repo.StreamKey.FindOne(l.ctx, existing.PrevKeyID)
			if err != nil {
				return nil, err
			}
			if prev != nil {
				replayGrace = prev.GraceUntil
			}
		}
		return &rpc.RotateStreamKeyReply{
			KeyId:      existing.KeyID,
			PrevKeyId:  existing.PrevKeyID,
			GraceUntil: replayGrace,
			Version:    existing.Version,
			Replayed:   true,
			Message:    "相同 request_id 的轮转已完成；明文不可找回",
		}, nil
	}

	old, err := repo.StreamKey.FindOne(l.ctx, in.KeyId)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, model.ErrStreamKeyNotFound
	}
	// 契约里没有 admin 位，轮转只能由密钥归属的主播本人发起（README 缺口）。
	if in.OperatorMid != old.AnchorMid {
		return nil, model.ErrOperatorRequired
	}
	if old.State != model.KeyStateActive {
		return nil, model.ErrStreamKeyNotUsable
	}
	ttl, err := issueTtlSeconds(cfg.IssueTtlSeconds, cfg.MaxIssueTtlSeconds, in.TtlSeconds)
	if err != nil {
		return nil, err
	}
	grace := positiveOrDefault(cfg.RotateGraceSeconds, in.GraceSeconds)
	domain, err := publishDomain(l.svcCtx)
	if err != nil {
		return nil, err
	}

	newVersion := old.Version + 1
	keyRef, err := keyRefFor(old.StreamName, newVersion)
	if err != nil {
		return nil, err
	}
	plaintext, err := newPlaintextKey(cfg.KeyRandomBytes)
	if err != nil {
		return nil, err
	}

	now := nowUnix()
	graceUntil := now + grace
	var newKeyID int64
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		id, err := repo.StreamKey.Insert(ctx, tx, &model.StreamKey{
			StreamName:   old.StreamName, // 同一 stream_name 的下一代密钥
			KeyHash:      sha256Hex(plaintext),
			KeyRef:       keyRef,
			KeyTail:      keyTailOf(plaintext),
			RoomID:       old.RoomID,
			SessionID:    old.SessionID,
			AnchorMid:    old.AnchorMid,
			ProtocolMask: old.ProtocolMask,
			State:        model.KeyStateActive,
			Version:      newVersion,
			PrevKeyID:    old.KeyID,
			MaxStreams:   old.MaxStreams,
			ExpireAt:     now + ttl,
			RequestID:    requestID,
			TraceID:      sanitizeTraceID(in.TraceId),
		})
		if err != nil {
			return err
		}
		newKeyID = id

		linked, err := repo.StreamKey.LinkRotation(ctx, tx, old.KeyID, id, graceUntil)
		if err != nil {
			return err
		}
		if !linked {
			// 旧密钥已不是 ACTIVE：并发的第二路轮转，回滚保证「一 subject 一把活密钥」。
			return model.ErrStreamKeyNotUsable
		}
		return nil
	})
	if err != nil {
		if model.IsDuplicate(err) {
			replay, findErr := repo.StreamKey.FindByIdempotencyRequest(l.ctx, requestID)
			if findErr != nil {
				return nil, findErr
			}
			if replay != nil {
				return &rpc.RotateStreamKeyReply{
					KeyId: replay.KeyID, PrevKeyId: replay.PrevKeyID, GraceUntil: graceUntil,
					Version: replay.Version, Replayed: true,
					Message: "相同 request_id 的轮转已完成；明文不可找回",
				}, nil
			}
		}
		return nil, err
	}

	message := "旧密钥在宽限期内仍可重连，请按新地址重启编码器"
	if in.Force {
		message = "已强制轮转：进行中的推流未受影响，旧密钥在宽限期内继续可用"
	}
	return &rpc.RotateStreamKeyReply{
		KeyId:        newKeyID,
		PrevKeyId:    old.KeyID,
		PlaintextKey: plaintext,
		PublishUrl:   buildPublishURL(domain, old.StreamName, plaintext, old.ProtocolMask),
		GraceUntil:   graceUntil,
		Version:      newVersion,
		Message:      message,
	}, nil
}
