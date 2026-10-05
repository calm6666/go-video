// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpsRefreshCacheLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主动失效运行时缓存（target config/topic/slot/all，递增 epoch 并写审计）
func NewOpsRefreshCacheLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsRefreshCacheLogic {
	return &OpsRefreshCacheLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsRefreshCacheLogic) OpsRefreshCache(req *types.ParamOpsRefreshCache) (resp *types.OpsRefreshCacheResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	// target 是契约里闭合的四个取值（proto 注释 config/topic/slot/all）。这里卡死集合而不是透传：
	// 缓存失效写错一个字母会「成功但什么都没失效」，是典型的静默失败，后台拿不到任何信号。
	switch req.Target {
	case "config", "topic", "slot", "all":
	default:
		return nil, errors.New("gateway/admin: target must be config/topic/slot/all")
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.OpsConfig.RefreshCache(l.ctx, &opsconfigrpc.RefreshCacheReq{
		Ctx:     callCtx,
		Target:  req.Target,
		CfgKey:  req.CfgKey,
		Scope:   req.Scope,
		TopicId: req.TopicId,
		SlotId:  req.SlotId,
		Reason:  req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsRefreshCache: operator=%d request_id=%s target=%s cfg_key=%s topic_id=%d slot_id=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.Target, req.CfgKey, req.TopicId, req.SlotId, err)
		return nil, err
	}
	return &types.OpsRefreshCacheResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsRefreshCacheData{
			Affected:     reply.GetAffected(),
			Epoch:        reply.GetEpoch(),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
