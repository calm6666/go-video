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

type GetOpsConfigLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 读取运营配置（默认走缓存，refresh=true 强制回源）
func NewGetOpsConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetOpsConfigLogic {
	return &GetOpsConfigLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 读取运营配置：scope 为空由 operation 视为 global；from_cache 与 version 一并透出，
// 后台改配置前必须看到 version 才能带 expect_version 提交（乐观锁闭环在客户端表单）。
func (l *GetOpsConfigLogic) GetOpsConfig(req *types.ParamGetOpsConfig) (resp *types.OperationConfigResponse, err error) {
	if l.svcCtx.Operation == nil {
		return nil, errors.New("operation service not configured")
	}
	opCtx, err := operationOpContext(l.ctx, req.Op, false)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("cfg_key", req.CfgKey); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Operation.GetOpsConfig(l.ctx, &operationrpc.GetOpsConfigReq{
		Ctx:     opCtx,
		CfgKey:  req.CfgKey,
		Scope:   req.Scope,
		Refresh: req.Refresh,
	})
	if err != nil {
		l.Errorf("gateway/admin/getOpsConfig: operator=%d cfg_key=%q scope=%q refresh=%v err=%v",
			opCtx.GetOperatorId(), req.CfgKey, req.Scope, req.Refresh, err)
		return nil, err
	}
	return &types.OperationConfigResponse{
		Code:    0,
		Message: "ok",
		Data: types.OperationConfigData{
			Config:    configToAPI(reply.GetConfig()),
			FromCache: reply.GetFromCache(),
		},
		TTL: int64TTL(reply.GetTtl()),
	}, nil
}
