package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Blacks3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewBlacks3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Blacks3Logic {
	return &Blacks3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询黑名单。
// 参考 service.Blacks：透传 social-graph 查询 mid 的黑名单 mid 集合。
// 返回 black_list 字段始终为非 nil map。
func (l *Blacks3Logic) Blacks3(in *rpc.MidReq) (*rpc.BlacksReply, error) {
	reply, err := l.svcCtx.Repository.Blacks(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("account/Blacks3: repository.Blacks mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if reply == nil {
		reply = &rpc.BlacksReply{}
	}
	if reply.BlackList == nil {
		reply.BlackList = map[int64]bool{}
	}
	return reply, nil
}
