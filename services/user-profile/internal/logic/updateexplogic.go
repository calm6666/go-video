package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateExpLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateExpLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateExpLogic {
	return &UpdateExpLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 更新经验值（增加）。
// 参考 service.UpdateExp：rank>=10000 校验→exp==0 时 Set 否则 Incr→写经验日志→失效缓存。
func (l *UpdateExpLogic) UpdateExp(in *rpc.AddExpReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.UpdateExp(l.ctx, in.Mid, in.Count, in.Operate, in.Reason, in.Ip); err != nil {
		l.Errorf("user-profile/UpdateExp: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
