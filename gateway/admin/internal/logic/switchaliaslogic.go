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

type SwitchAliasLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 切换查询别名到新版本索引（expected_current 乐观校验）
func NewSwitchAliasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SwitchAliasLogic {
	return &SwitchAliasLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 别名切换：聚合 search-indexer SwitchAlias RPC（零停机重建的关键步骤）。
// 索引名前缀、expected_current 乐观校验、目标索引 doc 数健康校验都由服务端执行，
// 网关不校验索引名，也不在失败时自动重试——误切换会直接影响线上查询（AGENTS.md §5）。
func (l *SwitchAliasLogic) SwitchAlias(req *types.ParamSwitchAlias) (resp *types.SearchSwitchAliasResponse, err error) {
	if l.svcCtx.SearchIndexer == nil {
		return nil, errors.New("search-indexer service not configured")
	}
	operatorID, err := adminOperatorID(l.ctx, "switchAlias", req.OperatorId)
	if err != nil {
		return nil, err
	}
	// 契约缺口：searchindexer.v1.SwitchAliasReq 只有字符串 operator，
	// 没有数值型运营账号字段，网关的 operator_id 只能落在本地审计日志里。
	if err := requireNonEmpty("operator", req.Operator); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("target_index", req.TargetIndex); err != nil {
		return nil, err
	}
	if req.SkipHealthCheck {
		// 跳过健康校验只用于紧急回滚：网关不拦截，但必须在日志里留下决策主体。
		l.Infof("gateway/admin/switchAlias: 运营跳过目标索引健康校验（仅限紧急回滚） alias=%s target_index=%s operator_id=%d operator=%s",
			req.Alias, req.TargetIndex, operatorID, req.Operator)
	}
	reply, err := l.svcCtx.SearchIndexer.SwitchAlias(l.ctx, &searchindexerrpc.SwitchAliasReq{
		Alias:           req.Alias,
		TargetIndex:     req.TargetIndex,
		ExpectedCurrent: req.ExpectedCurrent,
		SkipHealthCheck: req.SkipHealthCheck,
		Operator:        req.Operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/switchAlias: alias=%s target_index=%s expected_current=%s operator_id=%d err=%v",
			req.Alias, req.TargetIndex, req.ExpectedCurrent, operatorID, err)
		return nil, err
	}
	return &types.SearchSwitchAliasResponse{
		Code:    0,
		Message: "ok",
		Data: types.SearchSwitchAliasData{
			Alias:         reply.GetAlias(),
			PreviousIndex: reply.GetPreviousIndex(),
			CurrentIndex:  reply.GetCurrentIndex(),
			DocCount:      reply.GetDocCount(),
			RecordState:   reply.GetRecordState(),
		},
		TTL: 0,
	}, nil
}
