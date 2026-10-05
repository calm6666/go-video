# inbox

站内信（收件箱）与未读状态服务：系统/运营消息投递、互动与内容与直播事件转成的站内提醒、
分类未读计数、游标分页收件箱与用户侧软删除。

- 数据所有者：inbox 服务（本库 5 张表只有本服务可读写，AGENTS.md §5）
- Owner：消息与触达域（运营侧发消息的入口在 `operation`，经 `gateway/admin` 转发，尚未接入）
- 数据库：`go_video_inbox`（`deploy/migrations/inbox/`）
- 注册中心 etcd Key：`inbox.v1.rpc`，默认监听 `0.0.0.0:8104`（8080 是 gateway/app 的 HTTP 端口，不能复用）
- 契约源：`rpc/inbox.proto`（`rpc/inbox.pb.go`、`rpc/inbox_grpc.pb.go`、`internal/server/inboxserver.go`、
  入口 `inbox.v1.go` 均为 `goctl`/`protoc` 产物，禁止手改；改契约后执行
  `powershell -File scripts/gen.ps1 -Service inbox`）
- 无 HTTP 面：本服务只提供 gRPC，`.api` 不存在，客户端一律经 `gateway/app` 聚合（AGENTS.md §6）

## 1. 职责与边界

| 归本服务 | 不归本服务 |
|---|---|
| 站内收件箱的写入与读取（消息主体 + 每人一行收件明细） | 站外触达（Push/短信/邮件）与其模板、供应商回执 → `notification` |
| 站内信的分类未读计数（系统/互动/内容/直播）与计数修复 | 动态（feed）的「有新动态」未读 → `feed` 自己计数，语义与 inbox 无关 |
| 把领域事件翻译成站内提醒（只翻已确定收件人的事件） | 决定「该发给谁」的 fan-out（粉丝列表、关注关系）→ `social-graph` / `feed` |
| 幂等投递（`idempotency_key`、`event_id` 去重）、退避重试、死信留档 | 事件生产与可靠投递（outbox + 发布器）→ 各业务服务 + `event-collector` |
| 用户侧软删除（只影响本人收件行） | 消息主体的撤回（`inbox_message.state`）目前只有 model 层能力，无 RPC 入口，见 §10 |
| 审核/版权/账号等系统消息的落库 | 审核结论本身 → `moderation-orchestrator`；版权窗口 → `rights` |

边界要点：

- **站内信归 inbox，站外通道归 notification。** `docs/service-catalog.md` 里
  「notify = inbox + notification」是历史合并草案，本期仍按两个服务、两套表实现；
  合并若发生，需先解决「收件箱是事实源、投递记录是通道回执」的所有权差异。
- **inbox 不做 fan-out。** 事件里没有明确收件人（`payload.recipients` / 互动目标 / 作者 / 主播）时，
  该事件按契约**跳过并记 succeeded**，不查粉丝表、不自行扩散（`internal/consumer/mapping.go`）。
  面向粉丝的广播（作品发布、开播）因此不由本服务发站内信。
- **未读计数不依赖 Redis。** 真值是 `inbox_user_message`，`inbox_unread_stat` 是可由明细重算的快照，
  Redis 只是快照的加速副本（AGENTS.md §5「未读计数可重算」约束）。
- 站内信正文/标题/`extra` 不得含手机号、IP、Token 等原文：消费侧只声明契约内字段，
  上游误投的敏感字段在反序列化时被丢弃（`mapping_test.go` 有回归用例）。

## 2. RPC 方法

`service Inbox`（gRPC，客户端 `rpc.NewInboxClient(zrpc.MustNewClient(...))`）：

| 方法 | 用途 | 幂等 / 关键约束 |
|---|---|---|
| `SendSystemMessage` | 系统/运营给 1~N 个用户投递站内信 | `idempotency_key` **必填**（服务端不代造），命中 `uniq_idempotency_key` 时返回首条 `msg_id` 且 `deduplicated=true`、`delivered=0`；收件人在单次调用内去重、丢弃 `mid<=0`，超 `Inbox.MaxRecipients` 拒绝 |
| `ListMessages` | 按分类游标分页拉收件箱 | 只读；`cursor` 不透明（内部 `(ctime,id)` 倒序），`next_cursor` 为空即到底；`ps` 超 `Inbox.MaxPageSize` 返回 `ErrPsTooLarge`；撤回的消息主体与本人已删除行不出现；`unread_total` 是附加信息，计数依赖故障时按 0 返回并记日志（不拖垮列表） |
| `MarkRead` | 批量标记已读 | 条件更新 `read_state=未读` 才计数，重复调用 `changed=0` 且不触碰快照；返回变更后 `unread_total` |
| `MarkAllRead` | 按分类（`CATEGORY_UNSPECIFIED`=全部）标记已读 | 同上，已全读时零写放大 |
| `GetUnreadCount` | 分类未读数 | 只读；读链路 Redis → `inbox_unread_stat` → 明细重算；`force_recompute=true` 跳过前两级 |
| `RecomputeUnread` | 从明细表重算并回写快照 + Redis | 计数漂移的**唯一修复入口**，可任意次重复执行；供 `gateway/app` 自助修复与 `services/cron` 校准调用 |
| `DeleteMessage` | 用户侧软删除 | 只改本人 `del_state`（并置已读），不影响其它收件人；一条都不属于该 mid 时返回 `ErrMessageNotFound` 而不是静默成功 |

