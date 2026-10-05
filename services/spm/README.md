# spm

用户行为分析和视频推荐特征服务。SPM 在本项目不是广告位参数，也不承担广告投放或商业化分析。

- **拥有数据**：播放/点击/搜索/跳过/点赞/收藏/关注行为指标、内容热度、用户兴趣和推荐特征。
- **提供能力**：实时窗口、离线回填、完播率、留存、兴趣画像、内容统计和特征输出。
- **依赖**：`event-collector`、`video`/`catalog` 发布事件、分析存储、`feature-store`。
- **约束**：事件可重放、字段脱敏、指标口径版本化；推荐特征异常时提供旧版本或默认值。

## 消费链路

事件入口只有 MQ（`internal/consumer`，尚未接线）；gRPC 的 16 个方法只做四件事：
读指标（GetMetric/BatchGetMetrics/ListHotSubjects/GetUserInterest/GetRetention）、
写回窗口指标（WriteMetricWindow，来源限计算链路）、口径与作业编排
（UpsertMetricDefinition/UpdateMetricDefinitionState/RecomputeMetrics/SubmitAggregationJob/
GetAggregationJob/ListAggregationJobs）、只读可观测（ListMetricDefinitions/ListConsumerState/
ListDeadLetters/GetMetricDefinition）。

这里**不存在**行为明细写入口，也不存在把排序/推荐结果写回 spm 的通道（`rpc/spm.proto` 头部边界）。

## 口径版本策略

- `(metric_key, metric_version)` 一旦登记，`formula`/`unit`/`supported_windows`/
  `source_event_types` 不可改写：`UpsertMetricDefinition` 命中已存在版本时逐项比对这四项，
  全等则 `reused=true` 回首次结果，任一不同返回 `ErrMetricVersionImmutable` 并点名差异项。
- 新登记一律 `DRAFT`（请求带 `state=ACTIVE/RETIRED` 直接拒绝），ACTIVE 只能由
  `UpdateMetricDefinitionState` 迁移：`DRAFT→ACTIVE`、`DRAFT→RETIRED`、`ACTIVE→RETIRED`。
  RETIRED 不复活、ACTIVE 不回退成 DRAFT。
- 状态迁移的 `reason` 落在 `description` 列（`MetricDefinitionModel.UpdateState` 覆写它）；
  `request_id` 列记的是**登记**请求的幂等键，迁移不覆写它，否则「谁登记的」这条线索会消失。
  因此迁移的幂等判定按「当前状态 == 目标状态 → `reused=true`」，而不是按 request_id 回读。
- 迁移走 CAS（`WHERE state = fromState`）。CAS 未命中时回读当前真值并报
  `ErrInvalidDefinitionState`，不静默成功。
- 读接口的 `metric_version=0` = 该 key 的 ACTIVE 版本，解析结果缓存于 CacheRedis
  （键 `govideo:spm:def:active:<metric_key>`）；登记与状态迁移都会显式失效该键。
  同 key 出现多个 ACTIVE 时 `resolveDefinition` 返回 `ErrMultipleActiveDefinition`。
- **写侧不接受 `metric_version=0`**（WriteMetricWindow 逐行、RecomputeMetrics 整体），
  否则历史窗口会被「当时的 ACTIVE」解释，事后无人能还原。
- 请求的 `window_type` 必须落在口径登记的 `supported_windows` 内，否则 `ErrInvalidWindow`。

## 窗口、水位与迟到

- `window_start` 一律由服务端按粒度规整后回显；`TOTAL` 恒为 0 且不建水位
  （`model.WindowWatermarkModel.Advance` 对它返回 `ErrInvalidWindow`）。
- 读接口 `window_start=0` = 「最近一个已闭合窗口」：先点查 `spm_window_watermark`，
  水位行不存在时才退回 `spm_metric_window` 的 `MAX(window_start)`（只统计左边界
  ≤ now - 窗口秒数的已闭合窗口）。水位落后 `Spm.WatermarkLagSeconds` 时照常返回真实
  `window_start`，同时在日志里记一条停摆证据。
- 无数据时读接口返回 `found=false` + 零值 point（回显查询身份，不伪造取值）；
  `BatchGetMetrics` 的 map 里缺数据的键不出现。
