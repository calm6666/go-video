package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖 ListBroadcastLogs —— 「为什么这条没到」的唯一取证入口。
// 读接口的风险不在读错，而在两件相反的事：越权读到别人的房间流水，
// 以及为了「方便」在读路径上偷偷写（缓存、计数、审计），把只读审计变成事实源。

// errLogsListProbe 只读探针：注入到 List 上，用来证明「授权/参数阶段根本没读表」。
var errLogsListProbe = errors.New("broadcast-logs-list-probe")

// secretLogText 只作为载荷正文的替身出现在这里：审计面唯一该露出的是它的摘要与字节数。
const secretLogText = "私信级敏感正文-只存在于发送者内存里"

// assertNoWrites 证明这次调用真的没落任何持久化：审计行、配额行、路由行三个写入口都必须是零。
// 为什么连 Quotas/Routes 也一起查：这几个 logic 文件共用同一套替身，读接口一旦「顺手」写缓存/写计数，
// 就把只读取证变成了事实源，而这类漂移在单个表的断言里看不出来。
func assertNoWrites(t *testing.T, e *testEnv) {
	t.Helper()
	if n := e.Logs.inserts; n != 0 {
		t.Fatalf("不该向 live_gw_broadcast_log 写入，实际 Insert %d 次", n)
	}
	if n := e.Quotas.creates + e.Quotas.updates; n != 0 {
		t.Fatalf("不该向 live_gw_access_quota 写入，实际写 %d 次", n)
	}
	if n := e.Routes.registers; n != 0 {
		t.Fatalf("不该向 live_gw_room_route 写入，实际 Register %d 次", n)
	}
}

// seedLogRows 按 id 升序追加审计行（model.List 的排序契约是 id 倒序，插入顺序即时间顺序）。
func seedLogRows(t *testing.T, e *testEnv, n int, mut func(i int, row *model.LiveGwBroadcastLog)) {
	t.Helper()
	for i := 0; i < n; i++ {
		row := &model.LiveGwBroadcastLog{
			Id:            int64(len(e.Logs.rows) + 1),
			MessageId:     fmt.Sprintf("log-%03d", i+1),
			RoomId:        roomID,
			Kind:          model.KindDanmaku,
			SenderMid:     mid,
			SenderRole:    model.RoleViewer,
			State:         model.BroadcastLogSent,
			DropReason:    model.DropOK,
			SourceService: "test",
			Ctime:         testBaseTime + int64(i),
		}
		if mut != nil {
			mut(i, row)
		}
		e.Logs.rows = append(e.Logs.rows, row)
	}
}

// --- 参数门禁 ---

func TestListBroadcastLogsParameterGate(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.ListBroadcastLogsReq
		want error
	}{
		{"空请求", nil, model.ErrInvalidRoomID},
		{"房间号为 0", &rpc.ListBroadcastLogsReq{RoomId: 0}, model.ErrInvalidRoomID},
		{"房间号为负", &rpc.ListBroadcastLogsReq{RoomId: -7}, model.ErrInvalidRoomID},
		// kind=0 是「不过滤」，越界值必须显式拒：当成未知类别查会返回空列表，
		// 被运营读成「这个类别从没发过消息」。
		{"kind 越界", &rpc.ListBroadcastLogsReq{RoomId: roomID, Kind: rpc.BroadcastKind(77)}, model.ErrInvalidBroadcastKind},
		{"kind 为负", &rpc.ListBroadcastLogsReq{RoomId: roomID, Kind: rpc.BroadcastKind(-1)}, model.ErrInvalidBroadcastKind},
		{"sender_mid 为负", &rpc.ListBroadcastLogsReq{RoomId: roomID, SenderMid: -1}, model.ErrInvalidMid},
		{"ps 超上限", &rpc.ListBroadcastLogsReq{RoomId: roomID, Page: &rpc.PageParam{Pn: 1, Ps: 51}}, model.ErrPsTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedLogRows(t, e, 2, nil)
			// 读表探针：参数阶段就返回的，绝不能已经发起查询（越界 kind 尤其危险，它会让全表扫变成「查不出东西」）。
			e.Logs.fail("List", errLogsListProbe)
			got, err := NewListBroadcastLogsLogic(ctxOperator(t, "1001"), e.Svc).ListBroadcastLogs(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("期望 %v，实际 err=%v reply=%+v", tc.want, err, got)
			}
			if got != nil {
				t.Fatalf("参数被拒不返回响应体: %+v", got)
			}
			assertNoWrites(t, e)
		})
	}
}

