package logic

import (
	"errors"
	"strconv"
	"testing"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件覆盖 GetRoomRoute 与 ListRoomRoutes —— 房间路由的两个读入口。
//
// 两个入口的差别就是契约本身，本文件围绕四件事组织断言：
//  1. 信任要求相反：GetRoomRoute 刻意不加调用方门禁（单房间路由是广播链路的内部依赖，
//     gateway/app 每次下发前都要读），ListRoomRoutes 走 requireOperatorRead（跨房间枚举暴露节点拓扑）。
//     同一份 RequireAttestedOperator=true 开关下，两者的结论必须相反，否则说明其中一侧的门禁是摆设。
//  2. 缓存只属于 GetRoomRoute：它和 BroadcastToRoom 共用 helpers.routeForFanout 的同一个键与同一份 TTL，
//     于是「运营查到的路由」与「广播实际打到的节点」不会各读各的；ListRoomRoutes 刻意不回看缓存，
//     发布排障要的是此刻的真实归属。两侧都用「毒化 FindOne」证明有没有读库。
//  3. serving_connections 只观测不判定：Redis 故障时整页/整条都按 0 回显且**不报错**，
//     而真正的读库故障必须原样上抛——把依赖故障降级成 0 是伪造结论。
//  4. 只读：不写审计、不写路由、不自增版本。

// --- 共用夹具 ---

// routeReadEnv 一条 SERVING 路由 + 若干在线连接，两个读入口共用的起点。
func routeReadEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	return e
}

// setRoutePrimary 直接改存储里的行（模拟「别的进程已把房间迁走」），返回该行的最新指针。
func setRoutePrimary(t *testing.T, e *testEnv, room int64, primary string, state int32) {
	t.Helper()
	row, ok := e.Routes.rows[room]
	if !ok {
		t.Fatalf("测试前提不成立：room_id=%d 没有路由行", room)
	}
	row.PrimaryNode = primary
	row.State = state
	row.Version++
	row.Mtime = e.Clock.unix()
}

// --- GetRoomRoute ---

func TestGetRoomRouteParameterGate(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.RoomRouteReq
		want error
	}{
		{"空请求", nil, model.ErrInvalidRoomID},
		{"房间号为 0", &rpc.RoomRouteReq{RoomId: 0}, model.ErrInvalidRoomID},
		{"房间号为负", &rpc.RoomRouteReq{RoomId: -7}, model.ErrInvalidRoomID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			seedRoute(e, roomID, model.RouteStateServing, nodeA)
			// 毒化两个读源：只要返回的是参数错误，就说明定位与读库都没开始。
			e.Routes.fail("FindOne", errRouteProbe)
			e.Leases.failWith("RoomConnectionCount", errors.New("redis should not be read"))
			got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v got %v", tc.want, err)
			}
			if got != nil {
				t.Fatalf("参数被拒时不该有路由回显: %+v", got)
			}
			if n := e.Leases.callCount("CacheGet"); n != 0 {
				t.Fatalf("参数阶段不该读缓存，CacheGet %d 次", n)
			}
		})
	}
}

// 钉住刻意设计的不对称：同一份「要求归因运营」开关下，单房间读放行、跨房间枚举拒绝。
//
// GetRoomRoute 的实现注释（getroomroutelogic.go:68-69）明确写了「本方法不加调用方门禁」，
// 而 ListRoomRoutes 走 gate.requireOperatorRead。这条用例的作用是让那句注释变成可执行的：
// 一旦有人给 GetRoomRoute 补上门禁、或把 ListRoomRoutes 的门禁摘掉，这里就会红。
// 注意这不是「GetRoomRoute 更安全」，而是它的暴露面本来就只有一个房间号：
// 能调通它的前提是已经知道 room_id，回显的也只有节点名，没有连接名单与正文。
func TestGetRoomRouteHasNoCallerGateWhileListRoomRoutesRequiresOne(t *testing.T) {
	e := routeReadEnv(t)
	e.apply(func(c *config.LiveGatewayConf) { c.RequireAttestedOperator = true })

	// 客户端面（无任何归因 metadata）读单房间路由：放行。
	got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("GetRoomRoute 在设计上不带调用方门禁: %v", err)
	}
	if got.GetNodeId() != nodeA {
		t.Fatalf("回显的不是主节点: %+v", got)
	}

	// 同一份请求身份读跨房间枚举：拒绝，且一条都没回。
	listed, err := NewListRoomRoutesLogic(ctxClient(t), e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{})
	if !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("ListRoomRoutes 在开关打开后必须拒绝未归因主体，实际 %v", err)
	}
	if len(listed.GetRoutes()) != 0 {
		t.Fatalf("被拒的枚举不该回任何拓扑: %+v", listed.GetRoutes())
	}

	// 判别性对照：只把身份换成归因 OPERATOR，其余一字不改，就必须放行。
	if _, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{}); err != nil {
		t.Fatalf("归因 OPERATOR 应可枚举: %v", err)
	}
}

