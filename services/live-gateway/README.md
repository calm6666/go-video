# live-gateway

直播 **WebSocket 长连接会话与房间广播** 的领域服务：连接租约、心跳、房间订阅、广播/单播下发、
断线重连票据、接入配额与路由治理。它是「消息能不能到达观众」这条链路的唯一出口。

- **拥有数据**（库名 `go_video_live_gateway`）：`live_gw_room_route`（房间→节点路由投影）、
  `live_gw_access_quota`（接入与广播配额配置）、`live_gw_broadcast_log`（广播审计流水）、
  `live_gw_reconnect_ticket`（重连票据审计与撤销名单），以及 Redis 里的全部易失在线态。
- **提供能力**：租约获取/续租/释放、心跳上报、入房/退房、路由查询与排空、房间广播、单播、
  弹幕与系统事件转发、踢下线、广播流水与配额读写（共 22 个 RPC，见下表）。
- **依赖**：Redis（在线态主存储）、MySQL（本服务自有库）、Kafka（`live.state.v1`/`danmaku.sent.v1`
  事件入口）、`live-room`（房间归属与可广播状态，本服务不自行判定）。**不依赖** commerce 任何接口。
- **约束**：
  - **逐条连接状态绝不写 MySQL**（每连接在线表、心跳明细、订阅成员列表都是高频易失数据，
    落库只会拖垮主库）；Redis 丢失只造成「客户端重连」，不造成业务事实丢失。
  - 广播**可以丢弃**（限流、无路由、无订阅者、载荷过大、通道未接线），**但绝不伪造权限**：
    校验失败必须回明确 deny/drop 原因，禁止用「成功下发 0 人」掩盖越权。
  - 不接受客户端自报角色：`NormalizeRole` 把 `UNSPECIFIED`/未知取值一律降级为 `VIEWER`；
    ANCHOR 必须向 `live-room` 核实房间归属。
  - 票据/密钥只存哈希（`ticket_hash CHAR(64)`），明文只出现在一次响应里，永不入库、永不入日志。

## 本期落地范围（务必先读）

契约（`rpc/livegateway.proto`）+ `model/` 四表 + 迁移 SQL + 配置装配 + **22 个 logic 方法全部实现**已完成
（`internal/logic/` 已无 `model.ErrNotImplemented`，也没有一处返回假成功，AGENTS.md §9）。
四条下发入口（`BroadcastToRoom`/`ForwardDanmaku`/`ForwardSystemEvent`/`SendToUser`）共用
`internal/logic/broadcastcore.go` 的**同一条判定链**（载荷上限 → 时效 → 鉴权 → 房间可广播 → 路由 →
去重 → 限流 → 扇出 → 审计）：鉴权刻意排在去重之前（复用旧 message_id 的越权探测必须留 DENIED 痕，
但该承诺在 `message_id` 已被首次受理占位时不成立，见「已知缺口」第 13 条），
去重排在限流之前（重放不消耗配额）。方法体上方的有序注释就是实现次序，改判定链时按注释复核。
logic 层已有 4 个测试文件（`broadcast_fanout_test.go`/`broadcast_logs_read_test.go`/
`forward_paths_test.go`/`reconnect_ticket_test.go` + 共用替身 `fakes_test.go`，2026-09-22 补）。
**注意其证据强度**：这批测试文件在本轮之前从未通过编译（两处占位符 `TraceIdUnused()`、
`contextLikeAlias` 导致整个包 `go vet`/`go test` 直接 build failed），修好后第一次真实执行暴露了
4 处断言与实现不符，已按实现的正确语义改正（详见「已知缺口」第 2 条）。

**迁移 SQL 已在隔离实例复验**：`127.0.0.1:3399`（数据目录 `.gotmp/mysql-data`）上库
`go_video_live_gateway` 的 4 张表 = `deploy/migrations/live-gateway/` 两个文件的 4 个 `CREATE TABLE`
（2026-09-21），`deploy/migrations/README.md` 记 `live-gateway | go_video_live_gateway | 2 | applied`。
本机 `127.0.0.1:3306` 是维护者真实库，全程未连接、未写入 —— **真实/共享实例仍未执行**，
上线前须由运维在目标实例执行。`model` ↔ SQL 另有逐列程序化比对（见「验证」）。

## 部署边界：长连接进程与 RPC 进程

**建议分离部署，且这是本服务的关键设计前提**（`docs/service-catalog.md` 把 `live-gateway` 列入
「按吞吐、资源或故障隔离需要优先独立」的服务，第三阶段独立）：

| 进程 | 职责 | 承载流量 | 扩缩容维度 |
|---|---|---|---|
| WS 接入层（适配器，**本期未实装**） | 持有 TCP/WebSocket 连接、读写帧、心跳、连接数配额 | 数十万级长连接、内存与 fd 敏感 | 连接数 / 内存 |
| RPC 面（`livegateway.v1.rpc`，`ListenOn: 0.0.0.0:8121`） | 鉴权、租约/路由/配额判定、广播扇出编排、审计落库 | 短平快 gRPC、CPU 与 DB/Redis 往返敏感 | QPS |

