package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MoralLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewMoralLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MoralLogic {
	return &MoralLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询节操值。
// 参考 service.Moral：缓存→DB，不存在时按默认值（7000）返回。
func (l *MoralLogic) Moral(in *rpc.MemberMidReq) (*rpc.MoralReply, error) {
	reply, err := l.svcCtx.Repository.Moral(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/Moral: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return reply, nil
}
