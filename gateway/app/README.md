# gateway/app

面向 Android、iOS、HarmonyOS、桌面客户端的 BFF 网关。属于 [gateway](..) 的独立子服务之一。

## 职责

- 对外公网 HTTP 入口，路由前缀 `/api`、`/account`、`/x/member`。
- 用户 access_token 校验、设备/客户端版本上下文、限流、风控、灰度和 trace_id。
- 面向端的 BFF 聚合和字段裁剪；不拥有用户、视频、评论等领域主数据。
- 长任务（上传、转码、审核、推荐计算）返回任务 ID，不在网关请求内同步等待。

## 边界

- 可以调用领域 RPC，不能直接读业务数据库或拼接数据库 model。
- 下游错误映射为稳定的公共错误码（见 [common/httpresponse](../../common/httpresponse)），不泄露 SQL、Token、对象存储签名。
- HTTP 响应统一使用四字段信封（`code`/`message`/`data`/`ttl`）。

## 路由

| 前缀 | 说明 | 下游 |
|---|---|---|
| `/api` | 健康检查 `GET /api/healthz` | - |
| `/account` | 账号聚合查询：`/info`、`/infos`、`/info/by/name`、`/card`、`/cards`、`/profile`、`/profile/stat`、`/vip`、`/vips`、`/privacy`（白名单 appkey 中间件） | account gRPC |
| `/account/v1` | 老客户端兼容路由：`/info`、`/infos`、`/card`、`/vip`（老字段名转换） | account gRPC |
| `/account/v2` | `/myinfo`、`/userinfo`（V2MyInfo 转换） | account gRPC |
| `/x/member` | 用户资料客户端路由：`/web/account`、`/base`、`/batchBase`、`/moral`、`/web/moral/log`、`/exp`、`/level`、`/official`、`/web/exp/log`、`/web/exp/reward` | user-profile gRPC |
| `/x/member/app/*` | 资料编辑：`/app/uname/update`、`/app/sign/update`、`/app/sex/update`、`/app/birthday/update`、`/app/face/update`、`/web/update` | user-profile gRPC |
| `/x/member/realname/*` | 实名认证：`/status`、`/info`、`/apply/status`、`/tel/capture`、`/tel/capture/check`、`/apply`、`/adult`、`/check` | user-profile gRPC |
| `/x/passport-login` | 登录：`GET /key`（RSA 公钥）、`POST /web/login`（密码/验证码）、`POST /web/captcha/send`、`POST /web/captcha/check`、`POST /web/register`、`POST /exit`、`POST /token/renew`、`GET /web/cookie/info`、`GET /web/token/info` | account gRPC |

路由风格参考 openbilibili-go-common 的 account-interface（`/x/member/*`）与
account 服务原 HTTP 面（`/account/*`）；网关只做 RPC 聚合与响应字段转换，
不承载业务规则（v1/v2 字段转换规则移植自参考仓库 server/http/v1.go、v2.go）。

## 配置

`etc/app.yaml`：`AccountRPC`（etcd key `account.v1.rpc`）、`UserProfileRPC`
（etcd key `user-profile.v1.rpc`）、`PrivacyAppKeys`（`/account/privacy` 白名单）。

## 生成与运行

```powershell
# 生成（从仓库根目录；框架文件由 goctl 生成，logic 骨架内只填聚合逻辑）
goctl api validate -api gateway/app/api/app.api
goctl api go -api gateway/app/api/app.api -dir gateway/app

# 运行
go run ./gateway/app -f gateway/app/etc/app.yaml

# 健康检查
Invoke-WebRequest http://127.0.0.1:8080/api/healthz -UseBasicParsing
```