// 自报的 metadata 不构成身份：伪造 caller-mid 也换不到「额外的读能力」，
// 但 GetRoomRoute 本来就不看身份，所以两侧都按未归因处理——这里钉的是「自报值不升权」。
func TestGetRoomRouteForgedCallerHeadersDoNotEscalate(t *testing.T) {
	e := routeReadEnv(t)
	e.apply(func(c *config.LiveGatewayConf) { c.RequireAttestedOperator = true })
	// attested 不是 "true"，role=OPERATOR 与 mid 都被 callerFrom 丢弃。
	ctx := ctxAs(t, "false", "OPERATOR", "1001", "internal-ops")
	if _, err := NewGetRoomRouteLogic(ctx, e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); err != nil {
		t.Fatalf("GetRoomRoute 无门禁，自报头既不升权也不降权: %v", err)
	}
	_, err := NewListRoomRoutesLogic(ctx, e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{})
	if !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("自报 OPERATOR 竟被当成归因主体: %v", err)
	}
}

func TestGetRoomRouteMissingRowIsNotFoundNotFabricated(t *testing.T) {
	e := newTestEnv(t) // 没有任何路由行
	got, err := NewGetRoomRouteLogic(ctxOperator(t, "1001"), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if !errors.Is(err, model.ErrRouteNotFound) {
		t.Fatalf("无行必须回 ErrRouteNotFound，实际 %v", err)
	}
	// 绝不回显一条 state=UNSPECIFIED 的空路由：调用方会读成「有路由但没节点」，广播就静默丢消息。
	if got != nil {
		t.Fatalf("查不到路由时不该有回显: %+v", got)
	}
	if n := e.Leases.callCount("CacheSet"); n != 0 {
		t.Fatalf("不存在的路由不该被回填进缓存（负缓存会让 Register 后最长 TTL 秒内广播不到）: %d 次", n)
	}
	assertNoWrites(t, e)
}

// GetRoomRoute 与广播共用同一条 routeForFanout：同键、同 TTL、同回源口径。
//
// 证明方式不是数调用次数，而是「让数据库说谎」：先把缓存喂热，再把库里的行改成另一个节点
// 并毒化 FindOne。此时读到的仍是缓存里的旧主节点，说明广播链路读的就是这个键；
// 而把 TTL 关成 0 的同一条用例必须在毒化后直接失败，说明挡住的确实是缓存而不是别的分支。
func TestGetRoomRouteServesFromSharedRouteCache(t *testing.T) {
	t.Run("缓存打开时不回源", func(t *testing.T) {
		e := routeReadEnv(t)
		if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); err != nil {
			t.Fatalf("首次读取失败: %v", err)
		}
		if _, ok := e.Leases.cache[repository.RouteCacheKey(roomID)]; !ok {
			t.Fatalf("首次读取后应按广播同口径的键回填缓存，实际键集=%v", keysOf(e.Leases.cache))
		}
		setRoutePrimary(t, e, roomID, nodeB, model.RouteStateDraining)
		e.Routes.fail("FindOne", errRouteProbe)

		got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
		if err != nil {
			t.Fatalf("缓存命中时不该回源: %v", err)
		}
		if got.GetNodeId() != nodeA || got.GetState() != rpc.RouteState_ROUTE_STATE_SERVING {
			t.Fatalf("第二次读应回缓存里的旧值，实际 %+v", got)
		}
	})

	t.Run("缓存关闭时每次都回源", func(t *testing.T) {
		e := routeReadEnv(t)
		e.apply(func(c *config.LiveGatewayConf) { c.RoomRouteCacheTTLSeconds = 0 })
		if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); err != nil {
			t.Fatalf("首次读取失败: %v", err)
		}
		if n := e.Leases.callCount("CacheSet"); n != 0 {
			t.Fatalf("TTL=0 时不该写缓存，实际 %d 次", n)
		}
		e.Routes.fail("FindOne", errRouteProbe)
		if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); !errors.Is(err, errRouteProbe) {
			t.Fatalf("没有缓存挡着时必须真的回源并把故障上抛，实际 %v", err)
		}
	})
}

