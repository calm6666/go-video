# RPC · `live-ingest`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/live-ingest/rpc/liveingest.proto` |
| protobuf 包 | `liveingest.v1` |
| go_package | `go-video/services/live-ingest/rpc` |
| 发现用的 etcd key | `liveingest.v1.rpc`（`services/live-ingest/etc/liveingest.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`liveingest.v1.rpc`） |
| 监听 | `8118`（`services/live-ingest/etc/liveingest.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_live_ingest` |
| 方法数 | 22（service `LiveIngest`） |
| 网关消费方 | `admin:LiveIngestRPC` |

## 契约说明

> 说明：live-ingest 是「直播推流接入与流状态」领域服务，是推流密钥（含哈希与
> 受控引用）、接入节点、流状态机、断流/重连记录和流状态事件的唯一所有者
> （AGENTS.md §5）。
>
> 与 live-room / live-media / live-gateway 的边界（本期强约束）：
>   - 房间业务状态（待完善/可开播/直播中/关闭/禁播）与场次生命周期归 live-room；
>     本服务只保存 room_id / session_id / anchor_mid 引用，不校验、不回查、
>     更不写 live-room 的表。
>   - 开播门禁由 live-room 负责：本服务签发密钥时只做「调用方已具备开播资格」
>     的入参校验（mid/room_id 必填 + 幂等），不复制 room 状态。
>   - 转码/录制/回放归 live-media，长连接广播归 live-gateway：两者都只消费本
>     服务产出的 live.state.v1 事件或按 stream_id 反查本服务 RPC。
>   - 跨服务只传业务主键（room_id / session_id / stream_id / node_id / mid），
>     本文件不 import 任何其他服务的 proto。
>
> 密钥安全约束（README「不保存长期明文推流密钥」）：
>   - 明文密钥只在 IssueStreamKey / RotateStreamKey 的响应里出现一次，
>     落库只有 SHA-256 哈希、Secret/Vault 引用与末 4 位辨认串；
>   - 任何 Get/List 接口都不得回显明文；日志与错误消息同样禁止。
>   - CDN/入口回调的签名密钥也不入库，配置里只放 Secret/Vault 引用位。
>
> 事件与幂等（README「流状态事件必须幂等并可追踪」）：
>   - 每次合法状态迁移在同一个事务里写 live_stream_event（分配该流单调递增的
>     seq）与 live_ingest_outbox（live.state.v1 信封 JSON），由独立发布器投递；
>   - 所有写接口都带幂等键（request_id / report_id / nonce），重放返回首次结果
>     且 reply.replayed=true，不产生新事件、不重复推进 seq；
>   - 消费方（live-room.ReportStreamState、live-gateway、live-media）按
>     event_id 去重、按 seq 拒绝乱序回退。

## service `LiveIngest`

> LiveIngest 推流接入与流状态服务。 / 数据所有权见 AGENTS.md §5：推流密钥（哈希与受控引用）、接入节点、流状态机、 / 断流/重连记录与流状态事件归本服务；房间业务状态归 live-room， / 外部只能通过带 event_id / report_id / request_id 幂等的入口推进合法状态。

gRPC 方法前缀：`liveingest.v1.LiveIngest/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `IssueStreamKey` | [`IssueStreamKeyReq`](#message-issuestreamkeyreq) | [`IssueStreamKeyReply`](#message-issuestreamkeyreply) | 签发推流密钥：入库只有哈希与 Secret/Vault 引用，明文只在本响应出现一次 |
| 2 | `VerifyPublishAuth` | [`VerifyPublishAuthReq`](#message-verifypublishauthreq) | [`VerifyPublishAuthReply`](#message-verifypublishauthreply) | 接入鉴权（RTMP/SRT/WebRTC 入口在建连时调用）：比对哈希、协议、有效期、配额，可选建档 IDLE 流 |
| 3 | `RotateStreamKey` | [`RotateStreamKeyReq`](#message-rotatestreamkeyreq) | [`RotateStreamKeyReply`](#message-rotatestreamkeyreply) | 轮转密钥：新密钥生效、旧密钥进入宽限期，重连不中断 |
| 4 | `RevokeStreamKey` | [`RevokeStreamKeyReq`](#message-revokestreamkeyreq) | [`RevokeStreamKeyReply`](#message-revokestreamkeyreply) | 吊销密钥：立即失效，可按需级联停止进行中的流 |
| 5 | `GetStreamKey` | [`GetStreamKeyReq`](#message-getstreamkeyreq) | [`GetStreamKeyReply`](#message-getstreamkeyreply) | 查询密钥元数据（永不回显明文或哈希） |
| 6 | `ListStreamKeys` | [`ListStreamKeysReq`](#message-liststreamkeysreq) | [`ListStreamKeysReply`](#message-liststreamkeysreply) | 分页查询密钥列表 |
| 7 | `UpsertIngestNode` | [`UpsertIngestNodeReq`](#message-upsertingestnodereq) | [`UpsertIngestNodeReply`](#message-upsertingestnodereply) | 节点注册/心跳上报（幂等 upsert，运维面） |
| 8 | `ListIngestNodes` | [`ListIngestNodesReq`](#message-listingestnodesreq) | [`ListIngestNodesReply`](#message-listingestnodesreply) | 分页查询接入节点 |
| 9 | `AssignIngestNode` | [`AssignIngestNodeReq`](#message-assigningestnodereq) | [`AssignIngestNodeReply`](#message-assigningestnodereply) | 为流分配接入节点（就近 + 配额 + 健康分打分；支持迁移） |
| 10 | `ReleaseIngestNode` | [`ReleaseIngestNodeReq`](#message-releaseingestnodereq) | [`ReleaseIngestNodeReply`](#message-releaseingestnodereply) | 释放流的节点占用（停流或运维摘流） |
| 11 | `ListNodeAssignments` | [`ListNodeAssignmentsReq`](#message-listnodeassignmentsreq) | [`ListNodeAssignmentsReply`](#message-listnodeassignmentsreply) | 分页查询节点分配记录（容量对账与排障） |
| 12 | `ReportStreamState` | [`ReportStreamStateReq`](#message-reportstreamstatereq) | [`ReportStreamStateReply`](#message-reportstreamstatereply) | 上报流状态：按状态机 CAS 推进 + 分配 seq + 同事务写事件与 outbox（report_id 幂等） |
| 13 | `GetStreamState` | [`GetStreamStateReq`](#message-getstreamstatereq) | [`GetStreamStateReply`](#message-getstreamstatereply) | 查询单流当前状态（按 stream_id 或房间的活跃流） |
| 14 | `ListStreams` | [`ListStreamsReq`](#message-liststreamsreq) | [`ListStreamsReply`](#message-liststreamsreply) | 分页查询流列表（开播巡检、断流扫描） |
| 15 | `CloseStream` | [`CloseStreamReq`](#message-closestreamreq) | [`CloseStreamReply`](#message-closestreamreply) | 强制停流（主播下播、运营/风控切断；IDLE/PUBLISHING/INTERRUPTED → STOPPED） |
| 16 | `ReportStreamHealth` | [`ReportStreamHealthReq`](#message-reportstreamhealthreq) | [`ReportStreamHealthReply`](#message-reportstreamhealthreply) | 健康采样上报：更新最新健康字段并留采样点，越过危险阈值时触发 INTERRUPTED 迁移 |
| 17 | `GetStreamHealth` | [`GetStreamHealthReq`](#message-getstreamhealthreq) | [`GetStreamHealthReply`](#message-getstreamhealthreply) | 流健康检查：当前判定 + 窗口聚合 + 最近采样点 |
| 18 | `ListStreamInterruptions` | [`ListStreamInterruptionsReq`](#message-liststreaminterruptionsreq) | [`ListStreamInterruptionsReply`](#message-liststreaminterruptionsreply) | 断流与重连记录查询 |
| 19 | `ListStreamEvents` | [`ListStreamEventsReq`](#message-liststreameventsreq) | [`ListStreamEventsReply`](#message-liststreameventsreply) | 事件位点：按 seq 游标拉取状态事件（live-room/live-media 补偿与对账） |
| 20 | `GetEventPublishCheckpoint` | [`GetEventPublishCheckpointReq`](#message-geteventpublishcheckpointreq) | [`GetEventPublishCheckpointReply`](#message-geteventpublishcheckpointreply) | Outbox 发布位点与滞后度（观测「事件必须可追踪」） |
| 21 | `RetryFailedEvents` | [`RetryFailedEventsReq`](#message-retryfailedeventsreq) | [`RetryFailedEventsReply`](#message-retryfailedeventsreply) | 把超过重试上限的失败事件重置为待发布（运营补偿，request_id 幂等） |
| 22 | `VerifyCdnCallback` | [`VerifyCdnCallbackReq`](#message-verifycdncallbackreq) | [`VerifyCdnCallbackReply`](#message-verifycdncallbackreply) | CDN/入口回调鉴权：签名 + 时间窗 + nonce 防重放，只建议状态、由调用方走 ReportStreamState |

## 消息与枚举

### message `EmptyReply`

> 通用空响应（幂等重放场景由业务 reply 自带 replayed 标记）

（空消息）

### enum `IngestProtocol`

> 推流协议（与 live_stream_key.protocol_mask 的位、live_stream.protocol 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `PROTOCOL_UNSPECIFIED` | 0 | 未指定，视为非法入参 |
| `PROTOCOL_RTMP` | 1 | RTMP / RTMPS（OBS 等主流推流助手） |
| `PROTOCOL_SRT` | 2 | SRT（低延迟、抗弱网） |
| `PROTOCOL_WEBRTC` | 3 | WebRTC（超低延迟连麦） |

### enum `StreamKeyState`

> 推流密钥状态（与 live_stream_key.state 列一致，取值不可变更）

| 值 | 编号 | 说明 |
|---|---|---|
| `STREAM_KEY_STATE_UNSPECIFIED` | 0 | 未指定 |
| `STREAM_KEY_STATE_ACTIVE` | 1 | 生效：可用于接入鉴权 |
| `STREAM_KEY_STATE_ROTATING` | 2 | 轮转中：旧密钥在 grace_until 前仍可用于重连 |
| `STREAM_KEY_STATE_RETIRED` | 3 | 已退役：轮转宽限期结束后的旧密钥终态 |
| `STREAM_KEY_STATE_EXPIRED` | 4 | 已过期：超过 expire_at，需重新签发 |
| `STREAM_KEY_STATE_REVOKED` | 5 | 已吊销：立即失效（禁播/泄露/风控），不可恢复 |

### enum `StreamState`

> 流状态机（与 live_stream.state 列一致，且与 live-room ReportStreamStateReq / 的 stream_state 取值严格一致：1 IDLE、2 PUBLISHING、3 INTERRUPTED、4 STOPPED）

| 值 | 编号 | 说明 |
|---|---|---|
| `STREAM_STATE_UNSPECIFIED` | 0 | 未指定 |
| `STREAM_STATE_IDLE` | 1 | 已建档：接入鉴权通过，等待推流真正到达 |
| `STREAM_STATE_PUBLISHING` | 2 | 推流中：媒体帧持续到达且健康度在阈值内 |
| `STREAM_STATE_INTERRUPTED` | 3 | 断流：心跳/帧中断，宽限期内允许重连 |
| `STREAM_STATE_STOPPED` | 4 | 已停止：本次推流会话终态（重推是新 stream_id） |

### enum `HealthState`

> 流健康判定（与 live_stream.health_state 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `HEALTH_STATE_UNSPECIFIED` | 0 | 未指定 |
| `HEALTH_STATE_HEALTHY` | 1 | 正常：码率/帧率/丢包均在阈值内 |
| `HEALTH_STATE_DEGRADED` | 2 | 劣化：越过告警阈值但未断流（灰度降级、可提示主播） |
| `HEALTH_STATE_CRITICAL` | 3 | 危险：接近断流（码率骤降、丢包高），logic 应触发 INTERRUPTED |
| `HEALTH_STATE_NO_DATA` | 4 | 无采样：超过 no_data 宽限期未收到健康上报 |

### enum `InterruptionEndReason`

> 断流结束原因（与 live_stream_interruption.end_reason 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `INTERRUPTION_END_REASON_UNSPECIFIED` | 0 | 未指定 |
| `INTERRUPTION_END_RECONNECTED` | 1 | 重连成功：INTERRUPTED → PUBLISHING |
| `INTERRUPTION_END_TIMEOUT` | 2 | 断流超时：INTERRUPTED → STOPPED（宽限期耗尽） |
| `INTERRUPTION_END_CLOSED` | 3 | 主动停流：INTERRUPTED → STOPPED（主播/运营关闭） |

### enum `StopReason`

> 停流原因（与 live_stream.stop_reason 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `STOP_REASON_UNSPECIFIED` | 0 | 未指定 |
| `STOP_REASON_ANCHOR_STOP` | 1 | 主播正常下播（入口收到 RTMP publish_done/unpublish） |
| `STOP_REASON_NODE_TIMEOUT` | 2 | 接入节点心跳丢失且断流宽限期耗尽 |
| `STOP_REASON_UNHEALTHY` | 3 | 健康度持续危险，服务端主动切断 |
| `STOP_REASON_REVOKED` | 4 | 密钥被吊销（禁播/泄露），级联停流 |
| `STOP_REASON_ADMIN` | 5 | 运营强制停流 |
| `STOP_REASON_KEY_EXPIRED` | 6 | 密钥过期前未推流，IDLE 流回收 |

### enum `IngestNodeState`

> 接入节点状态（与 live_ingest_node.state 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `INGEST_NODE_STATE_UNSPECIFIED` | 0 | 未指定 |
| `INGEST_NODE_STATE_ONLINE` | 1 | 在线可分配 |
| `INGEST_NODE_STATE_DRAINING` | 2 | 摘流中：不再分配新流，存量流跑完即下线 |
| `INGEST_NODE_STATE_OFFLINE` | 3 | 离线：心跳丢失或人工下线 |

### enum `AssignmentState`

> 节点分配记录状态（与 live_node_assignment.state 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `ASSIGNMENT_STATE_UNSPECIFIED` | 0 | 未指定 |
| `ASSIGNMENT_STATE_ACTIVE` | 1 | 生效中：该流正占用节点配额 |
| `ASSIGNMENT_STATE_RELEASED` | 2 | 已释放：停流或主动释放 |
| `ASSIGNMENT_STATE_MIGRATED` | 3 | 已迁移：被更优节点替换（prev_node_id 记录来源） |

### enum `OutboxState`

> 事件发布状态（与 live_ingest_outbox.state 列一致）

| 值 | 编号 | 说明 |
|---|---|---|
| `OUTBOX_STATE_UNSPECIFIED` | 0 | 未指定 |
| `OUTBOX_STATE_PENDING` | 1 | 待发布（含退避重试中） |
| `OUTBOX_STATE_PUBLISHED` | 2 | 已发布 |
| `OUTBOX_STATE_FAILED` | 3 | 超过最大重试，等待运营 RetryFailedEvents |

### message `StreamKeyInfo`

> --- 推流密钥主体 --- / 推流密钥元数据（DB 行投影，永不含明文密钥）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `key_id` | `int64` | 1 | — | 密钥 ID（自增主键） |
| `stream_name` | `string` | 2 | — | 流标识（推流 URL 的 name 段，可下发客户端，非密钥） |
| `key_hint_tail` | `string` | 3 | — | 明文末 4 位，仅供主播在多个密钥间辨认，不可用于鉴权 |
| `key_ref` | `string` | 4 | — | Secret/Vault 引用（例如 vault:secret/live-ingest/stream-key/<id>） |
| `state` | [`StreamKeyState`](#enum-streamkeystate) | 5 | — | 密钥状态 |
| `version` | `int32` | 6 | — | 轮转代次，从 1 递增 |
| `prev_key_id` | `int64` | 7 | — | 由哪个密钥轮转而来，0 表示首发 |
| `protocols` | [`IngestProtocol`](#enum-ingestprotocol) | 8 | repeated | 允许的接入协议 |
| `room_id` | `int64` | 9 | — | 绑定房间引用（live-room 主键，本服务不校验） |
| `session_id` | `int64` | 10 | — | 绑定场次引用，0 表示未绑定具体场次 |
| `anchor_mid` | `int64` | 11 | — | 主播用户 ID |
| `expire_at` | `int64` | 12 | — | 过期时间（Unix 秒） |
| `grace_until` | `int64` | 13 | — | 轮转宽限截止（Unix 秒），0 表示不适用 |
| `current_stream_id` | `string` | 14 | — | 当前非终态流 ID，空串表示无活跃流 |
| `rotate_to_key_id` | `int64` | 15 | — | 轮转后继密钥 ID，0 表示无 |
| `reason` | `string` | 16 | — | 最近一次状态变更原因（吊销/过期说明） |
| `ctime` | `int64` | 17 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 18 | — | 修改时间（Unix 秒） |

### message `IssueStreamKeyReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 绑定房间 ID，必须 > 0（引用值，由 live-room 侧保证存在） |
| `anchor_mid` | `int64` | 2 | — | 主播用户 ID，必须 > 0（gateway 注入，本服务不解析 token） |
| `session_id` | `int64` | 3 | — | 场次引用，0 表示不绑定具体场次 |
| `protocols` | [`IngestProtocol`](#enum-ingestprotocol) | 4 | repeated | 允许协议，缺省为 [PROTOCOL_RTMP] |
| `ttl_seconds` | `int64` | 5 | — | 密钥有效期（秒），<=0 时取配置默认 IssueTtlSeconds |
| `max_streams` | `int32` | 6 | — | 该密钥并发流上限，<=0 时取配置默认 MaxStreamsPerKey |
| `request_id` | `string` | 7 | — | 幂等键，重试必须复用同一值 |
| `trace_id` | `string` | 8 | — | 链路追踪 ID |

### message `IssueStreamKeyReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `key_id` | `int64` | 1 | — | 新密钥 ID |
| `stream_name` | `string` | 2 | — | 流标识 |
| `plaintext_key` | `string` | 3 | — | 明文密钥：仅此一次返回，调用方必须即时展示并丢弃本地副本 |
| `publish_url` | `string` | 4 | — | （密钥不落库，重放时无法找回，见 replayed 说明） / 拼好的推流地址（含明文密钥，仅本次响应有效） |
| `protocols` | [`IngestProtocol`](#enum-ingestprotocol) | 5 | repeated | 生效协议 |
| `expire_at` | `int64` | 6 | — | 过期时间（Unix 秒） |
| `replayed` | `bool` | 7 | — | true 表示命中 request_id 的幂等重放：只回元数据， |

### message `RotateStreamKeyReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `key_id` | `int64` | 1 | — | 被轮转的当前密钥 ID，必须 > 0 |
| `grace_seconds` | `int64` | 2 | — | 旧密钥宽限秒数，<=0 时取配置默认 RotateGraceSeconds |
| `ttl_seconds` | `int64` | 3 | — | 新密钥有效期（秒），<=0 时取配置默认 IssueTtlSeconds |
| `request_id` | `string` | 4 | — | 幂等键 |
| `operator_mid` | `int64` | 5 | — | 操作者（主播本人或运营），必须 > 0 |
| `force` | `bool` | 6 | — | true 时不停止进行中的流（旧密钥在宽限期内继续可用） |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `RotateStreamKeyReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `key_id` | `int64` | 1 | — | 新密钥 ID |
| `prev_key_id` | `int64` | 2 | — | 被替换的旧密钥 ID |
| `plaintext_key` | `string` | 3 | — | 新明文密钥，仅此一次返回（replayed=true 时为空串） |
| `publish_url` | `string` | 4 | — | 新推流地址（含明文密钥，仅本次响应有效，replayed=true 时为空串） |
| `grace_until` | `int64` | 5 | — | 旧密钥宽限截止（Unix 秒） |
| `version` | `int32` | 6 | — | 新密钥轮转代次 |
| `replayed` | `bool` | 7 | — | true 表示命中 request_id，未产生新密钥 |
| `message` | `string` | 8 | — | 说明（宽限期内的重连语义等） |

### message `RevokeStreamKeyReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `key_id` | `int64` | 1 | — | 被吊销的密钥 ID，必须 > 0 |
| `operator_mid` | `int64` | 2 | — | 操作者（主播本人或运营），必须 > 0 |
| `admin` | `bool` | 3 | — | true 表示运营侧吊销（可吊销他人密钥） |
| `stop_stream` | `bool` | 4 | — | true 表示级联强制停止该密钥的进行中流 |
| `reason` | `string` | 5 | — | 吊销原因（审计留存，禁播/泄露/风控等） |
| `request_id` | `string` | 6 | — | 幂等键 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `RevokeStreamKeyReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`StreamKeyState`](#enum-streamkeystate) | 1 | — | 吊销后的密钥状态（REVOKED） |
| `stopped_stream_ids` | `string` | 2 | repeated | 被级联停止的流 ID，未级联时为空 |
| `replayed` | `bool` | 3 | — | true 表示命中 request_id |
| `message` | `string` | 4 | — | 说明（已是 REVOKED 时按幂等成功返回） |

### message `GetStreamKeyReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `key_id` | `int64` | 1 | — | 按密钥 ID 查询（与 stream_name 二选一，0 表示不按 ID） |
| `stream_name` | `string` | 2 | — | 按流标识查询当前生效密钥（接入排障用） |

### message `GetStreamKeyReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `key` | [`StreamKeyInfo`](#message-streamkeyinfo) | 1 | — | 密钥元数据（无明文、无哈希） |

### message `ListStreamKeysReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_id` | `int64` | 1 | — | 房间过滤，0 不限制 |
| `anchor_mid` | `int64` | 2 | — | 主播过滤，0 不限制 |
| `state` | [`StreamKeyState`](#enum-streamkeystate) | 3 | — | 状态过滤，UNSPECIFIED 不限制 |
| `pn` | `int32` | 4 | — | 页码，从 1 开始 |
| `ps` | `int32` | 5 | — | 每页条数，服务端夹取到 MaxListPageSize |
| `operator_mid` | `int64` | 6 | — | 调用者；非运营只能查自己的密钥 |
| `admin` | `bool` | 7 | — | true 表示运营侧查询 |

### message `ListStreamKeysReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `keys` | [`StreamKeyInfo`](#message-streamkeyinfo) | 1 | repeated | 密钥列表，按 key_id 倒序 |
| `total` | `int32` | 2 | — | 符合条件的总行数 |
| `pn` | `int32` | 3 | — | 回显页码 |
| `ps` | `int32` | 4 | — | 回显每页条数 |

### message `VerifyPublishAuthReq`

> --- 接入鉴权（RTMP/SRT/WebRTC 入口调用） ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_name` | `string` | 1 | — | 推流 URL 中的流标识 |
| `plaintext_key` | `string` | 2 | — | 客户端上报的明文密钥；服务端只与哈希比对，绝不落库或写日志 |
| `protocol` | [`IngestProtocol`](#enum-ingestprotocol) | 3 | — | 本次接入协议 |
| `room_id` | `int64` | 4 | — | 入口解析出的房间引用，0 表示由本服务按密钥回填 |
| `client_ip` | `string` | 5 | — | 推流端来源 IP（只做当次校验与哈希留证，不入库明文） |
| `client_version` | `string` | 6 | — | 推流客户端标识（OBS 版本等，观测用） |
| `create_stream` | `bool` | 7 | — | true（入口默认）鉴权通过时建档 IDLE 流并返回 stream_id |
| `request_id` | `string` | 8 | — | 幂等键；同一 request_id 重放返回同一 stream_id |
| `trace_id` | `string` | 9 | — | 链路追踪 ID |

### message `VerifyPublishAuthReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `allowed` | `bool` | 1 | — | 是否放行 |
| `reason` | `string` | 2 | — | 拒绝原因码（key_not_found/key_revoked/key_expired/protocol_denied/quota_exceeded/room_mismatch） |
| `key_id` | `int64` | 3 | — | 命中的密钥 ID，拒绝时可能为 0 |
| `room_id` | `int64` | 4 | — | 密钥绑定的房间引用 |
| `session_id` | `int64` | 5 | — | 场次引用，0 表示未绑定 |
| `anchor_mid` | `int64` | 6 | — | 主播 ID |
| `stream_id` | `string` | 7 | — | 本次推流会话 ID；未建档时为空 |
| `stream_state` | [`StreamState`](#enum-streamstate) | 8 | — | 建档后的流状态（IDLE） |
| `key_expire_at` | `int64` | 9 | — | 密钥过期时间（Unix 秒），入口据此设定连接保活上限 |
| `server_time` | `int64` | 10 | — | 服务端当前时间（Unix 秒），入口用于校时 |
| `replayed` | `bool` | 11 | — | true 表示命中 request_id，返回首次结果 |

### message `StreamInfo`

> --- 流状态上报与查询 --- / 流状态与累计指标（live_stream 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 推流会话 ID（ULID，主键） |
| `key_id` | `int64` | 2 | — | 使用的密钥 ID |
| `stream_name` | `string` | 3 | — | 流标识（冗余，便于排障） |
| `room_id` | `int64` | 4 | — | 房间引用（live-room 主键） |
| `session_id` | `int64` | 5 | — | 场次引用，0 表示未绑定 |
| `anchor_mid` | `int64` | 6 | — | 主播 ID |
| `protocol` | [`IngestProtocol`](#enum-ingestprotocol) | 7 | — | 接入协议 |
| `node_id` | `string` | 8 | — | 当前接入节点，空串表示未分配 |
| `state` | [`StreamState`](#enum-streamstate) | 9 | — | 当前流状态 |
| `seq` | `int64` | 10 | — | 当前事件序号（live.state.v1 的 seq，单调递增） |
| `publish_started_at` | `int64` | 11 | — | 首次进入 PUBLISHING 的时间（Unix 秒） |
| `state_changed_at` | `int64` | 12 | — | 最近一次状态变更时间（Unix 秒） |
| `last_heartbeat_at` | `int64` | 13 | — | 最近一次心跳/上报时间（Unix 秒） |
| `interrupted_total_seconds` | `int64` | 14 | — | 本次推流累计中断秒数 |
| `interruption_count` | `int32` | 15 | — | 本次推流累计断流次数 |
| `stop_reason` | [`StopReason`](#enum-stopreason) | 16 | — | 停流原因，非终态为 UNSPECIFIED |
| `health_state` | `int32` | 17 | — | 健康判定，见 HealthState |
| `health_reported_at` | `int64` | 18 | — | 最近健康上报时间（Unix 秒） |
| `video_bitrate_bps` | `int64` | 19 | — | 最近一次视频码率采样（bps） |
| `audio_bitrate_bps` | `int64` | 20 | — | 最近一次音频码率采样（bps） |
| `fps` | `int32` | 21 | — | 最近一次视频帧率（×100 存储后取整） |
| `packet_loss_ppm` | `int32` | 22 | — | 最近一次丢包率（百万分比） |
| `ctime` | `int64` | 23 | — | 建档时间（Unix 秒） |
| `mtime` | `int64` | 24 | — | 修改时间（Unix 秒） |

### message `ReportStreamStateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 流 ID（VerifyPublishAuth 返回值） |
| `state` | [`StreamState`](#enum-streamstate) | 2 | — | 目标状态（PUBLISHING/INTERRUPTED/STOPPED） |
| `node_id` | `string` | 3 | — | 上报来源节点，可空 |
| `reason` | `string` | 4 | — | 原因摘要（写入事件 payload，不含明文密钥） |
| `occurred_at` | `int64` | 5 | — | 事件发生时间（Unix 秒），0 表示服务端取当前时间 |
| `report_id` | `string` | 6 | — | 幂等键：同一 report_id 重放不产生新事件、不重复推进 seq |
| `expect_seq` | `int64` | 7 | — | 乐观并发：非 0 时要求当前 seq 等于该值，否则返回冲突 |
| `trace_id` | `string` | 8 | — | 链路追踪 ID |

### message `ReportStreamStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 流 ID |
| `state` | [`StreamState`](#enum-streamstate) | 2 | — | 迁移后的状态 |
| `seq` | `int64` | 3 | — | 本次迁移分配的事件序号（幂等重放时为首次的 seq） |
| `event_id` | `string` | 4 | — | live.state.v1 的 event_id（消费方去重锚点） |
| `room_id` | `int64` | 5 | — | 房间引用（便于调用方对账） |
| `session_id` | `int64` | 6 | — | 场次引用 |
| `replayed` | `bool` | 7 | — | true 表示命中 report_id，未产生新事件 |
| `applied` | `bool` | 8 | — | false 表示非法迁移或 seq 冲突，状态未变更 |
| `interruption_id` | `int64` | 9 | — | 关联的断流记录 ID，0 表示无 |
| `message` | `string` | 10 | — | 说明（非法迁移/终态/冲突等） |

### message `GetStreamStateReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 按流 ID 查询（与 room_id 二选一） |
| `room_id` | `int64` | 2 | — | 按房间查询其当前非终态流，0 表示不按房间查 |

### message `GetStreamStateReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream` | [`StreamInfo`](#message-streaminfo) | 1 | — | 流状态；查不到时 stream_id 为空 |
| `found` | `bool` | 2 | — | 是否存在匹配记录 |

### message `ListStreamsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `room_ids` | `int64` | 1 | repeated | 房间过滤（批量开播巡检），空表示不限制 |
| `node_id` | `string` | 2 | — | 节点过滤，空表示不限制 |
| `state` | [`StreamState`](#enum-streamstate) | 3 | — | 状态过滤，UNSPECIFIED 表示只看非终态（IDLE/PUBLISHING/INTERRUPTED） |
| `protocol` | [`IngestProtocol`](#enum-ingestprotocol) | 4 | — | 协议过滤，UNSPECIFIED 不限制 |
| `heartbeat_before` | `int64` | 5 | — | 只返回 last_heartbeat_at <= 该值（Unix 秒）的流，用于断流扫描；0 不限制 |
| `pn` | `int32` | 6 | — | 页码，从 1 开始 |
| `ps` | `int32` | 7 | — | 每页条数，服务端夹取到 MaxListPageSize |
| `operator_mid` | `int64` | 8 | — | 调用者（非运营只能看自己的流） |
| `admin` | `bool` | 9 | — | true 表示运营侧查询 |

### message `ListStreamsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `streams` | [`StreamInfo`](#message-streaminfo) | 1 | repeated | 流列表，按 last_heartbeat_at 升序（最可疑的在前） |
| `total` | `int32` | 2 | — | 符合条件的总行数 |
| `pn` | `int32` | 3 | — | 回显页码 |
| `ps` | `int32` | 4 | — | 回显每页条数 |
| `server_time` | `int64` | 5 | — | 服务端当前时间（Unix 秒） |

### message `CloseStreamReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 目标流 ID |
| `stop_reason` | [`StopReason`](#enum-stopreason) | 2 | — | 停流原因，UNSPECIFIED 时服务端按 ADMIN 记录 |
| `reason` | `string` | 3 | — | 原因摘要（进事件 payload） |
| `request_id` | `string` | 4 | — | 幂等键 |
| `operator_mid` | `int64` | 5 | — | 操作者（主播本人或运营），必须 > 0 |
| `admin` | `bool` | 6 | — | true 表示运营侧强制停流 |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `CloseStreamReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`StreamState`](#enum-streamstate) | 1 | — | 迁移后的状态（STOPPED；已终态时也是 STOPPED） |
| `seq` | `int64` | 2 | — | 本次停流事件序号 |
| `event_id` | `string` | 3 | — | live.state.v1 的 event_id |
| `interrupted_total_seconds` | `int64` | 4 | — | 本次推流累计中断秒数 |
| `replayed` | `bool` | 5 | — | true 表示命中 request_id |
| `applied` | `bool` | 6 | — | false 表示流已是终态且无变更（按幂等成功处理） |
| `message` | `string` | 7 | — | 说明 |

### message `ReportStreamHealthReq`

> --- 流健康检查 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 流 ID |
| `node_id` | `string` | 2 | — | 采样来源节点 |
| `video_bitrate_bps` | `int64` | 3 | — | 视频码率（bps） |
| `audio_bitrate_bps` | `int64` | 4 | — | 音频码率（bps） |
| `fps_x100` | `int32` | 5 | — | 视频帧率 ×100 取整（29.97fps -> 2997） |
| `packet_loss_ppm` | `int32` | 6 | — | 丢包率（百万分比） |
| `rtt_ms` | `int64` | 7 | — | 往返时延（毫秒），0 表示未测 |
| `sample_window_seconds` | `int32` | 8 | — | 本次采样窗口（秒），<=0 时取配置默认 HealthSampleWindowSeconds |
| `occurred_at` | `int64` | 9 | — | 采样时间（Unix 秒），0 表示服务端当前时间 |
| `report_id` | `string` | 10 | — | 幂等键：同一 report_id 重放只保留一条采样 |
| `trace_id` | `string` | 11 | — | 链路追踪 ID |

### message `ReportStreamHealthReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `health_state` | [`HealthState`](#enum-healthstate) | 1 | — | 服务端判定（DEGRADED/CRITICAL 时 reason 给出越界指标） |
| `triggered_interrupt` | `bool` | 2 | — | true 表示本次上报已触发 INTERRUPTED 迁移 |
| `seq` | `int64` | 3 | — | 若触发了迁移，本次事件序号；否则为流当前 seq |
| `event_id` | `string` | 4 | — | 若触发了迁移，事件 ID；否则空串 |
| `replayed` | `bool` | 5 | — | true 表示命中 report_id，未新增采样 |
| `message` | `string` | 6 | — | 说明 |

### message `GetStreamHealthReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 流 ID |
| `window_seconds` | `int32` | 2 | — | 聚合窗口（秒），<=0 时取配置默认 HealthSampleWindowSeconds |
| `sample_limit` | `int32` | 3 | — | 附带返回的最近采样条数，服务端夹取到 MaxSamplePoints |

### message `HealthSample`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `occurred_at` | `int64` | 1 | — | 采样时间（Unix 秒） |
| `video_bitrate_bps` | `int64` | 2 | — | 视频码率（bps） |
| `audio_bitrate_bps` | `int64` | 3 | — | 音频码率（bps） |
| `fps_x100` | `int32` | 4 | — | 帧率 ×100 |
| `packet_loss_ppm` | `int32` | 5 | — | 丢包率（百万分比） |
| `rtt_ms` | `int64` | 6 | — | 往返时延（毫秒） |

### message `GetStreamHealthReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 流 ID |
| `state` | [`StreamState`](#enum-streamstate) | 2 | — | 当前流状态 |
| `health_state` | [`HealthState`](#enum-healthstate) | 3 | — | 当前判定 |
| `health_reported_at` | `int64` | 4 | — | 最近上报时间（Unix 秒） |
| `avg_video_bitrate_bps` | `int64` | 5 | — | 窗口内平均视频码率（bps） |
| `min_video_bitrate_bps` | `int64` | 6 | — | 窗口内最低视频码率（bps） |
| `max_packet_loss_ppm` | `int32` | 7 | — | 窗口内最高丢包率（百万分比） |
| `sample_count` | `int32` | 8 | — | 窗口内采样点数 |
| `samples` | [`HealthSample`](#message-healthsample) | 9 | repeated | 最近采样点，按 occurred_at 升序 |
| `interrupted_total_seconds` | `int64` | 10 | — | 累计中断秒数 |
| `interruption_count` | `int32` | 11 | — | 累计断流次数 |

### message `StreamInterruptionInfo`

> 断流与重连记录（live_stream_interruption 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `interruption_id` | `int64` | 1 | — | 记录 ID |
| `stream_id` | `string` | 2 | — | 所属流 |
| `room_id` | `int64` | 3 | — | 房间引用 |
| `episode_no` | `int32` | 4 | — | 该流第几次断流，从 1 递增 |
| `node_id` | `string` | 5 | — | 断流时的接入节点 |
| `started_at` | `int64` | 6 | — | 断流开始（Unix 秒） |
| `ended_at` | `int64` | 7 | — | 断流结束（Unix 秒），0 表示仍在中断中 |
| `duration_seconds` | `int64` | 8 | — | 本次中断时长（秒），未结束为 0 |
| `end_reason` | [`InterruptionEndReason`](#enum-interruptionendreason) | 9 | — | 结束原因 |
| `reconnect_attempts` | `int32` | 10 | — | 期间重连尝试次数 |
| `start_event_id` | `string` | 11 | — | 开启该记录的 live.state.v1 event_id |
| `end_event_id` | `string` | 12 | — | 关闭该记录的 event_id，空串表示未关闭 |
| `reason` | `string` | 13 | — | 原因摘要 |

### message `ListStreamInterruptionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 按流查询（与 room_id 二选一） |
| `room_id` | `int64` | 2 | — | 按房间查询其所有流的断流记录，0 表示不按房间查 |
| `only_open` | `bool` | 3 | — | true 表示只返回未结束（ended_at=0）的记录 |
| `start_time` | `int64` | 4 | — | started_at 下界（Unix 秒），0 不限制 |
| `end_time` | `int64` | 5 | — | started_at 上界（Unix 秒），0 不限制 |
| `limit` | `int32` | 6 | — | 返回条数，服务端夹取到 MaxListPageSize |
| `trace_id` | `string` | 7 | — | 链路追踪 ID |

### message `ListStreamInterruptionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `interruptions` | [`StreamInterruptionInfo`](#message-streaminterruptioninfo) | 1 | repeated | 记录，按 started_at 升序 |
| `total` | `int32` | 2 | — | 符合条件总行数（上限 MaxCountRows，超出为 -1） |

### message `IngestNodeInfo`

> --- 接入节点 --- / 接入节点（live_ingest_node 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `node_id` | `string` | 1 | — | 节点 ID（主键，运维分配的稳定标识） |
| `name` | `string` | 2 | — | 节点展示名 |
| `region` | `string` | 3 | — | 地理区域码（例如 CN-East） |
| `protocols` | [`IngestProtocol`](#enum-ingestprotocol) | 4 | repeated | 支持的协议 |
| `endpoint_rtmp` | `string` | 5 | — | RTMP 接入地址（明文可下发，不含密钥） |
| `endpoint_srt` | `string` | 6 | — | SRT 接入地址 |
| `endpoint_webrtc` | `string` | 7 | — | WebRTC 接入地址（信令网关） |
| `state` | [`IngestNodeState`](#enum-ingestnodestate) | 8 | — | 节点状态 |
| `capacity_streams` | `int32` | 9 | — | 并发流配额 |
| `active_streams` | `int32` | 10 | — | 当前占用（由分配记录派生，允许短暂偏差） |
| `health_score` | `int32` | 11 | — | 健康分 0~100，分配打分依据 |
| `last_heartbeat_at` | `int64` | 12 | — | 最近心跳（Unix 秒） |
| `labels` | `string` | 13 | — | 运维标签（k=v;k=v，灰度/机房隔离用） |
| `ctime` | `int64` | 14 | — | 创建时间（Unix 秒） |
| `mtime` | `int64` | 15 | — | 修改时间（Unix 秒） |

### message `UpsertIngestNodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `node` | [`IngestNodeInfo`](#message-ingestnodeinfo) | 1 | — | 节点信息；node_id 必填，其余按字段非空更新 |
| `create_if_absent` | `bool` | 2 | — | true 表示节点注册（首次出现即建档） |
| `heartbeat_only` | `bool` | 3 | — | true 表示只刷新 last_heartbeat_at/active_streams/health_score |
| `operator_mid` | `int64` | 4 | — | 操作者（运营或节点身份），必须 > 0 |
| `request_id` | `string` | 5 | — | 幂等键 |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `UpsertIngestNodeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `node` | [`IngestNodeInfo`](#message-ingestnodeinfo) | 1 | — | 落库后的节点 |
| `created` | `bool` | 2 | — | true 表示本次新建 |
| `replayed` | `bool` | 3 | — | true 表示命中 request_id |

### message `ListIngestNodesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `region` | `string` | 1 | — | 区域过滤，空不限制 |
| `protocol` | [`IngestProtocol`](#enum-ingestprotocol) | 2 | — | 必须支持的协议，UNSPECIFIED 不限制 |
| `state` | [`IngestNodeState`](#enum-ingestnodestate) | 3 | — | 状态过滤，UNSPECIFIED 不限制 |
| `pn` | `int32` | 4 | — | 页码，从 1 开始 |
| `ps` | `int32` | 5 | — | 每页条数，服务端夹取到 MaxListPageSize |
| `operator_mid` | `int64` | 6 | — | 调用者，必须 > 0（节点信息属运维面） |

### message `ListIngestNodesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `nodes` | [`IngestNodeInfo`](#message-ingestnodeinfo) | 1 | repeated | 节点列表，按 health_score 降序 |
| `total` | `int32` | 2 | — | 符合条件总行数 |
| `pn` | `int32` | 3 | — | 回显页码 |
| `ps` | `int32` | 4 | — | 回显每页条数 |

### message `AssignIngestNodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 目标流 ID |
| `protocol` | [`IngestProtocol`](#enum-ingestprotocol) | 2 | — | 需要的协议（必须与流的接入协议一致） |
| `prefer_region` | `string` | 3 | — | 期望区域（就近接入），空表示不限制 |
| `prefer_node_id` | `string` | 4 | — | 指定节点（重连回到原节点），空表示服务端打分 |
| `force_reassign` | `bool` | 5 | — | true 表示把已分配流迁移到新节点 |
| `request_id` | `string` | 6 | — | 幂等键：同一 request_id 重放返回同一分配 |
| `reason` | `string` | 7 | — | 分配/迁移原因（观测与审计） |
| `trace_id` | `string` | 8 | — | 链路追踪 ID |

### message `AssignIngestNodeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `node_id` | `string` | 1 | — | 分配到的节点 |
| `node` | [`IngestNodeInfo`](#message-ingestnodeinfo) | 2 | — | 节点接入地址，入口据此把客户端重定向到边缘 |
| `assignment_id` | `int64` | 3 | — | 分配记录 ID |
| `score` | `int32` | 4 | — | 打分（观测：为何选它） |
| `prev_node_id` | `string` | 5 | — | 迁移前的节点，空串表示首次分配 |
| `replayed` | `bool` | 6 | — | true 表示命中 request_id |
| `message` | `string` | 7 | — | 说明（无可用节点等） |

### message `ReleaseIngestNodeReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 释放该流的节点占用 |
| `node_id` | `string` | 2 | — | 可选：仅当当前分配是该节点时释放（防止误释放） |
| `reason` | `string` | 3 | — | 释放原因 |
| `request_id` | `string` | 4 | — | 幂等键 |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `ReleaseIngestNodeReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `released` | `bool` | 1 | — | false 表示本就无生效分配 |
| `node_id` | `string` | 2 | — | 被释放的节点，空串表示无 |
| `replayed` | `bool` | 3 | — | true 表示命中 request_id |

### message `NodeAssignmentInfo`

> 节点分配记录（live_node_assignment 行投影）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `assignment_id` | `int64` | 1 | — | 记录 ID |
| `stream_id` | `string` | 2 | — | 流 ID |
| `room_id` | `int64` | 3 | — | 房间引用 |
| `node_id` | `string` | 4 | — | 节点 |
| `protocol` | [`IngestProtocol`](#enum-ingestprotocol) | 5 | — | 协议 |
| `state` | [`AssignmentState`](#enum-assignmentstate) | 6 | — | 记录状态 |
| `score` | `int32` | 7 | — | 分配打分 |
| `prev_node_id` | `string` | 8 | — | 迁移来源节点 |
| `assigned_at` | `int64` | 9 | — | 分配时间（Unix 秒） |
| `released_at` | `int64` | 10 | — | 释放时间（Unix 秒），0 表示未释放 |
| `reason` | `string` | 11 | — | 分配/释放原因 |
| `trace_id` | `string` | 12 | — | 链路追踪 ID |

### message `ListNodeAssignmentsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 按流查询（与 node_id 二选一） |
| `node_id` | `string` | 2 | — | 按节点查询其分配 |
| `state` | [`AssignmentState`](#enum-assignmentstate) | 3 | — | 状态过滤，UNSPECIFIED 不限制 |
| `pn` | `int32` | 4 | — | 页码，从 1 开始 |
| `ps` | `int32` | 5 | — | 每页条数，服务端夹取到 MaxListPageSize |
| `operator_mid` | `int64` | 6 | — | 调用者，必须 > 0 |

### message `ListNodeAssignmentsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `assignments` | [`NodeAssignmentInfo`](#message-nodeassignmentinfo) | 1 | repeated | 分配记录，按 assignment_id 倒序 |
| `total` | `int32` | 2 | — | 符合条件总行数 |
| `pn` | `int32` | 3 | — | 回显页码 |
| `ps` | `int32` | 4 | — | 回显每页条数 |

### message `StreamEventInfo`

> --- 事件与发布位点（live.state.v1） --- / 流状态事件（live_stream_event 行投影，payload 与 live.state.v1 对齐）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_id` | `string` | 1 | — | 事件唯一 ID（ULID，消费方去重锚点） |
| `stream_id` | `string` | 2 | — | 流 ID |
| `room_id` | `int64` | 3 | — | 房间引用 |
| `session_id` | `int64` | 4 | — | 场次引用 |
| `seq` | `int64` | 5 | — | 该流单调递增序号（乱序回退依据） |
| `from_state` | [`StreamState`](#enum-streamstate) | 6 | — | 迁移前状态 |
| `to_state` | [`StreamState`](#enum-streamstate) | 7 | — | 迁移后状态 |
| `node_id` | `string` | 8 | — | 关联节点 |
| `interruption_id` | `int64` | 9 | — | 关联断流记录，0 表示无 |
| `interrupted_seconds` | `int32` | 10 | — | to_state=STOPPED 时本次累计中断秒数 |
| `stop_reason` | [`StopReason`](#enum-stopreason) | 11 | — | 停流原因，非停流事件为 UNSPECIFIED |
| `reason` | `string` | 12 | — | 原因摘要（不含明文密钥） |
| `occurred_at` | `int64` | 13 | — | 事件发生时间（Unix 秒） |
| `ctime` | `int64` | 14 | — | 落库时间（Unix 秒） |

### message `ListStreamEventsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `stream_id` | `string` | 1 | — | 流 ID |
| `after_seq` | `int64` | 2 | — | 只返回 seq > after_seq 的事件（对账/补偿游标），0 表示从头 |
| `limit` | `int32` | 3 | — | 条数，服务端夹取到 MaxEventPageSize |
| `desc` | `bool` | 4 | — | true 表示按 seq 倒序（取最新事件） |

### message `ListStreamEventsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `events` | [`StreamEventInfo`](#message-streameventinfo) | 1 | repeated | 事件列表 |
| `max_seq` | `int64` | 2 | — | 该流当前最大 seq（消费方据此判断是否追平） |
| `has_more` | `bool` | 3 | — | 是否还有更新的事件 |

### message `GetEventPublishCheckpointReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `pending_limit` | `int32` | 1 | — | 返回待发布样本条数，服务端夹取到 MaxEventPageSize |
| `include_failed` | `bool` | 2 | — | 是否附带失败事件样本 |

### message `EventPublishCheckpoint`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `last_published_id` | `int64` | 1 | — | 已发布的最大 outbox id（位点） |
| `last_published_at` | `int64` | 2 | — | 最近发布时间（Unix 秒） |
| `pending_count` | `int32` | 3 | — | 待发布行数（含退避中） |
| `failed_count` | `int32` | 4 | — | 失败行数（需 RetryFailedEvents） |
| `oldest_pending_id` | `int64` | 5 | — | 最老待发布 outbox id，0 表示无 |
| `oldest_pending_at` | `int64` | 6 | — | 最老待发布事件的 occurred_at（Unix 秒），滞后度指标 |
| `lag_seconds` | `int64` | 7 | — | server_time - oldest_pending_at，无待发布时为 0 |
| `pending_sample` | [`StreamEventInfo`](#message-streameventinfo) | 8 | repeated | 待发布事件样本 |
| `failed_sample` | [`StreamEventInfo`](#message-streameventinfo) | 9 | repeated | 失败事件样本 |
| `server_time` | `int64` | 10 | — | 服务端当前时间（Unix 秒） |

### message `GetEventPublishCheckpointReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `checkpoint` | [`EventPublishCheckpoint`](#message-eventpublishcheckpoint) | 1 | — | 位点快照 |

### message `RetryFailedEventsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `event_ids` | `string` | 1 | repeated | 指定重试的事件 ID，空表示按 limit 批量重试全部失败事件 |
| `limit` | `int32` | 2 | — | 批量上限，服务端夹取到 MaxEventRetryBatch |
| `request_id` | `string` | 3 | — | 幂等键 |
| `operator_mid` | `int64` | 4 | — | 操作者（运营），必须 > 0 |
| `reason` | `string` | 5 | — | 重试原因（审计） |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |

### message `RetryFailedEventsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `retried` | `int32` | 1 | — | 本次被重置为待发布的事件数 |
| `remaining_failed` | `int32` | 2 | — | 仍处失败态的事件数 |
| `replayed` | `bool` | 3 | — | true 表示命中 request_id |
| `message` | `string` | 4 | — | 说明 |

### message `VerifyCdnCallbackReq`

> --- CDN / 入口回调鉴权 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `domain` | `string` | 1 | — | 触发回调的推流域名 |
| `stream_name` | `string` | 2 | — | 回调解析出的流标识 |
| `event_type` | `string` | 3 | — | 回调事件（publish/publish_done/unpublish 等，由厂商定义） |
| `timestamp` | `int64` | 4 | — | 回调时间戳（Unix 秒） |
| `nonce` | `string` | 5 | — | 回调随机串（防重放，唯一索引） |
| `signature` | `string` | 6 | — | 厂商签名（服务端只与 HMAC 结果比对，不落库明文） |
| `client_ip` | `string` | 7 | — | 回调来源 IP（服务端只存哈希，不入库明文） |
| `raw_params_digest` | `string` | 8 | — | 原始参数摘要（SHA-256 hex，留证用；调用方可直接传摘要） |
| `trace_id` | `string` | 9 | — | 链路追踪 ID |

### message `VerifyCdnCallbackReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `allowed` | `bool` | 1 | — | 签名与时间窗是否通过 |
| `reason` | `string` | 2 | — | 拒绝原因码（signature_mismatch/timestamp_skew/replayed_nonce/domain_unbound/stream_not_found） |
| `stream_id` | `string` | 3 | — | 解析出的流 ID，空串表示无法归属 |
| `key_id` | `int64` | 4 | — | 关联密钥 ID，0 表示未解析 |
| `room_id` | `int64` | 5 | — | 房间引用 |
| `suggest_state` | [`StreamState`](#enum-streamstate) | 6 | — | 建议推进的流状态（仍需调用方经 ReportStreamState 走合法迁移） |
| `callback_log_id` | `int64` | 7 | — | 本次回调留证记录 ID（live_cdn_callback） |
| `replayed` | `bool` | 8 | — | true 表示命中同一 nonce，回调已被处理过 |