- `WriteMetricWindow` 是幂等覆盖（按 `uniq_metric` 六列），绝不累加；
  `allow_late_write=false` 时，早于「本批主体类型水位 - `LateToleranceSeconds`」的行计入
  `rejected`/`rejected_keys`（`"<subject_type>:<subject_id>:<metric_key>"`），
  迟到数据只能显式回填。指标行与它描述的水位在**同一事务**里提交
  （`Windows.WithSession` + `Watermarks.WithSession`），否则 `window_start=0` 会解析到空窗口。
- 重放判定走 `Windows.ListByNaturalKeys`（`write_request_id` 上无索引，按它查等于全表扫描）：
  命中行全部带同一 `write_request_id` 才判为重放，回放首次写入计数、不再覆盖。
- 水位只按**批内实际覆盖到的 `subject_type`** 推进，不推进 `subject_type=0` 的跨主体汇总位：
  「这批是否覆盖全部主体」只有聚合器知道，`WriteMetricWindowReq` 里没有这个信息。

## 作业与租约语义

- `spm_aggregation_job` 按 `uniq_request_id` 幂等。`InsertIfAbsent` 不回填主键，
  所以提交后统一按 `FindByRequestID` 回读，`job_id`/进度都以库里的行为准。
- 同一 `request_id` 复用到内容不同的作业（类型/主体/口径/粒度/区间任一不同）返回
  `ErrRequestIdConflict`：幂等键的语义是「同一份请求的重复投递」。
- `RecomputeMetrics` 派生 `JOB_TYPE_RECOMPUTE`（强制显式口径版本）；
  `SubmitAggregationJob` 拒绝 `JOB_TYPE_RECOMPUTE`（它的 `metric_version=0` 表示 ACTIVE，
  用它排队重算等价于悄悄改写历史）。REALTIME 作业只接受 5 分钟/小时两档；
  `OFFLINE_BACKFILL` 必须给 `metric_key` 与显式版本。
- 跨度上限 `Spm.MaxWindowsPerJob`，超限拒绝而不是拆半执行。本方法不落指标、不阻塞请求。
- 认领侧（`ClaimPending`/`RenewLease`/`UpdateProgress`/`MarkFinished`，租约
  `Spm.JobLeaseSeconds`，超过 `Spm.JobMaxRetry` 置 FAILED）由执行器使用，**本轮尚未接线**，
  见下节。领取令牌必须「每次领取」唯一，不能按进程复用。

## 数据保留策略

- `Spm.BehaviorRetentionDays` 决定 `spm_behavior_event` 的清理水位（`DeleteExpired` 分批，
  单批 `Spm.DeleteBatchSize`）；`Spm.ProjectionRetentionDays`（≥ 前者）决定投影表清理水位。
- `spm_consumer_offset` 的成功行由 `DeleteSettledBefore` 清理；`ListConsumerState` 的聚合
  下界取 `now - BehaviorRetentionDays*86400`（契约里没有 `since` 字段）。
  这意味着**清理任务长期停跑时，早于该线的历史堆积不会出现在汇总里**——
  它换来的是「汇总聚合有界、不做全表扫描」。
- 投影可比事实活得久：口径变更后要解释历史窗口，靠的就是旧投影 + 口径登记行。

## 事件词汇

- `source_event_types` 必须逐落在 `model.SupportedEventType` 白名单内，不接受自造事件名；
  CSV 在登记时去重排序（`"1,2"` 与 `"2, 1"` 判为同一份登记）。
- `unit` 只允许 `count/ratio/seconds/score`。
- 兴趣键只允许受控词表 `zone:<id>`/`tag:<id>`/`catalog:<id>`/`up:<mid>`
  （`model.ValidInterestKey`），读侧再校验一次，脏键不回带也不猜。
- 隐私边界（AGENTS.md §7）：本服务的响应里没有设备号、IP、搜索词原文与行为明细；
  死信的 `payload_preview` 是写入侧已脱敏的前缀，读侧不二次加工，也不提供取回原文的入口。

## 契约缺口

以下是本期实现无法从现有契约/表结构得到、需要下一轮改契约或改 schema 的点：