// 广播读的是同一个键：GetRoomRoute 喂热缓存后，把库改脏并毒化 FindOne，
// BroadcastToRoom 仍然打到缓存里那个节点。两侧不一致就是最难解释的观感，这里把它钉成一条边。
func TestGetRoomRouteWarmsTheKeyBroadcastReads(t *testing.T) {
	e := routeReadEnv(t)
	mustSeedSenderLease(t, e, "lgwl_route_bcast", roomID, mid, model.RoleViewer)
	if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); err != nil {
		t.Fatalf("预热路由缓存失败: %v", err)
	}
	setRoutePrimary(t, e, roomID, nodeB, model.RouteStateServing)
	e.Routes.fail("FindOne", errRouteProbe)

	got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil || got.GetNodeId() != nodeA {
		t.Fatalf("路由读应命中缓存: err=%v got=%+v", err, got)
	}

	// 必须带 sender_lease_id：helpers.go 的 resolveSender 对未归因客户端「无凭据即丢」，
	// 那样扇出根本不会发生，缓存这条边也就无从验证。
	_, err = NewBroadcastToRoomLogic(ctxClient(t), e.Svc).BroadcastToRoom(
		&rpc.BroadcastToRoomReq{RoomId: roomID, Kind: rpc.BroadcastKind_BROADCAST_KIND_DANMAKU,
			MessageId: "msg-route-shared", SenderMid: mid, SenderRole: rpc.ConnRole_CONN_ROLE_VIEWER,
			SenderLeaseId: "lgwl_route_bcast", Payload: []byte("hello")})
	if err != nil {
		t.Fatalf("广播读缓存即可，不该因回源失败而报错: %v", err)
	}
	assertFanoutOnce(t, e)
	if nodes := e.Tp.fanoutReqs[0].Nodes; len(nodes) != 1 || nodes[0] != nodeA {
		t.Fatalf("广播与路由读用的是同一个键，应打到缓存里的 %s，实际 %v", nodeA, nodes)
	}
}

func TestGetRoomRouteProjectionPassesThroughEveryDecisionField(t *testing.T) {
	e := newTestEnv(t)
	row := &model.LiveGwRoomRoute{
		RoomId: roomID, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(nodeB),
		ShardCount: 3, State: model.RouteStateServing, Version: 7,
		Ctime: testBaseTime + 11, Mtime: testBaseTime + 22,
	}
	e.Routes.seed(row)
	// 在线数只来自 Redis：两条活连接 + 一条已过期的，读数必须是 2 而不是 3。
	e.seedActiveLease("lgwl_route_on1", "conn-1", nodeA, roomID, mid, model.RoleViewer)
	e.seedActiveLease("lgwl_route_on2", "conn-2", nodeA, roomID, otherMid, model.RoleViewer)
	stale := e.seedActiveLease("lgwl_route_exp", "conn-3", nodeA, roomID, anchorMid, model.RoleAnchor)
	stale.ExpireAt = e.Clock.unix() - 1

	got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.GetRoomId() != roomID || got.GetNodeId() != nodeA {
		t.Fatalf("主节点投影错: %+v", got)
	}
	if rep := got.GetReplicaNodes(); len(rep) != 1 || rep[0] != nodeB {
		t.Fatalf("副本节点应从 replica_nodes 解出，实际 %v", rep)
	}
	if got.GetState() != rpc.RouteState_ROUTE_STATE_SERVING || got.GetShardCount() != 3 {
		t.Fatalf("state/shard 投影错: %+v", got)
	}
	// version 必须原样回传：DrainRoomRoute 要求带 expected_version，投影丢了版本号运营就没法排空。
	if got.GetVersion() != 7 {
		t.Fatalf("version 必须原样回传，实际 %d", got.GetVersion())
	}
	if got.GetUpdatedAt() != testBaseTime+22 || got.GetCtime() != testBaseTime+11 {
		t.Fatalf("时间列投影错: %+v", got)
	}
	if got.GetServingConnections() != 2 {
		t.Fatalf("serving_connections 只认此刻有效的连接，实际 %d", got.GetServingConnections())
	}
	// 表里根本没有连接数列：读数只能来自 Redis，这里用调用次数证明没有「顺手」查 MySQL 统计。
	if n := e.Leases.callCount("RoomConnectionCount"); n != 1 {
		t.Fatalf("连接数应恰好读一次 Redis，实际 %d 次", n)
	}
	assertNoWrites(t, e)
}