两者**不共享进程内状态**，通信只经 Redis（租约/订阅集合/去重窗口）与 Kafka：
接入层因此可以按连接数独立重启而不影响 RPC 面，RPC 面发布也不会踢掉在线连接。
`LiveGateway.WsListenOn`（默认 `0.0.0.0:8122`）是接入层的约定端口，
`ServiceContext` 不监听它——本轮禁止新增依赖，而仓库内没有任何 websocket 库，
所以接入由 `internal/connection` 的 `Manager` 接口 + 显式 stub 承接（该目录尚未创建，见缺口）。
同进程部署（一个二进制同时 listen 8121/8122）在开发环境可行，但它会让「连接数」和「QPS」
两个扩缩容维度互相牵制，**生产环境不要这样做**。

## 数据分层（本契约最重要的约束）

```
Redis (CacheRedis，易失、可重建)                MySQL go_video_live_gateway（少量表）
├─ 连接租约 lease_id → {mid,room_id,node,ttl}   ├─ live_gw_room_route     重启后重建路由表
├─ 房间订阅集合 room → set(conn_id)             ├─ live_gw_access_quota   配额配置（事实源）
├─ 心跳计数与最后心跳时间                        ├─ live_gw_broadcast_log  广播审计（只存摘要）
├─ 广播去重窗口 / 限流计数                       └─ live_gw_reconnect_ticket 票据审计+撤销名单
└─ 重连票据有效位（一次性 GETDEL）
   ↑ 在线状态唯一事实源                            ↑ 只存「审计」与「跨进程重启恢复」两类投影
```

- **在线状态事实只在 Redis**：`ListRoomConnections`、在线人数、订阅成员全部读 Redis，
  MySQL 里没有一张「每连接一行」的表，这是刻意的。
- **路由表是可重算投影**：清库后由各节点重新上报即可恢复，故允许与其它投影一起截断。
- **配额配置不是投影**：`live_gw_access_quota` 是运营写入的事实源，回收任务**不得**连带清空
  （迁移脚本注释里逐条写明了这一点）。
- **审计流水可截断**：`live_gw_broadcast_log` / `live_gw_reconnect_ticket` 按保留期清理
  （`BroadcastLogRetentionDays`），丢失只丢排障线索，不影响业务正确性。

## 方法与契约（22 个 RPC ↔ 表 ↔ 幂等键）

「主存储」列标 `Redis` 的方法**不写 MySQL**；标 `route`/`quota`/`log`/`ticket` 的对应
`live_gw_*` 表。幂等键是下一轮实现必须落实的约束，不是描述。

| 方法 | 类型 | 主存储 | MySQL 表 | 幂等键 / 去重依据 |
|---|---|---|---|---|
| `AcquireConnectionLease` | 写 | Redis 租约 | `route` + `ticket`（校验/占位） | `request_id` + `(node_id, conn_id)` 唯一租约 |
| `RenewConnectionLease` | 写 | Redis `EXPIRE` | — | `lease_id`（条件续租：仅 ACTIVE） |
| `ReleaseConnectionLease` | 写 | 删租约 + 退订 | — | `lease_id`（重复释放回 `already_released`，不报错） |
| `GetConnectionLease` | 读 | Redis | — | 只读 |
| `ReportClientHeartbeat` | 写 | 心跳计数/时间 | — | `(lease_id, seq)` 单调递增，旧 seq 不回退状态 |
| `ListRoomConnections` | 读 | Redis `SCAN` | — | 只读（`ConnectionScanLimit` 夹住扫描量） |
| `IssueReconnectTicket` | 写 | 票据有效位 | `ticket`（插入审计行） | `request_id`；`ticket_hash` 唯一 |
| `RedeemReconnectTicket` | 写 | `GETDEL` 有效位 | `ticket.Consume`（条件更新） | `request_id` + 一次性 `ticket_hash` |
| `RevokeReconnectTicket` | 写 | 删有效位 | `ticket`（置 REVOKED） | `ticket_id`，或 `(room_id, mid)` + `request_id` |
| `JoinRoom` | 写 | 订阅集合 | `route`（读路由） | `request_id` + `(lease_id, room_id)` |
| `LeaveRoom` | 写 | `SREM` | — | `(lease_id, room_id)`（不在集合里是成功） |
| `GetRoomRoute` | 读 | 读缓存 | `route` | 只读 |
| `ListRoomRoutes` | 读 | 连接数读数 | `route` | 只读（分页 `clampPage` 夹取） |
| `DrainRoomRoute` | 写 | 路由状态位 | `route` | `request_id` + `expected_version` 乐观锁 |
| `BroadcastToRoom` | 写 | 去重窗口 + 限流 | `broadcast_log` + `route` | **`(room_id, message_id)`**（本服务核心幂等键） |
| `SendToUser` | 写 | 目标连接租约 | `broadcast_log` | `(room_id, target_mid, message_id)` |
| `ForwardDanmaku` | 写 | 去重窗口 | `broadcast_log` | `(room_id, message_id)` |
| `ForwardSystemEvent` | 写 | 去重窗口 | `broadcast_log`（`room.close` 联动 `route`，须两步转状态） | `(room_id, event_id)` |
| `KickConnection` | 写 | 删租约 + 删有效位 | `ticket`（置 REVOKED）+ `broadcast_log` | `request_id`；租约置 KICKED，`ban_until` 只在 Redis（无 MySQL 列） |
| `ListBroadcastLogs` | 读 | — | `broadcast_log` | 只读（时间窗 + 收窄维度，同 audit 的查询硬约束思路） |
| `GetAccessQuota` | 读 | 解析缓存 | `access_quota` | 只读（按 `QuotaScopeChain` 逐层回退） |
| `UpsertAccessQuota` | 写 | 失效缓存 | `access_quota` | `request_id` + `expected_version` 乐观锁 |