错误由 `model` 的哨兵错误表达（`ErrInvalidMid`、`ErrEmptyRecipients`、`ErrTooManyRecipients`、
`ErrEmptyContent`、`ErrInvalidCategory`、`ErrInvalidIdempotencyKey`、`ErrMessageNotFound`、
`ErrInvalidCursor`、`ErrPsTooLarge`），经 gRPC status 出口；不外泄 SQL 片段或事件原文。

写路径的事务边界：消息主体 + 每个收件人的明细行 + 未读快照增量在**同一事务**内提交，
提交后才失效 Redis（不做增量写缓存），因此回滚不会留下脏计数。

## 3. 消费的领域事件

`internal/consumer` 是本服务唯一的事件入口（AGENTS.md §3：消费者放在拥有写入权的服务里）。

| Topic（`config.DefaultTopics`） | 生产者 | 分类 | 转成站内信的动作 | 跳过的动作（记 succeeded） |
|---|---|---|---|---|
| `engagement.action.v1` | `engagement` / `social-graph` | 2 互动 | `like`、`favorite`、`share`、`follow`、`comment`、`danmaku` | `cancel_like`、`unfollow`、未知/空 action |
| `content.published.v1` | `video` / `catalog` / `rights` | 3 内容 | `offline`、`expired`、`delete`（通知作者，附 `reason`） | `publish`、`update`（面向粉丝，属 fan-out） |
| `live.state.v1` | `live-ingest`（唯一） | 4 直播 | `interrupt`、`stop`、`ban`（通知主播） | `start`（面向粉丝）、`unban` |

- `live.state.v1` 的 payload 里**没有 `action`**：live-ingest 发的是状态迁移事实，只有 `stream_state`
  （1 IDLE、2 PUBLISHING、3 INTERRUPTED、4 STOPPED）。动作名因此在本服务侧推导
  （`internal/consumer/mapping.go` 的 `liveActionFromStreamState`）：3 → `interrupt`、4 → `stop`，
  1/2 → 空串即跳过。开播（PUBLISHING）不发站内信是刻意的：它面向粉丝，而本服务不持有粉丝关系，
  在这里编一个动作名只会多一条注定跳过的 `inbox_consumer_offset` 记录。
  若上游显式给了 `action`（`ban`/`unban` 这类状态机表达不了的语义），优先取它、推导只作兜底。
- 收件人取 `anchor_mid`（生产者已把它放进 payload，见 live-ingest README 的 payload 表），
  `anchor_mid` 为 0 时回落到 `recipients`；`room_id` 为 0 时回落到信封的 `aggregate_id`。
- 三个状态常量在 `mapping.go` 里就地写字面值并注释指向 live-ingest 的 `model.StreamState*`：
  跨服务直连别人的 model 违反 AGENTS.md §5，改动必须同步 `docs/api-and-events.md` §5 的 payload 表。

处理流程固定为：`eventenvelope.Validate`（前置，`ParseEnvelope` 内 UnmarshalJSON 自动执行）
→ 按 `event_id` 领取处理权 → 构造消息 → `repository.Deliver` 事务投递 → 回写终态。

### 去重与失败收敛（三层）

1. `inbox_consumer_offset`：`uniq_event_id(event_id)` + 状态机
   `received → processing → succeeded | retry | dead_letter`。终态不可回退，
   重复/乱序/迟到投递只会读到既有状态。`processing` 超过 `Kafka.StaleProcessingSeconds`
   视为进程崩溃遗留，可被其它实例抢占。
2. `inbox_message.idempotency_key = evt:<event_id>`：即使状态表被清空也不会重复投递。
3. Kafka 位点：`Kafka.ForceCommit=false`，处理失败时不提交位点，由 broker 重投兜底。

- 失败按 `base * 2^(n-1)` 退避（上限 `Kafka.RetryMaxSeconds`），第 `Kafka.MaxAttempts` 次仍失败
  即转入 `inbox_dead_letter`（`uniq(topic,payload_digest)` 留档，只存摘要与脱敏预览）。
- 信封解析失败、超长 `event_id`（落不进 `VARCHAR(64)`）等毒消息：按 `(topic, 摘要)` 留档并**确认位点**，
  避免分区被卡住；契约类错误（空 payload、非法分类/收件人）直接判死，不消耗重试配额。
- 事件 payload 里未声明的字段被丢弃，生产者新增字段不会打挂消费者（docs §2 兼容窗口）。

### 消费者启动方式（最终采用）

**随 RPC 进程按配置开关启动，装配点在 `internal/svc/servicecontext.go`（goctl 标注 Safe to edit 的手写文件），
不改生成入口 `inbox.v1.go`。** `NewServiceContext` 结束前调用 `startConsumer()`，三种结果都有明确日志、绝不静默：

1. `Kafka.Enabled=false`（示例配置默认值）：不连 Kafka。站内信只来自 `SendSystemMessage` RPC；
   若 `Kafka.RetrySweeperEnabled=true`（默认）仍启动退避清扫器，把历史上留在 `retry` 的事件继续推进。
   这条链路只依赖 MySQL，与 MQ 无关。
2. `Kafka.Enabled=true` 且二进制以 `-tags inbox_kafka` 构建：`consumer.NewSupervisor` 为每个 topic
   建一个 `kq` 消费者并启动清扫循环；`Kafka.*` 任一项不合法时 `ValidateKafka` 报错，错误信息点名具体配置键。
3. `Kafka.Enabled=true` 但默认构建（未链接 Kafka 客户端）：`NewKqFactory()` 返回
   `ErrKafkaRuntimeNotBuilt`，`logx.Must` 让进程启动即失败。

