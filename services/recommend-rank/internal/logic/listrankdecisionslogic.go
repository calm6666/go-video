package logic

import (
	"context"
	"fmt"

	"go-video/services/recommend-rank/internal/svc"
	"go-video/services/recommend-rank/model"
	"go-video/services/recommend-rank/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRankDecisionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRankDecisionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRankDecisionsLogic {
	return &ListRankDecisionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查排序决策摘要（审计与实验核对）
//
// 读的是 rank_decision_log 事实表，本接口不掺任何推导：
// 每条回包能看到「当时用哪个 model_key/model_version、命中哪个 exp_key/variant_key/bucket_no、
// 是否降级、兜底策略是什么、过滤计数与前 N 个 aid」，足以回答「这批流量当时怎么被排的」。
//
// 边界（防无界扫描）：
//   - ps 超过 Rank.MaxDecisionPage 直接 ErrPageTooDeep，不静默截断（截断会让调用方以为拿全了）；
//   - offset 有硬上界（helpers.maxDecisionOffset）：这张表增长最快，深翻页是 OFFSET 扫行，
//     超过窗口就是「该改用时间窗」的信号；
//   - from_time > to_time 报错而不是回空列表（多半是两个参数写反）。
//
// has_more 由 model 侧「多取一行」得到，不用 COUNT(*)：审计读要的是「还有没有下一页」，
// 不是「总共有多少行」。
func (l *ListRankDecisionsLogic) ListRankDecisions(in *rpc.ListRankDecisionsReq) (*rpc.ListRankDecisionsReply, error) {
	if l.svcCtx == nil || l.svcCtx.Repository == nil {
		return nil, model.ErrRepositoryNotConfigured
	}
	if in == nil {
		return nil, fmt.Errorf("%w: 需要分页参数 pn/ps", model.ErrInvalidPage)
	}
	offset, limit, err := pageArgs(in.GetPn(), in.GetPs(), l.svcCtx.MaxDecisionPage)
	if err != nil {
		return nil, err
	}
	if err := checkTimeRange(in.GetFromTime(), in.GetToTime()); err != nil {
		return nil, err
	}
	expKey, err := optionalIdent("exp_key", in.GetExpKey(), colIdent)
	if err != nil {
		return nil, err
	}
	variantKey, err := optionalIdent("variant_key", in.GetVariantKey(), colIdent)
	if err != nil {
		return nil, err
	}
	modelKey, err := optionalIdent("model_key", in.GetModelKey(), colIdent)
	if err != nil {
		return nil, err
	}
	modelVersion, err := optionalIdent("model_version", in.GetModelVersion(), colIdent)
	if err != nil {
		return nil, err
	}
	scene, err := optionalIdent("scene", in.GetScene(), colIdent)
	if err != nil {
		return nil, err
	}

	rows, hasMore, err := l.svcCtx.Repository.ListDecisions(l.ctx, model.DecisionQuery{
		ExpKey:       expKey,
		VariantKey:   variantKey,
		ModelKey:     modelKey,
		ModelVersion: modelVersion,
		Scene:        scene,
		FromTime:     in.GetFromTime(),
		ToTime:       in.GetToTime(),
		OnlyDegraded: in.GetOnlyDegraded(),
		Offset:       offset,
		Limit:        limit,
	})
	if err != nil {
		return nil, err
	}
	return &rpc.ListRankDecisionsReply{Entries: decisionInfos(rows), HasMore: hasMore}, nil
}
