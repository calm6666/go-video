package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Card3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCard3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Card3Logic {
	return &Card3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询单个用户名片。
// 参考 service.Card：缓存 miss 时由 repository 回源并异步回填。
// card 为 nil 时返回零值 Card（mid 透传）。
func (l *Card3Logic) Card3(in *rpc.MidReq) (*rpc.CardReply, error) {
	card, err := l.svcCtx.Repository.Card(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("account/Card3: repository.Card mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if card == nil {
		card = &rpc.Card{Mid: in.Mid}
	}
	return &rpc.CardReply{Card: card}, nil
}
