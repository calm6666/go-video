package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetNameLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetNameLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetNameLogic {
	return &SetNameLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 设置昵称。
// 参考 service.SetName：写库→失效本地缓存→Outbox 通知（动作 updateUname）。
func (l *SetNameLogic) SetName(in *rpc.UpdateUnameReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetName(l.ctx, in.Mid, in.Name); err != nil {
		l.Errorf("user-profile/SetName: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
