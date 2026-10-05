# notification

外部通知投递服务。

- **拥有数据**：Push/短信/邮件模板、投递记录、供应商响应、重试和死信。
- **提供能力**：模板渲染、供应商路由、频控、退避重试、投递回执。
- **依赖**：MQ、Push/短信/邮件供应商、`inbox`、`operation`。
- **约束**：验证码和通知内容脱敏；供应商失败不能阻塞领域事务；禁止营销广告投递。

## RPC 方法（按域分组，共 11 个）

- **模板**：`UpsertTemplate`（建/改草稿）、`PublishTemplate`（草稿转可投递并把上一版下线）、
  `ListTemplates`、`RenderTemplate`（按版本渲染，支持预览草稿）。
- **投递**：`SendNotification`（入队/同步投递两态）、`GetDeliveryStatus`、`ListDeliveries`。
- **死信**：`ListDeadLetters`、`RetryDeadLetter`（按事件信封重放，不重造载荷）。
- **免打扰与静默时段**：`UpdateDndPreference`（整行覆盖式写入）、`GetDndPreference`（未设置时回默认）。

## 运行与自检

```bash
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp
go build ./services/notification/...
go vet ./services/notification/...
go test -p 1 -count=1 ./services/notification/...
```

## 测试覆盖

离线单测（纯 Go，全部不连库、不连中间件、不发网络请求）。数字由导出文件给出，
格式 `顶层/子用例`（`top` 已排除 `TestMain`）。

### 1. `internal/logic` 用例清单（11 个用例文件 + `fakes_test.go` 替身层）— `79/0`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `sendnotificationlogic_test.go` | 15/0 | 把 Enqueuer 的显式拒绝原样上抛（不降级成「看起来受理了」）；`SyncSend` 只对本次新建且待投递的任务投一次，投递失败不改写已受理响应 |
| `retrydeadletterlogic_test.go` | 10/0 | 「先重投、后标记 retried」的顺序；复位 CAS 未命中时报 `ErrIllegalStateTransition` 且不消费死信；复位后行真的可投递；信封缺失显式 `ErrEventPayloadMissing` |
| `dndpreferencelogic_test.go` | 9/0 | 写侧落库后读回（不回显请求）、全量覆盖 + `ON DUPLICATE KEY` 不动 `ctime`；无记录时回默认值 + `found=false`；通道位掩码与投递侧 `IsChannelMuted` 同口径 |
| `upserttemplatelogic_test.go` | 8/0 | 入库前执行与投递时同一套 `policy.CheckTemplate`；版本推进/草稿覆盖/唯一键冲突可判定，校验不过一行不落 |
| `listdeadletterslogic_test.go` | 6/0 | `total` 与页码无关、`ctime DESC, id DESC`；过滤条件全 AND 且 0 值＝不过滤；只投影 10 个契约字段（`source`/`delivery_id` 无载体，`TestListDeadLettersCannotAskForSource` 钉住） |
| `publishtemplatelogic_test.go` | 6/0 | 草稿→已发布是投递可用性开关；单已发布不变量；发布已发布/已下线版本被拒 |
| `rendertemplatelogic_test.go` | 6/0 | 预览不落库、渲染口径与投递一致、缺变量给清单而不是补空串 |
| `getdeliverystatuslogic_test.go` | 5/0 | 读回的是库里的状态而非请求期望；不存在＝`found=false` 且无错误；供应商回执列原样带出；`params_json`/`biz_group_key`/`source_event_id`/`lang` 不外泄 |
| `listdeliverieslogic_test.go` | 5/0 | `total`、`ctime` 倒序、AND 过滤；调用方 `biz_key` 与库里落的 `biz_key` 不是同一个值 |
| `sendnotification_test.go` | 5/0 | 同方法的另一组断言：模板可用性与语言回落 |
| `listtemplateslogic_test.go` | 4/0 | 分页越界返回空页 + 真实总数；不支持的语言显式拒绝，不把查询失败写成没有数据 |
| `fakes_test.go` | 0/0 | 替身装配层（见 §4），不贡献用例数 |

### 2. 其他层

| 层 | 文件 | 顶层/子 | 内容 |
|---|---|---|---|
| `internal/send` | 1 | 18/0 | 入队口径：`biz_key` 幂等、行级去重、同 mid 多设备、语言回落、缺变量/注入/请求校验入口即拒、静音通道抑制、静默时段不被优先级绕过、配额抑制与配额不可用 fail-closed、偏好读失败 fail-closed、锁模板版本、`New` 拒坏配置 |
| `internal/policy` | 4 | 30/0 | `backoff_test.go`(8) 退避阶梯与幂等键；`render_test.go`(10) 模板渲染与注入防护；`dnd_test.go`(7) 跨天窗口与时区边界；`deliverystate_test.go`(5) 投递/事件状态机非法迁移拒、终态不复活 |
| `internal/consumer` | 1 | 15/2 | 派发器：未配置供应商绝不外发、退避阶梯耗尽转死信、受理才标 `sent` 并留摘要、未受理/空回执不发、静默时段持有而不烧重试次数、静音通道不发、偏好不可用 fail-closed、过期抑制、`ErrContactNotWired` 走重试不走死信、模板永久问题转死信、终态行跳过、认领窗口防重复发送、按到期时间与优先级排、坏时区拒构造 |
| `internal/config` | 1 | 1/1 | `etc` 示例配置可被 go-zero 加载（子用例逐个文件） |
| `model/` | 0 | — | **无离线单测**（已知缺口 2） |
| `internal/repository/` | 0 | — | **无离线单测**（已知缺口 2） |
| `internal/provider/` | 0 | — | **无离线单测**：HTTP 适配器的渲染与签名路径完全没有用例（已知缺口 2） |
| `internal/svc/` | 0 | — | **无离线单测** |

