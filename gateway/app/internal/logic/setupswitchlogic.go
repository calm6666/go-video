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

type SetUpSwitchLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 设置关注弹窗开关
func NewSetUpSwitchLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetUpSwitchLogic {
	return &SetUpSwitchLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// SetUpSwitch 聚合 creator SetUpSwitch RPC：按业务来源覆盖本人开关状态。
// 幂等由 creator 服务以 (mid, from) 唯一键保证；state 取值合法性不在网关判定（AGENTS.md §5）。
func (l *SetUpSwitchLogic) SetUpSwitch(req *types.ParamSetUpSwitch) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}
	if _, err = l.svcCtx.Creator.SetUpSwitch(l.ctx, &creatorrpc.UpSwitchReq{
		Mid:   req.Mid,
		From:  req.From,
		State: req.State,
	}); err != nil {
		l.Errorf("gateway/app/setUpSwitch: mid=%d from=%d state=%d err=%v", req.Mid, req.From, req.State, err)
		return nil, err
	}
	return emptyResponse(), nil
}
