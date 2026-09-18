package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Relations3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRelations3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Relations3Logic {
	return &Relations3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量查询关注关系。
// 参考 service.Relations：透传 social-graph 查询 mid 与 owners 的关注关系。
// 返回的 map 按 owners 顺序补齐默认值，保证每个 owner 都有条目。
func (l *Relations3Logic) Relations3(in *rpc.RelationsReq) (*rpc.RelationsReply, error) {
	reply, err := l.svcCtx.Repository.Relations(l.ctx, in.Mid, in.Owners)
	if err != nil {
		l.Errorf("account/Relations3: repository.Relations mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if reply == nil {
		reply = &rpc.RelationsReply{}
	}
	if reply.Relations == nil {
		reply.Relations = map[int64]*rpc.RelationReply{}
	}
	return reply, nil
}
