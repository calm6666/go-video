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

type ListBlockWordsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询屏蔽词（scope/oid/only_enabled 过滤）
func NewListBlockWordsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListBlockWordsLogic {
	return &ListBlockWordsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 屏蔽词分页查询：聚合 danmaku ListBlockWords RPC。
// danmaku 服务要求 operator_mid > 0 才允许运营侧读取词库，网关沿用同一主体做审计门槛；
// 分页口径与 danmaku model 一致（ps 上限 100，越界回落 20）。
func (l *ListBlockWordsLogic) ListBlockWords(req *types.ParamListBlockWords) (resp *types.DanmakuBlockWordsResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	if err := requireOperator("operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	// scope=0 在这里是「不按作用域过滤」，与写操作的必填语义不同。
	scope, err := danmakuBlockWordScope(req.Scope, true)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeDanmakuPage(req.Pn, req.Ps)
	reply, err := l.svcCtx.Danmaku.ListBlockWords(l.ctx, &danmakurpc.ListBlockWordsReq{
		Scope:       scope,
		Oid:         req.Oid,
		OnlyEnabled: req.OnlyEnabled,
		Pn:          pn,
		Ps:          ps,
		OperatorMid: req.OperatorMid,
	})
	if err != nil {
		l.Errorf("gateway/admin/listBlockWords: scope=%d oid=%d only_enabled=%v pn=%d ps=%d operator_mid=%d err=%v",
			req.Scope, req.Oid, req.OnlyEnabled, pn, ps, req.OperatorMid, err)
		return nil, err
	}
	return &types.DanmakuBlockWordsResponse{
		Code:    0,
		Message: "ok",
		Data: types.DanmakuBlockWordsData{
			Total: reply.GetTotal(),
			Words: blockWordsToAPI(reply.GetWords()),
		},
		TTL: 0,
	}, nil
}
