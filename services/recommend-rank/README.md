# recommend-rank

视频推荐**排序**服务：对调用方传入的候选做站内多目标打分排序，并把每一次排序沉淀成可回放的决策摘要。

它管的是「这一屏按什么顺序排、用哪一版模型、命中哪个实验」，不管候选从哪来（那是 recommend-recall），
也不管内容本身（那是 video/catalog）。运营能改的只有模型版本、特征配置与实验状态三件事，
且每次改动都带 `operator`/`reason` 并可审计。

## 职责

- **多目标排序**：4 个受控目标（`pred_click` / `pred_finish` / `pred_interact` / `pred_negative`）
  按登记的权重线性合成综合分，负向目标做减法；目标集合由 `model.SupportedObjectives()` 白名单收口，
  不在名单内的 objective 直接报错（`model/errors.go:363-382`）。打分是纯函数（`internal/logic/scoring.go:1-9`），
  不读全局状态，因此同一份「候选 + 特征 + 权重」必然得到同一顺序。
- **A/B 实验与稳定分桶**：`model.BucketOf()`（sha256 大端取模）保证同一主体对同一实验的分桶可重算；
  `GetExperimentAssignment` 首次访问落一行 sticky 记录，之后的读取以库内事实为准，
  于是「运营面看到的分组」和「在线实际命中的分组」是同一个数。
- **可审计决策**：每次排序产出一个 `decision_id`，连同两个 digest、`top_aids`、三类过滤计数、
  模型三元组（model_key / model_version / feature_config_version）、实验三元组（exp_key / variant_key / bucket_no）、
  耗时与降级标记写入 `rank_decision_log`；调用方拿 recall 的 `snapshot_id` + 本次 `request_id` 就能把链路回放出来。
- **失败关闭的降级**：模型/特征/安全结论读不到时按 `FallbackStrategy` 回退召回原序并置 `degraded=true`，
  绝不返回「看起来正常」的空结果（`rpc/rank.proto:25-27`）。

在推荐链路里的位置：**recall 之后**。候选由调用方（gateway/app 经 recommend-recall）传入，跨服务只传主键 `aid`。
本服务**不调用召回接口**——`internal/svc/servicecontext.go:42-44` 明确记录了这一点，`internal/repository/` 里
也没有任何 recall 客户端；召回快照只作为 `snapshot_id` 字段被透传进审计行（`rpc/rank.proto:162`）。

硬约束（契约级，违反即放弃本次结果）：**返回的 items 永远是入参 candidates 的子集**，
不得凭空生成 aid（`rpc/rank.proto:16-17`，实现见 `internal/logic/rankcandidateslogic.go:158-168`
与 `internal/logic/scoring.go:424`）。

不做的事（AGENTS.md §7）：不提供广告位参数、不做广告投放与广告推荐、不做「手工置顶/加权某个 aid」的写接口。
契约里因此没有 `aid → 权重/位置` 的入参，也没有任何商业化字段（详见下方数据所有权边界）。

## 数据所有权边界

本服务只拥有 **`go_video_recommend_rank` 库里的 5 张 `rank_*` 表**与 **Redis 上 `rk:v1:` 前缀的键**：

| 归属 | 载体 | 说明 |
| --- | --- | --- |
| 自有表 | `rank_model_version`、`rank_feature_config`、`rank_experiment`、`rank_experiment_assignment`、`rank_decision_log` | 迁移见 `deploy/migrations/recommend-rank/000001~000005` |
| 自有缓存 | `rk:v1:{rtcfg\|activemodel\|runexp\|decision\|assign}:*` | `internal/repository/cache.go:43-49`，只存本域配置快照与回放指针 |
| 只读外部 | feature-store 特征、spm 质量先验/兴趣、moderation-orchestrator 可见性、ops-config 干预参数 | 4 个只读接口，见 `internal/repository/downstream.go:28/:43/:55/:90` |

AGENTS.md §5 对本服务的两条约束及落地证据：

1. **不得同步改计数。** 全仓 `UPDATE` 语句只出现在两处：模型版本状态迁移（`model/modelversion.go:144-245` 的事务）
   与实验状态迁移（`model/experiment.go`）——都是本域元数据。播放量、点赞数、评论数等其它域的计数
   在本服务代码里既没有模型文件也没有 SQL；`rank_decision_log` 是**只插入**的摘要表
   （`model/decisionlog.go` 只有 `InsertTx`/`Insert` 与读方法，加上按保留期的清理删除）。
2. **不得写其他域主数据。** 稿件、用户、媒资、订单都不属于本服务，本服务不落它们的主数据。
   证据有三条：① `model/migration_sync_test.go:28` 的表名正则只接受 `rank_[a-z_]+`，
   任何越界表名都会让「迁移 ↔ model 对账门禁」直接失败；② 4 个下游接口全部只有读方法、
   没有任何写/提交语义（`internal/repository/downstream.go` 的接口声明）；③ `Repository.SetDownstream`
   （`internal/repository/repository.go:87`）**没有生产调用点**，装配时固定注入 `NewStubDownstream()`
   （`internal/repository/repository.go:73`），所以当前连一次下游读都发不出去。

同时登记本 README 旧版本里的三处不实（均已按代码改正）：

- 「提供能力：**频控**」——频控只登记了 override 白名单键、没有任何生效代码，见已知缺口 3。
- 「依赖：**recommend-recall**」——本服务不调用召回，只接收调用方传入的候选与 `snapshot_id`。
- 「依赖：**moderation**」——应对应 `moderation-orchestrator`（可见性复核，且本期未接线，见待接线接口）。

