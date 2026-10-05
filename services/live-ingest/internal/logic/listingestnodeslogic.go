package logic

import (
	"context"
	"fmt"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListIngestNodesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListIngestNodesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListIngestNodesLogic {
	return &ListIngestNodesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询接入节点
//
// 节点信息含可下发的接入地址，属运维面数据：operator_mid 必填（<=0 直接拒绝），
// 否则任何拿到 RPC 的调用方都能枚举全部机房入口。
// protocol 过滤传的是枚举值，位图换算由 model.IngestNodeFilter.build 做（协议枚举与
// 协议位图是两套取值，logic 不把位图写进 filter）。
// 排序沿用 model 的 health_score 降序 + node_id 升序：分数相同才按 ID，翻页结果才可重复。
func (l *ListIngestNodesLogic) ListIngestNodes(in *rpc.ListIngestNodesReq) (*rpc.ListIngestNodesReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	region, err := checkRegion(in.Region)
	if err != nil {
		return nil, err
	}
	filter := model.IngestNodeFilter{Region: region}
	if in.Protocol != rpc.IngestProtocol_PROTOCOL_UNSPECIFIED {
		if _, ok := model.ProtocolMask(int32(in.Protocol)); !ok {
			return nil, model.ErrInvalidProtocol
		}
		filter.Protocol = int32(in.Protocol)
	}
	if in.State != rpc.IngestNodeState_INGEST_NODE_STATE_UNSPECIFIED {
		state := nodeStateFromRPC(in.State)
		if state == 0 {
			// 未知状态值不退回「不过滤」：那会让调用方以为筛过了，实际拿到的是全量。
			return nil, fmt.Errorf("%w: 节点状态过滤值非法", model.ErrNodeNotFound)
		}
		filter.State = state
	}

	ps := clampPageSize(in.Ps, l.svcCtx.Config.LiveIngest.MaxListPageSize)
	pn := clampPageNo(in.Pn)
	filter.Limit = ps
	filter.Offset = pageOffset(pn, ps)

	rows, err := repo.IngestNode.ListByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	total, err := repo.IngestNode.CountByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	return &rpc.ListIngestNodesReply{
		Nodes: nodeInfos(rows),
		Total: toInt32Total(total),
		Pn:    pn,
		Ps:    ps,
	}, nil
}
