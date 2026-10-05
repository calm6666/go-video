# live-ingest

直播推流接入与流状态服务：推流密钥、接入节点、流状态机、断流/重连、流健康与 `live.state.v1` 事件的生产者。

当前状态：**契约 + goctl 生成 + model + 迁移 SQL + 配置装配 + 22 个 logic 方法实现 + Outbox 发布器全部落地**
（逻辑轮 2026-09-21；22 个 RPC 方法都有构造器级用例，`internal/logic` 为 12 个测试文件 /
191 条顶层用例 + 30 个子用例，其中 10 个用例文件、`fakes_test.go` 与 `testsupport_test.go`
是替身层与脚手架；明细见下方「测试覆盖」节。生产轮 2026-10-04 补上 `internal/publisher`，
用例数为 5 个文件 `28/5`（默认构建），带 `-tags liveingest_kafka` 时 `30/6`，口径见「测试覆盖」第 2 组）。
本服务的 logic 已无 `model.ErrNotImplemented` 桩，也没有处于 `t.Skip` 的用例
（原先唯一一条 `TestStreamStateMachine_StoppedClearsNodePointer` 随缺陷 #1 修复而启用，见已知缺口第 10 条）。

- **拥有数据**：推流密钥（含哈希与受控引用）、接入节点与配额、流状态机与 `seq`、断流/重连区间、健康采样、`live.state.v1` 事件与 Outbox、CDN 回调留证。
- **提供能力**：RTMP/SRT/WebRTC 接入鉴权、密钥签发/轮转/吊销、节点分配、流状态上报与查询、健康检查、事件位点与补偿、回调鉴权。
- **依赖**：MySQL（自有库 `go_video_live_ingest`）、Redis（`CacheRedis`）、Kafka（Outbox 投递：发布器已落地并在 `svc` 里接线，但发送端只在 `-tags liveingest_kafka` 构建下存在，且 `Kafka.Enabled` 示例值为 `false`）、CDN/媒体入口（外部，本轮为显式 stub）。
- **约束**：不保存长期明文推流密钥；流状态事件必须幂等并可追踪。

## 运行参数

| 项 | 值 |
|---|---|
| 入口 | `liveingest.v1.go`（单入口，见 `docs/commands.md` §5） |
| 配置 | `etc/liveingest.v1.yaml`（`internal/config/config_load_test.go` 用 `conf.Load` 真实加载 `etc/` 下每个 yaml） |
| 服务名 / etcd key | `liveingest.v1.rpc` |
| 监听 | `0.0.0.0:8118`（8120/8121/8122 已被 live-media/live-gateway 占用，本服务避开 peer 的 `8080` 冲突） |
| proto | `rpc/liveingest.proto`，`package liveingest.v1`，生成物在 `rpc/*.pb.go` |
| 构建标签 | `liveingest_kafka`（两个生产者接线之一，另一个是 `playback_kafka`；默认构建不带 `kq`，见下方「事件发布」节） |
| 事件生产 | `live_ingest_outbox` → topic `live.state.v1`，分区键 `stream_id`；开关 `Kafka.Enabled`（示例值 `false`） |
| 重新生成 | `./scripts/gen.ps1 -Service live-ingest`（AGENTS.md §4：框架代码只能由生成命令产出） |

## 与 live-room 的边界（本期强约束）

| 事实 | 所有者 | 本服务的做法 |
|---|---|---|
| 房间业务状态（待完善/可开播/直播中/关闭/禁播）、场次生命周期、开播门禁 | `live-room` | 只保存 `room_id` / `session_id` / `anchor_mid` 引用：不校验、不回查、**不写 live-room 任何表**，也不建跨库外键 |
| 推流密钥（明文/哈希/Vault 引用）、轮转与吊销 | `live-ingest` | `live_stream_key`，明文一次都不入库 |
| 接入节点、容量配额、流→节点分配 | `live-ingest` | `live_ingest_node` / `live_node_assignment` |
| 流状态机、`seq`、断流/重连、健康 | `live-ingest` | `live_stream*` 四张表 |
| 转码/录制/回放 | `live-media` | 只被事件与 `stream_id` 反查触达 |
| 长连接广播（进房/退房/弹幕通道） | `live-gateway` | 同上 |

跨服务只传业务主键（`room_id` / `session_id` / `stream_id` / `node_id` / `mid`），`liveingest.proto` 不 import 任何其他服务的 proto。
`live-room` 的 zRPC client 本轮**不构造也不 import**（其生成包在并行开发中）；配置位 `LiveRoomRPC` 已在 yaml 里注释保留，接入时在 `internal/svc/servicecontext.go` 装配。

## RPC 方法（22）

`request_id` / `report_id` / `nonce` 是写接口的幂等键；重放返回首次结果并置 `replayed=true`，**不产生新事件、不重复推进 `seq`**。
分页统一 `pn`/`ps`（夹取到 `LiveIngest.MaxListPageSize`），事件类查询用 `limit`（夹取到 `MaxEventPageSize`）；时间统一 Unix 秒。

### 推流密钥（6）

| 方法 | 幂等键 | 说明 |
|---|---|---|
| `IssueStreamKey` | `request_id` | 签发密钥。入库只有 SHA-256 哈希 + `key_ref`（Secret/Vault）+ 末 4 位辨认串；明文只在本响应出现一次，`replayed=true` 时 `plaintext_key` 恒为空串（明文不可找回，需改用 `RotateStreamKey`） |
| `VerifyPublishAuth` | `request_id`（`create_stream=true` 时必填） | 接入入口建连鉴权：比对哈希、协议位、有效期、并发配额；可选建档 `IDLE` 流（分配 `stream_id`，`seq=0`，不产生事件） |
| `RotateStreamKey` | `request_id` | 轮转：新密钥 `ACTIVE`，旧密钥进 `ROTATING` 并写 `grace_until`，宽限期内旧 URL 可重连 |
| `RevokeStreamKey` | `request_id` | 吊销（不可逆终态），可级联停止进行中的流（`stop_reason=4`，`source=admin`） |
| `GetStreamKey` | — | 密钥元数据；永不回显明文或 `key_hash` |
| `ListStreamKeys` | — | 分页列表；非 admin 强制收敛到本人 `anchor_mid` |