边界自查里唯一需要说明的写路径：`rank_experiment_assignment` 的 sticky 行由 `GetExperimentAssignment` 写入
（`model/experimentassignment.go:77-116` 用 `INSERT IGNORE`，并发下不产生第二条）。热路径 `RankCandidates`
**不写分桶行**，避免在线 QPS 放大写（`internal/logic/rankcandidateslogic.go:96`）。

## gRPC API

`recommendrank.v1.Rank`，10 个方法。分组轴取自 `rpc/rank.proto` 自身的 5 段分隔注释
（`:157`、`:187`、`:245`、`:306`、`:385`）——这 5 段就是契约设计时的真实切分：一次在线排序、两条审计读、
五条受控写、一条分桶读、一条参数下发。

鉴权口径（整服务共性，先说一次）：本服务**没有服务端鉴权拦截器**（`recommendrank.v1.go` 未注册任何
interceptor），`operator`/`reason`/`idempotency_key` 只做非空与形状校验（`internal/logic/helpers.go:116-152`）。
信任边界靠调用方：写接口只由 gateway/admin 暴露，且挂在 `AdminPermission` middleware 下、`operator`
由会话渲染成 `admin:<admin_id>`（`gateway/admin/api/admin.api:6469-6513`）。

### 在线排序（`rank.proto:157`）

| 方法 | 做什么 | 幂等 / 超时 / 边界要点 |
| --- | --- | --- |
| `RankCandidates` | 对传入候选做多目标打分排序，返回子集 + 决策摘要 + 降级信息 | **幂等**：`request_id` 命中既有决策即按既有事实回放；同 `request_id` 换了输入（`input_digest` 不同）报 `ErrRequestIDReused`，不拿旧结果冒充（`rankcandidateslogic.go:118-122`、`:877-880`）；`idempotency_key` 为空时用 `request_id` 兜底（`:225-227`）。**超时**：下游单次 `DownstreamTimeoutMs`（默认 30ms，`:829-832`），打分预算 `ScoreBudgetMs`（80ms，`:676-685`），整体由调用方超时兜。规模超限一律报错不静默裁剪；空候选硬报错不写审计行（`:232-235`）。隐私：`device_id_hash` 必须是 64 位十六进制 sha256（`:199`） |

### 审计读（`rank.proto:187`，结果摘要必须可查）

| 方法 | 做什么 | 幂等 / 超时 / 边界要点 |
| --- | --- | --- |
| `GetRankDecision` | 按 `decision_id` 或 `request_id` 回放一次排序决策 | 只读，无幂等键；两个键必须给且只给一个，否则 `ErrDecisionIDRequired`；未命中返回 `ErrDecisionNotFound` 而不是空对象冒充命中（`internal/logic/getrankdecisionlogic.go:29-81`）。读的是 MySQL，不走缓存 |
| `ListRankDecisions` | 按实验/模型/场景/时间窗分页查决策摘要，支持 `only_degraded` | 只读；`ps` 超 `MaxDecisionPage`（100）报错不裁剪；索引按真实查询条件建（`deploy/migrations/recommend-rank/000005`）。供 admin 审计页与实验核对用 |

### 模型版本与特征配置（`rank.proto:245`，写接口全部带 operator/reason 与幂等键）

| 方法 | 做什么 | 幂等 / 超时 / 边界要点 |
| --- | --- | --- |
| `UpsertModelVersion` | 登记/更新模型版本元数据（多目标权重、绑定的特征配置、工件引用、离线指标） | 幂等靠业务唯一键 `(model_key, version)`：同语义重复登记回 `deduplicated=true` 而非新建。`version` 不可变，改权重只涨 `revision`。校验：权重目标受控、权重和 ≤ `MaxWeightSum`(10)、`artifact_ref` ≤ min(配置, 列宽 512) 且只存对象存储 key（`upsertmodelversionlogic.go:72-80`、`:190`）。绑定不存在的特征配置直接拒。写成功后失效 ACTIVE 缓存（`:159`） |
| `SetModelVersionState` | 迁移版本状态 DRAFT→READY→ACTIVE→RETIRED；**激活就是回滚开关** | 目标态只允许 READY/ACTIVE/RETIRED；`reason` 必填（激活/回滚理由是审计要求）。ACTIVE 靠生成列 + `UNIQUE KEY uniq_active` 保证每个 `model_key` 至多一个，切换在事务里做（`model/modelversion.go:144-245`）。同目标态重复调用回 `deduplicated=true` 且 `changed=false`（`setmodelversionstatelogic.go:76-78`、`:146`）。`event_id` 恒为空字符串——MQ 未接，留空而不是伪造（`:184-190`） |
| `UpsertFeatureConfig` | 登记特征配置版本：特征清单 + 缺失值策略 + 预留的 feature-store 读取场景 | 幂等靠 `UNIQUE KEY uniq_config_version`；`feature_keys` ≤ 512，清单以 sha256 `keys_digest` 存指纹以便比对。配置版本**不可变**，同版本改内容报冲突（`upsertfeatureconfiglogic.go:33`、`:69`）。`missing_policy` 只接受 `default`/`drop_source`/`reject`（`model/errors.go:392-403`）。新登记即 `enabled`，停用入口 RPC 未暴露（`:84`） |

### A/B 实验与分桶（`rank.proto:306`）