### 广播门禁顺序（`BroadcastToRoom`，顺序即契约）

1. 参数：`room_id > 0`、`message_id` 非空、`payload` 不超 `MaxPayloadBytes`；
2. 连接三元组：票据/租约的 `(mid, room_id, 有效期)` 必须一致，不一致 → `DROP_REASON_BAD_TICKET`；
3. 角色归一：`NormalizeRole`，客户端自报一律 `VIEWER`；
4. 权限矩阵：`RoleAllowedToSend(role, kind)` 是唯一依据 → 否则 `DROP_REASON_PERMISSION_DENIED`；
5. 房间可广播：`live-room` 报关闭 → `DROP_REASON_ROOM_CLOSED`；未配置下游 → **显式报错**不静默；
6. 配额限流：`Resolve(scope...)` → 超 QPS `DROP_REASON_RATE_LIMITED`；
7. 幂等：先占 Redis 去重窗口（`SETNX` + 分钟级 TTL），未命中再落 `uniq_room_message`，
   重复 → `DROP_REASON_DUPLICATED`；
8. 扇出：无路由 `DROP_REASON_NO_ROUTE`、无订阅者 `DROP_REASON_NO_SUBSCRIBER`；
   `require_reliable = true` 时**必须显式失败**，不得降级为尽力而为；
9~11. 审计落库（摘要不存正文）→ 回 `accepted`/`drop_reason` → 错误映射：
   鉴权/参数/依赖故障返回 `error`（gRPC 层可见），可丢弃类才走 `accepted + drop_reason`。

## 状态机（实现与测试都以此为准，`model/errors.go` 单点实现）

```
租约 leaseTransitions   ACTIVE → {ACTIVE, EXPIRED, RELEASED, KICKED}   · EXPIRED → {ACTIVE}
                       RELEASED / KICKED = 终态（无出边）
票据 ticketTransitions  ISSUED → {USED, REVOKED, EXPIRED}              · 三者皆终态
路由 routeTransitions   SERVING → {DRAINING}
                       DRAINING → {SERVING, OFFLINE}   OFFLINE → {SERVING}
```

- **禁止 `SERVING → OFFLINE` 捷径**：必须先 `DRAINING`（只出不进）排空，否则在线连接被硬切，
  观众看到的是「莫名掉线」。`IsValidRouteTransition` 不给任何绕过口子；
  房间关闭也要走 `SERVING → DRAINING → OFFLINE` 两步条件更新（为 `room.close` 开例外属契约变更，需评审）。
- **票据一次性**：`USED` 与 `REVOKED` 互斥且都无出边。已使用的票据再兑换必须失败，
  这是防重放的全部依据；状态机 + Redis `GETDEL` 双保险。
- **`KICKED` 是终态**：被踢连接不能靠续租复活，复活只能走「吊销期外重新 `AcquireConnectionLease`」；
  反之 `EXPIRED → ACTIVE` 允许（客户端带新凭据重连续租）。
- 心跳超时判定统一走 `Expired(at, now)`：`at == 0` 表示未设置、**不过期**，
  避免「冷启动无心跳」被误判为超时踢人。

## 配额模型与继承

`live_gw_access_quota` 一行 = 一个作用域的配置，`uniq_scope(scope, scope_id)` 保证唯一。
读取按 `QuotaScopeChain` 逐层回退，**每个维度的第一个非零值生效**（`applyQuotaLayer`）：

```
USER  → GLOBAL          （不回退 ROOM/NODE：否则"某房间某用户"的降配会被节点配置覆盖，
                          运营降配失效，且排障时无法解释这条连接为什么被拒）
ROOM  → GLOBAL
NODE  → GLOBAL
GLOBAL = 终态；再往下是 config 里的 Default* 兜底值
```

- **`0` 表示继承，不是「关闭」**：所以 `allow_guest` 无法表达「显式禁止游客」（见缺口）。
- 所有值写入前过 `CheckQuotaBounds`，TTL 类再被夹到 `[MinLeaseTTLSeconds, MaxLeaseTTLSeconds]`；
  生效结果与命中层级由 `Resolve` 返回 `EffectiveQuota`（`HitScopes` 元素形如 `3:7`，即
  `scope:scope_id`），**只用于响应与日志，不落库**。

## 表与迁移文件

| 表 | 列数 | 迁移文件 | model |
|---|---|---|---|
| `live_gw_room_route` | 11 | `deploy/migrations/live-gateway/000001_create_route_and_quota_tables.sql` | `model/live_gw_room_route.go` |
| `live_gw_access_quota` | 17 | 同上 | `model/live_gw_access_quota.go` |
| `live_gw_broadcast_log` | 16 | `000002_create_broadcast_log_and_ticket_tables.sql` | `model/live_gw_broadcast_log.go` |
| `live_gw_reconnect_ticket` | 21 | 同上 | `model/live_gw_reconnect_ticket.go` |

索引要点：`uniq_room_message(room_id, message_id)` 是广播幂等的最后防线；
`uniq_ticket_hash`/`uniq_ticket_id` 双唯一键；`uniq_request_id` 兜住写接口重试；
路由表 `idx_state_room`（按状态翻页）+ `idx_primary_state`（节点排障）。
审计两表**没有 `version` 列**：它们只做条件插入与终态流转，model 侧走 `bumpVersion=false`，
加列只会让热表白白变宽。`live_gw_broadcast_log` 也**没有 `mtime`**（写一次即终结）。