// --- 授权 ---

func TestListBroadcastLogsAuthorization(t *testing.T) {
	cases := []struct {
		name     string
		ctx      context.Context
		require  bool
		wantdeny bool
	}{
		{"归因运营", ctxOperator(t, "1001"), false, false},
		{"归因内部服务", ctxService(t, "moderation"), false, false},
		{"归因运营但不带人类 mid", ctxOperator(t, ""), false, false},
		// RequireAttestedOperator=false 只是放开 requireOperatorRead 这一层；
		// 本方法还有一层「非可信主体必须是该房主播」的门禁，未归因caller 的 mid 被 callerFrom 归零 → 必拒。
		{"未归因且开关关闭", ctxClient(t), false, true},
		{"未归因且开关打开", ctxClient(t), true, true},
		{"自报主播 metadata", ctxAs(t, "true", "ANCHOR", "5001", ""), false, true},
		{"归因运营但 mid 为 0", ctxAs(t, "true", "OPERATOR", "0", ""), false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedLogRows(t, e, 3, nil)
			e.apply(func(c *config.LiveGatewayConf) { c.RequireAttestedOperator = tc.require })
			if tc.wantdeny {
				e.Logs.fail("List", errLogsListProbe)
			}
			reply, err := NewListBroadcastLogsLogic(tc.ctx, e.Svc).ListBroadcastLogs(
				&rpc.ListBroadcastLogsReq{RoomId: roomID})
			if tc.wantdeny {
				if !errors.Is(err, model.ErrPermissionDenied) {
					t.Fatalf("期望 ErrPermissionDenied，实际 err=%v reply=%+v", err, reply)
				}
				// 探针错误没出现 ⇒ 授权先于任何一次读表。
				if errors.Is(err, errLogsListProbe) {
					t.Fatalf("授权拒出前不该读表")
				}
				assertNoWrites(t, e)
				if n := e.Leases.callCount("CacheGet"); n != 0 {
					t.Fatalf("只读接口被拒时不该碰缓存，CacheGet %d 次", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("可信主体应放行: %v", err)
			}
			if reply.GetPage().GetTotal() != 3 {
				t.Fatalf("期望 total=3，实际 %d", reply.GetPage().GetTotal())
			}
		})
	}
}

// TestListBroadcastLogsRoomOwnerReadPathIsReachable 记录一条走不到的分支：
// listbroadcastlogslogic.go:81-98 写了「未归因主体若是该房主播则可查自家流水」，
// 但 caller.go:87-96 把 ANCHOR 这类角色声明按未归因处理（default 分支清 attested，
// 随后 mid 归零，自报 mid 不作为授权输入），于是 c.mid<=0 恒成立，
// IsOwner 与那条「非归属主体读审计」的 DENIED 审计都成了死代码。
// 正确口径：主播读自己房间的审计需要一个能携带可信 mid 的归因通道（例如 attested=ANCHOR 被承认，
// 或由接入层用另一种可验证方式声明 mid），否则注释里的放开条件应该删掉。
// 归因通道本身今天还不存在（全仓无人注入 x-gw-caller-*），故登记在 README 已知缺口 14 而不是就地发明。
func TestListBroadcastLogsRoomOwnerReadPathIsReachable(t *testing.T) {
	t.Skip("缺陷已上报: services/live-gateway/internal/logic/listbroadcastlogslogic.go:81-98 + caller.go:87-96" +
		" —— 主播读路径不可达（缺可携带可信 mid 的归因通道）")
	e := newTestEnv(t)
	seedLogRows(t, e, 2, nil)
	e.Rooms.owner = anchorMid
	reply, err := NewListBroadcastLogsLogic(ctxAs(t, "true", "ANCHOR", "5001", ""), e.Svc).ListBroadcastLogs(
		&rpc.ListBroadcastLogsReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("归属主播查自家流水应放行: err=%v reply=%+v", err, reply)
	}
	if reply.GetPage().GetTotal() != 2 {
		t.Fatalf("应读到 2 行，实际 %+v", reply.GetPage())
	}
}

// --- 过滤、投影与排序 ---

func TestListBroadcastLogsFiltersAndProjection(t *testing.T) {
	e := newTestEnv(t)
	seedLogRows(t, e, 5, func(i int, row *model.LiveGwBroadcastLog) {
		switch i {
		case 1: // 一条被房间层限流丢掉的弹幕
			row.State = model.BroadcastLogDropped
			row.DropReason = model.DropRateLimited
			row.MessageId = "log-dropped"
		case 2: // 一条越权被拒的系统消息（另一个发送者）
			row.Kind = model.KindModeration
			row.State = model.BroadcastLogDenied
			row.DropReason = model.DropPermissionDenied
			row.SenderMid = otherMid
			row.SenderRole = model.RoleViewer
			row.MessageId = "log-denied"
		case 3: // 一条带 event_id 的事件投递
			row.Kind = model.KindRoomState
			row.EventId = "evt-9"
			row.MessageId = "evt:evt-9"
			row.PayloadDigest = payloadDigest([]byte(secretLogText), 32)
			row.PayloadBytes = int32(len(secretLogText))
		}
	})
	logic := NewListBroadcastLogsLogic(ctxOperator(t, "1001"), e.Svc)
	base := func() *rpc.ListBroadcastLogsReq { return &rpc.ListBroadcastLogsReq{RoomId: roomID} }

	t.Run("不过滤时按 id 倒序返回全部", func(t *testing.T) {
		reply, err := logic.ListBroadcastLogs(base())
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if reply.GetPage().GetTotal() != 5 {
			t.Fatalf("total 应是过滤后的全部 5，实际 %d", reply.GetPage().GetTotal())
		}
		got := reply.GetLogs()
		if len(got) != 5 {
			t.Fatalf("应返回 5 行，实际 %d", len(got))
		}
		if got[0].GetId() != 5 || got[4].GetId() != 1 {
			t.Fatalf("排序必须是 id 倒序（最新在前），实际首 %d 末 %d", got[0].GetId(), got[4].GetId())
		}
	})

	t.Run("kind 过滤", func(t *testing.T) {
		reply, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{
			RoomId: roomID, Kind: rpc.BroadcastKind_BROADCAST_KIND_MODERATION})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if reply.GetPage().GetTotal() != 1 || reply.GetLogs()[0].GetSenderMid() != otherMid {
			t.Fatalf("kind 过滤漏了行: %+v", reply.GetLogs())
		}
	})

	t.Run("sender_mid 过滤", func(t *testing.T) {
		reply, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{RoomId: roomID, SenderMid: mid})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		// 5 行里只有第 3 行不是 mid。
		if reply.GetPage().GetTotal() != 4 {
			t.Fatalf("sender_mid=%d 应查得 4 行，实际 %d", mid, reply.GetPage().GetTotal())
		}
	})

	t.Run("only_dropped 覆盖三种非 SENT 态", func(t *testing.T) {
		reply, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{RoomId: roomID, OnlyDropped: true})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		// model 侧的过滤条件是 state != SENT，因此 DROPPED/DENIED/DUPLICATED 都在集合里。
		if reply.GetPage().GetTotal() != 2 {
			t.Fatalf("期望 2 行非 SENT，实际 %d", reply.GetPage().GetTotal())
		}
		for _, row := range reply.GetLogs() {
			if row.GetState() == model.BroadcastLogSent {
				t.Fatalf("only_dropped 结果里混进了 SENT 行: %+v", row)
			}
		}
	})

	t.Run("投影只回摘要与字节数", func(t *testing.T) {
		reply, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{RoomId: roomID})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		var eventRow *rpc.BroadcastLogInfo
		for _, row := range reply.GetLogs() {
			if row.GetEventId() == "evt-9" {
				eventRow = row
			}
			// 载荷正文只能来自本服务之外：整条投影里出现敏感正文即脱敏失效。
			if strings.Contains(row.String(), secretLogText) {
				t.Fatalf("审计投影泄露了载荷正文: %s", row.String())
			}
		}
		if eventRow == nil {
			t.Fatalf("找不到事件投递行: %+v", reply.GetLogs())
		}
		want := payloadDigest([]byte(secretLogText), 32)
		if eventRow.GetPayloadDigest() != want || eventRow.GetPayloadBytes() != int32(len(secretLogText)) {
			t.Fatalf("事件行必须原样回显摘要与字节数: digest=%q bytes=%d，期望 digest=%q",
				eventRow.GetPayloadDigest(), eventRow.GetPayloadBytes(), want)
		}
		if len(want) != 32 || strings.Contains(eventRow.GetPayloadDigest(), secretLogText) {
			t.Fatalf("摘要必须是收敛后的定长十六进制串，不能是可还原正文的形式: %q", eventRow.GetPayloadDigest())
		}
		if eventRow.GetMessageId() != "evt:evt-9" {
			t.Fatalf("事件投递的幂等键必须是 evt:<event_id>，否则同一事件在两张账上对不上: %q", eventRow.GetMessageId())
		}
	})

	t.Run("空结果返回空切片而不是 null", func(t *testing.T) {
		reply, err := NewListBroadcastLogsLogic(ctxOperator(t, "1001"), e.Svc).ListBroadcastLogs(
			&rpc.ListBroadcastLogsReq{RoomId: otherRoom})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if reply.GetLogs() == nil {
			t.Fatalf("空结论必须是可遍历的空切片（客户端直接 for 循环）")
		}
		if reply.GetPage().GetTotal() != 0 || len(reply.GetLogs()) != 0 {
			t.Fatalf("期望 total=0 且无行，实际 %+v", reply)
		}
	})

	t.Run("按房间隔离", func(t *testing.T) {
		// 别的房间即使有同名 message_id 也查不到：审计定位键是 (room_id, message_id)。
		e.Logs.rows = append(e.Logs.rows, &model.LiveGwBroadcastLog{
			Id: 99, MessageId: "log-001", RoomId: otherRoom, Kind: model.KindDanmaku,
			State: model.BroadcastLogSent, DropReason: model.DropOK, Ctime: testBaseTime,
		})
		reply, err := NewListBroadcastLogsLogic(ctxOperator(t, "1001"), e.Svc).ListBroadcastLogs(base())
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if reply.GetPage().GetTotal() != 5 {
			t.Fatalf("串到了别的房间的流水：total=%d", reply.GetPage().GetTotal())
		}
	})
}

