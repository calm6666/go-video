// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	spmrpc "go-video/services/spm/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SpmMetricDefinitionListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 口径目录分页（含 DRAFT/RETIRED：历史窗口要靠旧口径解释）
func NewSpmMetricDefinitionListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmMetricDefinitionListLogic {
	return &SpmMetricDefinitionListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmMetricDefinitionList 转发 spm ListMetricDefinitions（口径目录分页）。
//
// state=0（UNSPECIFIED）是「不限状态」，网关不替后台默认成「只看 ACTIVE」：RETIRED 口径还在
// 解释历史窗口，看不到的时候运营会以为那批数据是凭空长出来的。
// total 与服务排序一律以服务回值为准，网关不本地过滤（先分页再筛与先筛再分页是两个总数）。
func (l *SpmMetricDefinitionListLogic) SpmMetricDefinitionList(req *types.ParamSpmMetricDefinitionList) (resp *types.SpmMetricDefinitionListResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	if err := spmNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := spmPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.ListMetricDefinitions(l.ctx, &spmrpc.ListMetricDefinitionsReq{
		MetricKey: req.MetricKey,
		State:     spmrpc.DefinitionState(req.State),
		Pn:        req.Pn,
		Ps:        req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmMetricDefinitionList: metric_key=%s state=%d pn=%d ps=%d err=%v",
			req.MetricKey, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SpmMetricDefinitionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmMetricDefinitionListData{
			Definitions: spmDefinitionsToAPI(reply.GetDefinitions()),
			Total:       reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
