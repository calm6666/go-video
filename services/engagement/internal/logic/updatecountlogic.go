package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateCountLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateCountLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateCountLogic {
	return &UpdateCountLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UpdateCount 运营修正计数增量（仅运营后台调用）。
func (l *UpdateCountLogic) UpdateCount(in *rpc.UpdateCountReq) (*rpc.EmptyReply, error) {
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}
	if in.MessageId <= 0 {
		return nil, model.ErrInvalidMessage
	}
	if err := l.svcCtx.Repository.UpdateCount(l.ctx, in.Business, in.OriginId, in.MessageId, in.LikeChange, in.DislikeChange); err != nil {
		l.Errorf("engagement/UpdateCount: business=%s msg=%d err=%v", in.Business, in.MessageId, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