并发更新统一「条件 UPDATE + `RowsAffected` 判定」（`conditionalUpdate`），
带 `version` 的表自动 `version = version + 1`、并总写 `mtime`；**不做读改写**，
避免排空/预热并发时把在线计数写脏。

## 配置

```yaml
Name: livegateway.v1.rpc
ListenOn: 0.0.0.0:8121                 # 8121 为本服务 RPC 面端口约定（全仓唯一）
DataSource: ...@tcp(127.0.0.1:3306)/go_video_live_gateway?charset=utf8mb4&parseTime=true&loc=Local
CacheRedis: {Host: 127.0.0.1:6379, Type: node}   # 必须叫 CacheRedis：RpcServerConf 内嵌了同名
                                                 # RedisKeyConf，叫 Redis 会让 conf.Load 报 conflict key redis
LiveRoomRPC: {Etcd: {Hosts: [127.0.0.1:2379], Key: liveroom.v1.rpc}, NonBlock: true}  # optional
LiveGateway:
  WsListenOn: 0.0.0.0:8122             # 接入层约定端口，ServiceContext 不监听（见「部署边界」）
  DefaultLeaseTTLSeconds: 30  MaxLeaseTTLSeconds: 300  MinLeaseTTLSeconds: 10
  DefaultTicketTTLSeconds: 120 MaxTicketTTLSeconds: 600
  HeartbeatMaxSkewSeconds: 300 ReconnectGraceSeconds: 60
  MaxPayloadBytes: 32768 PayloadDigestBytes: 32 BroadcastLogRetentionDays: 30
  DefaultRoomBroadcastQps: 200 DefaultUserBroadcastQps: 5 DefaultMaxRoomConnections: 50000
  AllowGuestByDefault: true RoomRouteCacheTTLSeconds: 10 QuotaCacheTTLSeconds: 60
  ConnectionScanLimit: 500
Kafka: {Brokers: [127.0.0.1:9092], Group: live-gateway.v1, SubscribeTopics: [live.state.v1, danmaku.sent.v1]}
```

## 验证

```powershell
cd d:\hilihili\backend\go-video; $env:GOCACHE="$PWD\.gotmp\gocache"; $env:GOTMPDIR="$PWD\.gotmp\gotmp"
go build ./services/live-gateway/... ; go vet ./services/live-gateway/...
go test ./services/live-gateway/... -count=1
```

`go test` 不连 MySQL/Redis/etcd/Kafka：

- `model/livegw_pure_test.go`（19 个测试）锁住纯函数契约：
  心跳/租约超时判定（含 `0` = 未设置不过期）、可注入时钟 `SetClock` 与 `NowUnix`/`NowMilli` 一致性、
  三个状态机的逐条边、权限矩阵逐格钉住（含「观众不得发处置类」）、`KindRequiresTrustedSender`、
  `TicketHash` 只出 64 位小写十六进制且**不含明文**、`clampPage` 边界（`ps` 超限夹取且 offset 用夹取后
  的值）、`replica_nodes` 编解码（`nil` → `"[]"`、空节点 ID 报错）、配额继承链顺序与
  「只有非零值命中」语义、`CheckQuotaBounds`、`isDuplicateErr`。
- `internal/config/config_load_test.go` 用 `conf.Load` **真实加载** `etc/` 下每个 yaml
  （字段名若写成 `Redis`，Load 会直接失败，所以「跑通」本身就是对字段名的断言），
  并断言 `DataSource` 非空且包含 `go_video_live_gateway`、Redis 类字段 `Host` 非空、
  示例配置里 `Token` 为空、TTL/保留期等参数不为 0。
- `model` ↔ 迁移 SQL 做了逐列程序化比对：四表列名、列序与 struct `db` tag /
  `liveGw*Columns` 查询串三者完全一致（「model 有而 SQL 缺」与「SQL 有而 model 不用」均为空集）。

## 已知缺口 / 待评审

1. **logic 层单测的证据强度**：`internal/logic` 当时只有 5 个用例文件（`broadcast_fanout_test.go`/
   `broadcast_logs_read_test.go`/`forward_paths_test.go`/`lease_lifecycle_test.go`/`reconnect_ticket_test.go`，
   共 125 个顶层用例）+ 共用替身 `fakes_test.go`（见「本期落地范围」）；现在这批是
   **13 个用例文件 + 替身，250 个顶层 / 230 个子用例**（逐文件见「测试覆盖」节），
   覆盖鉴权矩阵、票据一次性消费、去重回放、配额继承、扇出与读侧。但这批断言在本轮之前**从未真正执行**
   （包级编译失败），首次运行改正了 4 处「断言与实现不符」：
   ① 一条合法弹幕打的是**两次**限流（房间层 `bqps` + 用户层 `dqps`），不是断言里写死的一次；
   ② 换个 `sender_mid` 复用旧 `message_id` 的重放**不会**复述首次结论——鉴权在去重之前，
   正确结论是 `PERMISSION_DENIED`（测试原本把它写成期望 `accepted`，那才是错的方向）；
   ③ `room.close` 的重放判 `NO_ROUTE` 而不是 `duplicated`（路由检查在第 5 步、去重在第 6 步，
   首次已把路由收敛到 OFFLINE），要验 `duplicated` 得用无路由副作用的事件（`moderation.mute`）；
   ④ `event_id` 含空白/超 64 字节的用例被 `requireEventID` 挡在参数门禁，测不到想测的分支。
   另：`model.ErrUnknownEventType`/`ErrEmptyEventType` 此前在 `ForwardSystemEvent` 里没被使用
   （未知 `event_type` 走 `fmt.Errorf` 裸错误），现已在 `forwardsystemeventlogic.go:86-92` 分别包成
   两个哨兵，断言由 `forward_paths_test.go:TestForwardSystemEventUnknownEventTypeNeedsSentinel` 生效。