### 接入节点（5）

| 方法 | 幂等键 | 说明 |
|---|---|---|
| `UpsertIngestNode` | `node_id`（自然键 upsert） | 注册/心跳（运维面）。`heartbeat_only=true` 只刷新计数与心跳，未注册节点不静默建档 |
| `ListIngestNodes` | — | 分页查询，`health_score` 降序 |
| `AssignIngestNode` | `request_id` | 就近 + 配额 + 健康分打分分配；支持迁移（旧记录置 `MIGRATED`）。分配记录与 `active_streams` 变更同事务 |
| `ReleaseIngestNode` | `request_id` | 释放占用并减配额 |
| `ListNodeAssignments` | — | 分配历史，容量对账与排障 |

### 流状态与健康（7）

| 方法 | 幂等键 | 说明 |
|---|---|---|
| `ReportStreamState` | `report_id` | 状态机 CAS 推进（`state` + `seq` 双条件）+ 分配 `seq` + 同事务写事件与 Outbox；`from==to` 是幂等 no-op，不占 `seq` |
| `GetStreamState` | — | 按 `stream_id` 或房间的活跃流；查不到返回 `found=false` 而非报错，便于 live-room 安全预检 |
| `ListStreams` | — | 巡检与断流扫描（`last_heartbeat_at` 升序，最可疑在前） |
| `CloseStream` | `request_id` | 强制停流 → `STOPPED`，同时释放密钥活跃指针与节点配额 |
| `ReportStreamHealth` | `report_id` | 采样上报：留点 + 回写最新字段；连续越界可触发 `INTERRUPTED`（`source=health`） |
| `GetStreamHealth` | — | 当前判定 + 窗口聚合 + 最近采样点（上限 `MaxSamplePoints`） |
| `ListStreamInterruptions` | — | 断流与重连区间；`total` 受 `CountHardLimit` 约束，超限返回 `-1` |

### 事件位点与回调（4）

| 方法 | 幂等键 | 说明 |
|---|---|---|
| `ListStreamEvents` | — | 按 `seq` 游标（`after_seq`）拉事件，回带 `max_seq` 供消费方判断是否追平 |
| `GetEventPublishCheckpoint` | — | Outbox 位点：已发布最大 id/时间、pending/failed 计数、最老待发布时间与 `lag_seconds` |
| `RetryFailedEvents` | `request_id` | 把 `FAILED` 重置为 `PENDING` 且清零 `retry_count`（否则发布器立刻再判失败） |
| `VerifyCdnCallback` | `nonce` | 回调鉴权留证：域名白名单 + 时间窗 + 签名比对 + `nonce` 唯一索引防重放；只回带 `suggest_state`，**不就地改状态** |

## 表与迁移（9 张表 / 3 个文件）

| 迁移文件 | 表 | 用途 | 承载幂等的唯一索引 |
|---|---|---|---|
| `deploy/migrations/live-ingest/000001_create_live_ingest_key_tables.sql` | `live_stream_key` | 密钥（只有 `key_hash`/`key_ref`/`key_tail`）+ `current_stream_id` 活跃指针 | `uniq_key_hash`、`uniq_request_id` |
| | `live_ingest_node` | 接入节点、容量配额、健康分、心跳 | 主键 `node_id` |
| | `live_node_assignment` | 流→节点分配记录（含迁移/释放历史） | `uniq_request_id` |
| `000002_create_live_stream_tables.sql` | `live_stream` | 一次推流会话 + 状态机 + `seq` + 最新健康 | `uniq_publish_request` |
| | `live_stream_interruption` | 断流区间（`ended_at=0` 为开放） | `uniq_start_event`、`uniq_stream_episode` |
| | `live_stream_health_report` | 健康采样点原始事实（无 `mtime`，append-only） | `uniq_report_id`（可 NULL） |
| `000003_create_live_stream_event_tables.sql` | `live_stream_event` | `live.state.v1` 事件事实来源（append-only） | `uniq_event_id`、`uniq_stream_seq`、`uniq_report_id`（可 NULL） |
| | `live_ingest_outbox` | 事务性 Outbox（payload = 信封 JSON） | `uniq_event_id` |
| | `live_cdn_callback` | 回调留证与判定结果（只存摘要） | `uniq_nonce` |

要点：

- 可选幂等列（`live_stream_event.report_id`、`live_stream_health_report.report_id`）用 **NULL** 而非空串：MySQL 唯一索引允许多个 NULL，内部迁移不互相冲突、非空 `report_id` 仍严格唯一（`model.nullableString`）。
- `live_stream_key.current_stream_id` 是「同一密钥至多一条非终态流」的活跃指针（CAS 抢占/释放），避免依赖 MySQL 生成列。
- 协议枚举（1/2/3，落 `live_stream.protocol`、`live_node_assignment.protocol`）与协议位图（1/2/4，落 `*_protocol_mask`）是两套取值，只能经 `model.ProtocolMask` / `model.ProtocolEnum` 转换。
- 时间列一律 `BIGINT` Unix 秒，`0` 表示「未发生/不适用」；无 `DATETIME`、无跨服务外键。
- 密钥、厂商签名、来源 IP 一律不存明文：只有 `CHAR(64)` SHA-256 摘要与 `key_ref` 引用（指向 Secret/Vault）。

## 流状态机

`live_stream.state` 取值与 `rpc.StreamState`、以及 live-room `ReportStreamStateReq.stream_state` **严格一致**（`model/errors.go` 的 `TestStreamStateNumberingPinned` 锁死）：1 `IDLE`、2 `PUBLISHING`、3 `INTERRUPTED`、4 `STOPPED`。

