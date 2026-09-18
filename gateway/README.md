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
