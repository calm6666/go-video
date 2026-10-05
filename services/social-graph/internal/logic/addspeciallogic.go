package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddSpecialLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddSpecialLogic {
	return &AddSpecialLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 特别关注（必先关注）。
func (l *AddSpecialLogic) AddSpecial(in *rpc.SpecialReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.SpecialMid <= 0 {
		return nil, model.ErrInvalidSpecialMid
	}
	if in.Mid == in.SpecialMid {
		return nil, model.ErrSelfAction
	}
	if _, err := l.svcCtx.Repository.AddSpecial(l.ctx, in.Mid, in.SpecialMid); err != nil {
		l.Errorf("social-graph/AddSpecial: mid=%d special=%d err=%v", in.Mid, in.SpecialMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
