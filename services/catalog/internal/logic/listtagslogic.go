package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListTagsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListTagsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListTagsLogic {
	return &ListTagsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListTags 标签查询：name 非空时按名字模糊匹配，否则按 tagids 列表查询。
// tagids 最多 100 个；ps 最大 50。
func (l *ListTagsLogic) ListTags(in *rpc.ListTagsReq) (*rpc.TagsReply, error) {
	if in.Ps > 50 {
		return nil, model.ErrInvalidPs
	}
	var (
		tags []*model.Tag
		err  error
	)
	switch {
	case in.Name != "":
		tags, err = l.svcCtx.Repository.SearchTags(l.ctx, in.Name, in.Pn, in.Ps)
	case len(in.Tagids) > 0:
		if len(in.Tagids) > 100 {
			return nil, model.ErrTooManyTagIDs
		}
		tags, err = l.svcCtx.Repository.FindTagsByIDs(l.ctx, in.Tagids)
	default:
		return &rpc.TagsReply{Tags: []*rpc.TagReply{}}, nil
	}
	if err != nil {
		l.Errorf("catalog/ListTags: name=%q tagids=%d err=%v", in.Name, len(in.Tagids), err)
		return nil, err
	}
	out := make([]*rpc.TagReply, 0, len(tags))
	for _, t := range tags {
		out = append(out, &rpc.TagReply{
			Tagid: t.TagID,
			Name:  t.Name,
		})
	}
	return &rpc.TagsReply{Tags: out}, nil
}
