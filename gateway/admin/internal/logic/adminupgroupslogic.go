// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrpc "go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminUpGroupsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询全部 UP 主特殊分组（按分组 ID 升序投影）
func NewAdminUpGroupsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminUpGroupsLogic {
	return &AdminUpGroupsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminUpGroups 聚合 creator UpGroups RPC（契约是 NoArgReq，分组字典无分页、无筛选）。
// 下游返回 map<int64,*UpGroup>，这里展平成按分组 ID 升序的数组，保证前端可直接 diff。
func (l *AdminUpGroupsLogic) AdminUpGroups() (resp *types.AdminUpGroupsResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}

	reply, err := l.svcCtx.Creator.UpGroups(l.ctx, &creatorrpc.NoArgReq{})
	if err != nil {
		l.Errorf("gateway/admin/adminUpGroups: err=%v", err)
		return nil, err
	}
	return &types.AdminUpGroupsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AdminUpGroupsData{Groups: upGroupsToAPI(reply.GetUpGroups())},
		TTL:     0,
	}, nil
}
