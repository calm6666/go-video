package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertIngestNodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertIngestNodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertIngestNodeLogic {
	return &UpsertIngestNodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 节点注册/心跳上报（幂等 upsert，运维面）
//
// 幂等口径：同一 node_id 重复注册语义上就是 upsert（配置列被最新值覆盖），
// 因此不需要额外的 request_id 幂等键；active_streams 不在覆盖列内，
// 它由分配记录派生，只能被 ReserveQuota/ReleaseQuota 的 CAS 改动。
func (l *UpsertIngestNodeLogic) UpsertIngestNode(in *rpc.UpsertIngestNodeReq) (*rpc.UpsertIngestNodeReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	// 运维面写：没有归因主体就不受理。
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	if in.RequestId != "" {
		if _, err := checkRequestID(in.RequestId); err != nil {
			return nil, err
		}
	}
	node := in.Node
	if node == nil || node.NodeId == "" {
		return nil, model.ErrNodeNotFound
	}
	nodeID, err := checkNodeID(node.NodeId)
	if err != nil {
		return nil, err
	}
	mask, err := protocolsToMask(node.Protocols)
	if err != nil {
		return nil, err
	}
	if mask == 0 {
		// 一个协议都不支持的节点进分配池永远不会被选中，注册它只是制造脏数据。
		return nil, model.ErrInvalidProtocol
	}
	healthScore := clampInt32(node.HealthScore, 0, healthScoreMax)

	existing, err := repo.IngestNode.FindOne(l.ctx, nodeID)
	if err != nil {
		return nil, err
	}

	if in.HeartbeatOnly {
		// 心跳只刷新占用数/健康分/心跳时间；未注册节点绝不静默建档。
		if existing == nil {
			return nil, model.ErrNodeNotFound
		}
		beat := node.LastHeartbeatAt
		if beat <= 0 {
			beat = nowUnix()
		}
		if _, err := repo.IngestNode.Upsert(l.ctx, &model.IngestNode{
			NodeID: nodeID, ActiveStreams: node.ActiveStreams, HealthScore: healthScore, LastHeartbeatAt: beat,
		}, true); err != nil {
			return nil, err
		}
		fresh, err := repo.IngestNode.FindOne(l.ctx, nodeID)
		if err != nil {
			return nil, err
		}
		return &rpc.UpsertIngestNodeReply{Node: nodeInfo(fresh)}, nil
	}

	if existing == nil && !in.CreateIfAbsent {
		// 首次建档必须显式声明：否则一个拼错的 node_id 会凭空造出一个可分配节点。
		return nil, model.ErrNodeNotFound
	}
	if node.CapacityStreams <= 0 {
		return nil, model.ErrNodeNotAssignable
	}
	state := nodeStateFromRPC(node.State)
	if state == 0 {
		state = model.NodeStateOnline
	}
	beat := node.LastHeartbeatAt
	if beat <= 0 {
		beat = nowUnix()
	}
	created, err := repo.IngestNode.Upsert(l.ctx, &model.IngestNode{
		NodeID: nodeID, Name: node.Name, Region: node.Region, ProtocolMask: mask,
		EndpointRtmp: node.EndpointRtmp, EndpointSrt: node.EndpointSrt, EndpointWebrtc: node.EndpointWebrtc,
		State: state, CapacityStreams: node.CapacityStreams, HealthScore: healthScore,
		LastHeartbeatAt: beat, Labels: node.Labels,
	}, false)
	if err != nil {
		return nil, err
	}
	fresh, err := repo.IngestNode.FindOne(l.ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return &rpc.UpsertIngestNodeReply{Node: nodeInfo(fresh), Created: created, Replayed: !created}, nil
}
