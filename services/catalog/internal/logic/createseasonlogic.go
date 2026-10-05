package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateSeasonLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateSeasonLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateSeasonLogic {
	return &CreateSeasonLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateSeason 运营创建季。season_id 入参为所属作品主季 ID（父作品）。
// 季初始状态为草稿。返回新建季自身的 season_id。
func (l *CreateSeasonLogic) CreateSeason(in *rpc.CreateSeasonReq) (*rpc.SeasonReply, error) {
	if in.SeasonId <= 0 {
		return nil, model.ErrInvalidWorkID
	}
	if in.SeasonNo <= 0 {
		return nil, model.ErrInvalidSeasonNo
	}
	s := &model.Season{
		WorkID:   in.SeasonId,
		SeasonNo: in.SeasonNo,
		Title:    in.Title,
		Cover:    in.Cover,
		State:    model.StateDraft,
	}
	id, err := l.svcCtx.Repository.CreateSeason(l.ctx, s)
	if err != nil {
		l.Errorf("catalog/CreateSeason: work_id=%d season_no=%d operator=%s err=%v",
			in.SeasonId, in.SeasonNo, in.Operator, err)
		return nil, err
	}
	return &rpc.SeasonReply{
		SeasonId: id,
		SeasonNo: s.SeasonNo,
		Title:    s.Title,
		Cover:    s.Cover,
		State:    s.State,
	}, nil
}
