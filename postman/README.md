# Postman 接口测试集合

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 文件 | 内容 |
|---|---|
| `go-video-app.postman_collection.json` | 终端面 198 条请求，28 个 folder + 16 个子 folder |
| `go-video-admin.postman_collection.json` | 运营面 313 条请求，32 个 folder + 59 个子 folder |
| `environments/go-video-local.postman_environment.json` | 15 个本地变量，由两个集合实际引用到的 `{{var}}` 反查生成 |

请求条数与 `routes.go` 注册数由同一份 `.api` 派生，脚本用漂移门禁把守（app 198 / admin 313）。

## 分组口径

folder 与 [接口文档索引](../docs/api/README.md) 的分组文件一一对应：顶层 folder = `.api` 里的 `@server prefix`；
同一前缀下有多个 `@server` 段（鉴权域不同）时再拆子 folder，子 folder 名带路由条数与中间件名，例如 `video 域运营路由（2 条 · 免鉴权）`。
**没有把所有接口平铺成一个 folder。**

## 导入

1. Postman → Import → 选两个 collection 与 `environments/go-video-local.postman_environment.json`。
2. 右上角环境切到 `go-video local`。
3. 终端面服务跑在 `:8080`、运营面 `:8081`（端口来自 `gateway/*/etc/*.yaml`）。

## 变量

下表由环境文件本身反查生成，不是手写清单：

| 变量 | 本地默认值 | 类型 |
|---|---|---|
| `adminUrl` | `http://127.0.0.1:8081` | string |
| `admin_token` | （空，需手工回填） | secret |
| `aid` | `1` | string |
| `appUrl` | `http://127.0.0.1:8080` | string |
| `appkey` | `android` | string |
| `asset_id` | `1` | string |
| `bvid` | `BV1xxxxxxxxx` | string |
| `clientIp` | `127.0.0.1` | string |
| `device_id` | `device-demo-001` | string |
| `epid` | `1` | string |
| `mid` | `1` | string |
| `season_id` | `1` | string |
| `task_id` | `1` | string |
| `window_id` | `1` | string |
| `work_id` | `1` | string |

语义提醒：

- `mid` 是 app 面的身份**入参约定**，不是可信凭据（app 网关没有 JWT 中间件）；
- `appkey` 只用于 `/account/privacy` 的 `AppkeyVerify` 白名单，白名单未配置时中间件放行；
- `admin_token` 需先跑 `POST {{adminUrl}}/admin/operation/login` 再手工回填，否则 `AdminPermission` 分组按 fail-closed 拒绝；
- 其余 `*_id` / `aid` / `bvid` 之类是路径与查询主键占位，指向库里真实数据前只会命中「记录不存在」分支。

## 入参编码（决定请求怎么发）

go-zero 的 `httpx.Parse` 对每个请求依次做 `ParseForm`（读 `r.Form`，含 URL 查询串）和 `ParseJsonBody`（只在 Content-Type 含 `json` 时读体），
所以本集合按 `.api` 的标签位置分发参数：`path`→路径段、`form`→查询串、`json`→JSON 体。实测口径：

- urlencoded 请求体**不会**喂给 `json` 字段（`mid=7&name=x` 会报 `field "name" is not set`）；
- `form` 字段可以从查询串取到，POST 也一样（`/m?mid=7` + `{"name":"x"}` 两个字段都能绑定）；
- `form` 标签的结构体数组绑不上（`type mismatch for field "parts"`），这类字段必须用 `json`，
  终端面 `POST /upload/complete` 的分片清单就是按这条修正过的。

集合里的实际分布：

| 集合 | JSON 体 | 只有查询串 | 无入参 |
|---|---|---|---|
| 终端面 | 1 | 189 | 8 |
| 运营面 | 231 | 68 | 14 |

## 请求体与 URL 的其它约定

- 数字型变量在 JSON 模板里**故意不带引号**（如 `"mid": {{mid}}`）：go-zero 的 int64 绑定不接受字符串，
  因此直接 `JSON.parse` 未替换的模板会报错，Postman 先替换变量再发送，实际发出的报文是合法 JSON。
- 幂等键（`biz_no`/`idempotency_key`/`out_trade_no` 等）用 Postman 内置 `{{$uuid}}`，`X-Trace-Id` 用 `{{$guid}}`，每次运行都不同。
- `url.raw` 与 `path[]` 由同一份替换结果生成，不会同时出现 `:aid` 和 `{{aid}}`。

## 断言口径

每个请求只带一条断言：**响应必须是统一四字段信封**（`code`/`message`/`data`/`ttl`）。
刻意不断言 `code == 0`：集合要能在未造数据的库上跑通，业务码非 0 是正常结果。
需要业务断言时按用例另建 folder，不要改本文件（会被下次生成覆盖）。

## 已知边界

- 集合与真实库数据无关，跑通 ≠ 接口正确；本仓库未连接任何数据库或网关执行过这套集合；
- `AdminPermission` 分组未带令牌时会被 fail-closed 拒绝，这是预期行为；
- 角色↔权限点绑定不在迁移种子内，新库需运营先建角色授权，否则受保护路由一律 403；
- 契约里没有 WebSocket/SSE 路由（`.api` 不支持声明），因此集合只有「一次请求一次响应」的用例；上传链路只覆盖申请预签名这类 HTTP 调用，对象存储侧的真实 PUT 不在集合内。
