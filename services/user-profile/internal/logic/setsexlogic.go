package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetSexLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetSexLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetSexLogic {
	return &SetSexLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 设置性别。
// 参考 service.SetSex：写库→失效本地缓存→Outbox 通知 account 失效缓存。
func (l *SetSexLogic) SetSex(in *rpc.UpdateSexReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetSex(l.ctx, in.Mid, in.Sex); err != nil {
		l.Errorf("user-profile/SetSex: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
