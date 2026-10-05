// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	creatorrpc "go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsSpecialLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量查询 UP 主特殊属性（最多 100）
func NewUpsSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsSpecialLogic {
	return &UpsSpecialLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// creatorUpsSpecialMaxMids 与 creator.proto 的 UpsSpecialReq 上限（obc validate max=100）一致，
// 网关提前拒绝可避免把超量批次放大成一次下游全表查询。
const creatorUpsSpecialMaxMids = 100

// UpsSpecial 聚合 creator UpsSpecial RPC：批量 UP 主特殊属性，返回 mid → 特殊属性 map。
// 未命中特殊属性的 mid 由 creator 服务省略，网关不补零值。
func (l *UpsSpecialLogic) UpsSpecial(req *types.ParamUpMids) (resp *types.UpsSpecialResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}
	if len(req.Mids) > creatorUpsSpecialMaxMids {
		return nil, errors.New("creator: mids exceeds 100")
	}
	reply, err := l.svcCtx.Creator.UpsSpecial(l.ctx, &creatorrpc.UpsSpecialReq{
		Mids: req.Mids,
	})
	if err != nil {
		l.Errorf("gateway/app/upsSpecial: count=%d err=%v", len(req.Mids), err)
		return nil, err
	}
	return &types.UpsSpecialResponse{
		Code:    0,
		Message: "ok",
		Data: types.UpsSpecialData{
			UpSpecials: upsSpecialToAPI(reply.GetUpSpecials()),
		},
		TTL: 0,
	}, nil
}
