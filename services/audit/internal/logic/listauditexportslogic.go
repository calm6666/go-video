package logic

import (
	"context"
	"strings"

	"go-video/services/audit/internal/svc"
	"go-video/services/audit/model"
	"go-video/services/audit/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAuditExportsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAuditExportsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAuditExportsLogic {
	return &ListAuditExportsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页列出导出任务
//
// 实现要点（规格 1~3）：
//  1. clampPage 归一化分页（ps 上限 Query.MaxPageSize）；
//  2. operator_id / state / ctime 区间为可选过滤，排序键固定在 model 侧（task_id DESC）；
//  3. 只回任务元信息，不回 filter_json 展开内容：导出条件本身可能含敏感维度，
//     要看条件走 GetAuditExport（那里才会留痕）。
//
// 本方法不写自审计：它回的是「有没有这么一次导出」，不含任何存证内容；
// 真正的数据出口在 GetAuditExport 签发地址那一刻，那里已经逐次留痕。
func (l *ListAuditExportsLogic) ListAuditExports(in *rpc.ListAuditExportsReq) (*rpc.ListAuditExportsReply, error) {
	if in == nil {
		return nil, model.ErrRequestRequired
	}
	cc := in.GetCtx()
	if err := validateCallContext(cc, false); err != nil {
		return nil, err
	}
	state, err := exportStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	if in.GetOperatorId() < 0 {
		return nil, model.ErrActorIDInvalid
	}
	if err := ctimeWindow(in.GetStartAt(), in.GetEndAt()); err != nil {
		return nil, err
	}
	d := buildDeps(l.svcCtx)
	pn, ps, err := d.clampPage(in.GetPn(), in.GetPs())
	if err != nil {
		return nil, err
	}
	rows, total, err := d.exports.List(l.ctx, model.ExportTaskFilter{
		OperatorID: in.GetOperatorId(),
		State:      state,
		StartAt:    in.GetStartAt(),
		EndAt:      in.GetEndAt(),
		Pn:         pn,
		Ps:         ps,
	})
	if err != nil {
		l.Errorf("ListAuditExports 查询失败 caller=%s state=%s err=%v",
			strings.TrimSpace(cc.GetCallerService()), state, err)
		return nil, err
	}
	return &rpc.ListAuditExportsReply{Items: exportTaskViews(rows), Total: total}, nil
}