2. **`internal/repository` 已建、`ServiceContext` 已接线**（`Leases`/`Store`/`Rooms`/`Fanout`/`Signer`
   六个依赖，DSN/Redis 缺配置时给 `unavailable*` 实现并在调用点显式报错，不起「看起来活着」的进程）：
   剩余缺口是这些依赖的**真实对端**——见下一条的扇出通道与 live-room zRPC。
3. **`internal/connection` 与 `internal/consumer` 目录不存在**：
   `Manager` 接口（WS 连接持有与扇出）和 Kafka 消费入口都还没有承载体，
   因此 `BroadcastToRoom` 的「扇出」一步在实现前必然落到 `DROP_REASON_TRANSPORT_UNAVAILABLE`。
4. **无 WebSocket 服务端**：仓库禁止新增依赖且无任何 websocket 库，
   `WsListenOn` 当前只是约定；接入协议（握手 URL、帧格式、心跳间隔）**是否要落到 `gateway/app`
   的路由面**需维护者拍板——本服务只提供 RPC 面，不拥有 HTTP 边缘。
5. **`ErrLiveRoomNotConfigured` 尚未定义**：`LiveRoomRPC` 未接线时，logic 注释里引用的是
   规划中的哨兵错误，实装 `repository` 时需一并补（当前 `model/errors.go` 无此符号）。
6. **迁移未在 MySQL 执行**（见「本期落地范围」），且未验证 `JSON` 列在
   `replica_nodes` 上的实际写入；`JSON_CONTAINS(replica_nodes, JSON_QUOTE(?))` 无多值函数索引，
   该索引需 MySQL 8.0.17+ 而镜像锁在 `8.0`，故按「运营低频查询」接受全表扫（迁移注释已写明）。
7. **`allow_guest` 无法表达显式禁止**：`0` = 继承、`1` = 允许、`2` 被 `CheckQuotaBounds` 拒绝，
   因此「某房间明确禁止游客」只能靠不给该作用域配行 + 全局兜底，语义有缺口（需 proto + DDL 变更）。
8. **`AccessQuotaInfo` 没有命中层级字段**：`GetAccessQuota` 无法把「这份配额来自哪一层」
   回给调用方，排障可解释性受限（`Resolve` 已返回 `EffectiveQuota.HitScopes`，透出需改 proto）。
9. **单播不可按目标检索**：`live_gw_broadcast_log` 无 `target_mid` 列，`SendToUser` 只能靠
   调用方为每个目标生成不同 `message_id` 来区分；若运营需要「按用户查收到的单播」，需加列 + 索引。
10. **无 HMAC 签名密钥配置位**：`IssueReconnectTicket` 需要服务端签名密钥，
    `config.go` 目前只有 `ticket_ttl_seconds`，需补 `Security` 段（且密钥只写环境变量名，
    字面量不进仓库，与 audit 服务同规则）。
11. **待评审：房间关闭是否允许跳过 `DRAINING`**。当前 `routeTransitions` 一律要求
    `SERVING → DRAINING → OFFLINE` 两步，好处是不存在硬切连接的代码路径，代价是关房间时
    路由收敛多一次往返、期间 `BroadcastToRoom` 仍可能返回 `accepted`（房间已关但路由还在 SERVING）。
    若维护者认为可接受，需在 `model/errors.go` 显式加一条带条件的例外并补测试。
12. **本轮不做**：gRPC 错误码到 `google.rpc.ErrorInfo` 的映射、广播载荷对象存储旁路、
    连接数指标的 Prometheus 上报。
13. **「越权必须留 DENIED 审计」这条承诺目前落不下**（缺陷，需改生产代码，不能靠改断言收口）：
    判定链把鉴权排在去重之前的理由就是「复用旧 `message_id` 的越权探测也必须留痕」，
    但 `live_gw_broadcast_log` 的唯一键恰恰是 `uniq_room_message(room_id, message_id)`
    （`deploy/migrations/live-gateway/000002_create_broadcast_log_and_ticket_tables.sql:84`）。
    同一 `message_id` 的首次受理行已占位，随后的 DENIED 行撞唯一键后
    `model/live_gw_broadcast_log.go:121-124` 按「重复投递」返回 `(0, nil)`——不报错、不留痕，
    于是「换发送者复用旧 message_id」这一条越权路径仍然无痕。
    修法二选一：审计定位串带上发送者/结论维度（要动唯一键，属迁移 + 契约评审），
    或把拒绝证据写进不与广播幂等冲突的第二张账。
    断言以 `t.Skip` 留在 `forward_paths_test.go:TestForwardDanmakuEscalationAuditRow`。
