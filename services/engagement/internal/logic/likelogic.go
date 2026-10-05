package logic

import (
	"context"

	"go-video/services/engagement/internal/svc"
	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LikeLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLikeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LikeLogic {
	return &LikeLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// actionToState 把 rpc.Action 转换为 thumbup_like 的 state 值。
// 0=取消, 1=点赞, 2=点踩
func actionToState(a rpc.Action) int32 {
	switch a {
	case rpc.Action_ACTION_LIKE:
		return 1
	case rpc.Action_ACTION_DISLIKE:
		return 2
	case rpc.Action_ACTION_CANCEL_LIKE, rpc.Action_ACTION_CANCEL_DISLIKE:
		return 0
	default:
		return 0
	}
}

// Like 点赞/取消点赞/点踩。
func (l *LikeLogic) Like(in *rpc.LikeReq) (*rpc.LikeReply, error) {
	if in.Business == "" {
		return nil, model.ErrInvalidBusiness
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.MessageId <= 0 {
		return nil, model.ErrInvalidMessage
	}
	like := &model.ThumbupLike{
		Business:  in.Business,
		Mid:       in.Mid,
		UpMid:     in.UpMid,
		OriginID:  in.OriginId,
		MessageID: in.MessageId,
		State:     actionToState(in.Action),
	}
	likeNum, dislikeNum, err := l.svcCtx.Repository.Like(l.ctx, like)
	if err != nil {
		l.Errorf("engagement/Like: business=%s mid=%d msg=%d action=%v err=%v",
			in.Business, in.Mid, in.MessageId, in.Action, err)
		return nil, err
	}
	return &rpc.LikeReply{
		OriginId:      in.OriginId,
		MessageId:     in.MessageId,
		LikeNumber:    likeNum,
		DislikeNumber: dislikeNum,
	}, nil
}
