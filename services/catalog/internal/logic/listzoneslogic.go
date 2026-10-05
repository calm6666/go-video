package logic

import (
	"context"

	"go-video/services/catalog/internal/svc"
	"go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListZonesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListZonesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListZonesLogic {
	return &ListZonesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListZones 查询分区树（返回扁平列表，由调用方按 parent 组装树）。
// 短 TTL Redis 缓存，降低分区树读取延迟。
func (l *ListZonesLogic) ListZones(in *rpc.EmptyReq) (*rpc.ZonesReply, error) {
	zones, err := l.svcCtx.Repository.ListZones(l.ctx)
	if err != nil {
		l.Errorf("catalog/ListZones: err=%v", err)
		return nil, err
	}
	out := make([]*rpc.ZoneReply, 0, len(zones))
	for _, z := range zones {
		out = append(out, &rpc.ZoneReply{
			Zoneid: z.ZoneID,
			Name:   z.Name,
			Parent: z.Parent,
		})
	}
	return &rpc.ZonesReply{Zones: out}, nil
}
