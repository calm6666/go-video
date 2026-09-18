package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DelCacheLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDelCacheLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelCacheLogic {
	return &DelCacheLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 失效指定用户的缓存。
// 服务间调用入口（对应 HTTP /cache/del 的 RPC 化，供 user-profile 等
// 资料变更方调用）：删除 Info/Card/Profile/Vip 缓存并异步回温；
// action=updateVip 时额外入延迟队列，5 秒后二次失效。
func (l *DelCacheLogic) DelCache(in *rpc.DelCacheReq) (*rpc.DelCacheReply, error) {
	errs := l.svcCtx.Repository.DelCache(l.ctx, in.Mid, in.Action)
	for _, e := range errs {
		l.Errorf("account/DelCache: mid=%d err=%v", in.Mid, e)
	}
	return &rpc.DelCacheReply{}, nil
}
