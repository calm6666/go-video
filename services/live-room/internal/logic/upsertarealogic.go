package logic

import (
	"context"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertAreaLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertAreaLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertAreaLogic {
	return &UpsertAreaLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营侧新建/修改直播分区
//
// 名称唯一性不做「先查再写」的竞态预检：uniq_area_name 才是真值，
// 预检只会多一个 TOCTOU 窗口，冲突由 Insert/Update 直接翻译成 ErrAreaNameConflict。
// 层级强制两级：三级分区会让发现页的选区路径与客户端渲染各自理解一遍。
func (l *UpsertAreaLogic) UpsertArea(in *rpc.UpsertAreaReq) (*rpc.UpsertAreaReply, error) {
	if in == nil {
		return nil, model.ErrOperatorRequired
	}
	if err := checkOperator(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	name, err := checkAreaName(in.GetAreaName(), l.svcCtx.Config.LiveRoom.AreaNameMaxLength)
	if err != nil {
		return nil, err
	}
	if in.GetAreaId() < 0 || in.GetParentAreaId() < 0 {
		return nil, model.ErrInvalidAreaID
	}
	state, err := areaStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	if in.GetParentAreaId() > 0 {
		if err := l.checkParent(in.GetParentAreaId(), in.GetAreaId()); err != nil {
			return nil, err
		}
	}
	reqID := strings.TrimSpace(in.GetRequestId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcUpsertArea, reqID, model.IdempotencyKindRequest,
		0, 0, "")
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcUpsertArea, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.UpsertAreaReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		// UpsertAreaReply 契约里没有 replayed 位（契约缺口）：重放时原样回既有结果，
		// created 仍是首次执行的事实。
		return reply, nil
	}

	created := in.GetAreaId() == 0
	if created {
		id, err := l.svcCtx.Areas.Insert(l.ctx, &model.LiveArea{
			AreaName:     name,
			ParentAreaID: in.GetParentAreaId(),
			Sort:         in.GetSort(),
			State:        state,
			OperatorMid:  in.GetOperatorMid(),
		})
		if err != nil {
			return nil, err
		}
		invalidateAreaListCache(l.ctx, l.svcCtx)
		reply := &rpc.UpsertAreaReply{AreaId: id, Created: true}
		saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
		return reply, nil
	}

	cur, err := l.svcCtx.Areas.FindOne(l.ctx, in.GetAreaId())
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, model.ErrAreaNotFound
	}
	// 停用是「不能再被新房间选择」，不是删数据：仍有未关闭房间挂在上面时不能停。
	if cur.State == model.AreaStateEnabled && state == model.AreaStateDisabled {
		if err := l.checkAreaReusable(cur.AreaID); err != nil {
			return nil, err
		}
	}
	ok, err := l.svcCtx.Areas.Update(l.ctx, &model.LiveArea{
		AreaID:       cur.AreaID,
		AreaName:     name,
		ParentAreaID: in.GetParentAreaId(),
		Sort:         in.GetSort(),
		State:        state,
		OperatorMid:  in.GetOperatorMid(),
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, model.ErrAreaNotFound
	}
	invalidateAreaListCache(l.ctx, l.svcCtx)
	reply := &rpc.UpsertAreaReply{AreaId: cur.AreaID, Created: false}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// checkParent 校验上级分区存在、启用且只有一层（父本身必须是一级分区）。
func (l *UpsertAreaLogic) checkParent(parentID, selfID int64) error {
	if selfID > 0 && parentID == selfID {
		return model.ErrAreaParentInvalid
	}
	level, parent, err := l.svcCtx.Areas.LevelOf(l.ctx, parentID)
	if err != nil {
		return err
	}
	if parent == nil {
		return model.ErrAreaParentInvalid
	}
	if parent.State != model.AreaStateEnabled {
		return model.ErrAreaParentInvalid
	}
	// LevelOf 返回的是 parent 自身所在层级；parent 已在第 2 层时再挂子分区就是三级。
	if level >= model.AreaMaxLevel {
		return model.ErrAreaParentInvalid
	}
	return nil
}

// checkAreaReusable 停用前的占用校验：有启用子分区或有未关闭房间在用时都拒绝。
func (l *UpsertAreaLogic) checkAreaReusable(areaID int64) error {
	children, err := l.svcCtx.Areas.CountChildren(l.ctx, areaID)
	if err != nil {
		return err
	}
	if children > 0 {
		return model.ErrAreaInUse
	}
	rooms, err := l.svcCtx.Rooms.CountByArea(l.ctx, areaID, areaOccupancyStates())
	if err != nil {
		return err
	}
	if rooms > 0 {
		return model.ErrAreaInUse
	}
	return nil
}

// areaStateFilter 校验分区启停取值：只允许 1 启用 / 0 停用。
func areaStateFilter(state int32) (int32, error) {
	switch state {
	case model.AreaStateEnabled, model.AreaStateDisabled:
		return state, nil
	default:
		return 0, model.ErrAreaStateInvalid
	}
}
