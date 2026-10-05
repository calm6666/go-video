package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// qoeSampleCeilMS 观测值的统计异常上限（毫秒）：60s 以外的 rtt/lag 认定是采样异常，
// 丢掉样本但仍受理心跳。阈值口径与 proto 注释一致（rtt_ms/received_lag_ms 均为毫秒观测值），
// 且这类 QoE 观测永不参与权限判定（proto ClientInfo 注释）。
const qoeSampleCeilMS = 60_000

type ReportClientHeartbeatLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportClientHeartbeatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportClientHeartbeatLogic {
	return &ReportClientHeartbeatLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 客户端心跳簿记：推进 last_heartbeat/max_seq，抽样产出 QoE 事件
func (l *ReportClientHeartbeatLogic) ReportClientHeartbeat(in *rpc.ReportClientHeartbeatReq) (*rpc.ReportClientHeartbeatReply, error) {
	// 已实现行为：
	// 1. 参数形状：room_id>0、mid>=0、seq>=0（畸形请求返回 error）；rtt_ms/received_lag_ms 负数或
	//    超过 qoeSampleCeilMS 只丢样本、仍受理心跳（观测不参与判定）。
	// 2. 凭据：租约不存在 / 已终态 / 已过期 → accepted=false + DROP_REASON_BAD_TICKET，不返回 error
	//    （否则客户端 gRPC 重试会放大成心跳风暴）；三元组不匹配 → accepted=false +
	//    DROP_REASON_PERMISSION_DENIED，并落一行 DENIED 审计（心跳是越权最高频入口）。
	// 3. 单调性：last_heartbeat_at 只由更晚的服务端接收时刻推进；seq 只推进最大值，
	//    重传（seq<=max_seq）accepted=true 但不推进状态、不计 QoE 样本；
	//    client_time 偏差超过 HeartbeatMaxSkewSeconds 只记时钟漂移观测，绝不改写服务端时刻。
	// 4. 全部写 Redis（LeaseRecord 里的心跳计数/max_seq/最后心跳时刻），MySQL 零写入。
	// 5. QoE 事件投递（event-collector / SPM 链路）本期未接线：跳过上报，不受理失败（见服务 README）。
	// 6. 返回 server_time 与剩余 TTL，供客户端校时与及时续租。
	if in == nil {
		return nil, model.ErrEmptyLeaseID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	leaseID := strings.TrimSpace(in.GetLeaseId())
	roomID, mid, seq := in.GetRoomId(), in.GetMid(), in.GetSeq()
	if seq < 0 {
		return nil, fmt.Errorf("live-gateway: heartbeat seq must be >= 0, got %d", seq)
	}
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	if err := g.requireMid(mid); err != nil {
		return nil, err
	}
	connID := strings.TrimSpace(in.GetConnId())
	now := model.NowUnix()
	// reject 只回结论、不回 error：心跳是高频调用，把「无凭据」这类客户端问题升级成 gRPC 错误
	// 会立即放大成重试风暴（proto:248 注释即要求「票据校验失败为 false 并给原因」）。
	reject := func(drop int32, ttl int32) (*rpc.ReportClientHeartbeatReply, error) {
		return &rpc.ReportClientHeartbeatReply{
			Accepted: false, ServerTime: now, TtlSeconds: ttl, DenyReason: dropReason(drop),
		}, nil
	}
	if leaseID == "" {
		// 没有凭据标识的心跳无法定位连接：按无效凭据回绝，让客户端重连而不是静默丢弃。
		return reject(model.DropBadTicket, 0)
	}

	rec, err := g.leaseLookup(l.ctx, leaseID)
	if err != nil {
		return nil, err // 存储故障原样返回：降级成 accepted=false 会把 Redis 故障伪装成客户端掉线
	}
	if rec == nil {
		return reject(model.DropBadTicket, 0)
	}
	if !tripleMatch(rec, roomID, mid) || (connID != "" && rec.ConnID != connID) {
		lgwAuditCredentialDenial(l.ctx, g, "heartbeat-denied", rec.RoomID, mid, rec.LeaseID,
			model.DropPermissionDenied, in.GetTraceId())
		// 只记标识与结论：心跳里没有正文、也没有 IP/设备号可记（AGENTS.md §7）。
		g.errorf("live-gateway: 心跳三元组不匹配 lease_id=%s 登记=(room %d, mid %d) 请求=(room %d, mid %d)",
			safeIdent(rec.LeaseID), rec.RoomID, rec.Mid, roomID, mid)
		return reject(model.DropPermissionDenied, rec.RemainingTTL(now))
	}
	if model.IsLeaseTerminal(rec.State) || rec.EffectiveState(now) == model.LeaseStateExpired {
		// 心跳不复活租约：复活是续租/重连的职责（Renew 允许 EXPIRED→ACTIVE，终态不允许）。
		return reject(model.DropBadTicket, rec.RemainingTTL(now))
	}

	// 时钟漂移观测：只影响日志，不影响任何判定与写入的时间值。
	if skew := lgwAbs(now - in.GetClientTime()); in.GetClientTime() > 0 &&
		skew > int64(g.cfg().HeartbeatMaxSkewSeconds) {
		g.infof("live-gateway: 心跳时钟漂移 lease_id=%s conn=%s skew=%ds（client_time 不作为服务端时刻）",
			safeIdent(rec.LeaseID), safeIdent(rec.ConnID), skew)
	}

	// 重传判定：max_seq 初值是 0，客户端可以从 0 起计数，所以「从未收过心跳」不能按重传处理，
	// 否则第一条 seq=0 的心跳会被永久吞掉。
	stale := rec.HeartbeatCount > 0 && seq <= rec.MaxSeq
	if stale {
		return &rpc.ReportClientHeartbeatReply{
			Accepted: true, ServerTime: now, TtlSeconds: rec.RemainingTTL(now),
			DenyReason: dropReason(model.DropOK),
		}, nil
	}

	if seq > rec.MaxSeq {
		rec.MaxSeq = seq
	}
	rec.HeartbeatCount++
	if now > rec.LastHeartbeatAt {
		rec.LastHeartbeatAt = now
	}
	// QoE 样本（rtt/lag）的异常值只丢样本、不丢心跳。
	if rtt, lag := in.GetRttMs(), in.GetReceivedLagMs(); !lgwSamplePlausible(rtt) || !lgwSamplePlausible(lag) {
		g.infof("live-gateway: QoE 采样异常丢弃 lease_id=%s rtt=%dms lag=%dms", safeIdent(rec.LeaseID), rtt, lag)
	}
	if perr := g.svcCtx.Leases.Put(l.ctx, rec); perr != nil {
		return nil, perr
	}
	return &rpc.ReportClientHeartbeatReply{
		Accepted:   true,
		ServerTime: now,
		TtlSeconds: rec.RemainingTTL(now),
		DenyReason: dropReason(model.DropOK),
	}, nil
}

// lgwSamplePlausible 判定 QoE 观测值是否落在可采信区间内（非负、不超统计异常上限）。
func lgwSamplePlausible(v int32) bool { return v >= 0 && v <= qoeSampleCeilMS }

// lgwAbs 取绝对值（int64，漂移观测用；不引入 math 包）。
func lgwAbs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
