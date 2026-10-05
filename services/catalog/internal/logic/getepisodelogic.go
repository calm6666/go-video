package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetEpisodeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetEpisodeLogic {
	return &GetEpisodeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetEpisode 查询集详情（先查 Redis 缓存，未命中查 DB 并回填）。
func (l *GetEpisodeLogic) GetEpisode(in *rpc.EpisodeReq) (*rpc.EpisodeReply, error) {
	if in.Epid <= 0 {
		return nil, model.ErrInvalidEpid
	}
	e, err := l.svcCtx.Repository.GetEpisode(l.ctx, in.Epid)
	if err != nil {
		l.Errorf("catalog/GetEpisode: epid=%d err=%v", in.Epid, err)
		return nil, err
	}
	if e == nil {
		return nil, model.ErrEpisodeNotFound
	}
	return &rpc.EpisodeReply{
		Epid:     e.Epid,
		SeasonId: e.SeasonID,
		EpNo:     e.EpNo,
		Title:    e.Title,
		AssetId:  e.AssetID,
		Duration: e.Duration,
		State:    e.State,
	}, nil
}
