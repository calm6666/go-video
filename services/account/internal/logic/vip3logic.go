package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Vip3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVip3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Vip3Logic {
	return &Vip3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询会员信息。
// 参考 service.Vip：透传 repository.Vip，repository 内部走缓存+回源。
// 依据 AGENTS.md §1 商业化范围外约束，会员业务不在本期实现，
// repository.RawVip 固定返回零值，缓存层只缓存零值结构。
func (l *Vip3Logic) Vip3(in *rpc.MidReq) (*rpc.VipReply, error) {
	vip, err := l.svcCtx.Repository.Vip(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("account/Vip3: repository.Vip mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	if vip == nil {
		return &rpc.VipReply{}, nil
	}
	return &rpc.VipReply{
		Type:       vip.Type,
		Status:     vip.Status,
		DueDate:    vip.DueDate,
		VipPayType: vip.VipPayType,
	}, nil
}