// Redis 读数不可用时按 0 回显且**不影响路由本身**：serving_connections 只是观测值，
// 拿它当判定依据会出现「Redis 抖动 → 房间看起来没人 → 广播被跳过」这类假结论。
// 成对用例把「路由本身读不到」留在 ErrRouteNotFound：降级只覆盖读数，不覆盖真错误。
func TestGetRoomRouteServingConnectionsDegradeToZeroWithoutFailing(t *testing.T) {
	e := routeReadEnv(t)
	e.Leases.failWith("RoomConnectionCount", errors.New("redis unavailable"))
	got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("连接数读不到不该让路由查询失败: %v", err)
	}
	if got.GetServingConnections() != 0 {
		t.Fatalf("降级时 serving_connections 必须是 0，实际 %d", got.GetServingConnections())
	}
	if got.GetNodeId() != nodeA || got.GetVersion() != 1 {
		t.Fatalf("降级不该动到路由本体投影: %+v", got)
	}

	e2 := newTestEnv(t)
	e2.Leases.failWith("RoomConnectionCount", errors.New("redis unavailable"))
	if _, err := NewGetRoomRouteLogic(ctxClient(t), e2.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); !errors.Is(err, model.ErrRouteNotFound) {
		t.Fatalf("没有路由行时读数降级不能把它伪装成有效路由，实际 %v", err)
	}
}

// replica_nodes 脏数据：主节点仍然有效，副本按空回显并回源，绝不因为副本列坏了就报错。
// 第二次读专门验「缓存里的脏副本也照样回源」——routeForFanout 命中缓存后还要解一次 JSON。
func TestGetRoomRouteDirtyReplicaNodesKeepPrimaryAndFallBackToDB(t *testing.T) {
	e := newTestEnv(t)
	e.Routes.seed(&model.LiveGwRoomRoute{
		RoomId: roomID, PrimaryNode: nodeA, ReplicaNodes: `{"not":"an-array"`,
		ShardCount: 1, State: model.RouteStateServing, Version: 2, Ctime: testBaseTime, Mtime: testBaseTime,
	})
	got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("副本列脏不该报错: %v", err)
	}
	if n := len(got.GetReplicaNodes()); n != 0 {
		t.Fatalf("脏副本应按空回显，实际 %d 项", n)
	}
	if got.GetNodeId() != nodeA {
		t.Fatalf("主节点仍要如实回显: %+v", got)
	}

	// 第二次读命中缓存，缓存里同样是脏副本 → 回源 DB。毒化 FindOne 应把这条路径暴露成错误。
	e.Routes.fail("FindOne", errRouteProbe)
	if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); !errors.Is(err, errRouteProbe) {
		t.Fatalf("脏缓存必须回源（而不是静默按无副本继续），实际 %v", err)
	}
}

func TestGetRoomRouteStoreFailurePropagates(t *testing.T) {
	t.Run("DataSource 未配置", func(t *testing.T) {
		e := routeReadEnv(t)
		e.markStoreMissing()
		e.apply(func(c *config.LiveGatewayConf) { c.RoomRouteCacheTTLSeconds = 0 })
		if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("缺库要显式失败而不是回「无路由」: %v", err)
		}
	})
	t.Run("读表故障", func(t *testing.T) {
		e := routeReadEnv(t)
		e.apply(func(c *config.LiveGatewayConf) { c.RoomRouteCacheTTLSeconds = 0 })
		e.Routes.fail("FindOne", errRouteProbe)
		if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); !errors.Is(err, errRouteProbe) {
			t.Fatalf("读表故障要原样上抛，实际 %v", err)
		}
	})
	t.Run("缓存写失败不影响正确性", func(t *testing.T) {
		e := routeReadEnv(t)
		e.Leases.failWith("CacheSet", errors.New("redis write failed"))
		got, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
		if err != nil {
			t.Fatalf("缓存写不进去只该记告警: %v", err)
		}
		if got.GetNodeId() != nodeA {
			t.Fatalf("回显错: %+v", got)
		}
	})
}

// --- ListRoomRoutes ---

// 钉住当前行为：ListRoomRoutes 是唯一「nil 请求不当错」的读方法。
//
// GetRoomRoute / ListRoomConnections / GetAccessQuota / SendToUser 在 in==nil 时都回
// ErrInvalidRoomID / ErrInvalidQuotaScope，而 listroomrouteslogic.go:48-50 把 nil 换成空请求继续执行，
// 于是 grpc 传空报文会拿到 200 + 空列表而不是 InvalidArgument。这里按现状断言，
// 并在交付报告里登记为契约不一致（不改生产代码）。
func TestListRoomRoutesNilRequestSucceedsAsEmptyPage(t *testing.T) {
	e := routeReadEnv(t)
	got, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(nil)
	if err != nil {
		t.Fatalf("当前实现把 nil 换成空请求继续执行，实际 %v", err)
	}
	if got.GetPage() == nil {
		t.Fatalf("page 必须恒非 nil，调用方不该判空")
	}
	if got.GetPage().GetTotal() != 1 || len(got.GetRoutes()) != 1 {
		t.Fatalf("nil 请求等价于「不过滤」，应回全量 1 行，实际 %+v", got)
	}

	// 对照：同一环境下另一个读入口对 nil 的处理是报错——这条对比让上面的钉住不是同义反复。
	if _, err := NewListRoomConnectionsLogic(ctxOperator(t, "1001"), e.Svc).ListRoomConnections(nil); !errors.Is(err, model.ErrInvalidRoomID) {
		t.Fatalf("ListRoomConnections 对 nil 必须报错（否则等于放开全房枚举），实际 %v", err)
	}
}

