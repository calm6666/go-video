// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrpc "go-video/services/creator/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminUpGroupMidsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询分组下的 UP 主
func NewAdminUpGroupMidsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminUpGroupMidsLogic {
	return &AdminUpGroupMidsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminUpGroupMids 聚合 creator UpGroupMids RPC：翻某个特殊分组的成员名册。
// 分组是否存在由 creator 判定（errInvalidGroupID），网关只把 pn/ps 收敛到服务的 1000 上限；
// mids 是服务侧给定的顺序，网关不再重排，否则会与下游分页偏移量不一致导致漏读/重读。
func (l *AdminUpGroupMidsLogic) AdminUpGroupMids(req *types.ParamAdminUpGroupMids) (resp *types.AdminUpGroupMidsResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}
	pn, ps := normalizeCreatorUpGroupMidsPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Creator.UpGroupMids(l.ctx, &creatorrpc.UpGroupMidsReq{
		GroupId: req.GroupId,
		Pn:      pn,
		Ps:      ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/adminUpGroupMids: group_id=%d pn=%d ps=%d err=%v", req.GroupId, pn, ps, err)
		return nil, err
	}
	return &types.AdminUpGroupMidsResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminUpGroupMidsData{
			Mids:  creatorInt64Ids(reply.GetMids()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