| from \ to | IDLE | PUBLISHING | INTERRUPTED | STOPPED |
|---|---|---|---|---|
| **IDLE** | no-op | ✅ 首帧到达 | ❌ | ✅ 未推流超时 / 密钥吊销 / 运营停流 |
| **PUBLISHING** | ❌ | no-op | ✅ 心跳或帧中断 | ✅ 正常下播 / 强制停流 |
| **INTERRUPTED** | ❌ | ✅ 宽限期内重连成功 | no-op | ✅ 宽限期耗尽 / 主动停流 |
| **STOPPED** | ❌ | ❌ | ❌ | 终态（无出边） |

- 判定入口：`model.CanTransitionStreamState(from, to)`；同态返回 `false`（幂等 no-op，由 logic 直接按「已应用」返回，不占 `seq`、不写事件）。
- 推进实现：条件 `UPDATE ... WHERE stream_id=? AND state=? AND seq=?` + `RowsAffected` 判定（`StreamModel.ApplyTransition`），`RowsAffected=0` → `ErrConcurrentUpdate`；读改写一律走 `LockByID`（`SELECT ... FOR UPDATE`，必须带事务会话）。
- 一次「推流会话」= 一个 `stream_id`：断流重连复用同一 `stream_id`（否则无法与 live-room 的 `seq` 守卫对齐），下播后重新开播是新 `stream_id`。
- 事件来源（`live_stream_event.source`）：`entry`（入口上报）、`health`（健康越界触发）、`cdn`（回调后由调用方推进）、`admin`（主播/运营/吊销级联）、`sweeper`（心跳超时、宽限期耗尽、密钥过期回收）。

## `live.state.v1` 事件契约

**信封**（`common/eventenvelope.Envelope`，即 `live_ingest_outbox.payload` 的完整 JSON）：

| 字段 | 本服务的取值 |
|---|---|
| `event_id` | ULID（`common/idgen`），与 `live_stream_event.event_id` 同源，消费方去重锚点 |
| `event_type` | `live.state`（`model.EventTypeStreamState`） |
| `schema_version` | `1`（`model.SchemaVersionStreamState`；字段变更必须递增） |
| topic | `eventenvelope.Topic("live.state", 1)` → **`live.state.v1`** |
| `occurred_at` | RFC3339（信封用字符串）；业务表用 Unix 秒 |
| `producer` | `live-ingest` |
| `aggregate_type` | `live_stream`（`model.AggregateTypeStream`） |
| `aggregate_id` | `stream_id` |
| `trace_id` | 与状态迁移同一 trace |
| `payload` | 下表 |

**payload 字段**（`internal/logic/streamstate.go` 的 `stateEventPayload`；除 `anchor_mid` 外与
live-room `ReportStreamStateReq` 一一对应）：

| payload 字段 | 类型 | live-room 入参 | 说明 |
|---|---|---|---|
| `stream_id` | string | `stream_id` | 推流会话 ID |
| `room_id` | int64 | `room_id` | 房间引用（消费方按此路由） |
| `anchor_mid` | int64 | 无（live-room 自带房间→主播绑定） | 主播 ID。只存在于 `live_stream` 行，而 inbox 要把断流/停播通知发给主播本人，因此由生产者提供；不进 live-room 入参，也不参与业务判定 |
| `session_id` | int64 | `session_id` | 0 表示由 live-room 按进行中场次解析 |
| `stream_state` | int32 | `stream_state` | 迁移后的状态（1/2/3/4，与 `to_state` 同值） |
| `stream_seq` | int64 | `stream_seq` | 该流单调序号（= `live_stream_event.seq`） |
| `occurred_at` | int64 | `occurred_at` | Unix 秒 |
| `interrupted_seconds` | int64 | `interrupted_seconds` | 本次中断秒数（关闭断流区间/停流事件携带；`omitempty`） |
| `reason` | string | `reason` | 摘要，不含密钥材料（`omitempty`） |
| `trace_id` | string | `trace_id` | 链路（`omitempty`） |

信封本身提供 `event_id`，对应 live-room 的 `event_id` 入参。

**幂等与反乱序（三重防线）**：

1. `event_id` 全局唯一（`uniq_event_id`）→ 消费方按它去重，重复投递返回「重复投递」。
2. `(stream_id, seq)` 唯一（`uniq_stream_seq`）→ 同一序号只能有一个事件；`seq` 只在合法迁移成功时 +1，消费方丢弃 `stream_seq` 小于已应用值的事件（乱序回退）。
3. `report_id` 唯一（`uniq_report_id`，NULL 不参与）→ 同一次上报只产生一个事件，重放不重复占号。

**同事务约束**：`live_stream.state` 迁移、`live_stream_event`、`live_ingest_outbox`（以及断流区间的开/关）必须在同一事务提交；MQ 故障不阻塞状态推进，由发布器按 `outbox.id` 升序异步投递（同流顺序）。补偿只能追加新 `seq` 的事件，事件表不可 UPDATE。

## 事件发布（`internal/publisher`，live.state.v1）

本服务是全仓第一个事件生产者（2026-10-04 起有第二个：`playback` 投 `playback.heartbeat.v1`）。
发布器把 `live_ingest_outbox` 的待发布行同步投到 `live.state.v1`，是「事务提交」与「下游收到」之间唯一的桥。
本包的循环是 `common/outbox` 引擎的**前代副本**，没有迁过去（三份已知差异记在
[common/outbox README 缺口 1](../../common/outbox/README.md)）；改动这里的退避、判死或计数时，
要同步评估引擎侧是否同改，避免两份实现继续分叉。

| 文件 | 角色 |
|---|---|
| `publisher.go` | `Publisher` 循环与单批语义：`New`/`Start`/`Stop`/`RunOnce`/`Stats`/`Running`，`Record`/`Store`/`Sender`/`Options` 四契约 |
| `outbox_store.go` | `Store` 的生产实现 `OutboxStore`（转调 `model.EventOutboxModel`）；`RequiredTopic()` 由 model 常量拼出，topic 字面量不在任何别处重复 |
| `params.go` | 配置 → 发布参数的唯一映射处：`OptionsFrom`、`SenderSettingsFrom`、`ValidatePublishKafka`（逐键点名）、`NewPublisher`（svc 唯一入口） |
| `kafkaruntime_kafka.go` | `-tags liveingest_kafka`：`kq.NewPusher(brokers, topic, kq.WithSyncPush())`，每 topic 一条写入通道 |
| `kafkaruntime_disabled.go` | 默认构建：`NewSender` 恒返回 `ErrKafkaRuntimeNotBuilt`，错误文本给可执行的下一步 |

