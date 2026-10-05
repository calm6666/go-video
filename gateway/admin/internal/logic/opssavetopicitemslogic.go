// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	opsconfigrpc "go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpsSaveTopicItemsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 全量覆盖专题条目（最多 500 条，position 从 1 连续）
func NewOpsSaveTopicItemsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsSaveTopicItemsLogic {
	return &OpsSaveTopicItemsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsSaveTopicItemsLogic) OpsSaveTopicItems(req *types.ParamOpsSaveTopicItems) (resp *types.OpsTopicItemsSaveResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if req.TopicId <= 0 {
		return nil, errors.New("gateway/admin: topic_id must be > 0")
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	// 全量覆盖语义下「空列表」等于清空整个专题，与「忘了传 items」在报文上无法区分，
	// 因此要求显式非空；真要清空请走 opsSaveTopic(state=2)。
	if len(req.Items) == 0 {
		return nil, errors.New("gateway/admin: items required (empty list would clear the topic)")
	}
	for i, item := range req.Items {
		// item_type/item_id 是专题条目唯一必须携带的信息（专题只存引用，不复制标题/时长），
		// 缺任一在下游定位不到内容；position 是否从 1 连续由 ops-config 判定。
		if item.ItemType == "" || item.ItemIid == "" {
			return nil, fmt.Errorf("gateway/admin: items[%d].item_type/item_id required", i)
		}
	}

	reply, err := l.svcCtx.OpsConfig.SaveTopicItems(l.ctx, &opsconfigrpc.SaveTopicItemsReq{
		Ctx:     callCtx,
		TopicId: req.TopicId,
		Items:   opsTopicItemsToRPC(req.Items),
		Reason:  req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsSaveTopicItems: operator=%d request_id=%s topic_id=%d items=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.TopicId, len(req.Items), err)
		return nil, err
	}
	return &types.OpsTopicItemsSaveResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsTopicItemsSaveData{
			TopicId:      reply.GetTopicId(),
			Total:        reply.GetTotal(),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
