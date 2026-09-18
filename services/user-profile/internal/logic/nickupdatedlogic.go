package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type NickUpdatedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewNickUpdatedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *NickUpdatedLogic {
	return &NickUpdatedLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 查询用户是否修改过昵称。
// 参考 service.NickUpdated：user_flag 表 NickUpdated 标志位。
func (l *NickUpdatedLogic) NickUpdated(in *rpc.MemberMidReq) (*rpc.NickUpdatedReply, error) {
	updated, err := l.svcCtx.Repository.NickUpdated(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("user-profile/NickUpdated: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.NickUpdatedReply{NickUpdated: updated}, nil
}