发布一行的判定顺序（`RunOnce` 内按 `id` 升序串行）：

| 情况 | 结果 | 依据 |
|---|---|---|
| 行带 `Defect`（topic 不属于本服务、`aggregate_id` 空、payload 非法信封、信封 `event_id`/`aggregate_id` 与列不一致） | 直接 `MarkFailed` 判死，**不发送、不占重试次数** | 重试不会让一行列错的 payload 变对 |
| `Send` 成功 | `MarkPublished(mtime)` | 同步推送：`Send` 返回 nil 才代表 broker 侧受理 |
| `Send` 失败且 `retry_count+1 < MaxAttempts` | `MarkRetry`，`next_retry_at = now + BaseBackoff * 2^(n-1)`（上限 `MaxBackoff`，指数夹在 2^30 防溢出成负数） | 负的上次时间是「立即到期」，退避会静默失效 |
| `Send` 失败且尝试耗尽 | `MarkFailed`，只能由 `RetryFailedEvents` RPC 放行 | 判死不是丢事件：行仍在表里 |
| 读库失败或状态写库失败 | 中断本批并返回错误，`Stats()` 的 `lastBatchErr` 记下 | 「行还停在 pending」必须让上层看到；投递失败不返回错误 |

四条边界，别把这份清单读成更强的结论：

- **同步投递是语义要求，不是性能选择**。`kq` 默认异步模式把写失败只写成一条日志、调用方拿到 `nil`，
  异步 = 把「没送出去」记成「已发布」，下游投影从此永久落后且无人发现。
- **不带鉴权**。`kq.NewPusher` 在 v1.2.2 只暴露 balancer/chunk/flush/sync/自动建 topic 五类选项，
  没有 dialer 注入口，所以 `KafkaConf` 刻意没有 `Username`/`Password`/`CaFile` 三个键（消费侧有）。
  带 SASL 或 TLS 的集群对接不了，换 MQ 或上游补了选项时只改 `kafkaruntime_kafka.go` 与 `SenderSettings`。
- **不自动建 topic**。刻意不传 `WithAllowAutoTopicCreation()`：topic 名写错时自动建出一个空 topic，
  消费者永远读不到，比启动即失败更难查；topic 由 deploy 侧统一创建。
- **单实例单协程，没有租约列**。多副本会各自轮询同一张表，同一事件可能投两次：不破坏正确性
  （消费方按 `event_id` 去重），但白烧一倍算力与日志，见已知缺口 2。

开启方式（两步，缺一不可）：

```sh
go build -tags liveingest_kafka -o .gotmp/liveingest ./services/live-ingest
```

并把 `etc/liveingest.v1.yaml` 的 `Kafka.Enabled` 置为 `true`。接线在
`internal/svc/servicecontext.go` 的 `startPublisher()` 里（`logx.Must`）：`Enabled=true` 但二进制没链接
发送端、或 `Kafka.*` 任一键不合规，进程启动即失败而不是「安静地不发事件」。
`Enabled=false` 时 `Publisher` 字段为 `nil`，outbox 只累积，滞后由 `GetEventPublishCheckpoint` 的
`lag_seconds` 暴露。停机走 `proc.AddWrapUpListener`：先取消 worker 上下文、等在途批次收尾，再关连接。

## 配置装配

- `internal/config/config.go`：`zrpc.RpcServerConf` + `CacheRedis`（**不叫 `Redis`**，否则与 `RpcServerConf` 内嵌的 `RedisKeyConf` 撞 `conflict key redis`）+ `DataSource` + `LiveIngest`（24 项领域参数：有效期/轮转宽限/断流宽限/健康阈值/分页上限/Outbox 退避/扫描器开关，默认 `SweeperEnabled: false`）+ `Cdn`（`Enabled`、`PublishDomains`、`CallbackSecretRef` 引用位、超时、同区域偏好）+ `Kafka` + `LiveRoomRPC`（optional，本轮不装配）。
- `Kafka` 是 10 个键：`Enabled`、`Brokers`、`Group`、`PublishTopics`、`MaxRetries`、`RetryBackoffSec`、`RetryMaxBackoffSec`、`PollIntervalSec`、`BatchLimit`、`SendTimeoutSec`。三点与消费侧不同，都是刻意的：
  - 没有 `Username`/`Password`/`CaFile`：`kq.NewPusher` 没有 dialer 注入口，配上也不会生效，留下三个假键比不写更糟（见「事件发布」节）。
  - `Group` 不参与必填校验：本服务只生产，该键只留给将来的死信重投工具。
  - 没有消费侧键（`Conns`/`Consumers`/`Processors`/`Offset`）：那些只在读取端有意义，本服务不消费事件。
- 示例值不含真实凭据：`DataSource` 用本地 `root:root`，`Cdn.CallbackSecretRef` 留空并注明 Vault 写法。
- `internal/svc/servicecontext.go`：`sqlx.NewMysql` → `repository.New(...)`（9 个 model + `*redis.Redis`）→ `repository.NewIngestGateway(c.Cdn)` → `logx.Must(startPublisher())`。CDN 未启用 / 扫描器被打开时打日志提示，`Kafka.*` 逐键校验失败或发送端未链接则**启动即失败**；`proc.AddWrapUpListener` 负责停机时收尾发布循环。
- `internal/repository/ingestgateway.go`：外部入口适配器接口（`ProbeStream` / `KickStream` / `BindCallbackDomain`）+ 显式 stub，全部返回 `ErrCdnNotConfigured`，注释标 `// 契约缺口`。**不伪造「已下发/已踢流」**。

