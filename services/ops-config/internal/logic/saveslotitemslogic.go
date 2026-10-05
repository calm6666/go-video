package logic

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveSlotItemsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveSlotItemsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveSlotItemsLogic {
	return &SaveSlotItemsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 全量覆盖坑位条目与排期（一次事务、同一个时间戳）
//
// 排期是成组生效的东西：一半新一半旧等于把线上位置交给随机结果，
// 所以整批必须在同一事务里写、共用同一个 ts。
func (l *SaveSlotItemsLogic) SaveSlotItems(in *rpc.SaveSlotItemsReq) (*rpc.SaveSlotItemsReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	if err := checkReason(in.GetReason(), lim); err != nil {
		return nil, err
	}
	if in.GetSlotId() <= 0 {
		return nil, model.ErrSlotNotFound
	}
	m := &l.svcCtx.Models

	slot, err := m.Slot.FindByID(l.ctx, in.GetSlotId())
	if err != nil {
		return nil, err
	}
	if slot == nil {
		return nil, fmt.Errorf("%w: slot_id=%d", model.ErrSlotNotFound, in.GetSlotId())
	}

	items := slotItemRows(in.GetItems())
	if len(items) == 0 {
		return nil, model.ErrBatchEmpty
	}
	if len(items) > lim.slotMaxItems {
		return nil, fmt.Errorf("%w: %d > %d", model.ErrSlotItemLimit, len(items), lim.slotMaxItems)
	}
	// logic 只做「每条都得是引用」这一层前置检查（错误信息能指到第几条）；
	// position 越界/重复、同内容重复、排期窗口这些判定留在 model.ReplaceAll 里，
	// 两侧共用一套规则才不会分叉。capacity 取**库里那份**，不信调用方传来的值。
	for i, it := range items {
		if strings.TrimSpace(it.ItemID) == "" || it.ItemType == "" {
			return nil, fmt.Errorf("items[%d]: %w", i, model.ErrItemRefRequired)
		}
		it.SlotID = slot.SlotID
	}

	ts := model.NowUnix()
	before, _ := m.SlotItem.CountBySlot(l.ctx, slot.SlotID, 0)
	affected, err := m.SlotItem.ReplaceAll(l.ctx, slot.SlotID, slot.Capacity, items,
		in.GetCtx().GetOperatorId(), ts, lim.slotMaxItems)
	if err != nil {
		return nil, err
	}

	// 端上按 code 寻址，因此删 code 维度的键；id 维度的键一起删，避免后台读到旧的。
	cacheDel(l.ctx, l.svcCtx.Cache, lim.slotKeys(slot), l.Logger)

	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "save_slot_items",
		"ops_config:slot", strconv.FormatInt(slot.SlotID, 10),
		"items_before="+strconv.FormatInt(before, 10)+",capacity="+strconv.Itoa(int(slot.Capacity)),
		fmt.Sprintf("items_after=%d,affected=%d", len(items), affected), in.GetReason(), ts)

	return &rpc.SaveSlotItemsReply{
		SlotId:       slot.SlotID,
		Total:        int32(len(items)),
		AuditEntryId: entryID,
	}, nil
}
