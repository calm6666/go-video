package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RichRelations3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRichRelations3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *RichRelations3Logic {
	return &RichRelations3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询富关系。
// 参考 service.RichRelations2：透传 social-graph 查询 owner 与 mids 的关系属性值。
// 返回的 map 按 mids 顺序补齐默认 0，保证每个 mid 都有条目。
func (l *RichRelations3Logic) RichRelations3(in *rpc.RichRelationReq) (*rpc.RichRelationsReply, error) {
	reply, err := l.svcCtx.Repository.RichRelations(l.ctx, in.Owner, in.Mids)
	if err != nil {
		l.Errorf("account/RichRelations3: repository.RichRelations owner=%d err=%v", in.Owner, err)
		return nil, err
	}
	if reply == nil {
		reply = &rpc.RichRelationsReply{}
	}
	if reply.RichRelations == nil {
		reply.RichRelations = map[int64]int32{}
	}
	return reply, nil
}