## 已知缺口（本轮未做，逻辑轮或后续轮处理）

1. **只有编译 + 单测级证据，没有真实对端联调**：22 个 logic 方法已实现（`model.ErrNotImplemented` 桩已全部移除，191 条顶层用例覆盖密钥、节点放置、状态机、停流/吊销、健康与 CDN 回调，明细见「测试覆盖」节），但 CDN 入口是 stub（见 5 条），发布器虽已接线却从未与 broker 联调（见 2 条），因此「停流真的踢掉了流」「事件真的到了 live-room」这类结论目前无法在本仓验证。
2. **发布器已接线，但没有任何 broker 侧证据**：`internal/publisher` 把 `ListPending` → 同步投递 →
   `MarkPublished`/`MarkRetry`/`MarkFailed` 这条链做完并在 `svc` 里启动，两种构建的用例都绿；
   然而本仓库从未与 Redpanda/Kafka 联调，`WithSyncPush` 的「返回 nil = broker 已受理」只在代码阅读层面成立。
   真实联调要在 `deploy/docker-compose` 的 redpanda 上跑一遍，并核对 `GetEventPublishCheckpoint` 的位点前进。
   另有两处未做：多副本没有租约列（同事件可能投两次，靠消费方去重兜正确性、白烧算力），以及判死行没有死信表
   （只能由 `RetryFailedEvents` 人工放行）。
3. **本服务没有 consumer（刻意如此）**：只产事件不消费事件，因此没有 `internal/consumer`。
   `live.state.v1` 的入站侧现在都已经在自己服务里落地：live-room 用它推进房间/场次投影
   （`services/live-room/internal/consumer`，tag `liveroom_kafka`），inbox 用它给主播发断流/停播站内信
   （tag `inbox_kafka`）。live-gateway / live-media 仍未订阅。
4. **状态扫描器未实现**：心跳超时 → `INTERRUPTED`、宽限期耗尽 → `STOPPED`、开放断流区间兜底关闭、密钥过期回收（`MarkExpired` / `MarkOfflineByHeartbeatTimeout` / `ListStaleActive` 等 model 能力已备好），`SweeperEnabled` 默认 `false`。
5. **CDN/媒体入口是 stub**：真实 RTMP/SRT/WebRTC 服务与厂商 SDK 不在本仓库，`internal/repository/ingestgateway.go` 的每个方法都带 `// 契约缺口` 与接线步骤；`GetStreamHealth` 逻辑轮需在 `ErrCdnNotConfigured` 时降级为纯 DB 视图。
6. **live-room zRPC client 未接入（刻意如此）**：`LiveRoomRPC` 配置位已在 `internal/config/config.go` 声明（`json:",optional"`，yaml 里整段注释掉），但 `internal/svc` 不构造该客户端。理由是两服务的关系是「事件生产者 → 消费者」，本服务不需要编译期依赖；live-room 的生成包 `go-video/services/live-room/rpc`（`LiveRoomClient`，21 个方法）已存在，将来若要在 `ReportStreamState` 之前做房间门禁回查，直接 import 并按 yaml 注释放开配置即可。
7. **迁移 SQL 已在隔离实例复验**：`127.0.0.1:3399`（数据目录 `.gotmp/mysql-data`）上库 `go_video_live_ingest` 已建，3 个迁移文件 ↔ 9 张业务表逐一对上（2026-09-21），`deploy/migrations/README.md` 的「当前覆盖」表也已记 `live-ingest | go_video_live_ingest | 3 | applied`。本机 3306 是维护者真实库，全程未连接、未执行 `scripts/migrate.ps1`；真实/共享实例仍未执行，上线前须由运维按 `docs/commands.md` 的命令在目标实例跑一次。列名/类型逐字对齐 `model/*Columns` 常量。
8. **`gateway/app` 未接入；`gateway/admin` 已接入 13 条**：运营面对外暴露 `LiveIngestRPC` 的
   流查询/健康/事件与中断列表、节点分配列表、密钥读取与吊销、强制停流、失败事件重试、节点注册/列表
   （13 个 logic 文件，逐个 `grep -l "LiveIngest\."` 数出；
   其中 `/admin/live/stream/close`、`/admin/live/stream/key/revoke`、`/admin/live/node/upsert`、
   `/admin/live/event/retry` 四条进 `routePermissions`，其余 9 条是免鉴权读）。密钥的**签发与轮转刻意不在 `/admin`**
   （`RotateStreamKey` 的 reply 带一次性明文，见 `docs/roadmap.md` 的运营面排除清单），
   终端侧也没有 live-ingest 路由——推流地址由主播侧走 `gateway/app` 的 creator/live 链路获取，尚未接线。
9. **事件注册表已同步**（`docs/api-and-events.md` §5 的表与其下「一个事件只有一个写入者」注记）：
   `live.state.v1` 的生产者登记为唯一 `live-ingest`，消费者含 `live-room`、`inbox` 与 `live-media`
   （第三个入站端 2026-10-04 起把它翻译成「本场次在线档位整场下线」，口径见该表下方「第三个入站」条）。
   两个候选生产者共用一个 topic 的历史问题已消除：房间态推进由 live-ingest 发事件、live-room 经
   `ReportStreamState` 入站消费，`aggregate_type` 不再需要区分双写者。
   §5 同时把「已接线」限定为编译/`go vet`/单测级证据，不在文档里冒充 broker 结论。
