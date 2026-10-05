# common/outbox

事件 Outbox 的通用发布循环：把「已在业务事务内提交」的 outbox 行按序同步投递到消息队列，
并把每条的结论（已发布 / 退避重试 / 判死）写回该行。对齐
[docs/api-and-events.md §4/§5](../../docs/api-and-events.md) 的事件契约与 AGENTS.md §5 的 Outbox 约束。

## 职责

- **发布循环**：`Start`/`Stop`/`RunOnce`，按 `Store.ListPending` 给定的顺序逐条同步投递，
  每轮之间间隔 `Options.Interval`，启动即先扫一轮（进程重启后的积压不用等一个间隔）。
- **退避与判死**：失败按 `BaseBackoff * 2^(n-1)` 退避、上限 `MaxBackoff`；
  累计尝试达到 `MaxAttempts` 即判死；`Row.Defect` 非空的行不投递、不占重试次数，直接判死。
- **可观察性**：`Stats()` 给出 published/retried/failed 与最近一轮扫描错误，`Running()` 给出循环状态。
- **行自洽反查**：`CheckRow` 供各服务适配层复用，拦下「列与 payload 不同源」这类致命错位。

## 明确不做的事

- 不含队列客户端。真实发送端只存在于各服务的 `*_kafka` 构建标签文件里，默认构建必须显式失败
  （口径与理由见 [docs/development.md §7](../../docs/development.md)）。
- 不含 SQL、表名、事件类型名。列映射、topic 派生、状态常量由各服务的 Store 适配承担。
- 不做跨实例抢占：表里没有租约列，多副本会各投各的（at-least-once，消费方按 `event_id` 去重）。
- 不做死信路由：判死只是把该行状态置为失败，重投工具不在本包范围内。

## 依赖

- `github.com/zeromicro/go-zero/core/logx`：结构化日志（`logx.Infow/Errorw` 带 outbox_id/event_id/topic/attempt）。
- `github.com/zeromicro/go-zero/core/threading`：`GoSafe` 起后台循环。
- `go-video/common/eventenvelope`：`CheckRow` 用它反序列化并校验信封。

## API

| 符号 | 锚点 | 说明 |
|---|---|---|
| `type Row struct` | `outbox.go:44` | 发布决策需要的最小行视图（ID/EventID/Topic/Key/Payload/RetryCount/Defect） |
| `type Store interface` | `outbox.go:74` | 四方法持久化契约：`ListPending`/`MarkPublished`/`MarkRetry`/`MarkFailed` |
| `type Sender interface` | `outbox.go:83` | `Send` 返回 nil 必须代表队列侧已受理；`Close` 释放连接 |
| `type Options struct` | `outbox.go:92` | Name/Interval/Batch/MaxAttempts/BaseBackoff/MaxBackoff/SendTimeout，零值不放行 |
| `func New(store, sender, opts) (*Publisher, error)` | `outbox.go:171` | 依赖缺失或参数非法直接失败 |
| `func (*Publisher) Start() error` | `outbox.go:204` | 重复 `Start` 报错（两套循环会把同批事件各投一遍） |
| `func (*Publisher) Stop()` | `outbox.go:232` | 等当前批次收尾后关连接；未启动时是 no-op，幂等 |
| `func (*Publisher) RunOnce(ctx) (int, error)` | `outbox.go:278` | 单轮扫描；读库或状态写库失败会中断本批并冒泡 |
| `func (*Publisher) Stats()` | `outbox.go:189` | published/retried/failed + 最近一轮扫描错误 |
| `func CheckRow(row *Row, allowedTopics []string) string` | `checkrow.go:24` | 返回该行的不可发布原因，空串表示可投递 |

## 决策表