1. **兴趣与留存的 `metric_version=0` 无法解析**：`spm_user_interest` 与
   `spm_retention_cohort` 只有 `metric_version`、没有 `metric_key` 列，注册表里没有可查的
   ACTIVE 指针。`GetUserInterest`/`GetRetention` 对 `version=0` 返回
   `ErrMetricVersionRequired`。修法二选一：给这两张表加 `metric_key`，或在契约里固定一个
   画像/留存口径键。
2. **同 key 至多一个 ACTIVE 没有数据库约束**：`spm_metric_definition` 只有
   `idx_key_state`。激活前 `CountActive` 拒绝明显冲突，激活后再数一次、发现并发赢家就把
   本次迁移回滚并报 `ErrMultipleActiveDefinition`；仍可能出现需要人工退役的中间态。
   彻底解法是把 ACTIVE 变成生成列上的唯一索引。
3. **`write_request_id` 无索引**：重放判定只能按 `uniq_metric` 六列回读（已实现），
   无法回答「某次回填写了哪些行」。
4. **`ListConsumerStateReq` 没有 `since`**：见「数据保留策略」。
5. **PGC 内容主体映射未落地**：上游 PGC 事件给的是 `episode_id`，榜单主体是
   `aid`/`catalog_item_id`；`spm_content_projection` 没有 episode→item 的映射列，
   因此 PGC 维度暂不出榜。
6. **执行器未接线**：本服务没有进程内 REALTIME ticker，`services/cron` 也还没有认领
   `JOB_TYPE_OFFLINE_BACKFILL`/`RECOMPUTE` 的任务，死信重放同样是 cron 的待接线项。
   作业目前只到「可提交、可查询」，`windows_done` 不会自己前进。
7. **短缓存 TTL 没有独立配置项**：热榜页与 ACTIVE 指针的 TTL 取
   `Spm.LateToleranceSeconds`（语义 = 已闭合窗口还会被改写的时间上界）；
   配 0 即完全关闭缓存。缓存只加速，丢失不影响任何口径值。
8. **后台入口的留痕位缺失**（`gateway/admin` 的 `/admin/spm` 已接线，本轮未改契约）：
   `GetUserInterestReq` 没有 `operator` 位，画像读取在服务侧落不下「谁读的」，
   而这条恰好是后台唯一挂权限判定的读口；`UpsertMetricDefinitionReq` 与 `RecomputeMetricsReq`
   没有 `reason` 位（`UpdateMetricDefinitionStateReq` 有），「为什么要这个版本」
   「为什么要重算这段」只能留在网关访问日志里。修法：给这三个请求补 `operator`/`reason`，
   并在 `ListConsumerStateReq` 一类读取上沿用同一约定。

## 其它实现约定

- `ListMetricDefinitions` 的 `metric_key` 是**精确匹配**（走 `uniq_metric_version` 最左列），
  不是前缀 LIKE；排序为 `metric_key ASC, metric_version DESC`（model 既定 SQL）。
- `BatchGetMetrics` 多口径共用一个窗口区间，`window_start_from=0` 时区间右端取各口径
  「最近闭合窗口」的**最小值**：取最大值会让水位落后的口径整段落空，看起来像「没人看」。
- 分页：`pn` 从 1 开始，`ps` 超过 `Spm.MaxPageSize` 直接拒绝（不静默 clamp）；
  `offset` 越过 `total` 或越过深翻页保护线（1e6）时只回 `total`、不发查询 SQL。
- 限流：读侧 `Spm.ReadQps`、写侧 `Spm.WriteQps/WriteBurst`，无令牌返回
  `ErrRateLimited`（不是空数据）。
- 作业与死信列表按 `id DESC` 排序（等价于 ctime 倒序，且翻页游标稳定）；
  口径列表按 `(metric_key ASC, metric_version DESC)`，见上一条。

## 测试覆盖

离线单测（纯 Go 内存替身，不连 MySQL/Redis/etcd/MQ）。数字由 `grep -cE '^func Test'`
（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。

### 1. logic 用例清单（`internal/logic`，5 个文件 `39/126`）

按链路分四组：写回与水位、口径注册与状态机、作业、读路径。

