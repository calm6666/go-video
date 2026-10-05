package logic

import (
	"context"
	"time"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddFavLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddFavLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddFavLogic {
	return &AddFavLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddFav 添加收藏到指定收藏夹。
func (l *AddFavLogic) AddFav(in *rpc.AddFavReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Oid <= 0 {
		return nil, model.ErrInvalidOid
	}
	if in.Fid <= 0 {
		return nil, model.ErrInvalidFid
	}
	now := time.Now().Unix()
	f := &model.FavoriteItem{
		Oid:   in.Oid,
		Mid:   in.Mid,
		Fid:   in.Fid,
		Tp:    in.Tp,
		Otype: in.Otype,
		Ctime: now,
		Mtime: now,
	}
	if err := l.svcCtx.Repository.AddFav(l.ctx, f); err != nil {
		l.Errorf("engagement/AddFav: mid=%d oid=%d fid=%d err=%v", in.Mid, in.Oid, in.Fid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
