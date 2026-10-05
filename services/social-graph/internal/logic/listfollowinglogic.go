package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListFollowingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListFollowingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListFollowingLogic {
	return &ListFollowingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// mid 的关注列表（分页）。
func (l *ListFollowingLogic) ListFollowing(in *rpc.ListReq) (*rpc.FollowingReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	rows, total, err := l.svcCtx.Repository.ListFollowing(l.ctx, in.Mid, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("social-graph/ListFollowing: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	items := make([]*rpc.RelationItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, &rpc.RelationItem{
			Mid:   r.FollowerMid,
			Ctime: r.Ctime,
			Attr:  int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING),
		})
	}
	return &rpc.FollowingReply{Total: total, Items: items}, nil
}
