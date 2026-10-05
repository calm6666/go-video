package logic

import (
	"context"
	"fmt"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRoomConnectionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRoomConnectionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRoomConnectionsLogic {
	return &ListRoomConnectionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 房间在线连接列表（Redis 视图，运营排障与主播工具）
func (l *ListRoomConnectionsLogic) ListRoomConnections(in *rpc.ListRoomConnectionsReq) (*rpc.ListRoomConnectionsReply, error) {
	// 实现要点（编号对应本方法原桩注释的 7 条）：
	// 1. 参数：room_id>0（ErrInvalidRoomID）；mid==0 表示整个房间，mid<0 按畸形入参拒绝
	//    （负值不是「不过滤」，把它静默读成整房枚举会把影响面放大到最大）；
	//    role 只有 UNSPECIFIED(0) 表示不过滤，越界值直接拒绝而不是 NormalizeRole 成 VIEWER
	//    （否则「查主播」会被悄悄改写成「查观众」）；分页走 helpers 的 g.pageParams。
	// 2. 调用方授权先于任何扫描：可信 OPERATOR/SERVICE 可整房枚举；未归因主体只能查
	//    「单个 mid 在本房间的连接」（主播工具自查路径），且必须问 live-room 归属，问不到就 fail-closed。
	//    观众名单是隐私数据，不做「返回空列表」的软降级。
	// 3. 数据源只有 Redis 订阅集合 + 租约键（Leases.ListRoomLeases / ListUserLeases），MySQL 零读取；
	//    扫描条数到 ConnectionScanLimit 就报 ErrScanLimitTooLarge，绝不截断当成全量
	//    （截断会让运营读成「这房间只剩这么多连接」）。
	// 4. snapshot_from_cache 恒 false：本服务没有进程内租约快照能力，
	//    Redis 故障时原样返回 error —— 返回「空列表 + true」是伪造结论（proto 头部「绝不伪造」）。
	// 5. page.total 与 leases 同口径（同一批过滤后的条数），不用 RoomConnectionCount 的近似值混填。
	// 6. 投影 leaseInfo(rec, "")：reconnect_ticket 恒空（列表里发票据 = 批量发放重连凭据），
	//    输出里没有任何内部 Redis 键；LeaseInfo 的 role 是服务端判定值而不是客户端自报值。
	// 7. 房间是否存在/是否可广播由 live-room 判定，本服务不复判房间状态；越权回 ErrPermissionDenied。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID, mid := in.GetRoomId(), in.GetMid()
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if mid < 0 {
		return nil, fmt.Errorf("%w: mid=%d, connection list accepts mid >= 0 (0 means whole room)", model.ErrInvalidMid, mid)
	}
	filterRole := int32(in.GetRole())
	if !model.ValidRole(filterRole) {
		return nil, fmt.Errorf("live-gateway: role filter %d is not a valid ConnRole", filterRole)
	}
	pn, ps, err := g.pageParams(in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}

	// --- 2. 授权门禁（先于扫描）---
	c, err := g.requireOperatorRead("ListRoomConnections")
	if err != nil {
		return nil, err
	}
	if !c.isPrivileged() {
		if mid <= 0 {
			return nil, fmt.Errorf("%w: unattested caller cannot enumerate a whole room's connections (mid must be > 0)",
				model.ErrPermissionDenied)
		}
		owner, oerr := g.svcCtx.Rooms.IsOwner(l.ctx, roomID, mid)
		if oerr != nil {
			// 含 ErrLiveRoomNotConfigured：归属问不到真值就不放行，「问不到」不等于「是主播」。
			return nil, oerr
		}
		if !owner {
			lgwAuditCredentialDenial(l.ctx, g, "connections-read-denied", roomID, mid,
				fmt.Sprintf("room%d-mid%d", roomID, mid), model.DropPermissionDenied, "")
			g.errorf("live-gateway: 未归因主体尝试枚举在线名单 room_id=%d mid=%d", roomID, mid)
			return nil, fmt.Errorf("%w: mid %d is not the owner of room %d, room connection list requires attested operator",
				model.ErrPermissionDenied, mid, roomID)
		}
	}

	// --- 3. 读 Redis 快照 ---
	limit := lgwScanLimit(g)
	var recs []*repository.LeaseRecord
	if mid > 0 {
		recs, err = g.svcCtx.Leases.ListUserLeases(l.ctx, roomID, mid, limit)
		if err != nil {
			return nil, err
		}
		// ListUserLeases 不回传 truncated 标记（repository 缺口），只能按「取满即疑似截断」保守判定。
		if lgwPossiblyTruncated(int32(len(recs)), limit) {
			return nil, scanLimitError(roomID, mid, limit)
		}
	} else {
		var scanned int32
		var truncated bool
		recs, scanned, truncated, err = g.svcCtx.Leases.ListRoomLeases(l.ctx, roomID, limit)
		_ = scanned // 扫描条数只用于诊断，不改变「是否截断」的判定口径
		if err != nil {
			return nil, err
		}
		if truncated {
			return nil, scanLimitError(roomID, 0, limit)
		}
	}

	// --- 过滤：终态记录不在线；角色按需收窄 ---
	now := model.NowUnix()
	matched := make([]*repository.LeaseRecord, 0, len(recs))
	for _, rec := range recs {
		if rec == nil || rec.RoomID != roomID {
			continue // 陈旧集合成员（记录已换绑别的房间）不计入本房间视图
		}
		if model.IsLeaseTerminal(rec.State) {
			continue // RELEASED/KICKED 不在线：KICKED 墓碑留给 GetConnectionLease 与 Kick 复用
		}
		// EXPIRED（宽限期内的断线）保留在列表里：那正是排障要看的状态，
		// LeaseInfo.state 与 ttl_seconds 会如实标出，page.total 与 leases 保持同一批口径。
		if filterRole != model.RoleUnspecified && model.NormalizeRole(rec.Role) != filterRole {
			continue
		}
		matched = append(matched, rec)
	}

	// --- 5/6. 稳定排序后分页投影 ---
	lgwSortLeasesByExpiry(matched)
	total := int32(len(matched))
	// 排障用的在线数分离：total 含宽限期内的 EXPIRED，active 只含此刻有效的连接。
	g.infof("live-gateway: 房间 %d 在线快照 room_total=%d active=%d page=%d/%d",
		roomID, total, lgwEffectiveCount(matched, now), pn, ps)
	start := (pn - 1) * ps
	if start > total {
		start = total
	}
	end := start + ps
	if end > total {
		end = total
	}
	leases := make([]*rpc.LeaseInfo, 0, end-start)
	for _, rec := range matched[start:end] {
		leases = append(leases, leaseInfo(rec, ""))
	}
	return &rpc.ListRoomConnectionsReply{
		Page: pageResult(total),
		// snapshot_from_cache 保持零值 false：见实现要点 4。
		Leases: leases,
	}, nil
}

// lgwPossiblyTruncated 取满 limit 即视为疑似截断：宁可让调用方缩小范围重查，
// 也不能把「只扫到这么多」当成「就只有这么多」。
func lgwPossiblyTruncated(n, limit int32) bool { return limit > 0 && n >= limit }

// scanLimitError 扫描到上限的统一结论（ErrScanLimitTooLarge 让运营看到真因，而不是拿到半份名单）。
func scanLimitError(roomID, mid int64, limit int32) error {
	return fmt.Errorf("%w: room_id=%d mid=%d scanned up to ConnectionScanLimit=%d, narrow the query",
		model.ErrScanLimitTooLarge, roomID, mid, limit)
}