func TestListBroadcastLogsPagination(t *testing.T) {
	e := newTestEnv(t)
	seedLogRows(t, e, 25, nil)
	logic := NewListBroadcastLogsLogic(ctxOperator(t, "1001"), e.Svc)

	t.Run("ps 缺省走默认 20", func(t *testing.T) {
		reply, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{RoomId: roomID})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if reply.GetPage().GetTotal() != 25 || len(reply.GetLogs()) != 20 {
			t.Fatalf("默认页大小应为 20（total 仍是 25）: total=%d rows=%d",
				reply.GetPage().GetTotal(), len(reply.GetLogs()))
		}
	})

	t.Run("翻页可重放且越界页返回空而不是报错", func(t *testing.T) {
		// 25 行 / ps=20：第 2 页恰好是剩下的 5 行，第 3 页越界。
		second, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{
			RoomId: roomID, Page: &rpc.PageParam{Pn: 2, Ps: 20}})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if second.GetPage().GetTotal() != 25 || len(second.GetLogs()) != 5 {
			t.Fatalf("第 2 页应有 5 行，实际 total=%d rows=%d", second.GetPage().GetTotal(), len(second.GetLogs()))
		}
		// total 恒等于过滤后的总行数（不是本页行数），否则客户端算不出「还有没有下一页」。
		if second.GetLogs()[0].GetId() != 5 {
			t.Fatalf("第 2 页首行应是 id=5（id 倒序翻页必须可重放），实际 %+v", second.GetLogs()[0])
		}
		third, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{
			RoomId: roomID, Page: &rpc.PageParam{Pn: 3, Ps: 20}})
		if err != nil {
			t.Fatalf("越界页不该报错: %v", err)
		}
		if third.GetPage().GetTotal() != 25 || len(third.GetLogs()) != 0 {
			t.Fatalf("越界页应返回空切片且 total 不变: total=%d rows=%d",
				third.GetPage().GetTotal(), len(third.GetLogs()))
		}
		if third.GetLogs() == nil {
			t.Fatalf("越界页也得给可遍历的空切片")
		}
	})

	t.Run("pn 为 0 归 1", func(t *testing.T) {
		reply, err := logic.ListBroadcastLogs(&rpc.ListBroadcastLogsReq{
			RoomId: roomID, Page: &rpc.PageParam{Pn: 0, Ps: 10}})
		if err != nil {
			t.Fatalf("不该失败: %v", err)
		}
		if len(reply.GetLogs()) != 10 || reply.GetLogs()[0].GetId() != 25 {
			t.Fatalf("pn<1 必须按第 1 页处理，实际 %+v", reply.GetLogs()[0])
		}
	})
}

