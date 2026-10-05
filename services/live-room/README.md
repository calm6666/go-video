# live-room

直播间业务状态服务：房间生命周期、开播门禁与场次簿记、主播绑定、禁播、直播分区、房间配置与回放引用。

当前状态：**契约 + goctl 生成 + model + 迁移 SQL + 配置装配 + 21 个 logic 方法实现 + `live.state.v1` 消费者全部落地**
（逻辑轮 2026-09-21；21 个方法都有构造器级用例，`internal/logic` 为 21 个测试文件 /
362 条顶层用例 + 198 个子用例，其中 `fakes_test.go` 是替身层；消费轮 2026-10-04 补上
`internal/consumer`，33 条顶层用例 + 39 个子用例，两种构建各跑一遍；明细见下方「测试覆盖」节）。
迁移 SQL 已在隔离实例复验为 `applied`（`deploy/migrations/README.md` 的
`live-room | go_video_live_room | 8 | applied` 行），列/索引正确性另由
`model/migration_parity_test.go` 逐列比对 `model/*Columns` 常量保证（详见「测试覆盖」）。

- **拥有数据**：`go_video_live_room` 库的 8 张 `live_*` 表——房间主体与状态版本、1:1 直播配置、主播绑定（含生效房主占位）、直播场次与开播快照、禁播记录与解除留痕、直播分区、状态流转日志、幂等与事件去重。
- **提供能力**：建房/改资料/关房、开播前置检查（`PrepareLive`）、开播/下播、推流状态事件入站、审核结论回写、禁播与解除、场次与回放引用、分区维护、房间与场次读接口。
- **依赖**：MySQL（自有库 `go_video_live_room`）、Redis（`CacheRedis`）、`creator`（主播直播资格）、`risk-control`（`CheckAction(ACTION_LIVE_START)`）、`moderation-orchestrator`（资料送审）；下游三个 client 全部 `optional` + `NonBlock`，缺配置时在调用点显式返回 `Err*NotConfigured`，不当作检查通过。Kafka 只在 `-tags liveroom_kafka` 且 `Kafka.Enabled=true` 时才是运行依赖；默认构建下把 `Enabled` 置 `true` 会让入口直接终止启动，不会静默地「不消费」。
- **约束**：房间状态与推流状态**分离**——本服务只存 `stream_id` 引用；房间状态只能由本服务的状态机矩阵推进，外部只能通过带 `event_id` 幂等的入口推进合法状态。

## 运行参数

| 项 | 值 |
|---|---|
| 入口 | `liveroom.v1.go`（单入口；入口/配置文件名约定见 `docs/commands.md` §7） |
| 配置 | `etc/liveroom.v1.yaml`（`internal/config/config_load_test.go` 用 `conf.Load` 真实加载 `etc/` 下每个 yaml） |
| 服务名 / etcd key | `liveroom.v1.rpc` |
| 监听 | `0.0.0.0:8119`（同批次 live-ingest 取 8118；8120/8121/8122 已被 live-media / live-gateway 占用） |
| proto | `rpc/liveroom.proto`，`package liveroom.v1`，生成物在 `rpc/*.pb.go` |
| 重新生成 | `./scripts/gen.ps1 -Service live-room`（AGENTS.md §4：框架代码只能由生成命令产出） |
| 构建标签 | 默认构建（不链接 Kafka 客户端）；`-tags liveroom_kafka` 才链接 `go-queue/kq`（见「事件消费」节） |
| 事件消费 | `internal/consumer` 消费 `live.state.v1` → `ReportStreamState`；`Kafka.Enabled` 示例值 `false`，本仓库从未与真实 broker 联调 |

## 与其他直播服务的边界（强约束）

跨服务只传业务主键（`room_id` / `session_id` / `stream_id` / `record_id` / `asset_id` / `aid` / `task_id` / `mid`），
`liveroom.proto` 不 import 任何其他服务的 proto；本服务不建跨库外键，也不直连别的服务的表或 Redis key。

| 事实 | 所有者 | 本服务的做法 |
|---|---|---|
| 房间业务状态、资料审核态、场次生命周期、开播门禁 | `live-room` | 唯一写入口，全部经状态机矩阵 + 条件 UPDATE |
| 推流密钥（明文/哈希/Vault 引用）、流状态机与 `seq` 真值、断流区间、接入节点 | `live-ingest` | **不 import 其 rpc 包**；只存 `stream_id` 字符串引用，不校验其存在性；状态经 `ReportStreamState` 入站 |
| 录制任务、转码、回放媒资与播放地址签发 | `live-media` / `transcode` / `asset` | 只存 `record_id` / `record_asset_id` / `record_aid` 三个引用；`live_type` 与 `record_enabled` 只是房间侧许可 |
| 弹幕、评论、屏蔽词 | `danmaku` / `comment` | 只有 `danmaku_enabled` / `reply_enabled` 开关 |
| 长连接广播（进房/退房/弹幕通道）、踢流 | `live-gateway` | 不直连，禁播只落状态与审计证据，踢流由 gateway/ingest 执行 |
| 主播身份与直播资格（`UpAttr from=2/3`） | `creator` | `PrepareLive` 经 `CreatorRPC` 读，绝不复制成常驻列 |
| 风控判定与黑名单 | `risk-control` | `CheckAction`，入参只带 `device_hash` / `ip_hash`，禁止明文 IP/设备号 |
| 内容审核队列与结论真值 | `moderation-orchestrator` | 只存 `moderation_task_id` / `verify_state` / `reject_reason` 投影 |
| 禁播原因的终端可见性 | `gateway/app` | 服务端不下发 `reason` 给观众端；`ListRoomBans` 是运营审计口，需 `operator_mid` |

**房间状态与推流状态刻意分成两套枚举**：`live_room.state`（业务态，本服务拥有）与
`ReportStreamStateReq.stream_state`（流态，live-ingest 拥有）。二者靠
「场次 `live_session.state` + `last_stream_seq`」对齐，不共用列、不做互相映射的隐式约定。

## RPC 方法（21）

写接口的幂等键：`request_id`（客户端/运营重试必须复用同一个值）或 `event_id`（事件入口）。
命中 `live_room_idempotency.uniq_dedup_key` 后回放首次结果并置 `replayed=true`——**不产生新写入、不推进状态、不占 `seq`**。
分页统一 `page`/`page_size`（由 `svc.PageSize` / `svc.AreaPageSize` 收敛，超上限直接 `ErrPageSizeTooLarge`，不静默截断）；
`ListSessions` 用 `cursor`（禁止 offset 分页：新场次不断插入会重复/漏项）。时间统一 Unix 秒。

### 房间主体与资料（4）

| 方法 | 幂等键 | 主要表 | 说明 |
|---|---|---|---|
| `CreateRoom` | `request_id` | `live_room` + `live_room_setting` + `live_room_anchor` + `live_room_state_log` | 校验分区可用与 `MaxRoomsPerOwner` → 落 `PENDING`/`verify_state=NONE`/`state_version=1` → 同事务建房主绑定与配置行 → 提交后送审回填 `moderation_task_id`。`ModerationRPC` 未配置返回 `ErrModerationNotConfigured`，不写「已送审」假状态 |
| `UpdateRoomInfo` | `request_id` | `live_room` + `live_area`(读) + `live_room_anchor`(读) + `live_room_state_log` | 终态房不可改；`UpdateProfile` 在**同一条 UPDATE** 里把 `verify_state` 重置为 `NONE`（改资料即重审），`allow_states=[PENDING,READY]` |
| `GetRoom` | — | `live_room` (+`live_room_setting` / `live_session` 可选) | 按 `room_id` 或房主 `owner_mid`；两者皆 0 返回 `ErrInvalidRoomID`。`with_setting` 缺行必须套服务端默认（不能让客户端把缺行读成「全关」） |
| `ListRooms` | — | `live_room` (+`live_room_anchor` 取「自己可见」) | `List` 与 `Count` 共用同一 `RoomListQuery`，保证 total 与页内容同口径；WHERE 恒带 `state > UNSPECIFIED` 挡掉未初始化脏行 |

### 开播链路（4）

| 方法 | 幂等键 | 主要表 | 说明 |
|---|---|---|---|
| `PrepareLive` | `request_id` | `live_room` + `live_room_ban`(读) + `live_room_setting`(读) + `live_room_anchor`(读) | 按固定顺序产出 5 个 checks（`anchor_qualification`/`risk_control`/`room_verified`/`not_banned`/`setting_ok`）；下游不可用一律 `passed=false` + `degraded=true`。仅当全通过且 `state=PENDING` 才 CAS 推进 `PENDING→READY` |
| `StartLive` | `request_id` | `live_room` + `live_session` + `live_room_state_log` | `READY→LIVING` + 新建场次（写标题/分区**快照**与 `stream_id` 引用）同事务；已有非终态场次即拒。`stream_id` 只是引用，密钥归 live-ingest |
| `EndLive` | `request_id` | `live_session` + `live_room` + `live_room_state_log` | `LIVING→READY` + 场次 `ENDED`；时长由 SQL 侧 `GREATEST(ended_at-started_at,0)` 与终态同条 UPDATE 落定。`end_reason` 只允许 `ANCHOR_STOP`/`STREAM_REPLAY` |
| `ReportStreamState` | `event_id` | `live_session` + `live_room` + `live_room_state_log` | `live.state.v1` 唯一入站写入口：`event_id` 去重 + `AdvanceStreamSeq` 严格递增守卫，乱序事件**绝不把状态往回拨**；`false` 时回查区分 `STALE` 与 `ILLEGAL_TRANSITION`。房间与场次投影同事务 |