// 门禁先于参数校验，也先于任何读库。
//
// 这一条与 ListRoomConnections 相反（那边参数校验在前），不对称本身要能被测出来：
// 把 RequireAttestedOperator 翻成 true 后，越权请求连「参数是不是合法」都不该被回显，
// 否则一个未归因调用方可以用错误文本探测本服务的校验边界。
func TestListRoomRoutesGatePrecedesParameterValidationAndRead(t *testing.T) {
	e := newTestEnv(t)
	e.apply(func(c *config.LiveGatewayConf) { c.RequireAttestedOperator = true })
	e.Routes.fail("List", errors.New("list should not run"))
	e.Leases.failWith("RoomConnectionCount", errors.New("count should not run"))

	bad := &rpc.ListRoomRoutesReq{
		NodeId: "gw node with spaces",
		State:  rpc.RouteState(99),
		Page:   &rpc.PageParam{Pn: 1, Ps: 9999},
	}
	_, err := NewListRoomRoutesLogic(ctxClient(t), e.Svc).ListRoomRoutes(bad)
	if !errors.Is(err, model.ErrPermissionDenied) {
		t.Fatalf("三重非法入参下仍应先被门禁拒掉，实际 %v", err)
	}
	if n := e.Leases.callCount("RoomConnectionCount"); n != 0 {
		t.Fatalf("被拒的枚举不该扫 Redis，RoomConnectionCount %d 次", n)
	}

	// 判别性对照：只换身份，同一份非法请求就必须走到第一个参数错（node_id 形状）。
	if _, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(bad); !errors.Is(err, model.ErrEmptyNodeID) {
		t.Fatalf("归因后应暴露参数错，实际 %v", err)
	}
}

// 参数校验的内部顺序：node_id → state → 分页。三个探针同时非法时，露出的必须是前一个。
func TestListRoomRoutesValidationOrderIsFixed(t *testing.T) {
	e := newTestEnv(t)
	l := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc)

	_, err := l.ListRoomRoutes(&rpc.ListRoomRoutesReq{
		NodeId: "bad node id", State: rpc.RouteState(99), Page: &rpc.PageParam{Ps: 9999},
	})
	if !errors.Is(err, model.ErrEmptyNodeID) {
		t.Fatalf("node_id 形状应先于 state 校验，实际 %v", err)
	}
	_, err = l.ListRoomRoutes(&rpc.ListRoomRoutesReq{
		NodeId: nodeA, State: rpc.RouteState(99), Page: &rpc.PageParam{Ps: 9999},
	})
	if !errors.Is(err, model.ErrInvalidTransition) {
		t.Fatalf("state 合法性应先于分页校验，实际 %v", err)
	}
	_, err = l.ListRoomRoutes(&rpc.ListRoomRoutesReq{
		NodeId: nodeA, State: rpc.RouteState_ROUTE_STATE_SERVING, Page: &rpc.PageParam{Ps: 9999},
	})
	if !errors.Is(err, model.ErrPsTooLarge) {
		t.Fatalf("最后是分页校验，实际 %v", err)
	}
}

// 非法 state 必须显式拒绝，而不是静默当「不过滤」：
// 后者会让运营看着全量列表以为过滤生效了，进而把「这个节点没在排空」读成「排空完成了」。
func TestListRoomRoutesStateFilterRejectsUnknownRefusesUnfiltered(t *testing.T) {
	e := newTestEnv(t)
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: roomID, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(),
		ShardCount: 1, State: model.RouteStateServing, Version: 1})
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: otherRoom, PrimaryNode: nodeB, ReplicaNodes: replicaJSON(),
		ShardCount: 1, State: model.RouteStateDraining, Version: 1})

	// UNSPECIFIED(0) 是「不过滤」的合法编码。
	all, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(
		&rpc.ListRoomRoutesReq{State: rpc.RouteState_ROUTE_STATE_UNSPECIFIED})
	if err != nil {
		t.Fatalf("state=UNSPECIFIED 应放行: %v", err)
	}
	if all.GetPage().GetTotal() != 2 {
		t.Fatalf("不过滤时应回两行，实际 %+v", all.GetPage())
	}
	// 4 越界（RouteState 只到 3）。
	if _, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(
		&rpc.ListRoomRoutesReq{State: rpc.RouteState(4)}); !errors.Is(err, model.ErrInvalidTransition) {
		t.Fatalf("越界 state 必须拒绝，实际 %v", err)
	}
}

