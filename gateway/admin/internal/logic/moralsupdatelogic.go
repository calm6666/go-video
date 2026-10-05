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

type MoralsUpdateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量变更节操值
func NewMoralsUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MoralsUpdateLogic {
	return &MoralsUpdateLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 批量变更节操值：调用 user-profile BatchAddMoral RPC，返回各用户变更后节操值。
func (l *MoralsUpdateLogic) MoralsUpdate(req *types.ParamUpdateMorals) (resp *types.MoralsUpdateResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if err := adminActorGate(l.ctx, "moralsUpdate", "operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.UserProfile.BatchAddMoral(l.ctx, &userprofilerc.UpdateMoralsReq{
		Mids:       req.Mids,
		Delta:      req.Delta,
		Origin:     req.Origin,
		Reason:     req.Reason,
		ReasonType: req.ReasonType,
		Operator:   req.Operator,
		Remark:     req.Remark,
		Status:     req.Status,
		IsNotify:   req.IsNotify,
		Ip:         req.IP,
	})
	if err != nil {
		l.Errorf("gateway/admin/moralsUpdate: err=%v", err)
		return nil, err
	}
	return &types.MoralsUpdateResponse{
		Code:    0,
		Message: "ok",
		Data:    types.MoralsUpdateData{AfterMorals: reply.GetAfterMorals()},
		TTL:     0,
	}, nil
}