### 房间处置与审计（4）

| 方法 | 幂等键 | 主要表 | 说明 |
|---|---|---|---|
| `CloseRoom` | `request_id` | `live_room` + `live_session` + `live_room_state_log` | 任意非终态 → `FINISHED`，进行中场次 `TERMINATED(REASON_ROOM_CLOSED)` + `ClearActiveSession`。已终态重复关闭按幂等重放返回，不报错 |
| `BanRoom` | `request_id` | `live_room` + `live_room_ban` + `live_session` + `live_room_state_log` | 需 `operator_mid>0`（`ErrOperatorRequired`）；同事务先 `Bans.Lift` 旧生效记录再 `Bans.Insert`，保证「一房间最多一条生效禁播」。永久禁播 `end_at=0` |
| `LiftBan` | `request_id` | `live_room_ban` + `live_room` + `live_room_state_log` | `Bans.Lift` 用 CAS（未命中按并发幂等重放）+ `BANNED→READY` 清 `ban_until`。无生效记录返回 `ban_id=0` + message，不谎报「解除成功」。cron 到期解除复用同一段逻辑 |
| `ListRoomBans` | — | `live_room_ban` | 需 `operator_mid>0`；`reason`/`lift_reason` 原样投影给运营侧，脱敏与可见性由 `gateway/admin` 决定 |

### 场次与回放（3）

| 方法 | 幂等键 | 主要表 | 说明 |
|---|---|---|---|
| `GetSession` | — | `live_session` | 按 `session_id`，或 `room_id` + `offset` 取最近第 N 场；查不到返回 `ErrSessionNotFound`，不返回零值 `SessionInfo` |
| `ListSessions` | — | `live_session` | `session_id` 倒序 cursor 分页，`next_cursor` 为空表示到底；cursor 解析失败返回 `ErrCursorInvalid`（不静默回到第一页） |
| `AttachReplay` | `request_id` | `live_session` + `live_room_state_log` | 单条 UPDATE 同时要求「属于该房间 + 已终态 + `CanReplayTransition(from,to)`」；只写 `record_id`/`record_asset_id`/`record_aid` 引用。置 `AVAILABLE` 前必须已由 live-media 确认媒资可播 |

### 配置与成员（3）

| 方法 | 幂等键 | 主要表 | 说明 |
|---|---|---|---|
| `UpdateRoomSetting` | `request_id` | `live_room_setting` | 整段覆盖式 `Upsert`（`ON DUPLICATE KEY UPDATE`，冲突键是 `PRIMARY KEY(room_id)`），客户端重试可安全重放。bool 为 `false` 即「显式关闭」，**不做「未传即不改」**（那是 `UpdateRoomInfo` 的口径） |
| `MutateAnchor` | `request_id` | `live_room_anchor` + `live_room`(读) | 校验角色/动作合法与三类上限（`MaxCohostPerRoom`/`MaxManagerPerRoom`/`MaxOwnerBindingsPerMid`）。房主移交只能走 `TransferOwner`（同事务先释放占位再绑定，杜绝无房主窗口）；房主行永不在此解绑 |
| `ListAnchors` | — | `live_room_anchor` | `role ASC, id ASC`（房主在最前，客户端可直接取首位）；本方法是房间成员的唯一读出口，不做跨房间聚合 |

### 直播分区（3）

| 方法 | 幂等键 | 主要表 | 说明 |
|---|---|---|---|
| `UpsertArea` | `request_id`（自然键 `area_name` 唯一） | `live_area` + `live_room`(读) | `area_id=0` 走 Insert，唯一键冲突直接 `ErrAreaNameConflict`（不做「先查再写」的竞态预检）；`LevelOf` 强制两级；停用前 `CountChildren`/`CountByArea` 有未关闭房间即 `ErrAreaInUse` |
| `ListAreas` | — | `live_area` | `parent_area_id`/`state` 用 `-1` 表示不过滤（**注意 `0` 是「只取一级分区」的合法值**，不能当缺省丢掉）；排序命中 `idx_parent_sort` |
| `ApplyRoomModerationResult` | `event_id` | `live_room` + `live_room_state_log` | `moderation.result.v1` 入站口；`in.task_id` 必须与 `live_room.moderation_task_id` 一致（迟到结论不得覆盖新任务，`ErrTaskMismatch`）。`BANNED` 房间不因资料审核通过而解封 |

## 表与迁移（8 张表 / 8 个文件）

`deploy/migrations/live-room/`，与 `model/` 的「一张表一个文件」一一对应：
（`model/doc.go` 与 8 个 model 文件的头注释都把「一张表一个文件」写成了引用，故未按「2-3 个主题分组文件」合并，
以免生成一批指向不存在文件的假引用；分主题的效果由 `0NNNNN` 递增序号保证。）

| 迁移文件 | 表 | 用途 | 关键键与索引 |
|---|---|---|---|
| `000001_create_live_room.sql` | `live_room` | 房间主体：状态、资料、当前场次投影、禁播到期 | `PRIMARY KEY(room_id)`；`idx_owner_room`、`idx_area_room`、`idx_state_room`、`idx_state_ban`、`idx_ctime` |
| `000002_create_live_room_setting.sql` | `live_room_setting` | 房间直播配置（1:1） | `PRIMARY KEY(room_id)`——**同时是 Upsert 的唯一冲突键，再加任何唯一键都会 UPSERT 命中错行** |
| `000003_create_live_room_anchor.sql` | `live_room_anchor` | 主播绑定：房主/联合主播/房管（软解绑保留行） | `PRIMARY KEY(id)`；`uniq_room_mid_role(room_id,mid,role)`、`uniq_active_owner(owner_room_id)`；`idx_room_role_state`、`idx_mid_state_room` |
| `000004_create_live_session.sql` | `live_session` | 场次：状态、开播快照、流事件序号、回放引用 | `PRIMARY KEY(session_id)`；`idx_room_session`、`idx_room_state_session`、`idx_state_session` |
| `000005_create_live_room_ban.sql` | `live_room_ban` | 禁播记录（临时/永久 + 解除留痕） | `PRIMARY KEY(ban_id)`；`idx_room_state_end`、`idx_mid_state_end`、`idx_state_end`；**刻意不建唯一键** |
| `000006_create_live_area.sql` | `live_area` | 直播分区（运营维护，两级） | `PRIMARY KEY(area_id)`；`uniq_area_name`、`idx_parent_sort` |
| `000007_create_live_room_state_log.sql` | `live_room_state_log` | 状态流转日志（房间/资料/场次/回放四类，append-only，无 `mtime`） | `PRIMARY KEY(log_id)`；`idx_room_type_log`、`idx_ctime` |
| `000008_create_live_room_idempotency.sql` | `live_room_idempotency` | 幂等与事件去重（`request_id` 与 `event_id` 共用一套键） | `PRIMARY KEY(id)`；`uniq_dedup_key(dedup_key)`——**除主键外只能有这一个键** |

要点：

- **可重放**：每个文件只有一条 `CREATE TABLE IF NOT EXISTS`，无 `ALTER`、无 `INSERT`/`UPDATE`/`DELETE`、无 `DROP`/`TRUNCATE`，可在线重复执行。
- **投影列不是真值**：`live_room.owner_mid`、`live_room.active_session_id`/`active_stream_id`、`live_session.duration_seconds` 都是**可重算的投影**——真值分别在同事务写的 `live_room_anchor.owner_room_id` 与 `live_session(started_at, ended_at, state)`。它们只加速读，不作为判据。
- **不留第三方凭据**：`active_stream_id`/`stream_id` 只存 live-ingest 给的标识字符串（`VARCHAR(64) COLLATE utf8mb4_bin`），不存密钥、不存哈希、不存 CDN 域名与厂商签名。
- **无跨库外键**：`area_id`/`record_id`/`record_asset_id`/`record_aid`/`moderation_task_id`/`mid` 全是业务主键引用，不建 FK；分区归属与「运营分区 vs 目录 `catalog_zone`」的重复问题见「已知缺口」。
- **每列都有 COMMENT，全表 `utf8mb4` / InnoDB**；时间列一律 `BIGINT` Unix 秒，`0` 表示「未发生/不适用」。
- **`NOT NULL` 且无 `DEFAULT` 是刻意约定**：`room_id`/`area_id` 等主键与必填 ID 不允许默认值——静默 `DEFAULT 0` 会把调用方的漏传写成脏行；`result_json` 是 `TEXT`，MySQL 本就不允许 DEFAULT。
- **排序可达性**：`ROOM_ORDER_LIVING_FIRST` 走 `(state = 3) DESC, room_id DESC`，靠 `idx_state_room` 前缀 + 文件排序；发现页无过滤条件是主键倒序扫 + `LIMIT`。二者都不新增冗余索引（已记入「已知缺口」）。
- **禁播/状态日志无 `mtime`**：append-only 表不留修改时间，避免「改过审计记录」的错觉。

