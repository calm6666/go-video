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

type OpsSaveTopicLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/更新专题（zone_ids/tag_ids 全量覆盖引用，expect_version 乐观锁）
func NewOpsSaveTopicLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsSaveTopicLogic {
	return &OpsSaveTopicLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsSaveTopicLogic) OpsSaveTopic(req *types.ParamOpsSaveTopic) (resp *types.OpsTopicResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("title", req.Title); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	// 新建必须给 slug（端上按它寻址）；更新时留空表示不改，语义由 ops-config 判定。
	if req.TopicId <= 0 {
		if err := requireNonEmpty("slug", req.Slug); err != nil {
			return nil, err
		}
	}
	if req.ExpectVersion < 0 {
		return nil, errOpsExpectVersionInvalid
	}
	if err := opsTimeWindow(req.StartAt, req.EndAt); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.OpsConfig.SaveTopic(l.ctx, &opsconfigrpc.SaveTopicReq{
		Ctx:           callCtx,
		TopicId:       req.TopicId,
		Slug:          req.Slug,
		Title:         req.Title,
		Description:   req.Description,
		Cover:         req.Cover,
		ZoneIds:       req.ZoneIds,
		TagIds:        req.TagIds,
		State:         req.State,
		Sort:          req.Sort,
		StartAt:       req.StartAt,
		EndAt:         req.EndAt,
		ExpectVersion: req.ExpectVersion,
		Reason:        req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsSaveTopic: operator=%d request_id=%s topic_id=%d slug=%s expect_version=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.TopicId, req.Slug, req.ExpectVersion, err)
		return nil, err
	}
	return &types.OpsTopicResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsTopicData{
			Topic:        opsTopicToAPI(reply.GetTopic()),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
