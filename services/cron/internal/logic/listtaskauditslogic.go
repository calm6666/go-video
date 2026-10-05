package logic

import (
	"context"
	"fmt"

	"go-video/services/cron/internal/svc"
	"go-video/services/cron/model"
	"go-video/services/cron/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTaskAuditsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTaskAuditsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTaskAuditsLogic {
	return &ListTaskAuditsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询任务变更审计。
//
// 审计是只追加的运营轨迹，本服务不提供更新/删除接口；超期清理由内部清理任务按
// Task.AuditRetentionDays 先归档再 DeleteBefore。
// 时间窗两端都给 0 时，服务端按留存期兜一个下界，避免对大表做无界 count/scan。
func (l *ListTaskAuditsLogic) ListTaskAudits(in *rpc.ListTaskAuditsReq) (*rpc.ListTaskAuditsReply, error) {
	if in == nil {
		in = &rpc.ListTaskAuditsReq{}
	}
	limit, err := l.svcCtx.PageSize(in.PageSize)
	if err != nil {
		return nil, err
	}
	cursorID, err := decodeIDCursor(in.Cursor)
	if err != nil {
		return nil, err
	}
	if in.CtimeFrom > 0 && in.CtimeTo > 0 && in.CtimeFrom > in.CtimeTo {
		return nil, fmt.Errorf("%w: ctime_from=%d ctime_to=%d",
			model.ErrInvalidTimeRange, in.CtimeFrom, in.CtimeTo)
	}
	ctimeFrom, ctimeTo := in.CtimeFrom, in.CtimeTo
	if ctimeFrom == 0 && ctimeTo == 0 {
		ctimeFrom = defaultAuditWindowFrom(l.svcCtx, l.svcCtx.ServerTime())
	}

	rows, next, err := l.svcCtx.Audits.ListByCursor(l.ctx, in.TaskKey, in.Action, ctimeFrom, ctimeTo, cursorID, limit)
	if err != nil {
		l.Errorf("ListTaskAudits failed, task_key=%s action=%s", in.TaskKey, in.Action)
		return nil, err
	}
	total, err := l.svcCtx.Audits.CountByFilter(l.ctx, in.TaskKey, in.Action, ctimeFrom, ctimeTo)
	if err != nil {
		l.Errorf("ListTaskAudits count failed, task_key=%s action=%s", in.TaskKey, in.Action)
		return nil, err
	}
	return &rpc.ListTaskAuditsReply{
		List:       auditInfoList(rows),
		NextCursor: encodeIDCursor(next),
		HasMore:    next > 0,
		Total:      total,
	}, nil
}

// defaultAuditWindowFrom 按配置的审计留存天数给出查询下界（Unix 秒）。
func defaultAuditWindowFrom(svcCtx *svc.ServiceContext, now int64) int64 {
	days := svcCtx.Config.Task.AuditRetentionDays
	if days <= 0 {
		days = 180
	}
	from := now - int64(days)*86400
	if from < 0 {
		return 0
	}
	return from
}