规模合计 **143 个顶层用例 + 3 个子用例**（logic 79/0、send 18/0、policy 30/0、consumer 15/2、config 1/1）。

### 3. 构造器级覆盖：`11/11`

探针取 `internal/logic` 全部 `NewXxxLogic` 构造器（11 个，与 11 个 RPC 方法一一对应），
逐个在 `*_test.go` 里查引用，`gaps:` 为空——没有「只能被间接断言」的方法。

### 4. 替身层与断言口径

替身在 `internal/testx/fakes.go`（logic/consumer/send 三包共用）与 `internal/logic/fakes_test.go`（装配）：

- 假件复刻 model 层真实语义而不是空实现：唯一键冲突、源状态守卫、退避扫描排序；
  配额假件带 `Fail` 开关，用来验证 Redis 不可用时的 fail-closed。
- 注入缝全部复用既有生产缝（`repository.NewWithModels`、`send.New`、`consumer.NewDispatcher`/
  `NewEventHandler`、`ServiceContext` 结构体字面量），**不新增生产注入点**；
  因此 `SendNotification` 走的是真实 Enqueuer 判定链，不是替身。
- 生产 `NewServiceContext` 会连 MySQL/Redis/etcd 并起后台协程，单测禁止调用。
- 断言口径：**不伪造成功**——无供应商/无派发器时只能留在 pending/accepted 事实态
  （`TestSendNotificationSyncSendWithoutProviderDoesNotFakeSuccess`、
  `TestSendNotificationSyncSendWithoutDispatcherLeavesTaskPending`）；
  **失败关闭**——配额或偏好读不到就整批拒绝，而不是「拿不到就放行」
  （`TestSendNotificationQuotaUnavailableFailsClosed`）；
  **隐私出口**——列表只投影契约字段，收件联系方式与正文以掩码形态出去；
  **顺序**——CAS 未命中不得消费事件；入参守卫返回各自哨兵错误并原样上抛，不退化成统一「不可用」。

### 5. 覆盖边界（不可省略）

- 用例不连接 MySQL、Redis、Kafka、搜索引擎与任何供应商 HTTP 端点。
- **本 README 没有隔离实例复验声明**：`deploy/migrations/notification` 的 SQL 与真实库的列级对账
  **未在目标实例复验**，也没有 `model`↔DDL 的静态一致性用例（已知缺口 2），所以「列宽/唯一键/索引
  与 model 一致」目前不是被测事实。
- Kafka 运行时默认走 `internal/consumer/kafkaruntime_disabled.go` 的占位实现，
  `-tags` 下的真实路径只有编译级证据（已知缺口 3）；供应商侧无实账号联调（已知缺口 4）。
- `internal/server`、handler 等 goctl 生成壳不在纯单测可达范围，本包只测到 logic 出入口。
- **1 条用例处于 skip 条件**（导出计数按 `t.Skip(` 口径为 1，本服务是全仓 3 个有 skip 的服务之一）：
  `internal/policy/backoff_test.go:93` 的 `t.Skip("无 tzdata")`——本机缺时区库时，
  「退避按绝对时间推进、与本机时区无关」这条断言不执行。同一原因的
  `internal/policy/dnd_test.go:17`（`t.Skipf` 助手，按 `t.Skip(` 口径不计入该 1 条）会让全部
  免打扰跨时区用例跳过。这两条都是环境相关的条件跳过，不是被禁用的缺陷用例，
  但在无 tzdata 的机器上它们**不提供任何证明**。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/notification/...   # -p 1 必须带：Windows 页面文件限制，并发跑多个测试包会 OOM(errno=1455)
gofmt -l services/notification                      # 必须为空
go vet ./services/notification/...                  # 应无输出
```

`go build ./services/notification/...` 与 `go vet` 见「运行与自检」；本节只声明覆盖范围与口径，
不含任何一次运行的结论。

## 已知缺口

1. **收件联系方式没有接线（结构性，需上游契约变更）**：`internal/provider/provider.go:53` 的
   `ErrContactNotWired` 是显式能力缺失——`account.v1` 契约里没有「按 `mid` 取手机号/邮箱」的方法，
   所以短信/邮件通道只能由调用方把联系方式传进来，本服务不能自行补全。
   要收口必须改 `api/` 下的 account 契约并同步 `services/account`，不在本期能力内。
2. **`model/`、`internal/repository/`、`internal/provider/` 零测试**：列宽/唯一键/索引与 model SQL 的
   一致性没有 `migration_parity_test.go` 这类门禁（`services/live-room`、`services/risk-control` 都有），
   HTTP 适配器（`internal/provider/http.go`）的模板渲染与签名路径也完全没有用例。
   补 model 层需要 sqlmock 或隔离实例，二者都不在本轮范围内。
3. **Kafka 运行时尚未实机联调**：默认构建走 `internal/consumer/kafkaruntime_disabled.go:3` 的占位实现，
   `-tags` 开关下的真实 Kafka 路径在 2026-10-04 实测 `go build`/`go vet`/`go test`（均带
   `-tags notification_kafka`）全部 rc=0，但依赖侧不再是需要授权的阻塞（`go-queue` 已在 `go.mod` 直接
   require）；缺的是 broker 上的投递/死信/重试实测。
4. **无真实供应商凭据**：`internal/provider` 只实现了通用 HTTP 适配器，Push/短信/邮件三家
   没有实账号联调，因此「供应商回执驱动投递状态推进」只有假件级证据。
5. **死信无自动重投 job**：`RetryDeadLetter` 是人工/运营触发的单条重放，按 `next_retry_at` 批量捞取的
   清扫任务归属 `services/cron`，尚未登记任务。
