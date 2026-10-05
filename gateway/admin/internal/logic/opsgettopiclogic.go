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

type OpsGetTopicLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 专题详情（with_items=true 时附带条目，未命中返回 found=false）
func NewOpsGetTopicLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsGetTopicLogic {
	return &OpsGetTopicLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsGetTopicLogic) OpsGetTopic(req *types.ParamOpsGetTopic) (resp *types.OpsTopicDetailResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	// topic_id 与 slug 二选一：两者都空时下游无法定位，网关先拦住而不是发一次必然失败的 RPC。
	if req.TopicId <= 0 && req.Slug == "" {
		return nil, errors.New("gateway/admin: topic_id or slug required")
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, false)
	if err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.OpsConfig.GetTopic(l.ctx, &opsconfigrpc.GetTopicReq{
		Ctx:       callCtx,
		TopicId:   req.TopicId,
		Slug:      req.Slug,
		WithItems: req.WithItems,
		ItemLimit: req.ItemLimit,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsGetTopic: operator=%d topic_id=%d slug=%s with_items=%v err=%v",
			callCtx.GetOperatorId(), req.TopicId, req.Slug, req.WithItems, err)
		return nil, err
	}
	// found=false 是「专题不存在」的正常答案，回成功 + found=false，不折成 error。
	return &types.OpsTopicDetailResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsTopicDetailData{
			Topic:    opsTopicToAPI(reply.GetTopic()),
			Items:    opsTopicItemsToAPI(reply.GetItems()),
			Found:    reply.GetFound(),
			CacheTTL: reply.GetTtl(),
		},
		TTL: 0,
	}, nil
}