// 节点过滤同时命中主节点与副本节点：只看主节点会让大房间的分片副本在排障视图里凭空消失。
func TestListRoomRoutesNodeFilterMatchesPrimaryOrReplica(t *testing.T) {
	e := newTestEnv(t)
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7001, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(nodeB),
		ShardCount: 2, State: model.RouteStateServing, Version: 1})
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7002, PrimaryNode: nodeB, ReplicaNodes: replicaJSON(),
		ShardCount: 1, State: model.RouteStateServing, Version: 1})
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7003, PrimaryNode: "gw-node-c", ReplicaNodes: replicaJSON(),
		ShardCount: 1, State: model.RouteStateServing, Version: 1})

	got, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(
		&rpc.ListRoomRoutesReq{NodeId: "  " + nodeB + "  "}) // CleanNodeID 归一空白
	if err != nil {
		t.Fatalf("过滤查询失败: %v", err)
	}
	if got.GetPage().GetTotal() != 2 {
		t.Fatalf("node_id=%s 应命中「主或副本」两行，实际 total=%d", nodeB, got.GetPage().GetTotal())
	}
	ids := roomIDsOf(got.GetRoutes())
	if len(ids) != 2 || ids[0] != 7001 || ids[1] != 7002 {
		t.Fatalf("命中行不符（副本行必须在内）: %v", ids)
	}
	// 命中副本那一行回显的是它自己的主节点，不是被查的那个节点——否则运营会以为副本是主。
	if got.GetRoutes()[0].GetNodeId() != nodeA {
		t.Fatalf("7001 的主节点应如实回显，实际 %+v", got.GetRoutes()[0])
	}
}

// 排空中/已下线的路由都会出现在列表里：本方法是发布排障的真值视图，
// 「只剩 SERVING」的隐式过滤会让人以为 DRAINING 已经走完，进而硬切节点。
func TestListRoomRoutesShowsDrainedAndOfflineRows(t *testing.T) {
	e := newTestEnv(t)
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7001, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(),
		ShardCount: 1, State: model.RouteStateServing, Version: 1})
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7002, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(),
		ShardCount: 1, State: model.RouteStateDraining, Version: 4, DrainReason: "graceful-offline"})
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7003, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(),
		ShardCount: 1, State: model.RouteStateOffline, Version: 9})

	got, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(
		&rpc.ListRoomRoutesReq{NodeId: nodeA})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.GetPage().GetTotal() != 3 {
		t.Fatalf("不带状态过滤时三种状态都要现身，实际 %+v", statesOf(got.GetRoutes()))
	}
	want := []rpc.RouteState{
		rpc.RouteState_ROUTE_STATE_SERVING,
		rpc.RouteState_ROUTE_STATE_DRAINING,
		rpc.RouteState_ROUTE_STATE_OFFLINE,
	}
	for i, st := range statesOf(got.GetRoutes()) {
		if st != want[i] {
			t.Fatalf("第 %d 行状态应为 %v（room_id 升序），实际 %v", i, want[i], st)
		}
	}
	// version 逐行原样回传：排空要带 expected_version，这里丢了版本号运营就只能猜。
	if v := got.GetRoutes()[1].GetVersion(); v != 4 {
		t.Fatalf("DRAINING 行的 version 要原样回传，实际 %d", v)
	}
}

