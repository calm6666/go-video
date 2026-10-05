// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListEventRecordsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListEventRecordsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListEventRecordsLogic {
	return &ListEventRecordsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询事件台账。
//
// 定位（proto 注释、AGENTS.md §7）：ec_event_record 是「采集台账」，只用于排障、去重判定、
// 投递推进与审计，不是行为事实表 —— 要算指标请消费 MQ topic（spm/feature-store 的输入源）。
// 本方法因此强制「至少一个过滤条件」：它是随流量线性增长的表，无条件翻页
// 等于把一个排障接口变成打垮自身 MySQL 的入口。
//
// 翻页口径与 ListIngestBatches 完全一致（同一套 newPageWindow）：
// (ctime, id) 倒序 + ps+1 探测 has_more，不用 OFFSET。
//
// 投递状态列是 ec_event_record 上的投影（真值在 ec_pending_delivery）。列表刻意不做
// 逐行回查 Outbox：那会把一次只读列表变成 N 次查询；需要精确投递结论请用 GetEventRecord。
func (l *ListEventRecordsLogic) ListEventRecords(in *rpc.ListEventRecordsReq) (*rpc.ListEventRecordsReply, error) {
	w, err := newPageWindow(l.svcCtx.Config, in.GetPageSize(), in.GetCursor(), in.GetCtimeFrom(), in.GetCtimeTo())
	if err != nil {
		return nil, err
	}
	batchID := strings.TrimSpace(in.GetBatchId())
	if len(batchID) > maxBatchIDBytes {
		return nil, model.ErrBatchIDRequired
	}
	eventType := strings.TrimSpace(in.GetEventType())
	if eventType != "" && !validEventTypeFilter(eventType) {
		return nil, model.ErrEventsRequired
	}
	category := int32(in.GetCategory())
	if category != 0 {
		if _, ok := eventSuffixOf(rpc.BehaviorCategory(category)); !ok {
			return nil, model.ErrInvalidPage
		}
	}
	decision := int32(in.GetDecision())
	if decision != 0 && !model.ValidDecision(decision) {
		return nil, model.ErrInvalidStateTransition
	}
	reason := int32(in.GetReason())
	if reason != 0 && !model.ValidReason(reason) {
		return nil, model.ErrInvalidStateTransition
	}
	delivery := int32(in.GetDeliveryState())
	if delivery != 0 && !model.ValidDeliveryState(delivery) {
		return nil, model.ErrInvalidStateTransition
	}
	topic := strings.TrimSpace(in.GetTopic())
	if topic != "" && !validTopicFilter(topic) {
		return nil, model.ErrInvalidPage
	}
	deviceHash := strings.TrimSpace(in.GetDeviceHash())
	if deviceHash != "" && !validDeviceHashFilter(deviceHash) {
		// 只接受加盐哈希形态：明文设备号会作为 SQL 参数留在 general log 里。
		return nil, model.ErrPrivacyFieldForbidden
	}
	f := model.EventRecordFilter{
		BatchID:       batchID,
		EventType:     eventType,
		Category:      category,
		Decision:      decision,
		Reason:        reason,
		DeliveryState: delivery,
		Topic:         topic,
		Mid:           in.GetMid(),
		DeviceHash:    deviceHash,
		CtimeFrom:     w.from,
		CtimeTo:       w.to,
	}
	// mid=0 在 proto3 里与「未传」同值，所以未登录事件不能用 mid 单独筛
	// （改用 batch_id / device_hash / ctime 区间组合）。
	filtered := batchID != "" || eventType != "" || category != 0 || decision != 0 || reason != 0 ||
		delivery != 0 || topic != "" || deviceHash != "" || in.GetMid() != 0 || w.from > 0 || w.to > 0
	if err := requireAnyFilter(filtered,
		"batch_id/event_type/category/decision/reason/delivery_state/topic/device_hash/ctime 区间"); err != nil {
		return nil, err
	}

	rows, err := l.svcCtx.Records.List(l.ctx, f, w.cursor, w.fetchLimit())
	if err != nil {
		return nil, err
	}
	list, hasMore := trimPage(rows, w.pageSize)
	total, err := l.svcCtx.Records.Count(l.ctx, f)
	if err != nil {
		return nil, err
	}
	var next string
	if hasMore && len(list) > 0 {
		last := list[len(list)-1]
		next = nextCursor(true, last.Ctime, last.ID)
	}
	return &rpc.ListEventRecordsReply{
		List:       recordToRPCList(list),
		NextCursor: next,
		HasMore:    hasMore,
		Total:      total,
	}, nil
}
