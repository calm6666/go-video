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

type GetUpSwitchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询关注弹窗开关
func NewGetUpSwitchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUpSwitchLogic {
	return &GetUpSwitchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// GetUpSwitch 聚合 creator UpSwitch RPC：按业务来源查询本人的关注弹窗/退订开关。
// 查询不传 state（UpSwitchReq.state 仅 SetUpSwitch 使用），缺省语义由 creator 服务决定。
func (l *GetUpSwitchLogic) GetUpSwitch(req *types.ParamUpSwitch) (resp *types.UpSwitchResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}
	reply, err := l.svcCtx.Creator.UpSwitch(l.ctx, &creatorrpc.UpSwitchReq{
		Mid:  req.Mid,
		From: req.From,
	})
	if err != nil {
		l.Errorf("gateway/app/getUpSwitch: mid=%d from=%d err=%v", req.Mid, req.From, err)
		return nil, err
	}
	return &types.UpSwitchResponse{
		Code:    0,
		Message: "ok",
		Data: types.UpSwitchData{
			State: reply.GetState(),
		},
		TTL: 0,
	}, nil
}
