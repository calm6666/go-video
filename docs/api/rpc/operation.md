# RPC · `operation`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/operation/rpc/operation.proto` |
| protobuf 包 | `operation.v1` |
| go_package | `go-video/services/operation/rpc` |
| 发现用的 etcd key | `operation.v1.rpc`（`services/operation/etc/operation.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | 与上面的 key 相同（`operation.v1.rpc`） |
| 监听 | `8109`（`services/operation/etc/operation.v1.yaml` 的 `ListenOn`） |
| 数据库 | `go_video_operation` |
| 方法数 | 22（service `Operation`） |
| 网关消费方 | `admin:OperationRPC` |

## 契约说明

> 说明：operation 是管理后台领域服务（AGENTS.md §3：管理后台的业务逻辑与数据
> 所有权在 services/operation，gateway/admin 只做入口聚合、统一鉴权和限流）。
> 本服务拥有 admin_user / role / permission / menu / ops_config / admin_task /
> audit_index / admin_session 自有表（库名 go_video_operation）。
>
> 数据所有权边界（AGENTS.md §5）：本服务对稿件、目录、版权、审核的**业务写操作**
> 一律通过 video / catalog / rights / moderation-orchestrator 的 RPC 完成，
> 不直连它们的库表；本服务只负责编排、权限判定和审计留痕。
>
> 与 account 的边界：管理员账号是后台身份（op_admin_user），与终端用户账号
> （account.mid / account 库表）完全隔离，不读写对方的表，token 语义互不通用。
> 需要展示管理员关联的员工/用户信息时只调用 account 的公开 RPC。
>
> 商业化范围外（AGENTS.md §1）：本契约不提供订单、支付、投币、广告投放与广告位
> 配置等能力；运营配置只覆盖内容展示、审核工作流和灰度开关。

## service `Operation`

> Operation 管理后台领域服务：RBAC、运营配置、管理任务编排与审计索引。

gRPC 方法前缀：`operation.v1.Operation/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `AdminLogin` | [`AdminLoginReq`](#message-adminloginreq) | [`AdminLoginReply`](#message-adminloginreply) | 管理员登录（口令校验 + 防爆破锁定），签发后台专用 token |
| 2 | `VerifyAdminPermission` | [`VerifyAdminPermissionReq`](#message-verifyadminpermissionreq) | [`VerifyAdminPermissionReply`](#message-verifyadminpermissionreply) | 权限校验（gateway/admin 每个受保护路由调用；结果带短缓存） |
| 3 | `CreateAdminUser` | [`CreateAdminUserReq`](#message-createadminuserreq) | [`CreateAdminUserReply`](#message-createadminuserreply) | 创建管理员账号（口令只在入参出现，响应永不返回散列） |
| 4 | `UpdateAdminUser` | [`UpdateAdminUserReq`](#message-updateadminuserreq) | [`UpdateAdminUserReply`](#message-updateadminuserreply) | 更新管理员账号（备注/状态/重置口令，重置口令会吊销会话） |
| 5 | `DisableAdminUser` | [`DisableAdminUserReq`](#message-disableadminuserreq) | [`DisableAdminUserReply`](#message-disableadminuserreply) | 禁用管理员账号（同时吊销全部会话） |
| 6 | `ListAdminUsers` | [`ListAdminUsersReq`](#message-listadminusersreq) | [`ListAdminUsersReply`](#message-listadminusersreply) | 分页查询管理员账号 |
| 7 | `AssignRoles` | [`AssignRolesReq`](#message-assignrolesreq) | [`AssignRolesReply`](#message-assignrolesreply) | 全量覆盖管理员角色（并集生效） |
| 8 | `CreateRole` | [`CreateRoleReq`](#message-createrolereq) | [`CreateRoleReply`](#message-createrolereply) | 创建角色并绑定权限点 |
| 9 | `ListRoles` | [`ListRolesReq`](#message-listrolesreq) | [`ListRolesReply`](#message-listrolesreply) | 分页查询角色 |
| 10 | `DeleteRole` | [`DeleteRoleReq`](#message-deleterolereq) | [`EmptyReply`](#message-emptyreply) | 删除角色（仍有成员时拒绝） |
| 11 | `ListPermissions` | [`ListPermissionsReq`](#message-listpermissionsreq) | [`ListPermissionsReply`](#message-listpermissionsreply) | 分页查询权限点 |
| 12 | `CreatePermission` | [`CreatePermissionReq`](#message-createpermissionreq) | [`CreatePermissionReply`](#message-createpermissionreply) | 创建权限点（resource + action 唯一） |
| 13 | `GetMenu` | [`GetMenuReq`](#message-getmenureq) | [`GetMenuReply`](#message-getmenureply) | 按管理员角色并集返回可见菜单（后台 Web 专用） |
| 14 | `SaveMenu` | [`SaveMenuReq`](#message-savemenureq) | [`SaveMenuReply`](#message-savemenureply) | 新建/更新菜单节点 |
| 15 | `GetOpsConfig` | [`GetOpsConfigReq`](#message-getopsconfigreq) | [`GetOpsConfigReply`](#message-getopsconfigreply) | 读取运营配置（默认走缓存，refresh=true 强制回源） |
| 16 | `SaveOpsConfig` | [`SaveOpsConfigReq`](#message-saveopsconfigreq) | [`SaveOpsConfigReply`](#message-saveopsconfigreply) | 写入运营配置（expect_version 乐观锁 + 操作者留痕） |
| 17 | `SubmitAdminTask` | [`SubmitAdminTaskReq`](#message-submitadmintaskreq) | [`SubmitAdminTaskReply`](#message-submitadmintaskreply) | 提交批量运营任务（request_id 幂等，状态 pending） |
| 18 | `GetAdminTask` | [`GetAdminTaskReq`](#message-getadmintaskreq) | [`GetAdminTaskReply`](#message-getadmintaskreply) | 查询任务与步骤明细 |
| 19 | `ListAdminTasks` | [`ListAdminTasksReq`](#message-listadmintasksreq) | [`ListAdminTasksReply`](#message-listadmintasksreply) | 分页查询任务 |
| 20 | `CancelAdminTask` | [`CancelAdminTaskReq`](#message-canceladmintaskreq) | [`CancelAdminTaskReply`](#message-canceladmintaskreply) | 取消任务（仅 pending/running 可取消） |
| 21 | `RunAdminTask` | [`RunAdminTaskReq`](#message-runadmintaskreq) | [`RunAdminTaskReply`](#message-runadmintaskreply) | 推进任务：逐步骤调用下游 RPC（由 cron 或人工触发，本服务不内置 worker） |
| 22 | `ListAuditIndex` | [`ListAuditIndexReq`](#message-listauditindexreq) | [`ListAuditIndexReply`](#message-listauditindexreply) | 查询管理操作审计索引（正文证据在下游服务/audit） |

## 消息与枚举

### message `EmptyReply`

> 通用空响应

（空消息）

### message `OpContext`

> OpContext 管理操作上下文：调用者身份与追踪信息。 / 除 AdminLogin/VerifyAdminPermission 外，所有方法都必须携带， / 服务端据此写审计索引（op_audit_index）并做日志关联。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `operator_id` | `int64` | 1 | — | 操作管理员 admin_id（0 表示未鉴权，写接口直接拒绝） |
| `operator_name` | `string` | 2 | — | 操作管理员用户名（冗余，便于列表展示） |
| `ip` | `string` | 3 | — | 来源 IP（服务端只存哈希，不落明文） |
| `user_agent` | `string` | 4 | — | 客户端 UA（截断后存索引） |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |
| `request_id` | `string` | 6 | — | 幂等键（写接口必填） |

### message `AdminLoginReq`

> --- 1. 管理员登录与权限校验 --- / AdminLoginReq 管理员登录：账号 + 口令 + 可选二次校验码。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `username` | `string` | 1 | — | 管理员账号名 |
| `password` | `string` | 2 | — | 口令（明文经 TLS 传输，服务端做 PBKDF2 校验；日志与响应禁止回显） |
| `second_factor` | `string` | 3 | — | 可选二次校验码（TOTP/短信），为空表示未启用二阶校验 |
| `ip` | `string` | 4 | — | 来源 IP |
| `user_agent` | `string` | 5 | — | 客户端 UA |
| `trace_id` | `string` | 6 | — | 链路追踪 ID |
| `request_id` | `string` | 7 | — | 幂等键 |

### message `AdminLoginReply`

> AdminLoginReply 登录成功，返回后台专用 token（与用户端 token 语义不同）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `token` | `string` | 1 | — | 管理会话 token（adm_ 前缀，仅 gateway/admin 可用） |
| `admin_id` | `int64` | 2 | — | 管理员 ID |
| `username` | `string` | 3 | — | 管理员用户名 |
| `expires_at` | `int64` | 4 | — | token 过期时间（Unix 秒） |
| `roles` | `string` | 5 | repeated | 命中角色名列表 |
| `ttl` | `int32` | 6 | — | 建议客户端缓存秒数（后台登录态固定 0，不建议缓存） |

### message `VerifyAdminPermissionReq`

> VerifyAdminPermissionReq 权限校验入口，由 gateway/admin 在每个受保护路由上调用。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `token` | `string` | 1 | — | 管理会话 token（与 admin_id 二选一，token 优先） |
| `admin_id` | `int64` | 2 | — | 已由网关解析出的管理员 ID |
| `resource` | `string` | 3 | — | 资源，如 video:submission、catalog:episode、rights:window |
| `action` | `string` | 4 | — | 动作，如 read、offline、approve |
| `trace_id` | `string` | 5 | — | 链路追踪 ID |

### message `VerifyAdminPermissionReply`

> VerifyAdminPermissionReply 权限判定结果。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `allowed` | `bool` | 1 | — | 是否放行 |
| `matched_roles` | `string` | 2 | repeated | 命中的角色名（用于拒绝时提示缺哪个权限） |
| `admin_id` | `int64` | 3 | — | 判定所用的管理员 ID（token 解析结果，便于网关写日志） |
| `ttl` | `int32` | 4 | — | 网关可缓存该判定的秒数 |
| `reason` | `string` | 5 | — | 拒绝原因（denied 时非空，脱敏） |

### message `AdminUserItem`

> --- 2. 管理员账号生命周期 --- / AdminUserItem 管理员账号投影（永不返回口令散列）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `admin_id` | `int64` | 1 | — | 管理员 ID |
| `username` | `string` | 2 | — | 账号名 |
| `state` | `int32` | 3 | — | 1 正常、2 禁用、3 锁定 |
| `remark` | `string` | 4 | — | 备注 |
| `operator_id` | `int64` | 5 | — | 最后修改人 |
| `last_login_at` | `int64` | 6 | — | 最近登录时间（Unix 秒） |
| `ctime` | `int64` | 7 | — | 创建时间 |
| `mtime` | `int64` | 8 | — | 修改时间 |
| `role_ids` | `int64` | 9 | repeated | 已分配角色 ID |
| `role_names` | `string` | 10 | repeated | 已分配角色名 |
| `second_factor_enabled` | `bool` | 11 | — | 是否启用二次校验（只回布尔，不回手机号等目标值） |

### message `CreateAdminUserReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `username` | `string` | 2 | — | 账号名（全局唯一） |
| `password` | `string` | 3 | — | 初始口令（只在创建/重置时传，响应不回显） |
| `remark` | `string` | 4 | — | 备注 |
| `role_ids` | `int64` | 5 | repeated | 初始角色（可为空，空表示无权限账号） |
| `second_factor_target` | `string` | 6 | — | 二次校验目标（手机号；非空时登录需带 second_factor，验证码由 account 服务下发） |

### message `CreateAdminUserReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `user` | [`AdminUserItem`](#message-adminuseritem) | 1 | — | — |

### message `UpdateAdminUserReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `admin_id` | `int64` | 2 | — | 目标管理员 |
| `remark` | `string` | 3 | — | 新备注（空表示不修改） |
| `state` | `int32` | 4 | — | 目标状态：0 不修改、1 正常、2 禁用 |
| `new_password` | `string` | 5 | — | 重置口令（空表示不改口令；非空时立即吊销该账号全部会话） |
| `second_factor_target` | `string` | 6 | — | 覆盖二次校验目标（空表示不修改；传 "-" 表示关闭二次校验） |

### message `UpdateAdminUserReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `user` | [`AdminUserItem`](#message-adminuseritem) | 1 | — | — |

### message `DisableAdminUserReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `admin_id` | `int64` | 2 | — | 目标管理员（禁用同时吊销会话） |
| `reason` | `string` | 3 | — | 禁用原因（写审计索引） |

### message `DisableAdminUserReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `user` | [`AdminUserItem`](#message-adminuseritem) | 1 | — | — |

### message `ListAdminUsersReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `state` | `int32` | 2 | — | 0 全部、1/2/3 按状态过滤 |
| `keyword` | `string` | 3 | — | 用户名模糊匹配 |
| `pn` | `int32` | 4 | — | 页码，从 1 开始 |
| `ps` | `int32` | 5 | — | 每页大小，最大 100 |

### message `ListAdminUsersReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`AdminUserItem`](#message-adminuseritem) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `RoleItem`

> --- 3. 角色与权限点 --- / RoleItem 角色投影。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `role_id` | `int64` | 1 | — | 角色 ID |
| `name` | `string` | 2 | — | 角色标识（唯一，如 super_admin、content_ops） |
| `title` | `string` | 3 | — | 角色展示名 |
| `state` | `int32` | 4 | — | 1 启用、2 停用 |
| `member_count` | `int64` | 5 | — | 成员数（删除角色时的保护依据） |
| `permission_ids` | `int64` | 6 | repeated | 已绑定权限点 ID |
| `ctime` | `int64` | 7 | — | — |
| `mtime` | `int64` | 8 | — | — |

### message `CreateRoleReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `name` | `string` | 2 | — | 角色标识（唯一） |
| `title` | `string` | 3 | — | 展示名 |
| `permission_ids` | `int64` | 4 | repeated | 初始权限点 |

### message `CreateRoleReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `role` | [`RoleItem`](#message-roleitem) | 1 | — | — |

### message `ListRolesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `state` | `int32` | 2 | — | 0 全部 |
| `keyword` | `string` | 3 | — | — |
| `pn` | `int32` | 4 | — | — |
| `ps` | `int32` | 5 | — | — |

### message `ListRolesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`RoleItem`](#message-roleitem) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `DeleteRoleReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `role_id` | `int64` | 2 | — | 有成员时禁止删除（返回明确错误） |

### message `AssignRolesReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `admin_id` | `int64` | 2 | — | 目标管理员 |
| `role_ids` | `int64` | 3 | repeated | 全量覆盖：空数组表示清空角色 |

### message `AssignRolesReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `role_ids` | `int64` | 1 | repeated | 覆盖后的角色集合 |

### message `PermissionItem`

> PermissionItem 权限点投影。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `permission_id` | `int64` | 1 | — | 权限点 ID |
| `resource` | `string` | 2 | — | 资源（如 video:submission、catalog:episode、rights:window、ops:config） |
| `action` | `string` | 3 | — | 动作（read/create/update/offline/approve/*） |
| `domain` | `string` | 4 | — | 所属域（video/catalog/rights/moderation/operation/system） |
| `description` | `string` | 5 | — | 说明 |
| `ctime` | `int64` | 6 | — | — |

### message `CreatePermissionReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `resource` | `string` | 2 | — | — |
| `action` | `string` | 3 | — | — |
| `domain` | `string` | 4 | — | — |
| `description` | `string` | 5 | — | — |

### message `CreatePermissionReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `permission` | [`PermissionItem`](#message-permissionitem) | 1 | — | — |

### message `ListPermissionsReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `domain` | `string` | 2 | — | 空表示全部域 |
| `pn` | `int32` | 3 | — | — |
| `ps` | `int32` | 4 | — | — |

### message `ListPermissionsReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`PermissionItem`](#message-permissionitem) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `MenuItem`

> --- 4. 后台菜单（仅 Web 后台使用，项目不支持小程序） --- / MenuItem 菜单节点（扁平返回，父级用 parent_id 表达，由前端组树）。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `menu_id` | `int64` | 1 | — | — |
| `parent_id` | `int64` | 2 | — | 0 表示根节点 |
| `name` | `string` | 3 | — | 菜单名 |
| `path` | `string` | 4 | — | 前端路由 |
| `icon` | `string` | 5 | — | 图标 |
| `sort` | `int32` | 6 | — | 同级排序，小者在前 |
| `required_permission` | `string` | 7 | — | 形如 "video:submission#offline"（resource#action），空表示登录即可见 |
| `state` | `int32` | 8 | — | 1 显示、2 隐藏 |
| `ctime` | `int64` | 9 | — | — |
| `mtime` | `int64` | 10 | — | — |

### message `GetMenuReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `admin_id` | `int64` | 2 | — | 按该管理员的角色并集过滤；0 表示用 ctx.operator_id |

### message `GetMenuReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`MenuItem`](#message-menuitem) | 1 | repeated | — |
| `ttl` | `int64` | 2 | — | 前端可缓存秒数 |

### message `SaveMenuReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `menu_id` | `int64` | 2 | — | 0 表示新建 |
| `parent_id` | `int64` | 3 | — | — |
| `name` | `string` | 4 | — | — |
| `path` | `string` | 5 | — | — |
| `icon` | `string` | 6 | — | — |
| `sort` | `int32` | 7 | — | — |
| `required_permission` | `string` | 8 | — | — |
| `state` | `int32` | 9 | — | 0 视为 1 |

### message `SaveMenuReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `menu` | [`MenuItem`](#message-menuitem) | 1 | — | — |

### message `ConfigItem`

> --- 5. 运营配置（分区/标签/审核阈值/灰度开关，不含商业化投放） --- / ConfigItem 运营配置投影。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `cfg_key` | `string` | 2 | — | 配置键 |
| `cfg_value` | `string` | 3 | — | 配置值（字符串承载，按 value_type 解析） |
| `value_type` | `string` | 4 | — | string/int/bool/json |
| `scope` | `string` | 5 | — | 生效范围：global/android/ios/harmony/desktop/<自定义> |
| `version` | `int64` | 6 | — | 版本号，每次成功写入 +1（乐观锁） |
| `state` | `int32` | 7 | — | 1 生效、2 下线 |
| `operator_id` | `int64` | 8 | — | 最后修改人 |
| `remark` | `string` | 9 | — | 变更说明 |
| `ctime` | `int64` | 10 | — | — |
| `mtime` | `int64` | 11 | — | — |

### message `GetOpsConfigReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `cfg_key` | `string` | 2 | — | — |
| `scope` | `string` | 3 | — | 空表示 global |
| `refresh` | `bool` | 4 | — | true 强制回源并刷新缓存 |

### message `GetOpsConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `config` | [`ConfigItem`](#message-configitem) | 1 | — | — |
| `from_cache` | `bool` | 2 | — | 命中缓存标记（便于排障） |
| `ttl` | `int32` | 3 | — | 建议下游缓存秒数 |

### message `SaveOpsConfigReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `cfg_key` | `string` | 2 | — | — |
| `cfg_value` | `string` | 3 | — | — |
| `value_type` | `string` | 4 | — | 空默认 string |
| `scope` | `string` | 5 | — | 空默认 global |
| `expect_version` | `int64` | 6 | — | 乐观锁：0 表示新建；非 0 表示基于该版本更新 |
| `state` | `int32` | 7 | — | 0 视为 1 |
| `remark` | `string` | 8 | — | 变更原因（进审计） |

### message `SaveOpsConfigReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `config` | [`ConfigItem`](#message-configitem) | 1 | — | — |

### message `TaskStepSpec`

> --- 6. 管理任务编排（批量运营动作，真实执行只走下游 RPC） --- / TaskStepSpec 提交任务时的步骤声明。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `target_type` | `string` | 1 | — | submission/episode/rights_window/moderation_appeal |
| `target_id` | `string` | 2 | — | 目标聚合 ID（字符串，兼容不同类型主键） |

### message `TaskInfo`

> TaskInfo 任务投影。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task_id` | `int64` | 1 | — | — |
| `task_type` | `string` | 2 | — | batch_offline_submission / batch_offline_episode / batch_expire_window / batch_process_appeal |
| `params` | `string` | 3 | — | 任务级参数（JSON 文本） |
| `state` | `string` | 4 | — | pending/running/succeeded/partial/failed/canceled |
| `request_id` | `string` | 5 | — | 幂等键 |
| `total` | `int32` | 6 | — | — |
| `succeeded` | `int32` | 7 | — | — |
| `failed` | `int32` | 8 | — | — |
| `progress` | `int32` | 9 | — | 已执行步数（succeeded + failed） |
| `operator_id` | `int64` | 10 | — | — |
| `trace_id` | `string` | 11 | — | — |
| `ctime` | `int64` | 12 | — | — |
| `mtime` | `int64` | 13 | — | — |
| `started_at` | `int64` | 14 | — | — |
| `finished_at` | `int64` | 15 | — | — |

### message `TaskStepInfo`

> TaskStepInfo 任务步骤投影。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `task_id` | `int64` | 2 | — | — |
| `step_no` | `int32` | 3 | — | — |
| `target_type` | `string` | 4 | — | — |
| `target_id` | `string` | 5 | — | — |
| `state` | `string` | 6 | — | pending/running/succeeded/failed/skipped/canceled |
| `result` | `string` | 7 | — | 下游返回摘要 |
| `err_msg` | `string` | 8 | — | 失败原因（脱敏，不含堆栈与密钥） |
| `ctime` | `int64` | 9 | — | — |
| `mtime` | `int64` | 10 | — | — |

### message `SubmitAdminTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `task_type` | `string` | 2 | — | — |
| `params` | `string` | 3 | — | JSON 文本，如 {"reason":"版权到期"} |
| `steps` | [`TaskStepSpec`](#message-taskstepspec) | 4 | repeated | 批量目标，最多 1000 步 |

### message `SubmitAdminTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task` | [`TaskInfo`](#message-taskinfo) | 1 | — | — |
| `reused` | `bool` | 2 | — | true 表示 request_id 命中已有任务（幂等返回） |

### message `GetAdminTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `task_id` | `int64` | 2 | — | — |
| `request_id` | `string` | 3 | — | 与 task_id 二选一 |

### message `GetAdminTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task` | [`TaskInfo`](#message-taskinfo) | 1 | — | — |
| `steps` | [`TaskStepInfo`](#message-taskstepinfo) | 2 | repeated | — |

### message `ListAdminTasksReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `state` | `string` | 2 | — | 空表示全部 |
| `task_type` | `string` | 3 | — | 空表示全部 |
| `operator_id` | `int64` | 4 | — | 0 表示全部 |
| `pn` | `int32` | 5 | — | — |
| `ps` | `int32` | 6 | — | — |

### message `ListAdminTasksReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`TaskInfo`](#message-taskinfo) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |

### message `CancelAdminTaskReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `task_id` | `int64` | 2 | — | — |
| `reason` | `string` | 3 | — | — |

### message `CancelAdminTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task` | [`TaskInfo`](#message-taskinfo) | 1 | — | — |

### message `RunAdminTaskReq`

> RunAdminTaskReq 由 services/cron 或人工触发的任务推进入口。 / 本服务不内置分发 worker（定时进程归 services/cron，AGENTS.md §3）， / 执行步骤时只调用 video/catalog/rights/moderation 的 RPC。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `task_id` | `int64` | 2 | — | — |
| `max_steps` | `int32` | 3 | — | 单次推进的最大步数，<=0 时按服务端默认（100） |

### message `RunAdminTaskReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `task` | [`TaskInfo`](#message-taskinfo) | 1 | — | — |
| `steps` | [`TaskStepInfo`](#message-taskstepinfo) | 2 | repeated | — |
| `executed` | `int32` | 3 | — | 本次实际执行步数 |

### message `AuditIndexItem`

> --- 7. 管理操作审计索引 --- / AuditIndexItem 审计索引投影。 / 边界：本服务只存“谁在何时对哪个聚合做了什么、结果如何”的索引与 trace_id； / 完整证据（请求正文、审核截图、前后快照）由被操作的领域服务和未来的 / services/audit（append-only 存储）保留，见 README。

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `id` | `int64` | 1 | — | — |
| `admin_id` | `int64` | 2 | — | — |
| `username` | `string` | 3 | — | 展示用冗余（从 op_admin_user 关联） |
| `action` | `string` | 4 | — | 动作，如 admin_user.create、ops_config.save |
| `resource_type` | `string` | 5 | — | 目标类型 |
| `resource_id` | `string` | 6 | — | 目标 ID |
| `result` | `string` | 7 | — | ok/denied/error |
| `ip_hash` | `string` | 8 | — | 来源 IP 哈希（不落明文） |
| `user_agent` | `string` | 9 | — | UA（截断） |
| `trace_id` | `string` | 10 | — | — |
| `request_id` | `string` | 11 | — | — |
| `ctime` | `int64` | 12 | — | — |

### message `ListAuditIndexReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `ctx` | [`OpContext`](#message-opcontext) | 1 | — | — |
| `admin_id` | `int64` | 2 | — | 0 表示全部 |
| `action` | `string` | 3 | — | — |
| `resource_type` | `string` | 4 | — | — |
| `resource_id` | `string` | 5 | — | — |
| `start_at` | `int64` | 6 | — | Unix 秒，含 |
| `end_at` | `int64` | 7 | — | Unix 秒，不含 |
| `pn` | `int32` | 8 | — | — |
| `ps` | `int32` | 9 | — | — |

### message `ListAuditIndexReply`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `items` | [`AuditIndexItem`](#message-auditindexitem) | 1 | repeated | — |
| `total` | `int64` | 2 | — | — |
