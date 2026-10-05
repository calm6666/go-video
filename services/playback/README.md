# playback

播放会话与播放授权（短期防盗链签名地址）服务，第二阶段起独立部署
（见 [docs/service-catalog.md](../../docs/service-catalog.md)）。

它只回答一个问题：**某个用户、在某个时间窗内，能不能取某个可播放对象**。
稿件、媒资、转码版本和版权窗口分别归 `video`/`asset`/`transcode`/`rights`，
本服务不复制它们的元数据，也不校验对象是否真的存在（AGENTS.md §5）。

- **拥有数据**：`playback_session`（播放会话/授权事实）、`playback_progress`
  （断点与播放质量，按 session 幂等）、`playback_outbox`（`playback.heartbeat.v1` 事件）。
- **提供能力**：签发短期防盗链播放地址（`GetPlaybackToken`）、CDN/网关回源校验
  （`VerifyPlaybackToken`）、播放心跳与断点（`ReportHeartbeat`）、会话详情查询
  （`GetSession`，供管理后台与排障）。
- **依赖**：`rights`（仅 PGC 版权窗口校验，通过 gRPC 契约）、MySQL、Redis（会话短缓存
  与播放计数）、CDN（A 型防盗链，配置私钥）、消息队列（只在 `-tags playback_kafka`
  构建下链接发送端；默认构建的 `NewSender` 恒返回 `ErrKafkaRuntimeNotBuilt`）。
  事件先与进度同事务写 Outbox，再由 `internal/publisher` 的循环投递（AGENTS.md §5）。
- **约束**：
  - 客户端永远拿不到对象存储长期密钥，只拿到带 `auth_key` 且**必然过期**的地址（§6）；
  - `Sign.PrivateKey` 未配置时 `GetPlaybackToken` 返回
    `playback: cdn private key is not configured`，**不签发无签名地址**；
  - PGC 版权窗口不可判定（`rights` 未配置或 RPC 失败）时返回
    `playback: rights service unavailable`，不退化为"默认可播"；
  - 写接口全部幂等：签发靠 `uniq_request_id`，心跳靠 `uniq_session_id` + 位置只前进；
  - 事件 payload 不含明文 mid、IP、签名串与播放地址（AGENTS.md §6）；
  - 不实现会员、订单、支付、清晰度计费（§1）。

## 为什么 UGC 不调用 rights

AGENTS.md §1/§5 规定整片版权内容（电影/电视剧/番剧）不允许通过普通投稿发布，
因此 UGC 的可播放性由稿件自身的发布状态与可见范围决定，那是 `video` 的领域，
由 `gateway/app` 在调用 `GetPlaybackToken` 之前完成校验；`playback` 只负责
"给谁、在什么时间窗内、能取哪个对象"。PGC 的版权窗口是时间相关的动态事实，
必须在签发的当下二次校验，所以只有 `content_type=PGC` 会调用 `CheckPlayable`，
并把窗口结束时间用于给授权到期时间封顶。

## 签名协议（CDN A 型防盗链）

```text
auth_key = ts-rand-uid-md5hash
md5hash  = MD5("URI-ts-rand-uid-PrivateKey")     # 32 位小写十六进制
```

- `URI` 是签发时绑定到会话的资源路径（`playback_session.uri`），换路径即失效；
- `ts` 是**过期时间点**（Unix 秒），不是签发时间，等于 `playback_session.expire_at`；
- `rand` 为 16 字节随机十六进制，保证同一秒签发的两个地址不同；
- `uid` 写入观看者 mid（游客为 `0`），仅供 CDN 日志排查；
- `PrivateKey` 只存在于服务端配置（Secret/Vault），永不落库、永不下发。

`auth_key` 不写进数据库：`GetSession` 因此不可能泄漏签名，回源校验时由客户端携带、
服务端用私钥重算比对。实现集中在 `internal/signurl`（纯计算，无 IO，便于确定性测试）。

## RPC 方法

package `playback.v1`，端口 `0.0.0.0:8102`，etcd key `playback.v1.rpc`。

