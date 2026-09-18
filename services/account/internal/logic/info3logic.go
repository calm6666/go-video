package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Info3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewInfo3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Info3Logic {
	return &Info3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个用户基础信息。
// 参考 service.Info：缓存 miss 时由 repository 回源 user-profile 并异步回填。
// info 为 nil 时返回零值 Info（mid 透传），保持调用方语义兼容。
func (l *Info3Logic) Info3(in *rpc.MidReq) (*rpc.InfoReply, error) {
	info, err := l.svcCtx.Repository.Info(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("account/Info3: repository.Info mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if info == nil {
		info = &rpc.Info{Mid: in.Mid}
	}
	return &rpc.InfoReply{Info: info}, nil
}
