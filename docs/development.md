# 开发文档

## 1. 开发环境

建议使用 Go 1.25.x、与 go-zero 版本匹配的 goctl、Docker Desktop 和 FFmpeg（仅开发转码/直播媒体时需要）。本项目支持 Windows、Linux、macOS；脚本优先提供 PowerShell，同时保持 CI Linux 可执行。

所有命令统一见 [commands.md](commands.md)；本文件只说明开发流程和约束，不重复维护命令清单。

## 2. 初始化和依赖

使用 `commands.md` 中的依赖检查和模块初始化命令。

本项目不把 `openbilibili-go-common` 作为 Go 依赖，它只是本地设计参考目录；不得从其 vendor 目录复制旧版框架代码。完整安装、模块和工具命令见 [commands.md](commands.md)。

## 3. 本地启动顺序

1. 启动 `deploy/docker-compose` 中的 MySQL、Redis、消息队列、OpenSearch、MinIO。
2. 执行数据库迁移和种子数据；每个服务的迁移必须可重复执行。
3. 第一阶段的 `identity/content/media/community/moderation` 是部署合并单元，逻辑目录仍分别保留；实现时由部署配置决定合并进程或独立进程。
4. 当前骨架只有 `gateway/app`、`gateway/admin`、`account` 和 `user-profile` 提供可运行入口；其它服务先按各自 README 和 go-zero 模板实现，再加入启动编排。
5. 最后启动 `cron`、事件消费者和 SPM；异步积压不能阻塞核心 API 启动。

使用 `commands.md` 中的本地依赖和服务启动命令。

服务启动必须提供健康检查和 readiness（gateway/app 当前为 `/api/healthz`，gateway/admin 为 `/admin/healthz`，后续可增加 `/api/readyz`）或等价 RPC 健康检查；readiness 只在必要依赖可用后通过。

## 4. go-zero 代码生成

### HTTP API

API 文件放在服务自己的 `api/`；生成目录不要覆盖手写业务代码：

使用 `scripts/gen.ps1` 或 `commands.md` 中的 API 生成命令；禁止直接手写 handler、路由和 types。

### RPC

protobuf 放在服务自己的 `rpc/` 或顶层 `api/proto/`（仅跨服务共享契约）：

使用 `scripts/gen.ps1` 或 `commands.md` 中的 RPC 生成命令；禁止手写 RPC client/server 和 `.pb.go`。

命令随 goctl 版本略有差异，生成前确认 `protoc`、`protoc-gen-go`、`protoc-gen-go-grpc` 兼容。生成文件要纳入版本控制，或在 CI 中使用固定工具链重新生成并比较结果。

## 5. 服务实现约定

- `handler` 负责请求、校验、用户上下文和响应；`logic` 负责用例、权限、状态机、事务和事件；`repository` 负责 SQL/Redis/OSS。
- `server` 只做 RPC 适配；`consumer` 负责事件去重、重试和死信；业务写入仍调用本服务逻辑。
- `svc.ServiceContext` 集中注入 DB、Redis、MQ、OSS、RPC client、配置和 tracer。
- `model` 只包含本服务实体；跨服务返回 DTO/protobuf，不共享数据库 model。
- 所有外部输入限制大小、格式、分页和超时；远程调用设置 deadline，重试区分幂等与非幂等。

## 6. 数据库和迁移

- 数据库按服务归属划分；初期可以同一 MySQL 实例的不同 schema，禁止跨 schema 写入。
- 迁移按服务、递增版本命名，例如 `deploy/migrations/content/000001_create_submission.sql`。
- 每次迁移写明 forward、rollback（若可逆）和锁风险；大表变更使用在线迁移。
- 先索引、读副本和归档，再考虑分库分表；不能无容量指标预设 8/16 个分片。

## 7. 事件和异步任务

事务性业务表与 Outbox 同库提交，发布器负责投递。消费者必须校验 schema、按 `event_id` 去重、记录处理状态和 trace_id，达到重试上限后进入死信并支持重放。

SPM 事件只用于用户行为分析和视频推荐，不携带广告位或商业化字段。播放心跳要采样、批量发送和脱敏，不能每个心跳同步写 MySQL。

## 8. 测试

使用 `commands.md` 中的测试、vet、格式化和契约检查命令。

测试覆盖状态机、权限、幂等、事件构造、Repository 集成、API/RPC 契约、重复消费、乱序、重试、死信、上传中断、转码失败和播放资产缺失。

## 9. 提交和评审

提交信息建议使用：`feat(content): ...`、`fix(media): ...`、`docs: ...`、`test(spm): ...`。一个提交尽量只包含一个领域和目的。评审说明必须包含接口/数据库/事件影响、迁移回滚、测试结果和未完成事项。