| 方法 | 入参 | 出参 | 说明 |
|---|---|---|---|
| `GetPlaybackToken` | `GetPlaybackTokenReq`（content_type/content_id/object_key/mid/platform/region/request_id） | `GetPlaybackTokenReply`（session_id/play_url/auth_key/expire_at/ttl/key_id） | 校验资格→建会话→签名。`request_id` 必填且幂等：重放返回同一 `session_id`、同一 `expire_at`（不延长授权）；若原请求已过期则返回 `session expired, request a new token`，客户端必须换新 `request_id` |
| `VerifyPlaybackToken` | `VerifyPlaybackTokenReq`（session_id/uri/auth_key） | `VerifyPlaybackTokenReply`（allow/deny_reason/expire_at/play_count） | 回源校验。判定顺序：会话存在 → 未撤销 → URI 与会话绑定一致 → 签名正确未过期 → 会话未到期。首次放行按会话累加一次播放计数（Redis SETNX 去重），计数失败只告警不影响放行 |
| `ReportHeartbeat` | `ReportHeartbeatReq`（session_id/position_ms/duration_ms/buffer_count/avg_bitrate/last_error） | `ReportHeartbeatReply`（event_id/accepted_at/max_position_ms） | 同一事务内幂等 upsert 进度 + 写 `playback.heartbeat.v1` Outbox。位置用 `GREATEST` 只前进不回退；迟到心跳仍保留进度并带 `session_expired` 标记 |
| `GetSession` | `GetSessionReq`（session_id） | `GetSessionReply`（session/progress/latest_progress） | 管理/排障查询。`latest_progress` 是同一观看者对同一内容的最近进度，用于回答"为什么没从头播放" |

`deny_reason` 是面向客户端与 CDN 日志的稳定枚举：`session_not_found`、
`session_revoked`、`session_expired`、`uri_mismatch`、`bad_auth_format`、
`sign_mismatch`、`signer_disabled`（配置异常时宁可拒绝回源，不放行无法验证的地址）。

## 数据表

迁移脚本：`deploy/migrations/playback/000001_create_playback_tables.sql`（库名 `go_video_playback`，
只新增表、无跨服务外键；`CREATE TABLE IF NOT EXISTS` 可重复应用）。

| 表 | 用途 | 关键约束/索引 |
|---|---|---|
| `playback_session` | 一次播放授权一行，记录谁在什么窗口内能取哪个对象 | `PRIMARY(session_id)`、`UNIQUE(request_id)`、`idx_mid_ctime`、`idx_content`、`idx_state_expire` |
| `playback_progress` | 断点与播放质量，心跳幂等 upsert 的事实表 | `UNIQUE(session_id)`、`idx_mid_content_mtime`；`position_ms` 单调不减 |
| `playback_outbox` | 与进度同事务写入的事件信封 | `UNIQUE(event_id)`、`idx_state_next_retry`；`state` 0 待发布/1 已发布/2 失败 |

回滚：

```sql
DROP TABLE IF EXISTS `playback_outbox`;
DROP TABLE IF EXISTS `playback_progress`;
DROP TABLE IF EXISTS `playback_session`;
```

## 事件发布（playback.heartbeat.v1）

`ReportHeartbeat` 在同一事务里写进度与 `playback_outbox`，投递由 `internal/publisher`
的后台循环负责。循环本体不在本服务重写：顺序、退避、判死与「写库失败中断本批」
这些与业务无关的决策都在 [common/outbox](../../common/outbox/README.md)，本包只回答
playback 的三个问题。

| 适配点 | 位置 | 说明 |
|---|---|---|
| 列 → `outbox.Row` 映射 + 一致性反查 | `internal/publisher/outbox_store.go:100` | topic 由行的 `event_type`+`schema_version` 现场拼出；分区键取 `aggregate_id`（session_id）；`outbox.CheckRow` 反查列与 payload 是否同源 |
| 本服务产出的 topic | `internal/publisher/outbox_store.go:40` | `RequiredTopic()` = `playback.heartbeat.v1`，锚在 `model/errors.go:74` 的常量，不在 yaml 或字面量里重复 |
| 配置 → 发布参数 | `internal/publisher/params.go:47`、`:64` | `OptionsFrom` 逐键映射，`ValidatePublishKafka` 逐键点名报错 |
| 队列发送端 | `internal/publisher/kafkaruntime_kafka.go`（`-tags playback_kafka`） | 每 topic 一个 `kq.NewPusher(..., kq.WithSyncPush())`；异步模式吞发送错误，会把「没送出去」写成「已发布」，因此禁用 |
| 默认构建 | `internal/publisher/kafkaruntime_disabled.go` | `NewSender` 恒返回 `ErrKafkaRuntimeNotBuilt`，`Kafka.Enabled=true` 时进程启动即失败 |
| 启停 | `internal/svc/servicecontext.go:81`、`:91`、`:119` | `proc.AddWrapUpListener` 注册收尾，`startPublisher` 决定启不启，`stopWorkers` 等在途批次跑完才关连接 |

