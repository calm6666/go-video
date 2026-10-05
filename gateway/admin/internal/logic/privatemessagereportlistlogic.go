// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PrivateMessageReportListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 举报台账游标翻页（状态/被举报人过滤；读取主体由 operator_mid 承载）
func NewPrivateMessageReportListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PrivateMessageReportListLogic {
	return &PrivateMessageReportListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PrivateMessageReportList 聚合 private-message ListReports。
//
// 本路由刻意不进 routePermissions（与 audit entry/list、ops-config config/list、cron task/list、
// collector 只读面同口径）：排障页每次刷新都会打一次 RPC，把读取也挂判定会把 operation 变成
// 读放大瓶颈；而这份台账本身不含任何写能力。读取主体仍必填 —— private-message 用 operator_mid
// 判定「谁能看举报台账」，网关不代为放宽，也不把 0 当「不限主体」。
//
// 网关只做三件事：形状校验（负数、page_size 越界的下限由服务判）、转达、投影。
// 游标语法、page_size 上限、状态枚举的合法集合都在服务侧。
func (l *PrivateMessageReportListLogic) PrivateMessageReportList(req *types.ParamPrivateMessageReportList) (resp *types.PrivateMessageReportListResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errPMServiceNotConfigured
	}
	if req == nil {
		return nil, errPMRequestMissing
	}
	if err := requireOperator("operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := pmNonNeg("target_mid", req.TargetMid); err != nil {
		return nil, err
	}
	if err := pmNonNeg("page_size", int64(req.PageSize)); err != nil {
		return nil, err
	}
	if err := pmNonNeg("state", int64(req.State)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.PrivateMessage.ListReports(l.ctx, &privatemessagerpc.ListReportsReq{
		State:       privatemessagerpc.ReportState(req.State),
		TargetMid:   req.TargetMid,
		Cursor:      req.Cursor,
		Ps:          req.PageSize,
		OperatorMid: req.OperatorMid,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/privateMessageReportList: state=%d target_mid=%d cursor=%q page_size=%d operator_mid=%d err=%v",
			req.State, req.TargetMid, req.Cursor, req.PageSize, req.OperatorMid, err)
		return nil, err
	}
	return &types.PrivateMessageReportListResponse{
		Code:    0,
		Message: "ok",
		Data:    pmReportListToAPI(reply),
		TTL:     0,
	}, nil
}
