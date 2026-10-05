package logic

import (
	"context"
	"time"

	"go-video/services/danmaku/internal/svc"
	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportDanmakuLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportDanmakuLogic {
	return &ReportDanmakuLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ReportDanmaku 举报弹幕。
//
// 依据 AGENTS.md §5：只写本服务自有的 danmaku_report 表，不直连 moderation 库、
// 也不改弹幕状态；审核域通过 ListPendingReports 拉取后回写结论。
// 幂等由唯一索引 uniq_dmid_reporter 保证，重复举报返回既有记录且 duplicated=true。
func (l *ReportDanmakuLogic) ReportDanmaku(in *rpc.ReportDanmakuReq) (*rpc.ReportDanmakuReply, error) {
	if in.Dmid <= 0 {
		return nil, model.ErrInvalidDmid
	}
	if in.ReporterMid <= 0 {
		return nil, model.ErrInvalidMid
	}
	d, err := l.svcCtx.Repository.GetDanmaku(l.ctx, in.Dmid)
	if err != nil {
		l.Errorf("danmaku/ReportDanmaku: get dmid=%d err=%v", in.Dmid, err)
		return nil, err
	}
	if d == nil {
		return nil, model.ErrDanmakuNotFound
	}
	if d.Mid == in.ReporterMid {
		// 自举报无意义，交由审核队列的其它来源处理。
		return nil, model.ErrForbidden
	}

	rep := &model.Report{
		Dmid:        in.Dmid,
		ReporterMid: in.ReporterMid,
		Reason:      in.Reason,
		Content:     in.Content,
		State:       model.ReportPending,
		TraceID:     in.TraceId,
		Ctime:       time.Now().Unix(),
	}
	reportID, created, err := l.svcCtx.Repository.ReportDanmaku(l.ctx, rep)
	if err != nil {
		l.Errorf("danmaku/ReportDanmaku: insert dmid=%d reporter=%d err=%v", in.Dmid, in.ReporterMid, err)
		return nil, err
	}
	return &rpc.ReportDanmakuReply{ReportId: reportID, Duplicated: !created}, nil
}
