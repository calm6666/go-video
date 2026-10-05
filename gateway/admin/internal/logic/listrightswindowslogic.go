// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	rightsrpc "go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRightsWindowsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询播放窗口
func NewListRightsWindowsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRightsWindowsLogic {
	return &ListRightsWindowsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询播放窗口：聚合 rights ListWindows RPC。
// content_id / contract_id 为 0 表示不筛选；content_type、state 直接映射 protobuf 枚举，
// 0（*_UNSPECIFIED）表示不筛选。
func (l *ListRightsWindowsLogic) ListRightsWindows(req *types.ParamListWindows) (resp *types.RightsWindowsResponse, err error) {
	if l.svcCtx.Rights == nil {
		return nil, errors.New("rights service not configured")
	}
	reply, err := l.svcCtx.Rights.ListWindows(l.ctx, &rightsrpc.ListWindowsReq{
		ContentId:   req.ContentId,
		ContractId:  req.ContractId,
		ContentType: rightsrpc.ContentType(req.ContentType),
		State:       rightsrpc.WindowState(req.State),
		Pn:          req.Pn,
		Ps:          req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRightsWindows: content_id=%d contract_id=%d content_type=%d state=%d pn=%d ps=%d err=%v",
			req.ContentId, req.ContractId, req.ContentType, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	windows := make([]types.RightsWindowItem, 0, len(reply.GetWindows()))
	for _, item := range reply.GetWindows() {
		windows = append(windows, toAdminRightsWindow(item))
	}
	return &types.RightsWindowsResponse{
		Code:    0,
		Message: "ok",
		Data: types.RightsWindowsData{
			// rights proto 的 total 是 int32，HTTP 载荷按 int64 暴露。
			Total:   int64(reply.GetTotal()),
			Windows: windows,
		},
		TTL: 0,
	}, nil
}
