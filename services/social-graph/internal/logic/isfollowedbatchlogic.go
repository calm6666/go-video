package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IsFollowedBatchLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIsFollowedBatchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsFollowedBatchLogic {
	return &IsFollowedBatchLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量查询 mid 是否关注 owners。
func (l *IsFollowedBatchLogic) IsFollowedBatch(in *rpc.RelationsReq) (*rpc.RelationsReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if len(in.Owners) == 0 {
		return &rpc.RelationsReply{Following: map[int64]bool{}}, nil
	}
	if len(in.Owners) > 100 {
		return nil, model.ErrTooManyOwners
	}
	out, err := l.svcCtx.Repository.IsFollowingBatch(l.ctx, in.Mid, in.Owners)
	if err != nil {
		l.Errorf("social-graph/IsFollowedBatch: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.RelationsReply{Following: out}, nil
}
