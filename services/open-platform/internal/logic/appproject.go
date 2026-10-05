package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"
)

// 应用投影的批量组装。
//
// 列表接口必须避免 N+1：每行的 scopes 与 secret 概览各用一次批量查询补齐
// （AppScopes.GrantedByApps / Secrets.SummariesByApps），
// 否则一次 ps=50 的列表会变成 100 次查询——放大成未定义规模的扫描。
// 两个批量方法都只读非敏感列，salt/hash 不进内存。

// appProjection 单应用投影（详情面）。
func appProjection(ctx context.Context, s *svc.ServiceContext,
	app *model.Application) (*rpc.ApplicationInfo, error) {
	if app == nil {
		return nil, model.ErrAppNotFound
	}
	granted, err := s.AppScopes.ListGranted(ctx, app.AppID)
	if err != nil {
		return nil, err
	}
	sums, err := s.Secrets.SummariesByApps(ctx, []int64{app.AppID}, nowUnix())
	if err != nil {
		return nil, err
	}
	return projectApp(app, granted, sums[app.AppID]), nil
}

// appProjections 批量投影，顺序与入参一致（nil 行跳过，保证响应不出现 null 元素）。
func appProjections(ctx context.Context, s *svc.ServiceContext,
	apps []*model.Application) ([]*rpc.ApplicationInfo, error) {
	ids := make([]int64, 0, len(apps))
	for _, a := range apps {
		if a != nil {
			ids = append(ids, a.AppID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	grantedMap, err := s.AppScopes.GrantedByApps(ctx, ids)
	if err != nil {
		return nil, err
	}
	sumMap, err := s.Secrets.SummariesByApps(ctx, ids, nowUnix())
	if err != nil {
		return nil, err
	}
	out := make([]*rpc.ApplicationInfo, 0, len(apps))
	for _, a := range apps {
		if a == nil {
			continue
		}
		out = append(out, projectApp(a, grantedMap[a.AppID], sumMap[a.AppID]))
	}
	return out, nil
}
