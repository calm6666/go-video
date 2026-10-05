package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSeasonsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSeasonsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSeasonsLogic {
	return &ListSeasonsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListSeasons 查询某作品的全部季。
// SeasonReq.season_id 在此 RPC 中语义为作品主季 ID（父作品）。
func (l *ListSeasonsLogic) ListSeasons(in *rpc.SeasonReq) (*rpc.SeasonsReply, error) {
	if in.SeasonId <= 0 {
		return nil, model.ErrInvalidWorkID
	}
	seasons, err := l.svcCtx.Repository.ListSeasons(l.ctx, in.SeasonId)
	if err != nil {
		l.Errorf("catalog/ListSeasons: work_id=%d err=%v", in.SeasonId, err)
		return nil, err
	}
	out := make([]*rpc.SeasonReply, 0, len(seasons))
	for _, s := range seasons {
		out = append(out, &rpc.SeasonReply{
			SeasonId: s.SeasonID,
			SeasonNo: s.SeasonNo,
			Title:    s.Title,
			Cover:    s.Cover,
			State:    s.State,
		})
	}
	return &rpc.SeasonsReply{Seasons: out}, nil
}
