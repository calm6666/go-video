// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpenQuotaRecomputeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 从调用流水重算配额投影（dry_run 先看差异；投影重算幂等故契约无幂等键位）
func NewOpenQuotaRecomputeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenQuotaRecomputeLogic {
	return &OpenQuotaRecomputeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenQuotaRecompute 转发 open-platform RecomputeQuota（把 op_quota_usage 拉回流水真值附近）。
//
// dry_run 由调用方显式给：网关不把它默认成 false 去「顺手写回」——重算本身幂等（同区间重跑
// 结论一致），但一次意外的写回会把在线扣减与投影的对齐点挪走，排障时先看差异才是安全顺序。
// 契约因此没有幂等键位，网关也不编一个塞进别的字段。
// app_id 必填 >0：proto 注释写「0 表示全部应用」，但实现拒绝 app_id<=0——跨应用会把所有应用的
// 流水并进同一个窗口桶，拿合并计数覆盖单应用行等于打穿限额语义（比不重算危险）。
// 本域按实现立闸，缺口记在 admin.api 文末。区间上界（lookback×4）与窗口数上限由服务判。
func (l *OpenQuotaRecomputeLogic) OpenQuotaRecompute(req *types.ParamOpenQuotaRecompute) (resp *types.OpenQuotaRecomputeResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openOperatorGate(l.ctx, "openQuotaRecompute", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("window_start", req.WindowStart); err != nil {
		return nil, err
	}
	if err := openNonNeg("window_end", req.WindowEnd); err != nil {
		return nil, err
	}
	// [from,to) 倒着给圈不出任何窗口：服务同样拒，但这里的错误要说得清是区间写反了。
	if req.WindowEnd <= req.WindowStart {
		return nil, errors.New("gateway/admin: window_end must be greater than window_start")
	}
	reply, err := l.svcCtx.OpenPlatform.RecomputeQuota(l.ctx, &openplatformrpc.RecomputeQuotaReq{
		AppId:       req.AppId,
		ApiCode:     req.ApiCode,
		WindowStart: req.WindowStart,
		WindowEnd:   req.WindowEnd,
		DryRun:      req.DryRun,
		OperatorMid: req.OperatorMid,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openQuotaRecompute: app_id=%d window=[%d,%d) dry_run=%t operator_mid=%d err=%v",
			req.AppId, req.WindowStart, req.WindowEnd, req.DryRun, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenQuotaRecomputeResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenQuotaRecomputeData{
			WindowsScanned: reply.GetWindowsScanned(),
			WindowsFixed:   reply.GetWindowsFixed(),
			MaxDelta:       reply.GetMaxDelta(),
		},
		TTL: 0,
	}, nil
}
