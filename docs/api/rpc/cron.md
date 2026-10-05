# RPC · `cron`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/cron/rpc/cron.proto` |
| protobuf 包 | `cron.v1` |
| go_package | `go-video/services/cron/rpc` |
| 发现用的 etcd key | `cron.v1.rpc`（`services/cron/etc/cron.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`cron.v1.rpc`） |
| 监听 | `8112`（`services/cron/etc/cron.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_cron` |
| 方法数 | 23（service `Cron`） |
| 网关消费方 | `admin:CronRPC` |

## 契约说明

> 说明：cron 是全仓定时/补偿任务的**调度与审计**所有者，只提供 gRPC（无 .api），
> 遵循 AGENTS.md §3（定时任务放在 services/cron）与 §4（领域服务不返回数据库原始对象）。
>
> 数据边界（AGENTS.md §5）：
>   - 本服务只写 go_video_cron 库自己的 5 张表（任务定义、执行记录、租约、游标、审计）；
>     任何业务数据变更都必须通过下游领域 RPC 完成，绝不直连其他服务的库/表/Redis key。
>   - 本服务不订阅 MQ：任务由 MySQL 到期扫描驱动（见 README「为什么 cron 不接 MQ」）。
>     上游若需要「事件驱动的补偿」，应由事件消费者所属服务落任务，cron 只负责按点触发。
>
> 幂等与重放语义（AGENTS.md §5、docs/api-and-events.md §3）：
>   - 一次「计划触发」由 (task_key, planned_at) 唯一标识，claim 依赖
>     cron_task_run 的 UNIQUE(task_key, planned_at, attempt)：同计划时刻并发只有一个赢家；
>   - 重试是同一 (task_key, planned_at) 下的新 attempt 行，不是新的计划时刻；
>   - 任务处理器必须以 (task_key, planned_at) 为幂等上下文，增量型任务（归档、报表、
>     索引重建、死信重放）以 cron_task_checkpoint 的 CAS 游标记录已处理水位，
>     因此「重放同一计划时刻」只会重新推进游标，不会产生第二次副作用。

## service `Cron`

> Cron 定时与补偿任务的调度、租约与审计服务。 / 方法名表达领域动作，不暴露数据库 CRUD（docs/api-and-events.md §1.1）。

gRPC 方法前缀：`cron.v1.Cron/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `RegisterTask` | [`RegisterTaskReq`](#message-registertaskreq) | [`RegisterTaskReply`](#message-registertaskreply) | 注册任务定义（task_key 唯一，重复注册幂等返回）。 |
| 2 | `UpdateTask` | [`UpdateTaskReq`](#message-updatetaskreq) | [`UpdateTaskReply`](#message-updatetaskreply) | 修改任务定义（乐观锁）。 |
| 3 | `GetTask` | [`GetTaskReq`](#message-gettaskreq) | [`GetTaskReply`](#message-gettaskreply) | 查询单个任务定义。 |
| 4 | `ListTasks` | [`ListTasksReq`](#message-listtasksreq) | [`ListTasksReply`](#message-listtasksreply) | 分页列出任务定义。 |
| 5 | `PauseTask` | [`PauseTaskReq`](#message-pausetaskreq) | [`TaskOperationReply`](#message-taskoperationreply) | 暂停任务（可恢复）。 |
| 6 | `ResumeTask` | [`ResumeTaskReq`](#message-resumetaskreq) | [`TaskOperationReply`](#message-taskoperationreply) | 恢复任务，按 MisfirePolicy 处理暂停期间的过期计划点。 |
| 7 | `DisableTask` | [`DisableTaskReq`](#message-disabletaskreq) | [`TaskOperationReply`](#message-taskoperationreply) | 停用任务（终态，保留历史）。 |
| 8 | `TriggerTask` | [`TriggerTaskReq`](#message-triggertaskreq) | [`TriggerTaskReply`](#message-triggertaskreply) | 立即触发一次执行，或补跑指定计划时刻。 |
| 9 | `ListDueTasks` | [`ListDueTasksReq`](#message-listduetasksreq) | [`ListDueTasksReply`](#message-listduetasksreply) | 拉取到期任务清单（只读，不占租约）。 |
| 10 | `AcquireLease` | [`AcquireLeaseReq`](#message-acquireleasereq) | [`AcquireLeaseReply`](#message-acquireleasereply) | 抢占某个计划时刻（同事务写租约与执行记录）。 |
| 11 | `RenewLease` | [`RenewLeaseReq`](#message-renewleasereq) | [`RenewLeaseReply`](#message-renewleasereply) | 心跳续租，栅栏令牌不一致时返回租约已失效。 |
| 12 | `ReleaseLease` | [`ReleaseLeaseReq`](#message-releaseleasereq) | [`EmptyReply`](#message-emptyreply) | 释放租约并以 SKIPPED/CANCELED 终结执行记录。 |
| 13 | `ReportTaskResult` | [`ReportTaskResultReq`](#message-reporttaskresultreq) | [`ReportTaskResultReply`](#message-reporttaskresultreply) | 上报执行结果（幂等，可带游标 CAS）。 |
| 14 | `GetTaskRun` | [`GetTaskRunReq`](#message-gettaskrunreq) | [`GetTaskRunReply`](#message-gettaskrunreply) | 查询单条执行记录。 |
| 15 | `ListTaskRuns` | [`ListTaskRunsReq`](#message-listtaskrunsreq) | [`ListTaskRunsReply`](#message-listtaskrunsreply) | 分页查询执行记录。 |
| 16 | `RetryRun` | [`RetryRunReq`](#message-retryrunreq) | [`RetryRunReply`](#message-retryrunreply) | 人工重试已终结的执行（同计划时刻追加 attempt）。 |
| 17 | `GetCheckpoint` | [`GetCheckpointReq`](#message-getcheckpointreq) | [`GetCheckpointReply`](#message-getcheckpointreply) | 查询单个游标。 |
| 18 | `ListCheckpoints` | [`ListCheckpointsReq`](#message-listcheckpointsreq) | [`ListCheckpointsReply`](#message-listcheckpointsreply) | 分页查询游标。 |
| 19 | `SaveCheckpoint` | [`SaveCheckpointReq`](#message-savecheckpointreq) | [`SaveCheckpointReply`](#message-savecheckpointreply) | 独立推进游标（CAS）。 |
| 20 | `GetLease` | [`GetLeaseReq`](#message-getleasereq) | [`GetLeaseReply`](#message-getleasereply) | 查询单个任务级租约。 |
| 21 | `ListLeases` | [`ListLeasesReq`](#message-listleasesreq) | [`ListLeasesReply`](#message-listleasesreply) | 分页查询租约（含已过期可抢占项）。 |
| 22 | `ListTaskAudits` | [`ListTaskAuditsReq`](#message-listtaskauditsreq) | [`ListTaskAuditsReply`](#message-listtaskauditsreply) | 分页查询任务变更审计。 |
| 23 | `GetSchedulerHealth` | [`GetSchedulerHealthReq`](#message-getschedulerhealthreq) | [`GetSchedulerHealthReply`](#message-getschedulerhealthreply) | 调度健康度（积压、运行中、退避、近一小时失败、过期租约）。 |

## 消息与枚举

### message `EmptyReply`

> 空响应。

（空消息）

### enum `TaskState`

> 任务定义状态。

| 值 | 编号 | 说明 |
|---|---|---|
| `TASK_STATE_UNSPECIFIED` | 0 | 未指定：查询语境表示「全部状态」 |
| `TASK_STATE_ENABLED` | 1 | 启用：到期即产生计划触发 |
| `TASK_STATE_PAUSED` | 2 | 暂停：不产生新的计划触发，已 RUNNING 的执行继续跑完 |
| `TASK_STATE_DISABLED` | 3 | 停用：终态，保留历史执行记录，只能重新 Register 恢复 |

### enum `ScheduleType`

> 调度方式。

| 值 | 编号 | 说明 |
|---|---|---|
| `SCHEDULE_TYPE_UNSPECIFIED` | 0 | — |
| `SCHEDULE_TYPE_CRON` | 1 | 标准 cron 表达式（UTC+配置时区），使用 cron_expr |
| `SCHEDULE_TYPE_INTERVAL` | 2 | 固定间隔，使用 interval_seconds |
| `SCHEDULE_TYPE_MANUAL` | 3 | 仅手动 Trigger/RetryRun，不自动到期 |

### enum `MisfirePolicy`

> 错过的计划点处理策略（进程宕机或多副本同时看到过期计划点时）。

| 值 | 编号 | 说明 |
|---|---|---|
| `MISFIRE_POLICY_UNSPECIFIED` | 0 | — |
| `MISFIRE_POLICY_FIRE_ONCE_NOW` | 1 | 合并所有过期点，只立即补跑一次 |
| `MISFIRE_POLICY_SKIP_TO_NEXT` | 2 | 丢弃过期点，从下一个计划点继续 |
| `MISFIRE_POLICY_FIRE_ALL` | 3 | 逐个补齐过期点（最多 BackfillLimit 个） |

### enum `RunState`

> 执行记录状态（cron_task_run.state）。

| 值 | 编号 | 说明 |
|---|---|---|
| `RUN_STATE_UNSPECIFIED` | 0 | — |
| `RUN_STATE_PENDING` | 1 | 已 claim，等待 worker 领取 |
| `RUN_STATE_RUNNING` | 2 | worker 持有租约执行中 |
| `RUN_STATE_RETRYING` | 3 | 本次失败，等待退避窗口后重试（同计划时刻的新 attempt） |
| `RUN_STATE_SUCCEEDED` | 4 | 终态：成功 |
| `RUN_STATE_FAILED` | 5 | 终态：达到 MaxAttempts 后失败，需人工 RetryRun |
| `RUN_STATE_TIMEOUT` | 6 | 终态：超过 TimeoutSeconds 被判定超时 |
| `RUN_STATE_CANCELED` | 7 | 终态：人工/暂停取消 |
| `RUN_STATE_SKIPPED` | 8 | 终态：并发上限/misfire 策略下主动跳过（无副作用） |

### enum `TriggerType`

> 触发来源。

| 值 | 编号 | 说明 |
|---|---|---|
| `TRIGGER_TYPE_UNSPECIFIED` | 0 | — |
| `TRIGGER_TYPE_SCHEDULED` | 1 | 到期自动触发 |
| `TRIGGER_TYPE_MANUAL` | 2 | 运营/管理后台手动触发 |
| `TRIGGER_TYPE_RETRY` | 3 | 退避重试产生的新 attempt |
| `TRIGGER_TYPE_REPLAY` | 4 | 人工重放已完成的历史执行 |

### enum `LeaseOutcome`

> 租约结果。

| 值 | 编号 | 说明 |
|---|---|---|
| `LEASE_OUTCOME_UNSPECIFIED` | 0 | — |
| `LEASE_OUTCOME_ACQUIRED` | 1 | 本次 claim 成功，返回 run_id + fence_token |
| `LEASE_OUTCOME_ALREADY_OWNED` | 2 | 同一计划时刻已被其它实例持有且租约未过期 |
| `LEASE_OUTCOME_ALREADY_CLAIMED` | 3 | 本实例已持有该计划时刻，可继续心跳（幂等重入） |
| `LEASE_OUTCOME_NOT_RUNNABLE` | 4 | 任务不存在/已暂停/已停用，不产生执行记录 |
| `LEASE_OUTCOME_CONCURRENCY_LIMIT` | 5 | 达到 ConcurrencyLimit，按 SKIPPED 记录 |

### enum `ReportState`

> 执行结果上报状态（ReportTaskResult 的目标状态，只允许 RUNNING 推进到这些值）。

| 值 | 编号 | 说明 |
|---|---|---|
| `REPORT_STATE_UNSPECIFIED` | 0 | — |
| `REPORT_STATE_SUCCEEDED` | 1 | 成功 |
| `REPORT_STATE_FAILED` | 2 | 失败：服务端按重试策略决定 RETRYING 或 FAILED |
| `REPORT_STATE_TIMEOUT` | 3 | 超时 |
| `REPORT_STATE_CANCELED` | 4 | 取消 |
| `REPORT_STATE_SKIPPED` | 5 | 主动跳过（无副作用） |

### message `TaskDefinition`

> TaskDefinition 任务定义（cron_task_definition 的对外投影）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | 自增主键 |
| `task_key` | `string` | 2 | — | 任务唯一键，如 rights.expire_scan（跨环境稳定，注册表按此索引） |
| `name` | `string` | 3 | — | 展示名 |
| `handler` | `string` | 4 | — | 处理器名：进程内注册表的键（见 README「任务注册表」） |
| `task_group` | `string` | 5 | — | 分组（rights/index/report/cleanup/replay），用于批量暂停与健康统计 |
| `schedule_type` | [`ScheduleType`](#enum-scheduletype) | 6 | — | — |
| `cron_expr` | `string` | 7 | — | SCHEDULE_TYPE_CRON 时使用 |
| `interval_seconds` | `int32` | 8 | — | SCHEDULE_TYPE_INTERVAL 时使用 |
| `timezone` | `string` | 9 | — | cron 表达式时区，默认 Asia/Shanghai |
| `timeout_seconds` | `int32` | 10 | — | 单次执行超时；到期由 worker 判定 TIMEOUT |
| `max_attempts` | `int32` | 11 | — | 含首次的最大尝试次数（1 表示不重试） |
| `retry_base_seconds` | `int32` | 12 | — | 退避基数：base * 2^(attempt-1) |
| `retry_max_seconds` | `int32` | 13 | — | 退避上限 |
| `concurrency_limit` | `int32` | 14 | — | 同一任务允许并行的执行数，1 表示串行 |
| `lease_ttl_seconds` | `int32` | 15 | — | 租约 TTL：到期后可被其它实例抢占 |
| `misfire_policy` | [`MisfirePolicy`](#enum-misfirepolicy) | 16 | — | — |
| `misfire_backfill_limit` | `int32` | 17 | — | MISFIRE_POLICY_FIRE_ALL 时单轮最多补齐的计划点数 |
| `params` | `string` | 18 | — | 处理器参数 JSON 文本（不含密钥，密钥走 Secret 引用） |
| `secret_refs` | `string` | 19 | — | 逗号分隔的环境变量名，处理器自行取值，库里不存密钥值 |
| `state` | [`TaskState`](#enum-taskstate) | 20 | — | — |
| `next_fire_at` | `int64` | 21 | — | 下一个计划时刻（Unix 秒，0 表示不参与到期扫描） |
| `last_fire_at` | `int64` | 22 | — | 最近一次产生执行记录的计划时刻 |
| `last_success_at` | `int64` | 23 | — | 最近一次成功完成时间 |
| `last_error` | `string` | 24 | — | 最近一次失败摘要（截断，不含堆栈） |
| `version` | `int64` | 25 | — | 乐观锁版本，UpdateTask/DisableTask 必须回传 |
| `owner` | `string` | 26 | — | 责任团队/服务 |
| `operator` | `string` | 27 | — | 最近一次变更操作人 |
| `ctime` | `int64` | 28 | — | — |
| `mtime` | `int64` | 29 | — | — |

### message `RunRecord`

> RunRecord 执行记录（cron_task_run 的对外投影）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run_id` | `int64` | 1 | — | 执行记录 ID |
| `task_key` | `string` | 2 | — | 任务键 |
| `planned_at` | `int64` | 3 | — | 计划时刻（Unix 秒）：与 task_key 共同构成幂等身份 |
| `attempt` | `int32` | 4 | — | 第几次尝试，从 1 开始 |
| `trigger_type` | [`TriggerType`](#enum-triggertype) | 5 | — | — |
| `state` | [`RunState`](#enum-runstate) | 6 | — | — |
| `lease_owner` | `string` | 7 | — | 持有租约的实例标识（hostname+pid+随机后缀） |
| `lease_expire_at` | `int64` | 8 | — | 租约到期时间，过期即可被抢占 |
| `fence_token` | `int64` | 9 | — | 单调递增栅栏令牌，上报时必须回传，防止被抢占后的僵尸写 |
| `started_at` | `int64` | 10 | — | — |
| `finished_at` | `int64` | 11 | — | — |
| `duration_ms` | `int64` | 12 | — | — |
| `result_summary` | `string` | 13 | — | 处理器回填的结果摘要（如 scanned=1200,updated=34） |
| `last_error` | `string` | 14 | — | — |
| `next_retry_at` | `int64` | 15 | — | RETRYING 状态的下次可执行时间 |
| `trace_id` | `string` | 16 | — | — |
| `ctime` | `int64` | 17 | — | — |
| `mtime` | `int64` | 18 | — | — |

### message `LeaseInfo`

> LeaseInfo 任务级互斥租约（cron_task_lease 的对外投影）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_key` | `string` | 1 | — | 默认等于 task_key；带 scope 时为 task_key + "/" + scope |
| `owner` | `string` | 2 | — | 当前持有实例，空表示无人持有 |
| `fence_token` | `int64` | 3 | — | 每次抢占 +1，处理器必须把它带给下游写操作 |
| `expire_at` | `int64` | 4 | — | 租约到期时间（Unix 秒），过期即可被抢占 |
| `acquired_at` | `int64` | 5 | — | — |
| `takeover_count` | `int32` | 6 | — | 因过期被抢占的次数，用于发现实例不稳定 |

### message `Checkpoint`

> Checkpoint 增量任务游标（cron_task_checkpoint 的对外投影）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `scope_key` | `string` | 2 | — | 同一任务内的分片/维度游标，如 region=cn、shard=7 |
| `value` | `int64` | 3 | — | 数值游标（通常是已处理到的主键或时间水位） |
| `value_str` | `string` | 4 | — | 字符串游标（如索引别名、分区名） |
| `version` | `int64` | 5 | — | CAS 版本，SaveCheckpoint 必须回传 expected_version |
| `operator` | `string` | 6 | — | — |
| `ctime` | `int64` | 7 | — | — |
| `mtime` | `int64` | 8 | — | — |

### message `TaskAudit`

> TaskAudit 任务变更审计（cron_task_audit 的对外投影）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `task_key` | `string` | 2 | — | — |
| `action` | `string` | 3 | — | register/update/pause/resume/disable/trigger/retry/replay |
| `from_state` | `string` | 4 | — | — |
| `to_state` | `string` | 5 | — | — |
| `operator` | `string` | 6 | — | — |
| `detail` | `string` | 7 | — | 变更摘要 JSON 文本，不含密钥 |
| `trace_id` | `string` | 8 | — | — |
| `ctime` | `int64` | 9 | — | — |

### message `RegisterTaskReq`

> --- 任务定义注册与查询 --- / RegisterTaskReq 注册（首次创建）任务定义。 / 幂等：以 task_key 为唯一键，同 key 重复注册返回已存在错误，避免误覆盖线上调度； / 修改已存在的定义必须走 UpdateTask 并携带 expected_version。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`TaskDefinition`](#message-taskdefinition) | 1 | — | 至少提供 task_key/name/handler/schedule 与超时/重试策略 |
| `idempotency_key` | `string` | 2 | — | 必填：调用方保证同一次注册动作唯一 |
| `operator` | `string` | 3 | — | — |
| `trace_id` | `string` | 4 | — | — |

### message `RegisterTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`TaskDefinition`](#message-taskdefinition) | 1 | — | — |
| `created` | `bool` | 2 | — | false 表示 task_key 已存在且 definition 与线上一致（幂等重入） |
| `dedupe_reason` | `string` | 3 | — | created=false 时说明命中了哪条幂等约束 |

### message `UpdateTaskReq`

> UpdateTaskReq 修改任务定义（含调度、超时、重试策略与参数）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `definition` | [`TaskDefinition`](#message-taskdefinition) | 2 | — | 只允许改 name/handler/task_group/schedule/timeout/retry/ |
| `expected_version` | `int64` | 3 | — | concurrency/lease_ttl/misfire/params/secret_refs/owner； / task_key/state/version 以服务端为准，忽略请求值 / 乐观锁：与服务端当前 version 不一致时返回冲突错误 |
| `operator` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `UpdateTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`TaskDefinition`](#message-taskdefinition) | 1 | — | — |

### message `GetTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |

### message `GetTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`TaskDefinition`](#message-taskdefinition) | 1 | — | — |

### message `ListTasksReq`

> ListTasksReq 任务定义分页（配置类小表，游标按 task_key 升序）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `state` | [`TaskState`](#enum-taskstate) | 1 | — | 未指定表示全部 |
| `task_group` | `string` | 2 | — | 空表示全部 |
| `handler` | `string` | 3 | — | 空表示全部 |
| `cursor` | `string` | 4 | — | 上一页 next_cursor，空表示第一页 |
| `page_size` | `int32` | 5 | — | 0 表示服务端默认值，超过上限返回 ErrPsTooLarge |

### message `ListTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`TaskDefinition`](#message-taskdefinition) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | 空表示没有更多 |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `PauseTaskReq`

> PauseTaskReq 暂停任务：不产生新的计划触发，正在跑的执行不中断。 / 幂等：重复暂停同一任务返回 changed=false，不重复写审计。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `reason` | `string` | 2 | — | 必填：审计要求可追溯 |
| `expected_version` | `int64` | 3 | — | — |
| `idempotency_key` | `string` | 4 | — | — |
| `operator` | `string` | 5 | — | — |
| `trace_id` | `string` | 6 | — | — |

### message `ResumeTaskReq`

> ResumeTaskReq 恢复暂停的任务。 / 恢复时如何处理暂停期间错过的计划点，由任务的 MisfirePolicy 决定。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `expected_version` | `int64` | 2 | — | — |
| `idempotency_key` | `string` | 3 | — | — |
| `operator` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `DisableTaskReq`

> DisableTaskReq 停用任务（终态）。历史执行记录与游标保留，供审计与回溯。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `reason` | `string` | 2 | — | — |
| `expected_version` | `int64` | 3 | — | — |
| `idempotency_key` | `string` | 4 | — | — |
| `operator` | `string` | 5 | — | — |
| `trace_id` | `string` | 6 | — | — |

### message `TaskOperationReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `definition` | [`TaskDefinition`](#message-taskdefinition) | 1 | — | — |
| `changed` | `bool` | 2 | — | false 表示目标状态已达成（幂等重入） |
| `audit_id` | `int64` | 3 | — | 本次写入的 cron_task_audit 行 |

### message `TriggerTaskReq`

> TriggerTaskReq 立即触发一次执行（planned_at = 服务端当前时间截断到秒）。 / 同秒内重复触发由 (task_key, planned_at, attempt) 唯一键挡住，返回已存在的 run。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `params` | `string` | 2 | — | 覆盖本次执行的参数 JSON 文本，空表示用定义中的 params |
| `planned_at` | `int64` | 3 | — | 0 表示服务端当前时间；显式传入即“补跑某个计划时刻” |
| `idempotency_key` | `string` | 4 | — | 必填 |
| `operator` | `string` | 5 | — | — |
| `trace_id` | `string` | 6 | — | — |

### message `TriggerTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run` | [`RunRecord`](#message-runrecord) | 1 | — | — |
| `created` | `bool` | 2 | — | — |

### message `ListDueTasksReq`

> --- 到期扫描与租约 --- / ListDueTasksReq 供本服务 worker（或独立调度进程）拉取到期任务。 / 只读语义：不产生执行记录，也不占用租约，claim 由 AcquireLease 完成。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `now` | `int64` | 1 | — | 0 表示服务端当前时间 |
| `limit` | `int32` | 2 | — | 单轮最多返回多少个到期任务，0 表示服务端默认值 |
| `task_group` | `string` | 3 | — | 空表示全部分组 |
| `lookahead_seconds` | `int32` | 4 | — | 允许预取的提前量，0 表示只返回已到期项 |

### message `DueTask`

> DueTask 一个可执行的计划点。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `planned_at` | `int64` | 2 | — | 计划时刻，claim 时的幂等身份 |
| `handler` | `string` | 3 | — | — |
| `params` | `string` | 4 | — | — |
| `timeout_seconds` | `int32` | 5 | — | — |
| `max_attempts` | `int32` | 6 | — | — |
| `lease_ttl_seconds` | `int32` | 7 | — | — |
| `concurrency_limit` | `int32` | 8 | — | — |
| `running_count` | `int64` | 9 | — | 当前 RUNNING 的执行数，供调度端提前退避 |
| `task_group` | `string` | 10 | — | — |

### message `ListDueTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`DueTask`](#message-duetask) | 1 | repeated | — |
| `server_time` | `int64` | 2 | — | 调度端据此校正本地时钟，避免各实例时钟漂移 |

### message `AcquireLeaseReq`

> AcquireLeaseReq 抢占某个计划时刻的执行权。 / 事务内：CAS cron_task_lease（过期即可抢占，fence_token+1）+ INSERT cron_task_run / （UNIQUE(task_key, planned_at, attempt) 保证同计划时刻单赢家）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `planned_at` | `int64` | 2 | — | 必填：计划时刻 |
| `attempt` | `int32` | 3 | — | 0/1 表示首次；>1 必须是服务端已有的 RETRYING 重试号 |
| `scope` | `string` | 4 | — | 任务级锁的分片键，空表示按 task_key 全局互斥 |
| `owner` | `string` | 5 | — | 实例标识，必填（hostname-pid-random） |
| `ttl_seconds` | `int32` | 6 | — | 0 表示使用任务定义的 lease_ttl_seconds |
| `trigger_type` | [`TriggerType`](#enum-triggertype) | 7 | — | — |
| `trace_id` | `string` | 8 | — | — |

### message `AcquireLeaseReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `outcome` | [`LeaseOutcome`](#enum-leaseoutcome) | 1 | — | — |
| `run_id` | `int64` | 2 | — | outcome=ACQUIRED/ALREADY_CLAIMED 时有效 |
| `fence_token` | `int64` | 3 | — | 后续 RenewLease/ReportTaskResult 必须回传 |
| `lease_expire_at` | `int64` | 4 | — | — |
| `server_time` | `int64` | 5 | — | — |
| `message` | `string` | 6 | — | 被拒绝时的人读原因 |

### message `RenewLeaseReq`

> RenewLeaseReq 心跳续租。fence_token 不一致说明租约已被抢占， / 服务端返回 ErrLeaseLost，调用方必须立刻放弃本次执行（不再写下游）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run_id` | `int64` | 1 | — | — |
| `owner` | `string` | 2 | — | — |
| `fence_token` | `int64` | 3 | — | — |
| `ttl_seconds` | `int32` | 4 | — | 0 表示沿用原 TTL |

### message `RenewLeaseReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease_expire_at` | `int64` | 1 | — | — |
| `server_time` | `int64` | 2 | — | — |

### message `ReleaseLeaseReq`

> ReleaseLeaseReq 释放租约并终结执行记录（ReportTaskResult 已隐式释放， / 本方法用于「领取后决定不执行」的分支，避免租约空转到过期）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run_id` | `int64` | 1 | — | — |
| `owner` | `string` | 2 | — | — |
| `fence_token` | `int64` | 3 | — | — |
| `final_state` | [`RunState`](#enum-runstate) | 4 | — | 只能是 RUN_STATE_SKIPPED 或 RUN_STATE_CANCELED |
| `reason` | `string` | 5 | — | — |

### message `ReportTaskResultReq`

> ReportTaskResultReq 上报执行结果。 / 幂等：重复上报同一 run_id 的终态返回 first_reported=false 且不改变已有终态； / 只有持有当前 fence_token 的实例可以写入。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run_id` | `int64` | 1 | — | — |
| `owner` | `string` | 2 | — | — |
| `fence_token` | `int64` | 3 | — | — |
| `state` | [`ReportState`](#enum-reportstate) | 4 | — | — |
| `result_summary` | `string` | 5 | — | 处理器结果摘要（禁止写入大对象、事件原文或敏感字段） |
| `error_message` | `string` | 6 | — | 失败原因，截断保存 |
| `checkpoint` | [`Checkpoint`](#message-checkpoint) | 7 | — | 非空时在同一个事务里 CAS 推进游标 |
| `trace_id` | `string` | 8 | — | — |

### message `ReportTaskResultReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run` | [`RunRecord`](#message-runrecord) | 1 | — | 上报后的最终状态 |
| `first_reported` | `bool` | 2 | — | false 表示该 run 已是终态，本次上报被幂等丢弃 |
| `checkpoint_advanced` | `bool` | 3 | — | 游标是否被本次上报推进 |
| `next_retry_at` | `int64` | 4 | — | state=RETRYING 时的下次可执行时间 |
| `next_attempt` | `int32` | 5 | — | 0 表示不再有重试机会 |

### message `GetTaskRunReq`

> --- 执行记录查询与人工干预 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run_id` | `int64` | 1 | — | — |

### message `GetTaskRunReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run` | [`RunRecord`](#message-runrecord) | 1 | — | — |

### message `ListTaskRunsReq`

> ListTaskRunsReq 执行记录分页（按 (planned_at, run_id) 倒序游标）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | 空表示全部任务 |
| `state` | [`RunState`](#enum-runstate) | 2 | — | 未指定表示全部 |
| `planned_from` | `int64` | 3 | — | 计划时刻下界（含） |
| `planned_to` | `int64` | 4 | — | 计划时刻上界（含），0 表示不限 |
| `cursor` | `string` | 5 | — | — |
| `page_size` | `int32` | 6 | — | — |

### message `ListTaskRunsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`RunRecord`](#message-runrecord) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `RetryRunReq`

> RetryRunReq 人工重试某条已终结（FAILED/TIMEOUT）的执行。 / 语义：在同一 (task_key, planned_at) 下追加 attempt = 历史最大 + 1 的新执行记录， / 因此处理器的幂等上下文不变，重放不会重复产生副作用。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run_id` | `int64` | 1 | — | — |
| `idempotency_key` | `string` | 2 | — | 必填 |
| `operator` | `string` | 3 | — | — |
| `reason` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `RetryRunReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `run` | [`RunRecord`](#message-runrecord) | 1 | — | 新建的重试运行 |
| `created` | `bool` | 2 | — | — |

### message `GetCheckpointReq`

> --- 游标 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `scope_key` | `string` | 2 | — | — |

### message `GetCheckpointReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `checkpoint` | [`Checkpoint`](#message-checkpoint) | 1 | — | — |
| `found` | `bool` | 2 | — | false 表示从未推进过游标，checkpoint 字段无效 |

### message `ListCheckpointsReq`

> ListCheckpointsReq 游标分页（按 (task_key, scope_key) 升序游标）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `cursor` | `string` | 2 | — | — |
| `page_size` | `int32` | 3 | — | — |

### message `ListCheckpointsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`Checkpoint`](#message-checkpoint) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `SaveCheckpointReq`

> SaveCheckpointReq 独立推进游标（不等执行结束）。 / expected_version：0 表示要求「尚不存在」，>0 表示 CAS；不一致返回 ErrCheckpointConflict。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `checkpoint` | [`Checkpoint`](#message-checkpoint) | 1 | — | — |
| `expected_version` | `int64` | 2 | — | — |
| `idempotency_key` | `string` | 3 | — | — |
| `operator` | `string` | 4 | — | — |
| `trace_id` | `string` | 5 | — | — |

### message `SaveCheckpointReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `checkpoint` | [`Checkpoint`](#message-checkpoint) | 1 | — | — |
| `advanced` | `bool` | 2 | — | — |

### message `GetLeaseReq`

> --- 租约查询 ---

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `scope` | `string` | 2 | — | — |

### message `GetLeaseReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `lease` | [`LeaseInfo`](#message-leaseinfo) | 1 | — | — |
| `found` | `bool` | 2 | — | — |

### message `ListLeasesReq`

> ListLeasesReq 租约分页（按 expire_at 升序，方便先看快过期的）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | 空表示全部 |
| `only_expired` | `bool` | 2 | — | true 表示只返回已过期可抢占的租约 |
| `now` | `int64` | 3 | — | — |
| `cursor` | `string` | 4 | — | — |
| `page_size` | `int32` | 5 | — | — |

### message `ListLeasesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`LeaseInfo`](#message-leaseinfo) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `ListTaskAuditsReq`

> --- 审计与健康度 --- / ListTaskAuditsReq 任务变更审计分页（按 id 倒序游标）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_key` | `string` | 1 | — | — |
| `action` | `string` | 2 | — | — |
| `ctime_from` | `int64` | 3 | — | — |
| `ctime_to` | `int64` | 4 | — | — |
| `cursor` | `string` | 5 | — | — |
| `page_size` | `int32` | 6 | — | — |

### message `ListTaskAuditsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `list` | [`TaskAudit`](#message-taskaudit) | 1 | repeated | — |
| `next_cursor` | `string` | 2 | — | — |
| `has_more` | `bool` | 3 | — | — |
| `total` | `int64` | 4 | — | — |

### message `GroupHealth`

> GroupHealth 单个分组的调度健康度。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_group` | `string` | 1 | — | — |
| `enabled_tasks` | `int32` | 2 | — | — |
| `paused_tasks` | `int32` | 3 | — | — |
| `due_backlog` | `int32` | 4 | — | 已过期但尚未产生执行记录的计划任务数 |
| `running` | `int32` | 5 | — | — |
| `retrying` | `int32` | 6 | — | — |
| `failed_last_hour` | `int32` | 7 | — | — |
| `expired_leases` | `int32` | 8 | — | 租约过期未释放的数量（实例崩溃线索） |
| `oldest_due_planned_at` | `int64` | 9 | — | 最老积压计划时刻，0 表示无积压 |

### message `GetSchedulerHealthReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `now` | `int64` | 1 | — | — |
| `task_group` | `string` | 2 | — | 空表示全部分组 |

### message `GetSchedulerHealthReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `server_time` | `int64` | 1 | — | — |
| `groups` | [`GroupHealth`](#message-grouphealth) | 2 | repeated | — |
| `version` | `string` | 3 | — | 服务构建版本，便于排障定位 |