打开投递的完整步骤：`go build -tags playback_kafka` → 在 broker 上创建 topic
（刻意不自动建，名字写错时自动建出空 topic 比启动失败更难查）→ `Kafka.Enabled: true`。
本仓库从未连接过任何 Kafka/Redpanda，上面这条链路的投递语义没有实测证据（缺口 1）。

## 配置

`etc/playback.v1.yaml` 是本地示例，**私钥必须来自 Secret/Vault，不得提交真实值**。

| Key | 说明 |
|---|---|
| `Name` / `ListenOn` / `Etcd.Key` | `playback.v1.rpc` / `0.0.0.0:8102` / `playback.v1.rpc` |
| `Mode` | `dev`/`test` 才允许 `Sign.EnableAuthKey: false`；生产必须 `pro` |
| `Redis` | 会话短缓存 `pb:s:<session_id>`、首次放行标记 `pb:v:<session_id>`、播放计数 `pb:pc:<content_type>:<content_id>` |
| `DataSource` | MySQL DSN，指向 `go_video_playback` |
| `RightsRPC` | optional。留空则注入显式失败实现，PGC 一律拒绝签发 |
| `Sign.BaseURL` | CDN 访问域名，如 `https://play.example.com` |
| `Sign.PrivateKey` | 防盗链私钥。**留空时 `GetPlaybackToken` 直接报错，不签发未签名地址** |
| `Sign.KeyID` | 密钥轮换排障标识，随响应返回 |
| `Sign.TokenTTL` | 可选，默认 1800 秒；PGC 会被版权窗口结束时间进一步收紧 |
| `Sign.EnableAuthKey` | 生产必须 `true`；`false` 且非 dev/test 时启动即失败 |
| `Kafka.Enabled` | 默认 `false`（示例配置钉住该值）。置 `true` 需先 `-tags playback_kafka` 构建，否则启动即失败 |
| `Kafka.Brokers` | 队列地址；本地 compose 的 Redpanda 为 `127.0.0.1:9092`。无 `Username/Password/CaFile`：go-queue v1.2.2 的 `kq.NewPusher` 不暴露 SASL/TLS 注入口 |
| `Kafka.PublishTopics` | 只允许 `playback.heartbeat.v1`；多写或漏写都按配置键点名拒绝 |
| `Kafka.MaxRetries` | 单事件累计尝试上限（含首次），达到即 `state=2`（失败，转人工） |
| `Kafka.RetryBackoffSec` / `Kafka.RetryMaxBackoffSec` | 退避基数与上限（第 n 次失败后等 `base * 2^(n-1)`，夹在 `max`）；上限小于基数被拒 |
| `Kafka.PollIntervalSec` / `Kafka.BatchLimit` / `Kafka.SendTimeoutSec` | 轮询间隔、单轮批次、单条投递超时（超时按一次失败计入退避，不算已投递） |

`Kafka` 段刻意没有 `Group` 键：本服务只生产事件，没有任何代码读它（校验一个没人读的键是假严格）。

## 目录结构与生成边界

