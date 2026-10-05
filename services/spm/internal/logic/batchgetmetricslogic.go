// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"time"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BatchGetMetricsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBatchGetMetricsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchGetMetricsLogic {
	return &BatchGetMetricsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量读取主体的一组指标/多个窗口（单次上限 50 口径 × 30 窗口）
func (l *BatchGetMetricsLogic) BatchGetMetrics(in *rpc.BatchGetMetricsReq) (*rpc.BatchGetMetricsReply, error) {
	// 逻辑轮规划：校验 keys<=MaxMetricKeysPerRequest(50)、window_count 在 1..MaxWindowCount(30)
	// -> 一次性解析各口径的有效版本 -> window_start_from=0 时按 spm_window_watermark 水位回推
	//    [watermark-(n-1)*窗口秒数, watermark] 的区间（水位缺失才退回 MAX(window_start)）
	// -> spm_metric_window 走 uniq_metric/idx_metric_window 做区间查询（一次 SQL，不按 key 循环打库）
	// -> 按 metric_key@v<version>:<window_start> 组装 map，缺数据的窗口不出现（不补 0）。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "BatchGetMetrics")
	if err != nil {
		return nil, err
	}
	defer done()

	subjectType, err := checkSubject(in.GetSubjectType(), in.GetSubjectId())
	if err != nil {
		return nil, err
	}
	cfg := l.svcCtx.Config.Spm
	keys := in.GetKeys()
	if len(keys) == 0 {
		return nil, model.ErrMetricKeyEmpty
	}
	if int32(len(keys)) > cfg.MaxMetricKeysPerRequest {
		return nil, fmt.Errorf("%w: keys=%d > %d", model.ErrTooManyKeys, len(keys),
			cfg.MaxMetricKeysPerRequest)
	}
	count := in.GetWindowCount()
	if count <= 0 {
		count = 1
	}
	if count > cfg.MaxWindowCount {
		return nil, fmt.Errorf("%w: window_count=%d > %d", model.ErrWindowRangeTooLarge,
			count, cfg.MaxWindowCount)
	}
	windowType, err := checkWindowType(in.GetWindowType(), nil)
	if err != nil {
		return nil, err
	}

	// 逐口径解析有效版本并顺带校验粒度：任一口径不可用就整单拒绝。
	// 部分成功会让「这个 key 为什么不在 map 里」变成无法回答的问题——
	// 少一个 key 和少一条数据在响应里长得一模一样。
	resolved := make([]model.MetricKeyVersion, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		key, err := checkMetricKey(k.GetMetricKey())
		if err != nil {
			return nil, err
		}
		if _, dup := seen[key]; dup {
			continue
		}
		def, err := resolveDefinition(l.ctx, l.svcCtx, key, k.GetMetricVersion())
		if err != nil {
			return nil, err
		}
		if !definitionSupportsWindow(def, windowType) {
			return nil, fmt.Errorf("%w: 口径 %s@v%d 未登记粒度 %d（supported_windows=%s）",
				model.ErrInvalidWindow, def.MetricKey, def.MetricVersion, windowType,
				def.SupportedWindows)
		}
		seen[key] = struct{}{}
		resolved = append(resolved, model.MetricKeyVersion{MetricKey: def.MetricKey,
			Version: def.MetricVersion})
	}

	starts, err := l.windowRange(subjectType, windowType, resolved, in.GetWindowStartFrom(), count)
	if err != nil {
		return nil, err
	}
	reply := &rpc.BatchGetMetricsReply{Points: map[string]*rpc.MetricPoint{}}
	if len(starts) == 0 {
		// 这些口径在该主体下一次都没有闭合窗口：空 map = 没有数据，
		// 与「口径不存在」（上面已显式报错）是两件事。
		return reply, nil
	}
	rows, err := l.svcCtx.Windows.ListByKeyWindows(l.ctx, subjectType, in.GetSubjectId(),
		windowType, resolved, starts)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		reply.Points[pointKey(row.MetricKey, row.MetricVersion, row.WindowStart)] = metricPointOf(row)
	}
	return reply, nil
}

// windowRange 推出本次要读的窗口左边界集合。
//
// window_start_from>0 时按调用方给的起点往后数 count 个；
// =0 时从各口径的「最近闭合窗口」往回推。多口径共用一个区间，区间右端取的是
// 最小的那个水位：取最大值会让水位落后（跑得慢、但数据没错）的口径整段落空，
// 看起来就像「这个指标最近没人看」。落后的口径因此少读最新的一两个窗口，
// 而 map 的键里带着 window_start，调用方不会把旧窗口读成新的。
//
// 水位行不存在时按该口径已落库的最大 window_start 兜底（同样是「已闭合」的口径）。
func (l *BatchGetMetricsLogic) windowRange(subjectType, windowType int32,
	keys []model.MetricKeyVersion, from int64, count int32) ([]int64, error) {
	if windowType == model.WindowTypeTotal {
		return []int64{0}, nil
	}
	if from > 0 {
		return consecutiveWindowStarts(from, windowType, count), nil
	}

	wms, err := l.svcCtx.Watermarks.ListByMetricKeys(l.ctx, subjectType, windowType, keys)
	if err != nil {
		return nil, err
	}
	byMetric := make(map[string]*model.WindowWatermark, len(wms))
	for _, w := range wms {
		byMetric[w.MetricKey] = w
	}
	closedBefore := time.Now().Unix() - model.WindowSeconds(windowType)

	var end int64
	for _, k := range keys {
		start := int64(0)
		if wm := byMetric[k.MetricKey]; wm != nil {
			start = wm.LastClosedStart
		} else {
			// 兜底路径按口径逐个查极值：只在水位尚未建立时（新口径/回填未跑）发生，
			// 键数上限已把这里的查询次数约束在 50 以内。
			start, err = l.svcCtx.Windows.FindLatestWindowStart(l.ctx, subjectType, k.MetricKey,
				k.Version, windowType, closedBefore)
			if err != nil {
				return nil, err
			}
		}
		if start <= 0 {
			continue
		}
		if end == 0 || start < end {
			end = start
		}
	}
	if end == 0 {
		return nil, nil
	}
	return consecutiveWindowStarts(end-int64(count-1)*model.WindowSeconds(windowType),
		windowType, count), nil
}
