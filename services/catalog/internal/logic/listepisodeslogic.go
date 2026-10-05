package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListEpisodesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListEpisodesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListEpisodesLogic {
	return &ListEpisodesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListEpisodes 查询某季的全部集。
// SeasonReq.season_id 在此 RPC 中语义为具体季 ID。
func (l *ListEpisodesLogic) ListEpisodes(in *rpc.SeasonReq) (*rpc.EpisodesReply, error) {
	if in.SeasonId <= 0 {
		return nil, model.ErrInvalidSeasonID
	}
	episodes, err := l.svcCtx.Repository.ListEpisodes(l.ctx, in.SeasonId)
	if err != nil {
		l.Errorf("catalog/ListEpisodes: season_id=%d err=%v", in.SeasonId, err)
		return nil, err
	}
	out := make([]*rpc.EpisodeReply, 0, len(episodes))
	for _, e := range episodes {
		out = append(out, &rpc.EpisodeReply{
			Epid:     e.Epid,
			SeasonId: e.SeasonID,
			EpNo:     e.EpNo,
			Title:    e.Title,
			AssetId:  e.AssetID,
			Duration: e.Duration,
			State:    e.State,
		})
	}
	return &rpc.EpisodesReply{Episodes: out}, nil
}