```text
services/playback/
├── rpc/playback.proto             人工维护的源契约
├── rpc/playback*.pb.go            goctl 生成，禁止手改
├── playback.v1.go                 goctl 生成的 RPC 入口（无 HTTP server）
├── etc/playback.v1.yaml           配置示例
├── model/                         手写 sqlx 模型（session/progress/outbox + 领域错误）
├── internal/
│   ├── config/config.go           手写（Safe to edit）
│   ├── svc/servicecontext.go      手写（Safe to edit）
│   ├── signurl/                   手写领域策略：CDN A 型签名与校验（纯计算）
│   ├── repository/                手写：MySQL + Redis + rights RPC 适配器
│   ├── publisher/                 手写：Outbox 发布适配层（循环在 common/outbox）
│   │   ├── outbox_store.go        列映射 + 一致性反查（outbox.Store 实现）
│   │   ├── params.go              配置 → 发布参数与逐键校验
│   │   ├── kafkaruntime_disabled.go   //go:build !playback_kafka
│   │   └── kafkaruntime_kafka.go      //go:build playback_kafka（唯一 import kq 的文件）
│   ├── logic/                     手写业务规则（含心跳事件 payload 组装）
│   └── server/playbackserver.go   goctl 生成，禁止手改
└── README.md
```

```powershell
# 契约变更后重新生成（从仓库根目录；logic 已存在时 goctl 不会覆盖）
./scripts/gen.ps1 -Service playback

go run ./services/playback -f services/playback/etc/playback.v1.yaml

# 带队列发送端的构建（Kafka.Enabled=true 必须用它，否则启动即失败）
go build -tags playback_kafka -o bin/playback-kafka.exe ./services/playback
```

健康检查用 gRPC health 探针（`grpc_health_probe -addr=127.0.0.1:8102`）。

## 测试覆盖

离线单测（纯 Go + 内存替身），不连接真实 MySQL/Redis/Kafka/etcd/对象存储，也不依赖网络。
数字由 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。
合计 **99 顶层 / 31 子用例**（logic `57/23`、publisher `24/3`、signurl `11/2`、repository `6/2`、config `1/1`）；
本服务没有处于 `t.Skip` 状态的用例。
动态口径（`-v` 实测，表格里的静态数会少算表驱动用例）：默认构建 `20` 顶层 + `21` 子用例，
`-tags playback_kafka` 构建 `21` 顶层 + `25` 子用例，两者 `0 SKIP / 0 FAIL`；
两个构建标签互斥，所以 `24` 个静态顶层不可能在一次运行里全部出现。

### 1. `internal/logic`（7 个文件：5 个用例文件 + `fakes_test.go` + `helpers_test.go`）— `57/23`

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `getplaybacktoken_test.go` | 12 | 7 | UGC 不调 rights 直接建会话、PGC 必须有可播窗口才签发；`request_id` 幂等——重放返回**同一** `session_id` 与同一 `expire_at`（不延长授权），跨用他人 `request_id` 被拒，重放到不可用会话时报错而不是换新；插入撞 `UNIQUE(request_id)` 时改用已存在的行（并发首请求不产生两条会话）；PGC 的 `expire_at` 被版权窗口结束时间收紧；私钥缺失**拒绝签发**、未签名地址只在 dev/test 出现；store 错误原样上抛；缓存预热写失败仍成功签发 |
| `verifyplaybacktoken_test.go` | 13 | 8 | 判定顺序（存在 → 未撤销 → URI 绑定 → 签名 → 未到期）与 `deny_reason` 是**应答而不是错误**；私钥缺失时拒绝回源；到期会话被推进为 expired 且**不写缓存**，`MarkExpired` 失败时照样拒绝；首次放行只计一次播放（SETNX 去重）、重复校验不重复计数、计数失败仍放行；缓存命中不再查库；Redis 故障时的降级方向被钉住；`uid` 段只用于日志（改 `uid` 不会导致校验失败，缺口 7 的护栏）；未签名模式跳过签名比对；游客会话接受 `uid=0` |
| `reportheartbeat_test.go` | 9 | 4 | 入参守卫；`duration_ms` 未知也接受；进度与 Outbox **同一事务**写入（`event_id` 可解码、信封字段齐）；`GREATEST` 让位置只前进不回退、迟到心跳保留进度并带 `session_expired` 标记；会话缺失或 store 故障时以失败收场（`rolledBack==1`）；进度写失败 ⇒ 一条事件都不留；不重放缓存错误 |
| `getsession_test.go` | 9 | 2 | 守卫在触库之前；「查无此会话」是错误而不是空应答；会话 + 进度逐字段投影（字段值互不相同以掩盖串字段）；`latest_progress` 是**同一观看者同一内容的另一条会话**的最近进度（回答「为什么没从头播放」）；游客不取最近进度；最近进度读失败被容忍、主读失败原样上抛；无进度时仍返回会话；命中暖缓存不再查库 |
| `heartbeatevent_test.go` | 7 | 1 | `playback.heartbeat.v1` 载荷的逐字段组装与 `session_expired` 标记；载荷**不含 PII**（AGENTS.md §7 的隐私纪律）；游客载荷的匿名口径；`hashMid` 稳定且带域分隔（同一 mid 在不同用途下不撞）；完播比计算；信封的 topic 与契约字段 |
| `helpers_test.go` | 7 | 1 | `clampExpireAt` 的夹紧口径、playback↔rights 的内容类型取值映射、未知 platform 必须拒绝而不是静默降级、签名错误映射到域错误、`deny_reason` 枚举稳定、`checkSameRequest` 的幂等比对到底比哪些字段、`uidOf` 的游客口径 |
| `fakes_test.go` | 0 | 0 | 替身层与断言工具（值拷贝、按顺序 `callLog`、按方法粒度注错、静默布景），不贡献用例数 |

