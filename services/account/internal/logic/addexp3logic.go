package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddExp3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddExp3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *AddExp3Logic {
	return &AddExp3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddExp3 增加经验值。
// 参考 service.AddExp：透传给 user-profile 服务持久化，经验值数据所有者属于 user-profile。
// repository 在写成功后失效该 mid 的 Info/Card/Profile/Vip 缓存。
// 当 user-profile 尚未接入时返回 repository.ErrNotImplemented。
func (l *AddExp3Logic) AddExp3(in *rpc.ExpReq) (*rpc.ExpReply, error) {
	if err := l.svcCtx.Repository.AddExp(l.ctx, in.Mid, in.Exp, in.Operater, in.Operate, in.Reason); err != nil {
		l.Errorf("account/AddExp3: repository.AddExp mid=%d exp=%v err=%v", in.Mid, in.Exp, err)
		return nil, err
	}
	return &rpc.ExpReply{}, nil
}
