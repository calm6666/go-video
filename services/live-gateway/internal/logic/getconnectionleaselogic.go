package logic

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件同时承载租约读路径的包内共用构件（lgwScanLimit / lgwActiveCount / lgwAuditCredentialDenial）：
// helpers.go 是 22 个方法的公共件、不在本轮改动范围内，所以这几个只被连接租约族用到的构件落在这里。
// 命名统一带 lgw 前缀，避免与并行实现的同包文件撞名。

// defaultConnectionScanLimit Redis 集合扫描条数兜底。
// 与 config.LiveGatewayConf.ConnectionScanLimit 的 default 标签同口径：
// 配置写成 0/负数时既不能让扫描无界（一个大房间能把 Redis 单线程拖住），也不能什么都不扫。
const defaultConnectionScanLimit int32 = 500

// lgwScanLimit 取连接集合的扫描上限（ListRoomLeases / ListUserLeases 的 limit）。
func lgwScanLimit(g gate) int32 {
	if v := g.cfg().ConnectionScanLimit; v > 0 {
		return v
	}
	return defaultConnectionScanLimit
}

// lgwEffectiveCount 数出「此刻还算有效」的连接条数（只认 ACTIVE，EXPIRED/终态都不算在线）。
func lgwEffectiveCount(recs []*repository.LeaseRecord, now int64) int32 {
	var n int32
	for _, rec := range recs {
		if rec != nil && rec.EffectiveState(now) == model.LeaseStateActive {
			n++
		}
	}
	return n
}

// lgwSortLeasesByExpiry 按 expire_at 倒序、lease_id 升序稳定排序。
// Redis SSCAN 不保证跨次调用的顺序，不排序的话同一页请求两次可能给出不同成员（翻页会重复/漏）。
func lgwSortLeasesByExpiry(recs []*repository.LeaseRecord) {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i] == nil {
			return false
		}
		if recs[j] == nil {
			return true
		}
		if recs[i].ExpireAt != recs[j].ExpireAt {
			return recs[i].ExpireAt > recs[j].ExpireAt
		}
		return recs[i].LeaseID < recs[j].LeaseID
	})
}

// lgwAuditCredentialDenial 把一次「凭据三元组不匹配」落进 live_gw_broadcast_log（state=DENIED）。
//
// message_id 用 "<场景>:<lease_id>:<Unix 秒>"：租约 ID 由本服务签发、不可猜，所以一个
// (场景, 连接, 秒) 最多只有一行，重放撞 uniq_room_message 后 Insert 直接返回 (0,nil)。
// 不这么做的话，一个坏客户端的心跳风暴会变成 MySQL 写入风暴（心跳明细本身是禁止落库的易失数据）。
// kind 取 MODERATION：三元组不匹配是风控信号，不是普通系统通知。
// 返回 error 只用于「调用方要不要记一条告警」，绝不把业务结论改成错误——审计写不进去
// 不能让越权探测看起来像成功，也不能让它看起来像依赖故障。
func lgwAuditCredentialDenial(ctx context.Context, g gate, scene string, roomID, mid int64,
	leaseID string, drop int32, traceID string) {
	if roomID <= 0 {
		return // 房间号都不合法的输入没有审计价值，写进去只会污染运营视图
	}
	if drop == 0 {
		drop = model.DropPermissionDenied
	}
	err := g.writeAudit(ctx, auditParams{
		RoomID:     roomID,
		Kind:       model.KindModeration,
		MessageID:  fmt.Sprintf("%s:%s:%d", scene, safeIdent(leaseID), model.NowUnix()),
		SenderMid:  mid,
		SenderRole: model.RoleViewer,
		State:      model.BroadcastLogDenied,
		Drop:       drop,
		Source:     "live-gateway/lease",
		TraceID:    traceID,
	})
	if err != nil {
		// 审计不可用只记日志：越权结论本身已经给出，不因依赖故障改变对客户端的答复。
		g.errorf("live-gateway: %s 越权审计写入失败 room_id=%d: %v", scene, roomID, err)
	}
}

type GetConnectionLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetConnectionLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetConnectionLeaseLogic {
	return &GetConnectionLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询租约（下发前的权限校验入口之一）
func (l *GetConnectionLeaseLogic) GetConnectionLease(in *rpc.ConnectionLeaseReq) (*rpc.ConnectionLeaseReply, error) {
	// 将来行为（已实现，见下）：
	// 1. 两种定位方式（契约已固定）：lease_id 非空按 ID 精确取；否则按 (room_id, mid) 取该用户
	//    在该房间的连接（多端同时在线场景）。两者都缺 → ErrEmptyLeaseID；room_id<=0 → ErrInvalidRoomID。
	// 2. 只读、无副作用：不续租、不刷新 TTL、不写任何键。
	// 3. 数据源只有 Redis；Redis 故障原样返回 error —— 把依赖故障翻译成 found=false 会被广播路径
	//    读成「这个房间确实没人」，那是假结论（proto 头部的「绝不伪造权限」）。
	// 4. 三元组：lease_id 命中但请求带了不一致的 room_id/mid → ErrTripletMismatch，不回显内容。
	// 5. 投影 reconnect_ticket 恒为空：票据是签名凭据，Get 是只读口，回显等于代他人重连，
	//    也违反「不泄露签名/密钥材料」。
	// 6. connection_count 是 (room_id, mid) 维度的有效连接数（多端同时在线）。
	if in == nil {
		return nil, model.ErrEmptyLeaseID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	leaseID := strings.TrimSpace(in.GetLeaseId())
	roomID, mid := in.GetRoomId(), in.GetMid()
	now := model.NowUnix()

	// --- 定位 ---
	if leaseID == "" {
		if mid <= 0 {
			// 既没有 lease_id 也没有 mid：影响面会是「整个房间」，只读口也不给这种枚举入口。
			return nil, model.ErrEmptyLeaseID
		}
		if err := g.requireRoomID(roomID); err != nil {
			return nil, err
		}
		if err := g.requireMid(mid); err != nil {
			return nil, err
		}
		recs, err := g.svcCtx.Leases.ListUserLeases(l.ctx, roomID, mid, lgwScanLimit(g))
		if err != nil {
			return nil, err
		}
		lgwSortLeasesByExpiry(recs)
		var active *repository.LeaseRecord
		for _, rec := range recs {
			if rec != nil && rec.EffectiveState(now) == model.LeaseStateActive {
				active = rec
				break
			}
		}
		if active == nil {
			// 「该用户在该房间是否有连接」= 否（过期/终态记录都不算在线）。
			return &rpc.ConnectionLeaseReply{Found: false, ConnectionCount: 0}, nil
		}
		return &rpc.ConnectionLeaseReply{
			Found:           true,
			Lease:           leaseInfo(active, ""),
			ConnectionCount: lgwEffectiveCount(recs, now),
		}, nil
	}

	rec, err := g.leaseLookup(l.ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return &rpc.ConnectionLeaseReply{Found: false, ConnectionCount: 0}, nil
	}
	// ConnectionLeaseReq 契约里没有 trace_id 字段（proto 的 4 个字段见 ConnectionLeaseReq），
	// 审计行的 trace_id 留空，由 gRPC trace 兜住。
	if roomID > 0 && rec.RoomID != roomID {
		lgwAuditCredentialDenial(l.ctx, g, "lease-get-denied", rec.RoomID, mid, rec.LeaseID,
			model.DropPermissionDenied, "")
		return nil, fmt.Errorf("%w: lease belongs to room %d, queried with room %d",
			model.ErrTripletMismatch, rec.RoomID, roomID)
	}
	if mid > 0 && rec.Mid != mid {
		lgwAuditCredentialDenial(l.ctx, g, "lease-get-denied", rec.RoomID, mid, rec.LeaseID,
			model.DropPermissionDenied, "")
		return nil, fmt.Errorf("%w: lease belongs to mid %d, queried with mid %d",
			model.ErrTripletMismatch, rec.Mid, mid)
	}

	// --- (room, mid) 维度连接数：游客没有 (room,mid) 索引，本次命中的这条就是全部 ---
	count := int32(0)
	if rec.Mid > 0 {
		recs, cerr := g.svcCtx.Leases.ListUserLeases(l.ctx, rec.RoomID, rec.Mid, lgwScanLimit(g))
		if cerr != nil {
			return nil, cerr
		}
		count = lgwEffectiveCount(recs, now)
	} else if rec.EffectiveState(now) == model.LeaseStateActive {
		count = 1
	}
	return &rpc.ConnectionLeaseReply{
		Found:           true,
		Lease:           leaseInfo(rec, ""),
		ConnectionCount: count,
	}, nil
}
