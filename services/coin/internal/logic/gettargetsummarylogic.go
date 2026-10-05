package logic

import (
	"context"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetTargetSummaryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetTargetSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetTargetSummaryLogic {
	return &GetTargetSummaryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 单内容投币汇总。
//
// coin_count 直接按 cn_toss 聚合（state=ACTIVE），本服务不维护任何计数投影表：
// 多一份会漂移的事实源，就多一类「详情页显示 3 币、数据库 2 币」且无法自愈的故障。
// like_count 恒为 0 —— 点赞归 engagement 持有，这里留该字段只是让调用方别去猜（proto 注释已说明）。
// 未配置或已关闭缓存时全部回源 MySQL，缓存只影响延迟不影响正确性。
func (l *GetTargetSummaryLogic) GetTargetSummary(in *rpc.GetTargetSummaryReq) (*rpc.GetTargetSummaryReply, error) {
	if in.Aid <= 0 {
		return nil, model.ErrInvalidTargetAid
	}
	aids := []int64{in.Aid}

	if hit := readSummaryCache(l.ctx, l.svcCtx, aids); hit != nil {
		if v, ok := hit[in.Aid]; ok {
			return &rpc.GetTargetSummaryReply{Summary: summaryOf(in.Aid, v)}, nil
		}
	}

	agg, err := l.svcCtx.Tosses.SummarizeTargets(l.ctx, aids)
	if err != nil {
		return nil, err
	}
	v := cachedSummary{}
	if a, ok := agg[in.Aid]; ok {
		v = cachedSummary{CoinCount: a.CoinCount, CoinUserCount: a.CoinUserCount}
	}
	writeSummaryCache(l.ctx, l.svcCtx, map[int64]cachedSummary{in.Aid: v})

	return &rpc.GetTargetSummaryReply{Summary: summaryOf(in.Aid, v)}, nil
}

// summaryOf 把聚合结果投影成响应；无投币记录时给出全 0 的汇总而不是 nil，
// 详情页据此显示「0 币」而不是「查不到这条稿件的投币信息」。
func summaryOf(aid int64, v cachedSummary) *rpc.TargetCoinSummary {
	return &rpc.TargetCoinSummary{
		Aid:           aid,
		CoinCount:     v.CoinCount,
		CoinUserCount: v.CoinUserCount,
		LikeCount:     0,
	}
}