理由：入口文件是生成产物、不可手改（AGENTS.md §4），因此消费者生命周期只能放在 ServiceContext；
go-zero 的 `proc.AddWrapUpListener` 在 SIGTERM 时先跑 wrap-up 再关 gRPC，`Supervisor.Stop()`
会等待在途消息处理完，符合「优雅退出可验证」。不采用「独立消费者进程/独立入口」是因为那需要新增
非 goctl 的启动产物，且会让同一份配置出现两套装配路径。

**默认构建为什么不链接 Kafka 客户端**：依赖侧其实没有阻塞，`github.com/zeromicro/go-queue v1.2.2`
已在主 `go.mod` 的直接 require 里（`go.mod:9`），其传递依赖 `github.com/segmentio/kafka-go` 在
indirect 块与 `go.sum` 也都齐，`go build -tags inbox_kafka ./services/inbox/...` 实测 rc=0，
**不需要任何 go.mod/go.sum 变更**（2026-10-04 复核；本文件此前写的「只是 indirect 依赖、
直接 import 会报 `updates to go.mod needed`」在依赖上已经不成立）。
仍然保留构建标签的理由在运行侧：本仓库没有任何 Kafka broker 联调过，默认产物若链接了 kq，
启动日志就会出现「消费者已启动」，而那恰恰是 §9 禁止的伪造。
因此 Kafka 客户端被隔离在 `internal/consumer/kafkaruntime_kafka.go`（`//go:build inbox_kafka`）一个文件里，
默认构建走 `kafkaruntime_stub.go` 显式报错，而不是伪造「已在消费」。启用步骤：

```bash
# 无需依赖变更；这只打开编译路径，消费语义仍需在真实 broker 上验证
go build -tags inbox_kafka ./services/inbox
# 部署时把 Kafka.Enabled 置为 true
```

替换 MQ 实现时只需改 `kafkaruntime_kafka.go`：状态机、退避、死信与 `Store` 接口都不依赖具体 MQ。

## 4. 数据表

迁移目录 `deploy/migrations/inbox/`（`CREATE TABLE IF NOT EXISTS` + InnoDB + utf8mb4 + 中文 COMMENT，
可重复执行；每文件头部含 owner/影响/回滚/锁风险）：

| 文件 | 表 | 关键约束 |
|---|---|---|
| `000001_create_inbox_message_tables.sql` | `inbox_message` | 消息主体一行；`uniq_idempotency_key`；`idx_biz(biz_type,biz_id)`、`idx_ctime`；`content`/`extra` 为 TEXT 且禁止敏感原文 |
| | `inbox_user_message` | 每收件人一行；**`uniq_mid_msg(mid,msg_id)`** 保证重复投递不产生第二行；`idx_mid_ctime`（列表 ORDER BY）、`idx_mid_read_ctime`（只看未读）、`idx_mid_category_read`（分类计数） |
| | `inbox_unread_stat` | `PRIMARY KEY(mid,category)`，四分类全写（缺失记 0），可由明细完全重建 |
| `000002_create_inbox_consumer_tables.sql` | `inbox_consumer_offset` | **`uniq_event_id`**（幂等真值）；`idx_state_retry(state,next_retry_at)` 供清扫器取到期行；`payload` 仅未终结行持有原文，`MarkSucceeded` 置回 NULL |
| | `inbox_dead_letter` | `uniq_topic_digest(topic,payload_digest)` 留档幂等；只存摘要 + 脱敏预览，不存原文 |

Redis key 只用 `inbox:unread:<mid>`（hash，字段为分类），TTL `Inbox.UnreadCacheSeconds`；
不读写其它服务的 key（AGENTS.md §5）。

### 留存与回收

`inbox_consumer_offset` / `inbox_dead_letter` 是增长表，两张表都单列 `idx_ctime` 供按 `ctime` 过期删除：
`succeeded` 行（已无 payload）与 `open` 之外的死信可由 `services/cron` 归档清理。
**清理前必须确认 `inbox_message.idempotency_key` 仍在**：它是第二层去重，删了状态行也不会重复投递；
反过来若同时清掉消息主体，历史事件重投就会被当成新事件二次投递。

## 5. 依赖

| 依赖 | 用途 |
|---|---|
| MySQL（`DataSource`） | 上表 5 张自有表，库名必须与 DSN 一致：`go_video_inbox` |
| Redis（`CacheRedis`） | 未读快照加速层，可整体丢弃：清空后最坏是回源 `inbox_unread_stat`/明细重算 |
| etcd | 注册 `inbox.v1.rpc` |
| Kafka/Redpanda（`Kafka.*`） | 领域事件读取（见 §3，默认构建未链接客户端） |
| 调用方 | **目前仓库内无服务 import `go-video/services/inbox/rpc`**。预期：`gateway/app`（收件箱/未读/已读/删除）、`operation`（`SendSystemMessage`，经 `gateway/admin`）、`services/cron`（`RecomputeUnread` 定期校准、死信重放）。上游事件生产者 `engagement`/`video`/`catalog`/`rights`/`live-*` 只需保证信封契约 |

## 6. 启动方式

```bash
cd services/inbox   # 或仓库根目录加 -f 路径

powershell -File scripts/gen.ps1 -Service inbox                    # 契约变更后重新生成
powershell -File scripts/migrate.ps1 -Action up -Service inbox     # 建库建表（需本地 MySQL）
go run ./services/inbox -f services/inbox/etc/inbox.v1.yaml        # 只跑 RPC，不消费事件
go run -tags inbox_kafka ./services/inbox -f services/inbox/etc/inbox.v1.yaml  # RPC + 事件消费
```