| 文件 | 顶层 | 子 | 钉住了什么 |
|---|---|---|---|
| **写回与水位** | | | |
| `writemetricwindow_test.go` | 11 | 6 | README「窗口、水位与迟到」的全部承诺：同一自然键（`uniq_metric` 六列）重复写回**收敛到最后一次的值**而不是累加（投影表被加两遍就是「榜上的数只会变大」）；重放按 `ListByNaturalKeys` 判定、命中行齐带同一 `write_request_id` 才回放首次计数且不二次覆盖；迟到边界是严格的——早于「本批主体类型水位 − `LateToleranceSeconds`」才计入 `rejected`/`rejected_keys`，`allow_late_write=true` 才收；迟到写回**不得把水位往回拉**（`advanceApplied` 断末次未应用）；指标行与它描述的水位**同一事务**提交（`txRuns` 精确计数，含「只有 1 行受理就只开 1 个事务」）；水位只按批内实际覆盖到的 `subject_type` 推进、不推进 `0` 汇总位；`TOTAL` 窗口单独一档；整单拒绝（必填、批量超上限、非计算链路来源、缺幂等键、写侧限流）与逐行拒绝分开，且拒绝路径 `upsert=0 tx=0`；水位点查失败 fail-closed，不允许「先插行、后发现有水位读不到」 |
| **口径注册与状态机** | | | |
| `metricdefinition_test.go` | 12 | 28 | 新登记一律 `DRAFT` 且 spec 归一化；同 `(metric_key, metric_version)` 重复登记走 `reused` 而不改写任何列（换 `request_id`、改 `description` 都不算第二次写入）；四项不可变列任一改动报 `ErrMetricVersionImmutable`；不安全登记（缺幂等键/幂等键超长/直投 ACTIVE/`description` 超长等）逐项拒绝；并发插入保赢家（同规格按 `reused` 回带赢家行、异规格仍按不可变拒绝、「插成功却读不到」必须显式报错）；状态机矩阵（非法迁移**一次 CAS 都不发**，`updateCalls` 为空）；迁移必须齐带入参；`reason` 落 `description` 列；CAS 未命中回读真值、不静默成功；ACTIVE 单写者（已有 ACTIVE 拒绝再激活、RETIRED 不占名额、退役后可再激活、`CountActive` 失败不得放行、并发赢家在第二眼出现时本次迁移回滚且 CAS 轨迹是两条）；`GetMetricDefinition` 读任意已登记版本（多 ACTIVE 不得降级成「查不到」）；`ListMetricDefinitions` 过滤逐列下传、非法状态过滤拒绝而不是当不限、越界页只回 `total` 不发查询、深翻页保护与 `total` 无关（现状哨兵：超长过滤串按不限处理） |
| **作业** | | | |
| `job_test.go` | 8 | 32 | README「作业与租约语义」：`RecomputeMetrics` 必须显式口径版本、只派生 PENDING 作业、**不写任何指标行也不开事务**（`upsert=0 windows=0 tx=0`）；`uniq_request_id` 幂等——同键改区间/改主体算冲突、改留痕（operator/reason）不算冲突、同键改粒度算冲突；`SubmitAggregationJob` 的类型守卫（拒绝 `RECOMPUTE`、REALTIME 只接受 5 分钟/小时、回填必须给口径键与显式版本、给了版本不给键也拒、粒度未登记在 `supported_windows` 拒、主体形态矛盾拒、跨度超 `MaxWindowsPerJob` 整单拒而不是拆半、写侧限流）；ACTIVE 版本在提交时解析并钉进作业行（`metric_version=0` 落成当前 ACTIVE 版本、全量实时作业不带口径键、区间按左边界规整、`window_start_to=0` 由服务端按「现在」补终点、主体 `(0,0)` 视为全部主体）；`GetAggregationJob` 双键查询的**现状哨兵**（按 `request_id` 查不到、两键矛盾时以 `job_id` 为准、负 `job_id` 加有效幂等键查不到）；`InsertIfAbsent` 不回填主键 ⇒ 回读为空时绝不回 `job_id=0`；列表的过滤与分页下传、非法类型/状态拒绝、越界页只回 `total`、深翻页保护、库故障原样上抛 |
| **读路径** | | | |
| `readpath_test.go` | 8 | 60 | 八个读方法共用的三条线：`window_start=0` 先点水位、水位行缺失才退回已闭合窗口的极值、显式 `window_start` 规整到左边界且**不查水位**；无数据回 `found=false` + 回显身份，**绝不伪造 0 值**；口径缺失/非 ACTIVE/粒度未登记一律显式报错，ACTIVE 指针二义不得随机挑一个。逐方法：`GetMetric` 命中路径每次回源、水位停摆仍回真实 `window_start`；`BatchGetMetrics` 区间右端取各口径**最近闭合窗口的最小值**（取最大值会让水位落后的口径整段落空）、一个闭合窗口都没有时空 map 且不发区间查询、重复口径只解析一次、任一口径不可用/DRAFT/未登记粒度**整单拒绝**；`ListHotSubjects` 出榜主体白名单、名次整榜连续编号、`zone_id` 进榜条件、还没有闭合窗口时空榜回显 0 而不是全 0 榜、`ps` 越界拒绝而不是 clamp、越界页只回 `total` 不再发列表 SQL、深翻页守卫与 `total` 无关；`ListConsumerState` 条件与分页逐列下传（topic 裁剪后原样进 SQL、`offset=15`/`limit=5` 如实传）、聚合下界就是 `now - BehaviorRetentionDays*86400` 且 COUNT 与 SUMMARIZE **各记一次同一下界**；`ListDeadLetters` 过滤条件（topic/state/since/offset/limit 组成的 `DeadLetterFilter`）同进 COUNT 与 LIST（`dls.seen` 两次全等）、`state` 白名单拒绝而不是当不限、留档字段逐列回带、`payload_preview` 原样回带且响应里不存在取原文的入口（用脱敏预览之外的明文标识符反查整份响应）；`GetUserInterest` `top_n` 默认取配置并原样下传、脏兴趣键丢弃只回受控键、空画像与过期画像都置 `stale`（`stale=false` + 空列表会被读成「这人确实没兴趣」）、判定基准是组内最大 `event_time` 且严格大于阈值；`GetRetention` `cohort_date` 规整到天边界、`rate` 由分子分母现算而不是照抄落库比率。每个用例都成对断言响应与假件计数/下传参数 |
| **替身与脚手架（无用例）** | | | |
| `fakes_test.go` | 0 | 0 | 10 个 model 接口的内存假实现 + 事务/限流假件 + `ServiceContext` 装配，见第 4 组 |

