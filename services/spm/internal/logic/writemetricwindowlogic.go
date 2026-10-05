// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"

	"go-video/services/spm/internal/svc"
	"go-video/services/spm/model"
	"go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type WriteMetricWindowLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewWriteMetricWindowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WriteMetricWindowLogic {
	return &WriteMetricWindowLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 聚合链路写回窗口指标（幂等覆盖，来源受 MetricSource 白名单约束）
func (l *WriteMetricWindowLogic) WriteMetricWindow(in *rpc.WriteMetricWindowReq) (*rpc.WriteMetricWindowReply, error) {
	// 逻辑轮规划：校验 points<=MaxWritePoints(500)、source 属于计算链路白名单（REALTIME/OFFLINE/RECOMPUTE）-> 逐行确认口径已登记且 ACTIVE -> 按 uniq_metric 六列覆盖写（重放幂等，不累加）-> allow_late_write=false 时，窗口早于「水位 - LateToleranceSeconds」的行计入 rejected（迟到数据只能显式回填）-> 同 request_id 已写过时回放首次计数，不重复覆盖 -> 事务内对每个闭合窗口推进 spm_window_watermark（WindowWatermarkModel.Advance 单调，只前进不回退），水位与指标必须同事务，否则 window_start=0 会解析到没有数据的窗口。
	done, err := acquireWriteToken(l.ctx, l.svcCtx, l.Logger, "WriteMetricWindow")
	if err != nil {
		return nil, err
	}
	defer done()

	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	source := int32(in.GetSource())
	if !model.ValidMetricSource(source) {
		// 只接受计算链路：人工覆盖指标在本契约里不存在（AGENTS.md §7 第 3 条）。
		return nil, fmt.Errorf("%w: source=%d", model.ErrInvalidMetricSource, source)
	}
	points := in.GetPoints()
	cfg := l.svcCtx.Config.Spm
	if int32(len(points)) > cfg.MaxWritePoints {
		return nil, fmt.Errorf("%w: points=%d > %d", model.ErrTooManyPoints, len(points),
			cfg.MaxWritePoints)
	}
	reply := &rpc.WriteMetricWindowReply{}
	if len(points) == 0 {
		// 空批次是调用方的 bug，但它不含任何需要改写的数据：返回 0/0 而不推进水位，
		// 比拒绝整次投递更容易定位（聚合器每 tick 都调这里）。
		return reply, nil
	}

	accepted := make([]*model.MetricWindow, 0, len(points))
	seen := make(map[string]struct{}, len(points))
	reject := func(pt *rpc.MetricPoint, why error) {
		reply.Rejected++
		reply.RejectedKeys = append(reply.RejectedKeys,
			rejectedPointKey(int64(int32(pt.GetSubjectType())), pt.GetSubjectId(),
				pt.GetMetricKey()))
		l.Errorf("spm/WriteMetricWindow: 拒绝 %s@v%d 主体 %d:%d 窗口 %d/%d: %v",
			pt.GetMetricKey(), pt.GetMetricVersion(), int32(pt.GetSubjectType()),
			pt.GetSubjectId(), pt.GetWindowType(), pt.GetWindowStart(), why)
	}
	// 迟到判定要读水位，而水位按 (主体类型, 口径, 版本, 粒度) 分组，同组只查一次。
	// allow_late_write=true 的显式回填不查水位：调用方已经承认自己在改历史窗口。
	// 查询是惰性的（见主循环），因为分组键要等该行通过入参校验后才可信。
	wmCache := make(map[string]*model.WindowWatermark)
	cached := newBatchDefinitions()

	for _, pt := range points {
		subjectType, err := checkSubject(pt.GetSubjectType(), pt.GetSubjectId())
		if err != nil {
			reject(pt, err)
			continue
		}
		metricKey, err := checkMetricKey(pt.GetMetricKey())
		if err != nil {
			reject(pt, err)
			continue
		}
		if pt.GetMetricVersion() <= 0 {
			// 写入侧不接受 version=0：ACTIVE 指针随时会换，
			// 「按当时的 ACTIVE 写进去」的历史窗口事后无人能解释。
			reject(pt, fmt.Errorf("%w: 写入必须显式给定口径版本", model.ErrMetricVersionRequired))
			continue
		}
		// 写入侧的口径校验与读侧同源：必须已登记且 ACTIVE。DRAFT 还没评审通过、
		// RETIRED 不再写入，两者都不接受数据落库（resolveDefinition 逐条给显式错误）。
		// 同一次调用里按 (key, version) 只解析一次：一批 500 行通常属于同一个口径，
		// 逐个解析会把一次写回变成 500 次注册表点查。错误同样缓存（本次请求内的
		// 口径状态视为一份快照）。
		def, err := cached.resolve(l.ctx, l.svcCtx, metricKey, pt.GetMetricVersion())
		if err != nil {
			reject(pt, err)
			continue
		}
		windowType, err := checkWindowType(pt.GetWindowType(), def)
		if err != nil {
			reject(pt, err)
			continue
		}
		if pt.GetNumerator() < 0 || pt.GetDenominator() < 0 || pt.GetSampleCount() < 0 {
			reject(pt, model.ErrNegativeMetricPoint)
			continue
		}
		if pt.GetEventTime() <= 0 {
			reject(pt, model.ErrEventTimeRequired)
			continue
		}
		start := windowStart(pt.GetWindowStart(), windowType)
		if !in.GetAllowLateWrite() {
			grp := watermarkGroupKey(pt)
			wm, cached := wmCache[grp]
			if !cached {
				// 走到这里分组键的各列都已通过校验，Find 只可能因真正的库错误失败。
				wm, err = l.svcCtx.Watermarks.Find(l.ctx, subjectType, def.MetricKey,
					def.MetricVersion, windowType)
				if err != nil {
					return nil, err
				}
				wmCache[grp] = wm
			}
			if lateWindow(wm, cfg.LateToleranceSeconds, start) {
				reject(pt, fmt.Errorf("%w: 窗口 %d 早于水位 %d 且超出容忍 %d 秒，"+
					"回填请置 allow_late_write=true", model.ErrInvalidWindow, start,
					wm.LastClosedStart, cfg.LateToleranceSeconds))
				continue
			}
		}
		// 自然键去重：model 的 UpsertBatch 对批内重复整批报错，
		// 这里先拒掉后来的那一条，保证一条坏数据不会拖垮同批的几百个主体。
		naturalKey := fmt.Sprintf("%d:%d:%s:%d:%d:%d", subjectType, pt.GetSubjectId(), metricKey,
			def.MetricVersion, windowType, start)
		if _, dup := seen[naturalKey]; dup {
			reject(pt, model.ErrDuplicatePointInBatch)
			continue
		}
		seen[naturalKey] = struct{}{}
		accepted = append(accepted, &model.MetricWindow{
			SubjectType:   subjectType,
			SubjectID:     pt.GetSubjectId(),
			MetricKey:     def.MetricKey,
			MetricVersion: def.MetricVersion,
			WindowType:    windowType,
			WindowStart:   start,
			MetricValue:   pt.GetValue(),
			Numerator:     pt.GetNumerator(),
			Denominator:   pt.GetDenominator(),
			SampleCount:   pt.GetSampleCount(),
			Source:        source,
			EventTime:     pt.GetEventTime(),
			WriteReqID:    in.GetRequestId(),
		})
	}
	if len(accepted) == 0 {
		// 整批都被拒：不写、不推进水位，rejected 与 rejected_keys 说明原因。
		return reply, nil
	}

	// 重放判定：按 uniq_metric 六列回读，命中行全部带同一 write_request_id 才算「这批已写过」。
	// 部分命中不算重放——覆盖写本身是幂等的，重写一次只会把值收敛到同一份。
	existing, err := l.svcCtx.Windows.ListByNaturalKeys(l.ctx, accepted)
	if err != nil {
		return nil, err
	}
	if replayed(existing, in.GetRequestId(), len(accepted)) {
		reply.Written = int32(len(accepted))
		l.Infof("spm/WriteMetricWindow: request_id=%s 重放，回放首次写入计数 %d 行",
			in.GetRequestId(), len(accepted))
		return reply, nil
	}

	if err := l.persist(accepted, in.GetAllowLateWrite(), in.GetRequestId()); err != nil {
		return nil, err
	}
	// written 报的是「本次受理并落库的行数」。MySQL 对值未变化的 UPSERT 不计入 affected，
	// 拿 affected 回话会让一次纯重算的写回看起来像写了 0 行，所以这里回的是受理行数。
	reply.Written = int32(len(accepted))
	return reply, nil
}

