package logic

import (
	"context"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListConfigVersionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListConfigVersionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListConfigVersionsLogic {
	return &ListConfigVersionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 版本历史分页
func (l *ListConfigVersionsLogic) ListConfigVersions(in *rpc.ListConfigVersionsReq) (*rpc.ListConfigVersionsReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkCfgKey(in.GetCfgKey(), lim); err != nil {
		return nil, err
	}
	pn, ps, err := pageOf(in.GetPn(), in.GetPs(), lim.maxPageSize)
	if err != nil {
		return nil, err
	}
	scope := normalizeScope(in.GetScope())
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	item, err := l.svcCtx.Models.ConfigItem.FindOne(l.ctx, trimKey(in.GetCfgKey()), scope)
	if err != nil {
		return nil, err
	}
	if item == nil {
		// 键不存在回空列表而不是报错：后台筛选框打错字不该 500。
		return &rpc.ListConfigVersionsReply{Items: nil, Total: 0, LatestVersion: 0}, nil
	}
	rows, total, err := l.svcCtx.Models.ConfigVersion.ListByConfig(l.ctx, item.ConfigID, pn, ps)
	if err != nil {
		return nil, err
	}
	// 值全量回、audit_entry_id=0 原样可见、request_id 一并回：
	// 这是版本历史，任何截断与隐藏都是在销毁「当时线上到底是什么」的证据。
	return &rpc.ListConfigVersionsReply{
		Items:         configVersionList(rows),
		Total:         lim.totalOf(total),
		LatestVersion: item.LatestVersion,
	}, nil
}
