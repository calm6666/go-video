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

type LiveIngestNodeUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 节点注册/元数据修改（派生字段由服务维护，后台只声明节点属性）
func NewLiveIngestNodeUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveIngestNodeUpsertLogic {
	return &LiveIngestNodeUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveIngestNodeUpsert 聚合 live-ingest UpsertIngestNode（节点注册与属性修改）。
//
// 两条不越权的边界：
//  1. 只组装 IngestNodeInput 里的可写字段（liveIngestNodeForRPC）。active_streams、
//     last_heartbeat_at、ctime、mtime 保持零值：前两个是节点上报的观测量，后两个是服务维护时钟，
//     后台表单拿不到这些事实，透传一个「自以为是的占用数」会让容量对账与打分失真；
//  2. heartbeat_only 固定 false，且它**不是表单字段**：该开关的语义是「只刷新
//     active_streams/health_score/last_heartbeat_at」，其中占用数与心跳由节点自己上报，
//     后台一旦能打开它，就等于用零值覆盖节点观测量（未注册节点还会被服务拒成 ErrNodeNotFound）。
//     节点心跳是机器链路，不进运营面。
//
// 是否允许新建（create_if_absent 与服务侧唯一性判定）、health_score 的取值范围、
// state 迁移是否合法都由 live-ingest 判定；request_id 原样透传，replayed=true 是幂等成功。
func (l *LiveIngestNodeUpsertLogic) LiveIngestNodeUpsert(req *types.ParamLiveIngestNodeUpsert) (resp *types.LiveIngestNodeUpsertResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveIngestNodeUpsert", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredText("node.node_id", req.Node.NodeId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("node.state", req.Node.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("node.capacity_streams", req.Node.CapacityStreams); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("node.health_score", req.Node.HealthScore); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.UpsertIngestNode(l.ctx, &liveingestrpc.UpsertIngestNodeReq{
		Node: liveIngestNodeForRPC(req.Node),
		// HeartbeatOnly 固定 false：节点心跳上报是机器链路，见函数注释。
		HeartbeatOnly:  false,
		CreateIfAbsent: req.CreateIfAbsent,
		OperatorMid:    req.OperatorMid,
		RequestId:      req.RequestId,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveIngestNodeUpsert: node_id=%s operator_mid=%d request_id=%s err=%v",
			req.Node.NodeId, req.OperatorMid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveIngestNodeUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveIngestNodeUpsertData{
			Node:     liveIngestNodeToAPI(reply.GetNode()),
			Created:  reply.GetCreated(),
			Replayed: reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