10. **缺陷 #1 已修复（2026-10-04）：停流事务现在一并清 `live_stream.node_id`**。
    `applyStreamTransition` 在流转入 `STOPPED` 时，于 `MarkStopped` 之后用与 `ReleaseIngestNode`
    同一条 CAS 口径 `Stream.SetNode(streamID, "", s.NodeID)` 清指针（`internal/logic/streamstate.go:238-252`），
    CAS 未命中即 `ErrConcurrentUpdate` 回滚，不留「终态流仍住在已归还配额的节点上」的两个事实。
    原 `t.Skip` 断言已去掉并转为回归防线：`internal/logic/streamstate_test.go` 的
    `TestStreamStateMachine_StoppedClearsNodePointer`；`closestream_test.go` 的两条写序断言同步多钉一次
    `Stream.SetNode`（含「无租约但流上有指针」的 IDLE 流形态，那种也要清）。
    **残留缺口（本轮未做）**：修复只管新发生的停流。真实库里历史上已停的流仍带着旧 `node_id`，
    没有回填/清理语句，也没有「按节点筛流只认在流态」的读侧收敛；
    上线这一版时需要一条一次性 `UPDATE live_stream SET node_id='' WHERE state=STOPPED AND node_id<>''`
    级别的数据修复（要配套迁移注释与影响行数留痕），属下一轮。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd/MQ）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，12 个文件 `191/30`）

按域分三组：密钥与回调、节点与流生命周期、读侧与健康。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **推流密钥与 CDN 回调** | | | |
| `streamkey_test.go` | 14 | 2 | 明文只回显一次、库里只剩摘要与 `key_ref`；重放 `request_id` 不发第二把密钥；轮转新旧在宽限期内共存；TTL 是夹取不是拒绝；无发布域名时 fail-closed |
| `keyquery_test.go` | 19 | 4 | 非 admin 的密钥列表把收口**下推到 SQL 过滤条件**且 `total` 同步收敛；分页夹取后回显；投影逐字节序列化后找不出任何摘要；非法 `stream_name` 与不存在的 `stream_name` 同判「未找到」，不给调用方一台枚举机 |
| `revoke_tmp_test.go` | 18 | 2 | 吊销的幂等锚点是「密钥终态」而不是 `request_id`（含 proto 注释与实现口径不一致，按现状钉死）；不存在/被自己吊/被他人吊三态可区分；级联停流按 `stream_id ASC` 且上限取密钥自身 `max_streams`，截断后不留信号；级联中途失败则密钥仍可用（整体回滚）。文件名里的 `_tmp` 是写作时的临时名，内容是 `RevokeStreamKey` 的完整覆盖（改名要同步本节与聚合表，留下一轮） |
| `cdncallback_test.go` | 12 | 2 | 坏签名、时钟越窗、`nonce` 重放、域名未绑定四条闸全部拒且都不授权；回调只回带 `suggest_state` 不就地写状态；拿不到签名密钥时不留证、不消费 `nonce`；库里只存摘要、日志不落 secret；签名用测试侧独立实现，避免「字段顺序改坏」被替身一起改掉 |
| **节点放置与流生命周期** | | | |
| `nodeplacement_test.go` | 27 | 3 | 打分公式（健康分 − 半载 + 同区域加分）与区域硬约束不可靠加分碰运气盖过；`AssignIngestNode` 的配额、租约、流上指针三处同事务；无候选时不伪造节点；迁移先占新节点再放旧节点；`ReleaseIngestNode` 幂等且配额回滚；心跳不覆盖配置也不静默建档 |
| `streamstate_test.go` | 14 | 1 | 状态机全矩阵与 `model` 声明的合法性逐格一致；`seq` 只在合法迁移时 +1；同态是幂等 no-op 并回放首次 `event_id`；`expect_seq` 不符与时间戳越界（过去/超 skew 的未来）都零写入；断流区间开与关；事件与 Outbox 共用同一 `event_id`；回滚后不留半截迁移。全部从 `ReportStreamState` 真入口打，不直接调私有函数 |
| `closestream_test.go` | 25 | 5 | 用替身的有序调用留痕（`fakeDB.callLog`）把「健康归位 / 密钥活跃指针 / 租约+配额 / 事件 / Outbox」的**写序**整段锁死（次数断言区分不开顺序不同）；IDLE/PUBLISHING/INTERRUPTED 可关、STOPPED 只能回放；守卫与预检在触库前返回（零 SQL 零事务）；CAS 丢失与事件/Outbox 失败整笔回滚；全程不碰 ingest gateway；`trace_id` 截断到列宽 |
| **健康与读侧** | | | |
| `healthreport_test.go` | 18 | 5 | 阈值判定 CRITICAL 优先于 DEGRADED、未配置维度不参与；「连续越界才断流」且连续受采样窗口约束；`report_id` 幂等回放先于任何流表读取；采样落库 + 投影回写 + 断流迁移同事务，任一步失败整笔回滚；迟到的合法样本会回退上报时间；日志不含指标明文、响应不含密钥材料 |
| `healthquery_test.go` | 22 | 0 | `GetStreamHealth` 的窗口是**夹取语义**而非校验语义（客户端只能收窄）；`NO_DATA` 是视图不是事实，读路径不得把最后一次判定抹进库；探测失败降级并区分原因；`ListStreamEvents` 的 `max_seq`/`has_more` 必须诚实（谎报会让对账空转或永久漏事件）；读路径零副作用 |
| `streamquery_test.go` | 22 | 6 | `ListStreams` 越权收口下推、集合与 `total` 同时收敛；分页「夹取 + 回显」而不是拒绝；未指定状态时排除终态行；`GetStreamState` 按房间只取最新非终态、按 `stream_id` 选择器永不回落到房间、选择器不命中返回 `found=false` 而非报错；排序用例一律铺互不相同的心跳值，不拿替身的兜底次序立契约 |
| **替身与脚手架（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 内存假库 + 9 个 model 接口实现 + 只实现 `TransactCtx` 的假连接，见第 4 组 |
| `testsupport_test.go` | 0 | 0 | 共用装配（真 `repository.New` + 逐个替换 model 接口字段）、断言助手、副作用探针与种子数据 |

### 2. 其他层

