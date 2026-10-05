package logic

import (
	"context"
	"fmt"

	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListBroadcastLogsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListBroadcastLogsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListBroadcastLogsLogic {
	return &ListBroadcastLogsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询广播审计流水（按房间）
func (l *ListBroadcastLogsLogic) ListBroadcastLogs(in *rpc.ListBroadcastLogsReq) (*rpc.ListBroadcastLogsReply, error) {
	// 已实现行为（编号对应本方法原桩注释）：
	// 1. 参数：room_id 必填（ErrInvalidRoomID，model.List 对无 room_id 直接拒绝，不给全表扫的口子）；
	//    kind 只有 0 表示不过滤，越界值显式拒绝而不是当「未知类别」（未知类别查不出任何行，
	//    会被读成「这个类别没发过消息」）；sender_mid<0 拒绝（0 才是「不过滤」）；
	//    only_dropped 直接透传，过滤集合是 DROPPED/DENIED/DUPLICATED 三态（model 里已定）；
	//    分页走 g.pageParams（默认 20、上限 MaxPageSize，超限拒绝而不是截断）。
	// 2. 授权：g.requireOperatorRead("ListBroadcastLogs") —— 本方法读的是「谁在什么时候发过什么」，
	//    属运营/审核取证面。可信 OPERATOR/SERVICE 直接放行；未归因主体只有**该房间主播**可查
	//    （问 live-room 归属，问不到就 fail-closed，不把「问不到」读成「不是主播」也不软降级成空列表），
	//    其余一律 ErrPermissionDenied。
	// 3. 数据源：live_gw_broadcast_log（MySQL），排序 id DESC 由 model.List 固定；
	//    本方法零写入、零缓存（缓存会让「为什么这条没到」的追问拿到旧答案）。
	// 4. 保留期：早于 LiveGateway.BroadcastLogRetentionDays 的行可能已被 PurgeBefore 清掉。
	//    契约里 ListBroadcastLogsReply 只有 page+logs（**没有承载保留期的字段**），
	//    所以「查无记录」这件事只能落进服务端日志（含保留天数），并在 README/本注释里写明：
	//    空列表是「保留期内无记录」的真实结论，不等于「从未发送」。这是契约缺口，见交付报告。
	// 5. 投影：conv.broadcastLogInfo 原样回显 payload_digest + payload_bytes；正文永不来自本服务
	//    （表里就没有正文字段）；event_id 为空表示非事件驱动投递。
	// 6. 一致性：Redis 去重窗口命中时 model.Insert 返回 0 行、不落新行，因此这里看到的 duplicated
	//    行数会小于 Redis 观测值——设计如此（DB 是持久事实，Redis 是分钟级窗口），
	//    不为此另造一张计数器表。
	// 7. 原桩提到的 CountByRoomSince 概览**本轮未接**：响应体没有对应字段，接了就是把读数算完丢掉；
	//    需要概览时应由运营面新增契约字段，而不是本方法私自塞进 logs。
	// 8. 错误映射：ErrInvalidRoomID / ErrPermissionDenied / ErrPsTooLarge 原样返回；
	//    空结果 total=0 + 空切片（不是 null，客户端要能直接遍历）。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	roomID := in.GetRoomId()
	if err := g.requireRoomID(roomID); err != nil {
		return nil, err
	}
	kind := int32(in.GetKind())
	if kind != 0 && !model.ValidBroadcastKind(kind) {
		return nil, fmt.Errorf("%w: kind=%d (0 means no filter)", model.ErrInvalidBroadcastKind, kind)
	}
	senderMid := in.GetSenderMid()
	if senderMid < 0 {
		return nil, fmt.Errorf("%w: sender_mid=%d, 0 means no filter", model.ErrInvalidMid, senderMid)
	}
	pn, ps, err := g.pageParams(in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}

	// --- 2. 授权门禁（先于任何读取）---
	c, err := g.requireOperatorRead("ListBroadcastLogs")
	if err != nil {
		return nil, err
	}
	if !c.isPrivileged() {
		if c.mid <= 0 {
			return nil, fmt.Errorf("%w: ListBroadcastLogs requires an attested operator or the room owner",
				model.ErrPermissionDenied)
		}
		owner, oerr := g.svcCtx.Rooms.IsOwner(l.ctx, roomID, c.mid)
		if oerr != nil {
			// 含 ErrLiveRoomNotConfigured：归属问不到真值就不放行。
			return nil, oerr
		}
		if !owner {
			lgwAuditCredentialDenial(l.ctx, g, "broadcast-logs-read-denied", roomID, c.mid,
				fmt.Sprintf("room%d-mid%d", roomID, c.mid), model.DropPermissionDenied, "")
			g.errorf("live-gateway: 非归属主体尝试读取广播审计 room_id=%d caller_mid=%d", roomID, c.mid)
			return nil, fmt.Errorf("%w: mid %d is not the owner of room %d, broadcast logs require attested operator",
				model.ErrPermissionDenied, c.mid, roomID)
		}
	}

	store, serr := g.svcCtx.RequireStore()
	if serr != nil {
		return nil, serr
	}
	rows, total, err := store.BroadcastLogs.List(l.ctx, model.BroadcastLogFilter{
		RoomId:      roomID,
		Kind:        kind,
		SenderMid:   senderMid,
		OnlyDropped: in.GetOnlyDropped(),
		Pn:          pn,
		Ps:          ps,
		MaxPageSize: g.cfg().MaxPageSize,
	})
	if err != nil {
		return nil, err
	}

	logs := make([]*rpc.BroadcastLogInfo, 0, len(rows))
	for _, row := range rows {
		logs = append(logs, broadcastLogInfo(row))
	}
	if total == 0 {
		// 契约无保留期字段（见要点 4），这条日志是运营唯一能拿到的「空不等于从未发送」的证据。
		g.infof("live-gateway: room_id=%d 保留期内无广播审计（保留 %d 天，更早的行可能已被清理）"+
			" kind=%d sender_mid=%d only_dropped=%t", roomID, g.cfg().BroadcastLogRetentionDays,
			kind, senderMid, in.GetOnlyDropped())
	}
	return &rpc.ListBroadcastLogsReply{Page: pageResult(total), Logs: logs}, nil
}
