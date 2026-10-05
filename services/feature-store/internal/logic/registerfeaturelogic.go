package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RegisterFeatureLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegisterFeatureLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegisterFeatureLogic {
	return &RegisterFeatureLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// registerSnapshot 是注册回执的可回放快照（远小于 result_json 的 2 KiB 上限）。
type registerSnapshot struct {
	FeatureKey string `json:"k"`
	Version    int32  `json:"v"`
	Created    bool   `json:"c"`
}

// 注册特征版本（privacy_level 必填，不可变字段命中冲突时拒绝）
//
// 校验顺序（先范围、再形态、后自洽）由 model.ValidateFeatureDefinition 一处承担；
// 命中已有 (feature_key, version) 时逐字段比不可变摘要：全等 reused=true，
// 任一不同 ErrFeatureDefinitionImmutable，TTL/默认值不同 ErrFeatureMetadataImmutable。
// 新版本一律以 DRAFT 入库：ACTIVE 只能由 UpdateFeatureState/SwitchFeatureVersion 达成，
// 注册即生效等于绕过评审。
func (l *RegisterFeatureLogic) RegisterFeature(in *rpc.RegisterFeatureReq) (*rpc.RegisterFeatureReply, error) {
	op := strings.TrimSpace(in.GetOperator())
	if err := checkOperator(op); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	src := in.GetDefinition()
	if src == nil {
		return nil, model.ErrFeatureKeyRequired
	}
	if s := int32(src.GetState()); s != 0 && s != model.FeatureStateDraft {
		return nil, fmt.Errorf("%w: registered versions always start as DRAFT",
			model.ErrFeatureStateTransition)
	}

	def := &model.FeatureDefinition{
		FeatureKey:    strings.TrimSpace(src.GetFeatureKey()),
		Version:       src.GetVersion(),
		Name:          src.GetName(),
		ValueType:     int32(src.GetValueType()),
		EntityScope:   int32(src.GetEntityScope()),
		Source:        int32(src.GetSource()),
		PrivacyLevel:  int32(src.GetPrivacyLevel()),
		WindowSeconds: src.GetWindowSeconds(),
		TTLSeconds:    src.GetTtlSeconds(),
		DefaultValue:  src.GetDefaultValue(),
		Dimension:     src.GetDimension(),
		State:         model.FeatureStateDraft,
		Description:   src.GetDescription(),
		ChangeNote:    src.GetChangeNote(),
		CreatedBy:     clip(op, maxOperatorLen),
	}
	if err := model.ValidateFeatureDefinition(def); err != nil {
		return nil, err
	}
	def.FillDigests()

	spec := receiptSpec{
		requestID:  strings.TrimSpace(in.GetRequestId()),
		opType:     model.ReceiptOpRegister,
		featureKey: def.FeatureKey,
		version:    def.Version,
		rowCount:   1,
		operator:   op,
	}
	res, owner, err := beginReceipt(l.ctx, l.svcCtx, l.Logger, spec)
	if err != nil {
		return nil, err
	}
	if !res.Execute {
		return l.replay(res.Receipt)
	}
	// 取得执行权后的任何失败都要落 MarkFailed：失败不占用执行权，
	// 同一 request_id 可以重试（幂等键的意义是「同一件事只发生一次」）。
	switch existing, findErr := l.svcCtx.Definitions.FindOne(l.ctx, def.FeatureKey, def.Version); {
	case findErr == nil:
		if rejectErr := l.reuseOrReject(existing, def); rejectErr != nil {
			failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, rejectErr)
			return nil, rejectErr
		}
		snap := registerSnapshot{FeatureKey: def.FeatureKey, Version: def.Version, Created: false}
		if err := l.finish(spec, owner, snap, 0); err != nil {
			return nil, err
		}
		return &rpc.RegisterFeatureReply{Reused: true, Definition: defToProto(existing)}, nil
	case errors.Is(findErr, model.ErrFeatureNotFound):
		// 走插入分支。
	default:
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, findErr)
		return nil, findErr
	}

	if err := l.svcCtx.Definitions.Insert(l.ctx, def); err != nil {
		if errors.Is(err, model.ErrFeatureVersionExists) {
			// 并发注册：对方先插了同一版本，按同一条幂等规则重判一次。
			existing, findErr := l.svcCtx.Definitions.FindOne(l.ctx, def.FeatureKey, def.Version)
			if findErr != nil {
				failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, findErr)
				return nil, findErr
			}
			if rejectErr := l.reuseOrReject(existing, def); rejectErr != nil {
				failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, rejectErr)
				return nil, rejectErr
			}
			snap := registerSnapshot{FeatureKey: def.FeatureKey, Version: def.Version, Created: false}
			if finishErr := l.finish(spec, owner, snap, 0); finishErr != nil {
				return nil, finishErr
			}
			return &rpc.RegisterFeatureReply{Reused: true, Definition: defToProto(existing)}, nil
		}
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}

	// 注册新 key 时幂等建立指针行（active_version=0）：没有指针行的 key 在读路径上
	// 无法与「未注册」区分，版本切换也无从 CAS。Ensure 不覆盖已有指针。
	if err := l.ensurePointerRow(def.FeatureKey); err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	snap := registerSnapshot{FeatureKey: def.FeatureKey, Version: def.Version, Created: true}
	if err := l.finish(spec, owner, snap, 1); err != nil {
		return nil, err
	}
	l.Infow("feature version registered", logx.Field("feature_key", def.FeatureKey),
		logx.Field("version", def.Version), logx.Field("operator", op))
	return &rpc.RegisterFeatureReply{Created: true, Definition: defToProto(def)}, nil
}

