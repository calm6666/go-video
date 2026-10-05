// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpsListTopicsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询专题（state/zone/tag/keyword 过滤，online_only 附加生效窗口判定）
func NewOpsListTopicsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsListTopicsLogic {
	return &OpsListTopicsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsListTopicsLogic) OpsListTopics(req *types.ParamOpsListTopics) (resp *types.OpsTopicsResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeOpsPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.OpsConfig.ListTopics(l.ctx, &opsconfigrpc.ListTopicsReq{
		Ctx:        callCtx,
		State:      req.State,
		ZoneId:     req.ZoneId,
		TagId:      req.TagId,
		Keyword:    req.Keyword,
		Pn:         pn,
		Ps:         ps,
		OnlineOnly: req.OnlineOnly,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsListTopics: operator=%d state=%d zone=%d tag=%d online_only=%v pn=%d ps=%d err=%v",
			callCtx.GetOperatorId(), req.State, req.ZoneId, req.TagId, req.OnlineOnly, pn, ps, err)
		return nil, err
	}
	return &types.OpsTopicsResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsTopicsData{
			Items: opsTopicsToAPI(reply.GetItems()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
