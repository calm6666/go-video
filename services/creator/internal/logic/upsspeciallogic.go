package logic

import (
	"context"

	"go-video/services/creator/internal/svc"
	"go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsSpecialLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsSpecialLogic {
	return &UpsSpecialLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 批量查询 UP 主特殊属性。
// 参考 service.UpsSpecial：批量直接打 DB（不走缓存）。
func (l *UpsSpecialLogic) UpsSpecial(in *rpc.UpsSpecialReq) (*rpc.UpsSpecialReply, error) {
	if len(in.Mids) == 0 {
		return &rpc.UpsSpecialReply{UpSpecials: map[int64]*rpc.UpSpecial{}}, nil
	}
	if len(in.Mids) > 100 {
		// 与 obc request.proto 中 UpsSpecialReq 的 validate:"max=100" 对齐
		return nil, errTooManyMids
	}
	idMap, err := l.svcCtx.Repository.UpsSpecial(l.ctx, in.Mids)
	if err != nil {
		l.Errorf("creator/UpsSpecial: count=%d err=%v", len(in.Mids), err)
		return nil, err
	}
	out := make(map[int64]*rpc.UpSpecial, len(idMap))
	for mid, ids := range idMap {
		out[mid] = &rpc.UpSpecial{GroupIds: ids}
	}
	return &rpc.UpsSpecialReply{UpSpecials: out}, nil
}