| 输入 | 动作 | 落库 |
|---|---|---|
| 正常行 + `Send` 返回 nil | 计 published | `MarkPublished(id, now)` |
| 正常行 + `Send` 返回错误，且 `retry_count+1 < MaxAttempts` | 计 retried | `MarkRetry(id, retry_count+1, now+退避, 错误原文)`，行留在待发布 |
| 正常行 + `Send` 返回错误，且 `retry_count+1 >= MaxAttempts` | 计 failed | `MarkFailed(id, "retries exhausted after N attempts: ...")` |
| `Row.Defect` 非空 | **不投递**、不占重试次数 | `MarkFailed(id, "unpublishable: " + Defect)` |
| `ListPending` 失败 | 中断本轮 | 不写任何行；错误冒泡给调用方，循环侧记进 `Stats` |
| 任一 `Mark*` 失败 | **中断本批**（后续行不再处理） | 冒泡错误：绝不把「行还停在待发布」读成「这行有结论了」 |

## 与各服务适配层的分工

各服务只提供三样东西，其余都复用本包：

1. `Store` 适配：把本服务 outbox 表映射成 `Row`，用 `event_type + schema_version` 派生 topic，
   调 `CheckRow` 填 `Defect`，并把四个状态方法转调本服务的 model。
2. `Sender`：放在 `<service>_kafka` 构建标签文件里（默认构建返回 `ErrKafkaRuntimeNotBuilt`）。
3. 配置：`Kafka.*` 键到 `Options` 的映射与逐键校验。

**当前使用方是 `services/playback`、`services/live-media`、`services/recommend-recall`、`services/upload`
与 `services/video`**（前两个 2026-10-04 接线，后三个 2026-10-05，各自的 `internal/publisher` +
`-tags playback_kafka` / `-tags livemedia_kafka` / `-tags recommendrecall_kafka` / `-tags upload_kafka` /
`-tags video_kafka`）：playback、recommend-recall、upload 与 video 各投递单个 topic
（`playback.heartbeat.v1`、`recall.pool.published.v1`、`media.task.v1`、`content.published.v1`），
live-media 投递 9 个 `livemedia.*.v1`，
所以「多 topic 集合相等校验」这条只有 live-media 侧有对应用例；另四者是「必须恰好等于 model 常量派生的
那一个 topic」，写多一个键名也拒启动。upload 与 video 各有一条与前三者都不同的形状：**事件行写在业务状态推进的
同一个事务回调内**（`services/upload/internal/repository/repository.go:288`~`:300`、
`services/video/internal/repository/repository.go:184`），
因此「表不存在」会让业务写一起失败，而不是「写成了、事件静默丢失」。
video 还多一层本包不管的业务判定：只有改变对外可见性的转换才产事件行（`contentActionFor`），
所以「事务成功了但一行事件都没写」在这条链上是正常结论，适配层为此准备的是
「组装失败就返回错误、由调用方回滚整笔」而不是「跳过」（见 `services/video/README.md`）。
「条件 UPDATE 命中 0 行折叠成日志而不是错误」在 live-media 与 recommend-recall 两侧都有用例
（后者的 `TestConditionalMissIsLoggedNotFatal` 与 `TestRunOnceDistinguishesMissFromWriteError`
把「0 行」和「写库失败」分开钉，前者 handled 计数照常、后者必须冒泡并停在失败那一行）。
`services/live-ingest` 仍是自己的副本，见缺口 1。

## 测试覆盖

命令口径（本机实测 2026-10-04，Windows 页面文件限制要求 `-p 1`）：

```bash
export GOCACHE=$PWD/.gotmp/gocache GOTMPDIR=$PWD/.gotmp/gotmp PATH=$PATH:/d/hilihili/software/go/bin
go vet -mod=readonly ./common/outbox/...
go test -mod=readonly -p 1 -count=1 -timeout 120s ./common/outbox/...
go test -mod=readonly -p 1 -count=1 -v -timeout 120s ./common/outbox/...   # 看 SKIP/FAIL 行
```

- **用例数**：静态 20 个顶层 `func Test`（`outbox_test.go` 16 + `checkrow_test.go` 4）、
  静态 5 处 `t.Run`；动态 `-v` 实测 `--- PASS` 50 行（20 顶层 + 30 子测试），
  `--- SKIP` 0、`--- FAIL` 0。表头用静态口径，动态数在这里披露：退避曲线、判死边界、
  `CheckRow` 缺陷表都是表驱动，子测试数多于静态 `t.Run` 计数。
