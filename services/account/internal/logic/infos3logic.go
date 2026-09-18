package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Infos3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewInfos3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Infos3Logic {
	return &Infos3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量查询用户基础信息。
// 参考 service.Infos：repository 内部按缓存命中分组回源并异步回填。
// 空 mids 返回空 map；缺失的 mid 不在结果中，由调用方按需补默认值。
func (l *Infos3Logic) Infos3(in *rpc.MidsReq) (*rpc.InfosReply, error) {
	infos, err := l.svcCtx.Repository.Infos(l.ctx, in.Mids)
	if err != nil {
		l.Errorf("account/Infos3: repository.Infos err=%v", err)
		return nil, err
	}
	if infos == nil {
		infos = map[int64]*rpc.Info{}
	}
	return &rpc.InfosReply{Infos: infos}, nil
}
