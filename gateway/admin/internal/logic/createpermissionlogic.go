// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreatePermissionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 创建权限点（resource + action 唯一）
func NewCreatePermissionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePermissionLogic {
	return &CreatePermissionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 创建权限点：(resource, action) 唯一性与 domain 归属由 operation 判定。
// 这是后台最敏感的写接口之一（新增权限点等于扩大授权面），必须带 request_id 留痕。
func (l *CreatePermissionLogic) CreatePermission(req *types.ParamCreatePermission) (resp *types.OperationPermissionResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("resource", req.Resource); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("action", req.Action); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Operation.CreatePermission(l.ctx, &operationrpc.CreatePermissionReq{
		Ctx:         opCtx,
		Resource:    req.Resource,
		Action:      req.Action,
		Domain:      req.Domain,
		Description: req.Description,
	})
	if err != nil {
		l.Errorf("gateway/admin/createPermission: operator=%d resource=%s action=%s domain=%q err=%v",
			opCtx.GetOperatorId(), req.Resource, req.Action, req.Domain, err)
		return nil, err
	}
	l.Infof("gateway/admin/createPermission: operator=%d permission_id=%d resource=%s action=%s request_id=%s",
		opCtx.GetOperatorId(), reply.GetPermission().GetPermissionId(), reply.GetPermission().GetResource(),
		reply.GetPermission().GetAction(), opCtx.GetRequestId())
	return &types.OperationPermissionResponse{
		Code:    0,
		Message: "ok",
		Data:    types.OperationPermissionData{Permission: permissionToAPI(reply.GetPermission())},
		TTL:     0,
	}, nil
}
