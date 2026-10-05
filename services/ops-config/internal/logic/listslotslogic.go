package logic

import (
	"context"

	"go-video/services/ops-config/internal/svc"
	"go-video/services/ops-config/model"
	"go-video/services/ops-config/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSlotsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSlotsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSlotsLogic {
	return &ListSlotsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 后台分页列出坑位定义（只读，不写审计）
func (l *ListSlotsLogic) ListSlots(in *rpc.ListSlotsReq) (*rpc.ListSlotsReply, error) {
	lim := newLimits(l.svcCtx.Config)
	pn, ps, err := pageOf(in.GetPn(), in.GetPs(), lim.maxPageSize)
	if err != nil {
		return nil, err
	}
	state := in.GetState()
	if state != 0 {
		if err := checkState(state); err != nil {
			return nil, err
		}
	}
	// UNSPECIFIED = 不按端过滤；给了端时 model 侧用
	// "(platforms = '' OR platforms LIKE ',p,')" 同时命中「不限端」与「含该端」，
	// 少了前一分支会漏掉几乎全部坑位；口径与内存侧 PlatformMatchesAny 一致。
	if err := checkPlatformInt(int32(in.GetPlatform()), false); err != nil {
		return nil, err
	}

	rows, total, err := l.svcCtx.Models.Slot.List(l.ctx, model.SlotFilter{
		Page:     in.GetPage(),
		State:    state,
		Platform: int32(in.GetPlatform()),
		Pn:       pn,
		Ps:       ps,
	})
	if err != nil {
		return nil, err
	}
	// 只回坑位定义：条目是排期数据，看内容走 ResolveSlot。
	// 混在一起会让这个接口每条坑位都多一次子查询。
	return &rpc.ListSlotsReply{Items: slotList(rows), Total: lim.totalOf(total)}, nil
}
