package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetFaceLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetFaceLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetFaceLogic {
	return &SetFaceLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 设置头像。
// 参考 service.SetFace：写库→失效本地缓存→Outbox 通知（动作 updateFace）。
func (l *SetFaceLogic) SetFace(in *rpc.UpdateFaceReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetFace(l.ctx, in.Mid, in.Face); err != nil {
		l.Errorf("user-profile/SetFace: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
