package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListWorksLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListWorksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListWorksLogic {
	return &ListWorksLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListWorks 分页查询作品。typeid=0 不过滤类型，state=-1 不过滤状态。
func (l *ListWorksLogic) ListWorks(in *rpc.ListReq) (*rpc.WorksReply, error) {
	if in.Ps > 50 {
		return nil, model.ErrInvalidPs
	}
	works, total, err := l.svcCtx.Repository.ListWorks(l.ctx, in.Typeid, in.State, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("catalog/ListWorks: typeid=%d state=%d err=%v", in.Typeid, in.State, err)
		return nil, err
	}
	out := make([]*rpc.WorkReply, 0, len(works))
	for _, w := range works {
		out = append(out, &rpc.WorkReply{
			SeasonId: w.SeasonID,
			Title:    w.Title,
			Cover:    w.Cover,
			Typeid:   w.TypeID,
			Intro:    w.Intro,
			State:    w.State,
		})
	}
	return &rpc.WorksReply{Total: total, Works: out}, nil
}