// persist 在一个事务里覆盖写指标行并推进水位。
//
// 水位与指标同事务是正确性前提：分开提交会出现「水位已前进、对应窗口还没落库」的窗口，
// 而所有读接口的 window_start=0 都按水位解析，那一刻整张榜会读成空。
// 反过来（先写指标后推水位）只让榜暂时落后一个窗口，不会读到不存在的数据。
func (l *WriteMetricWindowLogic) persist(rows []*model.MetricWindow, allowLate bool,
	requestID string) error {
	watermarks := closedWindowWatermarks(rows, requestID)
	return l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, session sqlx.Session) error {
		windows := l.svcCtx.Windows.WithSession(session)
		if _, err := windows.UpsertBatch(ctx, rows, allowLate); err != nil {
			return fmt.Errorf("spm/logic: WriteMetricWindow 覆盖写指标: %w", err)
		}
		marks := l.svcCtx.Watermarks.WithSession(session)
		for _, wm := range watermarks {
			applied, err := marks.Advance(ctx, wm)
			if err != nil {
				return fmt.Errorf("spm/logic: WriteMetricWindow 推进水位 %s@v%d/%d: %w",
					wm.MetricKey, wm.MetricVersion, wm.WindowType, err)
			}
			if !applied {
				// 单调推进被拒不是错误：并发下另一个实例（或另一次重放）已经推进到更远的
				// 窗口。指标行仍然按自然键覆盖写生效，榜读到的仍是更新的窗口。
				l.Infof("spm/WriteMetricWindow: 水位回退被拒 subject_type=%d %s@v%d wt=%d start=%d",
					wm.SubjectType, wm.MetricKey, wm.MetricVersion, wm.WindowType,
					wm.LastClosedStart)
			}
		}
		return nil
	})
}

