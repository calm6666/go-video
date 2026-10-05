// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	searchqueryrpc "go-video/services/search-query/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportQueryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 上报查询行为（SPM 分析链路，query_id 幂等）
func NewReportQueryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportQueryLogic {
	return &ReportQueryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ReportQuery 只做 SPM 行为分析投递（AGENTS.md §7）：query_id 幂等，
// 明文 IP 不下发——ip_hash 由边缘/风控层写入，网关此处置空并在 README 记为待办。
func (l *ReportQueryLogic) ReportQuery(req *types.ParamReportQuery) (resp *types.ReportQueryResponse, err error) {
	if l.svcCtx.SearchQuery == nil {
		return nil, errors.New("search-query service not configured")
	}
	reply, err := l.svcCtx.SearchQuery.ReportQuery(l.ctx, &searchqueryrpc.ReportQueryReq{
		QueryId:      req.QueryId,
		Keyword:      req.Keyword,
		SearchType:   searchqueryrpc.SearchType(req.SearchType),
		Mid:          req.Mid,
		HitCount:     req.HitCount,
		ResultState:  searchqueryrpc.QueryResultState(req.ResultState),
		LatencyMs:    req.LatencyMs,
		Platform:     req.Platform,
		AppVersion:   req.AppVersion,
		DeviceIdHash: req.DeviceIdHash,
	})
	if err != nil {
		l.Errorf("gateway/app/reportQuery: query_id=%s err=%v", req.QueryId, err)
		return nil, err
	}
	return &types.ReportQueryResponse{
		Code:    0,
		Message: "ok",
		Data: types.ReportQueryData{
			Accepted:     reply.GetAccepted(),
			Deduplicated: reply.GetDeduplicated(),
			EventId:      reply.GetEventId(),
		},
		TTL: 0,
	}, nil
}
