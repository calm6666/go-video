package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type SaveSlotLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSaveSlotLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SaveSlotLogic {
	return &SaveSlotLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 保存坑位定义（不含条目）
func (l *SaveSlotLogic) SaveSlot(in *rpc.SaveSlotReq) (*rpc.SaveSlotReply, error) {
	lim := newLimits(l.svcCtx.Config)
	if err := checkWriteContext(in.GetCtx(), lim); err != nil {
		return nil, err
	}
	code := strings.TrimSpace(in.GetCode())
	if code == "" {
		return nil, model.ErrSlotCodeRequired
	}
	if !model.ValidSlotCode(code) {
		return nil, model.ErrSlotCodeInvalid
	}
	if err := checkPlatformSliceEnum(in.GetPlatforms()); err != nil {
		return nil, err
	}
	platforms, err := model.NormalizePlatformList(platformInt32List(in.GetPlatforms()))
	if err != nil {
		return nil, err
	}
	capacity, err := slotCapacity(in.GetCapacity(), lim)
	if err != nil {
		return nil, err
	}
	state := in.GetState()
	if state == 0 {
		// 新坑位必须先定义再排内容：默认启用会让端上立刻拿到一个空坑位。
		state = model.StateOff
	}
	if err := checkState(state); err != nil {
		return nil, err
	}
	if in.GetExpectVersion() < 0 {
		return nil, model.ErrVersionConflict
	}
	m := &l.svcCtx.Models
	ts := model.NowUnix()

	if in.GetSlotId() == 0 {
		slot := &model.RecommendSlot{
			Code:       code,
			Page:       strings.TrimSpace(in.GetPage()),
			Title:      in.GetTitle(),
			Platforms:  platforms,
			Capacity:   capacity,
			State:      state,
			OperatorID: in.GetCtx().GetOperatorId(),
			Remark:     in.GetRemark(),
			Ctime:      ts,
		}
		id, ierr := m.Slot.Insert(l.ctx, slot)
		if ierr != nil {
			if errors.Is(ierr, model.ErrSlotCodeConflict) {
				return nil, fmt.Errorf("%w: code=%s", model.ErrSlotCodeConflict, code)
			}
			return nil, ierr
		}
		slot.SlotID = id
		cacheDel(l.ctx, l.svcCtx.Cache, lim.slotKeys(slot), l.Logger)
		entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "create_slot",
			"ops_config:slot", strconv.FormatInt(id, 10), "", slotDigest(slot), in.GetReason(), ts)
		return &rpc.SaveSlotReply{Slot: slotInfo(slot), AuditEntryId: entryID}, nil
	}

	cur, err := m.Slot.FindByID(l.ctx, in.GetSlotId())
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("%w: slot_id=%d", model.ErrSlotNotFound, in.GetSlotId())
	}
	if capacity < cur.Capacity {
		// 缩容前必须确认现有生效条目装得下：否则已经排好的内容会凭空从页面上消失，
		// 而调用方拿到的「成功」里没有任何线索指向这件事。
		on, cerr := m.SlotItem.CountBySlot(l.ctx, cur.SlotID, model.StateOn)
		if cerr != nil {
			return nil, cerr
		}
		if int32(on) > capacity {
			return nil, fmt.Errorf("%w: 现有生效条目 %d 条 > 新容量 %d，请先调整条目再缩容",
				model.ErrSlotCapacityInvalid, on, capacity)
		}
	}

	next := &model.RecommendSlot{
		SlotID:     cur.SlotID,
		Code:       code,
		Page:       strings.TrimSpace(in.GetPage()),
		Title:      in.GetTitle(),
		Platforms:  platforms,
		Capacity:   capacity,
		State:      state,
		Version:    cur.Version,
		OperatorID: in.GetCtx().GetOperatorId(),
		Remark:     in.GetRemark(),
		Ctime:      cur.Ctime,
	}
	ok, uerr := m.Slot.UpdateWithVersion(l.ctx, next, in.GetExpectVersion(), ts)
	if uerr != nil {
		return nil, uerr
	}
	if !ok {
		return nil, fmt.Errorf("%w: slot_id=%d expect=%d current=%d",
			model.ErrVersionConflict, cur.SlotID, in.GetExpectVersion(), cur.Version)
	}

	var keys []string
	keys = append(keys, lim.slotKeys(cur)...)
	keys = append(keys, lim.slotKeys(next)...)
	cacheDel(l.ctx, l.svcCtx.Cache, dedupeStrings(keys), l.Logger)

	entryID := appendAudit(l.ctx, l.svcCtx.Audit, l.Logger, in.GetCtx(), "save_slot",
		"ops_config:slot", strconv.FormatInt(cur.SlotID, 10),
		fmt.Sprintf("version=%d,state=%d,capacity=%d", cur.Version, cur.State, cur.Capacity),
		slotDigest(next), in.GetReason(), ts)

	return &rpc.SaveSlotReply{Slot: slotInfo(next), AuditEntryId: entryID}, nil
}

// slotCapacity 归一坑位容量：0 取 model 默认，上限同时受配置与硬上限约束。
// 没有容量上限就等于给网关一个无界结果集，所以这里没有「不限制」这个选项。
func slotCapacity(raw int32, lim limits) (int32, error) {
	if raw == 0 {
		raw = model.DefaultSlotCapacity
	}
	if !model.ValidSlotCapacity(raw) {
		return 0, fmt.Errorf("%w: %d（合法区间 1..%d）", model.ErrSlotCapacityInvalid, raw, model.MaxSlotCapacityHard)
	}
	if lim.slotMaxCapacity > 0 && raw > int32(lim.slotMaxCapacity) {
		return 0, fmt.Errorf("%w: %d > 配置上限 %d", model.ErrSlotCapacityInvalid, raw, lim.slotMaxCapacity)
	}
	return raw, nil
}

func slotDigest(s *model.RecommendSlot) string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("code=%s,page=%s,capacity=%d,state=%d,platforms=%q,version=%d",
		s.Code, s.Page, s.Capacity, s.State, s.Platforms, s.Version)
}
