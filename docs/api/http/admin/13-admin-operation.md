# 运营面 · `/admin/operation`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

> 真源：`gateway/admin/api/admin.api`　·　生成一致性由本脚本的 routes.go 漂移门禁把守。

## 本组概览

| 小节 | 鉴权 | 路由数 |
|---|---|---|
| operation 域运营路由（免鉴权） | 免鉴权 | 2 |
| operation 域运营路由（受 AdminPermission 保护） | AdminPermission | 20 |

合计 **22** 条。

入参编码看下方各表的「位置」列：`path`→路径段、`form`→URL 查询串（POST 也一样）、`json`→JSON 请求体。
为什么 `form` 只能走查询串，见 [接口文档索引](../../README.md#阅读前要知道的四件事)第 4 条。

## operation 域运营路由（免鉴权）（免鉴权，2 条）

> 只有登录与权限判定入口不经 AdminPermission 中间件：
> 前者还没有会话，后者正是中间件要调用的方法（放在受保护组里会自锁）。
> 登录态由 operation 签发的 adm_ 前缀后台 token 表达，与终端用户 token 互不通用。

鉴权：免鉴权（刻意不进 `routePermissions` 的只读运营面）

| 方法 | 完整路径 | 说明 | handler | logic 文件 |
|---|---|---|---|---|
| POST | `/admin/operation/login` | 管理员登录（PBKDF2 校验 + 防爆破锁定 + 可选二次校验），签发后台 token | `adminLogin` | `adminloginlogic.go` |
| POST | `/admin/operation/permission/verify` | 权限判定调试入口（token 或 admin_id + resource + action，返回判定与命中角色） | `verifyAdminPermission` | `verifyadminpermissionlogic.go` |

### POST `/admin/operation/login` — 管理员登录（PBKDF2 校验 + 防爆破锁定 + 可选二次校验），签发后台 token

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/adminloginhandler.go`
- 业务实现：`gateway/admin/internal/logic/adminloginlogic.go`

请求：`ParamAdminLogin`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Username` | `username` | json | `string` | 是 | — | — |
| `Password` | `password` | json | `string` | 是 | — | — |
| `SecondFactor` | `second_factor` | json | `string` | 否 | — | — |
| `Ip` | `ip` | json | `string` | 否 | — | — |
| `UserAgent` | `user_agent` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |

响应：`AdminLoginResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminLoginData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/permission/verify` — 权限判定调试入口（token 或 admin_id + resource + action，返回判定与命中角色）

- 权限口径：免鉴权
- goctl 入口：`gateway/admin/internal/handler/verifyadminpermissionhandler.go`
- 业务实现：`gateway/admin/internal/logic/verifyadminpermissionlogic.go`

请求：`ParamVerifyAdminPermission`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Token` | `token` | json | `string` | 否 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 否 | — | — |
| `Resource` | `resource` | json | `string` | 是 | — | 契约缺口：operation.proto 未定义 resource 取值枚举， / 可用资源串由运营通过 CreatePermission 落库，网关只做非空校验。 |
| `Action` | `action` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

响应：`AdminPermissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminPermissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## operation 域运营路由（受 AdminPermission 保护）（AdminPermission，20 条）

> RBAC、菜单、运营配置、管理任务与审计索引的业务规则与数据全部在 services/operation；
> 网关只补审计主体、幂等键与分页上限，并在中间件里按路由表调用 VerifyAdminPermission。

鉴权：`AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按下表「权限点」判定；中间件对 `routePermissions` 表外路径 fail-closed，因此这一列空的行等于「谁都进不来」，必须补登记。

| 方法 | 完整路径 | 说明 | 权限点（resource / action） | handler | logic 文件 |
|---|---|---|---|---|---|
| POST | `/admin/operation/user/create` | 创建管理员账号（初始口令只进不出，响应永不返回散列） | `operation:admin_user` / `create` | `createAdminUser` | `createadminuserlogic.go` |
| POST | `/admin/operation/user/update` | 更新管理员账号（备注/状态/重置口令，重置口令会吊销会话） | `operation:admin_user` / `update` | `updateAdminUser` | `updateadminuserlogic.go` |
| POST | `/admin/operation/user/disable` | 禁用管理员账号（同时吊销全部会话） | `operation:admin_user` / `disable` | `disableAdminUser` | `disableadminuserlogic.go` |
| POST | `/admin/operation/user/list` | 分页查询管理员账号（state/keyword 过滤，ps 上限 100） | `operation:admin_user` / `read` | `listAdminUsers` | `listadminuserslogic.go` |
| POST | `/admin/operation/role/assign` | 全量覆盖管理员角色（空数组表示清空） | `operation:role` / `assign` | `assignRoles` | `assignroleslogic.go` |
| POST | `/admin/operation/role/create` | 创建角色并绑定权限点 | `operation:role` / `create` | `createRole` | `createrolelogic.go` |
| POST | `/admin/operation/role/list` | 分页查询角色 | `operation:role` / `read` | `listRoles` | `listroleslogic.go` |
| POST | `/admin/operation/role/delete` | 删除角色（仍有成员时 operation 拒绝） | `operation:role` / `delete` | `deleteRole` | `deleterolelogic.go` |
| POST | `/admin/operation/permission/list` | 分页查询权限点（domain 为空表示全部域） | `operation:permission` / `read` | `listPermissions` | `listpermissionslogic.go` |
| POST | `/admin/operation/permission/create` | 创建权限点（resource + action 唯一） | `operation:permission` / `create` | `createPermission` | `createpermissionlogic.go` |
| POST | `/admin/operation/menu/get` | 按管理员角色并集返回可见菜单（后台 Web 专用） | `operation:menu` / `read` | `getMenu` | `getmenulogic.go` |
| POST | `/admin/operation/menu/save` | 新建/更新菜单节点（menu_id 为 0 表示新建） | `operation:menu` / `update` | `saveMenu` | `savemenulogic.go` |
| POST | `/admin/operation/config/get` | 读取运营配置（默认走缓存，refresh=true 强制回源） | `ops:config` / `read` | `getOpsConfig` | `getopsconfiglogic.go` |
| POST | `/admin/operation/config/save` | 写入运营配置（expect_version 乐观锁，冲突需重新拉取） | `ops:config` / `update` | `saveOpsConfig` | `saveopsconfiglogic.go` |
| POST | `/admin/operation/task/submit` | 提交批量运营任务（op.request_id 幂等，步骤最多 1000） | `operation:task` / `create` | `submitAdminTask` | `submitadmintasklogic.go` |
| POST | `/admin/operation/task/get` | 查询任务与步骤明细（task_id 或 request_id） | `operation:task` / `read` | `getAdminTask` | `getadmintasklogic.go` |
| POST | `/admin/operation/task/list` | 分页查询管理任务（state/task_type/operator_id 过滤） | `operation:task` / `read` | `listAdminTasks` | `listadmintaskslogic.go` |
| POST | `/admin/operation/task/cancel` | 取消任务（仅 pending/running 可取消） | `operation:task` / `cancel` | `cancelAdminTask` | `canceladmintasklogic.go` |
| POST | `/admin/operation/task/run` | 推进任务（逐步骤调用下游 RPC，由 cron 或人工触发） | `operation:task` / `run` | `runAdminTask` | `runadmintasklogic.go` |
| POST | `/admin/operation/audit/list` | 分页查询管理操作审计索引（正文证据在被操作的领域服务） | `operation:audit` / `read` | `listAuditIndex` | `listauditindexlogic.go` |

### POST `/admin/operation/user/create` — 创建管理员账号（初始口令只进不出，响应永不返回散列）

- 权限口径：AdminPermission · 权限点 `operation:admin_user` / `create`
- goctl 入口：`gateway/admin/internal/handler/createadminuserhandler.go`
- 业务实现：`gateway/admin/internal/logic/createadminuserlogic.go`

请求：`ParamCreateAdminUser`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Username` | `username` | json | `string` | 是 | — | — |
| `Password` | `password` | json | `string` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `RoleIds` | `role_ids` | json | `[]int64` | 否 | — | — |
| `SecondFactorTarget` | `second_factor_target` | json | `string` | 否 | — | — |

响应：`OperationAdminUserResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAdminUserData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/user/update` — 更新管理员账号（备注/状态/重置口令，重置口令会吊销会话）

- 权限口径：AdminPermission · 权限点 `operation:admin_user` / `update`
- goctl 入口：`gateway/admin/internal/handler/updateadminuserhandler.go`
- 业务实现：`gateway/admin/internal/logic/updateadminuserlogic.go`

请求：`ParamUpdateAdminUser`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `NewPassword` | `new_password` | json | `string` | 否 | — | — |
| `SecondFactorTarget` | `second_factor_target` | json | `string` | 否 | — | — |

响应：`OperationAdminUserResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAdminUserData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/user/disable` — 禁用管理员账号（同时吊销全部会话）

- 权限口径：AdminPermission · 权限点 `operation:admin_user` / `disable`
- goctl 入口：`gateway/admin/internal/handler/disableadminuserhandler.go`
- 业务实现：`gateway/admin/internal/logic/disableadminuserlogic.go`

请求：`ParamDisableAdminUser`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |

响应：`OperationAdminUserResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAdminUserData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/user/list` — 分页查询管理员账号（state/keyword 过滤，ps 上限 100）

- 权限口径：AdminPermission · 权限点 `operation:admin_user` / `read`
- goctl 入口：`gateway/admin/internal/handler/listadminusershandler.go`
- 业务实现：`gateway/admin/internal/logic/listadminuserslogic.go`

请求：`ParamListAdminUsers`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OperationAdminUsersResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAdminUsersData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/role/assign` — 全量覆盖管理员角色（空数组表示清空）

- 权限口径：AdminPermission · 权限点 `operation:role` / `assign`
- goctl 入口：`gateway/admin/internal/handler/assignroleshandler.go`
- 业务实现：`gateway/admin/internal/logic/assignroleslogic.go`

请求：`ParamAssignRoles`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `RoleIds` | `role_ids` | json | `[]int64` | 否 | — | — |

响应：`OperationAssignRolesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAssignRolesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/role/create` — 创建角色并绑定权限点

- 权限口径：AdminPermission · 权限点 `operation:role` / `create`
- goctl 入口：`gateway/admin/internal/handler/createrolehandler.go`
- 业务实现：`gateway/admin/internal/logic/createrolelogic.go`

请求：`ParamCreateRole`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Title` | `title` | json | `string` | 否 | — | — |
| `PermissionIds` | `permission_ids` | json | `[]int64` | 否 | — | — |

响应：`OperationRoleResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationRoleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/role/list` — 分页查询角色

- 权限口径：AdminPermission · 权限点 `operation:role` / `read`
- goctl 入口：`gateway/admin/internal/handler/listroleshandler.go`
- 业务实现：`gateway/admin/internal/logic/listroleslogic.go`

请求：`ParamListRoles`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OperationRolesResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationRolesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/role/delete` — 删除角色（仍有成员时 operation 拒绝）

- 权限口径：AdminPermission · 权限点 `operation:role` / `delete`
- goctl 入口：`gateway/admin/internal/handler/deleterolehandler.go`
- 业务实现：`gateway/admin/internal/logic/deleterolelogic.go`

请求：`ParamDeleteRole`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `RoleId` | `role_id` | json | `int64` | 是 | — | — |

响应：`EmptyResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/permission/list` — 分页查询权限点（domain 为空表示全部域）

- 权限口径：AdminPermission · 权限点 `operation:permission` / `read`
- goctl 入口：`gateway/admin/internal/handler/listpermissionshandler.go`
- 业务实现：`gateway/admin/internal/logic/listpermissionslogic.go`

请求：`ParamListPermissions`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Domain` | `domain` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OperationPermissionsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationPermissionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/permission/create` — 创建权限点（resource + action 唯一）

- 权限口径：AdminPermission · 权限点 `operation:permission` / `create`
- goctl 入口：`gateway/admin/internal/handler/createpermissionhandler.go`
- 业务实现：`gateway/admin/internal/logic/createpermissionlogic.go`

请求：`ParamCreatePermission`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Resource` | `resource` | json | `string` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | — |
| `Domain` | `domain` | json | `string` | 否 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |

响应：`OperationPermissionResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationPermissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/menu/get` — 按管理员角色并集返回可见菜单（后台 Web 专用）

- 权限口径：AdminPermission · 权限点 `operation:menu` / `read`
- goctl 入口：`gateway/admin/internal/handler/getmenuhandler.go`
- 业务实现：`gateway/admin/internal/logic/getmenulogic.go`

请求：`ParamGetMenu`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 否 | — | — |

响应：`OperationMenusResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationMenusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/menu/save` — 新建/更新菜单节点（menu_id 为 0 表示新建）

- 权限口径：AdminPermission · 权限点 `operation:menu` / `update`
- goctl 入口：`gateway/admin/internal/handler/savemenuhandler.go`
- 业务实现：`gateway/admin/internal/logic/savemenulogic.go`

请求：`ParamSaveMenu`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `MenuId` | `menu_id` | json | `int64` | 否 | — | — |
| `ParentId` | `parent_id` | json | `int64` | 否 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Path` | `path` | json | `string` | 否 | — | — |
| `Icon` | `icon` | json | `string` | 否 | — | — |
| `Sort` | `sort` | json | `int32` | 否 | — | — |
| `RequiredPermission` | `required_permission` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |

响应：`OperationMenuResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationMenuData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/config/get` — 读取运营配置（默认走缓存，refresh=true 强制回源）

- 权限口径：AdminPermission · 权限点 `ops:config` / `read`
- goctl 入口：`gateway/admin/internal/handler/getopsconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/getopsconfiglogic.go`

请求：`ParamGetOpsConfig`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Refresh` | `refresh` | json | `bool` | 否 | — | — |

响应：`OperationConfigResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/config/save` — 写入运营配置（expect_version 乐观锁，冲突需重新拉取）

- 权限口径：AdminPermission · 权限点 `ops:config` / `update`
- goctl 入口：`gateway/admin/internal/handler/saveopsconfighandler.go`
- 业务实现：`gateway/admin/internal/logic/saveopsconfiglogic.go`

请求：`ParamSaveOpsConfig`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `CfgValue` | `cfg_value` | json | `string` | 是 | — | — |
| `ValueType` | `value_type` | json | `string` | 否 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |

响应：`OperationSaveConfigResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationSaveConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/task/submit` — 提交批量运营任务（op.request_id 幂等，步骤最多 1000）

- 权限口径：AdminPermission · 权限点 `operation:task` / `create`
- goctl 入口：`gateway/admin/internal/handler/submitadmintaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/submitadmintasklogic.go`

请求：`ParamSubmitAdminTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskType` | `task_type` | json | `string` | 是 | — | — |
| `Params` | `params` | json | `string` | 否 | — | — |
| `Steps` | `steps` | json | `[]OperationTaskStepSpec` | 否 | — | — |

响应：`OperationSubmitTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationSubmitTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/task/get` — 查询任务与步骤明细（task_id 或 request_id）

- 权限口径：AdminPermission · 权限点 `operation:task` / `read`
- goctl 入口：`gateway/admin/internal/handler/getadmintaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/getadmintasklogic.go`

请求：`ParamGetAdminTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

响应：`OperationTaskDetailResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationTaskDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/task/list` — 分页查询管理任务（state/task_type/operator_id 过滤）

- 权限口径：AdminPermission · 权限点 `operation:task` / `read`
- goctl 入口：`gateway/admin/internal/handler/listadmintaskshandler.go`
- 业务实现：`gateway/admin/internal/logic/listadmintaskslogic.go`

请求：`ParamListAdminTasks`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `State` | `state` | json | `string` | 否 | — | — |
| `TaskType` | `task_type` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OperationTasksResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/task/cancel` — 取消任务（仅 pending/running 可取消）

- 权限口径：AdminPermission · 权限点 `operation:task` / `cancel`
- goctl 入口：`gateway/admin/internal/handler/canceladmintaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/canceladmintasklogic.go`

请求：`ParamCancelAdminTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |

响应：`OperationTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/task/run` — 推进任务（逐步骤调用下游 RPC，由 cron 或人工触发）

- 权限口径：AdminPermission · 权限点 `operation:task` / `run`
- goctl 入口：`gateway/admin/internal/handler/runadmintaskhandler.go`
- 业务实现：`gateway/admin/internal/logic/runadmintasklogic.go`

请求：`ParamRunAdminTask`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `MaxSteps` | `max_steps` | json | `int32` | 否 | — | — |

响应：`OperationRunTaskResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationRunTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

### POST `/admin/operation/audit/list` — 分页查询管理操作审计索引（正文证据在被操作的领域服务）

- 权限口径：AdminPermission · 权限点 `operation:audit` / `read`
- goctl 入口：`gateway/admin/internal/handler/listauditindexhandler.go`
- 业务实现：`gateway/admin/internal/logic/listauditindexlogic.go`

请求：`ParamListAuditIndex`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | — |
| `ResourceType` | `resource_type` | json | `string` | 否 | — | — |
| `ResourceId` | `resource_id` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

响应：`OperationAuditsResponse`

| Go 字段 | JSON 字段 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAuditsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |


响应类型自带统一信封 `code`/`message`/`data`/`ttl`（AGENTS.md §6）；`data` 指向的结构体见本文「类型附录」。

## 类型附录

### `ParamAdminLogin`

> operation 域请求参数 / 管理员登录：request_id 必填（登录会写会话与审计索引，必须可归因）； / second_factor 仅在账号开启二次校验时必填，由 operation 服务判定。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Username` | `username` | json | `string` | 是 | — | — |
| `Password` | `password` | json | `string` | 是 | — | — |
| `SecondFactor` | `second_factor` | json | `string` | 否 | — | — |
| `Ip` | `ip` | json | `string` | 否 | — | — |
| `UserAgent` | `user_agent` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |

### `AdminLoginResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminLoginData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamVerifyAdminPermission`

> 权限判定调试入口：token 与 admin_id 二选一（token 优先，由 operation 解析）， / resource/action 必填（下游对空值直接报 ErrPermissionInvalid）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Token` | `token` | json | `string` | 否 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 否 | — | — |
| `Resource` | `resource` | json | `string` | 是 | — | 契约缺口：operation.proto 未定义 resource 取值枚举， / 可用资源串由运营通过 CreatePermission 落库，网关只做非空校验。 |
| `Action` | `action` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |

### `AdminPermissionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `AdminPermissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCreateAdminUser`

> 初始口令只进不出；role_ids 为空表示创建无权限账号； / second_factor_target 是手机号，非空时登录需带 second_factor。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Username` | `username` | json | `string` | 是 | — | — |
| `Password` | `password` | json | `string` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `RoleIds` | `role_ids` | json | `[]int64` | 否 | — | — |
| `SecondFactorTarget` | `second_factor_target` | json | `string` | 否 | — | — |

### `OperationAdminUserResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAdminUserData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamUpdateAdminUser`

> state：0 不修改、1 正常、2 禁用；new_password 非空会立即吊销该账号全部会话； / second_factor_target 传 "-" 表示关闭二次校验。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `NewPassword` | `new_password` | json | `string` | 否 | — | — |
| `SecondFactorTarget` | `second_factor_target` | json | `string` | 否 | — | — |

### `ParamDisableAdminUser`

> 禁用同时吊销会话；reason 进审计索引。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |

### `ParamListAdminUsers`

> state 传 0 表示全部；ps 上限 100（与 operation 的 maxPageSize 一致）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OperationAdminUsersResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAdminUsersData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamAssignRoles`

> role_ids 是全量覆盖语义，空数组表示清空角色。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `RoleIds` | `role_ids` | json | `[]int64` | 否 | — | — |

### `OperationAssignRolesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAssignRolesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCreateRole`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Title` | `title` | json | `string` | 否 | — | — |
| `PermissionIds` | `permission_ids` | json | `[]int64` | 否 | — | — |

### `OperationRoleResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationRoleData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListRoles`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Keyword` | `keyword` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OperationRolesResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationRolesData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamDeleteRole`

> 角色仍有成员时 operation 直接拒绝删除。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `RoleId` | `role_id` | json | `int64` | 是 | — | — |

### `EmptyResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `EmptyData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListPermissions`

> domain 为空表示全部域（video/catalog/rights/moderation/operation/system）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Domain` | `domain` | json | `string` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OperationPermissionsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationPermissionsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCreatePermission`

> resource + action 唯一；通配约定见 operation 服务 rbac 实现。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `Resource` | `resource` | json | `string` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | — |
| `Domain` | `domain` | json | `string` | 否 | — | — |
| `Description` | `description` | json | `string` | 否 | — | — |

### `OperationPermissionResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationPermissionData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamGetMenu`

> admin_id 传 0 表示按 op.operator_id 返回其可见菜单。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 否 | — | — |

### `OperationMenusResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationMenusData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSaveMenu`

> menu_id 为 0 表示新建；state 传 0 由服务端视为 1。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `MenuId` | `menu_id` | json | `int64` | 否 | — | — |
| `ParentId` | `parent_id` | json | `int64` | 否 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Path` | `path` | json | `string` | 否 | — | — |
| `Icon` | `icon` | json | `string` | 否 | — | — |
| `Sort` | `sort` | json | `int32` | 否 | — | — |
| `RequiredPermission` | `required_permission` | json | `string` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |

### `OperationMenuResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationMenuData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamGetOpsConfig`

> scope 为空表示 global；refresh=true 强制回源并刷新服务端缓存。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `Refresh` | `refresh` | json | `bool` | 否 | — | — |

### `OperationConfigResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSaveOpsConfig`

> expect_version：0 表示新建，非 0 表示基于该版本更新，冲突时 operation 返回 / ErrConfigVersionConflict（网关不做静默覆盖）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `CfgValue` | `cfg_value` | json | `string` | 是 | — | — |
| `ValueType` | `value_type` | json | `string` | 否 | — | — |
| `Scope` | `scope` | json | `string` | 否 | — | — |
| `ExpectVersion` | `expect_version` | json | `int64` | 否 | — | — |
| `State` | `state` | json | `int32` | 否 | — | — |
| `Remark` | `remark` | json | `string` | 否 | — | — |

### `OperationSaveConfigResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationSaveConfigData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamSubmitAdminTask`

> task_type：batch_offline_submission / batch_offline_episode / / batch_expire_window / batch_process_appeal；params 是 JSON 文本； / steps 最多 1000 条（operation 侧 maxTaskSteps）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskType` | `task_type` | json | `string` | 是 | — | — |
| `Params` | `params` | json | `string` | 否 | — | — |
| `Steps` | `steps` | json | `[]OperationTaskStepSpec` | 否 | — | — |

### `OperationSubmitTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationSubmitTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamGetAdminTask`

> task_id 与 request_id 二选一（都传时 operation 以 task_id 为准）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

### `OperationTaskDetailResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationTaskDetailData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListAdminTasks`

> state/task_type 为空表示不过滤；operator_id 传 0 表示全部。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `State` | `state` | json | `string` | 否 | — | — |
| `TaskType` | `task_type` | json | `string` | 否 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OperationTasksResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationTasksData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamCancelAdminTask`

> 仅 pending/running 可取消。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 否 | — | — |

### `OperationTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamRunAdminTask`

> max_steps <= 0 时由 operation 按服务端默认（Cache.RunSteps，默认 100）推进。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `MaxSteps` | `max_steps` | json | `int32` | 否 | — | — |

### `OperationRunTaskResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationRunTaskData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `ParamListAuditIndex`

> admin_id 传 0 表示全部；start_at 含、end_at 不含（Unix 秒）。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Op` | `op` | json | `AdminOpContext` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 否 | — | — |
| `Action` | `action` | json | `string` | 否 | — | — |
| `ResourceType` | `resource_type` | json | `string` | 否 | — | — |
| `ResourceId` | `resource_id` | json | `string` | 否 | — | — |
| `StartAt` | `start_at` | json | `int64` | 否 | — | — |
| `EndAt` | `end_at` | json | `int64` | 否 | — | — |
| `Pn` | `pn` | json | `int32` | 否 | — | — |
| `Ps` | `ps` | json | `int32` | 否 | — | — |

### `OperationAuditsResponse`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Code` | `code` | json | `int` | 是 | — | — |
| `Message` | `message` | json | `string` | 是 | — | — |
| `Data` | `data` | json | `OperationAuditsData` | 是 | — | — |
| `TTL` | `ttl` | json | `int64` | 是 | — | — |

### `AdminLoginData`

> 管理员登录：token 是后台专用会话凭证（adm_ 前缀，与终端用户 token 互不通用）。 / 口令只在请求体出现，任何响应与日志都不得回显。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Token` | `token` | json | `string` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `Username` | `username` | json | `string` | 是 | — | — |
| `ExpiresAt` | `expires_at` | json | `int64` | 是 | — | — |
| `Roles` | `roles` | json | `[]string` | 是 | — | — |

### `AdminPermissionData`

> 权限判定结果（对齐 operation.v1.VerifyAdminPermissionReply； / reason 是下游给出的稳定脱敏原因，allowed=false 时非空）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Allowed` | `allowed` | json | `bool` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `MatchedRoles` | `matched_roles` | json | `[]string` | 是 | — | — |
| `Reason` | `reason` | json | `string` | 是 | — | — |

### `AdminOpContext`

> 管理后台领域服务（RBAC / 菜单 / 运营配置 / 管理任务 / 审计索引）。 / 字段口径逐项对齐 services/operation/rpc/operation.proto（operation.v1.*）， / 不新增下游没有的字段；下游缺失的能力在 logic 里以「契约缺口」注释标注。 / AdminOpContext 是 operation 除 AdminLogin/VerifyAdminPermission 外所有方法 / 必带的操作者上下文（proto OpContext：审计主体与链路信息）。 / 网关把它作为统一嵌套字段 op 放进每个 POST 体，字段名与 proto 一一对应： /   - operator_id：审计主体。中间件用后台会话解析出的 admin_id 覆盖客户端声明值； /   - request_id：幂等键，写接口必填（proto 注释「写接口必填」）； /   - ip / user_agent：operation 服务侧只做哈希与脱敏后入审计索引。

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `OperatorName` | `operator_name` | json | `string` | 否 | — | — |
| `Ip` | `ip` | json | `string` | 否 | — | — |
| `UserAgent` | `user_agent` | json | `string` | 否 | — | — |
| `TraceId` | `trace_id` | json | `string` | 否 | — | — |
| `RequestId` | `request_id` | json | `string` | 否 | — | — |

### `OperationAdminUserData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `User` | `user` | json | `OperationAdminUserItem` | 是 | — | — |

### `OperationAdminUsersData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OperationAdminUserItem` | 是 | — | — |

### `OperationAssignRolesData`

> AssignRoles 是全量覆盖语义，回传覆盖后的角色集合

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoleIds` | `role_ids` | json | `[]int64` | 是 | — | — |

### `OperationRoleData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Role` | `role` | json | `OperationRoleItem` | 是 | — | — |

### `OperationRolesData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OperationRoleItem` | 是 | — | — |

### `EmptyData`

（该类型无字段：空请求 / 空响应。）

### `OperationPermissionsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OperationPermissionItem` | 是 | — | — |

### `OperationPermissionData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Permission` | `permission` | json | `OperationPermissionItem` | 是 | — | — |

### `OperationMenusData`

> GetMenuReply.ttl 是前端可缓存秒数，放到信封 ttl 字段

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Items` | `items` | json | `[]OperationMenuItem` | 是 | — | — |

### `OperationMenuData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Menu` | `menu` | json | `OperationMenuItem` | 是 | — | — |

### `OperationConfigData`

> from_cache 命中缓存标记，便于排障；GetOpsConfigReply.ttl 进信封 ttl

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Config` | `config` | json | `OperationConfigItem` | 是 | — | — |
| `FromCache` | `from_cache` | json | `bool` | 是 | — | — |

### `OperationSaveConfigData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Config` | `config` | json | `OperationConfigItem` | 是 | — | — |

### `OperationTaskStepSpec`

> 提交任务的步骤声明（对齐 operation.v1.TaskStepSpec，单次最多 1000 步）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TargetType` | `target_type` | json | `string` | 是 | — | — |
| `TargetId` | `target_id` | json | `string` | 是 | — | — |

### `OperationSubmitTaskData`

> reused=true 表示 op.request_id 命中已有任务（幂等返回）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `OperationTaskItem` | 是 | — | — |
| `Reused` | `reused` | json | `bool` | 是 | — | — |

### `OperationTaskDetailData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `OperationTaskItem` | 是 | — | — |
| `Steps` | `steps` | json | `[]OperationTaskStepItem` | 是 | — | — |

### `OperationTasksData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OperationTaskItem` | 是 | — | — |

### `OperationTaskData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `OperationTaskItem` | 是 | — | — |

### `OperationRunTaskData`

> executed 是本次推进实际执行的步数

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Task` | `task` | json | `OperationTaskItem` | 是 | — | — |
| `Steps` | `steps` | json | `[]OperationTaskStepItem` | 是 | — | — |
| `Executed` | `executed` | json | `int32` | 是 | — | — |

### `OperationAuditsData`

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Total` | `total` | json | `int64` | 是 | — | — |
| `Items` | `items` | json | `[]OperationAuditItem` | 是 | — | — |

### `OperationAdminUserItem`

> 管理员账号投影（对齐 operation.v1.AdminUserItem，永不返回口令散列； / state：1 正常、2 禁用、3 锁定）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `Username` | `username` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `LastLoginAt` | `last_login_at` | json | `int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `RoleIds` | `role_ids` | json | `[]int64` | 是 | — | — |
| `RoleNames` | `role_names` | json | `[]string` | 是 | — | — |
| `SecondFactorEnabled` | `second_factor_enabled` | json | `bool` | 是 | — | — |

### `OperationRoleItem`

> 角色投影（对齐 operation.v1.RoleItem；state：1 启用、2 停用； / member_count 是服务侧删除角色的保护依据）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `RoleId` | `role_id` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Title` | `title` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `MemberCount` | `member_count` | json | `int64` | 是 | — | — |
| `PermissionIds` | `permission_ids` | json | `[]int64` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OperationPermissionItem`

> 权限点投影（对齐 operation.v1.PermissionItem；resource 支持 "*"、"域:*"、"*:动作" 通配）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `PermissionId` | `permission_id` | json | `int64` | 是 | — | — |
| `Resource` | `resource` | json | `string` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | — |
| `Domain` | `domain` | json | `string` | 是 | — | — |
| `Description` | `description` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |

### `OperationMenuItem`

> 菜单节点（对齐 operation.v1.MenuItem，扁平返回，父级用 parent_id 表达，由前端组树； / required_permission 形如 "video:submission#offline"，空表示登录即可见；state：1 显示、2 隐藏）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `MenuId` | `menu_id` | json | `int64` | 是 | — | — |
| `ParentId` | `parent_id` | json | `int64` | 是 | — | — |
| `Name` | `name` | json | `string` | 是 | — | — |
| `Path` | `path` | json | `string` | 是 | — | — |
| `Icon` | `icon` | json | `string` | 是 | — | — |
| `Sort` | `sort` | json | `int32` | 是 | — | — |
| `RequiredPermission` | `required_permission` | json | `string` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OperationConfigItem`

> 运营配置投影（对齐 operation.v1.ConfigItem；version 是乐观锁版本号， / 每次成功写入 +1；value_type：string/int/bool/json；state：1 生效、2 下线）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `CfgKey` | `cfg_key` | json | `string` | 是 | — | — |
| `CfgValue` | `cfg_value` | json | `string` | 是 | — | — |
| `ValueType` | `value_type` | json | `string` | 是 | — | — |
| `Scope` | `scope` | json | `string` | 是 | — | — |
| `Version` | `version` | json | `int64` | 是 | — | — |
| `State` | `state` | json | `int32` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `Remark` | `remark` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OperationTaskItem`

> 管理任务投影（对齐 operation.v1.TaskInfo； / state：pending/running/succeeded/partial/failed/canceled）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `TaskType` | `task_type` | json | `string` | 是 | — | — |
| `Params` | `params` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Total` | `total` | json | `int32` | 是 | — | — |
| `Succeeded` | `succeeded` | json | `int32` | 是 | — | — |
| `Failed` | `failed` | json | `int32` | 是 | — | — |
| `Progress` | `progress` | json | `int32` | 是 | — | — |
| `OperatorId` | `operator_id` | json | `int64` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |
| `StartedAt` | `started_at` | json | `int64` | 是 | — | — |
| `FinishedAt` | `finished_at` | json | `int64` | 是 | — | — |

### `OperationTaskStepItem`

> 任务步骤投影（对齐 operation.v1.TaskStepInfo；步骤 state： / pending/running/succeeded/failed/skipped/canceled；err_msg 已脱敏）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `TaskId` | `task_id` | json | `int64` | 是 | — | — |
| `StepNo` | `step_no` | json | `int32` | 是 | — | — |
| `TargetType` | `target_type` | json | `string` | 是 | — | — |
| `TargetId` | `target_id` | json | `string` | 是 | — | — |
| `State` | `state` | json | `string` | 是 | — | — |
| `Result` | `result` | json | `string` | 是 | — | — |
| `ErrMsg` | `err_msg` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |
| `Mtime` | `mtime` | json | `int64` | 是 | — | — |

### `OperationAuditItem`

> 审计索引投影（对齐 operation.v1.AuditIndexItem；result：ok/denied/error， / ip_hash 是不可逆摘要，网关与下游都不落明文 IP；正文证据在被操作的领域服务）

| Go 字段 | 参数/JSON 键 | 位置 | 类型 | 必填 | 默认/约束 | 说明 |
|---|---|---|---|---|---|---|
| `Id` | `id` | json | `int64` | 是 | — | — |
| `AdminId` | `admin_id` | json | `int64` | 是 | — | — |
| `Username` | `username` | json | `string` | 是 | — | — |
| `Action` | `action` | json | `string` | 是 | — | — |
| `ResourceType` | `resource_type` | json | `string` | 是 | — | — |
| `ResourceId` | `resource_id` | json | `string` | 是 | — | — |
| `Result` | `result` | json | `string` | 是 | — | — |
| `IpHash` | `ip_hash` | json | `string` | 是 | — | — |
| `UserAgent` | `user_agent` | json | `string` | 是 | — | — |
| `TraceId` | `trace_id` | json | `string` | 是 | — | — |
| `RequestId` | `request_id` | json | `string` | 是 | — | — |
| `Ctime` | `ctime` | json | `int64` | 是 | — | — |


<!-- file: docs/api/http/admin/13-admin-operation.md -->