14. **`internal/logic` 还剩 3 个用例是 `t.Skip` 状态，不要把「有 5 个用例文件」读成「断言都在跑」**：
    每条 skip 的第一参数就是缺陷定位（`缺陷已上报: <file:line> —— <结论>`），改动生产代码后
    删掉那行 `t.Skip` 即可让断言生效。这 3 条都不是「改一行就能收口」的，各自卡在数据模型、
    身份通道或口径决策上：

    | 跳过用例 | 为什么不能只改代码 |
    |---|---|
    | `TestForwardDanmakuEscalationAuditRow`（`forward_paths_test.go:481`） | 见上一条第 13 项：DENIED 审计行撞上 `uniq_room_message` 被静默丢弃，修法要么动唯一键（迁移 + 契约评审），要么加第二张拒绝证据账 |
    | `TestListBroadcastLogsRoomOwnerReadPathIsReachable`（`broadcast_logs_read_test.go:160`） | 主播读自己房间的审计这条路径**结构性不可达**：`caller.go:87-96` 只承认 attested OPERATOR/SERVICE，其余角色（含 ANCHOR）按未归因处理并把 `mid` 清零，于是 `listbroadcastlogslogic.go:81-98` 的「按 mid 问 live-room 归属」分支永远进不去。修法要先定「谁替主播背书 mid」——全仓今天没有任何一方注入 `x-gw-caller-*`（第 3、4 项），单在本服务放开 ANCHOR 等于让调用方自报角色换读权限 |
    | `TestForwardSystemEventUnwiredRoomGateStillFails`（`forward_paths_test.go:957`） | 口径冲突未拍板：`forwardsystemeventlogic.go:53-55` 声明「live-room 未接线时不因此拒发可信内部事件」，而 `broadcastcore.go:114-118` 对未接线一律返回 `ErrLiveRoomNotConfigured`。两侧各有理由（fail-open 保开播事件必达 vs fail-closed 不向不存在的房间投递），属第 11 项同类的可用性决策，不由实现方单方面定 |

    已收口的不再占 skip：本轮把原来 13 条里的 10 条改成了**在跑的断言**（对应的生产修法与用例名
    逐条见下一条）。判据统一是「取消 skip 后，把修法还原就一定会红」——判别力靠用例内部
    钉住前提（例如断言凭据确实被读过、兜底确实查了事件列），不靠改生产代码试探。
15. **本轮已收口的 10 条（原 `t.Skip` 用例 → 现在的修法与断言）**：

    | 用例（现在会跑） | 生产修法 |
    |---|---|
    | `TestBroadcastSenderCredentialRoleMustBeRecheckedAgainstMatrix` | 矩阵判定抽成 `helpers.go:454 matrixDrop`，租约（`helpers.go:425`）与票据（`:443`）两支换成真角色后**复检**；归因主体仍免检（凭据只用于记账） |
    | `TestForwardSystemEventSourceServiceIsForgeableByOperator` | 审计来源只认归因：`forwardsystemeventlogic.go:103-114`，自报 `source_service` 与归因不符只告警、不入库 |
    | `TestBroadcastPayloadLimitMustClampToProcessCap` | `broadcastcore.go:352 maxPayloadForRoom` 把生效配额夹到进程上限（取更严的一侧），配错一层不再等于无上限 |
    | `TestBroadcastRateLimitedDropMustBackfillDedupOutcome` | `broadcastcore.go:157-164`：限流丢弃后回填幂等结论，同一 `message_id` 重试拿到 RATE_LIMITED 而不是「并发中，请重试」 |
    | `TestBroadcastEventDedupFallbackLooksUpEventColumn` | 兜底回读按去重维度给列：`broadcastcore.go:404 findMessageRow` / `:412 findEventRow`（原先固定查 `message_id`，事件路径永远落空）。对偶用例 `TestBroadcastMessageDedupFallbackLooksUpMessageColumn` 钉住消息侧不许跟着改错 |
    | `TestForwardSystemEventCarriesEventIdentityToTransport` | `broadcastcore.go:172` 交给扇出层的 `MessageID` 用 `in.auditMessageID()`，系统事件不再是一条空 `message_id` 消息 |
    | `TestForwardDanmakuReplyOmitsMessageID` | `forwarddanmakulogic.go:107` 回填 `message_id`，调用方拿到幂等句柄 |
    | `TestForwardSystemEventReplyDropsTargetedConnections` | `forwardsystemeventlogic.go:168` 回填 `targeted_connections` |
    | `TestBroadcastTargetRolesMustRespectMaxTargetRoles` | `broadcasttoroomlogic.go:134 lgwBroadcastRoles` + `repository.ErrTooManyTargetRoles`：`target_roles` 长度按 `config.go:112 MaxTargetRoles` 拒越界（此前该配置在全仓无引用点） |
16. **`target_roles` 角色名没有词表校验**（缺口，未写 skip 用例）：上一条只夹取了长度，
    列表里的字符串仍原样交给扇出层。子通道有 `NormalizeTopics` 白名单（拼错就拒，因为
    「静默收不到」比报错糟糕），角色侧却没有可信词表可比：`model` 的角色是 int32
    （`model/errors.go:116-135`，只有 `NormalizeRole(int32)`），字符串→角色的映射归谁定
    属于尚未存在的接入层（第 3 项）。现在拼错一个角色名的结果是「发给 0 个连接 + accepted」，
    要收口需先定 `target_roles` 的取值表归属，不在本服务凭空发明。
