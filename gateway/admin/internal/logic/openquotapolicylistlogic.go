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

type OpenQuotaPolicyListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 配额规则目录分页（服务侧 requireOperator；app_id=0 是全局兜底层级、api_code=「*」是通配规则本身）
func NewOpenQuotaPolicyListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenQuotaPolicyListLogic {
	return &OpenQuotaPolicyListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenQuotaPolicyList 转发 open-platform ListQuotaPolicies。
//
// 本方法在契约层就只服务运营：服务用 requireOperator 拒 mid<=0（规则集含运营意图，
// 不按应用 owner 切片外发），网关漏配中间件也拿不到数据——正因如此这一条挂权限点。
// app_id=0 是「只看全局兜底层级」的合法取值而不是「不限」，跨层级查询要留空（不放假筛选项）；
// api_code 空 = 不过滤、"*" = 通配规则本身，两者不同义，网关不做归一化。
// ps=0 由服务换成配置默认页大小，超上限直接拒而不是静默裁剪（裁剪会让调用方以为拿到了全部）。
func (l *OpenQuotaPolicyListLogic) OpenQuotaPolicyList(req *types.ParamOpenQuotaPolicyList) (resp *types.OpenQuotaPolicyListResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openOperatorGate(l.ctx, "openQuotaPolicyList", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := openNonNeg("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("ps", int64(req.Ps)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.ListQuotaPolicies(l.ctx, &openplatformrpc.ListQuotaPoliciesReq{
		AppId:       req.AppId,
		ApiCode:     req.ApiCode,
		Cursor:      req.Cursor,
		Ps:          req.Ps,
		OperatorMid: req.OperatorMid,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openQuotaPolicyList: app_id=%d api_code=%s operator_mid=%d err=%v",
			req.AppId, req.ApiCode, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenQuotaPolicyListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenQuotaPolicyListData{
			List:       openQuotaPoliciesToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
