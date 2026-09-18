package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Cards3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCards3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Cards3Logic {
	return &Cards3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量查询用户名片。
// 参考 service.Cards：repository 内部按缓存命中分组回源并异步回填。
func (l *Cards3Logic) Cards3(in *rpc.MidsReq) (*rpc.CardsReply, error) {
	cards, err := l.svcCtx.Repository.Cards(l.ctx, in.Mids)
	if err != nil {
		l.Errorf("account/Cards3: repository.Cards err=%v", err)
		return nil, err
	}
	if cards == nil {
		cards = map[int64]*rpc.Card{}
	}
	return &rpc.CardsReply{Cards: cards}, nil
}
