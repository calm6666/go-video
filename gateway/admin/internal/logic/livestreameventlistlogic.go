// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveingestrpc "go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveStreamEventListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 流状态事件（按 seq 游标对账；后台只读，不能代写事件）
func NewLiveStreamEventListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamEventListLogic {
	return &LiveStreamEventListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamEventList 聚合 live-ingest ListStreamEvents。
//
// 后台看事件是为了对账「状态迁移有没有发生、谁触发的」，不是为了重放：
// 推进状态机只有 ReportStreamState 一条路（节点带 report_id 上报），那是一条都不开的入口，
// 网关因此也不提供 after_seq 的写入面。after_seq 原样透传，网关不代为翻页拼接。
//
// limit 上限（MaxEventPageSize）与 desc 的默认排序由服务判定，网关只挡负数。
func (l *LiveStreamEventListLogic) LiveStreamEventList(req *types.ParamLiveStreamEventList) (resp *types.LiveStreamEventListResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredText("stream_id", req.StreamId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("after_seq", req.AfterSeq); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("limit", req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.ListStreamEvents(l.ctx, &liveingestrpc.ListStreamEventsReq{
		StreamId: req.StreamId,
		AfterSeq: req.AfterSeq,
		Limit:    req.Limit,
		Desc:     req.Desc,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamEventList: stream_id=%s after_seq=%d err=%v", req.StreamId, req.AfterSeq, err)
		return nil, err
	}
	return &types.LiveStreamEventListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamEventListData{
			List:    liveStreamEventsToAPI(reply.GetEvents()),
			MaxSeq:  reply.GetMaxSeq(),
			HasMore: reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
