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

type AdminHighAllyUpsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询高能联盟 UP 主签约信息
func NewAdminHighAllyUpsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminHighAllyUpsLogic {
	return &AdminHighAllyUpsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminHighAllyUps 聚合 creator GetHighAllyUps RPC：按 mid 批量查签约信息。
// mids 为空时服务侧直接返回空 map（不打 DB），网关不额外拒绝；上限 100 与同域批量契约一致，
// 防止后台输入框被放大成超长 IN 查询。返回按 mid 升序展平，未签约的 mid 不补零值。
func (l *AdminHighAllyUpsLogic) AdminHighAllyUps(req *types.ParamAdminHighAllyUps) (resp *types.AdminHighAllyUpsResponse, err error) {
	if l.svcCtx.Creator == nil {
		return nil, errors.New("creator service not configured")
	}
	if err := creatorHighAllyMidsGuard(req.Mids); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Creator.GetHighAllyUps(l.ctx, &creatorrpc.HighAllyUpsReq{
		Mids: req.Mids,
	})
	if err != nil {
		l.Errorf("gateway/admin/adminHighAllyUps: count=%d err=%v", len(req.Mids), err)
		return nil, err
	}
	return &types.AdminHighAllyUpsResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AdminHighAllyUpsData{Ups: signUpsToAPI(reply.GetLists())},
		TTL:     0,
	}, nil
}