自检：

```bash
go build ./services/inbox/... && go vet ./services/inbox/...
go test -p 1 -count=1 ./services/inbox/...   # -p 1 的原因见 §9.6
gofmt -l services/inbox    # 必须无输出
```

启动日志可验证：`inbox/svc:` 打印 Kafka 运行时状态与是否消费、`inbox/consumer:` 打印
已启动的 topics/group/brokers/force_commit。健康检查用 gRPC health 探针
（`grpc_health_probe -addr=127.0.0.1:8104`）；本服务无 HTTP，因此没有 `/api/healthz`。

## 7. 配置 key（`etc/inbox.v1.yaml`）

| Key | 说明 | 默认 |
|---|---|---|
| `Name` / `ListenOn` / `Etcd.Key` | 服务名 / 监听 / 注册 key | `inbox.v1.rpc` / `0.0.0.0:8104` / `inbox.v1.rpc` |
| `CacheRedis` | 业务缓存。**键名不能写 `Redis`**：`zrpc.RpcServerConf` 内嵌同名 `RedisKeyConf`，`conf.Load` 会报 `conflict key redis`，代码可编译但服务起不来（`internal/config/config_load_test.go` 是该回归） | `127.0.0.1:6379`, node |
| `DataSource` | MySQL DSN，库名 `go_video_inbox` | 本地示例；生产从配置中心/Secret 注入 |
| `Kafka.Enabled` | 是否随进程启动事件消费者；`true` 而运行时未链接 → 启动失败（§3） | `false` |
| `Kafka.RetrySweeperEnabled` | 是否单独启动退避重投清扫器（只依赖 MySQL） | `true` |
| `Kafka.Brokers` / `Group` / `Topics` | broker 列表 / 消费组 / 订阅 topic（空则用 `config.DefaultTopics`） | `127.0.0.1:9092` / `inbox.v1.consumer` / 三个 v1 topic |
| `Kafka.Offset` | 首次启动起点，只允许 `first`\|`last` | `last` |
| `Kafka.Conns` / `Consumers` / `Processors` | 每 topic 连接数 / 每连接拉取协程 / 并发处理协程 | 1 / 2 / 4 |
| `Kafka.ForceCommit` | 失败时是否仍提交位点。**必须保持 false**，否则退避重投只剩 DB 一条腿 | `false` |
| `Kafka.MaxAttempts` / `RetryBaseSeconds` / `RetryMaxSeconds` | 尝试上限与指数退避区间 | 8 / 10 / 1800 |
| `Kafka.StaleProcessingSeconds` | `processing` 超过该秒数视为崩溃遗留，可被重新抢占 | 300 |
| `Kafka.Username` / `Password` / `CaFile` | SASL 与 TLS 根证书路径，必须成对配置；生产走环境变量/Secret，**示例配置一律留空**（AGENTS.md §4） | 空 |
| `Inbox.PageSize` / `MaxPageSize` | `ListMessages` 默认与上限（超过返回 `ErrPsTooLarge`） | 20 / 50 |
| `Inbox.MaxRecipients` | 单次 `SendSystemMessage` 收件人上限；更大规模运营群发应走 `notification` | 500 |
| `Inbox.UnreadCacheSeconds` | 未读快照 Redis TTL，到期回源 DB | 1800 |
| `Inbox.MaxContentBytes` | 标题/正文字节上限（超长直接拒绝，避免打爆展示层） | 4096 |
| 内嵌 `ServiceConf` | `Mode`、`Log.*`、`Telemetry.*`、`Prometheus.*` 按 go-zero 约定 | — |

## 8. 回滚

按影响面从小到大：

1. **停消费**：`Kafka.Enabled=false` 滚动重启。RPC 读写不受影响，事件积压在 broker（保留期内可追平），
   已入 `retry` 的事件仍由清扫器推进。
2. **收缩订阅**：从 `Kafka.Topics` 移除单个 topic，只停一类提醒。
3. **限流**：调低 `Kafka.Processors`/`Consumers`，或收紧 `Inbox.MaxRecipients`。
4. **计数修复**：`RecomputeUnread`（逐 mid）或 `GetUnreadCount(force_recompute=true)`；
   极端情况 `DEL inbox:unread:*`（仅本服务前缀）后由 DB 快照回填。
5. **死信重放**：`inbox_dead_letter` 按 `state=open` 取出，人工确认后可回投 Kafka；
   `inbox_consumer_offset` 里把对应 `event_id` 的行删除或置 `state='retry'`、`next_retry_at=0` 即可重跑
   （`evt:<event_id>` 幂等键已存在时是 no-op，不会二次投递）。
6. **表结构回滚**：执行各迁移文件头的 `DROP TABLE`（`inbox_message`/`inbox_user_message` 是用户可见数据，
   先备份；`inbox_consumer_offset` 删除后历史事件会被当作首次投递，但 `uniq_idempotency_key` 仍阻止重复写入）。
7. **代码回滚**：本服务不写他人数据，回滚镜像即可；注意回滚到没有 `RetrySweeperEnabled` 的版本前，
   先确认 `inbox_consumer_offset` 里没有滞留的 `retry` 行。

## 9. 测试覆盖

离线单测合计 **109 个顶层用例 + 15 个子用例**：logic `54/14`、consumer `38/0`、repository `16/0`、
config `1/1`（逐文件与逐层数字取自 `.gotmp/readme-metrics/inbox.txt` 与
`.gotmp/readme-test-aggregate.txt` 的导出，本节不重新计数）。本服务 0 条用例处于 `t.Skip` 状态。
全部为纯逻辑单测，不连接 MySQL/Redis/Kafka/etcd（AGENTS.md §9：不用「永不失败的假实现」掩盖状态机）。

