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

type LiveStreamListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 流列表巡检（开播巡检/断流扫描；admin 作用域由网关声明，见类型注释）
func NewLiveStreamListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamListLogic {
	return &LiveStreamListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamList 聚合 live-ingest ListStreams。
//
// operator_mid 是读取主体（proto 要求 > 0），但不做会话校验：本路由是只读面，
// 与 live-room 的 list 同一口径不挂 AdminPermission。
// admin 位取网关常量 liveIngestAdminScope，请求体里没有 admin 字段，客户端无法声明；
// 理由与信任边界见 conv_live_ingest.go 的常量注释。
//
// state 与 heartbeat_before 原样透传：state=0 在 live-ingest 里是「只看非终态」而不是
// 「不过滤」，网关若代为改写就会让后台把「断流」读成「没有流」，取值语义由服务定义。
func (l *LiveStreamListLogic) LiveStreamList(req *types.ParamLiveStreamList) (resp *types.LiveStreamListResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := requireOperator("operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("protocol", req.Protocol); err != nil {
		return nil, err
	}
	if err := liveNonNeg("heartbeat_before", req.HeartbeatBefore); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("pn", req.Pn); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("ps", req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.ListStreams(l.ctx, &liveingestrpc.ListStreamsReq{
		RoomIds:         req.RoomIds,
		NodeId:          req.NodeId,
		State:           liveingestrpc.StreamState(req.State),
		Protocol:        liveingestrpc.IngestProtocol(req.Protocol),
		HeartbeatBefore: req.HeartbeatBefore,
		Pn:              req.Pn,
		Ps:              req.Ps,
		OperatorMid:     req.OperatorMid,
		Admin:           liveIngestAdminScope,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamList: operator_mid=%d node_id=%s state=%d err=%v",
			req.OperatorMid, req.NodeId, req.State, err)
		return nil, err
	}
	return &types.LiveStreamListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamListData{
			List:       liveStreamsToAPI(reply.GetStreams()),
			Total:      reply.GetTotal(),
			Pn:         reply.GetPn(),
			Ps:         reply.GetPs(),
			ServerTime: reply.GetServerTime(),
		},
		TTL: 0,
	}, nil
}