## 状态机（4 套，全部在 `model/errors.go`）

所有迁移都是「条件 `UPDATE ... WHERE` 带 `from` 态与 `state_version`」+ `RowsAffected` 判定；
`(false, nil)` 表示并发下已被他人推进，调用方必须重读或按 `ErrConcurrentUpdate` 退出。
禁止「先 SELECT 再无条件 UPDATE」。矩阵里的未知取值一律拒绝（不允许 `default:` 放行）。

### 房间状态 `live_room.state`（1-6，`RoomState*`）

| from \ to | PENDING | READY | LIVING | FINISHED | BANNED | DISABLED |
|---|---|---|---|---|---|---|
| **PENDING(1)** | — | ✅ PrepareLive 全通过 | ❌ 必须先 READY | ✅ 主播主动关闭 | ✅ 审核/运营禁播 | ✅ 主播停用 / 运营下架 |
| **READY(2)** | ✅ 改资料重审后回退 | — | ✅ StartLive | ✅ | ✅ | ✅ |
| **LIVING(3)** | ❌ 直播中不得退回待完善（观众会看到不存在的房间） | ✅ EndLive / 流停止事件 | — | ✅ | ✅ | ❌ 必须先终止场次 |
| **FINISHED(4)** | ❌ | ❌ | ❌ | 终态（无出边） | ❌ | ❌ |
| **BANNED(5)** | ✅ 解禁且资料未通过 | ✅ 解禁且资料已通过 | ❌ | ✅ | — | ❌ 解禁落点不含停用 |
| **DISABLED(6)** | ✅ 重新送审 | ✅ 恢复启用 | ❌ | ✅ 彻底关闭 | ✅ 停用期间发现违规 | — |

- 判定入口：`ValidRoomState` / `CanRoomTransition` / `RoomTransitionTargets`（返回副本，不暴露矩阵内部 map）。
- `state_version` 每次迁移 +1，`Rooms.Transition` 用它做乐观锁；`live_room_state_log` 记前后值与 `source`。
- 只有 `FINISHED` 是终态：`DISABLED`（非违规停用）刻意留了恢复与禁播两条边，因此**不能当终态处理**。
  改资料走 `Rooms.UpdateProfile(allow_states=[PENDING, READY])`，`LIVING`/`BANNED`/`DISABLED`/`FINISHED` 都改不动。
- `BANNED` 的解禁落点取决于 `verify_state`（通过回 `READY`，否则回 `PENDING`），所以两条边都在矩阵里，由 logic 二选一。

### 资料审核状态 `live_room.verify_state`（1-4，`VerifyState*`）

`NONE(1) → REVIEWING(2) → { PASSED(3) | REJECTED(4) }`；`REJECTED → REVIEWING` 是**唯一**出口
（禁止直接写 `PASSED`，那等于绕过审核），`PASSED → REVIEWING` 允许（改资料即重审）。
`verify_state` 决定解禁落点是 `PENDING` 还是 `READY`。

### 场次状态 `live_session.state`（1-4，`SessionState*`）

| from \ to | PENDING | LIVING | ENDED | TERMINATED |
|---|---|---|---|---|
| **PENDING(1)** | — | ✅ 首帧/开播 | ✅ 推流从未到达 | ✅ 禁播/关房/流超时 |
| **LIVING(2)** | ❌ 不得回退 | — | ✅ 主播下播 | ✅ 禁播/关房/断流超宽限期 |
| **ENDED(3)** | ❌ | ❌ | 终态 | ❌ 时长簿记已落定 |
| **TERMINATED(4)** | ❌ | ❌ | ❌ | 终态 |

- `ActiveSessionStates = {PENDING, LIVING}` 是「同一房间至多一条非终态场次」这一不变量的读出口径（`FindActiveByRoom`）。
- `last_stream_seq` 只能严格递增（`AdvanceStreamSeq` 的 `WHERE last_stream_seq < ?`），这是事件乱序守卫的落库形态。
- 回放状态 `live_session.replay_state`（1-4）独立于场次状态：
  `NONE → { PROCESSING | AVAILABLE }`、`PROCESSING → { AVAILABLE | NONE | REMOVED }`、`AVAILABLE → REMOVED`、`REMOVED` 终态。
  `PROCESSING → NONE` 表示录制失败且确认无回放；`AVAILABLE → REMOVED` 表示版权撤回或违规下架（引用保留可审计），
  撤回后**不得自行复活**。`AttachReplay` 只接受目标 `PROCESSING`/`AVAILABLE`/`REMOVED`。

### 禁播记录状态 `live_room_ban.state`（1-3，`BanState*`）

`ACTIVE(1) → LIFTED(2) | EXPIRED(3)`，两个都是终态。永久禁播 `end_at=0` 只能被 `LiftBan` 解除，
到期扫描（`Bans.ExpireDue`）永不自动放行它。

### 推流状态（不归本服务，仅作入参）

`ReportStreamStateReq.stream_state` 取值与 live-ingest `rpc.StreamState` 严格一致：
1 `IDLE`、2 `PUBLISHING`、3 `INTERRUPTED`、4 `STOPPED`（`model.StreamState*`，由
`TestModelStateConstantsMatchProtoEnums` 锁死与 `rpc` 枚举的编号对应）。
`IDLE` 不产生迁移；`INTERRUPTED` 只记观测，超过 `StreamInterruptGraceSeconds` 才由终止事件置 `TERMINATED(STREAM_TIMEOUT)`。

## 事件消费（`internal/consumer`，live.state.v1）

live-room 是**纯消费端**：本服务不发布任何事件（`KafkaConf` 刻意没有 `PublishTopics`），
唯一的入站事件是 live-ingest 产出的 `live.state.v1`，翻译后交给既有的
`logic.ReportStreamState`。消费者**不新增任何表**：去重与乱序防护已经在那段 logic 里
（`live_room_idempotency.dedup_key` 唯一键 + `live_session.last_stream_seq` 单调守卫），
本包只做「翻译 + 决定位点能不能前进」。

| 文件 | 职责 | 为什么这么切 |
|---|---|---|
| `mapping.go` | 信封解析、payload 解码、`Translate` 成 `ReportStreamStateReq`、`Classify(result)` | 生产侧字段口径只在这里出现一次；`SupportedTopic` 由 `eventenvelope.Topic()` 推导，不手抄字符串 |
| `handler.go` | 三条判定线 + 进程内尝试封顶 + `Stats` 计数 | 满足 `kq.ConsumeHandler`；判定与 MQ 实现无关 |
| `queue.go` | `Settings`/`QueueFactory`/`Supervisor`、`ValidateKafka`、`EffectiveTopics` | Kafka 客户端不在这里；工厂接口让单测用假工厂驱动生命周期 |
| `wiring.go` | `NewSvcApplicator`、`Start` | 接线要调 `logic`，而 `logic → svc`，放 `svc` 就成环，故由入口文件调用 |
| `kafkaruntime_disabled.go` | `//go:build !liveroom_kafka`：`NewKqFactory` 恒返回 `ErrKafkaRuntimeNotBuilt` | 默认构建不链接 Kafka 客户端，也不假装在消费 |
| `kafkaruntime_kafka.go` | `//go:build liveroom_kafka`：`Settings` → `kq.KqConf` | 全仓库 Kafka 客户端只出现在这一个文件，换 MQ 只改这里 |

判定口径（`Handler.Consume` 返回值即「能不能提交位点」，返回 `nil` = 可提交）：

| 投递形态 | 结论 | 是否进 logic |
|---|---|---|
| 空白投递、信封语法或契约非法、`event_id` 空白或 >64 字节（`dedup_key` 列宽）、缺 `room_id`、`stream_state` 不在 1..4、`stream_seq<=0`、`occurred_at` 无法确定 | `skipped`，提交位点并写 **error** 日志 | **否**（0 次依赖调用） |
| `event_type` / `schema_version` 不是 `live.state` / v1 | `skipped`，提交位点，info 日志 | 否 |
| logic 返回 result 1..5（applied / duplicate / stale / illegal_transition / mismatch） | 各自计数，提交位点 | 是 |
| logic 返回未知 result、`(nil, nil)`、或依赖故障（MySQL/Redis 抖动） | 前 `Kafka.MaxRetries` 次返回错误让 broker 重投 | 是 |
| 同一 `event_id` 累计达到 `MaxRetries` | `given_up`，提交位点 + error 日志点名「需要运维用新 `event_id` 走 `ReportStreamState` 重放」 | 是 |

三条必须知道的边界：

