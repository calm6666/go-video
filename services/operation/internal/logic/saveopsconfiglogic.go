package logic

import (
	"context"

	"go-video/services/operation/internal/repository"
	"go-video/services/operation/internal/svc"
	"go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveOpsConfigLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveOpsConfigLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveOpsConfigLogic {
	return &SaveOpsConfigLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 写入运营配置（expect_version 乐观锁 + 操作者留痕）
func (l *SaveOpsConfigLogic) SaveOpsConfig(in *rpc.SaveOpsConfigReq) (*rpc.SaveOpsConfigReply, error) {
	actor, err := actorFrom(in.Ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := l.svcCtx.Repository.SaveOpsConfig(l.ctx, actor, repository.SaveConfigInput{
		CfgKey:        in.CfgKey,
		CfgValue:      in.CfgValue,
		ValueType:     in.ValueType,
		Scope:         in.Scope,
		ExpectVersion: in.ExpectVersion,
		State:         in.State,
		Remark:        in.Remark,
	})
	if err != nil {
		// 乐观锁冲突（model.ErrConfigVersionConflict）以错误返回：
		// 后台需重新拉取最新版本后再提交，不做静默覆盖。
		l.Errorf("operation/SaveOpsConfig: operator=%d key=%q expect_version=%d err=%v",
			actor.AdminID, in.CfgKey, in.ExpectVersion, err)
		return nil, err
	}
	return &rpc.SaveOpsConfigReply{Config: configItem(cfg)}, nil
}