### 9.1 logic 用例清单（10 个文件，顶层 54 / 子用例 14）

八个用例文件对每个 RPC 方法都跑同一套四类断言，表里只写该方法独有的判定链：

- **参数守卫表**：多个非法入参逐一被拒，且用「守卫之后调用序列仍为空」证明守卫发生在触库/触缓存之前
  （`msg_ids` 为空这种「下游也会兜住」的守卫尤其要钉，否则退化成一次静默的空操作）；
- **正常路径逐字段投影**：`Deliver` 落库的 13 个字段、`Message` 的 11 个响应字段、
  快照四分类的每个键都用可辨识值核对，不写「等于刚写进去的值」这种永真断言；
- **失败传播**：每个下游依赖（消息主体写入、收件明细写入、快照增量/覆盖、明细重算、缓存）
  各注入一次具体错误，断言 `errors.Is` 命中、调用序列在失败点截断、事务不留半截副作用；
- **本方法的业务不变量**：见下表最后一列。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---:|---:|---|
| `sendsystemmessage_test.go` | 3 | 1 | 入参守卫与默认值补齐：`idempotency_key` 必填且被 trim、服务端不代造（否则调用方重试会重复投递）；`TestSendSystemMessageNilRequestIsRejected` 固定 nil 请求的拒绝口径。 |
| `sendsystemmessagelogic_test.go` | 4 | 1 | 投递主链路：`TestSendSystemMessageDeliversOneRowPerRecipientInOneTransaction` 把「主体 + 每收件人明细 + 快照增量」钉在同一事务；`TestSendSystemMessageIsIdempotentOnReplay` 命中 `uniq(idempotency_key)` 时不二次写行、不二次加计数；`TestSendSystemMessageInvalidatesAllRecipientsInOneCall`；`TestSendSystemMessagePropagatesDependencyErrors`。 |
| `listmessageslogic_test.go` | 12 | 2 | 列表只读收件明细、未读是附加信息：`TestListMessagesDegradesWhenUnreadReadFails` 断言缓存/权威计数失败时列表仍可用并按 0 返回（与写接口相反，缺口 13）；`TestListMessagesProjectsEveryField` 逐字段投影；`TestListMessagesSecondPage`+`TestListMessagesSurvivesCacheUnavailable` 覆盖 `(ctime,id)` 游标翻页与三级回落；`TestListMessagesUsesConfiguredDefaultPageSize`、`TestListMessagesRejectsBadParamsBeforeQuery` 证明守卫先于触库。 |
| `getunreadcountlogic_test.go` | 7 | 2 | 「Redis 加速副本 → `inbox_unread_stat` 快照 → 明细重算」三层回落顺序及每层回写口径：`TestGetUnreadCountServesFromCacheWhenHot`（并固定副本命中时 `mtime=0`，见缺口 12）、`TestGetUnreadCountRefillsCacheFromStatSnapshot`、`TestGetUnreadCountRecomputesWhenStatMissing`、`TestGetUnreadCountForceRecomputeRepairsBothLayers`、`TestGetUnreadCountDegradesWhenCacheUnavailable`。 |
| `markreadlogic_test.go` | 6 | 2 | 三条不变量：变更才刷新（`TestMarkReadIsIdempotentAndNeverDoubleDecrements`，重复标记 changed=0 且不二次扣减）；明细与快照同事务（调用轨迹 `:tx` 后缀是唯一证据）；写接口不静默降级（`TestMarkReadReportsFailureWhenAuthoritativeCountReadBreaks`）。 |
| `markallreadlogic_test.go` | 7 | 2 | 一条 UPDATE 影响多行的独有风险：`TestMarkAllReadClearsEveryCategoryAndSkipsAlreadyReadRows` 挡住已读行被反复改写；`TestMarkAllReadScopedToCategoryLeavesOtherCategoriesUnread` 证明分类过滤在 SQL `WHERE` 里而不是取回后在 Go 里筛；`TestMarkAllReadNeverTouchesOtherRecipients`；`TestMarkAllReadOnEmptyInboxFallsBackToRecompute`。 |
| `deletemessagelogic_test.go` | 8 | 2 | 越权必须报错而非静默成功：`TestDeleteMessageRejectsMessagesThatAreNotTheCallers` 要求返回 `ErrMessageNotFound`，不能退化成 `changed=0` 的假成功；`TestDeleteMessageDoesNotDisturbOtherRecipientsOrTheMessageBody`（一发多收，硬删主体会弄没别人的消息）；`TestDeleteMessageOfReadMessageChangesRowButNotCount` 区分 matched 与 changed 口径。 |
| `recomputeunreadlogic_test.go` | 7 | 2 | 纠偏工具必须从明细真值把派生两层一起修回：`TestRecomputeUnreadRepairsBothDerivedLayersFromTruth`、`TestRecomputeUnreadWritesAllFourCategoriesIncludingZeros`（`GROUP BY` 不出的分类也要写 0）、`TestRecomputeUnreadOfFullyReadInboxReturnsZeroSnapshot`、`TestRecomputeUnreadIsRepeatable`、`TestRecomputeUnreadIsScopedToRequestedMid`。 |
| `fakes_test.go` | 0 | 0 | 替身层本身（无断言），四条纪律见 §9.4。 |
| `unread_helpers_test.go` | 0 | 0 | 三层一致布景（`seedThreeUnread` 等，无断言）：「明细/快照/副本三者一致」是所有回写断言的前提。 |