1. **重投只对「抢键之前」的失败有意义**。`claimDedup` 发生在第一次读之前
   （`logic/reportstreamstatelogic.go:67`），抢键后的读或事务失败会把 `event_id` 永久留在
   去重表里，重投只能拿到 `result=2`（重复），房间投影停在失败前那一刻。
   本包不伪造「重试已成功」，只把这一条写进 `Consume` 的注释与 `given_up` 日志。
2. **尝试台账是进程内的、有容量上限的**（`MaxTrackedEvents` 默认 4096，打满逐出一条）。
   本服务没有持久化消费位点/死信表，不封顶会让一条注定失败的事件在分区里无限热循环，
   把后面的事件全堵住；被逐出的事件重新获得额度，代价是多几次无用功，正确性仍由唯一键兜住。
3. **只允许订阅 `live.state.v1`**。`ValidateKafka` 直接拒绝其他 topic（含 `moderation.result.v1`：
   入站 RPC 已有，但本仓库 moderation-orchestrator 还没有该 topic 的生产者，
   订阅它等于读出来再丢掉）；同时要求 `Offset∈{first,last}`、`Conns/Consumers/Processors/MaxRetries>0`、
   `Username`/`Password` 成对、`CaFile` 可读（go-queue 读到不可用证书会 **`log.Fatal` 打死进程**，
   所以必须提前以普通 error 拒绝），错误里逐条点名配置键。

启用方式（两步都要做，缺一不可）：

```sh
go build -tags liveroom_kafka ./services/live-room      # 1 链接 Kafka 运行时
# 2 把 etc 配置里的 Kafka.Enabled 置为 true（示例配置默认 false）
```

`Enabled=true` 而二进制没链接运行时、或消费参数不完整时，入口用 `logx.Must` **终止启动**：
宁可不启动，也不要带着「以为在消费、其实没有」的进程对外服务。
`Enabled=false`（仓库默认）时 `consumer.Start` 返回 `(nil, nil)`，本进程不消费，
房间投影只由 `ReportStreamState` RPC 推进，live-ingest 的事件留在 `live_ingest_outbox` 里不前进。

## 配置装配

- `internal/config/config.go`：`zrpc.RpcServerConf` + `CacheRedis`（**不叫 `Redis`**——`RpcServerConf` 内嵌同名 `RedisKeyConf`，会触发 `conflict key redis`，能编译但启动即挂）+ `DataSource` + `LiveRoom`（18 项领域参数：房间/绑定上限、标题与分区名长度、三档分页、断流宽限期、三类缓存 TTL、送审 business、保留天数、扫描批大小与 `BanExpirySweepEnabled`）+ `CreatorRPC` / `RiskControlRPC` / `ModerationRPC`（全部 optional）+ `Kafka`。
- 示例值不含真实凭据：`DataSource` 用本地 `root:root`，并在注释里给出 `secretRef: vault:...#dsn` 的生产写法。
- `internal/svc/servicecontext.go`：`sqlx.NewMysql` → 8 个 model + `*redis.Redis` + 3 个下游 client；`Notes()` 在启动日志里点名「配了开关但本轮没有执行者」的项（如 `BanExpirySweepEnabled=true` 而无 cron 接线、下游 client 缺 etcd key），不静默启动。
- 分页收敛的唯一执行点是 `svc.PageSize` / `svc.AreaPageSize`（共用 `pageSize`）：配置被写成 0/负数时退到本包兜底常量（20/100/200），保证「列表查询必须带 LIMIT」。
- `Kafka`（`KafkaConf` 13 项）：`Enabled`（默认 `false`）、`Brokers`、`Group`、`SubscribeTopics`、
  `MaxRetries`、`Offset`、`Conns`、`Consumers`、`Processors`、`ForceCommit`、`Username`/`Password`/`CaFile`。
  本轮**删掉了 `RetryBackoffSec`**：本服务没有持久化重试表，没有任何代码读它，
  重投节奏由 broker 与进程内 `MaxRetries` 决定，留着等于给出「有退避状态机」的假象。
  示例配置里 `Username`/`Password`/`CaFile` 一律留空（生产由环境变量/Secret 注入），
  这条口径由 `config_load_test.go:TestExampleKafkaBlockIsHonest` 钉住。
- `liveroom.v1.go` 调 `consumer.Start(c, ctx)` 并在 `logx.Must(err)` 之后 `defer sup.Stop()`：
  接线不能放 `internal/svc`（`logic → svc`，`consumer → logic`，放 svc 成环）。

## 已知缺口（本轮未做，逻辑轮或后续轮处理）

1. **只有编译 + 单测级证据，没有真实对端联调**：21 个 logic 方法在逻辑轮（2026-09-21）全部落地，
   本服务没有任何 logic 方法返回 `model.ErrNotImplemented`（该哨兵只剩 `model/errors.go` 的定义与
   model 层「不该返回它」的反向断言）。但 `creator` / `risk-control` / `moderation-orchestrator`
   三个下游只被测替身驱动过，`PrepareLive` 的降级支路（`degraded=true`）与 `ReportStreamState`
   收到的真实 `live.state.v1` 载荷都尚未实机验证。
2. **迁移 SQL 已在隔离实例复验为 `applied`，但真实/共享实例仍未执行**：列/主键/唯一键/索引与
   `model/*Columns` 及 model 里每条 SQL 的对齐由 `model/migration_parity_test.go` 静态校验（含「SQL 有但
   model 不用」必须写明理由的反向清单）；`deploy/migrations/README.md:85` 记
   `live-room | go_video_live_room | 8 | applied`（隔离实例 `127.0.0.1:3399`，数据目录 `.gotmp/mysql-data`）。
   本机 3306 是维护者真实库，全程未连接、未执行 `scripts/migrate.ps1`；上线前须由运维按 `docs/commands.md`
   在目标实例跑一次。
3. **`Bind(role=OWNER)` 的错行风险已在 logic 层关掉，代价是「房主移交无 RPC 入口」**：
   MySQL 的双唯一键冲突只更新「第一个命中的唯一键」所在行，因此 `Bind(role=OWNER)` 在 `uniq_room_mid_role`
   未命中而 `uniq_active_owner` 命中时会改写**原房主那一行**——静默失败而不是 `ErrDuplicateOwner`。
   逻辑轮的处置是一律拒绝：`mutateanchorlogic.go:157-160` 对 `role=OWNER` 返回
   `ErrAnchorRoleInvalid`（「房主绑定由 CreateRoom 建立，移交需 TransferOwner」），注释在 `:29-33`。
   遗留缺口：`model.TransferOwner`（`model/live_room_anchor.go:272`，同事务先释放占位再占位）
   **没有任何 logic 调用者**，`rpc.MutateAnchor` 里也没有移交动作位，因此本期换房主无路可走，
   需要在 `liveroom.proto` 增方法（契约缺口，改动要过生成与兼容性检查）。
4. **`UpsertArea` / `ListAreas` 的归属与 `catalog` / `ops-config` 重叠（需维护者裁决）**：本表 `live_area` 与 `deploy/migrations/catalog/000001_create_catalog_tables.sql` 的 `catalog_zone`（注释即「内容分区表」，列 `zoneid/name/parent`）语义高度重复，`ops-config` 也维护运营侧标签目录。当前实现按 proto 既成事实把「直播分区」的所有权放在 live-room（`live_room.area_id` 引用 `live_area`），本轮迁移**不写入任何种子分区数据**。是否合并到 `catalog_zone`（直播分区作为其一个 root）或明确双向同步规则，需要维护者决定；本轮**未自行迁移数据**。
5. **`live.state.v1` 消费者已接线，但没有 broker 级证据，且没有死信/持久重试状态**：
   `internal/consumer`（mapping/handler/queue/wiring + `liveroom_kafka` 标签的两个运行时）已落地，
   `liveroom.v1.go` 会随进程装配消费者，`ReportStreamState` 从此有了仓内调用者（此前只有 gateway 之外的空口契约）。
   仍缺三件事：
   - **没在真实 broker 上联调过**。`kq.NewQueue` 不做握手，本包的单测全部走假工厂与假 applicator，
     「拉取到消息 → 位点前进 → 故障重投」这条链路只有编译/vet/单测级证据；
     `Enabled` 因此在示例配置里保持 `false`。
   - **没有死信表**。放弃重投（`given_up`）只写 error 日志并提交位点，事件正文不回写任何表，
     事后只能靠 live-ingest 侧的 `live_ingest_outbox` 捞出原始载荷、由运维换新 `event_id` 走 RPC 重放。
   - **`event_id` 去重键在 `logic` 里先于第一次读被占用**（`reportstreamstatelogic.go:67`），
     所以抢键之后的失败重投只会得到 `result=2`；本包把契约非法的投递挡在 logic 之前正是为了少烧键，
     但烧键窗口本身在 logic/model 层，属于第 14 条同族缺陷，未在本轮改动。
   `moderation.result.v1 → ApplyRoomModerationResult` 的事件链路**仍不接线**：本仓库 moderation-orchestrator
   没有该 topic 的生产者（只有 `submitworkerresultlogic.go:49` 的 TODO），`ValidateKafka` 因此直接拒绝订阅它。
