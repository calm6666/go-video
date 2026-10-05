package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListNodeAssignmentsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListNodeAssignmentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListNodeAssignmentsLogic {
	return &ListNodeAssignmentsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询节点分配记录（容量对账与排障）
//
// 必须至少给 stream_id 或 node_id 之一：分配记录随每次迁移追加一行，
// 无维度的分页就是对着最大的一张调度表做全表 COUNT，运维面板会先把自己打挂。
// 越权模型与节点列表一致：operator_mid 必填，但不按人收敛——分配历史是容量对账
// 与调度排障的事实，按 mid 过滤会让「谁占了这个节点」查不出来。
func (l *ListNodeAssignmentsLogic) ListNodeAssignments(in *rpc.ListNodeAssignmentsReq) (*rpc.ListNodeAssignmentsReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.StreamId) == "" && strings.TrimSpace(in.NodeId) == "" {
		return nil, fmt.Errorf("%w: 必须按 stream_id 或 node_id 限定范围", model.ErrAssignmentNotFound)
	}
	filter := model.NodeAssignmentFilter{}
	if in.StreamId != "" {
		streamID, err := checkStreamID(in.StreamId)
		if err != nil {
			return nil, err
		}
		filter.StreamID = streamID
	}
	if in.NodeId != "" {
		nodeID, err := checkNodeID(in.NodeId)
		if err != nil {
			return nil, err
		}
		filter.NodeID = nodeID
	}
	if in.State != rpc.AssignmentState_ASSIGNMENT_STATE_UNSPECIFIED {
		state := assignmentStateFromRPC(in.State)
		if state == 0 {
			return nil, fmt.Errorf("%w: 分配状态过滤值非法", model.ErrAssignmentNotFound)
		}
		filter.State = state
	}

	ps := clampPageSize(in.Ps, l.svcCtx.Config.LiveIngest.MaxListPageSize)
	pn := clampPageNo(in.Pn)
	filter.Limit = ps
	filter.Offset = pageOffset(pn, ps)

	rows, err := repo.NodeAssignment.ListByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	total, err := repo.NodeAssignment.CountByFilter(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	return &rpc.ListNodeAssignmentsReply{
		Assignments: assignmentInfos(rows),
		Total:       toInt32Total(total),
		Pn:          pn,
		Ps:          ps,
	}, nil
}