// 排序 room_id ASC + 翻页可重放：插入顺序故意打乱，断言的是回显顺序而不是存储顺序。
func TestListRoomRoutesOrdersByRoomIDAndPagesWithoutOverlap(t *testing.T) {
	e := newTestEnv(t)
	for _, r := range []int64{7003, 7001, 7002, 7004, 7005} {
		e.Routes.seed(&model.LiveGwRoomRoute{RoomId: r, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(),
			ShardCount: 1, State: model.RouteStateServing, Version: 1})
	}
	l := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc)

	first, err := l.ListRoomRoutes(&rpc.ListRoomRoutesReq{Page: &rpc.PageParam{Pn: 1, Ps: 2}})
	if err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	if ids := roomIDsOf(first.GetRoutes()); len(ids) != 2 || ids[0] != 7001 || ids[1] != 7002 {
		t.Fatalf("必须按 room_id 升序，实际 %v", ids)
	}
	// total 是过滤后的全量条数，不是本页条数：拿本页条数填 total 会让调用方永远翻不到底。
	if first.GetPage().GetTotal() != 5 {
		t.Fatalf("total 应为全量 5，实际 %d", first.GetPage().GetTotal())
	}
	third, err := l.ListRoomRoutes(&rpc.ListRoomRoutesReq{Page: &rpc.PageParam{Pn: 3, Ps: 2}})
	if err != nil {
		t.Fatalf("第三页失败: %v", err)
	}
	if ids := roomIDsOf(third.GetRoutes()); len(ids) != 1 || ids[0] != 7005 {
		t.Fatalf("越界页之后的尾页应回第 5 行，实际 %v", ids)
	}

	// ps 越界是报错而不是夹到 50：夹了会让调用方写出错误的翻页循环（它以为一页 9999 条）。
	if _, err := l.ListRoomRoutes(&rpc.ListRoomRoutesReq{Page: &rpc.PageParam{Pn: 1, Ps: 51}}); !errors.Is(err, model.ErrPsTooLarge) {
		t.Fatalf("ps=51（MaxPageSize=50）必须报错，实际 %v", err)
	}
	// 边界成对：恰好等于上限放行。
	if _, err := l.ListRoomRoutes(&rpc.ListRoomRoutesReq{Page: &rpc.PageParam{Pn: 1, Ps: 50}}); err != nil {
		t.Fatalf("ps 恰好等于 MaxPageSize 应放行: %v", err)
	}
	// ps<=0 落到默认 20（本服务全量只有 5 行，用「不报错」夹住这条默认值分支）。
	if _, err := l.ListRoomRoutes(&rpc.ListRoomRoutesReq{Page: &rpc.PageParam{Pn: 0, Ps: 0}}); err != nil {
		t.Fatalf("pn/ps 非正值应被规范化，实际 %v", err)
	}
	// 夹取口径由配置决定：把 MaxPageSize 调小，同一个 ps 就从合法变非法。
	e.apply(func(c *config.LiveGatewayConf) { c.MaxPageSize = 2 })
	if _, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(
		&rpc.ListRoomRoutesReq{Page: &rpc.PageParam{Ps: 3}}); !errors.Is(err, model.ErrPsTooLarge) {
		t.Fatalf("MaxPageSize=2 时 ps=3 必须报错，实际 %v", err)
	}
}

// ListRoomRoutes 不读路由缓存（区别于 GetRoomRoute）：发布排障要看此刻真实归属。
// 证明方式：先用 GetRoomRoute 喂热缓存，再把库里的行改成 DRAINING@B——
// 枚举必须回 DB 的新值，同环境的 GetRoomRoute 必须还回缓存的旧值。
func TestListRoomRoutesBypassesRouteCacheWhileGetRoomRouteUsesIt(t *testing.T) {
	e := routeReadEnv(t)
	if _, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID}); err != nil {
		t.Fatalf("预热缓存失败: %v", err)
	}
	setRoutePrimary(t, e, roomID, nodeB, model.RouteStateDraining)

	before := e.Leases.callCount("CacheGet")
	got, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{})
	if err != nil {
		t.Fatalf("枚举失败: %v", err)
	}
	if n := e.Leases.callCount("CacheGet"); n != before {
		t.Fatalf("路由枚举不该碰路由读缓存，CacheGet 从 %d 变 %d", before, n)
	}
	if len(got.GetRoutes()) != 1 {
		t.Fatalf("应恰好一行，实际 %+v", got.GetRoutes())
	}
	row := got.GetRoutes()[0]
	if row.GetNodeId() != nodeB || row.GetState() != rpc.RouteState_ROUTE_STATE_DRAINING {
		t.Fatalf("枚举必须回 DB 的真实归属，实际 %+v", row)
	}

	// 同一环境、同一房间：GetRoomRoute 仍回缓存里的旧值——两侧的差别就是契约。
	single, err := NewGetRoomRouteLogic(ctxClient(t), e.Svc).GetRoomRoute(&rpc.RoomRouteReq{RoomId: roomID})
	if err != nil {
		t.Fatalf("单房间读失败: %v", err)
	}
	if single.GetNodeId() != nodeA || single.GetState() != rpc.RouteState_ROUTE_STATE_SERVING {
		t.Fatalf("GetRoomRoute 应命中同一条缓存，实际 %+v", single)
	}
}

