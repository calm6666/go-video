package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetExpLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetExpLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetExpLogic {
	return &SetExpLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 直接设置经验值（仅运营）。
// 参考 service.SetExp：rank>=10000 校验→Set 到目标值→写经验日志→失效缓存。
func (l *SetExpLogic) SetExp(in *rpc.AddExpReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetExp(l.ctx, in.Mid, in.Count, in.Operate, in.Reason, in.Ip); err != nil {
		l.Errorf("user-profile/SetExp: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
