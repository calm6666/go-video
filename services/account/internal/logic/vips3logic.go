package logic

import (
	"context"

	"go-video/services/account/internal/svc"
	"go-video/services/account/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type Vips3Logic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVips3Logic(ctx context.Context, svcCtx *svc.ServiceContext) *Vips3Logic {
	return &Vips3Logic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量查询会员信息。
// 参考 service.Vips：透传 repository.Vips，repository 内部按缓存命中分组回源并异步回填。
// 依据 AGENTS.md §1 商业化范围外约束，会员业务不在本期实现，全部返回零值。
func (l *Vips3Logic) Vips3(in *rpc.MidsReq) (*rpc.VipsReply, error) {
	vips, err := l.svcCtx.Repository.Vips(l.ctx, in.Mids)
	if err != nil {
		l.Errorf("account/Vips3: repository.Vips err=%v", err)
		return nil, err
	}
	result := make(map[int64]*rpc.VipReply, len(vips))
	for mid, v := range vips {
		if v == nil {
			result[mid] = &rpc.VipReply{}
			continue
		}
		result[mid] = &rpc.VipReply{
			Type:       v.Type,
			Status:     v.Status,
			DueDate:    v.DueDate,
			VipPayType: v.VipPayType,
		}
	}
	return &rpc.VipsReply{Vips: result}, nil
}