// Redis 读数不可用时整页照常回，只是每行 serving_connections 都是 0：
// 一页 50 行不该被一个观测值打死，但也不能因此把「读数不可用」伪装成「房间没人」以外的结论。
func TestListRoomRoutesRedisFailureDegradesWholePageButKeepsRows(t *testing.T) {
	e := newTestEnv(t)
	for _, r := range []int64{7001, 7002, 7003} {
		e.Routes.seed(&model.LiveGwRoomRoute{RoomId: r, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(),
			ShardCount: 1, State: model.RouteStateServing, Version: 1})
		e.seedActiveLease("lgwl_route_cnt_"+strconv.Itoa(int(r)), "conn-"+strconv.Itoa(int(r)), nodeA, r, int64(9000+r), model.RoleViewer)
	}
	e.Leases.failWith("RoomConnectionCount", errors.New("redis unavailable"))

	got, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{})
	if err != nil {
		t.Fatalf("读数降级不该让整页失败: %v", err)
	}
	if len(got.GetRoutes()) != 3 {
		t.Fatalf("三行都要回显，实际 %d", len(got.GetRoutes()))
	}
	for _, row := range got.GetRoutes() {
		if row.GetServingConnections() != 0 {
			t.Fatalf("降级时 serving_connections 一律按 0，实际 %+v", row)
		}
	}
	// 每行都取一次读数（降级是逐行的，不是整页短路后其余行留零值假装取过）。
	if n := e.Leases.callCount("RoomConnectionCount"); n != 3 {
		t.Fatalf("应逐行尝试读数 3 次，实际 %d 次", n)
	}
}

// 列表里的副本脏数据按空回显但主节点照常出现（与 GetRoomRoute 同口径，不回 500）。
func TestListRoomRoutesDirtyReplicaColumnDegradesPerRow(t *testing.T) {
	e := newTestEnv(t)
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7001, PrimaryNode: nodeA, ReplicaNodes: `{"broken":1}`,
		ShardCount: 1, State: model.RouteStateServing, Version: 1})
	e.Routes.seed(&model.LiveGwRoomRoute{RoomId: 7002, PrimaryNode: nodeA, ReplicaNodes: replicaJSON(nodeB),
		ShardCount: 1, State: model.RouteStateServing, Version: 1})

	got, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{})
	if err != nil {
		t.Fatalf("脏副本不该报错: %v", err)
	}
	if len(got.GetRoutes()) != 2 {
		t.Fatalf("两行都要回显，实际 %d", len(got.GetRoutes()))
	}
	if n := len(got.GetRoutes()[0].GetReplicaNodes()); n != 0 {
		t.Fatalf("脏副本行应按空回显，实际 %d 项", n)
	}
	if n := len(got.GetRoutes()[1].GetReplicaNodes()); n != 1 {
		t.Fatalf("干净行不受影响，实际 %d 项", n)
	}
}

func TestListRoomRoutesStoreFailurePropagates(t *testing.T) {
	t.Run("DataSource 未配置", func(t *testing.T) {
		e := routeReadEnv(t)
		e.markStoreMissing()
		if _, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{}); !errors.Is(err, repository.ErrStoreUnavailable) {
			t.Fatalf("缺库要显式失败，不能回「空列表」这种假结论: %v", err)
		}
	})
	t.Run("读表故障", func(t *testing.T) {
		e := routeReadEnv(t)
		e.Routes.fail("List", errRouteProbe)
		if _, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(&rpc.ListRoomRoutesReq{}); !errors.Is(err, errRouteProbe) {
			t.Fatalf("读表故障要原样上抛（空列表会被读成「这个节点上没房间」）: %v", err)
		}
	})
}

// 空结果不是错误：total=0 + 空切片 + page 恒非 nil。
// 这一条把「查不到」和「查失败」分开，否则 gateway/admin 会把依赖故障画成空表格。
func TestListRoomRoutesEmptyResultIsAPageNotAnError(t *testing.T) {
	e := newTestEnv(t)
	seedRoute(e, roomID, model.RouteStateServing, nodeA)
	got, err := NewListRoomRoutesLogic(ctxOperator(t, "1001"), e.Svc).ListRoomRoutes(
		&rpc.ListRoomRoutesReq{NodeId: "gw-node-none"})
	if err != nil {
		t.Fatalf("空结果不该报错: %v", err)
	}
	if got.GetPage() == nil || got.GetPage().GetTotal() != 0 {
		t.Fatalf("page 必须非空且 total=0，实际 %+v", got.GetPage())
	}
	if got.GetRoutes() == nil || len(got.GetRoutes()) != 0 {
		t.Fatalf("routes 应是空切片而不是 nil: %+v", got.GetRoutes())
	}
	assertNoWrites(t, e)
}

// --- 本文件专用小工具 ---

func roomIDsOf(rows []*rpc.RoomRouteInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetRoomId())
	}
	return out
}

func statesOf(rows []*rpc.RoomRouteInfo) []rpc.RouteState {
	out := make([]rpc.RouteState, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetState())
	}
	return out
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