### 2. `internal/publisher`（5 个文件 `24/3`）

| 文件 | 顶层用例 | 子用例 | 钉住了什么 |
|---|---|---|---|
| `outbox_store_test.go` | 8 | 1（9 例表驱动） | `RequiredTopic()` 是跨服务契约字面量 `playback.heartbeat.v1` 且必须由 model 常量拼出；列 → `outbox.Row` 映射（topic 现场派生、分区键取 `aggregate_id`、payload 原样透传）；**9 种不可发布判定**逐条点名（event_type 写歪、schema 升 v2、event_type 空、`event_id` 列空、分区键空、payload 非 JSON、缺 producer、payload 与列的 `event_id`/`aggregate_id` 不一致）；`ListPending` 跳过 nil 行、`now`/`limit` 原样透传、读库错误点名 `playback_outbox` 并保留底层错误；三个 `Mark*` 的参数不被吞；真适配器 + 假 model 端到端 `RunOnce`（调用序列只有 `ListPending` 与 `MarkPublished`，发布器不得越权 `Insert`）；投递失败 ⇒ 写退避而**绝不**写已发布 |
| `params_test.go` | 6 | 1（12 例表驱动） | `SenderSettingsFrom` 去空白/去重/不改写配置底层切片；`OptionsFrom` 六个旋钮逐键映射且 `Name` 必须是 `playback/publisher`；`ValidatePublishKafka` 对 12 种不合规配置逐键点名（含「多写一个本服务不产出的 topic」被拒）；多条错误一次报全（分号分隔，不做改一个重启一次）；`NewPublisher` 四条出口（配置错/缺 model/缺 sender/齐备）与参数透传、构造不自行启动循环；`Kafka.MaxRetries` 超 int32 时映射绕负数，配置层放行而引擎拦住，且错误同时点名 `Options.MaxAttempts` 与 `Kafka.MaxRetries` |
| `example_yaml_test.go` | 3 | 0 | 用真实 `conf.Load` 读 `etc/playback.v1.yaml`：`Enabled=false` 必须保持（默认构建打开即炸）、其余键全部合格（「只差一次翻转」）、`PublishTopics` 恰好等于 `RequiredTopic()`、六个旋钮字面量与 yaml 注释同源；用示例配置装配 + 投递一次（`SendTimeoutSec=5` 真的落到每次投递的 ctx 上）；补 `Kafka` 段不得把 `Sign`/`DataSource`/`CacheRedis` 改坏 |
| `kafkaruntime_disabled_test.go`（`!playback_kafka`） | 3 | 0 | 默认构建即使参数全对也拿不到发送端，错误文本必须给可执行下一步（`playback_kafka`/`broker`/`Kafka.Enabled` 三个词）；启动日志点名积压表与 topic；`Enabled=true` 的示例配置走 svc 的入口形状必失败且不返回半截发送端 |
| `kafkaruntime_kafka_test.go`（`playback_kafka`） | 4 | 1（4 例表驱动） | 带 tag 构建能按配置建通道、重复 topic 只建一条、`Close` 幂等且清 topic 列表；四类不完整参数在**建通道之前**被拒（不留半截 producer）；未登记 topic 与空分区键在**触网之前**被拒（不假装投递成功）；`RuntimeNotes` 同时说明「已链接」不等于「在投递」且「从未联调」+ 下游还没有消费者 |

