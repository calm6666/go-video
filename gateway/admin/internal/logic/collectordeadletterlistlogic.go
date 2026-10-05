// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorDeadLetterListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 投递死信游标翻页（topic/state/时间窗；reason 是稳定枚举）
func NewCollectorDeadLetterListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorDeadLetterListLogic {
	return &CollectorDeadLetterListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorDeadLetterList 转发 event-collector ListDeadLetters。
// state 在本域是字符串枚举（open/replayed/discarded）而不是整数：它与 ec_dead_letter 的
// 列值一一对应，网关不翻译、不校验取值集合（未知值在服务侧查不到行，也不会误改成终态）。
// topic/state/时间窗全空时的无界扫描拒绝、page_size 上限与 cursor 语法都由服务判定。
// 死信只有摘要（payload_digest）与稳定枚举 reason，事件原文不入库、也不出自本面（AGENTS.md §7）。
func (l *CollectorDeadLetterListLogic) CollectorDeadLetterList(req *types.ParamCollectorDeadLetterList) (resp *types.CollectorDeadLetterListResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	if err := collectorTimeWindow(req.CtimeFrom, req.CtimeTo); err != nil {
		return nil, err
	}
	if err := collectorNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.EventCollector.ListDeadLetters(l.ctx, &collectorrpc.ListDeadLettersReq{
		Topic:     req.Topic,
		State:     req.State,
		CtimeFrom: req.CtimeFrom,
		CtimeTo:   req.CtimeTo,
		Cursor:    req.Cursor,
		PageSize:  req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorDeadLetterList: topic=%s state=%s cursor=%q page_size=%d err=%v",
			req.Topic, req.State, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CollectorDeadLetterListResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorDeadLetterListData{
			List:       collectorDeadLetterListToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
