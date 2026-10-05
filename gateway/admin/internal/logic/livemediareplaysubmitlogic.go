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

type LiveMediaReplaySubmitLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交回放拼接任务（只登记与校验切片区间，不拼接、不发布）
func NewLiveMediaReplaySubmitLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaReplaySubmitLogic {
	return &LiveMediaReplaySubmitLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaReplaySubmit 聚合 live-media SubmitReplayTask。
//
// 这条路由**只登记意图**：不拼接、不写 asset、不建稿件、更不推进发布状态（后续由 Worker 依次
// 回报并回填引用），所以它的权限点是 live:replay:submit，与「回填引用」「刷新投影」分开。
//
// from_seq/to_seq 与 start_at/end_at 都是「<=0 表示这一端不限制」的哨兵（服务侧语义），网关拒负数、
// 并挡住倒置的区间 —— 倒置区间在下游只会拼出一个空回放或报错，事前拒比事后解释便宜。
// record_id 必填（回放素材来源），title 会成为稿件标题所以要求非空，但标题是否合规由 video/
// 审核链路判定。allow_gaps=false 时切片有缺口会被服务直接拒（避免产出坏回放），这个判断在下游，
// 网关不去数缺口再决定要不要放行。本方法没有 operator 位，谁提交的落在网关日志。
func (l *LiveMediaReplaySubmitLogic) LiveMediaReplaySubmit(req *types.ParamLiveMediaReplaySubmit) (resp *types.LiveMediaReplaySubmitResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveMediaSessionGate(l.ctx, "liveMediaReplaySubmit"); err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("record_id", req.RecordId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("title", req.Title); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"live_session_id", req.SessionId},
		{"anchor_mid", req.AnchorMid},
		{"from_seq", req.FromSeq},
		{"to_seq", req.ToSeq},
		{"start_at", req.StartAt},
		{"end_at", req.EndAt},
	} {
		if err := liveNonNeg(f.name, f.v); err != nil {
			return nil, err
		}
	}
	if err := liveMediaWindow("from_seq", "to_seq", req.FromSeq, req.ToSeq); err != nil {
		return nil, err
	}
	if err := liveMediaWindow("start_at", "end_at", req.StartAt, req.EndAt); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.SubmitReplayTask(l.ctx, &livemediarpc.SubmitReplayTaskReq{
		RoomId:        req.RoomId,
		LiveSessionId: req.SessionId,
		RecordId:      req.RecordId,
		FromSeq:       req.FromSeq,
		ToSeq:         req.ToSeq,
		StartAt:       req.StartAt,
		EndAt:         req.EndAt,
		AllowGaps:     req.AllowGaps,
		AnchorMid:     req.AnchorMid,
		Title:         req.Title,
		Description:   req.Description,
		RequestId:     req.RequestId,
		TraceId:       req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaReplaySubmit: room_id=%d record_id=%d from_seq=%d to_seq=%d request_id=%s err=%v",
			req.RoomId, req.RecordId, req.FromSeq, req.ToSeq, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaReplaySubmitResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaReplayTaskToAPI(info),
		TTL:     0,
	}, nil
}