- `model`（2 文件 `25/0`）：
  - `errors_test.go` `13/0`：流状态编号 1/2/3/4 与 live-room 入参、`live.state.v1` payload 锁死；
    迁移矩阵、终态/活跃集合、协议位图与枚举互转、Outbox 编号 ↔ proto、`ClampLimit`、
    rune 安全截断、`IsDuplicate`、`nullableString`、哨兵错误互不相同。
  - `migration_parity_test.go` `12/0`：把「model 的 SQL 与 `deploy/migrations/live-ingest` 逐列一致」
    变成可执行门禁——列全集三处文本对齐、INSERT 列清单与省略列有解释、主键/唯一键覆盖 model 依赖、
    索引存在性、可空性与默认值纪律、每列有注释、回滚覆盖每张表、迁移可重复执行且禁用项、
    文件名与顺序、密钥列只存摘要。
- `internal/publisher`（5 个文件，默认构建 `28/5`，`-tags liveingest_kafka` 时 `30/6`）：
  口径同上（`grep -cE '^func Test'` / `grep -c 't.Run('`），但表驱动实际跑出的子用例更多，
  按 `-v` 输出实测为默认 `28/34`、带标签 `30/38`。逐文件：
  - `publisher_test.go` `17/4`：一轮内严格按 `id` 升序串行投递且不重排（同流顺序的前提）；
    `Send` 收到的 ctx 必须带 `SendTimeout` 的 deadline；退避指数 `Base * 2^(n-1)` 且被 `MaxBackoff` 夹住，
    `nextCount=1000` 这类大指数不允许溢出成负数、`retry_count` 异常为 0 时也不允许算出早于当前的时间；
    尝试耗尽与「差一次」的判死边界不 off-by-one；带 `Defect` 的行零次发送直接判死；
    读库失败与状态写库失败都中断本批并回传错误，而投递失败不回传；空扫一轮零触库；
    `New`/`Options.validate`/`ValidatePublishKafka` 的每个失败分支都在错误里点名配置键；
    `OptionsFrom` 逐字段映射（漏字段=改了 yaml 没生效）；`SenderSettingsFrom` 去重+trim+返回副本；
    `Start` 重复启动报错、`Stop` 幂等且等循环收尾。
  - `outbox_store_test.go` `7/1`：`RequiredTopic()` 必须等于字面量 `live.state.v1`（文档 §5 的跨服务契约，
    改名等于换掉所有消费方的订阅）且必须由 model 常量拼出；
    列 → `Record` 映射与四类一致性缺陷识别（topic 不属于本服务、`aggregate_id` 空、payload 非法信封、
    信封与列不同源）；`ListPending` 保持 SQL 顺序并包装错误；三个状态写转调 model 且参数原样；
    最后一条用真 `OutboxStore` + 假 model 跑完整 `RunOnce`。
  - `example_yaml_test.go` `2/0`：示例配置除 `Enabled` 外每个键都必须填好（「打开开关就能投」必须是假的），
    并且用真实 `conf.Load` 读 yaml 而不是手写字面量；`NewPublisher` 能用示例配置装配出可投递的发布器。
  - `kafkaruntime_disabled_test.go` `2/0`（`//go:build !liveingest_kafka`）：默认构建即使参数完全合规也拿不到
    发送端，错误文本必须含 `liveingest_kafka`/`broker`/`Kafka.Enabled`（给运维可执行的下一步）；
    启动日志要点名积压位置 `live_ingest_outbox` 与 topic。
  - `kafkaruntime_kafka_test.go` `4/1`（`//go:build liveingest_kafka`）：带标签构建能建通道且**不碰网络**；
    参数不完整时先拒绝；未登记 topic / 空分区键在拨号前返回错误；`RuntimeNotes` 仍说清「没联调」。
    这里刻意不调 `Start`，也不依赖「`kq.NewPusher` 不拨号」这一实现细节。
- `internal/config`（1 文件 `1/1`）：`config_load_test.go` 用真实 `conf.Load` 加载 `etc/` 下每个 yaml
  （`conflict key redis` 这类只在真实加载时才暴露）。
- `internal/repository`、`internal/svc`：**无离线单测**（logic 用例通过替身替换 `repository` 的导出
  model 接口字段来间接覆盖装配路径，适配器自身的 SQL 与外部网关实现没有用例；`svc` 的发布器启动只有
  `example_yaml_test.go` 从配置侧间接打到 `NewPublisher`）。
- 本服务没有 `internal/consumer`（只产事件不消费事件）、也没有 `internal/policy` 目录，见已知缺口 3。

### 3. 构造器级覆盖

`22/22`：探针取 `internal/logic` 全部 `New*Logic(`（22 个，与 RPC 方法一一对应），
逐个在 `*_test.go` 里查引用，`gaps:` 为空，即每个方法的 logic 都从构造器进入被打过。

### 4. 替身层与断言口径

替身在 `internal/logic/fakes_test.go`，装配口径在 `testsupport_test.go`：

- 注入方式不改实现：`repository.Repository` 的 9 个 model 字段是导出接口字段，可整体替换；
  `repository.New` 接受任意 `sqlx.SqlConn`，而本服务事务入口只有 `TransactCtx`；
  logic 从不直接用 Redis（缓存键空间只是文档承诺），因此 Redis 留 `nil`。
- 复刻的语义：`TransactCtx` 成功即提交、失败整库还原（等价 MySQL 回滚，所以「拒绝的请求零副作用」
  是可证伪的断言而不是口号）；唯一索引冲突回带 MySQL 原文案（`Error 1062 / Duplicate entry`），
  否则 `model.IsDuplicate` 那条幂等分支根本进不到；`report_id` / `publish_request` 空值不参与唯一冲突
  （真库靠 `nullableString` 落 NULL）；状态迁移一律「条件 UPDATE + `RowsAffected` 判定」的形状照抄，
  否则「CAS 输了」这类分支测不到；`closestream_test.go` 另外用有序调用留痕复刻写序。
- **本轮修掉一处替身缺陷**（`fakes_test.go` 的 `fakeOutbox.ListByState`）：截断条件曾写成
  `if limit > 0 && limit >= len(out) { break }`，第一行必停，与真 SQL
  （`model/outbox.go:255` 的 `ORDER BY id ASC LIMIT clampLimit(limit,500)`）不符——
  任何「取满 limit」的读侧断言都会被它吃掉。现在走 `clampLimitLikeModel(limit, 500)`，
  与本文件其它分页替身同一口径。
