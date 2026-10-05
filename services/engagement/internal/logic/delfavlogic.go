package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DelFavLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDelFavLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelFavLogic {
	return &DelFavLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DelFav 取消收藏。
func (l *DelFavLogic) DelFav(in *rpc.DelFavReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Oid <= 0 {
		return nil, model.ErrInvalidOid
	}
	if err := l.svcCtx.Repository.DelFav(l.ctx, in.Mid, in.Oid, in.Tp, in.Fid); err != nil {
		l.Errorf("engagement/DelFav: mid=%d oid=%d fid=%d err=%v", in.Mid, in.Oid, in.Fid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