### 2. 其他层

- `model`（3 个文件 `34/7`）——本服务的 model 层没有单独的 repository 层，静态门禁直接落在这里：
  - `migration_parity_test.go` `13/0`：把「model 的 SQL 与 `deploy/migrations/spm` 逐列一致」
    变成可执行门禁——列名与声明顺序、可空与默认、列宽与 model 截断长度、主键与全部唯一键
    （`uniq_metric` 六列、`uniq_watermark` 四列、`uniq_request_id` 等）、model 查询依赖的索引前缀
    （`idx_metric_latest`、`idx_claim`、`idx_state_ctime`、`idx_cohort_date`）、列注释与表选项、
    迁移文件命名与顺序、可重复执行与禁用语句、迁移里不得留未建模的列、库名 `go_video_spm`
    与 `etc` 的 `DataSource` 同值。不连库，只读 SQL 文本比对。
    本文件 `:25` 自己写明「迁移本轮**没有**在 MySQL 上执行过，结构正确性只能靠这种静态一致性证明」。
  - `insert_sql_source_test.go` `2/1`：10 张表里只有 3 张把批量 INSERT 的列清单提成包级常量，
    其余 7 张内联在方法里的字符串拼接中。这里用 `go/ast` 读**实现源码**把拼接表达式求值成真实
    列清单再与迁移比对，钉列数 == VALUES 条数、`rowPlaceholders` 首参常量 == 列数、
    显式清单不得含自增 `id`、写路径只打已建模的表。不把清单搬进测试：文件头 `:24` 说明那样
    等于再造一份需要同步的文本，门禁会永远「对得上」。
  - `window_cursor_test.go` `19/6`：不连库就能证对的纯计算——`AlignWindow` 幂等/只向左/与
    `WindowSeconds` 严格同档、天级分桶与 `DayStartUnix` 一致、`checkWatermark` 拦住未规整的
    `last_closed_start`（放过去会让「最近闭合窗口」稳定指向不存在的窗口）、`clampLimit/clampOffset/clampBatch`
    的上界（漏一次 `LIMIT` 编译期与单测都发现不了）、受控词表闸门（`ValidInterestKey`/`NormalizeAction`/
    `ContentStateOfAction` 不默认 NORMAL/`TopicFor` 委托信封/内容类型归一与主体类型解析）、
    `build*Query` 的占位符与 args 必须同序、`BuildEventFilter` 必须有界区间、NULL payload 走 SQL NULL。
