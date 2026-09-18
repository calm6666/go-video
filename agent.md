# Go-Video Agent 开发说明

本文件是 `AGENTS.md` 的入口别名，供使用 `agent.md` 约定的工具和开发者快速阅读；完整强制规则以 [AGENTS.md](AGENTS.md) 为准。

## 必须遵守的范围

- 使用 Go + go-zero，保持 `gateway/`、`services/`、`common/`、`api/`、`deploy/`、`scripts/`、`docs/` 单仓结构。
- 支持 Android、iOS、HarmonyOS 和电脑客户端；管理后台使用 Web；不支持小程序。
- 实现视频投稿、版权目录、上传/转码/播放、直播、评论、弹幕、关系链、动态、搜索、推荐、SPM、通知和运营后台。
- 不实现会员、支付、投币、广告、广告分析、创作者分成等商业化能力。

## 修改前

1. 阅读 `AGENTS.md` 和对应的 `docs/*.md`。
2. 检查工作区和已有修改，不覆盖用户内容。
3. 找到数据所有者，确认 API/RPC、事件、数据库和权限影响。
4. 文档、配置和源码使用 UTF-8。

## go-zero 规则与代码生成纪律

- 业务服务统一放在 `services/<service>`，按 `api`、`rpc`、`model`、`etc`、`internal/{config,handler,logic,server,consumer,repository,svc,types}` 组织。
- **所有服务目录中的框架代码（除业务逻辑及明确允许的业务扩展外）必须使用 goctl/protoc 生成或更新，严禁手写或直接修改生成文件。** 修改 `.api`/`.proto` 后，执行 `docs/commands.md` 的增量更新命令。
- 所有非业务逻辑的 go-zero 框架代码必须用 goctl 生成或更新，禁止手写：包括 handler、路由、types、RPC client/server、ServiceContext、配置骨架和入口模板。
- HTTP API 使用 `.api` 和 goctl 生成；内部 RPC 使用 protobuf/gRPC；不要混用 Kratos/Blademaster 目录。
- `.api`、`.proto`、迁移、事件 schema 和 `internal/logic`/`repository` 等业务扩展可以手写；生成目录和 `.pb.go` 不得手改。
- 修改 API/RPC 源后，必须执行 `scripts/gen.ps1` 重新生成，并检查生成差异；不得修改生成文件来保存业务逻辑。
- `handler` 不写业务规则，`logic` 处理用例，`repository` 访问本服务数据库，`consumer` 处理异步消息。
- gateway 拆分为 `gateway/app`（终端客户端 BFF）和 `gateway/admin`（管理后台入口聚合）两个独立子服务，只做入口和聚合，不直连领域数据库；管理后台业务逻辑放 `services/operation`；任务服务放 `services/cron`。

## 数据和事件规则

- 一个领域一个写入者；禁止跨服务直连 MySQL、Redis 业务 key 或内部 model。
- 业务事务使用 Outbox；事件包含 `event_id`、`event_type`、`schema_version`、`occurred_at`、`trace_id` 和业务主键。
- 消费者必须幂等、可重试、可进死信；高频计数异步聚合并可重算。
- SPM 只分析用户播放、点击、搜索、跳过、点赞、收藏、关注和分享行为，为视频推荐生成特征；不做广告分析。

## HTTP 响应规范

- 所有 HTTP API 成功和业务错误统一返回 `{"code":0,"message":"ok","data":{},"ttl":0}` 形状。
- `code=0` 表示成功；`message` 成功固定为 `ok`；`data` 必须是接口契约定义的对象，无数据使用空对象；`ttl` 使用秒，`0` 表示不缓存。
- 成功响应结构写入 `.api` 并由 goctl 生成；错误响应通过 `common/httpresponse` 和 go-zero `httpx` 统一处理，禁止 handler 手写另一套 JSON。

## 完成前

- 更新相关 README/开发文档、API/RPC/事件说明和迁移脚本。
- 至少运行 [docs/commands.md](docs/commands.md) 中的测试、vet 和契约检查命令。
- 检查健康检查、日志 trace_id、超时、鉴权、幂等和回滚方案。
- 交付时说明真实完成项、验证命令、风险和未完成事项。
