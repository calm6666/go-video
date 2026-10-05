package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListBlacksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListBlacksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListBlacksLogic {
	return &ListBlacksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// mid 的黑名单列表（分页）。
func (l *ListBlacksLogic) ListBlacks(in *rpc.ListReq) (*rpc.BlacksReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	rows, total, err := l.svcCtx.Repository.ListBlacks(l.ctx, in.Mid, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("social-graph/ListBlacks: mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	items := make([]*rpc.RelationItem, 0, len(rows))
	for _, r := range rows {
		items = append(items, &rpc.RelationItem{
			Mid:   r.BlackMid,
			Ctime: r.Ctime,
			Attr:  int32(rpc.RelationAttr_RELATION_ATTR_BLACKED),
		})
	}
	return &rpc.BlacksReply{Total: total, Items: items}, nil
}
