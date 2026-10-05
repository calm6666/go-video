package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/feature-store/internal/svc"
	"go-video/services/feature-store/model"
	"go-video/services/feature-store/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type EraseEntityFeaturesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewEraseEntityFeaturesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EraseEntityFeaturesLogic {
	return &EraseEntityFeaturesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// maxEraseRounds 是单次擦除的分批轮数保险（轮数 × 每轮上限 = 一个主体可擦除的行数上界）。
// 超出即报错而不是「擦到这儿算完成」：隐私工单的完成判定不能靠一个循环没跑完。
const maxEraseRounds = 100

// eraseSnapshot 是擦除回执的可回放快照。
type eraseSnapshot struct {
	Erased  int32 `json:"e"`
	Touched int32 `json:"t"`
}

// 按主体删除个体特征（隐私工单执行）
//
// 这是本服务权限最高的一条路径，四道闸缺一不可：
//  1. operator 必须落在 Privacy.OperatorPrefixes 白名单内（空白名单一律拒绝，fail closed）；
//  2. 擦除覆盖该主体的全部版本残值，不只当前 ACTIVE 视图 —— 只抹当前视图等于没删；
//  3. 先选行、按主键删、删完立即 DEL 命中的 fs:val:* 键：
//     「库里删了、缓存还在」意味着擦除后仍能读到已删的个体数据；
//  4. 收尾前复查一次「还有没有剩」，有则报失败而不是谎报擦干净了。
//
// 定义与版本切换审计保留（它们描述的是特征口径，不含任何主体标识）。
// 日志与响应都不出现 entity_id：它本身就是个体标识（AGENTS.md §6）。
func (l *EraseEntityFeaturesLogic) EraseEntityFeatures(
	in *rpc.EraseEntityFeaturesReq) (*rpc.EraseEntityFeaturesReply, error) {
	scope, entityID, err := checkEntity(in.GetEntity())
	if err != nil {
		return nil, err
	}
	requestID := strings.TrimSpace(in.GetRequestId())
	operator := strings.TrimSpace(in.GetOperator())
	reason := strings.TrimSpace(in.GetReason())
	if err := checkRequestID(requestID); err != nil {
		return nil, err
	}
	if err := checkOperator(operator); err != nil {
		return nil, err
	}
	if err := checkReason(reason); err != nil {
		return nil, err
	}
	// 空白名单 = 谁都不能擦除：隐私能力不可用是合规缺陷，不能退化成「默认放行」。
	if !model.OperatorInPrefixes(operator, l.svcCtx.Config.Privacy.OperatorPrefixes) {
		return nil, fmt.Errorf("%w: operator is not in the privacy allowlist", model.ErrPrivacyOperatorForbidden)
	}
	minPrivacy := int32(in.GetMinPrivacyLevel())
	if minPrivacy != model.PrivacyUnspecified && !model.ValidPrivacyLevel(minPrivacy) {
		return nil, fmt.Errorf("%w: min_privacy_level %d", model.ErrPrivacyUnsetNotAllowed, minPrivacy)
	}
	limit := l.svcCtx.PurgeLimit()

	spec := receiptSpec{
		requestID:   requestID,
		opType:      model.ReceiptOpErase,
		entityScope: scope,
		entityID:    entityID,
		rowCount:    limit,
		operator:    operator,
		reason:      reason,
	}
	res, owner, err := beginReceipt(l.ctx, l.svcCtx, l.Logger, spec)
	if err != nil {
		return nil, err
	}
	if !res.Execute {
		return l.replay(res.Receipt)
	}

	var erased int64
	features := make(map[string]struct{})
	for round := 0; round < maxEraseRounds; round++ {
		// ListForErase 与 DeleteByEntity 同一谓词（不按 ACTIVE 指针与状态收窄），
		// 因此「选出来的行」就是「要删的行」，缓存清理的键集合不会与删除集合错位。
		rows, err := l.svcCtx.Values.ListForErase(l.ctx, scope, entityID, minPrivacy, limit)
		if err != nil {
			failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		ids := make([]int64, 0, len(rows))
		keys := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ValueID)
			keys = append(keys, r.Key().CacheKey())
			features[r.FeatureKey] = struct{}{}
		}
		deleted, err := l.svcCtx.Values.DeleteByIDs(l.ctx, ids)
		if err != nil {
			failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
			return nil, err
		}
		erased += deleted
		invalidateCache(l.ctx, l.svcCtx, l.Logger, keys)
		if len(rows) < int(limit) {
			break // 没取满一批 = 已经是最后一页
		}
	}
	// 收尾复查：仍有残值说明没擦完（轮数保险触发，或期间又写进来新值），必须报错。
	leftover, err := l.svcCtx.Values.ListForErase(l.ctx, scope, entityID, minPrivacy, 1)
	if err != nil {
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, err)
		return nil, err
	}
	if len(leftover) > 0 {
		e := fmt.Errorf("%w: entity still has feature rows after %d rounds", model.ErrTooManyRows, maxEraseRounds)
		failReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, e)
		return nil, e
	}

	touched := int32(len(features))
	raw := marshalJSON(eraseSnapshot{Erased: int32(erased), Touched: touched})
	if err := finishReceipt(l.ctx, l.svcCtx, l.Logger, spec, owner, model.ReceiptSnapshot{
		ResultJSON:   raw,
		ResultDigest: resultDigest(raw),
		AffectedRows: erased,
		DetailKept:   true,
	}); err != nil {
		return nil, err
	}
	l.Infow("entity features erased", logx.Field("entity_scope", scope),
		logx.Field("erased_rows", erased), logx.Field("features_touched", touched),
		logx.Field("min_privacy_level", minPrivacy), logx.Field("operator", operator))
	return &rpc.EraseEntityFeaturesReply{ErasedRows: int32(erased), FeaturesTouched: touched}, nil
}

// replay 同一 request_id 重放：返回首次计数，绝不重复擦除一遍。
func (l *EraseEntityFeaturesLogic) replay(receipt *model.WriteReceipt) (*rpc.EraseEntityFeaturesReply, error) {
	var snap eraseSnapshot
	if err := unmarshalJSON(receipt.ResultJSON, &snap); err != nil {
		return nil, fmt.Errorf("feature-store: erase receipt unreadable: %w", err)
	}
	return &rpc.EraseEntityFeaturesReply{ErasedRows: snap.Erased, FeaturesTouched: snap.Touched,
		Reused: true}, nil
}
