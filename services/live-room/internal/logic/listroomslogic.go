package logic

import (
	"context"
	"fmt"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRoomsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRoomsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRoomsLogic {
	return &ListRoomsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// anchorVisibleFetchCap 是「按主播可见」路径一次取回 room_id 的硬上限。
// 列表必须带界：无界取 id 再回表等于把 live_room_anchor 的全索引扫给每次刷新。
// 超过该窗口的翻页直接拒绝，而不是悄悄返回不完整的一页。
const anchorVisibleFetchCap = 500

// 分页浏览房间（发现页/主播主页/运营列表）
//
// 两条取数路径共用同一套过滤条件：
//   - owner_mid=0：Rooms.List + Rooms.Count 共用 RoomListQuery，total 与页内容同口径；
//   - owner_mid>0：房管/连麦主播也要看到自己所在的房间，所以先按绑定表取可见 room_id
//     再回表（rooms.ListByRoomIDs），total 取可见集合大小——它是本服务能给出的真实上界。
func (l *ListRoomsLogic) ListRooms(in *rpc.ListRoomsReq) (*rpc.ListRoomsReply, error) {
	if in == nil {
		return &rpc.ListRoomsReply{}, nil
	}
	size, err := l.svcCtx.PageSize(in.GetPageSize())
	if err != nil {
		return nil, err
	}
	page, err := clampPage(in.GetPage())
	if err != nil {
		return nil, err
	}
	offset := pageOffset(page, size)
	state, err := roomStateFilter(in.GetState())
	if err != nil {
		return nil, err
	}
	order, err := roomOrderFilter(in.GetOrder())
	if err != nil {
		return nil, err
	}
	if in.GetAreaId() < 0 {
		return nil, model.ErrInvalidAreaID
	}

	if in.GetOwnerMid() > 0 {
		return l.listByVisibleAnchor(in.GetOwnerMid(), model.RoomListQuery{
			AreaID: areaOf(in.GetAreaId()),
			State:  state,
			Order:  order,
			Limit:  int32(size),
		}, page, size, offset)
	}
	if in.GetOwnerMid() < 0 {
		return nil, model.ErrInvalidMid
	}

	q := model.RoomListQuery{
		AreaID: areaOf(in.GetAreaId()),
		State:  state,
		Order:  order,
		Offset: offset,
		Limit:  int32(size),
	}
	rows, err := l.svcCtx.Rooms.List(l.ctx, q)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Rooms.Count(l.ctx, q)
	if err != nil {
		return nil, err
	}
	return &rpc.ListRoomsReply{
		Rooms:    roomInfoList(rows),
		Total:    clampTotal(total),
		Page:     int32(page),
		PageSize: int32(size),
	}, nil
}

// listByVisibleAnchor 「我可见的房间」路径：绑定表取可见 room_id → 切片 → 回表。
// total 是可绑定房间数（本服务能给出的真实上界）；q.OwnerMid 留空，
// 可见性完全来自 ids，否则房管/连麦主播会被 owner_mid 再筛掉。
func (l *ListRoomsLogic) listByVisibleAnchor(ownerMid int64, q model.RoomListQuery,
	page, size int, offset int32) (*rpc.ListRoomsReply, error) {
	if offset+int32(size) > anchorVisibleFetchCap {
		return nil, fmt.Errorf("%w: 可见房间翻页上限 %d", model.ErrInvalidPage, anchorVisibleFetchCap)
	}
	ids, err := l.svcCtx.Anchors.ListRoomsByMid(l.ctx, ownerMid, model.AnchorRoleUnspecified, anchorVisibleFetchCap)
	if err != nil {
		return nil, err
	}
	total := clampTotal(int64(len(ids)))
	if int(offset) >= len(ids) {
		return &rpc.ListRoomsReply{Total: total, Page: int32(page), PageSize: int32(size)}, nil
	}
	pageIDs := ids[offset:]
	if len(pageIDs) > size {
		pageIDs = pageIDs[:size]
	}
	rows, err := l.svcCtx.Rooms.ListByRoomIDs(l.ctx, pageIDs, q)
	if err != nil {
		return nil, err
	}
	return &rpc.ListRoomsReply{
		Rooms:    roomInfoList(rows),
		Total:    total,
		Page:     int32(page),
		PageSize: int32(size),
	}, nil
}

// roomStateFilter 校验状态过滤：UNSPECIFIED 表示不过滤，其余必须是已定义状态。
func roomStateFilter(s rpc.RoomState) (int32, error) {
	v := int32(s)
	if v == model.RoomStateUnspecified {
		return 0, nil
	}
	if !model.ValidRoomState(v) {
		return 0, fmt.Errorf("%w: state=%d", model.ErrInvalidRoomTransition, v)
	}
	return v, nil
}

// roomOrderFilter 校验排序：只允许 rpc.RoomOrder 声明的三个取值，
// 未知取值不退化成默认序——「按没索引的列排」是列表最贵的故障。
func roomOrderFilter(o rpc.RoomOrder) (int32, error) {
	v := int32(o)
	switch v {
	case model.RoomOrderIDDesc, model.RoomOrderLivingFirst, model.RoomOrderCtimeDesc:
		return v, nil
	default:
		return 0, fmt.Errorf("%w: order=%d", model.ErrRoomOrderInvalid, v)
	}
}

// areaOf 归一分区过滤：0 与负数在这里都是「不过滤」的显式写法，
// 但 0 同时是「未设置」的列值，所以只在负数时报错（调用前已判过）。
func areaOf(id int64) int64 {
	if id < 0 {
		return 0
	}
	return id
}

// clampTotal 把 COUNT(*) 收敛到 int32：proto 契约里 total 是 int32，
// 溢出时给出上限而不是回绕成负数（负数总数会让前端分页直接崩）。
func clampTotal(n int64) int32 {
	const maxInt32 = int64(^uint32(0) >> 1)
	if n > maxInt32 {
		return int32(maxInt32)
	}
	if n < 0 {
		return 0
	}
	return int32(n)
}
