package logic

import (
	"context"

	"go-video/services/user-profile/internal/svc"
	"go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SetRankLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetRankLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetRankLogic {
	return &SetRankLogic{ctx: ctx, svcCtx: svcCtx, Logger: logx.WithContext(ctx)}
}

// 设置排名。
// 参考 service.SetRank：写库→失效本地缓存→Outbox 通知。
func (l *SetRankLogic) SetRank(in *rpc.UpdateRankReq) (*rpc.EmptyReply, error) {
	if err := l.svcCtx.Repository.SetRank(l.ctx, in.Mid, in.Rank); err != nil {
		l.Errorf("user-profile/SetRank: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
