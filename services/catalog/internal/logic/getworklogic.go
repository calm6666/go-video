package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetWorkLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetWorkLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetWorkLogic {
	return &GetWorkLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetWork 查询作品（先查 Redis 缓存，未命中查 DB 并回填）。
func (l *GetWorkLogic) GetWork(in *rpc.WorkReq) (*rpc.WorkReply, error) {
	if in.SeasonId <= 0 {
		return nil, model.ErrInvalidSeasonID
	}
	w, err := l.svcCtx.Repository.GetWork(l.ctx, in.SeasonId)
	if err != nil {
		l.Errorf("catalog/GetWork: season_id=%d err=%v", in.SeasonId, err)
		return nil, err
	}
	if w == nil {
		return nil, model.ErrWorkNotFound
	}
	return &rpc.WorkReply{
		SeasonId: w.SeasonID,
		Title:    w.Title,
		Cover:    w.Cover,
		Typeid:   w.TypeID,
		Intro:    w.Intro,
		State:    w.State,
	}, nil
}
