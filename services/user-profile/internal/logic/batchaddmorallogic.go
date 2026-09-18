package logic

import (
	"context"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type BatchAddMoralLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBatchAddMoralLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BatchAddMoralLogic {
	return &BatchAddMoralLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 批量变更节操值。
// 参考 service.UpdateMorals：单事务批量读-改-写 + 日志，返回各用户变更后的节操值。
func (l *BatchAddMoralLogic) BatchAddMoral(in *rpc.UpdateMoralsReq) (*rpc.UpdateMoralsReply, error) {
	afterMorals, err := l.svcCtx.Repository.BatchUpdateMoral(l.ctx, in.Mids, &repository.UpdateMoralArg{
		Delta:      in.Delta,
		Origin:     in.Origin,
		Reason:     in.Reason,
		ReasonType: in.ReasonType,
		Operator:   in.Operator,
		Remark:     in.Remark,
		Status:     in.Status,
		IsNotify:   in.IsNotify,
		IP:         in.Ip,
	})
	if err != nil {
		l.Errorf("user-profile/BatchAddMoral: err=%v", err)
		return nil, err
	}
	return &rpc.UpdateMoralsReply{AfterMorals: afterMorals}, nil
}
