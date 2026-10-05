// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"strconv"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListHotSubjectsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListHotSubjectsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListHotSubjectsLogic {
	return &ListHotSubjectsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 热度榜投影（只读，按指标值倒序分页）
func (l *ListHotSubjectsLogic) ListHotSubjects(in *rpc.ListHotSubjectsReq) (*rpc.ListHotSubjectsReply, error) {
	// 逻辑轮规划：校验 ps<=MaxPageSize(100) 与 subject_type（只允许 AID/CATALOG_ITEM/ZONE，MID 不出榜）
	// -> 解析 ACTIVE 口径 -> window_start=0 时先读 spm_window_watermark 的最近闭合窗口，
	//    水位行还不存在（首日/新口径）才退回 spm_metric_window 的 MAX(window_start)（idx_metric_latest）
	// -> model.ListHot 走 idx_metric_window 按 metric_value 倒序分页，
	//    LEFT JOIN spm_content_projection ON (subject_type,subject_id)：投影缺行放行（未收到内容事件的
	//    历史主体不因投影缺失被吞掉），有行则要求 state=0，从而剔除下架/过期/删除内容
	//    （等价约束也覆盖 zone_id 过滤）-> total 用同条件 COUNT，
	// -> 回带实际 window_start/metric_version 供调用方对齐口径。
	done, err := acquireReadToken(l.ctx, l.svcCtx, l.Logger, "ListHotSubjects")
	if err != nil {
		return nil, err
	}
	defer done()

	subjectType, err := checkHotSubjectType(in.GetSubjectType())
	if err != nil {
		return nil, err
	}
	metricKey, err := checkMetricKey(in.GetMetricKey())
	if err != nil {
		return nil, err
	}
	def, err := resolveDefinition(l.ctx, l.svcCtx, metricKey, in.GetMetricVersion())
	if err != nil {
		return nil, err
	}
	windowType, err := checkWindowType(in.GetWindowType(), def)
	if err != nil {
		return nil, err
	}
	size, err := pageSize(l.svcCtx, in.GetPs())
	if err != nil {
		return nil, err
	}
	offset := pageOffset(in.GetPn(), size)

	start := in.GetWindowStart()
	if start == 0 {
		if start, _, err = resolveWindowStart(l.ctx, l.svcCtx, l.Logger, subjectType,
			def.MetricKey, def.MetricVersion, windowType); err != nil {
			return nil, err
		}
		if start == 0 {
			// 该口径还没有任何闭合窗口：给空榜并回显 window_start=0，
			// 调用方据此知道「榜还没开始」，而不是「所有主体值为 0」。
			return &rpc.ListHotSubjectsReply{
				MetricVersion: def.MetricVersion,
				WindowStart:   0,
			}, nil
		}
	} else {
		start = windowStart(start, windowType)
	}

	// 榜的分区过滤条件与主体一起进缓存键：zone_id 变了就是另一个榜。
	cacheKey := hotListCacheKey(subjectType, def.MetricKey, def.MetricVersion, windowType, start,
		in.GetZoneId(), offset, size)
	var cached rpc.ListHotSubjectsReply
	if cacheGetJSON(l.ctx, l.svcCtx, cacheKey, &cached) {
		return &cached, nil
	}

	total, err := l.svcCtx.Windows.CountHot(l.ctx, subjectType, def.MetricKey, def.MetricVersion,
		windowType, start, in.GetZoneId())
	if err != nil {
		return nil, err
	}
	reply := &rpc.ListHotSubjectsReply{
		Total:         total,
		WindowStart:   start,
		MetricVersion: def.MetricVersion,
	}
	if !pageFits(offset, total, size) {
		// 越界页与深翻页保护：total 已经给了调用方边界，这里不再发查询 SQL。
		return reply, nil
	}
	rows, err := l.svcCtx.Windows.ListHot(l.ctx, subjectType, def.MetricKey, def.MetricVersion,
		windowType, start, in.GetZoneId(), offsetTo32(offset), size)
	if err != nil {
		return nil, err
	}
	subjects := make([]*rpc.ListHotSubjectsReply_HotSubject, 0, len(rows))
	for i, r := range rows {
		subjects = append(subjects, &rpc.ListHotSubjectsReply_HotSubject{
			SubjectId:   r.SubjectID,
			Value:       r.MetricValue,
			Numerator:   r.Numerator,
			Denominator: r.Denominator,
			// 名次按「本页在整榜中的位置」连续编号：调用方翻页时不需要自己累加。
			Rank: int32(offset) + int32(i) + 1,
		})
	}
	reply.Subjects = subjects
	cacheSetJSON(l.ctx, l.svcCtx, cacheKey, reply)
	return reply, nil
}

// hotListCacheKey 组装热榜页的缓存键。键里带已解析的 window_start 与口径版本，
// 因此水位前进、口径 ACTIVE 切换都不会让旧页串到新榜上；TTL 只兜「同窗口被重算改写」
// 这一种陈旧（WriteMetricWindow 落在已闭合窗口上的迟到修正），过期上界同样就是
// 迟到容忍线（见 shortCacheTTL）。
func hotListCacheKey(subjectType int32, metricKey string, version, windowType int32,
	windowStart, zoneID, offset int64, limit int32) string {
	return hotListCachePrefix + strconv.Itoa(int(subjectType)) + ":" + metricKey + "@v" +
		strconv.Itoa(int(version)) + ":" + strconv.Itoa(int(windowType)) + ":" +
		strconv.FormatInt(windowStart, 10) + ":" + strconv.FormatInt(zoneID, 10) + ":" +
		strconv.Itoa(int(offset)) + "/" + strconv.Itoa(int(limit))
}