// closedWindowWatermarks 按 (主体类型, 口径, 版本, 粒度) 归并出本批要推进的水位。
//
// 只推进「本批实际覆盖到的 subject_type」，不推进跨主体汇总位（subject_type=0）：
// 一批数据是否覆盖了全部主体，只有聚合器自己知道，WriteMetricWindow 的入参里
// 没有这个信息。汇总水位留给显式的汇总链路（README「作业与租约语义」）。
//
// 每组的 last_closed_start 取组内最大窗口左边界，rows_written 只统计落在这个边界上的行数
// ——水位列的语义是「那个闭合窗口写了多少主体」，写成整组行数会让人以为一次算了几百个窗口。
func closedWindowWatermarks(rows []*model.MetricWindow, requestID string) []*model.WindowWatermark {
	groups := make(map[string]*model.WindowWatermark)
	order := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.WindowType == model.WindowTypeTotal {
			// TOTAL 恒为 window_start=0，「最近闭合窗口」对它没有意义（model 也按
			// ErrInvalidWindow 拒绝建水位）。
			continue
		}
		key := fmt.Sprintf("%d:%s:%d:%d", r.SubjectType, r.MetricKey, r.MetricVersion,
			r.WindowType)
		g, ok := groups[key]
		if !ok {
			groups[key] = &model.WindowWatermark{
				SubjectType:     r.SubjectType,
				MetricKey:       r.MetricKey,
				MetricVersion:   r.MetricVersion,
				WindowType:      r.WindowType,
				LastClosedStart: r.WindowStart,
				LastEventTime:   r.EventTime,
				UpdateRequestID: requestID,
			}
			order = append(order, key)
			continue
		}
		if r.EventTime > g.LastEventTime {
			g.LastEventTime = r.EventTime
		}
		if r.WindowStart > g.LastClosedStart {
			g.LastClosedStart = r.WindowStart
		}
	}
	out := make([]*model.WindowWatermark, 0, len(order))
	for _, key := range order {
		g := groups[key]
		counted := int64(0)
		for _, r := range rows {
			if r.SubjectType == g.SubjectType && r.MetricKey == g.MetricKey &&
				r.MetricVersion == g.MetricVersion && r.WindowType == g.WindowType &&
				r.WindowStart == g.LastClosedStart {
				counted++
			}
		}
		g.RowsWritten = counted
		out = append(out, g)
	}
	return out
}

// watermarkGroupKey 迟到判定用的水位分组键，与 closedWindowWatermarks 同构。
func watermarkGroupKey(pt *rpc.MetricPoint) string {
	return fmt.Sprintf("%d:%s:%d:%d", int32(pt.GetSubjectType()), pt.GetMetricKey(),
		pt.GetMetricVersion(), int32(pt.GetWindowType()))
}

// replayed 判断整批是否已由同一幂等键写过：命中行数与批内行数相等，且每行的
// write_request_id 都是本次 request_id。任何一行不同（或没命中）都按「需要写」处理。
func replayed(existing []*model.MetricWindow, requestID string, want int) bool {
	if len(existing) != want {
		return false
	}
	for _, row := range existing {
		if row.WriteReqID != requestID {
			return false
		}
	}
	return true
}