发布循环本体（顺序、退避曲线、int32 溢出、判死边界、启停生命周期）的用例在
[common/outbox](../../common/outbox/README.md)：默认构建 20 顶层 / 动态 50 例，本包不重复测引擎。
`kafkaruntime_kafka_test.go` 能离线跑是因为 `kq.NewPusher` 只 new 一个懒解析地址的
`kafka.Writer`；用例因此**绝不**向已登记 topic 调 `Send`，那会真去拨 broker。

### 3. 其他层

- `internal/signurl`（1 个文件 `11/2`）：CDN A 型防盗链的纯计算面——协议哈希等价、签名确定性、
  签名/校验往返、格式与过期两类拒绝、**私钥缺失必须报错**、非 dev/test 禁止关闭签名、
  TTL 默认值与显式值、`normalizeURI` 拒绝路径穿越与绝对 URL、`BuildURL` 拼接、随机串生成。
- `internal/repository`（2 个文件 `6/2`）：`rights_client_test.go` 钉住 playback↔rights 内容类型映射
  （两边编号刻意不同，禁止 `int32` 直转，否则 PGC 被当 UGC 跳过版权校验）、rights 未配置时
  **永不返回「可播放」**、非法 content_type 在发 RPC 之前被拒；`cache_test.go` 钉住会话缓存 TTL
  必须落在 `[min,max]` 且永不为 0（TTL=0 会让 Redis `SETEX` 直接报错）与 key 前缀隔离。
- `internal/config`（1 个文件 `1/1`）：`etc/*.yaml` 可被 go-zero 加载并反射递归检查必填字段。
- `model/`（`playbackmodel.go`、`progressmodel.go`、`outboxmodel.go`、`errors.go`、`now.go`）
  **无离线单测**：三张表的 SQL 文本、列名、占位符与索引无门禁。
- `internal/svc` **无离线单测**（只被 `publisher` 的示例配置用例间接覆盖装配参数）；
  `internal/server/playbackserver.go`、`rpc/*.pb.go` 是 goctl 生成壳。
- 本服务没有 `internal/consumer`、`internal/policy` 层：`playback_outbox` 的发布器本轮已接线
  （见「事件发布」节），但**没有**任何服务消费 `playback.heartbeat.v1`（缺口 1），
  `RevokeSessionsByContent` 的消费者也未接入（缺口 2），所以「事件真的送达 broker」
  「版权撤回立刻踢掉会话」既没有 broker 侧证据也没有用例。

### 4. 构造器级覆盖：**4/4**

探针取 `internal/logic` 全部 `New*Logic(`，共 4 个（`GetPlaybackToken`、`VerifyPlaybackToken`、
`ReportHeartbeat`、`GetSession`，即「RPC 方法」表里的四条），`gaps:` 为空——每个方法都有直接驱动
自身构造器的用例。

### 5. 注入缝与替身口径

`Repository` 的缓存依赖是具体类型 `*Cache`，因此 `internal/repository` 暴露
`Cacher` 接口 + `NewWithDeps(cache, conn, sessionMd, progressMd, outboxMd)`；生产路径仍只走
`New(rds, conn)`。logic 用例据此组装**真实 Repository**，只把 6 个依赖
（缓存、三张表的 model、`sqlx.SqlConn`、`RightsChecker`）换成内存替身，
所以缓存回源、状态推进、`TransactCtx` 事务这些判定链都在被测路径上，而不是把 Repository 也 mock 掉。
`conn` 不能省：`ReportHeartbeat` 的「进度 + Outbox」必须走同一个事务。

`internal/logic/fakes_test.go` 记了四条替身纪律（值拷贝、按真实 SQL 口径分配主键、
按顺序记录调用轨迹、按方法粒度注入错误），其中第四条最容易踩坑：**布数据必须走静默写入路径**
（`put`/`warm`），否则序列断言会把布景调用也算进去。`session_id` 是 ULID（不可注入），
因此它只能靠 `wantCount` + 读回断言，不能出现在精确调用序列里。

