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

type LiveNodeAssignmentListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 节点分配台账（容量对账与排障；按流或按节点查）
func NewLiveNodeAssignmentListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveNodeAssignmentListLogic {
	return &LiveNodeAssignmentListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveNodeAssignmentList 聚合 live-ingest ListNodeAssignments。
//
// 这是 /node/upsert 之外的对账视图：节点占用与实际分配的差、迁移链（prev_node_id）、
// 释放原因都在这里看，替代直接开放 AssignIngestNode / ReleaseIngestNode —— 那两条是控制面成对
// 调用，人工单边释放会让节点配额与真实流对不上，运营面只看台账不动手。
//
// 必须给 stream_id 或 node_id 之一：全表扫分配记录没有运营意义，服务也不会代为限定范围。
func (l *LiveNodeAssignmentListLogic) LiveNodeAssignmentList(req *types.ParamLiveNodeAssignmentList) (resp *types.LiveNodeAssignmentListResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := requireOperator("operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveAssignmentSubjectGate(req.StreamId, req.NodeId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("pn", req.Pn); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("ps", req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.ListNodeAssignments(l.ctx, &liveingestrpc.ListNodeAssignmentsReq{
		StreamId:    req.StreamId,
		NodeId:      req.NodeId,
		State:       liveingestrpc.AssignmentState(req.State),
		Pn:          req.Pn,
		Ps:          req.Ps,
		OperatorMid: req.OperatorMid,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveNodeAssignmentList: operator_mid=%d stream_id=%s node_id=%s err=%v",
			req.OperatorMid, req.StreamId, req.NodeId, err)
		return nil, err
	}
	return &types.LiveNodeAssignmentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveNodeAssignmentListData{
			List:  liveNodeAssignmentsToAPI(reply.GetAssignments()),
			Total: reply.GetTotal(),
			Pn:    reply.GetPn(),
			Ps:    reply.GetPs(),
		},
		TTL: 0,
	}, nil
}
