package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetSignLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetSignLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetSignLogic {
	return &SetSignLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 设置签名。
// 参考 service.SetSign：写库→失效本地缓存→Outbox 通知。
func (l *SetSignLogic) SetSign(in *rpc.UpdateSignReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetSign(l.ctx, in.Mid, in.Sign); err != nil {
		l.Errorf("user-profile/SetSign: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
