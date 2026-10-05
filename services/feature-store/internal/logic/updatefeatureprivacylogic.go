package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type UpdateFeaturePrivacyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateFeaturePrivacyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateFeaturePrivacyLogic {
	return &UpdateFeaturePrivacyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// privacySnapshot 是隐私调整回执的可回放快照。
type privacySnapshot struct {
	FeatureKey   string `json:"k"`
	Version      int32  `json:"v"`
	PrivacyLevel int32  `json:"p"`
}

// 调整隐私级别（独立入口，单独留痕）
//
// 隐私级别是「谁能读、是否进入个体删除范围」的唯一判据，所以：
//   - 不允许 UNSPECIFIED（「不知道多敏感」的特征不允许存在）；
//   - 必须与 entity_scope 自洽（PrivacyMatchesScope），否则个体维度能被标成聚合级绕过授权；
//   - 更新走 CAS（WHERE privacy_level = 旧值），两个并发调整不会互相覆盖后只剩一条审计；
//   - 收紧级别时清理 fs:active:<key>，避免旧缓存继续被低授权调用方命中。
func (l *UpdateFeaturePrivacyLogic) UpdateFeaturePrivacy(
	in *rpc.UpdateFeaturePrivacyReq) (*rpc.UpdateFeaturePrivacyReply, error) {
	key := strings.TrimSpace(in.GetFeatureKey())
	op := strings.TrimSpace(in.GetOperator())
	if err := checkFeatureKey(key); err != nil {
		return nil, err
	}
	if err := checkVersion(in.GetVersion()); err != nil {
		return nil, err
	}
	if err := checkOperator(op); err != nil {
		return nil, err
	}
	if err := checkReason(in.GetReason()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	to := int32(in.GetPrivacyLevel())
	if to == model.PrivacyUnspecified {
		return nil, model.ErrPrivacyUnsetNotAllowed
	}
	if !model.ValidPrivacyLevel(to) {
		return nil, fmt.Errorf("%w: privacy_level %d", model.ErrPrivacyUnsetNotAllowed, to)
	}

	spec := receiptSpec{
		requestID:  strings.TrimSpace(in.GetRequestId()),
		opType:     model.ReceiptOpPrivacyChange,
		featureKey: key,
		version:    in.GetVersion(),
		rowCount:   1,
		operator:   op,
		reason:     strings.TrimSpace(in.GetReason()),
	}
	res, owner, err := beginReceipt(l.ctx, l.svcCtx, l.Logger, spec)
	if err != nil {
		return nil, err
	}
	if !res.Execute {
		return l.replay(res.Receipt)
	}

	def, err := l.svcCtx.Definitions.FindOne(l.ctx, key, in.GetVersion())
	if err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	if !model.PrivacyMatchesScope(def.EntityScope, to) {
		e := fmt.Errorf("%w: scope %d cannot carry level %d", model.ErrPrivacyScopeMismatch,
			def.EntityScope, to)
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, e)
		return nil, e
	}
	if def.PrivacyLevel == to {
		// 重复提交同一目标级别：幂等成功，不追加 from==to 的噪声审计。
		return l.finish(spec, owner, def, true)
	}

	if err := l.applyInTx(def, to, spec, op); err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	if to > def.PrivacyLevel {
		// 收紧：清掉指针缓存，让下一次读重新解析并受新级别约束。
		invalidateCache(l.ctx, l.svcCtx, l.Logger, []string{model.ActiveVersionCacheKey(key)})
	}
	stored, err := l.svcCtx.Definitions.FindOne(l.ctx, key, def.Version)
	if err != nil {
		return nil, err
	}
	l.Infow("feature privacy updated", logx.Field("feature_key", key),
		logx.Field("version", def.Version), logx.Field("from", def.PrivacyLevel),
		logx.Field("to", to), logx.Field("operator", op))
	return l.finish(spec, owner, stored, false)
}

// applyInTx 把「CAS 改级别」与「追加审计」放同一事务：
// 只改成功没留痕、或只留痕没改成功，都是不可接受的中间态。
func (l *UpdateFeaturePrivacyLogic) applyInTx(def *model.FeatureDefinition, to int32,
	spec receiptSpec, op string) error {
	return l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		ok, err := l.svcCtx.Definitions.UpdatePrivacy(ctx, def.FeatureKey, def.Version,
			def.PrivacyLevel, to)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: privacy_level changed concurrently on %s@v%d",
				model.ErrVersionConflict, def.FeatureKey, def.Version)
		}
		_, err = l.svcCtx.Switches.Append(ctx, tx, &model.VersionSwitch{
			FeatureKey:  def.FeatureKey,
			SwitchType:  model.SwitchTypePrivacyChange,
			FromVersion: def.Version,
			ToVersion:   def.Version,
			FromValue:   strconv.FormatInt(int64(def.PrivacyLevel), 10),
			ToValue:     strconv.FormatInt(int64(to), 10),
			// 隐私调整不改值语义，两侧定义摘要相同：这一列记的是「改完之后口径仍是这个」。
			FromDigest: def.DefinitionDigest,
			ToDigest:   def.DefinitionDigest,
			Operator:   clip(op, maxOperatorLen),
			Reason:     clip(spec.reason, maxReasonLen),
			RequestID:  spec.requestID,
			TraceID:    traceIDOf(ctx),
		})
		return err
	})
}

func (l *UpdateFeaturePrivacyLogic) finish(spec receiptSpec, owner string,
	def *model.FeatureDefinition, reused bool) (*rpc.UpdateFeaturePrivacyReply, error) {
	raw := marshalJSON(privacySnapshot{FeatureKey: def.FeatureKey, Version: def.Version,
		PrivacyLevel: def.PrivacyLevel})
	if err := finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   raw,
		ResultDigest: resultDigest(raw),
		AffectedRows: 1,
		DetailKept:   true,
	}); err != nil {
		return nil, err
	}
	return &rpc.UpdateFeaturePrivacyReply{Definition: defToProto(def), Reused: reused}, nil
}

func (l *UpdateFeaturePrivacyLogic) replay(receipt *model.WriteReceipt) (*rpc.UpdateFeaturePrivacyReply, error) {
	var snap privacySnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: privacy receipt unreadable: %w", err)
	}
	def, err := l.svcCtx.Definitions.FindOne(l.ctx, snap.FeatureKey, snap.Version)
	if err != nil {
		return nil, err
	}
	return &rpc.UpdateFeaturePrivacyReply{Definition: defToProto(def), Reused: true}, nil
}