17. **目标离线时 `SendToUser` 是一个不做鉴权的「在线态探针」**（缺陷，需改判定次序，本轮只用例钉住现状）：
    `sendtouserlogic.go:74` 先读 Redis 在线态，`:88-98` 命中集为空就当场 `return` `result=NO_LEASE`，
    而鉴权链在它之后（`runBroadcast` 的第 4 步，`broadcastcore.go:99-110`），于是这条路径上
    `runBroadcast` 一次都没跑。后果两条：
    ① 任何能连到本 RPC 面的调用方，**无需任何凭据**就能用「`NO_LEASE` vs `DENIED`/受理」判别
       「某人此刻是否在该房间在线」——这是一条在线态侧信道。本服务没有任何 gRPC 拦截器
       （全服务 `grep UnaryInterceptor` 无命中），归因只来自 metadata
       `x-gw-caller-attested`/`-role`/`-mid`/`-service`（`internal/logic/caller.go:24-27`），
       而 `internal/config/config.go:103` 明确写着「一旦接入层补上 `x-gw-caller-*` metadata，
       这里必须改 true 才算权限闭环」——即当前接入面还不存在（第 3、4 项），metadata 由调用方自报；
    ② 同一请求若目标在线，越权会落一条 `state=DENIED` 审计；目标离线时 `:88-98` 分支里没有
       `auditDrop`，**一条审计都不落**，于是「探测别人在不在线」在取证面上完全无痕，
       与第 13 项同属「拒了但查不到」。
    钉住用例：`unicast_sendtouser_test.go:TestSendToUserOfflineShortCircuitSkipsAuthorisation`
    ——同一份无凭据的 MODERATION 单播，目标在线 → `DENIED` + 1 条 DENIED 审计，
    目标离线 → `NO_LEASE` + 0 条审计，用例断言两者结论必须不同。
    修法二选一：把鉴权提到目标定位之前（代价是目标不在线也要读一次凭据，而那本来就是 Redis 读），
    或在 `NO_LEASE` 分支落一条不与 `uniq_room_message` 冲突的探针审计。两者都是生产改动，本轮未动。
18. **单播幂等键与 proto 注释不一致**（契约缺口，与第 9 项同源）：`rpc/livegateway.proto:443` 写明
    `message_id` 的幂等键是 `(room, target, message_id)`，实现的去重键却是 `(room_id, message_id)`
    （`runBroadcast` 第 6 步 + `uniq_room_message` 兜底），`live_gw_broadcast_log` 也没有 `target_mid` 列。
    后果：同一调用方拿同一个 `message_id` 给同房间两个不同目标发定向消息，第二个目标**不会有任何
    `Deliver`**（被判为重放并复述首次结论），应答看起来仍然成功——这是「发出去了但对方没收到」的
    一条静默路径。钉住用例：`TestSendToUserIdempotencyKeyExcludesTarget`。
    本轮只保留既有约束（`sendtouserlogic.go:40-44` 已写进注释）：**调用方必须为每个 target 生成不同
    `message_id`**。真正的修法要同步改 proto 注释、加 `target_mid` 列并把唯一键改成
    `(room_id, target_mid, message_id)` ⇒ 迁移 + `./scripts/gen.sh live-gateway` + 契约评审。
19. **注释说「target 维度走 USER 链」，实现的 USER 层键是发送者**（口径缺陷）：
    `sendtouserlogic.go:46-47` 声明单播限流按 target 走 USER 链，而 `broadcastcore.go:323-336` 的
    USER 层只在 `UserScoped && senderMid > 0` 时生效，计数键是 `dqps:mid:<senderMid>`，
    配额解析也用 `QuotaScopeUser` + `senderMid`。后果：定向消息的配额实际挂在**发送者**身上——
    给同一个人连发多条不消耗目标侧额度，反过来一个高配额发送者可以把某个低配额接收者刷到爆
    （目标侧没有任何 per-target 上限）。钉住用例：`TestSendToUserRateLimitsSenderNotTarget`
    （目标降到 1 仍能连发两条且 `dqps:mid:<target>` 计数为 0；发送者降到 1 则第二条
    `RATE_LIMITED`，且 `result=UNSPECIFIED` + `deny_reason=RATE_LIMITED` 而不是 `DENIED`——
    限流可重试、拒绝不可重试，两者不许互相冒充）。
    修法要么实现真正的 per-target 键（需要新配额档位与配置面），要么把注释与本条口径改成
    「发送者维度」；本轮不凭空发明一个没有配置支撑的档位。

## 测试覆盖（2026-10-03 实测导出，全部离线）

数字来源：顶层用例＝`grep -cE '^func Test'` 扣掉 `TestMain`；子用例＝`grep -c 't.Run('`；
构造器覆盖＝枚举 `internal/logic` 里 `func NewXxxLogic(` 的名字，再逐个回查是否被 `*_test.go` 直接调用。
本节不转录任何旧文档的计数。

### 1. `internal/logic`（13 个用例文件 + 共用替身：250 顶层 / 230 子）