// finish 落回执快照（明细可完整回放：注册结果只有 key/version/created 三项）。
func (l *RegisterFeatureLogic) finish(spec receiptSpec, owner string,
	snap registerSnapshot, affected int64) error {
	raw := marshalJSON(snap)
	return finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   raw,
		ResultDigest: resultDigest(raw),
		AffectedRows: affected,
		DetailKept:   true,
	})
}

// reuseOrReject 判定「同一版本的重复注册」是幂等复用还是原地改写。
func (l *RegisterFeatureLogic) reuseOrReject(existing, want *model.FeatureDefinition) error {
	if existing == nil {
		return fmt.Errorf("%w: %s@v%d", model.ErrFeatureNotFound, want.FeatureKey, want.Version)
	}
	if !existing.SameImmutableAs(want) {
		return fmt.Errorf("%w: %s@v%d", model.ErrFeatureDefinitionImmutable,
			want.FeatureKey, want.Version)
	}
	if !existing.SameDefinitionAs(want) {
		return fmt.Errorf("%w: %s@v%d", model.ErrFeatureMetadataImmutable,
			want.FeatureKey, want.Version)
	}
	if existing.PrivacyLevel != want.PrivacyLevel {
		// 隐私级别不进注册摘要（它由 UpdateFeaturePrivacy 独立变更并留审计），
		// 但也不能让一次注册悄悄改掉它。
		return fmt.Errorf("%w: privacy_level differs, use UpdateFeaturePrivacy for an audited change",
			model.ErrFeatureMetadataImmutable)
	}
	return nil
}

// ensurePointerRow 在事务里幂等建指针行（model.Ensure 要求 session：脱离事务的写入
// 与后续切换之间没有隔离边界，宁可显式起一个短事务）。
func (l *RegisterFeatureLogic) ensurePointerRow(featureKey string) error {
	return l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		return l.svcCtx.ActiveVersions.Ensure(ctx, tx, featureKey)
	})
}

// replay 按首次回执回放注册结果：created 取快照值，definition 取库里当前行
// （已有版本不可原地改写，两者必然一致；状态若已推进，回放的也是真实的那一份）。
func (l *RegisterFeatureLogic) replay(receipt *model.WriteReceipt) (*rpc.RegisterFeatureReply, error) {
	var snap registerSnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: register receipt unreadable: %w", err)
	}
	def, err := l.svcCtx.Definitions.FindOne(l.ctx, snap.FeatureKey, snap.Version)
	if err != nil {
		return nil, err
	}
	return &rpc.RegisterFeatureReply{
		Created:    snap.Created,
		Reused:     true,
		Definition: defToProto(def),
	}, nil
}
