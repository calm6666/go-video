package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddBlackLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddBlackLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddBlackLogic {
	return &AddBlackLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 拉黑（自动取关）。
func (l *AddBlackLogic) AddBlack(in *rpc.BlackReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.BlackMid <= 0 {
		return nil, model.ErrInvalidBlackMid
	}
	if in.Mid == in.BlackMid {
		return nil, model.ErrSelfAction
	}
	if _, _, err := l.svcCtx.Repository.AddBlack(l.ctx, in.Mid, in.BlackMid); err != nil {
		l.Errorf("social-graph/AddBlack: mid=%d black=%d err=%v", in.Mid, in.BlackMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
