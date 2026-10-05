# 终端面 · `/moderation`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/app/api/app.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| moderation 增量：作者申诉 | 免鉴权 | 1 |

合计 **1** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## moderation 增量：作者申诉（免鉴权，1 条）

鉴权：免鉴权（网关无中间件；终端身份按约定用 `mid` 入参传递，见 `docs/api-and-events.md`）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/moderation/appeal` | 对驳回结论提交申诉（仅作者本人） | `submitAppeal` | `submitappeallogic.go` |

### POST `/moderation/appeal` — 对驳回结论提交申诉（仅作者本人）

- 权限口径：免鉴权
- goctl 入口：`gateway/app/internal/handler/submitappealhandler.go`
- 业务实现：`gateway/app/internal/logic/submitappeallogic.go`

请求：`ParamSubmitAppeal`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Content` | `content` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

响应：`ModerationAppealResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationAppealData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamSubmitAppeal`

> 审核结论由 moderation-orchestrator 唯一持有（AGENTS.md §5）；终端只能提交申诉， / 处理申诉走 gateway/admin。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | form | `int64` | 是 | — | — |
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Content` | `content` | form | `string` | 是 | — | — |
| `IP` | `ip` | form | `string` | 是 | — | — |

### `ModerationAppealResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `ModerationAppealData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ModerationAppealData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Appeal` | `appeal` | json | `ModerationAppealInfo` | 是 | — | — |

### `ModerationAppealInfo`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AppealId` | `appeal_id` | json | `int64` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Content` | `content` | json | `string` | 是 | — | — |
| `FinalVerdict` | `final_verdict` | json | `int32` | 是 | — | — |
| `FinalReason` | `final_reason` | json | `string` | 是 | — | — |
| `Handler` | `handler` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/app/21-moderation.md -->
