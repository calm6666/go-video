# gateway/admin

面向管理后台 Web 的入口聚合网关。属于 [gateway](..) 的独立子服务之一。

## 职责

- 管理后台 HTTP 入口，路由前缀 `/admin`、`/x/member`（运营路由）。
- 管理员账号鉴权 + RBAC、IP 白名单、限流、操作审计、trace_id。
- 面向后台的响应聚合；不拥有用户、视频、评论等领域主数据。

## 边界

- **仅做入口聚合**（路由、鉴权、限流、响应聚合）；管理后台的业务逻辑、RBAC 规则、运营配置、审计落库全部由 [services/operation](../../services/operation) 等领域服务承担。
- `gateway/admin` 不直接读写业务数据库，不拥有领域主数据。
- 可以调用领域 RPC（尤其是 operation），不能拼接数据库 model。
- 下游错误映射为稳定的公共错误码（见 [common/httpresponse](../../common/httpresponse)），不泄露 SQL、Token、对象存储签名。
- HTTP 响应统一使用四字段信封（`code`/`message`/`data`/`ttl`）。
- 仅内网/VPN 访问，不对公网暴露。

## 路由

| 前缀 | 说明 | 下游 |
|---|---|---|
| `/admin` | 健康检查 `GET /admin/healthz` | - |
| `/admin/account` | 缓存运营：`GET /cache/del`（mid/modifiedAttr）、`POST /cache/clear`（JSON 消息） | account `DelCache` RPC |
| `/x/member` | 用户运营：`POST /morals/update`、`POST /moral/update`、`POST /moral/undo`、`POST /exp/set`、`POST /exp/update`、`POST /property/review/add`、`GET /realname/stripped/info`、`GET /realname/mid/by/card`、`GET /web/login/log`（登录日志） | user-profile / account gRPC |

路由风格参考参考仓库 member 服务的内部运营 HTTP 面（`/x/internal/member/*`），
按本仓库架构迁移到管理后台网关并以 RPC 聚合。

## 配置

`etc/admin.yaml`：`AccountRPC`（etcd key `account.v1.rpc`）、`UserProfileRPC`
（etcd key `user-profile.v1.rpc`）。

## 生成与运行

```powershell
# 生成（从仓库根目录；框架文件由 goctl 生成，logic 骨架内只填聚合逻辑）
goctl api validate -api gateway/admin/api/admin.api
goctl api go -api gateway/admin/api/admin.api -dir gateway/admin

# 运行
go run ./gateway/admin -f gateway/admin/etc/admin.yaml

# 健康检查
Invoke-WebRequest http://127.0.0.1:8081/admin/healthz -UseBasicParsing
```
