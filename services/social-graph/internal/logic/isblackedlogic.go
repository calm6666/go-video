package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type IsBlackedLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewIsBlackedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *IsBlackedLogic {
	return &IsBlackedLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询 mid 是否拉黑 owner。
func (l *IsBlackedLogic) IsBlacked(in *rpc.RelationReq) (*rpc.RelationReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Owner <= 0 {
		return nil, model.ErrInvalidOwnerMid
	}
	ok, err := l.svcCtx.Repository.IsBlacked(l.ctx, in.Mid, in.Owner)
	if err != nil {
		l.Errorf("social-graph/IsBlacked: mid=%d owner=%d err=%v", in.Mid, in.Owner, err)
		return nil, err
	}
	return &rpc.RelationReply{Following: ok}, nil
}
