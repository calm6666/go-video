package logic

import (
	"context"

	"go-video/services/search-indexer/internal/repository"
	"go-video/services/search-indexer/internal/svc"
	"go-video/services/search-indexer/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitRebuildTaskLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitRebuildTaskLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitRebuildTaskLogic {
	return &SubmitRebuildTaskLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交全量/分区重建任务（request_id 幂等），返回 task_id 与预分配目标索引。
//
// 提交阶段只写 MySQL，不碰 OpenSearch：重建目标索引由 runner 幂等创建，
// 这样 OpenSearch 抖动时管理端仍能可靠登记任务，不会「以为没提交」而重复提交。
func (l *SubmitRebuildTaskLogic) SubmitRebuildTask(in *rpc.SubmitRebuildTaskReq) (*rpc.SubmitRebuildTaskReply, error) {
	res, err := l.svcCtx.Repository.SubmitRebuildTask(l.ctx, repository.SubmitRebuildInput{
		Scope:      in.Scope,
		ScopeValue: in.ScopeValue,
		Alias:      in.Alias,
		RequestID:  in.RequestId,
		Operator:   in.Operator,
	})
	if err != nil {
		l.Errorf("search-indexer/SubmitRebuildTask: scope=%s/%s alias=%s request_id=%s operator=%s err=%v",
			in.Scope, in.ScopeValue, in.Alias, in.RequestId, in.Operator, err)
		return nil, err
	}
	if res.Duplicated {
		l.Infof("search-indexer/SubmitRebuildTask: 命中 request_id 幂等 request_id=%s task_id=%s",
			in.RequestId, res.Task.TaskID)
	}

	return &rpc.SubmitRebuildTaskReply{
		TaskId:      res.Task.TaskID,
		TargetIndex: res.Task.TargetIndex,
		State:       res.Task.State,
		Duplicated:  res.Duplicated,
	}, nil
}