| 方法 | 做什么 | 幂等 / 超时 / 边界要点 |
| --- | --- | --- |
| `UpsertExperiment` | 新建/修改实验变体（层、桶区间 `[start,end)`、绑定的模型/特征版本、参数覆盖、起止时间） | 幂等靠 `UNIQUE KEY uniq_variant (exp_key, variant_key)`；同语义回 `deduplicated`，改内容涨 `revision`。`overrides` ≤ 4096 字节且只接受 5 个受控 key（`model/override.go:22-49`），出现商业化字段即拒。模型/特征版本必须已存在（`upsertexperimentlogic.go:172`）。**RUNNING 变体不可改**，要改先暂停。同层桶区间重叠只有应用层「先查后写」，并发不拒（`:140`，见已知缺口 8） |
| `SetExperimentState` | 迁移变体状态 RUNNING/PAUSED/STOPPED | `reason` 必填；转 RUNNING 前复核桶重叠并拒绝越过 `end_at` 的启动（`setexperimentstatelogic.go:134`、`:165-169`）。幂等口径同上（同目标态回 `deduplicated`、`changed=false`） |
| `GetExperimentAssignment` | 查/登记某主体在某实验的稳定分桶 | 决定性的：`BucketOf(subject_type, subject_id, exp_key, hash_seed, bucket_count)` 可重算，sticky 行优先于纯计算。写用 `INSERT IGNORE`，天然幂等且并发不产生第二条（`model/experimentassignment.go:77-116`）。库内 `hash_seed` 与入参不同 → `ErrExperimentSeedConflict`（改盐必须显式失败，见 `getexperimentassignmentlogic.go:29-107`）；桶空间与变体登记不一致时拒答；实验不在 RUNNING 时不写新行。`bucket_count` 只回显、上限 1_000_000（`:65-67`），实际分桶口径永远用库内/配置值 |

### 在线面参数与运行时状态（`rank.proto:385`）

| 方法 | 做什么 | 幂等 / 超时 / 边界要点 |
| --- | --- | --- |
| `GetRankRuntimeConfig` | 下发在线排序参数：ACTIVE 模型三元组、目标列表、上限/预算/兜底策略、当前 RUNNING 变体集合、配置代次摘要 | 只读。本服务**唯一**走 Redis 读路径的接口（`rk:v1:rtcfg:` + `RuntimeConfigCacheTTLSeconds`，`internal/repository/cache.go:52`、`getrankruntimeconfiglogic.go:63/:87/:95-114`）；其余缓存键只有写/失效（见已知缺口 4）。`active_model_version` 为空即宣告「在线必然降级」，而不是回一个看起来正常的默认值 |

## 数据模型与迁移

5 个迁移，按版本号递增，全部在 `deploy/migrations/recommend-rank/`；每个文件头部写明库名、数据所有者
（AGENTS.md §5）、表 ↔ model 映射、唯一键的幂等语义、索引对应的真实查询、保留期与回滚语句。

| 迁移 | 表 | 关键列 / 索引 | 幂等与保留期 |
| --- | --- | --- | --- |
| `000001_create_rank_model_version.sql` | `rank_model_version` | `state`、`objective_weights` JSON、`feature_config_version`、`artifact_ref VARCHAR(512)`、`offline_metrics`（禁含广告/支付指标，`:30-31`）；`idx_model_state`、`idx_feature_config`；生成列 `active_model_key VARCHAR(64) GENERATED ALWAYS AS (IF(state=3, model_key, NULL)) STORED` + `UNIQUE KEY uniq_active` | 业务唯一键 `(model_key, version)`；「每个 model_key 至多一个 ACTIVE」由数据库而非代码保证 |
| `000002_create_rank_feature_config.sql` | `rank_feature_config` | `feature_keys` JSON + `keys_digest`（sha256 清单指纹）+ `feature_count`（冗余便于巡检）；`missing_policy` 三值枚举 | `UNIQUE KEY uniq_config_version`；配置版本不可变 |
| `000003_create_rank_experiment.sql` | `rank_experiment` | `layer_key`、`bucket_start/end`、`overrides`、`state`、`revision`；`idx_layer_state`/`idx_running`/`idx_state_end`/`idx_exp_key`；`:25-29` 显式声明「本表不提供任何把某个 aid 推进/置顶/屏蔽的列」 | `UNIQUE KEY uniq_variant (exp_key, variant_key)`；桶重叠校验只在应用层（`:36-38` 自陈缺口）；`UPDATE...LIMIT` 要求 `binlog_format=ROW`（`:58-59`） |
| `000004_create_rank_experiment_assignment.sql` | `rank_experiment_assignment` | `subject_type`、`subject_id`（mid 或设备 sha256，不存明文设备号）、`hash_seed`、`bucket_no`、`variant_key`；`idx_variant`/`idx_seed_variant`/`idx_assigned_at` | `UNIQUE KEY uniq_subject (exp_key, subject_type, subject_id, hash_seed)` + `INSERT IGNORE`；`hash_seed` 进唯一键所以换盐历史可追；保留期 = `AssignmentRetentionDays`(180) |
| `000005_create_rank_decision_log.sql` | `rank_decision_log` | 38 列宽行：两个 digest、`top_aids`、`snapshot_id`/`pool_version`、`degraded`/`degrade_reason`/`fallback_strategy`、5 个计数列、`cost_ms`、8 个二级索引（写放大上限） | `UNIQUE KEY uniq_request_id`（回放锚点）+ `UNIQUE KEY uniq_decision_id` + **非唯一** `idx_idempotency`（同键多次请求是允许的，只用于人工核对）；归档两步走 `SelectExpiredBefore` → 导出 → `DeleteExpiredBefore`；不分区（文件里给了理由） |

