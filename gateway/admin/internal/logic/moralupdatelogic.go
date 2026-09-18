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

type MoralUpdateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 变更单个用户节操值
func NewMoralUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MoralUpdateLogic {
	return &MoralUpdateLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 变更单个用户节操值：调用 user-profile AddMoral RPC。
func (l *MoralUpdateLogic) MoralUpdate(req *types.ParamUpdateMoral) (resp *types.MoralsUpdateResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.AddMoral(l.ctx, &userprofilerc.UpdateMoralReq{
		Mid:        req.Mid,
		Delta:      req.Delta,
		Origin:     req.Origin,
		Reason:     req.Reason,
		ReasonType: req.ReasonType,
		Operator:   req.Operator,
		Remark:     req.Remark,
		Status:     req.Status,
		IsNotify:   req.IsNotify,
		Ip:         req.IP,
	}); err != nil {
		l.Errorf("gateway/admin/moralUpdate: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.MoralsUpdateResponse{
		Code:    0,
		Message: "ok",
		Data:    types.MoralsUpdateData{AfterMorals: map[int64]int64{}},
		TTL:     0,
	}, nil
}
