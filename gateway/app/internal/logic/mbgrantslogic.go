// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MbGrantsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的会员开通记录（用户侧台账）
func NewMbGrantsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MbGrantsLogic {
	return &MbGrantsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MbGrants 读本人授予台账：mid 必填且必须为正，网关只查自己那一份（ListGrantsReq 的
// mid=0 跨用户语义属于运营面，不在终端路由出现）。page/page_size 原样透传，
// 0 表示由服务取默认并裁剪，网关不设上限也不改写。
// 台账是历史事实，但会随退款回收追加行，因此 TTL 0。
func (l *MbGrantsLogic) MbGrants(req *types.ParamMbGrants) (resp *types.MbGrantsResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errors.New("membership service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.ListGrants(l.ctx, &membershiprpc.ListGrantsReq{
		Mid:     req.Mid,
		VipType: membershiprpc.VipType(req.VipType),
		FromTs:  req.FromTs,
		ToTs:    req.ToTs,
		Page:    int64(req.Page),
		Size:    int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/mbGrants: mid=%d page=%d err=%v", req.Mid, req.Page, err)
		return nil, err
	}
	return &types.MbGrantsResponse{
		Code:    0,
		Message: "ok",
		Data: types.MbGrantsData{
			Grants:   mbGrantsToAPI(reply.GetGrants()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
