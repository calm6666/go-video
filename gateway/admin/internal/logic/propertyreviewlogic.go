// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"encoding/json"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PropertyReviewLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 添加用户属性变更审核
func NewPropertyReviewLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PropertyReviewLogic {
	return &PropertyReviewLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 添加用户属性变更审核：调用 user-profile AddPropertyReview RPC。
// extra 非空时校验 JSON 格式。
func (l *PropertyReviewLogic) PropertyReview(req *types.ParamPropertyReview) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	extra := req.Extra
	if extra != "" {
		var m map[string]any
		if err = json.Unmarshal([]byte(extra), &m); err != nil {
			l.Errorf("gateway/admin/propertyReview: bad extra %q err=%v", extra, err)
			return nil, err
		}
	}
	if _, err = l.svcCtx.UserProfile.AddPropertyReview(l.ctx, &userprofilerc.AddPropertyReviewReq{
		Mid:      req.Mid,
		New:      req.New,
		State:    int32(req.State),
		Property: int32(req.Property),
		Extra:    extra,
	}); err != nil {
		l.Errorf("gateway/admin/propertyReview: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