6. **禁播到期与幂等记录清理无执行者**：`Rooms.ListBansToExpire` + `Bans.ListDue`/`ExpireDue` 与 `live_room_idempotency` 的过期清理（`IdempotencyRetentionDays`/`SweepBatchLimit`）在 model 层已备好，归属 `services/cron`；`BanExpirySweepEnabled` 默认 `false`。
7. **未做无界扫描的读接口保护**：`ListRooms` 的发现页（无 `owner_mid`/`area_id`/`state` 过滤）与 `ROOM_ORDER_LIVING_FIRST` 的表达式排序在数据量上来后需要索引或缓存策略；当前只有 `idx_state_room` / `idx_ctime` 可用，不加冗余索引的取舍已记在迁移文件头。
8. **`Count` 类查询的硬上限未定**：`List*` 的 `total` 直接走 `COUNT(*)`，房间/场次表变大后需要上限或近似计数（live-ingest 已有 `CountHardLimit` 先例，本服务 proto 未定义该字段）。
9. **collation 偏离仓库惯例（需维护者确认）**：本轮按任务口径把 8 张表统一为表级 `utf8mb4` / `utf8mb4_0900_ai_ci`，
   并把 `stream_id` / `active_stream_id` / `dedup_key` 三列显式改为 `utf8mb4_bin`。
   但仓库现状是：`audit` / `operation` / `spm` / `recommend-rank` / `playback` / `live-gateway` 等用表级
   `utf8mb4_unicode_ci`，`event-collector` / `open-platform` / `private-message` 用列级 `utf8mb4_bin`，
   同批次 `live-ingest` 的建表语句**根本不带表级 COLLATE**（只写 `DEFAULT CHARSET=utf8mb4`，跟库默认走）。
   三列改 `utf8mb4_bin` 是正确性要求（大小写折叠会让两个仅大小写不同的 `event_id` 被判为重复并静默丢事件），
   但表级 COLLATE 是否要跟仓库多数派统一到 `unicode_ci`、或全库一次性收敛到一个值，需要维护者决定后统一改。
10. **gateway 已接入，但 21 个方法里只有 18 个可达**：`gateway/app` 挂 15 条 `/live/*` 路由
    （7 条观众面读 + 8 条主播面写），`gateway/admin` 挂 12 条 `/admin/live/*`
    （5 条进 `routePermissions`：`room/close`、`room/ban`、`room/ban/lift`、`setting/update`、`area/upsert`；
    7 条免鉴权读）。未走 HTTP 的三个是内部链路入口：`ReportStreamState`（本轮起由 `internal/consumer`
    消费 `live.state.v1` 调用，见第 5 条）、
    `ApplyRoomModerationResult`（审核回写，其事件在本仓库还没有生产者，见第 5 条）、`AttachReplay`
    （live-media 回写）——已记在 `docs/roadmap.md` 的刻意不暴露清单。
11. **`live_area` 的读写权限口径待定**：`/admin/live/area/list` 免鉴权、`/admin/live/area/upsert`
    受 `live:area:update` 保护，`/live/areas`（终端）也免鉴权。分区是运营内容而非公开配置，
    是否需要把读侧也纳入鉴权（或与第 4 条的归属裁决一并处理），尚未决定。
12. **`CreateRoom` 回写送审结果时丢掉了 `UpdateProfile` 的 CAS 结论（缺陷，只钉未改）**：
    `internal/logic/createroomlogic.go:168` 写成 `if _, err := ...UpdateProfile(...)`，第一返回值被丢弃；
    而 model 侧的 `UPDATE live_room ... WHERE room_id=? AND state IN (allowStates)`
    （`model/live_room.go:320-361`）命中 0 行时返回的是 `(false, nil)`——房间在本事务提交后、
    回写之前被别的入口推进过状态就会走到这一支。后果是两件事实同时错：应答仍声称
    `verify_state=REVIEWING` + `moderation_task_id=刚拿到的任务 ID`，并追加一条 `NONE→REVIEWING` 的状态日志，
    而库里 `verify_state` 仍是 `NONE`、`moderation_task_id` 仍是 `0`。
    修法要求「回写没落地就不谎报」（`ok==false` 时与送审失败同口径停在 `NONE`，
    并按 `ErrConcurrentUpdate` 记日志、不写状态日志行）。
    当前行为由 `createroom_logic_test.go:TestCreateRoomCasMissOnVerifyBackfillStillReportsReviewing`
    钉住（用 `raceBefore("live_room.UpdateProfile")` 复现并发推进）；改生产代码时该用例的期望要同步改。
13. **`StartLive` 的冲突日志文案与取值相反（缺陷，只钉未改）**：`startlivelogic.go:121` 说
    「按最早一条判定冲突」，但 `Sessions.ListActiveByRoom` 是 `ORDER BY session_id DESC`
    （`model/live_session.go:188-197`），所以 `actives[0]` 与紧随其后的错误消息
    （`:124`）里的 `session_id` 都是**最新**一场。判定结果不受影响（`len(actives)>0` 就拒），
    但运维照错误消息排障会拿到错的场次。改法：文案改成「按最新一条」，或按声明取末位。
14. **`StartLive`/`EndLive` 在业务守卫之前就抢 `request_id`（行为后果，未改）**：
    `startlivelogic.go:57-73` 的 `claimDedup` 早于房间状态、审核态、绑定、禁播、在播场次五道守卫，
    因此「未过审」「状态不对」这类拒绝会把键消费掉且永不回填结果；同一 `request_id` 重试
    恒为 `ErrIdempotencyResultMissing`（`helpers.go:361-363`），客户端必须换新键。
    已由 `TestStartLiveAcceptsExactlyTheStatesThatCanReachLiving`、`TestStartLiveUnverifiedRoomRejected`
    等用例的 `wantKeyBurnedNoResult` 断言钉住（EndLive 侧同形）。
    改法不唯一（挪抢键位置，或拒绝时删键并释放），需与维护者定口径后再动。
15. **`PrepareLive` 的四个结论面缺口（缺陷，只钉未改）**：
    - **A：CHALLENGE 与 BLOCK 共用一个 `deny_code`。** `conv.go:212` 声明了 `denyRiskChallenge = "risk_challenge"`，
      但整包（生产代码）没有任何引用点：`denyCodeForCheck`（`conv.go:247-265`）只看检查项 code，
      不看裁决类型，所以风控要求挑战时客户端拿到的仍是 `risk_denied`，只能靠 `retry_after_seconds` 反推。
      钉在 `TestPrepareLiveRiskDecisionMatrix` 的 CHALLENGE 两行（`wantDeny = denyRiskControl`）。
    - **B：`retry_after_seconds` 归因错位。** `preparelivelogic.go:124-127` 在「任一检查项未通过」时
      无条件回填风控算出的 `retryAfter`，即使首个失败项是资格/资料/禁播/配置。
      钉在 `TestPrepareLiveRetryAfterIsAttributedToRiskEvenWhenAnotherCheckFails`（同形态 ALLOW 对照为 0）。
    - **C：PENDING→READY 的迁移与审计不在同一事务，且审计失败只记日志。**
      `preparelivelogic.go:132`（`Rooms.Transition`）与 `:142`（`StateLogs.Insert`）是两条独立 SQL，
      `:147-149` 对 Insert 的错误只 `l.Errorf`，于是「房间已 READY 且对外回了 `ready=true`，
      但 `live_room_state_log` 一行都没有」是可达形态，违反 AGENTS.md §8「状态推进要留审计证据」。
      钉在 `TestPrepareLiveAuditInsertFailureDoesNotRollBackTransition`（`wantTxCount(..., 0)` + 审计 0 行 + 结果照样回填）。
      对照：`ApplyRoomModerationResult` 的同类写是在 `TransactCtx` 里的（见第 16 条）。
    - **D：`already_living` 分支是死代码，在播房间被误归因为「资料未通过」。**
      `preparelivelogic.go:128-130` 的 `case room.State == RoomStateLiving` 只有在五项全通过时才可能被评估，
      而 `verifyStateCheck`（`:237-249`）对 LIVING 直接判 `room_verified` 未通过，因此这条分支永远走不到；
      实际下发的 `deny_code` 是 `room_not_verified`。钉在
      `TestPrepareLiveLivingRoomReportsRoomNotVerifiedNotAlreadyLiving`（显式断言 `deny_code != denyRoomLiving`）。