- `internal/config`（1 个文件 `6/2`）：`config_load_test.go` 用与 `main` 里 `conf.MustLoad` 同一条
  解析路径加载 `etc/*.yaml` 并过 `Validate`（钉住两类「能编译、启动即挂」事故：Config 自带的
  `redis` 字段与 `zrpc.RpcServerConf` 内嵌的同名键冲突、字段缺 `,optional`/`default`），另钉
  Config 不得有名为 `redis` 的字段、`Validate` 拒绝语义危险的配置组合、`DataSource` 必须指向本服务库、
  `Listen` 不得占用网关 HTTP 端口、示例配置不含密钥。
- `internal/svc`、`internal/server`：**无离线单测**。本服务没有 `internal/repository`、
  `internal/policy`、`internal/consumer` 目录（README「消费链路」：事件入口只有 MQ，尚未接线）。
- 尚未接线的执行器/清理侧只有静态索引证据、没有用例：作业认领四个方法
  （`model/aggregation_job.go:170/226/246/269` 的 `ClaimPending`/`RenewLease`/`UpdateProgress`/`MarkFinished`）
  与三个清理方法（`model/behavior_event.go:362`、`model/retention_cohort.go:196`、
  `model/consumer_offset.go:377`）——静态门禁只钉住它们依赖的索引存在
  （`model/migration_parity_test.go:239`、`:257`）。缺口的权威登记在本 README 的上一节「契约缺口」
  （第 6 条执行器未接线）。
- 合计 **79 个顶层用例、135 个子用例**（logic `39/126` + model `34/7` + config `6/2`）；
  **0 个 `t.Skip`**。

### 3. 构造器级覆盖

`16/16`：探针取 `internal/logic` 全部 `New*Logic(`（16 个，与 `rpc/spm.proto:494-530` 声明的
16 个方法一一对应：GetMetric / BatchGetMetrics / ListHotSubjects / GetUserInterest / GetRetention /
WriteMetricWindow / RecomputeMetrics / UpsertMetricDefinition / UpdateMetricDefinitionState /
GetMetricDefinition / ListMetricDefinitions / SubmitAggregationJob / GetAggregationJob /
ListAggregationJobs / ListConsumerState / ListDeadLetters），逐个在 `*_test.go` 里查引用，`gaps:` 为空，
即每个方法都从构造器进入被打过。

### 4. 替身层与断言口径

`ServiceContext` 的依赖字段本身就是 10 个 model 接口加 `sqlx.SqlConn` 与两个
`ratelimit.Limiter`（`internal/svc/servicecontext.go:21-59`），所以注入缝就是结构体本身：
用例直接手工装配 `&svc.ServiceContext{Config: testConfig(), DB: fakeConn, 十个 model: 假件,
ReadLimiter/WriteLimiter: 计数假件}`，logic 与 `helpers.go` 的判定整条留在被测路径上。
`Cache` 恒为 `nil` ⇒ `shortCacheTTL` 为 0，缓存路径整体关闭，真值判定全部走内存 model。
`testConfig()` 与 `etc` 示例同值且**必须过 `Validate()`**（不过就 `panic`）：假的
`ServiceContext` 不能代表一个非法服务端。

假件复刻的是 model 层注释写明的 SQL 语义，而不是「永远成功」：`uniq_metric` 六列覆盖、
`ON DUPLICATE` 的 `event_time` 守卫、水位 `last_closed_start <= ?` 的条件推进、`UpdateState`
的 `WHERE state = fromState` CAS、`InsertIfAbsent` 不回填主键（插成功却读不到由 `created`/`readBackNil`
旋钮专门造出来）。并发与「读后写之间被改掉」用钩子与脚本化返回值表达：`onUpdateState` 在 CAS 之前
并发改状态、`countSeq` 让同一次迁移的两次 `CountActive` 给出不同值（并发赢家只在第二眼出现）、
`findBlind` 让前 N 次版本点查返回 nil。事务假件 `fakeConn.TransactCtx` 用「快照 → 失败整库还原」
等价 MySQL 回滚，并计 `txRuns`/`txRollbacks`；`WithSession` 派生出的窗口/水位假件**只多一个
「我在事务内」的事实，计数记在同一份状态上**（`winState`/`wmState`），否则「没走事务」这个 bug
会被假件一起放行。错误注入按方法/语句粒度，注入用的 `errStub` 与 model 哨兵区分开，
保证断言的是「错误被原样上抛」。

