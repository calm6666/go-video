// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	searchindexerrpc "go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitRebuildTaskLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交索引重建任务（request_id 幂等，scope full/partition/content_type）
func NewSubmitRebuildTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitRebuildTaskLogic {
	return &SubmitRebuildTaskLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 提交重建任务：聚合 search-indexer SubmitRebuildTask RPC。
// request_id 是契约要求的幂等键，缺失会被拒绝——重建代价高，不允许运营重试产生第二个任务；
// scope/scope_value 的取值合法性由 search-indexer 校验（AGENTS.md §5：索引投影归本服务）。
func (l *SubmitRebuildTaskLogic) SubmitRebuildTask(req *types.ParamSubmitRebuildTask) (resp *types.SearchRebuildSubmitResponse, err error) {
	if l.svcCtx.SearchIndexer == nil {
		return nil, errors.New("search-indexer service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "submitRebuildTask", req.OperatorId)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("request_id", req.RequestId); err != nil {
		return nil, err
	}
	// 契约缺口：searchindexer.v1.SubmitRebuildTaskReq 只有字符串 operator，
	// 没有数值型运营账号字段，网关的 operator_id 只能落在本地审计日志里。
	if err := requireNonEmpty("operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.SearchIndexer.SubmitRebuildTask(l.ctx, &searchindexerrpc.SubmitRebuildTaskReq{
		Scope:      req.Scope,
		ScopeValue: req.ScopeValue,
		Alias:      req.Alias,
		RequestId:  req.RequestId,
		Operator:   req.Operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/submitRebuildTask: scope=%s scope_value=%s alias=%s request_id=%s operator=%s operator_id=%d err=%v",
			req.Scope, req.ScopeValue, req.Alias, req.RequestId, req.Operator, operatorID, err)
		return nil, err
	}
	l.Infof("gateway/admin/submitRebuildTask: operator_id=%d request_id=%s task_id=%s duplicated=%v",
		operatorID, req.RequestId, reply.GetTaskId(), reply.GetDuplicated())
	return &types.SearchRebuildSubmitResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchRebuildSubmitData{
			TaskId:      reply.GetTaskId(),
			TargetIndex: reply.GetTargetIndex(),
			State:       reply.GetState(),
			Duplicated:  reply.GetDuplicated(),
		},
		TTL: 0,
	}, nil
}
