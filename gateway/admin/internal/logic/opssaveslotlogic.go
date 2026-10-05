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

type OpsSaveSlotLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/更新推荐位定义（code 唯一，capacity 上限由 ops-config 判定）
func NewOpsSaveSlotLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpsSaveSlotLogic {
	return &OpsSaveSlotLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OpsSaveSlotLogic) OpsSaveSlot(req *types.ParamOpsSaveSlot) (resp *types.OpsSlotResponse, err error) {
	if l.svcCtx.OpsConfig == nil {
		return nil, errOpsServiceNotConfigured
	}
	callCtx, err := opsCallContext(l.ctx, req.Ctx, true)
	if err != nil {
		return nil, err
	}
	// code 是坑位的唯一寻址句柄（端上按它取位），不允许空。
	if err := requireNonEmpty("code", req.Code); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("title", req.Title); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if req.Capacity < 0 {
		return nil, errors.New("gateway/admin: capacity must be >= 0")
	}
	if req.ExpectVersion < 0 {
		return nil, errOpsExpectVersionInvalid
	}

	reply, err := l.svcCtx.OpsConfig.SaveSlot(l.ctx, &opsconfigrpc.SaveSlotReq{
		Ctx:           callCtx,
		SlotId:        req.SlotId,
		Code:          req.Code,
		Page:          req.Page,
		Title:         req.Title,
		Platforms:     opsPlatformsToRPC(req.Platforms),
		Capacity:      req.Capacity,
		State:         req.State,
		ExpectVersion: req.ExpectVersion,
		Remark:        req.Remark,
		Reason:        req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/opsSaveSlot: operator=%d request_id=%s slot_id=%d code=%s expect_version=%d err=%v",
			callCtx.GetOperatorId(), callCtx.GetRequestId(), req.SlotId, req.Code, req.ExpectVersion, err)
		return nil, err
	}
	return &types.OpsSlotResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpsSlotData{
			Slot:         opsSlotToAPI(reply.GetSlot()),
			AuditEntryId: reply.GetAuditEntryId(),
		},
		TTL: 0,
	}, nil
}
