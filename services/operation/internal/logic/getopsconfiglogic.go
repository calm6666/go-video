package logic

import (
	"context"

	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetOpsConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetOpsConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetOpsConfigLogic {
	return &GetOpsConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 读取运营配置（默认走缓存，refresh=true 强制回源）
func (l *GetOpsConfigLogic) GetOpsConfig(in *rpc.GetOpsConfigReq) (*rpc.GetOpsConfigReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	cfg, fromCache, err := l.svcCtx.Repository.GetOpsConfig(l.ctx, in.CfgKey, in.Scope, in.Refresh)
	if err != nil {
		l.Errorf("operation/GetOpsConfig: operator=%d key=%q scope=%q err=%v", actor.AdminID, in.CfgKey, in.Scope, err)
		return nil, err
	}
	return &rpc.GetOpsConfigReply{
		Config:    configItem(cfg),
		FromCache: fromCache,
		Ttl:       int32(l.svcCtx.Repository.ConfigTTL()),
	}, nil
}
