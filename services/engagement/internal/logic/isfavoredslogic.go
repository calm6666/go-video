package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IsFavoredsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIsFavoredsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsFavoredsLogic {
	return &IsFavoredsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// IsFavoreds 批量查询是否已收藏。
func (l *IsFavoredsLogic) IsFavoreds(in *rpc.IsFavoredsReq) (*rpc.IsFavoredsReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if len(in.Oids) == 0 {
		return &rpc.IsFavoredsReply{Faveds: map[int64]bool{}}, nil
	}
	if len(in.Oids) > 100 {
		return nil, model.ErrTooManyMessageIDs
	}
	faved, err := l.svcCtx.Repository.IsFavoreds(l.ctx, in.Mid, in.Oids, in.Tp)
	if err != nil {
		l.Errorf("engagement/IsFavoreds: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.IsFavoredsReply{Faveds: faved}, nil
}
