// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	danmakurpc "go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportDanmakuLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 举报弹幕
func NewReportDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportDanmakuLogic {
	return &ReportDanmakuLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ReportDanmaku 写 danmaku 自己的举报表，由 moderation 拉取处理；
// 网关不直连审核库（AGENTS.md §5）。同一举报者重复举报返回既有记录。
func (l *ReportDanmakuLogic) ReportDanmaku(req *types.ParamDanmakuReport) (resp *types.DanmakuReportResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	reply, err := l.svcCtx.Danmaku.ReportDanmaku(l.ctx, &danmakurpc.ReportDanmakuReq{
		Dmid:        req.Dmid,
		ReporterMid: req.ReporterMid,
		Reason:      req.Reason,
		Content:     req.Content,
	})
	if err != nil {
		l.Errorf("gateway/app/reportDanmaku: dmid=%d reporter=%d err=%v", req.Dmid, req.ReporterMid, err)
		return nil, err
	}
	return &types.DanmakuReportResponse{
		Code:    0,
		Message: "ok",
		Data: types.DanmakuReportData{
			ReportId:   reply.GetReportId(),
			Duplicated: reply.GetDuplicated(),
		},
		TTL: 0,
	}, nil
}
