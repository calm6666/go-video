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

type OpsSaveSlotItemsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 全量覆盖坑位条目与排期（最多 200 条，position 不重复且不超过 capacity）
func NewOpsSaveSlotItemsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsSaveSlotItemsLogic {
	return &OpsSaveSlotItemsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsSaveSlotItemsLogic) OpsSaveSlotItems(req *types.ParamOpsSaveSlotItems) (resp *types.OpsSlotItemsSaveResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	if req.SlotId <= 0 {
		return nil, errors.New("gateway/admin: slot_id must be > 0")
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	// 与专题条目同理：空列表会清空整个坑位，这里要求显式非空，position/capacity 合法性由 ops-config 判定。
	if len(req.Items) == 0 {
		return nil, errors.New("gateway/admin: items required (empty list would clear the slot)")
	}
	for i, item := range req.Items {
		// item_type/item_id 是这条坑位记录的唯一存在理由（坑位只存引用，不复制标题），
		// 缺任一都在下游定位不到内容；position 是否重复/超 capacity 由 ops-config 判定。
		if item.ItemType == "" || item.ItemIid == "" {
			return nil, fmt.Errorf("gateway/admin: items[%d].item_type/item_id required", i)
		}
		if item.StartAt < 0 || item.EndAt < 0 {
			return nil, fmt.Errorf("gateway/admin: items[%d].start_at/end_at must be >= 0", i)
		}
	}

	reply, err := l.svcCtx.OpsConfig.SaveSlotItems(l.ctx, &opsconfigrpc.SaveSlotItemsReq{
		Ctx:    callCtx,
		SlotId: req.SlotId,
		Items:  opsSlotItemsToRPC(req.Items),
		Reason: req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsSaveSlotItems: operator=%d request_id=%s slot_id=%d items=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.SlotId, len(req.Items), err)
		return nil, err
	}
	return &types.OpsSlotItemsSaveResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsSlotItemsSaveData{
			SlotId:       reply.GetSlotId(),
			Total:        reply.GetTotal(),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