16. **`ApplyRoomModerationResult` 的五个结论面缺口（缺陷，只钉未改）**：
    - **E：`verdict` 白名单只挡 `UNSPECIFIED`。** `applyroommoderationresultlogic.go:57-59` 的判定写成
      「等于 `VerifyStateUnspecified(0)` 且等于 `VERDICT_UNSPECIFIED`」，未定义取值（如 99）穿过守卫、
      先 `Claim` 掉 `event_id`、读完房间，才被 `verifyTargetForVerdict`（`helpers.go:401`）拒为
      `ErrVerdictInvalid`。钉在 `TestModerationResultUndefinedVerdictBurnsEventKey`
      （含「同键重投变成重复投递应答」的对照，说明坏投递会永久占掉这个 `event_id`）。
    - **F：业务守卫在抢键之后。** `ErrRoomNotFound` / `ErrTaskMismatch` / `ErrInvalidVerifyTransition` /
      `ErrConcurrentUpdate` 四类失败都发生在 `claimDedup` 之后，键已消费且永不回填结果
      （`wantKeyBurnedNoResult`）。与第 14 条同因：消费方若按「报错就重投同一 `event_id`」实现，
      第二次会被判成重复投递而静默丢掉真结论。
    - **G：结论与现状一致时仍会补一条 `verify_state` 自迁移审计。** 资料已是 `PASSED`、房间还在 `PENDING` 时，
      重复的 PASS 结论会推 `PENDING→READY`（收敛，正确），但 `apply()` 无条件再写一条
      `state_type=2, from=3, to=3` 的资料审计（`applyroommoderationresultlogic.go:155-162`），
      审计表里出现一次没发生的迁移。钉在 `TestModerationResultSameConclusionStillMovesRoom`。
    - **H：`operator` 由调用方自报且不校验。** 本入口没有 `checkOperator`（对比 `banroomlogic.go:38`：
      `operator_mid<=0` 直接拒），因此审计行的「处理人」可为 0（机审）也可为任意伪造 mid。
      钉在 `TestModerationResultOperatorIsSelfReported`。
    - **I：`truncateRunes(reason, 250)` 不可达。** `:142` 与 `:159` 对审计文本做截断，
      但 `checkReason`（`helpers.go:156`）已经在 >250 时整条拒绝，超长尾巴永远不会走到截断分支；
      保留它等于留一条「以为会截断」的假路径。边界对照钉在 `TestModerationResultReasonWidthBoundary`
      （250 字符原样落库、251 字符整条拒绝且零依赖调用）。
    - 另记一条**矩阵后果**（不是本方法缺陷）：资料侧矩阵只允许
      `REVIEWING → PASSED/REJECTED`，所以「已 PASSED 的房间被巡检打回」「已 REJECTED 的房间复审改判通过」
      在本入口都是 `ErrInvalidVerifyTransition`（钉在 `TestModerationResultVerifyMatrixIsTheGate`）。
      要让结论回灌支持复审改判，得先改 `model/errors.go` 的 `verifyTransitions`，属于口径变更而非 bugfix。
17. **`live_room_idempotency` 的键空间没有按 `kind` 分域**：`dedup_key` 是全局唯一列
    （迁移 `000008`），`Claim` 与 `dedupRecord`（`helpers.go:345`）都只比 `rpc` 名，不比 `kind`，
    因此客户端 `request_id` 一旦撞上上游 `event_id`，就会被对方方法当作自己的重复投递放行。
    钉在 `TestModerationResultEventKeyIsGlobalNotPerMethod`（「kind 不参与判定」那一条）。
    契约里没有分域字段，改法要么加列、要么在键上拼方法域——都要过迁移与兼容性评审。

三处文档同步已在这一轮做完（不再是缺口）：`deploy/migrations/README.md` 补了
`live-room | go_video_live_room | 8 | applied` 行、`docs/commands.md` §7 把 `live-room` 列入有可运行入口的
43 个服务、`docs/api-and-events.md` §5 把 `live.state.v1` 的生产者改为唯一 live-ingest 且消费者含 live-room。

## 测试覆盖

离线单测（纯 Go 手写替身，不连 MySQL/Redis/etcd/MQ）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，21 个文件 `362/198`）

