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

type GetEventRecordLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetEventRecordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetEventRecordLogic {
	return &GetEventRecordLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单条事件的校验结论与投递状态。
//
// 台账定位（proto 注释）：这里是「采集台账」，不是行为事实表——要算指标请消费 MQ topic。
// 正文只有 payload_digest + payload_bytes 与可选对象存储引用（blob_* 不在投影里），绝无原文。
//
// 投递真值在 ec_pending_delivery：ec_event_record.delivery_state 只是 dispatcher 回写的投影，
// 本方法既然能同时读到两边，就以 Outbox 为准（applyPendingTruth），否则排障时
// 会拿一个滞后投影得出「事件已发出去」的错误结论。两者长期不一致由对账任务修正
// （README 已知缺口）。
func (l *GetEventRecordLogic) GetEventRecord(in *rpc.GetEventRecordReq) (*rpc.GetEventRecordReply, error) {
	eventID := strings.TrimSpace(in.GetEventId())
	if eventID == "" || len(eventID) > maxEventIDBytes {
		return nil, model.ErrEventIDRequired
	}
	row, err := l.svcCtx.Records.FindByEventID(l.ctx, eventID)
	if err != nil {
		if model.IsNotFound(err) {
			// 事件可能只是没上报过：这是「有没有」的答案，不是错误。
			return &rpc.GetEventRecordReply{Found: false}, nil
		}
		return nil, err
	}
	rec := recordToRPC(row)
	pending, err := l.svcCtx.Pending.ListByEventID(l.ctx, eventID)
	if err != nil {
		// 读 Outbox 失败时宁可报错，也不回一份明知可能过期的投递结论：
		// 排障场景里「看起来已投递」比「查不出来」更容易造成误判（AGENTS.md §9）。
		l.Errorf("event-collector/logic: 读 Outbox 投递行失败 event_id=%s: %v", fitColumn(eventID, maxEventIDBytes), err)
		return nil, err
	}
	applyPendingTruth(rec, pending)
	return &rpc.GetEventRecordReply{Record: rec, Found: true}, nil
}