- 未实现的方法内嵌真实 model 接口（值为 `nil`）：被测代码一旦调用到没建模的方法就 panic 在方法名上，
  等价「logic 偷偷写了替身没覆盖的表」，不静默放过。
- 它证明不了什么：真实 SQL 文本、列宽、索引是否命中、`ORDER BY` 在并列值上的实际次序、
  驱动返回的 matched rows 与 changed rows 差别、`SELECT ... FOR UPDATE` 的真实行锁语义，
  以及 `repository`/`ingestgateway` 适配层本身的映射。排序类用例因此只铺互不相同的值。

### 5. 覆盖边界

- **2026-10-03 整树 `go test -p 1` 首次把本包跑红，四条用例是断言侧写错的**（不是实现回归，
  也不涉及放宽任何校验；修完后本包 191/30 全绿）。逐条留档，防止下一次有人把形状改回去：
  - `revoke_tmp_test.go` 的 `TestRevokeStreamKey_PrestartStates`：`e.keyRow()` 走的是一次真的
    `StreamKey.FindOne`（`testsupport_test.go:477`），却被排在 `requireCallsFrom` **之后**调用，
    于是断言序列里凭空多出一次读。改为「先比触库形状、再读回行」。
  - 同文件的 `TestRevokeStreamKey_LeavesNoAttributionOrTimestamp`：期望写成「多一行密钥」，
    实际吊销只把现有密钥行 UPDATE 成 REVOKED（九张表零新增），而**零新增正是该用例要钉的缺陷证据**；
    同包 `TestRevokeStreamKey_NoCascadeLeavesStreamRunningIsSilent` 早就用 `[9]int{}` 表达同一口径。
  - 同文件的级联主用例：`seedRevokeFixture` 每次都 `seedNode("node-rev")`，而 `seedNode` 是按
    `node_id` **整行覆盖**，第二把密钥的夹具把前面 `ReserveQuota` 的计数抹成种子值，
    于是级联退配额在第二条流上拿到 `ErrNodeNotFound: node quota already drained` 并整体回滚。
    拆出 `seedRevokeKey`（只铺密钥）给第二个夹具用。另外 `requireCallOrder` 的口径是
    「列几次要求几次」，原先只列一遍却跑三条流，必然报「次数 3 期望 1」；现按 A/B/C 三段列全，
    比原来更强（顺序 + 每条流的收尾步骤数一起钉）。
  - `healthreport_test.go` 的 `seedHealthSamples`：`report_id` 只带循环下标，按「一次一条、状态各异」
    逐次调用时永远撞 `uniq_report_id`；key 里补上 `ago`。
  - `closestream_test.go` 的断流区间原因：期望写成「运营切断」，真 SQL 是
    `reason = CONCAT(reason, IF(reason='','', ' | '), ?)`（`model/streaminterruption.go:154`），
    开区间的「心跳丢失」被保留、关因追加在后面；现按 `心跳丢失 | 运营切断` 断言，
    这条恰好是「覆盖写会丢掉前半句」这类缺陷的唯一防线。
- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端。
- **本服务已无 skip 用例**（2026-10-04 缺陷 #1 修复后）：原先 `streamstate_test.go:389` 的
  `TestStreamStateMachine_StoppedClearsNodePointer` 是唯一一条 `t.Skip` 断言，现在随实现补齐而生效。
  不要把用例文件数当成断言都在跑：全仓仍有 4 条 skip（live-gateway 3 条缺陷哨兵、
  notification `policy/backoff_test.go:93` 的「无 tzdata」条件跳过），登记在 `docs/roadmap.md` 的实测表。
- 迁移 SQL 与真实库的列级对账只在隔离实例 `127.0.0.1:3399`（库 `go_video_live_ingest`，3 个文件 ↔
  9 张表，2026-09-21）复验过，见已知缺口 7；真实/共享实例从未执行。
  `model/migration_parity_test.go` 自身是静态文本比对，不接触任何实例。
- CDN/媒体入口（`internal/repository/ingestgateway.go`）是显式 stub，全部返回 `ErrCdnNotConfigured`；
  状态扫描器未接线，因此「超时流真的被停」不在离线覆盖内。
- **`internal/publisher` 全绿不等于事件真的到了 broker**。发布器实际跑出的子用例是默认 34 条、
  带标签 38 条，全部落在假 `Store` + 假 `Sender` 上，其中带标签的 4 条也只验到「通道建立/参数拒绝」
  这一层，全程没有拨号。
  本仓库从未与 Redpanda 联调，「事件已送达」「同步推送的失败语义」都未经实例确认（见已知缺口 2）。
- `internal/server`、`internal/handler`（本服务无 HTTP）、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内。

### 6. 验证命令

```sh
go test -p 1 -count=1 ./services/live-ingest/...
go test -p 1 -count=1 -tags liveingest_kafka ./services/live-ingest/internal/publisher/
gofmt -l services/live-ingest    # 必须为空
go vet ./services/live-ingest/...
go vet -tags liveingest_kafka ./services/live-ingest/internal/publisher/
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455），
不是可选的性能调优。带 `-tags liveingest_kafka` 的两条是硬要求：默认构建根本不编译
`kafkaruntime_kafka.go`，只跑默认构建等于没验过生产者。
`-race` 在本机不可用（CGO_ENABLED=0 且没有 gcc），并发语义靠 `Start`/`Stop` 的生命周期用例兜。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

## 自检

```sh
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
gofmt -l services/live-ingest              # 无输出
go build ./services/live-ingest/...
go build -tags liveingest_kafka ./services/live-ingest/...
go vet ./services/live-ingest/...
go vet -tags liveingest_kafka ./services/live-ingest/internal/publisher/
go test -p 1 -count=1 ./services/live-ingest/...
go test -p 1 -count=1 -tags liveingest_kafka ./services/live-ingest/internal/publisher/
```
