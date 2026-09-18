package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetBirthdayLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetBirthdayLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetBirthdayLogic {
	return &SetBirthdayLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 设置生日。
// 参考 service.SetBirthday：写库→失效本地缓存→Outbox 通知。
func (l *SetBirthdayLogic) SetBirthday(in *rpc.UpdateBirthdayReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetBirthday(l.ctx, in.Mid, in.Birthday); err != nil {
		l.Errorf("user-profile/SetBirthday: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