按层分四组：写侧房间主体与开播链路、写侧处置与配置、读侧、判定链与投影一致性。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **写侧：房间主体与开播链路** | | | |
| `createroom_logic_test.go` | 28 | 1 | 入参守卫全部在触库之前（表驱动 + 断言「此后一次依赖调用都不许发生」）；房间行 + 房主绑定 + 配置行 + 审计日志在同一个 `TransactCtx` 里按固定顺序写且四行都读回；送审是**提交后**的外部往返，失败必须吞掉并保持 `verify_state=NONE`；`request_id` 首次受理/命中重放零新写/键被他人用过拒绝三态 |
| `startlive_logic_test.go` | 19 | 9 | `request_id` 三态用 callLog 起点切片与计数双重锁死；合法 from 态不写死而由 `model.RoomTransitionTargets` 推出「能进 LIVING 的集合」并整跑 6 个态；房间 CAS 用读到的 `state_version`，`raceBefore` 推走版本后必须 `ErrConcurrentUpdate`；入参守卫在抢键前（键不烧）、业务守卫在抢键后（键烧但无结果）分头锁 |
| `endlive_logic_test.go` | 16 | 14 | 场次 LIVING→ENDED 与房间 LIVING→READY 两条边由 model 矩阵守；「EndLive 先于 AttachReplay」用「本方法绝不碰 replay_state/不调 AttachReplay」反向锁；房间 CAS 与场次 CAS 各布一个竞态钩子（钩子没触发 `checkRaces` 判红）；`end_reason` 白名单；下播不伪造回放可用性、BANNED 房间下播后仍是 BANNED |
| `preparelive_logic_test.go` | 16 | 23 | 六条入参守卫顺序 room_id→mid→request_id→platform→device_hash→ip_hash 全在触库前，未定义 platform 一律拒（不做「未知即放行」）；五项检查恒定按 anchor_qualification/risk_control/room_verified/not_banned/setting_ok 全量产出、任何一项失败都不短路；下游未接线 = `degraded=true` 且按未通过处理（CHALLENGE/BLOCK/UNSPECIFIED 不放行，decision=ALLOW 但 degraded 也算未通过）；只有全通过且 PENDING 才抬到 READY |
| `roominfo_logic_test.go` | 27 | 11 | 「未传即不改」的部分更新（与 `roomsetting` 的整段覆盖互为镜像，各钉一半）；差量为空时一行都不写、只回当前投影；`live_room.UpdateProfile` 这条 UPDATE 不推进 `state_version`；字段级守卫与可编辑状态判定都在 `Anchors.IsEnabled` 之后、`claimDedup` 之前（被拒不烧键）；全程不开事务（`wantTxCount` 0） |
| `roomsetting_logic_test.go` | 14 | 10 | 整段覆盖是契约：false = 显式关闭、nil setting 直接拒绝（「未传」无法与「全关」区分）；`live_room_setting.Upsert` 冲突键是 PRIMARY KEY(room_id)，更新分支保 `ctime` 只刷 `mtime`；归属只认生效房主（与 UpdateRoomInfo 的「房主或生效联合主播」差异被分别钉住）；本方法全程不开事务且抢键后的写不带房间状态 CAS（房间被并发关闭后配置行照样写进去） |
| `closeroom_logic_test.go` | 14 | 16 | FINISHED 是本服务唯一「关房即终态」入口，已在 FINISHED 走幂等重放零写入不抢键，而 BANNED/DISABLED 不是终态照样能关；`expectVersion` 恒传 0（关房不参与乐观锁，只有命中 0 行才 `ErrConcurrentUpdate`）；在播场次强制终止与房间迁 FINISHED 同事务且顺序固定（先场次、再 t3 审计、再房间、再 t1 审计）；`admin=false` 的房主归属校验在抢键之前，`admin=true` 完全不读 FindOwner；关房走 `clearActiveSessionPatch()` 一条 UPDATE 清挂机位 |
| **写侧：处置、成员、分区与事件入站** | | | |
| `banlift_logic_test.go` | 30 | 25 | BanRoom/LiftBan 成对钉：禁播时长只有一条写通道（`banEndAt` 算 `end_at` → `RoomPatch{BanUntil}` 落投影），永久禁播恒 `end_at=0` 且只能由 LiftBan 清 `ban_until` 才闭合；`live_room_ban` 是 append-only + 一次解除写三列（插入时 `lift_operator_mid/lift_reason/lifted_at` 是字面量 0/""/0，解除的 UPDATE WHERE 带 state=1 → 重复解除命中 0 行）；BanRoom 无归属校验、LiftBan 连 FindOwner 都不调（调用次数 0 锁死）；已在 BANNED 再禁播不走 TransitionTx（矩阵无自边）改走 SetBanUntilTx |
| `mutateanchor_logic_test.go` | 18 | 28 | 归属校验只有「生效房主」一条路且在抢键之前；角色归一（BIND 必须给具体角色且拒 OWNER，UNBIND 允许 0 但拒 OWNER）在两次读之后、抢键之前；本方法**没有任何事务**且不写 `live_room_state_log`；claim 早于「正在开播不可解绑」与配额检查，这类业务拒绝把 `request_id` 消费掉且永不回填结果（哨兵用例钉住）；`bound_count` 是按角色统计，与配额里不分角色的 `CountActiveRoomsByMid` 不是同一个数；UNBIND 丢弃 RowsAffected（没绑过也成功） |
| `moderationresult_logic_test.go` | 17 | 9 | 入参守卫顺序 event_id→room_id→task_id→reason→verdict 全在触库前（事件键不烧）；重复投递回 `applied=false` 且回的是**当前投影**而非首次快照；`task_id` 必须等于 `live_room.moderation_task_id`（陈旧结论不得覆盖新任务）；verify 矩阵与房间业务矩阵是两条线——资料通过只把 PENDING 抬到 READY，LIVING/BANNED 无对应合法边故只回写结论、绝不伪造迁移，BANNED 不因资料通过而解封 |
| `reportstream_logic_test.go` | 15 | 14 | **抢 `event_id` 键在读房间之前**（与其余写方法相反，读失败也会永久消费该键）；本方法从不调 `saveDedupResult`，kind=Event 的键恒停在「已登记、result_json 为空」形态（`duplicate()` 里读 result_json 的分支在生产路径不可达，用哨兵钉住）；「事件被丢弃」不用 gRPC error 而是 result=2..5 + nil error；事务内顺序固定（场次 seq → 房间迁移 + t1 审计 → t3 审计），房间 CAS 未命中会留场次写；seq 守卫未命中后回查区分「陈旧」(3) 与「并发推进」(4) |
| `attachreplay_logic_test.go` | 11 | 11 | 本方法全程不开事务（状态写是一句带条件的 UPDATE、审计走非 Tx 的 `StateLogs.Insert`）；归属与终态的读侧校验在抢键前、同一条件在 SQL WHERE 里再存在一遍；「同状态 + 同引用」短路不写 UPDATE/审计但回包 `replayed=false`（与其余写方法的重放语义不一致，哨兵钉住）；三个引用列只在 >0 时写所以补引用是增量的；target 只允许 PROCESSING/AVAILABLE/REMOVED；审计写失败被 `l.Errorf` 吞掉（状态已改、审计缺失、应答仍成功） |
| `upsertarea_logic_test.go` | 14 | 20 | 名称唯一性没有「先查再写」预检，`uniq_area_name` 是唯一真值并翻译成 `ErrAreaNameConflict`；层级校验在抢键之前、占用校验在抢键之后（同一方法两种顺序并存，键是否被消费分头断言）；`created` 的判据是 `area_id == 0`，Update 命中 0 行回 `ErrAreaNotFound` 不伪造 created；停用分区 = 不能再被新房间选择而非删数据，占用口径 5 个状态（FINISHED 除外）；Update 整行覆盖导致不带 `parent_area_id` 会悄悄把二级分区降级（哨兵用例） |
| `streamevent_logic_test.go` | 6 | 2 | `live.state.v1` → 房间状态迁移的映射表逐格钉住，并断言「任何被给出的迁移都必须在 model 矩阵内」——logic 不许自己发明边 |
| **读侧** | | | |
| `roomread_logic_test.go` | 27 | 2 | 锁的是**接线**而不是投影：守卫顺序（拒绝时一次依赖调用都不许发生）、取数路径（按 room_id 还是按房主、是否走绑定表回表）、查询条件是否原样到达 model、`total` 与页内容的口径关系；查无此行回 NotFound 而不是零值 Reply |
| `sessionread_logic_test.go` | 21 | 0 | 「游标而不是偏移」：`ListSessions` 只用 `session_id` 游标翻页（新场次不断插入，offset 必然重复/漏项），因此没有 total 也不数总数，游标解析失败必须报错不能退化成「当作第一页」；`GetSession` 的 session_id/room_id 二选一且 session_id 优先，负 offset 在触库前拒，查不到一律 `ErrSessionNotFound`（回零值等于谎报有一场没状态的直播） |
| `memberread_logic_test.go` | 30 | 0 | 三个读方法口径各不相同：`ListAnchors` 只读 `live_room_anchor`、不校验房间是否存在（房间删了绑定还在，审计要能看到），且 `AnchorListQuery.Offset` 到不了 SQL（缺陷已登记）；`ListAreas` 的「不过滤」是 -1 而不是 0（0 对 parent_area_id 与 state 都是真实取值），分区页上限 200 与房间列表 100 两条通道不得互相放水；`ListRoomBans` 是审计列表，必须带 `operator_mid` 归因且不看生效窗口（end_at 已过但 state 仍是 ACTIVE 的记录照样列出，回收由 cron 负责） |
| **判定链与投影一致性** | | | |
| `validation_logic_test.go` | 16 | 1 | 标题/封面/引用/理由的清洗与拒绝、标识去重、`trace_id` 脱敏与文本按列宽截断、`normalizePlatform` 拒绝小程序、分页边界、游标编解码、`nextCursor` 只在满页时给出、未知枚举过滤值拒绝、按动作定主播角色 |
| `conv_projection_test.go` | 12 | 1 | 反向遍历 `rpc.Xxx_name` 钉「proto 枚举 ↔ model 常量 ↔ 投影」三者不漂移（proto 新增枚举值不会编译报错，最坏是被静默当成合法状态投影给终端）；每个 end_reason 只有一个写入者；每个 verdict/anchor action 都有映射；平台枚举排除小程序；RPC name 常量与生成的 ServiceDesc 一致 |
| `statemachine_logic_test.go` | 11 | 1 | logic 依赖的房间迁移矩阵逐状态钉住（任何一条边的增删都必须同时改这里与 README，杜绝「代码悄悄放宽」）；verdict → verify 目标态、verify 结果 → 房间态、解禁后的房间态、重提交后的 verify 目标、end_reason 与回放 target 白名单 |
| **替身层（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 8 个 model 接口的内存实现 + 假连接 + 有序 callLog + `raceBefore` 钩子，见第 4 组 |

### 2. 其他层

- `model`（2 文件 `26/3`）：
  - `model_rules_test.go` `17/3`：四套状态机矩阵封闭性（房间/verify/场次/回放与禁播）、model 常量与
    `rpc` 枚举编号一致、每个 proto 枚举值都有 model 常量、SQL 片段构造器口径（`WHERE` 保证
    List 与 Count 对齐、排序是封闭集、`-1` 作为「不过滤」、`RoomPatch` 的 SET 子句、
    占位符绝不产出空 IN 列表）、rune 计数与 bool→int32 往返、**入参校验先于 SQL**
    （用 nil 连接调用 67 处，只允许拿到哨兵错误，panic 或 `ErrNotImplemented` 都算失败）、
    哨兵错误互不相同。
  - `migration_parity_test.go` `9/0`：解析 `deploy/migrations/live-room/*.sql` 与 `model` 的
    `*Columns` 常量逐列（含顺序）比对，校验主键/唯一键/索引是否覆盖 model 里每条 SQL 的访问路径、
    每列有 COMMENT、表选项、文件命名与升序、可重放且无禁用语句，并要求「SQL 有但 model 不查」的列
    显式声明预留理由。
- `internal/config`（1 文件 `2/1`）：`conf.Load` 真实加载 `etc/` 下每个 yaml，断言 `ListenOn=8119`、
  `DataSource` 指向 `go_video_live_room`、默认值与 `config.go` tag 逐字对齐、下游 etcd key
  与各服务自身 yaml 一致。
- `internal/svc`（1 文件 `4/0`）：`pagesize_test.go` 钉分页收敛是「列表查询必须带 LIMIT」这条硬约束的
  唯一执行点，直接构造 `ServiceContext` 调用、不给 DB/Cache 赋值——真接到 SQL 之前就该被拦住的入参
  绝不该走到连接池。
