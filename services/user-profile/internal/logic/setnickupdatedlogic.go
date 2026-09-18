package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetNickUpdatedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetNickUpdatedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetNickUpdatedLogic {
	return &SetNickUpdatedLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 标记用户已修改过昵称。
// 参考 service.SetNickUpdated：置 user_flag 表的 NickUpdated 标志位。
func (l *SetNickUpdatedLogic) SetNickUpdated(in *rpc.MemberMidReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetNickUpdated(l.ctx, in.Mid); err != nil {
		l.Errorf("user-profile/SetNickUpdated: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