### 9.2 其他层

| 文件 / 层 | 文件数 | 顶层 | 子 | 钉住了什么 |
|---|---:|---:|---:|---|
| `internal/consumer/consumer_test.go` | 1 | 23 | 0 | `event_id` 去重状态机（`TestProcessDeliversOnceAndMarksSucceeded`/`TestProcessDuplicateEventDoesNotRedeliver`/`TestProcessConcurrentClaimIsDeferredNotRedelivered`/终态不回退）、退避重投与死信收敛（`TestProcessTransientFailureSchedulesBackoff`、`TestProcessRetriesAdvanceBackoffThenDeadLetter`、`TestProcessPermanentContractErrorDeadLettersImmediately`）、毒消息按摘要留档与超长 `event_id`（`TestProcessMalformedEnvelopeArchivedWithoutClaim`、`TestProcessOversizedEventIDIsArchivedNotRetried`）、留档失败时不提交位点（`TestDeadLetterArchiveFailureKeepsOffsetUncommitted`）、`SweepOnce`/`RunRetrySweeper` 收敛与取消、`TestBackoffDelayGrowsAndCaps`、`TestValidateKafkaNamesTheBrokenKey`、`Supervisor` 的 topic 去重与 Start/Stop/回滚。**这一层验证的是消费判定与去重序列（内存 `Store` 替身驱动的 `received → processing → succeeded\|retry\|dead_letter` 状态机与调用顺序），不是真实 Kafka 投递**：broker 的拉取、位点提交、分区再平衡都不在被测路径上（默认构建不链接 Kafka 运行时，见 §10 缺口 1）。 |
| `internal/consumer/mapping_test.go` | 1 | 15 | 0 | 「事件 payload → 站内信」映射：信封解析与未知字段容忍（`TestParseEnvelopeRejectsBadInput`/`IgnoresUnknownFields`）、分类与文案渲染（`TestBuildEngagementLike`、`TestFollowMessageHasNoUnrenderedPlaceholder`、`TestBuildContentOfflineAppendsReason`、`TestLiveStateMessagesUseAnchorAsRecipient`）、收件人解析优先级与去重（`TestExplicitRecipientsWinAndAreDeduplicated`）、自互动/无收件人/fan-out 场景跳过（`TestBuildEngagementAuthorFallbackAndSelfLike`、`TestEngagementRevokeActionsAndUnknownActionSkip`、`TestContentPublishAndLiveStartSkipWithoutFanout`）、`occurred_at`→`ctime`、rune 级截断不切断 UTF-8（`TestLongTitlesAreClippedOnRuneBoundaries`）、敏感字段不外泄（`TestSensitivePayloadFieldsDoNotLeak`）、topic 随 `schema_version` 推导（`TestDerivedTopicUsesSchemaVersion`）。 |
| `internal/repository/repository_test.go` | 1 | 10 | 0 | 只测编排：投递的收件人去重与幂等 no-op（`TestDeliverSameIdempotencyKeyIsNoop`）、依赖失败时错误上抛且不失效缓存（`TestDeliverErrorPropagatesWithoutCacheInvalidate`）、未读快照与明细重算逐步一致（`TestUnreadSnapshotAlwaysMatchesRecompute` 覆盖投递/已读/重复已读/全读/删除/重复删除）、脏快照被 `TestRecomputeRepairsDriftedSnapshot` 修回、缓存三级回落（`TestGetUnreadCacheLayers`、`TestGetUnreadWithoutStatRowsFallsBackToRecompute`）、越权删除拒绝、入参校验的哨兵错误。 |
| `internal/repository/cursor_test.go` | 1 | 6 | 0 | `(ctime,id)` 游标必须稳定、不透明、可拒绝伪造（客户端直接回传该串）：往返、零值=第一页、URL 安全、非法/溢出游标拒绝、同秒翻页单调递减。 |
| `internal/config/config_load_test.go` | 1 | 1 | 0 | `etc/*.yaml` 必须真能被 `conf.Load` 加载，且 `DataSource`、`CacheRedis.Host` 非空——这是「`Config` 自带 Redis 字段与 `zrpc.RpcServerConf` 内嵌同名键冲突，代码可编译但启动即报 `conflict key redis`」的回归（子用例按文件展开）。 |
| `model/` | 0 | 0 | 0 | **无离线单测**（SQL 文本、列名、索引命中只在真实库上才暴露）。 |
| `internal/svc/` | 0 | 0 | 0 | **无离线单测**。 |
| `internal/server/`、`rpc/*.pb.go`、`*_server.go` | — | — | — | goctl 生成壳，不在单测范围内。 |

### 9.3 构造器级覆盖

**7/7**：探针 `PROBE 7 gaps:` 后为空，`internal/logic` 的 7 个 `NewXxxLogic` 构造器都有用例，
没有「只有间接断言、没有构造器级用例」的方法。

### 9.4 注入缝与替身口径

`ServiceContext.Repository` 是具体类型 `*repository.Repository`（`internal/svc/servicecontext.go`），
生产构造走 `New(rds, conn, c)`。inbox 的依赖在 `Repository` 内部本来就按接口持有
（`UnreadCache` + 5 个 model 接口），缺的只是把替身装进去的入口，因此 `internal/repository`
只新增一个 `NewWithDeps(conn, cache, cfg, msgMd, userMd, statMd, offsetMd, dlqMd)`：
`New` 仍是唯一生产入口并内部转调 `newRepository`，没有动 `svc`，也没有为测试再抽一层接口。

