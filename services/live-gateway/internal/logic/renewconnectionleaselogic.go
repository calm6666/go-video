package logic

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// renewDedupPrefix 续租去重窗口用的「伪 rpc 名」。
// 走 LeaseStore.ClaimRequest 的 req 键空间（键 = req:<名字>|<标识>），标识里带 lease_id 与 Unix 秒，
// 于是「同一租约同一秒的多次续租」只生效一次。repository 里预留的 keyRenewDedup 没有任何导出访问器
// （见交付报告缺口清单），这里复用同一形状的幂等窗口而不是另起键前缀。
const renewDedupPrefix = "renew-lease"

// renewDedupTTLSeconds 续租去重键的存活秒数：只需覆盖「同一个 Unix 秒」，
// 窗口里已经编码了秒，TTL 只用于回收，不留长期键。
const renewDedupTTLSeconds = 2

type RenewConnectionLeaseLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRenewConnectionLeaseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenewConnectionLeaseLogic {
	return &RenewConnectionLeaseLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 续租（TTL 刷新）：三元组不匹配一律拒绝，不静默改绑
func (l *RenewConnectionLeaseLogic) RenewConnectionLease(in *rpc.RenewConnectionLeaseReq) (*rpc.RenewConnectionLeaseReply, error) {
	// 已实现行为：
	// 1. 参数：lease_id 非空、conn_id 非空、room_id>0、mid>=0。
	// 2. 三元组：租约不存在 → ErrLeaseNotFound（与「已过期」区分开，客户端必须重新 Acquire，
	//    服务端绝不静默签发新租约）；room_id/mid/conn_id 与登记值不符 → ErrTripletMismatch + 越权审计。
	// 3. 状态：终态不回写（KICKED 在禁止重连窗口内 → PERMISSION_DENIED，其余终态 → BAD_TICKET），
	//    EXPIRED/ACTIVE 按 model.IsValidLeaseTransition 允许推进到 ACTIVE（同一 lease_id 复活）。
	// 4. 禁止重连窗口：BanUntil(room_id, mid) 仍在生效时一律 renewed=false + PERMISSION_DENIED，
	//    非终态也不能靠续租绕过 Kick 写的封禁（封禁只挡 Acquire 等于没封）。
	// 5. 幂等：同一秒的重放只递增一次 renew_count（Redis 去重键 lease_id|秒）。
	// 6. TTL：与 Acquire 复用 gate.clampLeaseTTL（请求值 → 配额值 → 配置默认，夹进
	//    [MinLeaseTTLSeconds, MaxLeaseTTLSeconds]）。expire_at 一律重算为 now+ttl，
	//    不在旧值上累加，所以续租无法把单条租约拉长超过上限。
	// 7. 续租等价一次心跳：推进 last_heartbeat_at（只由更晚的服务端时刻推进）。
	// 8. 只写 Redis：本方法零 MySQL 写入（配额只读，且走 QuotaCacheTTLSeconds 读缓存）。
	// 9. 错误映射：renewed=false 必带非 UNSPECIFIED 的 deny_reason，成功回 DROP_REASON_OK。
	if in == nil {
		return nil, model.ErrEmptyLeaseID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	leaseID := strings.TrimSpace(in.GetLeaseId())
	if leaseID == "" {
		return nil, model.ErrEmptyLeaseID
	}
	if err := g.requireRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := g.requireMid(in.GetMid()); err != nil {
		return nil, err
	}
	connID := strings.TrimSpace(in.GetConnId())
	if connID != "" {
		if err := g.requireConnID(connID); err != nil {
			return nil, err
		}
	}

	rec, err := g.leaseLookup(l.ctx, leaseID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		// 键已被回收：与「租约还在但已过期」是两种结论，客户端处理方式不同（前者必须重新 Acquire）。
		return nil, fmt.Errorf("%w: lease_id=%s", model.ErrLeaseNotFound, safeIdent(leaseID))
	}
	now := model.NowUnix()
	if !tripleMatch(rec, in.GetRoomId(), in.GetMid()) || (connID != "" && rec.ConnID != connID) {
		lgwAuditCredentialDenial(l.ctx, g, "lease-renew-denied", rec.RoomID, in.GetMid(), rec.LeaseID,
			model.DropPermissionDenied, in.GetTraceId())
		g.errorf("live-gateway: 续租三元组不匹配 lease_id=%s 登记=(room %d, mid %d) 请求=(room %d, mid %d)",
			safeIdent(rec.LeaseID), rec.RoomID, rec.Mid, in.GetRoomId(), in.GetMid())
		return nil, model.ErrTripletMismatch
	}

	state := rec.EffectiveState(now)
	if model.IsLeaseTerminal(rec.State) {
		drop, err := lgwKickedDenyReason(l.ctx, g, rec, now)
		if err != nil {
			return nil, err
		}
		return &rpc.RenewConnectionLeaseReply{
			Renewed: false, State: leaseState(rec.State), ExpireAt: rec.ExpireAt,
			TtlSeconds: 0, DenyReason: dropReason(drop),
		}, nil
	}
	if !model.IsValidLeaseTransition(rec.State, model.LeaseStateActive) {
		// 状态机不给的边就不续（含 UNKNOWN 取值），也不回退成「安静失败」。
		g.errorf("live-gateway: 租约 %s 状态 %d 不允许推进到 ACTIVE", safeIdent(rec.LeaseID), rec.State)
		return &rpc.RenewConnectionLeaseReply{
			Renewed: false, State: leaseState(state), ExpireAt: rec.ExpireAt,
			TtlSeconds: rec.RemainingTTL(now), DenyReason: dropReason(model.DropBadTicket),
		}, nil
	}

	// 禁止重连窗口是跨房间的：Kick 写用户维度 ban 时，别的房间里的活租约也可能还在续租。
	// 不查这个窗口的话，「封禁」就只挡 Acquire，被踢者靠一条老租约的无限续租就能一直在线。
	ban, banErr := g.svcCtx.Leases.BanUntil(l.ctx, rec.RoomID, rec.Mid)
	if banErr != nil {
		if !errors.Is(banErr, model.ErrInvalidMid) {
			return nil, banErr
		}
		ban = 0 // 游客没有封禁键（repository 明确拒绝 mid<=0），不构成拦截
	} else if ban > now {
		lgwAuditCredentialDenial(l.ctx, g, "lease-renew-banned", rec.RoomID, rec.Mid, rec.LeaseID,
			model.DropPermissionDenied, in.GetTraceId())
		return &rpc.RenewConnectionLeaseReply{
			Renewed: false, State: leaseState(state), ExpireAt: rec.ExpireAt,
			TtlSeconds: 0, DenyReason: dropReason(model.DropPermissionDenied),
		}, nil
	}

	// 配额里的 lease_ttl_seconds 是运营意图，解析失败绝不降级为默认值（helpers 同一口径）。
	eff, err := g.resolveQuota(l.ctx, model.QuotaScopeRoom, rec.RoomID)
	if err != nil {
		return nil, err
	}
	ttl := g.clampLeaseTTL(in.GetTtlSeconds(), eff.LeaseTtlSeconds)

	// 同秒重放只生效一次：抢不到去重键就是重传，返回既有结论而不再累加 renew_count。
	dedupKey := rec.LeaseID + "|" + strconv.FormatInt(now, 10)
	if _, fresh, cerr := g.svcCtx.Leases.ClaimRequest(l.ctx, renewDedupPrefix, dedupKey, renewDedupTTLSeconds); cerr != nil {
		return nil, cerr
	} else if !fresh {
		return &rpc.RenewConnectionLeaseReply{
			Renewed: true, State: leaseState(state), ExpireAt: rec.ExpireAt,
			TtlSeconds: rec.RemainingTTL(now), DenyReason: dropReason(model.DropOK),
		}, nil
	}

	rec.State = model.LeaseStateActive
	rec.ExpireAt = now + int64(ttl)
	rec.RenewCount++
	if now > rec.LastHeartbeatAt {
		rec.LastHeartbeatAt = now // 续租等价一次心跳：只续租不发心跳的客户端不该被判失联
	}
	if perr := g.svcCtx.Leases.Put(l.ctx, rec); perr != nil {
		return nil, perr
	}
	return &rpc.RenewConnectionLeaseReply{
		Renewed:    true,
		State:      leaseState(rec.State),
		ExpireAt:   rec.ExpireAt,
		TtlSeconds: rec.RemainingTTL(now),
		DenyReason: dropReason(model.DropOK),
	}, nil
}

// lgwKickedDenyReason 终态租约续租被拒时的可解释原因：
// 被踢且仍在禁止重连窗口内 → PERMISSION_DENIED（客户端能算出还要等多久）；
// 其余终态（RELEASED，或 KICKED 但吊销期已过）→ BAD_TICKET（凭据失效，重新 Acquire 即可）。
func lgwKickedDenyReason(ctx context.Context, g gate, rec *repository.LeaseRecord, now int64) (int32, error) {
	if rec.State != model.LeaseStateKicked {
		return model.DropBadTicket, nil
	}
	ban, err := g.svcCtx.Leases.BanUntil(ctx, rec.RoomID, rec.Mid)
	if err != nil {
		if errors.Is(err, model.ErrInvalidMid) {
			// 游客没有封禁键（repository 明确拒绝 mid<=0），按凭据失效处理。
			return model.DropBadTicket, nil
		}
		return 0, err
	}
	if ban > now {
		return model.DropPermissionDenied, nil
	}
	return model.DropBadTicket, nil
}
