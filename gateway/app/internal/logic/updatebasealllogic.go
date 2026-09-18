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

type UpdateBaseAllLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 整体更新用户基础资料
func NewUpdateBaseAllLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateBaseAllLogic {
	return &UpdateBaseAllLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 整体更新用户基础资料：经 user-profile SetName/SetSex/SetFace/SetBirthday/SetSign
// 逐字段聚合（参考仓库 /x/member/web/update 语义，服务端整体更新走多个字段 RPC）。
func (l *UpdateBaseAllLogic) UpdateBaseAll(req *types.ParamBaseAll) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if req.Name != "" {
		if _, err = l.svcCtx.UserProfile.SetName(l.ctx, &userprofilerc.UpdateUnameReq{Mid: req.Mid, Name: req.Name}); err != nil {
			return nil, err
		}
	}
	if req.Sex != 0 {
		if _, err = l.svcCtx.UserProfile.SetSex(l.ctx, &userprofilerc.UpdateSexReq{Mid: req.Mid, Sex: req.Sex}); err != nil {
			return nil, err
		}
	}
	if req.UserSign != "" {
		if _, err = l.svcCtx.UserProfile.SetSign(l.ctx, &userprofilerc.UpdateSignReq{Mid: req.Mid, Sign: req.UserSign}); err != nil {
			return nil, err
		}
	}
	if req.Birthday != 0 {
		if _, err = l.svcCtx.UserProfile.SetBirthday(l.ctx, &userprofilerc.UpdateBirthdayReq{Mid: req.Mid, Birthday: req.Birthday}); err != nil {
			return nil, err
		}
	}
	if err != nil {
		l.Errorf("gateway/app/updateBaseAll: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
