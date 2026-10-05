// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetMetricLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetMetricLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetMetricLogic {
	return &GetMetricLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 读取单主体单口径单窗口的指标值
func (l *GetMetricLogic) GetMetric(in *rpc.GetMetricReq) (*rpc.GetMetricReply, error) {
	// 逻辑轮规划：校验 subject_type/window_type 非 UNSPECIFIED、metric_key 非空 -> metric_version=0 时解析 spm_metric_definition 的 ACTIVE 版本
	// -> window_start=0 时先读 spm_window_watermark（uniq_watermark）的最近闭合窗口，水位缺失才退回
	//    MetricWindow.FindLatestWindowStart（idx_metric_latest 极值）-> 先读 CacheRedis 加速层（spm:metric:...），
	//    miss 回源 spm_metric_window（uniq_metric 六列定位）；无数据时 found=false 且 point 留零值，绝不伪造 0。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "GetMetric")
	if err != nil {
		return nil, err
	}
	defer done()

	subjectType, err := checkSubject(in.GetSubjectType(), in.GetSubjectId())
	if err != nil {
		return nil, err
	}
	metricKey, err := checkMetricKey(in.GetMetricKey())
	if err != nil {
		return nil, err
	}
	// 口径先解析再校验粒度：supported_windows 是口径的一部分，脱离口径无从判断
	// 「这个指标有没有 5 分钟档」。
	def, err := resolveDefinition(l.ctx, l.svcCtx, metricKey, in.GetMetricVersion())
	if err != nil {
		return nil, err
	}
	windowType, err := checkWindowType(in.GetWindowType(), def)
	if err != nil {
		return nil, err
	}

	start := in.GetWindowStart()
	if start == 0 {
		// window_start=0 = 「最近一个已闭合窗口」，解析顺序见 resolveWindowStart。
		if start, _, err = resolveWindowStart(l.ctx, l.svcCtx, l.Logger, subjectType,
			def.MetricKey, def.MetricVersion, windowType); err != nil {
			return nil, err
		}
	} else {
		start = windowStart(start, windowType)
	}

	// 命中路径不做单独缓存：uniq_metric 六列是唯一键点查，而「最近闭合窗口」每次都随水位
	// 前进，缓存它只会让刚闭合的窗口读成上一窗口的值。热榜（ListHotSubjects）才是缓存受益方。
	row, err := l.svcCtx.Windows.FindOne(l.ctx, subjectType, in.GetSubjectId(), def.MetricKey,
		def.MetricVersion, windowType, start)
	if err != nil {
		return nil, err
	}
	if row == nil {
		// 无数据：found=false，指标列一律 0。这里回显的是「按哪个口径、哪个窗口查的」，
		// 不是伪造出来的取值——调用方必须看 found 才能决定是否当 0 用。
		return &rpc.GetMetricReply{
			Found: false,
			Point: &rpc.MetricPoint{
				MetricKey:     def.MetricKey,
				MetricVersion: def.MetricVersion,
				SubjectType:   in.GetSubjectType(),
				SubjectId:     in.GetSubjectId(),
				WindowType:    in.GetWindowType(),
				WindowStart:   start,
			},
		}, nil
	}
	return &rpc.GetMetricReply{Found: true, Point: metricPointOf(row)}, nil
}