复刻的 SQL 语义：`playback_session` 的 `UNIQUE(request_id)` 冲突、`playback_progress` 的
`GREATEST` 只前进、`playback_outbox` 的 `UNIQUE(event_id)`、会话状态门槛、
`ORDER BY mtime DESC LIMIT 1` 的最近进度取法。

`internal/publisher` 的替身口径不同，有两处是刻意为之：
`fakeOutboxModel` 内嵌 `model.PlaybackOutboxModel` 接口而不实现 `Insert`，
所以「发布器偷着写事件」会直接 panic；`fakeSender` 带互斥锁，因为启停用例会读到它的调用痕迹。
时钟**没有注入**：`common/outbox` 的 `now` 是包内私有字段，本包的用例只用真实时钟的
前后区间（`before <= publishedAt <= after`）做界限，退避曲线的精确值由引擎自己的用例负责。
代价是这里不能断言 `next_retry_at` 的精确字面量，只能断言「在未来」与「retry_count=1」。

### 6. 覆盖边界（不可省略）

- 用例不连接 MySQL、Redis、Kafka/etcd、对象存储，也不依赖网络；签名用的私钥来自测试布景。
- 替身只复刻 model 层 SQL 的**语义**，不证明 SQL 本身；`model/*.go` 的列名与占位符仍无单测。
- 替身**不做回滚**，所以「Outbox 写失败 ⇒ 进度一起消失」这条只能断言事务以失败收场
  （`rolledBack==1`），真实原子性由 MySQL 保证。
- 媒体字节不进 MySQL、也不进用例：本服务只签名与记账，分片上传、真实对象存储、CDN 回源
  全都不在被测路径上。
- **预签名/签名链路只有离线判定被验证**：`signurl` 的哈希、往返、格式与过期拒绝、私钥缺失必须报错
  都是纯计算结论；真实 CDN 是否按同一套 A 型协议校验、`auth_key` 能否在真实边缘节点通过、
  签名地址被真实播放消费——一概不在覆盖内。CDN 侧的联调只能由集成环境验证。
- **回调链路同理**：`ReportHeartbeat` 的「进度 + Outbox 同事务」与 `event_id` 幂等键被钉住，
  Outbox 发布循环的映射、退避落库、判死也各有用例，但本仓库从未与任何 broker 联调，
  且 `playback.heartbeat.v1` 在仓库内**没有消费者**（缺口 1），所以到 `event-collector`/`spm`
  这一段仍没有任何自动化验证；`RevokeSessionsByContent` 无调用方（缺口 2），
  「版权撤回后会话最多再活一个 `Sign.TokenTTL`」是被登记的现状，不是被测结论。
- 迁移 SQL ↔ 真实库的列级对账：本 README 的「数据表」节只登记 DDL 位置与回滚脚本，
  没有声明在隔离实例 `127.0.0.1:3399` 做过列级复验，因此按**未在目标实例复验**处理；
  迁移登记以 `deploy/migrations/README.md` 为权威，本包没有 model↔DDL 的自动对账门禁。
- `internal/server/playbackserver.go`、`rpc/*.pb.go`、handler 等 goctl 生成壳不在单测范围内。

2026-09-22 变异探针（改坏生产规则看用例是否真的红）两组均失败被抓：删掉 `VerifyPlaybackToken`
的 URI 绑定判定 ⇒ `uri_mismatch` 拒绝用例红；绕过 `CountPlayOnce` 的 SETNX 去重 ⇒
「一个会话只计一次」用例红。探针后已还原并复跑为绿。

### 7. 验证命令

```bash
# 默认构建（不链接队列发送端）
go test -p 1 -count=1 -timeout 120s ./services/playback/...
gofmt -l services/playback
go vet ./services/playback/...

# 带队列发送端的构建：两个标签都必须绿，否则接线等于没测
go test -p 1 -count=1 -timeout 120s -tags playback_kafka ./services/playback/...
go vet -tags playback_kafka ./services/playback/...
```

- `-p 1` **必须保留**：Windows 页面文件限制下并发链接多个测试包会 OOM（`errno=1455`）；
  `-count=1` 关闭测试缓存。
- `go vet` 期望无输出、`gofmt -l` 期望为空列表。门禁统一串行执行，本节只登记命令与口径，
  不在文档里声明执行结论。

## 已知缺口

