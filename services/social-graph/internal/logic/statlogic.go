package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type StatLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StatLogic {
	return &StatLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询关注数与粉丝数。
func (l *StatLogic) Stat(in *rpc.MidReq) (*rpc.StatReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	following, follower, err := l.svcCtx.Repository.Stat(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("social-graph/Stat: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.StatReply{Following: following, Follower: follower, Whisper: 0}, nil
}
