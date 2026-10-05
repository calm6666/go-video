package logic

import (
	"context"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListConfigsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListConfigsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListConfigsLogic {
	return &ListConfigsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 后台分页列出配置项
func (l *ListConfigsLogic) ListConfigs(in *rpc.ListConfigsReq) (*rpc.ListConfigsReply, error) {
	lim := newLimits(l.svcCtx.Config)
	pn, ps, err := pageOf(in.GetPn(), in.GetPs(), lim.maxPageSize)
	if err != nil {
		return nil, err
	}
	scope := strings.TrimSpace(in.GetScope())
	if scope != "" {
		if err := checkScope(scope); err != nil {
			return nil, err
		}
	}
	if in.GetState() != 0 {
		if err := checkState(in.GetState()); err != nil {
			return nil, err
		}
	}
	keyword, err := checkKeywordLen(in.GetKeyword())
	if err != nil {
		return nil, err
	}
	rows, total, err := l.svcCtx.Models.ConfigItem.List(l.ctx, model.ConfigItemFilter{
		Scope: scope, Keyword: keyword, State: in.GetState(), Pn: pn, Ps: ps,
	})
	if err != nil {
		return nil, err
	}
	// 投影不回配置值：值只存在于 ops_config_version。
	// 列表页要值请走 ListConfigVersions，否则「列表显示的是旧值」会成为第二个事实源。
	return &rpc.ListConfigsReply{Items: configItemList(rows), Total: total}, nil
}
