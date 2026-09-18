// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MoralUndoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 撤销节操值变更
func NewMoralUndoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MoralUndoLogic {
	return &MoralUndoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 撤销节操值变更：调用 user-profile UndoMoral RPC。
func (l *MoralUndoLogic) MoralUndo(req *types.ParamUndo) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.UndoMoral(l.ctx, &userprofilerc.UndoMoralReq{
		LogId:    req.LogID,
		Remark:   req.Remark,
		Operator: req.Operator,
	}); err != nil {
		l.Errorf("gateway/admin/moralUndo: log_id=%s err=%v", req.LogID, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
