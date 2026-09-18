// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MemberBatchBaseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量查询用户基础资料
func NewMemberBatchBaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MemberBatchBaseLogic {
	return &MemberBatchBaseLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 批量查询用户基础资料：聚合 user-profile Bases RPC。
func (l *MemberBatchBaseLogic) MemberBatchBase(req *types.ParamMids) (resp *types.BatchBaseResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.Bases(l.ctx, &userprofilerc.MemberMidsReq{Mids: req.Mids})
	if err != nil {
		l.Errorf("gateway/app/memberBatchBase: err=%v", err)
		return nil, err
	}
	return &types.BatchBaseResponse{
		Code:    0,
		Message: "ok",
		Data:    types.BatchBaseData{BaseInfos: toMemberBaseMap(reply.GetBaseInfos())},
		TTL:     0,
	}, nil
}
