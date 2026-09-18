package logic

import (
	"context"

	"go-video/services/user-profile/internal/repository"
	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddMoralLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddMoralLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddMoralLogic {
	return &AddMoralLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 变更节操值。
// 参考 service.UpdateMoral：事务内读-改-写 + 日志，变更后失效缓存并按阈值通知。
func (l *AddMoralLogic) AddMoral(in *rpc.UpdateMoralReq) (*rpc.EmptyReply, error) {
	err := l.svcCtx.Repository.UpdateMoral(l.ctx, &repository.UpdateMoralArg{
		Mid:        in.Mid,
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
		l.Errorf("user-profile/AddMoral: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
