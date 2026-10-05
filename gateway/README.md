# gateway

对外统一 HTTP/WebSocket 入口层，按客户端类型拆分为两个独立 go-zero API 子服务，各自独立构建、配置、测试和发布。遵循 [AGENTS.md §3](../AGENTS.md) 的目录边界与「每个服务子目录可独立构建、配置、测试和发布」原则。

## 子服务

| 子服务 | 客户端 | 路由前缀 | 鉴权 | 默认端口 | 部署 |
|---|---|---|---|---|---|
| [app](app) | Android、iOS、HarmonyOS、桌面客户端 | `/api` | 用户 access_token、设备/客户端版本上下文 | 8080 | 对外公网，高并发 |
| [admin](admin) | 管理后台 Web | `/admin` | 管理员账号 + RBAC、IP 白名单 | 8081 | 内网/VPN，低并发 |

当前不支持小程序专用路由、SDK 或数据模型（见 [AGENTS.md §1](../AGENTS.md)）。

## 职责边界

两个子服务都只做：
- HTTP/WebSocket 路由、TLS、CORS、请求大小限制和协议适配。
- access token 校验、设备/客户端版本上下文、限流、风控、灰度和 trace_id。
- 面向端的 BFF 聚合和字段裁剪；不拥有用户、视频、评论等领域主数据。

`gateway/admin` 仅做入口聚合（路由、鉴权、限流、响应聚合）；管理后台的业务逻辑、RBAC 规则、运营配置、审计落库全部由 [services/operation](../services/operation) 等领域服务承担。`gateway/admin` 不直接读写业务数据库。

## 约束

- 网关可以调用领域 RPC/API，不能直接读业务数据库或拼接数据库 model。
- 下游错误映射为稳定的公共错误码（见 [common/httpresponse](../common/httpresponse)），不泄露 SQL、Token、对象存储签名。
- 长任务（上传、转码、审核、推荐计算）返回任务 ID，不在网关请求内同步等待。
- HTTP 响应统一使用 [common/httpresponse](../common/httpresponse) 的四字段信封（`code`/`message`/`data`/`ttl`）。

## 目录结构

```text
gateway/
├── app/                        # 面向终端客户端的 BFF 网关
│   ├── api/app.api
│   ├── etc/app.yaml
│   ├── internal/{config,handler,logic,middleware,svc,types}/
│   ├── app.go
│   └── README.md
├── admin/                      # 面向管理后台 Web 的入口聚合网关
│   ├── api/admin.api
│   ├── etc/admin.yaml
│   ├── internal/{config,handler,logic,middleware,svc,types}/
│   ├── admin.go
│   └── README.md
└── README.md
```

## 拆分理由

1. **鉴权模型不同**：app 走用户会话 access_token，admin 走管理员账号 + RBAC；共用中间件会导致复杂分支。
2. **限流/风控策略不同**：app 面向公网高并发需激进限流和风控，admin 内网低并发更关注操作审计。
3. **部署特性不同**：app 对公网暴露需 CDN/WAF，admin 仅内网/VPN 访问；独立部署便于各自扩展和升级。
4. **故障隔离**：admin 网关故障不影响终端用户访问；app 网关故障不影响运营后台。

## 生成与运行

详见 [docs/commands.md](../docs/commands.md)。要点：

```powershell
# 生成（从仓库根目录）
goctl api go -api gateway/app/api/app.api -dir gateway/app
goctl api go -api gateway/admin/api/admin.api -dir gateway/admin

# 运行
go run ./gateway/app -f gateway/app/etc/app.yaml
go run ./gateway/admin -f gateway/admin/etc/admin.yaml
```

当前两个子服务都提供 `GET /api/healthz`（app）和 `GET /admin/healthz`（admin）健康检查；接入生产依赖后增加 readiness 检查。

## 测试覆盖

两个子服务的**明细**各自登记在 [app/README.md](app/README.md) 与 [admin/README.md](admin/README.md)
的「测试覆盖」节（用例清单、替身口径、缺口分组名单都在那里）。本节只做一次汇总，并写明口径。

| 对象 | logic 用例（顶层/子） | 其他有测试的层 | logic 构造器级覆盖 |
|---|---|---|---|
| `gateway/app` | 13 个文件 `117/11` | `internal/config` `2/1` | **`75/198`**（123 个没有） |
| `gateway/admin` | 24 个文件 `402/44` | `internal/middleware` `10/2`、`internal/config` `1/1` | **`229/313`**（84 个没有） |
| 两个网关合计 | `519/55` | 合计 `532/59` | **`304/511`**（207 个没有） |

口径与边界（读者必须知道的三件事）：

1. **网关用例是「client 接口打桩 + 回复投影断言」**：内嵌 goctl 生成的 RPC client 接口、只覆盖
   本用例需要的方法，不建 gRPC 连接、不起 server、不碰数据库。它们证明的是「HTTP↔RPC 的装配与
   投影」，**永远不等于**下游领域服务的覆盖——同一条业务判定（状态机、配额、金额、幂等、权限授予、
   隐私级别）的证明只存在于 `services/<domain>` 自己的测试里。看网关数字判断线上行为可用性是错的。
2. **缺口是真实存在且已列名**：`gateway/app` 的 123 个无构造器级用例的方法集中在上传/投稿、
   账号与会话与老客户端读侧、资料写侧与成长值、实名认证、搜索、弹幕、站内信、关系与动态、
   版权目录、播放，以及商业化终端面的会员/钱包/投币/订单；`gateway/admin` 的 84 个集中在
   直播 ingest/网关面、operation 后台自身面、用户运营、版权目录与窗口、风控、媒资与转码、审核、
   索引运营、稿件。完整逐个名单按域分组的表格在上述两份 README 的 §1。
3. **没有测试的层**：`internal/handler`、`internal/types`、`internal/svc` 两侧均无离线单测
   （goctl/protoc 生成壳与客户端装配不在纯单测可达范围）；`gateway/app` 的 `internal/middleware`
   也**没有**用例（access token 校验、限流、风控、灰度无单测）。`gateway/admin` 的鉴权侧有 3 个文件：
   `AdminPermission` 中间件判定口径 + 两条权限点漂移门禁（`routePermissions` ↔ 生成的 `routes.go`、
   ↔ `op_permission` 种子迁移）——它们是**静态集合的双向比对**，不验证 `VerifyAdminPermission` 的实际结果。

网关层不拥有数据、没有 `deploy/migrations` 下的迁移，因此不存在「迁移 SQL ↔ 真实库列级对账」这一层
验证；这个结论只说明网关无库可验，不代表下游服务已在隔离实例复验过。端到端（真实 HTTP 请求 →
真实下游 RPC → 结果可用）从未在网关目录内验证。验证命令：

```powershell
go test -p 1 -count=1 ./gateway/app/... ./gateway/admin/...   # -p 1 必须带：Windows 页面文件限制，并发跑多个测试包会 OOM(errno=1455)
gofmt -l gateway                                              # 必须为空
go vet ./gateway/...                                          # 应无输出
```

两个子服务各自 0 条 skip；全仓处于 skip 状态的用例都在领域服务侧（`live-gateway` 3、`live-ingest` 1、`notification` 1）。
