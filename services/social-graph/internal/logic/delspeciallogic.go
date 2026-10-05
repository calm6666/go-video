package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DelSpecialLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDelSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelSpecialLogic {
	return &DelSpecialLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取消特别关注（幂等）。
func (l *DelSpecialLogic) DelSpecial(in *rpc.SpecialReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.SpecialMid <= 0 {
		return nil, model.ErrInvalidSpecialMid
	}
	if _, err := l.svcCtx.Repository.DelSpecial(l.ctx, in.Mid, in.SpecialMid); err != nil {
		l.Errorf("social-graph/DelSpecial: mid=%d special=%d err=%v", in.Mid, in.SpecialMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