logic 用例据此组装**真实 Repository**，只把 7 个依赖（连接、缓存、5 张表的 model）换成内存替身，
于是「明细与快照同事务」「写提交后才失效 Redis」「三级回落与回填」「游标翻页」整条编排都在被测路径上，
而不是把 `Repository` 也 mock 掉。`conn` 不能省：`TransactCtx` 的边界与被包住的事务会话
（调用轨迹后缀 `:tx` / `:conn`）是这几条不变量的唯一证据。消费侧替身（`offset`/`dlq`）
一旦被 logic 路径调用就返回 `errUnusedDependency`，避免永不失败的假实现。

`internal/logic/fakes_test.go` 记了四条替身纪律（值拷贝、按 `%s.%s:%s` 顺序记录轨迹、
按方法粒度注入错误、**布数据走静默写入路径** `seedMessage`/`seedRow`/`seedStat`/`cache.warm`）。
第四条最容易踩坑：布景一旦经过公开方法，序列断言会把布景调用也算进去，红在难找的地方。
`msg_id` 由替身自增（真实环境是 `LAST_INSERT_ID`），因此它只出现在读回断言与用 `fmt.Sprintf`
现拼的序列里；要断言精确序列的场合改用固定布景 id（71xx 列表、73xx 未读布景、77xx 删除）。

覆盖边界（如实声明）：替身只复刻 model 层 SQL 的**语义**（`uniq(idempotency_key)` 的 ODKU 复用主键、
`INSERT IGNORE` 的 affected 口径、状态门槛 UPDATE 返回真实变更行数、`GROUP BY category`、
`ORDER BY ctime DESC,id DESC LIMIT ps+1`），不证明 SQL 文本与列名本身，那部分由
`deploy/migrations/inbox/*.sql` 与集成环境负责；`model/*.go` 仍无单测。事务原子性用
「写入立即可见 + 登记逆操作 + 出错逆序回滚」模拟，目的是钉住哪些语句进了同一个事务，
不验证 MySQL 的隔离级别；`UnreadCache.Get` 没有 error 返回值，Redis 故障在接口上就是 miss，
所以「缓存故障降级直读」断言的是回落而不是超时。替身也证明不了驱动侧口径：索引是否命中、
真实 `matched rows` 与 `changed rows` 的差别、并发事务下的间隙锁与隔离级别行为。

### 9.5 覆盖边界

- **不连接任何外部依赖**：无 MySQL、无 Redis、无 Kafka/Redpanda、无 etcd、无对象存储。
  未读三层结构（Redis 副本 / `inbox_unread_stat` 快照 / `inbox_user_message` 真值）的一致性只在
  内存替身上验证编排与回写时机，不验证真实 SQL 执行结果。
- **迁移 SQL 未在目标实例复验**：`deploy/migrations/inbox/*.sql` 目前只做到与 model 逐列比对，
  权威登记见 §10 缺口 10；本节不把它写成已在隔离实例 `127.0.0.1:3399` 复验过。
- **Kafka 真实链路未覆盖**：`-tags inbox_kafka` 的运行时文件（`internal/consumer/kafkaruntime_kafka.go`）
  不在默认构建内，`§9.2` 的 38 条 consumer 用例覆盖的是与 MQ 解耦的 `Supervisor`/`Store` 契约，
  即消费判定与去重序列，不是真实投递（§10 缺口 1）。
- **跨连接读复刻不出**：`refreshStatTx` 在事务内用池连接重算未读（§10 缺口 11），logic 替身对
  「事务内写入立即可见」，因此用例钉住的是「哪些语句在同一事务里」，不是这个 bug 的行为。
- **对外面尚未接线**：仓库内无服务 import `go-video/services/inbox/rpc`（§5），`gateway/app`
  聚合仍在桩上（§10 缺口 3），所以端到端「用户看到未读数」这条链路没有测试证据。
- **goctl 生成壳不在范围内**：`internal/server`、`rpc/*.pb.go`、handler 由生成器负责。

### 9.6 验证命令

```bash
go build ./services/inbox/... && go vet ./services/inbox/...
gofmt -l services/inbox                          # 必须无输出
go test -p 1 -count=1 ./services/inbox/...       # 离线单测
```

`-p 1` 必须带上：Windows 页面文件上限下，同时跑多个测试包会 OOM（errno=1455），
这与被测代码无关；`-count=1` 关闭测试缓存以拿到真实执行结果。

### 9.7 变异探针记录（2026-09-22）

三组，改坏真实规则都被用例抓红：去掉 `markReadBatch` 的 `n == 0` 短路 ⇒
`TestMarkReadIsIdempotentAndNeverDoubleDecrements` 的调用序列断言红（重复标记仍去重算并覆盖快照）；
绕过 `DeleteMessages` 的 `owned == 0` 判定 ⇒
`TestDeleteMessageRejectsMessagesThatAreNotTheCallers` 红（越权从 `ErrMessageNotFound`
变成 `changed=0` 的假成功）；删掉 `MarkRead` 的 `len(in.MsgIds) == 0` 守卫 ⇒
`TestMarkReadGuards` 两个子用例红。探针后已还原，`grep -rn "false &&" services/inbox/` 为空。

## 10. 已知缺口

