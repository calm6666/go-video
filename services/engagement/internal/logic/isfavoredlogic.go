package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IsFavoredLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIsFavoredLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsFavoredLogic {
	return &IsFavoredLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// IsFavored 查询是否已收藏某对象。
func (l *IsFavoredLogic) IsFavored(in *rpc.IsFavoredReq) (*rpc.IsFavoredReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Oid <= 0 {
		return nil, model.ErrInvalidOid
	}
	faved, err := l.svcCtx.Repository.IsFavored(l.ctx, in.Mid, in.Oid, in.Tp)
	if err != nil {
		l.Errorf("engagement/IsFavored: mid=%d oid=%d err=%v", in.Mid, in.Oid, err)
		return nil, err
	}
	return &rpc.IsFavoredReply{Faved: faved}, nil
}