// --- 只读保证 ---

func TestListBroadcastLogsIsPureRead(t *testing.T) {
	e := newTestEnv(t)
	seedLogRows(t, e, 3, nil)
	before := len(e.Logs.all())
	if _, err := NewListBroadcastLogsLogic(ctxOperator(t, "1001"), e.Svc).ListBroadcastLogs(
		&rpc.ListBroadcastLogsReq{RoomId: roomID}); err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	// 「为什么这条没到」的追问不能被缓存答成旧账：本方法必须零写入、零缓存。
	assertNoWrites(t, e)
	if len(e.Logs.all()) != before {
		t.Fatalf("读路径写进了审计表")
	}
	for _, m := range []string{"CacheGet", "CacheSet", "CacheDel", "QuotaEpoch", "AllowRate", "ClaimMessage"} {
		if n := e.Leases.callCount(m); n != 0 {
			t.Fatalf("只读接口不该调用 %s，实际 %d 次", m, n)
		}
	}
}

func TestListBroadcastLogsWithoutStore(t *testing.T) {
	e := newTestEnv(t)
	e.markStoreMissing()
	// 授权先行：未归因主体即使没有存储也拿到同一个结论（不泄露「有没有库」这件事）。
	if _, err := NewListBroadcastLogsLogic(ctxClient(t), e.Svc).ListBroadcastLogs(
		&rpc.ListBroadcastLogsReq{RoomId: roomID}); !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("期望 ErrPermissionDenied，实际 %v", err)
	}
	reply, err := NewListBroadcastLogsLogic(ctxOperator(t, "1001"), e.Svc).ListBroadcastLogs(
		&rpc.ListBroadcastLogsReq{RoomId: roomID})
	if err == nil {
		t.Fatalf("存储不可用不能报「查无记录」这种假结论: %+v", reply)
	}
	if errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("错误映射串了: %v", err)
	}
}