1. **`playback.heartbeat.v1` 只有生产者，没有消费者，也没有 broker 证据**：发布循环本轮已接线
   （`internal/publisher` + `common/outbox`，默认构建仍拒绝创建发送端），但
   - 本仓库从未连接过任何 Kafka/Redpanda，`WithSyncPush` 的错误回传、分区键顺序、
     topic 不存在时的行为全部只在离线替身上验证过；
   - `docs/api-and-events.md §5` 里该事件的下游是 `event-collector`/`spm`，而两者都**没有**
     `internal/consumer` 目录（各自 `internal/` 下只有 config/logic/server/svc），
     所以打开 `Kafka.Enabled` 只会让 `playback_outbox.state` 推进，
     不会让完播率与热度特征前进；
   - `deploy` 侧没有该 topic 的创建脚本（刻意不自动建 topic），启用前要先建 topic。
2. **版权撤回/下架的消费者未接入**：`Repository.RevokeSessionsByContent` 尚无调用方。
   在它被 `rights`/`catalog` 事件驱动之前，被撤回内容的会话最多再存活一个
   `Sign.TokenTTL`（默认 30 分钟），属于可接受窗口而非漏洞。
3. **死信没有人工放行入口**：`state=2` 的行只能靠 DBA 手工 `UPDATE`（本服务没有
   `RetryFailedEvents` 类 RPC，`model.PlaybackOutboxModel` 也没有 `ResetFailed`），
   而 `CheckRow` 判死的行（topic 不归属本服务、列与 payload 不同源）同样落在这个状态里，
   两者在库里不可区分，只能靠 `last_error` 前缀 `unpublishable:` 分辨。
4. **没有积压口径**：`model` 没有 `CountPending`/滞后统计方法，运维只能自己
   `SELECT COUNT(*) FROM playback_outbox WHERE state=0`。心跳是高频写表，
   `Kafka.Enabled=false` 时积压行数会一直涨而无人被发现（启动日志会说明这一点，但没有指标）。
5. **多副本会重复投递**：`playback_outbox` 没有租约/占位列，两个实例同时跑会各投一遍。
   消费侧按 `event_id` 去重所以不破坏正确性，但会白烧一倍算力与日志。
6. **清晰度/码率协商不在本服务**：`object_key` 由调用方从 `asset`/`transcode` 解析后传入，
   本服务只签名。按用户等级降码率属于商业化范围（AGENTS.md §1），本期不做。
7. **容量治理**：`playback_session`/`playback_progress` 是写多表，按 `ctime` 的归档
   或分区（保留 30~90 天）尚未实现。
8. **断点续播读接口**：`GetSession` 已能返回进度，但面向客户端的
   "按 mid+content 列出全部断点" 查询未提供，接入 `gateway/app` 时再评估。
9. **播放量口径**：本服务的 Redis 计数只是近似热计数，正式播放量以
   `event-collector`/`spm` 聚合 `playback.heartbeat.v1` 的结果为准。
10. **`auth_key` 的 `uid` 段未与会话 `mid` 绑定**：`signurl.Verify` 用**请求携带的** token 里的
   `uid` 重算哈希，logic 也不比对返回值与会话 mid，因此改 `uid` 不会导致校验失败
   （用例 `TestVerifyPlaybackTokenUidSegmentIsLogOnly` 把这个事实固定下来，防止后来者误当漏洞修掉）。
   伪造仍然需要私钥，所以不是越权；但若哪天要用 `uid` 做审计归属，加固点就在这里。
11. **`GetPlaybackToken` 只挡 `state=revoked`**：一行 `state=expired` 但 `expire_at` 仍在未来的会话
   重放时会被当作有效继续返回。真实状态机到不了这个组合（`MarkExpired` 只在 `expire_at<=now` 时推进），
   所以是**潜在**缺口而非现网缺陷；本服务没有为它写用例，因为那等于把该行为固化成契约。
   若要收严，应在签发分支同时要求 `state=active` 且 `expire_at>now`。
12. **带鉴权的集群不可用**：go-queue v1.2.2 的 `kq.NewPusher` 不暴露 dialer 注入口，
   因此 `Kafka` 段没有 `Username/Password/CaFile`，SASL/TLS 集群需要换实现或升级依赖。

