// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpenQuotaUsageListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 配额用量读（按应用；投影可漂移，跨应用汇总不在本契约）
func NewOpenQuotaUsageListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenQuotaUsageListLogic {
	return &OpenQuotaUsageListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenQuotaUsageList 转发 open-platform ListQuotaUsage。
//
// app_id 必填 >0：用量投影按真实 app_id 记账（op_quota_usage 里没有 0 这一行，
// app_id=0 只在规则层表示「兜底层级」），跨应用汇总属 SPM/运营报表口径，不在本契约。
// operator_mid 必填 >0：mid==0 在服务侧被解释成「owner 自查，归属由网关校验」，
// 而后台既不是 owner 也没做过那道归属校验——两者读到的行集相同、审计结论不同。
// window_start=0 = 当前窗口，是合法哨兵而不是「未填」；读不到生效规则时服务回错误而不是
// 一个看起来正常的 0 余额，网关也不把该错误折叠成空列表。
func (l *OpenQuotaUsageListLogic) OpenQuotaUsageList(req *types.ParamOpenQuotaUsageList) (resp *types.OpenQuotaUsageListResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openOperatorGate(l.ctx, "openQuotaUsageList", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("window_start", req.WindowStart); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.ListQuotaUsage(l.ctx, &openplatformrpc.ListQuotaUsageReq{
		AppId:       req.AppId,
		ApiCode:     req.ApiCode,
		WindowStart: req.WindowStart,
		OperatorMid: req.OperatorMid,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openQuotaUsageList: app_id=%d window_start=%d operator_mid=%d err=%v",
			req.AppId, req.WindowStart, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenQuotaUsageListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenQuotaUsageListData{
			List: openQuotaUsagesToAPI(reply.GetList()),
		},
		TTL: 0,
	}, nil
}
