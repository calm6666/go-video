package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListFollowerLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListFollowerLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListFollowerLogic {
	return &ListFollowerLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// mid 的粉丝列表（分页）。
func (l *ListFollowerLogic) ListFollower(in *rpc.ListReq) (*rpc.FollowerReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	rows, total, err := l.svcCtx.Repository.ListFollower(l.ctx, in.Mid, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("social-graph/ListFollower: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	items := make([]*rpc.RelationItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, &rpc.RelationItem{
			Mid:   r.Mid,
			Ctime: r.Ctime,
			Attr:  int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWER),
		})
	}
	return &rpc.FollowerReply{Total: total, Items: items}, nil
}
