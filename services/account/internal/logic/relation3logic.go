package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Relation3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRelation3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Relation3Logic {
	return &Relation3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询关注关系。
// 参考 service.Relation：透传 social-graph 查询 mid 是否关注 owner。
// 当 social-graph 尚未接入时返回默认 false，不报错。
func (l *Relation3Logic) Relation3(in *rpc.RelationReq) (*rpc.RelationReply, error) {
	reply, err := l.svcCtx.Repository.Relation(l.ctx, in.Mid, in.Owner)
	if err != nil {
		l.Errorf("account/Relation3: repository.Relation mid=%d owner=%d err=%v", in.Mid, in.Owner, err)
		return nil, err
	}
	if reply == nil {
		reply = &rpc.RelationReply{}
	}
	return reply, nil
}
