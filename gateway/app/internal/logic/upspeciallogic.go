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

type UpSpecialLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单个 UP 主特殊属性
func NewUpSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpSpecialLogic {
	return &UpSpecialLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpSpecial 聚合 creator UpSpecial RPC：单个 UP 主的特殊用户组 ID 列表。
// 分组字典（组名、颜色）属运营面，只在 gateway/admin 暴露，本路由只回 ID。
func (l *UpSpecialLogic) UpSpecial(req *types.ParamUpMid) (resp *types.UpSpecialResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}
	reply, err := l.svcCtx.Creator.UpSpecial(l.ctx, &creatorrpc.UpSpecialReq{
		Mid: req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/upSpecial: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.UpSpecialResponse{
		Code:    0,
		Message: "ok",
		Data: types.UpSpecialData{
			UpSpecial: upSpecialToAPI(reply.GetUpSpecial()),
		},
		TTL: 0,
	}, nil
}
