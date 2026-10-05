// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	danmakurpc "go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminDeleteDanmakuLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营删除任意弹幕（admin=true，reason 落 op_log 审计）
func NewAdminDeleteDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminDeleteDanmakuLogic {
	return &AdminDeleteDanmakuLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminDeleteDanmaku 聚合 danmaku DeleteDanmaku RPC：operator_mid 作为 admin_mid 传给下游的 mid，
// 并固定 admin=true 绕过「只能删自己弹幕」的归属校验；弹幕是否存在、状态机是否允许转 DELETED
// 以及 op_log 审计都由 danmaku 服务判定（AGENTS.md §5/§8）。reason/trace_id 原样透传，便于回溯。
func (l *AdminDeleteDanmakuLogic) AdminDeleteDanmaku(req *types.ParamAdminDeleteDanmaku) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	if err := adminSubjectGate(l.ctx, "adminDeleteDanmaku", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}

	if _, err := l.svcCtx.Danmaku.DeleteDanmaku(l.ctx, &danmakurpc.DeleteDanmakuReq{
		Dmid:    req.Dmid,
		Mid:     req.OperatorMid,
		Admin:   true,
		Reason:  req.Reason,
		TraceId: req.TraceId,
	}); err != nil {
		l.Errorf("gateway/admin/adminDeleteDanmaku: operator=%d dmid=%d err=%v", req.OperatorMid, req.Dmid, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
