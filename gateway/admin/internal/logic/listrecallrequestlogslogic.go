// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	recallrpc "go-video/services/recommend-recall/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRecallRequestLogsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 召回请求日志分页（按用户/场景/时间窗）
func NewListRecallRequestLogsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRecallRequestLogsLogic {
	return &ListRecallRequestLogsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListRecallRequestLogs 转发 recommend-recall ListRecallRequestLogs。
// 过滤条件按契约原样给：mid=0 表示不按用户过滤（游客请求也进结果集），scene 空表示不过滤。
// 网关只做形态门槛（非负、时间窗不倒置），页大小上限 MaxRequestLogPage 与
// 「这个用户能不能被这样查」的数据权限都在服务侧（AGENTS.md §7：行为数据脱敏在服务落地）。
// 这是只读面：本路由不挂 AdminPermission，读取主体不落 recall_request_log（契约里就没有操作者列），
// 与 audit/ops-config/cron/live 的读面同一口径。
func (l *ListRecallRequestLogsLogic) ListRecallRequestLogs(req *types.ParamRecommendRecallLogList) (resp *types.RecommendRecallLogListResponse, err error) {
	if l.svcCtx.RecommendRecall == nil {
		return nil, errRecallServiceNotConfigured
	}
	if req == nil {
		return nil, errRecommendRequestMissing
	}
	if err := recommendNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := recommendTimeWindow(req.FromTime, req.ToTime); err != nil {
		return nil, err
	}
	if err := recommendPaging(req.Pn, req.Ps); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.RecommendRecall.ListRecallRequestLogs(l.ctx, &recallrpc.ListRecallRequestLogsReq{
		Mid:      req.Mid,
		Scene:    req.Scene,
		FromTime: req.FromTime,
		ToTime:   req.ToTime,
		Pn:       req.Pn,
		Ps:       req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRecallRequestLogs: scene=%s mid=%d pn=%d err=%v", req.Scene, req.Mid, req.Pn, err)
		return nil, err
	}
	return &types.RecommendRecallLogListResponse{
		Code:    0,
		Message: "ok",
		Data: types.RecommendRecallLogListData{
			List:    recallRequestLogsToAPI(reply.GetEntries()),
			HasMore: reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
