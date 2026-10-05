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

type BlockWordLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 屏蔽词新增/停用/删除（action 1/2/3，operator_mid 必填）
func NewBlockWordLogic(ctx context.Context, svcCtx *svc.ServiceContext) *BlockWordLogic {
	return &BlockWordLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 屏蔽词管理：聚合 danmaku BlockWord RPC。
// 词长度、scope 与 oid 的组合、状态推进都由 danmaku 服务判定（AGENTS.md §5），
// 网关只保证有审计主体并且不把未知枚举透传给下游。
func (l *BlockWordLogic) BlockWord(req *types.ParamBlockWord) (resp *types.DanmakuBlockWordResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	if err := adminSubjectGate(l.ctx, "blockWord", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	action, err := danmakuBlockWordAction(req.Action)
	if err != nil {
		return nil, err
	}
	scope, err := danmakuBlockWordScope(req.Scope, false)
	if err != nil {
		return nil, err
	}
	// 契约缺口：danmaku.v1.BlockWordReq 没有 request_id/idempotency_key 字段，
	// DISABLE/DELETE 重试无法在网关侧做幂等保护，只能依赖服务端 Upsert/软删语义收敛。
	reply, err := l.svcCtx.Danmaku.BlockWord(l.ctx, &danmakurpc.BlockWordReq{
		Action:      action,
		Word:        req.Word,
		Scope:       scope,
		Oid:         req.Oid,
		OperatorMid: req.OperatorMid,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/blockWord: action=%d scope=%d oid=%d operator_mid=%d err=%v",
			req.Action, req.Scope, req.Oid, req.OperatorMid, err)
		return nil, err
	}
	return &types.DanmakuBlockWordResponse{
		Code:    0,
		Message: "ok",
		Data: types.DanmakuBlockWordResultData{
			WordId: reply.GetWordId(),
			State:  reply.GetState(),
		},
		TTL: 0,
	}, nil
}
