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

type ListRebuildTasksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询重建任务（cursor + state 过滤，limit 上限 100）
func NewListRebuildTasksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRebuildTasksLogic {
	return &ListRebuildTasksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 重建任务列表：聚合 search-indexer ListRebuildTasks RPC。
// cursor 风格分页（不用 offset），limit 上限 100 与服务端一致；
// state 为空表示不过滤，取值集合由服务端定义。
func (l *ListRebuildTasksLogic) ListRebuildTasks(req *types.ParamListRebuildTasks) (resp *types.SearchRebuildTasksResponse, err error) {
	if l.svcCtx.SearchIndexer == nil {
		return nil, errors.New("search-indexer service not configured")
	}
	if err := requireOperatorID(req.OperatorId); err != nil {
		return nil, err
	}
	limit := normalizeSearchLimit(req.Limit)
	// 契约缺口：searchindexer.v1.ListRebuildTasksReq 没有 operator 字段，
	// 运营查了哪些任务无法落到服务侧审计，只能记在网关日志里。
	reply, err := l.svcCtx.SearchIndexer.ListRebuildTasks(l.ctx, &searchindexerrpc.ListRebuildTasksReq{
		State:  req.State,
		Cursor: req.Cursor,
		Limit:  limit,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRebuildTasks: state=%s cursor=%s limit=%d operator_id=%d err=%v",
			req.State, req.Cursor, limit, req.OperatorId, err)
		return nil, err
	}
	return &types.SearchRebuildTasksResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchRebuildTasksData{
			Tasks:      rebuildTasksToAPI(reply.GetTasks()),
			NextCursor: reply.GetNextCursor(),
		},
		TTL: 0,
	}, nil
}