断言口径：读路径的失败模式不是报错，而是「把查不到表现成 0 值真数据」和「把筛错了表现成 全命中」，
所以每个用例断言**两半**——响应本身，加上假件的调用计数与下传参数（`findOneCalls`/`latestCalls`/
`naturalCalls`/`listKeysCalls`/`summarizeCalls`/`countCalls`、`seenTopic`/`seenSince`/`seenStates`/
`seenOffset`/`seenTopN`、`upsertLate`/`advanceApplied`、`updateCalls` 形如 `key@vN:from->to`）。
计数才是「该拒的根本没去查库」「该省的第二条 SQL 真的没发出去」「COUNT 与 LIST 同条件」的唯一证据；
顺序结论则来自 `txRuns` 的精确值和 `updateCalls`/`advanceApplied` 的追加序（第二条即回滚）。
布数据走静默播种路径（`seedDef` 等直接往内存库放行，绕过 logic），因此计数断言从 0 数起。

### 5. 覆盖边界

- 用例不连接 MySQL/Redis/etcd/MQ/Elasticsearch/对象存储，也不起 gRPC 服务端；
  `internal/server`、`rpc/*.pb.go` 等 goctl 生成壳不在单测范围内。
- 替身只复刻 SQL 的**语义**，不证明 SQL 与列名本身：唯一键是用 map key 模拟的（真库靠索引），
  CAS 未命中在替身里是「状态不等」而真库是 `RowsAffected == 0`——驱动 matched vs changed rows
  的差别（`clientFoundRows`）替身证不到；分页是 `sort` + 切片，`ORDER BY`/`LIMIT/OFFSET`
  与索引命中不被覆盖。列名与索引由第 2 组的三个 model 静态门禁在**文本层**钉住，
  结论仍是「与迁移 SQL 一致」而不是「与真库一致」。
- 事务原子性由 MySQL 保证：替身只证「同事务、失败整体回滚、事务开了几个」，
  不覆盖隔离级别、间隙锁、死锁与重试。
- 缓存路径整体未执行（`Cache` 为 `nil`）：热榜分页与 ACTIVE 指针的读穿、以及「登记/状态迁移显式失效
  `govideo:spm:def:active:<metric_key>`」（README「口径版本策略」）在离线用例里没有真 Redis 参与；
  限流假件只按「前 N 次拒绝」计数，令牌桶速率、突发与并发不在范围内。
- 消费者链路（MQ 投递、事件去重、退避重试、死信写入）没有用例——本服务无 `internal/consumer`。
  `BehaviorEventModel`/`ContentProjectionModel` 两个假件刻意是纯 nil 接口（`fakes_test.go:1037-1047`），
  调用即 panic，因为 16 个 gRPC 方法都不写这两张表（见「消费链路」）。
- 时钟不可注入：logic 直接调 `time.Now()`（`fakes_test.go:1051-1052` 声明），迟到容忍、水位停摆与
  `stale` 的基准都是「相对 now」，绝对常量只作历史起点；跨分钟/跨午夜的抖动不在覆盖范围内。
- **迁移 SQL 与真实库的列级对账未在目标实例复验**：本 README 没有隔离实例（`127.0.0.1:3399`）
  的复验结论，`model/migration_parity_test.go:25` 本身写明「迁移本轮**没有**在 MySQL 上执行过」；
  `deploy/migrations/README.md:130` 把本服务登记为 `applied`，其口径只是「`up` + `status` 跑通」
  （见同文件 `:21`），不覆盖 model ↔ DDL 的逐列对账。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/spm/...
gofmt -l services/spm    # 必须为空
go vet ./services/spm/...
```

`-p 1` 是硬要求：Windows 页面文件限制下并发跑多个测试包会 OOM（errno=1455），不是可选的性能调优。
本节只声明覆盖范围与口径，不代表任何门禁结论；执行结果由仓库级质量门禁统一记录。