迁移与 model 层的一致性由 `model/migration_sync_test.go` 钉住：它挡住「模型里写了 SQL 但迁移没有对应列/索引」、
「表名前缀越界（`:28` 只认 `rank_[a-z_]+`）」、列别名不一致（`:42-44`）、生成列缺失（`:48-50`）、
5 张表任一必需键缺失（`:53-64`）这 5 类事故。迁移均已执行（`deploy/migrations/README.md:123` 标记 applied）。

## 配置与运行

配置全部来自 `-f` 指定的 yaml（`recommendrank.v1.go:19`）；本服务**不读任何自定义环境变量**，
端口、DSN、上限、开关一律以 `etc/recommendrank.v1.yaml` 为准。

| 项 | 值 | 位置 |
| --- | --- | --- |
| gRPC 服务名 / 监听 | `recommendrank.v1.rpc` / `0.0.0.0:8123` | `etc/recommendrank.v1.yaml:1-2` |
| 注册中心 | etcd `127.0.0.1:2379`，key `recommendrank.v1.rpc` | `:3-6` |
| MySQL | 库 `go_video_recommend_rank`（`DataSource`） | `:15-17` |
| Redis | 键名**必须**是 `CacheRedis`：`zrpc.RpcServerConf` 内嵌了同名鉴权/限流字段，业务缓存另起名字会让服务启动即失败 | `:8-13` |
| HTTP | 无（纯 gRPC 服务，没有 HTTP 服务器） | `recommendrank.v1.go` |
| gRPC reflection | 仅 dev/test 注册 | `recommendrank.v1.go:31-33` |

```powershell
# 执行迁移（-Action up；默认 -Action status 只看当前版本）
pwsh scripts/migrate.ps1 -Action up -Service recommend-rank

# 起服务
go run ./services/recommend-rank/recommendrank.v1.go -f services/recommend-rank/etc/recommendrank.v1.yaml
```

改契约的正确顺序（`docs/commands.md:104-118`，goctl 1.10.2）：改 `rpc/rank.proto` →
在 `services/recommend-rank/rpc` 下 `goctl rpc protoc rank.proto --zrpc_out=..` 重新生成 →
`node scripts/gen-api-docs.mjs` 同步接口文档。不要手改生成物。

**健康检查**：纯 gRPC 服务，走标准 gRPC health 探针（与其余 rpc 服务同一口径）：

```
grpc_health_probe -addr=127.0.0.1:8123
```

注意 `deploy/docker-compose/docker-compose.yml` 只对 MySQL/OpenSearch 做 healthcheck，本服务没有容器级探针；
仓库里也没有任何 etc yaml 设置 go-zero 的 `Health:` 开关。`Repository.Ping`（`internal/repository/repository.go:102-113`，
注释写着「健康探针与启动自检用」）目前**没有生产调用点**，所以探针只能证明进程活着和 gRPC 可连，
不能证明 MySQL/Redis 可读——见已知缺口 7。

生成物与手写件的区分：`rpc/rank.pb.go`、`rpc/rank_grpc.pb.go`、`internal/server/` 由 goctl 生成，
不要手改；`internal/logic/scoring.go`、`conv.go`、`helpers.go`、`internal/repository/*.go`、`model/*.go` 是手写件。

### 降级矩阵

`degraded=true` 时 `fallback` 说明用了哪条兜底路；`ttl_seconds` 在降级时固定为 0（`rankcandidateslogic.go:921`、
`recommendedTTL` `:927-932`），避免降级结果被网关缓存住。`reason` 是受控枚举
（`rpc/rank.proto:81-90`），落库与回包共用一套 key（映射见 `internal/logic/conv.go`）。

| 触发条件 | `degrade_reason` | `fallback` | 是否算降级 | `allow_degrade=false` 时 |
| --- | --- | --- | --- | --- |
| 无 ACTIVE 模型 / 模型不可服务 | `model_unavailable` | 配置的 `DefaultFallback`（默认 `recall_order`），`resolveModel` 顺序为 钉定→ACTIVE→previous | 是 | 直接报错 |
| 特征读取未启用 / 读取失败 / 一条特征都没拿到 | `feature_unavailable` | 同上；**禁止**用全 0 分冒充正常（`:666-674`） | 是 | 直接报错 |
| 打分超 `ScoreBudgetMs`，裁剪未打分候选 | `budget_exhausted` | 保留已打分部分并计入 `truncated` | 是 | 直接报错 |
| 安全复核读不到结论 | `safety_unavailable` | 一条都不放行（`:144-147`） | 是 | 直接报错 |
| 声明启用安全复核但没接线（部署不一致） | — | 无兜底 | — | **无论开关都硬报错**（`:138-140`） |
| RUNNING 实验读取失败 | `experiment_unavailable` | 落到 control 组继续 | 是 | 直接报错 |
| 出参 aid 越出候选集（不变量破坏） | `model_unavailable` + `ErrCandidateSubsetBroken` | 回退召回原序，记 error 日志 | 是 | 直接报错 |
| 幂等回放命中既有决策 | — | 复用既有事实，score 归零、`reason_code=replay` | 否 | — |
| 入参候选为空 | `empty_candidates`（枚举预留，**当前到不了**） | — | — | 实际硬报 `ErrEmptyCandidates`，见已知缺口 6 |
| 排序服务整体不可用（进程/网络） | 由调用方判定 | 调用方直接用 recall 的 `rank_in_source` 顺序 | — | — |

