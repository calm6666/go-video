package logic

import (
	"context"

	"go-video/services/social-graph/internal/svc"
	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RichRelationsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRichRelationsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RichRelationsLogic {
	return &RichRelationsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量查询 owner 与 mids 的全部关系位（双向关注 + 拉黑 + 特别关注）
func (l *RichRelationsLogic) RichRelations(in *rpc.RichRelationsReq) (*rpc.RichRelationsReply, error) {
	if in.Owner <= 0 {
		return nil, model.ErrInvalidOwnerMid
	}
	if len(in.Mids) == 0 {
		return &rpc.RichRelationsReply{Attrs: map[int64]int32{}}, nil
	}
	if len(in.Mids) > 100 {
		return nil, model.ErrTooManyMids
	}
	attrs, err := l.svcCtx.Repository.RichRelations(l.ctx, in.Owner, in.Mids)
	if err != nil {
		l.Errorf("social-graph/RichRelations: owner=%d mids=%d err=%v", in.Owner, len(in.Mids), err)
		return nil, err
	}
	if attrs == nil {
		attrs = map[int64]int32{}
	}
	return &rpc.RichRelationsReply{Attrs: attrs}, nil
}
