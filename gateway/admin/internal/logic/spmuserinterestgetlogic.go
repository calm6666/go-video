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

type SpmUserInterestGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单用户兴趣画像（脱敏受控词表；stale=true 时调用方应按冷启动口径解释）
func NewSpmUserInterestGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SpmUserInterestGetLogic {
	return &SpmUserInterestGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SpmUserInterestGet 转发 spm GetUserInterest（针对单个 mid 的兴趣画像投影）。
//
// 本路由受 AdminPermission 保护（spm:interest / read），与榜单类只读不同档：它是「定向读某个人
// 的画像」，每次都有明确的被读主体，授权要能单独收回、访问要进判定与留痕。
// 契约缺口（已上报）：GetUserInterestReq 只有 mid/metric_version/top_n 三位，**没有 operator 位**，
// 因此这次读取在 spm 侧落不下「谁读的」——留痕只剩网关访问日志与 operation 的判定记录两处，
// 服务侧无法自证。会话身份因此只用于日志主体，不下传（没有可传的字段，也不塞进别的位）。
// top_n 上限 100 由服务判（超了是拒，不是裁：裁一半会让后台以为这个人的兴趣就只有这么多）。
// stale=true 原样回：网关不因为「画像过期」把它折叠成空列表——空列表与过期是两件事，
// 前者是「没兴趣」，后者是「这些数据不能用来做冷启动判断」。
// 日志只打 mid 与条数：interest_key 是行为画像内容，不进日志（§7）。
func (l *SpmUserInterestGetLogic) SpmUserInterestGet(req *types.ParamSpmUserInterestGet) (resp *types.SpmUserInterestGetResponse, err error) {
	if l.svcCtx.Spm == nil {
		return nil, errSpmServiceNotConfigured
	}
	if req == nil {
		return nil, errSpmRequestMissing
	}
	operator, err := spmOperator(l.ctx, "spmUserInterestGet")
	if err != nil {
		return nil, err
	}
	if err := spmPositiveID("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := spmNonNeg("metric_version", int64(req.MetricVersion)); err != nil {
		return nil, err
	}
	if err := spmNonNeg("top_n", int64(req.TopN)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Spm.GetUserInterest(l.ctx, &spmrpc.GetUserInterestReq{
		Mid:           req.Mid,
		MetricVersion: req.MetricVersion,
		TopN:          req.TopN,
	})
	if err != nil {
		l.Errorf("gateway/admin/spmUserInterestGet: mid=%d metric_version=%d top_n=%d operator=%s err=%v",
			req.Mid, req.MetricVersion, req.TopN, operator, err)
		return nil, err
	}
	l.Infof("gateway/admin/spmUserInterestGet: mid=%d interests=%d stale=%t operator=%s",
		req.Mid, len(reply.GetInterests()), reply.GetStale(), operator)
	return &types.SpmUserInterestGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.SpmUserInterestGetData{
			Interests:     spmInterestsToAPI(reply.GetInterests()),
			MetricVersion: reply.GetMetricVersion(),
			Stale:         reply.GetStale(),
		},
		TTL: 0,
	}, nil
}