最后一行是 `rpc/rank.proto:25-27` 规定的红线：**本服务不可用时链路不依赖本服务也能出结果**。
整条在线路径上没有任何一处会写别的服务的表。

### 待接线接口

`internal/repository/downstream.go` 声明 4 个只读接口，实现当前全是返回 `model.ErrNotImplemented` 的 stub
（`IsUnwired()` 供自检，`:126`）。开关在 `etc/recommendrank.v1.yaml:47-51`，未接线前一律 false——
宁可显式降级也不伪造特征/安全结论。

| 接口（方法） | 目标服务 / 方法 | 打开的开关 | 接线位置 | 未接线时的行为 |
| --- | --- | --- | --- | --- |
| `FeatureSource`（`ItemFeatures` / `UserFeatures`） | feature-store `FeatureStore.BatchGetFeatures`（候选侧批量）/ `GetFeature`（用户侧单点），按 `feature_config_version` 下传受控清单 | `FeatureFetchEnabled` | `Repository.SetDownstream`（`repository.go:87`，目前无调用点） | `feature_unavailable` 降级（`rankcandidateslogic.go:753-755` → `:657-664`） |
| `BehaviorSource`（`ContentQuality` / `UserInterests`） | spm `BatchGetMetrics`（完播/互动/负反馈质量指标）/ `GetUserInterest`。§7：只作排序特征，不提供广告位/投放/商业化字段（`downstream.go:39-51`） | `BehaviorFetchEnabled` | 同上 | 先验 map 为空：`scoreValue.lookup` 只能取模型预估，取不到就按 `missing_policy` 处理（`scoring.go:296-305`）；读取失败也不致命，只留错误日志（`rankcandidateslogic.go:779-792`） |
| `SafetyGate`（`VisibleAids`） | moderation-orchestrator 的批量可见性复核。**对方契约目前只有单条 `GetResult`、没有批量读**，接线时须先补批量 rpc 或改由 gateway 在调排序前用 video 发布态过滤（`downstream.go:55-64` 记了两个方案） | `SafetyCheckEnabled` | 同上 | 硬报 `ErrSafetyGateNotConfigured` ⇒ 现状下 `RankCandidates` 必然失败（已知缺口 1） |
| `OpsConfigReader`（`Resolve`） | ops-config `OpsConfig.ResolveConfig`（场景级 key，如 `recommend.rank.scene.home_feed`）——运营干预的**唯一**入口，`Intervention` 结构体刻意不含 aid/权重/商业化字段（`downstream.go:71-86`） | `OpsConfigEnabled` | 同上 | 用 yaml 静态参数，不读运营配置；`ops_config_revision` 恒为空串（`getrankruntimeconfiglogic.go:210-229`） |

用户侧特征（`FeatureSource.UserFeatures`）本期不取：打分只按 aid 维度查表，用户特征要生效必须先落到
「与目标同名的受控特征」里——这条口径写在 `rankcandidateslogic.go:741-745`，接线时不要绕过。

## 测试覆盖

离线单测（纯 Go，不连 MySQL / Redis / etcd / gRPC，也不需要网络）。
数字为 `grep -cE '^func Test'`（已排除 `TestMain`）与 `grep -c 't.Run('` 实测导出，格式 `顶层/子用例`。
规模合计 **42 顶层 + 1 子用例**，本服务 0 条用例处于 `t.Skip` 状态。

**这一节的重点不是数量而是形状**：`internal/logic` 只有契约级用例，业务分支的离线覆盖集中在 `model` 的
纯计算函数上（分桶哈希、aid 摘要、状态机、override 白名单），因此下面第 5 组的边界声明是读这份覆盖清单的前提。

### 1. `internal/logic`（2 个文件）— `6/0`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `contract_consistency_test.go` | 6/0 | 「rpc 枚举编号 == model 常量」这条隐式契约：`TestRankSourceMatchesModelConstants`、`TestSourcePriorityMatchesRecall`（与 recall 侧同源，任一侧单独加值即失败）、`TestStateEnumsMatchModel`、`TestDegradeAndFallbackEnumsMatchModel`（降级 reason 与 fallback 策略两侧对齐）；`TestEveryRpcMethodFailsClosedOnNilRequest` 逐个调 10 个 `NewXxxLogic` 构造器并断空请求不被当成成功（`:228-230` 同时断言方法数恰为 10）；`TestNoLogicFileStillStubsOut` 扫 logic 目录断无 `ErrNotImplemented` 残留空桩 |
| `helpers_test.go` | 0/0（脚手架） | 只有 `testCtx()` 与 `newTestServiceContext()`，**没有 `func Test`**。`newTestServiceContext` 返回 `&svc.ServiceContext{}` 零值（`Repository == nil`）且刻意不调 `svc.NewServiceContext`（后者会用 etc yaml 的 DSN 去 `sql.Open`）——它证明的是「依赖缺席时必须失败关闭」，不是业务行为 |

### 2. 其他层（同口径实测）

- `model/` — **3 文件 `24/0`**：`hash_test.go`(14) 覆盖纯计算链与判定算式——`DigestAids` 的顺序敏感性、
  `JoinAndSplitAids` 往返、`BucketOf` 稳定且落在 `[0,n)`、`ScaleBucket`、`NewDecisionID`、
  模型版本与实验两条状态机的合法/非法迁移、`ValidateSubjectID` 拒绝明文设备号（隐私红线）、
  桶区间校验、`SupportedObjectives` 排除商业化目标、`JoinFeatureKeys` 去重升序、
  policy/fallback 枚举校验、实验聚合 id 与 join where、`IsDuplicateErr`；
  `override_test.go`(5) 钉 override 白名单：只放行已登记 key、拒绝项点名 key 名、体积上限、
  白名单自身合法且唯一；`migration_sync_test.go`(5) 是迁移 ↔ model 的**静态**对账（表名集合、
  列与 tag 一致、表自描述注释、幂等键存在、文件头含 owner 与回滚），只读文件不连库。