- `internal/consumer`（6 文件 `33/39`，默认构建与 `-tags liveroom_kafka` 各 33 条顶层用例）：
  - `mapping_test.go` `9/11`：`TestTranslateReadsProducersVerbatimMessage` 用的是 live-ingest
    真正写进 `live_ingest_outbox.payload` 的那段字节（字段名与顺序取自生产者），
    改任一侧字段名都会红；`SupportedTopic` 必须由 `eventenvelope.Topic()` 推导且等于 `live.state.v1`；
    回退链（`stream_id`←`aggregate_id`、`occurred_at`/`trace_id`←信封）与 `event_id` 去空白；
    `TestTranslateRejectsBeforeLogic` 11 行表 + 64 字节边界两侧各一次 + 手工构造「信封时间换算不出正秒数」
    的 Year-1 用例，逐条要求错误**归到具体哨兵**且点名配置/字段；上游多投的 `client_ip`/`stream_key`/`token`
    必须出不了 `req`（泄漏串逐个断言）；`Classify` 覆盖 logic 的全部 result 与 0/99/-1 三种未知形态。
  - `handler_test.go` `10/14`：`TestContractViolationsNeverReachLogic` 用 `calls==0` 证明「非法事件绝不进 logic
    （否则白烧 `event_id`）」；result 1..5 逐个允许提交位点；依赖故障原样交回队列（`errors.Is` 断到注入的那个错误）
    且第 `MaxRetries` 次转为 `given_up` + 释放台账；恢复后计数清零；未知 `result=99` 与 `(nil, nil)` 不算成功；
    台账容量打满时逐出（并断言被逐出的事件重投仍能拿到额度）；64 并发投递计数精确。
  - `queue_test.go` `8/14`：`ValidateKafka` 的 14 行「逐字段破坏」表要求错误**点名那个键**且带统一前缀
    （含「`Brokers: [""]` 也算空」这条由用例逼出来的修正）；`CaFile` 不可读必须拒绝（go-queue 会 `log.Fatal`），
    且校验错误不得回显口令/用户名；`EffectiveTopics` 去空白去重保序；`SettingsFrom` 逐字段透传
    （`Name`/`Log`/`Mode` 漏传会让 kq 重设全进程 logger）；假工厂驱动 `Supervisor` 的
    Start/重复 Start 拒绝/Stop 重复调用/工厂报错回滚/`(nil, nil)` 队列拒绝，并断言关闭顺序逆序、
    半途失败时 `Started()` 仍为 false。
  - `wiring_test.go` `3/0`：`Enabled=false` 返回 `(nil, nil)` 且坏配置也不阻塞启动；
    `Enabled=true` 缺 `ServiceContext` 先报这一条（守卫顺序）；`NewSvcApplicator` 只断言非 nil，绝不真调 logic。
  - `kafkaruntime_disabled_test.go` `3/0`（`!liveroom_kafka`）：默认构建必须返回 `ErrKafkaRuntimeNotBuilt`
    且错误文案给出可执行下一步；`RuntimeNotes` 不得出现「已在消费」，`Enabled=false` 分支要点名
    `live.state.v1`/`ReportStreamState`/`live_ingest_outbox`；`Enabled=true` 时 `Start` 必须把进程打死。
  - `kafkaruntime_kafka_test.go` `3/0`（`liveroom_kafka`）：`var _ kq.ConsumeHandler = (*Handler)(nil)` 是编译期钉子
    （go-queue 改签名就先编译失败）；工厂的 nil handler / 空 topic 守卫必须在碰 broker 之前生效；
    链接了运行时也**不得**跳过 topic 映射校验；notes 必须保留「未在 broker 上联调」声明。
    该文件不调用 `factory.New` 建真实队列，也不 `Start`：单测不依赖「`NewQueue` 不拨号」这个实现细节。
- 本服务没有 `internal/repository`（logic 直接读 `svc.ServiceContext` 上导出的 8 个 model 接口字段，
  装配点即替换点，因此该层无离线单测也不该有），也没有 `internal/policy` 目录。

### 3. 构造器级覆盖

`21/21`：探针取 `internal/logic` 全部 `New*Logic(`（21 个，与 21 个 RPC 方法一一对应：读侧 7 + 写侧 14），
`gaps:` 为空。注意「有构造器级用例」只等于「一次调用打了哪几条 SQL、按什么顺序、失败后库里留下什么形态」
被锁住，**不**等于所有分支都锁住——各文件未覆盖的分支写在该文件头注释与已知缺口里。

### 4. 替身层与断言口径

`internal/logic/fakes_test.go` 是手写替身集合，直接 implement `model/*.go` 的 8 个接口，
不 import 任何 `model.NewXxxModel(conn)`，不连 MySQL/Redis。四条读侧纪律 + 一条写侧纪律：

1. **读取返回值拷贝**：每个 `Find*`/`List*` 返回行副本，否则「有没有真的落库」「投影读的是哪一份数据」
   这类断言会被共享指针掩盖。
2. **写侧像真实 SQL 一样写**：`Insert` 自己分配主键（表内 max+1，模拟 AUTO_INCREMENT），
   只写生产语句里出现的列，**生产 SQL 不写的列一律不自动补**
   （`live_room_ban.insert` 恒把 `lift_operator_mid/lift_reason/lifted_at` 写成 0/""/0、
   `live_room_setting.upsert` 更新分支保留 `ctime`、`UpdateProfile` 不碰 `state_version`）；
   未实现的写方法返回 `errUnexpectedDependency` 而不是假成功；布数据也必须布成
   「生产写得出来的行」，否则直接 panic。
3. **有序 callLog**（`<表>.<方法>[:<键>]`）：既数次数也断顺序——List 先于 Count 且两次同一个 query、
   `owner_mid` 路径必须走绑定表取 id 再回表且不得再走 `Rooms.List`、守卫拒绝后一次依赖调用都不许发生；
   写侧的键只放调用方可见的入参，替身分配的自增主键不进键。
4. **事务不回滚**（写侧加的一条）：`fakeConn.TransactCtx` 直接调 `fn`，中途失败时前面已写的行
   **留在库里**（真实 MySQL 会回滚）。因此失败用例一律断言「失败后库里到底还剩什么」，
   绝不写「应当已回滚」这种假结论——这正是本套替身能暴露「副作用跨语句分裂」的地方。
5. 并发用 `raceBefore` 钩子在真实调用点前改库，钩子没触发时 `checkRaces` 直接判红（防止
   「并发用例其实没并发」）。
6. 断言强度口径：`wantSeq` 断的是 from 之后的**完整**序列（多一条就红，所以「零写入」不需要另写前缀计数，
   `HasPrefix(".Insert")` 这类永真断言一律不允许）；`wantMethodCount` 按去掉 `:键` 的方法名**精确**计数；
   `wantKeyBurnedNoResult` 钉「键被消费但没有结果」这一族缺陷。
7. 它证明不了什么：真实 SQL 文本与列宽、唯一索引与二级索引是否真的存在、`ORDER BY` 在并列值上的
   实际次序、驱动返回的 matched vs changed rows、`SELECT ... FOR UPDATE` 的行锁与隔离级别；
   `db.TransactCtx` 的回滚缺失意味着「事务原子性」在本服务单测里是**被观察的缺陷面**而不是保障。
   依赖真实 Redis 的缓存命中/失效也完全没覆盖（`ServiceContext.Cache` 是具体类型、没有接口缝，
   口径写在 `fakes_test.go` 文件头第 3 条）。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端；
  下游 `creator`/`risk-control`/`moderation-orchestrator` 三个 client 在单测里是替身或 nil，
  「未接线」一律按 `Err*NotConfigured` 走显式分支，不作为检查通过。
- `internal/consumer` 同理：处理判定用假 `Applicator`（记录每次入参）驱动，生命周期用假 `QueueFactory` 驱动；
  `-tags liveroom_kafka` 的那三个用例也不调用 `kq.NewQueue`、不启动队列，因此
  **「测试通过」不含任何 broker 证据**，位点提交与重投节奏仍需联调（见已知缺口第 5 条）。
- **本服务 0 条用例处于 skip**（主代理实测口径）。全仓 2026-10-04 普查：8 处 `t.Skip`/`t.Skipf` 调用点，
  本机真正触发 3 条，全部在 live-gateway（live-ingest 原有的那条已随缺陷 #1 修复而启用，
  明细见 `docs/roadmap.md` 实测表第 2 条）；不要把用例文件数当成断言都在跑，也不要把「有构造器级用例」
  读成「所有分支都在跑」——哨兵用例（`TODO(缺陷)`）钉的是当前行为，不是理想行为。
- 迁移 SQL 与真实库的列级对账只在隔离实例 `127.0.0.1:3399`（数据目录 `.gotmp/mysql-data`，
  库 `go_video_live_room`，8 个文件 `applied`）复验过，见上方已知缺口里的 `deploy/migrations/README.md`
  登记行；真实/共享实例未执行。
- 缓存面（Redis 命中/回填/失效）、`internal/server` 与 `rpc/*.pb.go` 等 goctl 生成壳不在单测范围内；
  本服务无 HTTP handler。
- 缺口清单的权威登记在上一节「已知缺口」，本节不新增缺口，只标注哪些缺口已有哨兵用例钉住。

### 6. 验证命令

```sh
go test -p 1 -count=1 ./services/live-room/...
gofmt -l services/live-room    # 必须为空
go vet ./services/live-room/...

# 消费者两种构建都要过（Kafka 运行时代码只在带标签时参与编译）
go build ./services/live-room/... && go vet ./services/live-room/internal/consumer/
go build -tags liveroom_kafka ./services/live-room/... && go vet -tags liveroom_kafka ./services/live-room/internal/consumer/
go test -count=1 -tags liveroom_kafka ./services/live-room/internal/consumer/
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455）。
`-race` 本机跑不了（CGO_ENABLED=0 且没有 gcc），并发用例只能用确定性总量断言。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。

## 自检

```sh
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
gofmt -l services/live-room                  # 无输出
go build ./services/live-room/...
go build -tags liveroom_kafka ./services/live-room/...
go vet ./services/live-room/...
go test -p 1 -count=1 ./services/live-room/...
grep -rn "add your logic here" services/live-room/internal/logic   # 无输出
```

测试分层、逐文件用例清单、替身纪律与覆盖边界见上一节「测试覆盖」（构造器覆盖 `21/21`）。
上面 `go test` 必须加 `-p 1`（Windows 页面文件限制，并发跑多个测试包会 OOM errno=1455）：
本节只列命令，不重复记数字，以免和「测试覆盖」节出现两套口径。
