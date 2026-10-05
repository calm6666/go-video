package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReleaseConnectionLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReleaseConnectionLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReleaseConnectionLeaseLogic {
	return &ReleaseConnectionLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 释放租约（正常断开/切房）
func (l *ReleaseConnectionLeaseLogic) ReleaseConnectionLease(in *rpc.ReleaseConnectionLeaseReq) (*rpc.EmptyReply, error) {
	// 已实现行为：
	// 1. 参数：lease_id 与 (room_id, mid) 至少一组，都缺则 ErrEmptyLeaseID；
	//    reason 只作审计与日志，不影响是否释放，但形状必须合法（换行会污染日志行）。
	// 2. 三元组校验：按 lease_id 取到租约后，请求里给出的 room_id/mid/conn_id 都要与登记值一致，
	//    不一致返回 ErrTripletMismatch——释放是破坏性操作，越权释放等于替别人断线。
	// 3. 幂等：租约不存在、或已处终态（RELEASED/KICKED）时按「已释放」返回成功，不报 ErrLeaseNotFound，
	//    否则客户端断线清理会永远重试；KICKED 记录保持 KICKED（终态不可回写，model.IsLeaseTerminal）。
	// 4. 级联清理（全在 Redis，LeaseStore.Delete 一次完成）：删 lease 键、删 (node|conn) 幂等键、
	//    SREM 房间集合与用户集合 → 房间连接计数随之下降，不会出现「计数被打成负数」。
	// 5. 断线宽限期：Delete 内已按 (room, mid) 写 OfflineView（ReconnectGraceSeconds），
	//    期内 RedeemReconnectTicket 能回显断开时刻与漏消息估算。
	// 6. reason=switch_room：conn 幂等键随本租约删除，同一连接可在新房间重新 Acquire，不会因键残留失败。
	// 7. 路由不随连接数归零而下线：这里完全不碰 live_gw_room_route（MySQL 零写入，README 数据分层）。
	// 8. 错误映射：成功返回 EmptyReply；参数/越权返回 model 哨兵；Redis 故障原样返回，不降级为成功。
	if in == nil {
		return nil, model.ErrEmptyLeaseID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	leaseID := strings.TrimSpace(in.GetLeaseId())
	connID := strings.TrimSpace(in.GetConnId())
	roomID, mid := in.GetRoomId(), in.GetMid()
	reason := strings.TrimSpace(in.GetReason())

	if leaseID == "" && mid <= 0 {
		// 只给 room_id 不带 mid 的影响面是「整个房间」，释放接口不接受这种范围。
		return nil, model.ErrEmptyLeaseID
	}
	if roomID < 0 {
		return nil, model.ErrInvalidRoomID
	}
	if err := g.requireMid(mid); err != nil {
		return nil, err
	}
	if leaseID == "" {
		// 按 (room_id, mid) 定位时房间号必填；纯凭 lease_id 释放时 room_id 可省略（作校验用）。
		if err := g.requireRoomID(roomID); err != nil {
			return nil, err
		}
	}
	if reason != "" {
		if err := g.requireReason(reason); err != nil {
			return nil, err
		}
	}

	// --- 定位目标租约 ---
	var targets []*repository.LeaseRecord
	if leaseID != "" {
		rec, err := g.leaseLookup(l.ctx, leaseID)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			// 键已回收 = 早已释放或从未签发：幂等成功。
			g.infof("live-gateway: ReleaseConnectionLease 未命中既有租约，按已释放返回 lease_id=%s reason=%s",
				safeIdent(leaseID), safeIdent(reason))
			return &rpc.EmptyReply{}, nil
		}
		if err := lgwReleaseTripleGuard(rec, roomID, mid, connID); err != nil {
			lgwAuditCredentialDenial(l.ctx, g, "lease-release-denied", rec.RoomID, mid, rec.LeaseID,
				model.DropPermissionDenied, in.GetTraceId())
			return nil, err
		}
		targets = []*repository.LeaseRecord{rec}
	} else {
		// (room_id, mid) 定位：该用户在该房间的全部连接（多端同时在线时一次收口）。
		recs, err := g.svcCtx.Leases.ListUserLeases(l.ctx, roomID, mid, lgwScanLimit(g))
		if err != nil {
			return nil, err
		}
		lgwSortLeasesByExpiry(recs)
		for _, rec := range recs {
			if rec == nil {
				continue
			}
			if connID != "" && rec.ConnID != connID {
				// 调用方点名了单条连接，其余连接不能被这次释放带走。
				continue
			}
			targets = append(targets, rec)
		}
		if len(targets) == 0 {
			g.infof("live-gateway: ReleaseConnectionLease room_id=%d mid=%d 无既有连接，幂等返回成功 reason=%s",
				roomID, mid, safeIdent(reason))
			return &rpc.EmptyReply{}, nil
		}
	}

	// --- 清理 ---
	for _, rec := range targets {
		if model.IsLeaseTerminal(rec.State) {
			// RELEASED：重复释放，无副作用；KICKED：终态不回写，也不删键（被踢记录要留给排障与
			// 「被踢不能复活」的判定依据，见 README 状态机）。
			g.infof("live-gateway: 租约 %s 已处终态 state=%d，释放按幂等返回", safeIdent(rec.LeaseID), rec.State)
			continue
		}
		if err := g.svcCtx.Leases.Delete(l.ctx, rec); err != nil {
			// Redis 故障必须原样返回：吞掉它会让客户端以为已经断开，而房间计数与订阅集合还留着这条连接。
			return nil, fmt.Errorf("live-gateway: release lease %s: %w", safeIdent(rec.LeaseID), err)
		}
	}
	g.infof("live-gateway: 已释放 %d 条租约 room_id=%d mid=%d reason=%s",
		len(targets), roomID, mid, safeIdent(reason))
	return &rpc.EmptyReply{}, nil
}

// lgwReleaseTripleGuard 释放前的三元组守卫：请求里给了什么就校什么。
// 未给 (room_id, mid) 时等于纯凭 lease_id 释放——lease_id 是本服务用 crypto/rand 签发的不可猜标识，
// 与其它按 lease_id 定位的入口（Get/Renew/心跳）保持同一口径。
func lgwReleaseTripleGuard(rec *repository.LeaseRecord, roomID, mid int64, connID string) error {
	if roomID > 0 && rec.RoomID != roomID {
		return fmt.Errorf("%w: lease belongs to room %d, release asked for room %d",
			model.ErrTripletMismatch, rec.RoomID, roomID)
	}
	if mid > 0 && rec.Mid != mid {
		return fmt.Errorf("%w: lease belongs to mid %d, release asked for mid %d",
			model.ErrTripletMismatch, rec.Mid, mid)
	}
	if connID != "" && rec.ConnID != connID {
		return fmt.Errorf("%w: lease belongs to conn %s, release asked for conn %s",
			model.ErrTripletMismatch, safeIdent(rec.ConnID), safeIdent(connID))
	}
	return nil
}
