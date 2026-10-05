# 运营面 · `/admin/video`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| video 域运营路由 | 免鉴权 | 2 |
| video 域写入口（受 AdminPermission 保护） | AdminPermission | 1 |

合计 **3** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## video 域运营路由（免鉴权，2 条）

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| GET | `/admin/video/submissions` | 分页查询稿件（按 mid/typeid/state 过滤） | `listVideoSubmissions` | `listvideosubmissionslogic.go` |
| GET | `/admin/video/submissions/:aid` | 查询稿件详情 | `getVideoSubmission` | `getvideosubmissionlogic.go` |

### GET `/admin/video/submissions` — 分页查询稿件（按 mid/typeid/state 过滤）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/listvideosubmissionshandler.go`
- 业务实现：`gateway/admin/internal/logic/listvideosubmissionslogic.go`

请求：`ParamListVideoSubmissions`

| Go 字段 | query 参数 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

响应：`VideoSubmissionsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### GET `/admin/video/submissions/:aid` — 查询稿件详情

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/getvideosubmissionhandler.go`
- 业务实现：`gateway/admin/internal/logic/getvideosubmissionlogic.go`

请求：`ParamVideoAid`

（该类型无字段：空请求 / 空响应。）

路径参数：

| Go 字段 | 路径段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |

响应：`VideoSubmissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## video 域写入口（受 AdminPermission 保护）（AdminPermission，1 条）

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/video/submissions/:aid/transition` | 推进稿件状态机（校验合法转换，禁止直接置为 PUBLISHED） | `video:submission` / `transition` | `transitionVideoSubmission` | `transitionvideosubmissionlogic.go` |

### POST `/admin/video/submissions/:aid/transition` — 推进稿件状态机（校验合法转换，禁止直接置为 PUBLISHED）

- 权限口径：AdminPermission · 权限点 `video:submission` / `transition`
- goctl 入口：`gateway/admin/internal/handler/transitionvideosubmissionhandler.go`
- 业务实现：`gateway/admin/internal/logic/transitionvideosubmissionlogic.go`

请求：`ParamVideoTransition`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Target` | `target` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

响应：`VideoSubmissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamListVideoSubmissions`

> video 域请求参数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Mid` | `mid` | form | `int64` | 是 | — | — |
| `Typeid` | `typeid` | form | `int32` | 是 | — | — |
| `State` | `state` | form | `int32` | 是 | — | — |
| `Pn` | `pn` | form | `int32` | 是 | default=1 | — |
| `Ps` | `ps` | form | `int32` | 是 | default=20 | — |

### `VideoSubmissionsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamVideoAid`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |

### `VideoSubmissionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `VideoSubmissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamVideoTransition`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | path | `int64` | 是 | — | — |
| `Target` | `target` | json | `int32` | 是 | — | — |
| `Operator` | `operator` | json | `string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `VideoSubmissionsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Submissions` | `submissions` | json | `[]VideoSubmissionItem` | 是 | — | — |

### `VideoSubmissionData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Submission` | `submission` | json | `VideoSubmissionItem` | 是 | — | — |

### `VideoSubmissionItem`

> 稿件条目（与 video.v1.Submission 对齐，状态码沿用 SubmissionState 枚举值）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Aid` | `aid` | json | `int64` | 是 | — | — |
| `Mid` | `mid` | json | `int64` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `Desc` | `desc` | json | `string` | 是 | — | — |
| `Cover` | `cover` | json | `string` | 是 | — | — |
| `Typeid` | `typeid` | json | `int32` | 是 | — | — |
| `Tag` | `tag` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/04-admin-video.md -->
