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

type RealnameApplyLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交实名认证申请
func NewRealnameApplyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameApplyLogic {
	return &RealnameApplyLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 提交实名认证申请：聚合 user-profile RealnameApply RPC。
func (l *RealnameApplyLogic) RealnameApply(req *types.ParamRealnameApply) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.RealnameApply(l.ctx, &userprofilerc.RealnameApplyReq{
		Mid:           req.Mid,
		CaptureCode:   req.Capture,
		Realname:      req.RealName,
		CardType:      int32(req.CardType),
		CardCode:      req.CardNum,
		Country:       int32(req.Country),
		HandImgToken:  req.IMG1Token,
		FrontImgToken: req.IMG2Token,
		BackImgToken:  req.IMG3Token,
	}); err != nil {
		l.Errorf("gateway/app/realnameApply: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
