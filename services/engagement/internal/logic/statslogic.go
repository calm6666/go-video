package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type StatsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StatsLogic {
	return &StatsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// stateToRPC 把 model.ThumbupLike.State (0/1/2) 映射到 rpc.LikeState。
func stateToRPC(s int32) rpc.LikeState {
	switch s {
	case 1:
		return rpc.LikeState_STATE_LIKE
	case 2:
		return rpc.LikeState_STATE_DISLIKE
	default:
		return rpc.LikeState_STATE_UNSPECIFIED
	}
}

// Stats 批量查询计数 + 当前用户点赞状态。
func (l *StatsLogic) Stats(in *rpc.StatsReq) (*rpc.StatsReply, error) {
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}
	if len(in.MessageIds) == 0 {
		return &rpc.StatsReply{Stats: map[int64]*rpc.StatState{}}, nil
	}
	if len(in.MessageIds) > 100 {
		return nil, model.ErrTooManyMessageIDs
	}
	stats, states, err := l.svcCtx.Repository.Stats(l.ctx, in.Business, in.OriginId, in.MessageIds, in.Mid)
	if err != nil {
		l.Errorf("engagement/Stats: business=%s origin=%d mid=%d err=%v", in.Business, in.OriginId, in.Mid, err)
		return nil, err
	}
	out := make(map[int64]*rpc.StatState, len(stats))
	for id, s := range stats {
		item := &rpc.StatState{
			OriginId:      in.OriginId,
			MessageId:     id,
			LikeNumber:    0,
			DislikeNumber: 0,
		}
		if s != nil {
			item.LikeNumber = s.LikeNumber
			item.DislikeNumber = s.DislikeNumber
		}
		if states != nil {
			if st, ok := states[id]; ok && st != nil {
				item.LikeState = stateToRPC(st.State)
			}
		}
		out[id] = item
	}
	return &rpc.StatsReply{Stats: out}, nil
}