1. **默认构建不消费 Kafka，真实 kq 路径只有编译证据、没有运行证据**：见 §3。
   2026-10-04 实测 `go build -tags inbox_kafka ./services/inbox/...` 与
   `go vet -tags inbox_kafka ./services/inbox/...` 均 rc=0，**依赖侧不再是阻塞**（原来写的
   「本期无法编译验证、需授权 go.mod 变更」已不成立）；但 `kafkaruntime_kafka.go` 的
   `kq.NewQueue`/分区/偏移提交从未对着 broker 跑过，§9 的用例覆盖的只是与 MQ 解耦的
   `Supervisor`/`Store` 契约。应加一条 `go build -tags inbox_kafka` 的 CI 检查，
   让这条路径至少不重新退化到编译不过。
2. **消息主体撤回无 RPC**：`model.InboxMessageModel.Withdraw`（`state=1`，对所有收件人隐藏）已实现但
   `rpc/inbox.proto` 没有对应方法，`ListMessages` 已按 `m.state=0` 过滤。运营撤回/版权下架要改站内信时，
   需先补契约（改 `.proto` 后跑 `scripts/gen.ps1 -Service inbox`）。
3. **无 HTTP 面**：`gateway/app` 的收件箱/未读/已读聚合桩尚未接入本服务 RPC（总任务
   「gateway/app 接入阶段1-2 新服务」未完成），当前只能经 gRPC 调用。
4. **死信重放是人工流程**：`inbox_dead_letter` 有 `state=open/replayed/ignored` 与
   `model.DeadLetterModel.ListOpen/MarkState`，但没有重放 job，也没有运营后台入口（应放 `operation` + `cron`）。
5. **`trace_id` 未落库**：`extraJSON.TraceID` 记录了信封里的 trace_id，但
   `inbox_message`/`inbox_consumer_offset` 都没有独立 `trace_id` 列，跨服务串链目前靠 `event_id`。
6. **留存清理未实现**：§4「留存与回收」描述的按 `ctime` 清理任务在 `services/cron` 中尚不存在，
   两张消费表会无界增长（`idx_ctime` 已就位）。
7. **`@提醒`/审核结果类系统消息没有生产者**：分类 1（系统）目前只能由 `SendSystemMessage` 写入；
   `docs/api-and-events.md` §5 尚未定义 `moderation.decision`/`mention.created` 事件，
   因此 @提醒 与审核通知的自动站内信未接入（AGENTS.md §8 要求审核结论由 `moderation-orchestrator` 发布）。
8. **`MaxContentBytes` 与消费侧截断口径不同**：RPC 侧按字节拒绝，事件侧按 rune 截断
   （`maxTitleRunes`/`maxContentRunes`），两条路径的正文长度上限不完全一致，需在契约评审时统一。
9. **未读快照无「按分类清零」优化**：`MarkAllRead`/删除路径都走「明细重算 + 覆盖快照」，
   重度用户（数万收件行）上单次成本随明细行数线性增长，必要时需改为增量扣减 + 定期校准。
10. **迁移 SQL 未在实例上验证**：本期禁止连库执行，`deploy/migrations/inbox/*.sql` 仍是「与 model
    逐列比对通过、未实际建表」的状态（总任务「隔离实例上验证阶段1-2 全部迁移 SQL」未完成）。
11. **`refreshStatTx` 在事务内用池连接重算未读（疑似生产缺陷，本期只登记未修）**：
    `internal/repository/repository.go` 的 `refreshStatTx` 调 `r.userMd.CountUnreadByCategory(ctx, mid)`，
    而 `model/inbox_user_message.go` 的该方法签名里没有 `session` 参数，固定走 `m.conn`。
    于是 `MarkRead`/`MarkAllRead`/`DeleteMessage` 在事务内读到的「未读分布」来自**另一条连接**，
    看不到本事务尚未提交的 UPDATE，写回 `inbox_unread_stat` 的快照会偏大（本次清掉的那几条仍被计入）；
    紧随其后的 `GetUnreadCount` 又刚被 `cache.Invalidate` 打掉副本，会命中这份脏快照，
    返回给客户端的 `unread_total` 偏大，直到下次重算或 `force_recompute` 兜底。
    修法：给 `CountUnreadByCategory` 增加 `session sqlx.Session` 参数（与 `MarkReadBatch` 同口径）
    并把事务会话传下去。logic 单测的替身对「事务内写入立即可见」，复刻不出这条跨连接读，
    因此 §9 的用例钉住的是编排（哪些语句在同一事务里），不是这个 bug。
12. **`GetUnreadCount` 在缓存命中时 `mtime=0`**：`GetUnread` 的 `SourceCache` 分支只带分类值，
    不带回更新时间（Redis hash 里也没存），因此副本命中时响应的 `mtime` 是 0，
    只有回源快照/重算两条路径才带真实 `mtime`。用例
    `TestGetUnreadCountServesFromCacheWhenHot` 把这个事实固定下来（避免后来者误当回归修掉），
    但客户端不能拿 `mtime==0` 判新鲜度；要统一需在缓存里并存 mtime。
13. **读接口与写接口的计数降级口径不同，契约里没写**：`ListMessages` 的 `unread_total`
    读失败时记日志并按 0 返回（列表本身可用，见 `TestListMessagesDegradesWhenUnreadReadFails`），
    而 `MarkRead`/`MarkAllRead`/`DeleteMessage` 的 `unread_total` 读失败直接整体报错
    （宁可不返回也不给 0，见 `TestMarkReadReportsFailureWhenAuthoritativeCountReadBreaks`）。
    两者都有用例，但 `docs/api/rpc` 的字段说明没区分，接入 `gateway/app` 时要在契约注释里写明，
    否则客户端会在列表里看到「0 未读」误以为已读生效。