- `internal/repository` — **1 文件 `6/0`**：`TestStubDownstreamNeverFakesSuccess`（桩一律
  `ErrNotImplemented`，绝不返回假特征/假安全结论）、`TestInterventionHasNoResultTamperingFields`
  （干预结构里没有 aid/权重/商业化字段）、缓存键命名空间、缓存被禁用时行为无害、
  Options 默认值确实被 Repository 采用、无库条件下的归档守卫路径。
- `internal/config` — **1 文件 `3/1`**：`etc/` 下每个 yaml 都能被解析（唯一的 `t.Run` 遍历 yaml）、
  反射钉住业务 Redis 字段名为 `CacheRedis`、排序上限来自配置。
- `internal/svc` — **1 文件 `3/0`**：`TestRepoOptionsMapsEveryLimit` 钉 config → repository Options 的
  逐字段映射（接错字段不会编译失败，所以用反射级断言）、零值配置回落默认、`positiveOr` 边界。
- 本服务没有 `internal/consumer`、也没有 `internal/policy` 目录；`internal/server/`、`rpc/*.pb.go`
  是 goctl/protoc 生成壳，不在单测范围。

### 3. 构造器级覆盖

`internal/logic` 的 10 个 RPC 构造器 **10/10**（探针 `PROBE 10 gaps:` 后为空，无缺口名字）。
**口径必须写清**：这 10/10 全部由 `TestEveryRpcMethodFailsClosedOnNilRequest` 一个入口达成，
它的断言是「10 个方法都有入口且空请求不被当成成功」，**不等于**「10 个方法的成功路径被测过」。

### 4. 替身层与断言口径

本服务**没有业务替身层**：`internal/logic` 的用例只使用零值 `ServiceContext`（`Repository == nil`），
下游侧的 `NewStubDownstream()` 是**桩**而不是替身——没有任何测试注入过会返回特征/安全结论的下游，
因此打分、可见性过滤、分桶命中、幂等回放这些业务分支在本仓测试里一次都没有被执行过。
`model` 的用例走纯函数，`migration_sync_test.go` 走文本对账，两者都不触达中间件
（AGENTS.md §9：单测不得触达真实中间件）。
`repository_test.go` 里读方法的用例只覆盖参数校验与空库路径（如归档守卫），
`INSERT IGNORE` 的并发语义、生成列 `uniq_active` 的 DB 层保证、`UPDATE ... LIMIT` 对
`binlog_format=ROW` 的要求都**没有回归用例**——它们是替身证明不了的部分。

### 5. 覆盖边界

- 用例不连接 MySQL / Redis / etcd / gRPC / 对象存储。
- **未证清单（与本节数字同等重要）**：`internal/logic` 的 8 步在线流程、`scoring.go` / `helpers.go` /
  `conv.go` 的纯函数（没有一条用例直接调用过它们）、降级矩阵的每一格、幂等回放的 `input_digest` 比对、
  实验分桶在真实数据上的命中率。
- **没有真实特征仓库**：`FeatureSource`/`BehaviorSource`/`SafetyGate`/`OpsConfigReader` 四个下游
  只有 stub（`Repository.SetDownstream` 无生产调用点），所以「特征缺失走 `feature_unavailable`」
  这类结论目前只有代码路径证据，没有用例证据。
- **没有线上 A/B**：`BucketOf` 的确定性与 sticky 行的幂等被 model 用例钉住，
  但同层桶重叠（已知缺口 8）、RUNNING 集合的在线命中、实验分组与决策行的关联都没有覆盖。
- 迁移 SQL：本 README 记「5 个迁移已执行（`deploy/migrations/README.md` 标记 applied）」且
  「只在隔离实例上手工执行过」，**未在真实/共享实例复验**；列级一致性只有
  `model/migration_sync_test.go` 的静态文本对账，不连库。
- `internal/server`、`rpc/rank.pb.go`、`rpc/rank_grpc.pb.go` 等生成壳不在单测范围。
- `scripts/rpc/smoke.{ps1,sh}` 覆盖全部 10 个方法，但属于连环境的端到端脚本，
  不作为本节离线覆盖的证据。

### 6. 验证命令

```bash
go test -p 1 -count=1 ./services/recommend-rank/...
go vet ./services/recommend-rank/...
gofmt -l services/recommend-rank           # 必须无输出
```

`-p 1` 是硬要求：Windows 页面文件限制下并发编译/运行多个测试包会 OOM（`errno=1455`），
测试门禁一律串行跑包（见 docs/commands.md）。

## 已知缺口

以下条目全部由本轮实际读过的代码钉住现状，`file:line` 均为登记时的真实位置。
与部分兄弟服务不同，本服务**没有**行为级用例可引用（见上方「覆盖边界」），因此每条给的是
「现状 + 影响 + 若要收严改哪里」，而不是「+ 用例名」。

