// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveMediaRecordSegmentListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 录制切片 keyset 分页（MISSING/CORRUPT 缺口必须看得见，否则回放像完整的）
func NewLiveMediaRecordSegmentListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRecordSegmentListLogic {
	return &LiveMediaRecordSegmentListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRecordSegmentList 聚合 live-media ListRecordSegments。
//
// 切片是只增不减的大表（一场三小时直播 = 数千行），因此这一条走 keyset 游标而不是 pn/ps：
// after_seq=0 从头部开始，next_after_seq / has_more 原样回给后台续翻，网关不自己推算下一页游标
// （那是服务的排序口径）。limit 上限（500）由 live-media 夹取，这里只挡负数。
//
// state=4 MISSING / 5 CORRUPT 必须能在列表里出现：少了十分钟的切片如果被网关“优化”掉，
// 拼出来的回放看起来是完整的，而这正是排障最需要的证据（proto 注释同口径）。
func (l *LiveMediaRecordSegmentListLogic) LiveMediaRecordSegmentList(req *types.ParamLiveMediaRecordSegmentList) (resp *types.LiveMediaRecordSegmentListResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("record_id", req.RecordId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("after_seq", req.AfterSeq); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("limit", req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveMedia.ListRecordSegments(l.ctx, &livemediarpc.ListRecordSegmentsReq{
		RecordId: req.RecordId,
		State:    livemediarpc.SegmentState(req.State),
		AfterSeq: req.AfterSeq,
		Limit:    req.Limit,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRecordSegmentList: record_id=%d after_seq=%d state=%d err=%v",
			req.RecordId, req.AfterSeq, req.State, err)
		return nil, err
	}
	return &types.LiveMediaRecordSegmentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveMediaRecordSegmentListData{
			Total:        reply.GetTotal(),
			NextAfterSeq: reply.GetNextAfterSeq(),
			HasMore:      reply.GetHasMore(),
			List:         liveMediaRecordSegmentsToAPI(reply.GetSegments()),
		},
		TTL: 0,
	}, nil
}
