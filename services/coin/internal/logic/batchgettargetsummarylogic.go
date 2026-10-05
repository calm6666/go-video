package logic

import (
	"context"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BatchGetTargetSummaryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBatchGetTargetSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchGetTargetSummaryLogic {
	return &BatchGetTargetSummaryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 列表页批量汇总。
//
// aids 先去重再裁剪到 Coin.MaxBatchAids：
//   - 不去重会让同一 aid 出现两次时聚合语句 IN 列表膨胀；
//   - 裁剪而不是报错，是因为列表页的屏宽由客户端决定，超量只应少给几条汇总，
//     不该让整个 feed 接口失败（缺的那几条由调用方下一屏再取）。
//
// 请求里给了 aid 但全部非法（<=0）时按参数错误处理：那是调用方 bug，静默返回空数组
// 会让整屏卡片显示「0 币」，把错误伪装成事实。
func (l *BatchGetTargetSummaryLogic) BatchGetTargetSummary(in *rpc.BatchGetTargetSummaryReq) (*rpc.BatchGetTargetSummaryReply, error) {
	coin := l.svcCtx.Coin()
	aids, dropped := normalizeAids(in.Aids, coin.MaxBatchAids)
	if len(in.Aids) > 0 && len(aids) == 0 {
		return nil, model.ErrInvalidAids
	}
	if dropped > 0 {
		l.Logger.Infof("coin: BatchGetTargetSummary 裁剪 %d 个 aid（上限 %d/次）", dropped, coin.MaxBatchAids)
	}
	if len(aids) == 0 {
		// 空入参是合法的空请求，返回 [] 而不是 null。
		return &rpc.BatchGetTargetSummaryReply{Summaries: []*rpc.TargetCoinSummary{}}, nil
	}

	out := make([]*rpc.TargetCoinSummary, 0, len(aids))
	values := make(map[int64]cachedSummary, len(aids))
	if hit := readSummaryCache(l.ctx, l.svcCtx, aids); hit != nil {
		for aid, v := range hit {
			values[aid] = v
		}
	}

	var missing []int64
	for _, aid := range aids {
		if _, ok := values[aid]; !ok {
			missing = append(missing, aid)
		}
	}
	if len(missing) > 0 {
		agg, err := l.svcCtx.Tosses.SummarizeTargets(l.ctx, missing)
		if err != nil {
			return nil, err
		}
		fresh := make(map[int64]cachedSummary, len(missing))
		for _, aid := range missing {
			v := cachedSummary{}
			if a, ok := agg[aid]; ok {
				v = cachedSummary{CoinCount: a.CoinCount, CoinUserCount: a.CoinUserCount}
			}
			fresh[aid] = v
		}
		writeSummaryCache(l.ctx, l.svcCtx, fresh)
		for aid, v := range fresh {
			values[aid] = v
		}
	}

	for _, aid := range aids {
		out = append(out, summaryOf(aid, values[aid]))
	}
	return &rpc.BatchGetTargetSummaryReply{Summaries: out}, nil
}

// normalizeAids 去重保序并裁剪到上限，返回被丢弃的数量（含重复与超限两部分）。
func normalizeAids(in []int64, maxAids int64) ([]int64, int) {
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	dropped := 0
	for _, aid := range in {
		if aid <= 0 {
			dropped++
			continue
		}
		if _, ok := seen[aid]; ok {
			dropped++
			continue
		}
		if maxAids > 0 && int64(len(out)) >= maxAids {
			dropped++
			continue
		}
		seen[aid] = struct{}{}
		out = append(out, aid)
	}
	return out, dropped
}
