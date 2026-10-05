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
4. `gateway/app`、`gateway/admin` 与全部 43 个领域服务都已有可运行入口（gRPC；两个网关另有 HTTP 面），
   清单与端口见 docs/roadmap.md 的“实现进度”表和 `commands.md` §7。
   ⚠ 两点必须区分：全部入口仅通过编译、单测与迁移脚本级验证，**没有做过端到端联调**（见 docs/roadmap.md 待办）；
   进程能起来、端口能连，不等于「可提供的服务」。2026-10-03 用「每个 `<X>Logic` 类型都有用例直接调用
   `New<X>Logic(`」探针复测：**43 个领域服务 588/588**（logic 层已无按方法零覆盖的服务），2026-10-04 因
   `social-graph` 新增 `RichRelations` 方法，同一探针复测为 **589/589**，
   逐服务数字见各自 README 的「测试覆盖」节与 docs/roadmap.md 的实测表。
   仍要按「未验证」对待的是这三类：① `model` 层只有 23/43 个服务有离线用例，其余 20 个服务的 SQL、
   列名与索引命中没有自动化证明；② 网关聚合层 `gateway/app` 75/198、`gateway/admin` 229/313
   （合计 304/511），且网关用例打桩的是下游 client，不证明下游服务本身；
   ③ 全仓仍有 5 条 `t.Skip` 用例（`live-gateway` 3、`live-ingest` 1、`notification` 1），
   定位逐条登记在对应服务 README 的已知缺口里。
5. 最后启动 `cron`、事件消费者和 SPM；异步积压不能阻塞核心 API 启动。
   `deploy/docker-compose` 目前只编排本地依赖（MySQL、Redis、MinIO、Redpanda、OpenSearch），
   应用进程仍按 `commands.md` §7 逐个 `go run` 启动，没有把 43 个服务写进 compose。

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

### 接口文档 / Postman / RPC 冒烟

`.api` 或 `.proto` 变更后，除 goctl 生成外还要执行 `node scripts/gen-api-docs.mjs`，让 `docs/api/`（按域分组的逐接口文档）、`postman/` 集合和 `scripts/rpc/smoke.*` 与契约同步；这三者都是生成产物，禁止手工编辑。提交前用 `node scripts/gen-api-docs.mjs --check` 校验漂移门禁（`.api`↔`routes.go`、类型与 logic 存在、`form` 入参可编码、内部链接可解析）。命令和门禁口径见 [commands.md](commands.md) §4。

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

目前唯一没按后半句落地的消费者是 `live-media` 的 `live.state.v1` 入站端：它没有位点表与死信表，
达到 `Kafka.MaxRetries` 只写一条 `given_up` 错误日志就提交位点。原因是它的写入动作（整场档位下线）
幂等性由行级 CAS 条件保证，重复投递扫不到行；代价是失败事件不可自动重放，
兜底为到期清扫或运营逐档位下线，登记在 `services/live-media/README.md` 缺口 3。

SPM 事件只用于用户行为分析和视频推荐，不携带广告位或商业化字段。播放心跳要采样、批量发送和脱敏，不能每个心跳同步写 MySQL。

Kafka 的生产者与消费者实现一律放在构建标签后面：生产侧 `liveingest_kafka`、`playback_kafka`、
`livemedia_kafka`、`recommendrecall_kafka`、`upload_kafka`、`video_kafka`（各自的 `internal/publisher`，除 live-ingest
外五者复用 `common/outbox` 的发布循环），
消费侧 `inbox_kafka`、`notification_kafka`、`searchindexer_kafka`、`liveroom_kafka`、`livemedia_kafka`
（各服务的 `internal/consumer`；最后一个是 live-media 的 `live.state.v1` 消费者，与本服务发布器共用同一个标签
和同一个 `Kafka.Enabled` 开关，所以十一条链路只有十个标签名，数链路时不要按标签数）。
默认构建不链接 kq，且在 `Enabled=true` 时启动即显式报错，不静默空转。改动这两侧必须按 `commands.md` 连带标签跑
build/vet/test。文档与 README 里的「已接线」只表示可编译、可静态检查、该包单测通过；本仓库从未做过 broker 联调，
不得写成「事件已打通」或「消费者已上线可用」。

## 8. 测试

使用 `commands.md` 中的测试、vet、格式化和契约检查命令。

测试覆盖状态机、权限、幂等、事件构造、Repository 集成、API/RPC 契约、重复消费、乱序、重试、死信、上传中断、转码失败和播放资产缺失。

## 9. 提交和评审

提交信息建议使用：`feat(content): ...`、`fix(media): ...`、`docs: ...`、`test(spm): ...`。一个提交尽量只包含一个领域和目的。评审说明必须包含接口/数据库/事件影响、迁移回滚、测试结果和未完成事项。

## 10. 文档写法约定

- 中文文档一律 UTF-8。新写的正文与代码注释不使用中文破折号「——」，改用逗号、冒号或括号；
  改动过的行若顺带带着「——」要一并换掉，但**不做全仓的既有文本批量替换**（噪声大，且历史行不因此变错）。
- 列表项统一「`- **粗体标签**：解释」，不使用裸标签行，也不使用冒号收尾的引导行去接表格。
- 每个服务 README 的「测试覆盖」按五组归类：框架生成物（不写测试并说明为什么）、手写 logic、
  `model`/`repository`、`config`/`svc`/`consumer`、替身与断言口径。用例数写明「`_test.go` 文件数 /
  顶层 `Test*` 数」，子测试只点名关键的；未覆盖的层与构造器逐个登记。
- 文档里的数字（路由数、RPC 方法数、用例数、表数、种子点数）必须是当场用命令导出的，不能沿用上一轮的值；
  每条声称能落回源文件（`file:line` 或某个命令的输出）。
- 「证据层级」要写清楚：迁移是否执行过（哪个实例）、事件是否真的投递过、接口是否被调用过。
  没有做到的就写「未」，不要用「已接线」「已就绪」冒充「已验证」。
