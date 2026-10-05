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

type LiveIngestNodeListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 接入节点列表（health_score 降序，含摘流/离线节点）
func NewLiveIngestNodeListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveIngestNodeListLogic {
	return &LiveIngestNodeListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveIngestNodeList 聚合 live-ingest ListIngestNodes。
//
// operator_mid 必须 > 0：proto 注明节点信息属运维面，要求调用者有身份（不是可选过滤条件）。
// 本路由是只读面、不挂 AdminPermission，因此只校验主体存在，不校验会话。
//
// 三个 endpoint 是接入地址（proto 注明不含密钥、可下发客户端），不是推流地址，可以整段回显；
// active_streams / last_heartbeat_at 是节点侧派生值，这里只读展示，写入口见 /node/upsert 的白名单。
func (l *LiveIngestNodeListLogic) LiveIngestNodeList(req *types.ParamLiveIngestNodeList) (resp *types.LiveIngestNodeListResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := requireOperator("operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("protocol", req.Protocol); err != nil {
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
	reply, err := l.svcCtx.LiveIngest.ListIngestNodes(l.ctx, &liveingestrpc.ListIngestNodesReq{
		Region:      req.Region,
		Protocol:    liveingestrpc.IngestProtocol(req.Protocol),
		State:       liveingestrpc.IngestNodeState(req.State),
		Pn:          req.Pn,
		Ps:          req.Ps,
		OperatorMid: req.OperatorMid,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveIngestNodeList: operator_mid=%d region=%s state=%d err=%v",
			req.OperatorMid, req.Region, req.State, err)
		return nil, err
	}
	return &types.LiveIngestNodeListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveIngestNodeListData{
			List:  liveIngestNodesToAPI(reply.GetNodes()),
			Total: reply.GetTotal(),
			Pn:    reply.GetPn(),
			Ps:    reply.GetPs(),
		},
		TTL: 0,
	}, nil
}
