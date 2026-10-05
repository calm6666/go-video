package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DelBlackLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDelBlackLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelBlackLogic {
	return &DelBlackLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取消拉黑（幂等）。
func (l *DelBlackLogic) DelBlack(in *rpc.BlackReq) (*rpc.EmptyReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.BlackMid <= 0 {
		return nil, model.ErrInvalidBlackMid
	}
	if _, err := l.svcCtx.Repository.DelBlack(l.ctx, in.Mid, in.BlackMid); err != nil {
		l.Errorf("social-graph/DelBlack: mid=%d black=%d err=%v", in.Mid, in.BlackMid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
