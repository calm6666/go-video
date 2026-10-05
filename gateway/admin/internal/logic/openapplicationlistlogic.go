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

type OpenApplicationListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 应用台账分页（运营全量分支；按开发者筛属开发者侧，见契约缺口）
func NewOpenApplicationListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenApplicationListLogic {
	return &OpenApplicationListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenApplicationList 转发 open-platform ListApplications 的运营全量分支。
//
// 表单里刻意没有 owner_mid：契约的运营分支（ListAll）只按 status + 游标筛，完全不读这一位
// （服务侧 listapplicationslogic 注释：in.Operator 只能由网关按会话角色置位）。
// 放上它就是一条「填了也不生效」的假筛选项；「按开发者查他的应用」属开发者侧。
// status=0 是契约里的「不按状态过滤」，未知取值由服务拒（不当成「不过滤」）；
// ps=0 由服务换成配置默认页大小，上限与超限拒绝都在服务侧。
func (l *OpenApplicationListLogic) OpenApplicationList(req *types.ParamOpenApplicationList) (resp *types.OpenApplicationListResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openSessionGate(l.ctx, "openApplicationList"); err != nil {
		return nil, err
	}
	if err := openNonNeg("status", int64(req.Status)); err != nil {
		return nil, err
	}
	if err := openNonNeg("ps", int64(req.Ps)); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.ListApplications(l.ctx, &openplatformrpc.ListApplicationsReq{
		Operator: true, // 后台入口这一事实由网关置位，表单没有也不该有这一位
		Status:   openplatformrpc.AppStatus(req.Status),
		Cursor:   req.Cursor,
		Ps:       req.Ps,
		TraceId:  req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openApplicationList: status=%d ps=%d err=%v", req.Status, req.Ps, err)
		return nil, err
	}
	return &types.OpenApplicationListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenApplicationListData{
			List:       openAppsToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
		},
		TTL: 0,
	}, nil
}