| 用例文件 | 顶层 | 子 | 钉住的判定链（取自各文件自身头部注释） |
|---|---|---|---|
| `access_quota_test.go` | 20 | 20 | 配额一对方法：写侧 `UpsertAccessQuota`、读侧 `GetAccessQuota`；每个拒绝分支都配一条放行对照 |
| `broadcast_fanout_test.go` | 31 | 23 | 房间广播链 `BroadcastToRoom → runBroadcast` 的门禁顺序与结论（重点不是 `accepted=true`） |
| `broadcast_logs_read_test.go` | 7 | 12 | `ListBroadcastLogs`——「为什么这条没到」的唯一取证入口 |
| `client_heartbeat_test.go` | 10 | 6 | `ReportClientHeartbeat`：推进 `last_heartbeat`/`max_seq`、抽样 QoE；最高频入口的失败方向与别的 RPC 相反 |
| `drain_room_route_test.go` | 20 | 9 | `DrainRoomRoute`——发布期节点优雅下线的唯一写入口，四条护栏 |
| `forward_paths_test.go` | 24 | 24 | 两条转发链 `ForwardDanmaku` / `ForwardSystemEvent`（共用判定链，只测差异） |
| `join_room_test.go` | 14 | 15 | `JoinRoom`：订阅登记 + 房间路由登记，含防订阅注入的执行点 |
| `kick_connection_test.go` | 18 | 18 | `KickConnection`：处置类写入口，判定方向与下发接口相反 |
| `lease_lifecycle_test.go` | 21 | 31 | 租约族 Acquire/Renew/Release/Get 完整判定链，「结论必须是真实原因」 |
| `leave_room_test.go` | 11 | 8 | `LeaveRoom`：退订（SREM 语义）与 `JoinRoom` 的三处差异 |
| `reconnect_ticket_test.go` | 42 | 50 | 票据族 Issue/Redeem/Revoke：`allowed=false` 必须给出准确 drop 与票据真实状态 |
| `route_reads_test.go` | 22 | 8 | `GetRoomRoute` / `ListRoomRoutes`：两个读入口的差别就是契约本身 |
| `unicast_sendtouser_test.go` | 10 | 6 | `SendToUser`（本轮新增）：参数门禁零存储读、目标集只算 ACTIVE 租约、不伪造送达数、离线短路（第 17 项）、幂等键不含 target（第 18 项）、限流作用在发送者（第 19 项）、载荷上限与广播共用、通道未接线、依赖故障透传 |
| `fakes_test.go` | 0 | 0 | 共用替身与脚手架，不含用例 |
| **合计** | **250** | **230** | 其中 3 条处于 `t.Skip`（见第 14 项） |

### 2. 其他层

- `model/livegw_pure_test.go`：19 顶层 / 2 子——纯函数契约，逐条清单见「验证」节。
- `internal/config/config_load_test.go`：1 顶层 / 1 子——`conf.Load` 真实加载 `etc/` 下每个 yaml。
- `internal/repository`（`leasestore.go`/`roomgate.go`/`signer.go`/`topics.go`/`transport.go`/
  `unavailable.go`）：**无离线单测**。logic 用例跑在替身上，Redis 键布局、真实 TTL 行为、
  管道原子性、Redis 与 MySQL 的漂移都不在被测路径上。
- `internal/svc`、`internal/server`（goctl 生成壳）：无用例。
- 本服务**没有** `internal/consumer` 目录，因此不存在消费者侧用例：`live.state.v1`/`danmaku.sent.v1`
  在本契约里是**出口**，由下游消费，本服务不实现消费闭环。

### 3. 构造器级覆盖：22 / 22

`internal/logic` 的 22 个 `NewXxxLogic`（对应 22 个 RPC）全部有直接驱动入口的用例，没有「只有间接断言」
的方法。探针口径见本节开头。

### 4. 替身与断言口径

- 替身语义对齐 `repository.RedisLeaseStore` 与 `model` 的同名方法契约（`fakes_test.go` 头部已声明理由：
  测试门禁禁止连任何 MySQL/Redis，且「依赖不可用必须显式失败」这类场景只有替身能造）。
- 错误注入是**逐方法**的（`failWith`），时钟可推进（`e.Clock.advance`），存储调用有 callLog，
  因此「拒绝分支一次依赖都不许碰」是可断言的而不是修辞。
- 判别力来自用例内部的前提对照（边界成对：1024/1025 字节；在线/离线；发送者/目标的限流键；
  两个 target 共用一个 `message_id`），**不靠改生产代码试探**。

### 5. 覆盖边界（不要把本节读成「已联调」）

- 用例不连 MySQL / Redis / etcd / Kafka，也不起 gRPC 服务器；`internal/server` 的壳与
  protobuf 编解码不在断言范围内。
- 扇出与接入层（`Fanout`/`Transport`）是替身：真实 WebSocket 握手、帧格式、心跳超时、
  跨节点转发与接入层回报的 `targeted_connections` 全部**未联调**（接入协议本身待拍板，见第 4 项）。
- 迁移是否在真实 MySQL 执行、`JSON` 列与函数索引的落地情况，以本文第 6 项为准；
  「验证」节里的 `model` ↔ SQL 比对是**程序化静态比对**，不是建表验证。
- 3 条 `t.Skip` 用例（第 14 项）在跑的红断言之外，仍是「已知缺陷的占位」，不要按用例文件数估算强度。

### 6. 验证命令

```powershell
cd d:\hilihili\backend\go-video; $env:GOCACHE="$PWD\.gotmp\gocache"; $env:GOTMPDIR="$PWD\.gotmp\gotmp"
go vet ./services/live-gateway/...
go test -p 1 -count=1 ./services/live-gateway/...
gofmt -l services/live-gateway
```

`-p 1` 是硬性要求：整树测试并发会撞上 Windows 页面文件上限（`errno=1455`，表现为链接期 OOM），
不是被测代码的问题。

## 开发约定

- `rpc/*.pb.go`、`internal/server/`、handler 由 goctl 生成，禁止手改；契约变更跑
  `powershell -File scripts/gen.ps1 -Service live-gateway` 重新生成。
- 可编辑源文件：`rpc/livegateway.proto`、`model/`、`internal/{logic,repository,consumer,config}/`、
  `etc/*.yaml`、`README.md`、迁移 SQL、`*_test.go`。
- 逐条连接状态永远不许进 MySQL；新增表前先回答「这张表是审计还是跨进程恢复投影」，
  两者都不是就不该有表。