1. **按仓库默认配置，`RankCandidates` 不可能成功。** 现状：etc yaml 里 `SafetyCheckEnabled: false`
   （`etc/recommendrank.v1.yaml:50`），`safetyGate` 在未启用时返回 `(nil, nil)`
   （`internal/logic/helpers.go:795-798`），`checkVisibility` 见 `gate == nil` 就报
   `ErrSafetyGateNotConfigured`（`internal/logic/rankcandidateslogic.go:842-844`），而调用处对这一条是硬报错、
   不走降级（`:138-140`）。影响：默认配置下每次在线排序都失败，这与其他三处对同一开关的口径矛盾——
   `internal/config/config.go:88-90` 与 yaml:47 的注释都承诺「未接线时显式降级为 `safety_unavailable`」，
   `internal/repository/downstream.go:62-64` 写着「`SafetyCheckEnabled=false` 时本接口返回 ErrNotImplemented，
   logic 必须把 degrade_reason 记为 safety_unavailable」，`rpc/rank.proto:25-27` 则假定召回原序兜底可用。
   特征路径是对称且正确的（`:753-755` 造一个非 nil 的 error ⇒ `:657-664` 正常降级），安全路径没这么做。
   若要收严：二选一并把四处注释统一——(a) 未启用时按 `safety_unavailable` 降级放行候选并置 `ttl_seconds=0`；
   (b) 保持硬报错但把「默认配置下在线面不可用」写进部署文档与探针。
2. **两个配置项是死开关。** 现状：`Rank.DegradeEnabled`（`internal/config/config.go:70-71`，yaml:41）在全仓只有
   两个读者——启动日志（`internal/svc/servicecontext.go:70`）与运行时配置投影
   （`internal/logic/getrankruntimeconfiglogic.go:245`），行为由请求里的 `allow_degrade` 决定；
   `FeatureCacheTTLSeconds`（`config.go:98-99`，yaml:55）**零读者**。影响：运营面 `degrade_enabled`
   与实际行为可以不一致，yaml 承诺的特征缓存不存在。若要收严：在 `parseRequest` 里让
   `allowDegrade = in.GetAllowDegrade() && conf.DegradeEnabled`，并删掉 `FeatureCacheTTLSeconds` 或落到
   `fetchFeatures` 的缓存调用上。
3. **频控与打散只登记不生效，两个统计列恒为 0。** 现状：`diversity_gap`/`frequency_cap` 在 override 白名单里
   （`model/override.go:22-49`）、被解析进 `overrideSet`（`internal/logic/helpers.go:479-487`，`:484` 注释已承认
   「只登记、本期不生效」），解析点 `:575`/`:585`，但 `scoring.go` 的任何环节都没用它们，
   `UpsertExperiment` 只把它俩打进日志（`internal/logic/upsertexperimentlogic.go:284`）。
   影响：`FilterStat.frequency_filtered` 与 `diversified_moved` 永远是 0
   （`rankcandidateslogic.go:586-588` 只赋了 safety/dedup/truncated 三个，回包 `:638-642`，投影 `conv.go:106/:108`，
   契约 `rank.proto:141/:143`），实验里配了频控的运营会以为生效了；`rpc/rank.proto:7` 与旧 README 声称的
   「频控与打散」能力目前不存在。若要收严：在 `scoring.go` 的打散/截断阶段消费这两个参数并回填计数，
   或在 `ValidateOverrideKeys` 里把它们标成「预留、不保证生效」。
4. **多数缓存键只写不读、只删不填。** 现状：`DecisionPointer` 的读方法（`internal/repository/cache.go:153`）
   在 `internal/` 里除测试（`repository_test.go:105`）外无调用点，写侧每次成功排序都执行
   （`rankcandidateslogic.go:616`），配套的 `DecisionReplayCacheTTLSeconds`（yaml:57）因此等于没接；
   `ActiveModelKey`（`cache.go:55`）只被 `InvalidateModelConfig`（`:139`）删除、从没人写入，
   所以 `Repository.ActiveModel`（`repository.go:139`）每次都打 MySQL；
   `RunningExperimentsKey`/`AssignmentKey`（`cache.go:60/:72`）只出现在 `repository_test.go:65-71`。
   影响：热路径上每条请求至少一次 ACTIVE 模型查询 + 一次 RUNNING 实验查询都落 DB，
   而 `upsertexperimentlogic.go:44` 与 `setexperimentstatelogic.go:43` 的注释却断言「RUNNING 集合有分钟级分片缓存」。
   若要收严：给这三类快照补读路径（在 `buildPlan` / `resolveExperiment` 里先 `GetSnapshot`），
   或删掉键与注释，别留下「看起来有缓存」的假象。
5. **幂等回放不能复原分数与整页。** 现状：`rank_decision_log` 只存 `result_digest` + 前 `MaxDigestAids`(20) 个
   `top_aids`（`rankcandidateslogic.go:583-585`），回放时逐条把 `score`/`objectives` 归零并标 `reason_code=replay`
   （`:870-875`、`:893-894`），且 `TtlSeconds` 固定 0（`:921`）。影响：同一 `request_id` 重放能得到**同一个顺序的
   证明**，但拿不到分数与完整条目，客户端二次拉取会看到分数为 0 的短列表；
   `getrankdecisionlogic.go:38-41` 已把这条写成「回放的完整性边界」。
   若要收严：要么新增逐条分数明细表（写放大要重新评估，000005 已把索引数当上限），
   要么在契约里把「回放 = 摘要级一致」写明，避免调用方按整页复原使用。