- **发布路径**（`outbox_test.go:212/253/272`）：钉住按 store 顺序原样投递（发布器不得重排）、
  只有 `Send` 返回 nil 才 `MarkPublished`、每条投递的 ctx 必带 `SendTimeout` deadline。
- **退避与判死**（`outbox_test.go:291/328/368/394/440`）：首次失败落 `retry_count=1` 与 `now+基数`；
  曲线表覆盖 1/2/3/10 次与夹住边界，含「移位 64 不得按 mod 64 回绕」与 int64 大数；
  `MaxAttempts-1` 与 `-2` 的边界不 off-by-one；`retry_count` 取 int32 上限时必须判死而不是绕成负数。
- **判死与写库失败**（`outbox_test.go:488/519`）：缺陷行不投递、原因带 `unpublishable:` 前缀且不连累同批正常行；
  四类库操作（读、置已发布、记重试、判死）失败都冒泡并中断本批。
- **启停与并发**（`outbox_test.go:658/707/735`）：启动即扫、重复 `Start` 报错、`Stop` 幂等且只关一次连接、
  `Stop` 之后不再扫描、后台循环把扫描错误记进 `Stats`、批次在飞时 `Stop` 必须等收尾。
- **参数与依赖**（`outbox_test.go:589/621`）：nil 依赖直接失败、六个非法参数逐一在错误里点名对应 yaml 键，
  多个键同时不合格时全部列出。
- **`CheckRow`**（`checkrow_test.go:43/56/105/116`）：一条自洽行放行；11 类缺陷逐条给出结论
  （topic 归属、分区键空、event_id 空、payload 非法 JSON/空串/缺 schema_version、
  信封与列的 event_id/aggregate_id 不同源）；缺陷原因里列出本服务声明的 topic；判据只读不写行内容。
- **替身与断言口径**：`Store`/`Sender` 都是本文件内的替身，不连 MySQL/Redis/etcd/MQ/对象存储，
  不起 gRPC 服务端；时钟用固定 `fakeNow`（`outbox_test.go:19`）注入，因此退避与时间戳断言是字面量比对；
  替身带互斥锁，启停用例在后台循环运行时读调用痕迹。断言一律走「落库调用序列 + 行状态」两侧，
  没有恒真断言，也没有为通过而放宽的容差。

## 已知缺口

1. **live-ingest 仍在用自己的副本**：`services/live-ingest/internal/publisher/publisher.go`
   是本包决策表的来源，本轮没有把它迁到 `common/outbox`（该包默认构建下 28 个顶层用例、
   `-tags liveingest_kafka` 下 32 个，都已验证过，改动应单独一轮）。
   两份实现现在有三处**已知差异**，收口时必须逐条对齐而不是直接替换：
   - 尝试计数：本包用 int64 累加并夹住负数（`outbox.go:333`），live-ingest 版是
     `rec.RetryCount + 1` 的 int32 加法，`retry_count` 取 int32 上限时会绕成负数而永不判死。
   - 日志字段 `attempt`：本包同样按 int64 打（`outbox.go:306`），live-ingest 版会打出负数。
   - `CheckRow` 多一条 `event_id` 列为空的判据（`checkrow.go:50`），live-ingest 的 `toRecord` 没有。
2. **本包从未在 broker 上验证过**：全仓库没有联调过 Kafka/Redpanda。这里能给的结论只到
   「可编译 / `go vet` 干净 / 20 个用例全绿」。「事件真的送达」「同步 `Send` 的失败语义确实如假设」
   必须等 Redpanda 上跑通才算数，口径见 [docs/roadmap.md](../../docs/roadmap.md) 的 MQ 接线节。
3. **多副本重复投递**：没有租约列，因此不做跨实例抢占。at-least-once 的正确性靠消费方按
   `event_id` 去重，代价是同批事件被投多份以及日志/算力浪费；各服务的现状写在服务 README。
4. **判死之后没有工具**：`MarkFailed` 只置状态，本包不提供死信表或重投接口。
   live-ingest 有 `RetryFailedEvents` RPC，其余服务多数没有对应运营入口。