6. **`RANK_DEGRADE_REASON_EMPTY_CANDIDATES` 是到不了的枚举。** 现状：`rank.proto:89` 为它写着
   「不是故障，但必须显式」，`rank.proto:16-17` 要求「候选为空时返回空列表 + 明确原因」，
   `model/errors.go:20-21` 同样这么注释；但实现是硬报错——`in == nil` 与零候选都直接返回
   `ErrEmptyCandidates`（`rankcandidateslogic.go:107-109`、`:232-235`），既不落审计行也不回空列表。
   目前唯一「用到」这个枚举的是映射表本身（`contract_consistency_test.go:157`）。影响：调用方按契约
   处理「空候选」分支时永远等不到 `degrade_reason=empty_candidates`。若要收严：要么按契约改成
   回空列表 + `degraded=true`（并决定是否污染审计表），要么删掉枚举与那三处注释、把「空候选=调用方 bug」写实。
7. **清理与自检方法全都没有生产调用点，表只涨不删。** 现状：`Repository.Ping`（`repository.go:102-113`）、
   `Repository.DisableFeatureConfig`（`:203`，契约未暴露停用入口，见 `upsertfeatureconfiglogic.go:84`）、
   `PurgeExpiredDecisionLogs`/`DeleteArchivedDecisionLogs`/`PurgeExpiredAssignments`
   （`repository.go:280/:289/:300`）、`model.RankDecisionLogModel.CountDegradedSince`（`decisionlog.go:94/:275`）、
   `FindByIdempotencyKey`（`decisionlog.go:89/:203`）、`RankExperimentModel.StopExpired`（`experiment.go:78/:340`）
   ——除 `repository_test.go:135-145` 外零调用。影响：`DecisionRetentionDays`/`AssignmentRetentionDays`
   （yaml:60/:62）形同虚设，过期实验不会自动 STOPPED，「降级率」类告警没有数据源，
   而 `000005` 的注释（`:59`）与 `repository.go:300` 的注释都把归档/清理说成已有流程；
   `000005` 也自陈「本期契约没有 prune RPC」。若要收严：接一个 cron/运维任务（本仓有 cron 服务），
   或把 prune 加进 RPC 并在本 README 的 API 表里登记。
8. **同层桶重叠是「先查后写」，并发能插进重叠区间。** 现状：`UpsertExperiment` 与 `SetExperimentState`
   都在应用层调 `BucketOverlapConflict` 预检（`upsertexperimentlogic.go:140`、`setexperimentstatelogic.go:134`），
   没有 `GET_LOCK` 也没有层锁行；`deploy/migrations/recommend-rank/000003_...sql:36-38` 已把这条列为自陈缺口。
   影响：同层两个变体可能拿到重叠区间，`model.BucketOf` 的双射假设被破坏，同一主体可能同时属于两个变体，
   实验结论失真且不易发现。若要收严：在 `layer_key` 上加咨询锁（或每层一行 `SELECT ... FOR UPDATE`）
   把「读区间—写区间」串起来，并在 `rank_experiment` 上补一条层内区间约束的巡检 SQL。
9. **写接口无服务端鉴权，`operator`/`reason` 可被任何能连上 etcd 的调用方伪造。** 现状：
   `internal/logic/helpers.go:116-152` 只做非空与形状校验，`recommendrank.v1.go` 没注册任何 interceptor，
   整个服务里没有 session/jwt/token 相关代码。影响：审计链的可信度完全依赖 gateway/admin
   的 `AdminPermission` middleware 与会话渲染（`gateway/admin/api/admin.api:6469-6513` 的 `operator: admin:<id>`），
   直连 8123 就绕过了它。若要收严：加服务端 unary interceptor 校验调用方身份（仓内已有同类做法可参考），
   或至少在文档与 etcd ACL 里明确「本端口只允许内网 gateway 网段访问」。
10. **四个预留字段/能力恒为「未接」。** 现状：`SetModelVersionStateReply.event_id` 恒为空串，因为 MQ 未接
    （`rank.proto:286`、`setmodelversionstatelogic.go:38`、`:184-190`「留空而不是伪造」）；
    用户侧特征完全不取（`rankcandidateslogic.go:741-745`）；`CandidateInput.batch_id`（`rank.proto:119`）
    没有落库列，只能塞进降级明细文本（`rankcandidateslogic.go:591-599`）；
    `Intervention` 的注释承诺「每次生效都会把 `config_revision` 记进 `rank_decision_log`」
    （`internal/repository/downstream.go:74-76`、`:84-85`），但 `rank_decision_log` 没有该列、决策行组装时也不读
    ops 代次——`config_revision` 只在 `GetRankRuntimeConfig` 的回包指纹里出现
    （`getrankruntimeconfiglogic.go:177-179`、`rpc/rank.proto:404`），其中 ops 分量在未接线时恒为空串（`:212-230`）。
    影响：模型切换无事件可订阅，下游只能轮询 `GetRankRuntimeConfig`；召回侧的分池信息在审计表里查不全；
    「这段时间的排序结果是被哪一版运营配置配的」无法从审计表反查。
    若要收严：接 Outbox/事件表并复用 `event_type` 命名规范；给用户特征定义受控同名 key；
    给 `rank_decision_log` 加 `batch_summary` 列（同步更新 000005 与对账门禁的 `requiredKeys`）。
11. **平台枚举的报错分类不对。** 现状：非法 `platform` 复用 `ErrInvalidSubjectType` 抛出
    （`rankcandidateslogic.go:203-205`），`model/errors.go` 里没有 `ErrInvalidPlatform`。
    影响：调用方按 sentinel 分流时会把「平台值不认识」误读成「主体类型不合法」，admin 侧排障也会指错字段。
    若要收严：补 `ErrInvalidPlatform` 并改这一处包装（`model/errors.go:38-39` 附近）。
