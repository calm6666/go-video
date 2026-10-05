# operation

管理后台领域服务：管理员账号与 RBAC、菜单、运营配置（一键值对）、批量运营任务编排、
后台会话与管理操作审计索引。本服务是 `op_*` 11 张表的数据所有者
（见 [AGENTS.md §3/§5](../../AGENTS.md)），纯 gRPC 服务，不提供 HTTP；
`gateway/admin` 只做入口聚合、统一鉴权与限流。

## 职责

- **持有数据**：`op_admin_user`、`op_role`、`op_permission`、`op_role_permission`、
  `op_admin_role`、`op_menu`、`op_config`、`op_admin_task`、`op_admin_task_step`、
  `op_audit_index`、`op_admin_session`（库名 `go_video_operation`）。
- **身份隔离**：管理员身份（`op_admin_user`）与终端用户账号（`account.mid`）完全隔离，
  不读写对方的表；后台 token（`adm_<issuer>_<random>_<hmac>`）与用户 token 互不通用
  （`rpc/operation.proto:16-18`、`internal/repository/session.go:106-118`）。
- **编排而非代写**：稿件下架、目录集下架、版权窗口失效、申诉驳回一律通过
  `video`/`catalog`/`rights`/`moderation-orchestrator` 的公开 RPC 完成
  （`internal/repository/downstream.go:25-55`），本服务不直连它们的库表。
- **权限判定源**：`VerifyAdminPermission` 是 `gateway/admin` 每条受保护路由的判定入口，
  结果按 `Cache.PermissionTTL` 在网关侧短缓存；授权写入会立即失效服务端快照
  （`etc/operation.v1.yaml:34-37`）。

## gRPC API

package `operation.v1`，端口 8109，etcd key `operation.v1.rpc`，22 个方法。
分组与 `rpc/operation.proto` 的契约分段一致，不得平铺。

### 1. 登录与权限校验

| 方法 | 说明 |
|---|---|
| `AdminLogin` | 口令校验（PBKDF2-HMAC-SHA256）+ 防爆破锁定 + 可选二次校验码，签发后台 token |
| `VerifyAdminPermission` | 按 `resource + action` 判定，gateway/admin 每请求调用，结果带短缓存 |

### 2. 管理员账号

| 方法 | 说明 |
|---|---|
| `CreateAdminUser` | 创建账号（口令只出现在入参，响应永不返回散列） |
| `UpdateAdminUser` | 备注/状态/重置口令；重置口令吊销全部会话 |
| `DisableAdminUser` | 禁用并吊销会话 |
| `ListAdminUsers` | 分页查询（只回 `second_factor_enabled` 布尔，不外泄校验目标） |

### 3. 角色与权限点

| 方法 | 说明 |
|---|---|
| `AssignRoles` | 全量覆盖管理员角色（并集生效） |
| `CreateRole` | 创建角色并绑定权限点 |
| `ListRoles` / `DeleteRole` | 分页；仍有成员时拒绝删除 |
| `ListPermissions` / `CreatePermission` | 分页；`resource + action` 唯一 |

### 4. 菜单

| 方法 | 说明 |
|---|---|
| `GetMenu` | 按管理员角色并集返回可见菜单树（后台 Web 专用） |
| `SaveMenu` | 新建/更新菜单节点 |

### 5. 运营配置（后台自用键值）

| 方法 | 说明 |
|---|---|
| `GetOpsConfig` | 默认走缓存，`refresh=true` 强制回源 |
| `SaveOpsConfig` | `expect_version` 乐观锁 + 操作者留痕 |

### 6. 批量运营任务

| 方法 | 说明 |
|---|---|
| `SubmitAdminTask` | `request_id` 幂等建任务，状态 `pending`，步骤展开入库 |
| `GetAdminTask` / `ListAdminTasks` | 任务与步骤明细、分页 |
| `CancelAdminTask` | 仅 `pending`/`running` 可取消 |
| `RunAdminTask` | 逐步骤调用下游 RPC；**本服务不内置 worker**（`rpc/operation.proto:507`） |

支持的 4 种任务类型与下游映射（`internal/repository/task.go:29-32`、`:402-444`）：

| task_type | 下游调用 |
|---|---|
| `batch_offline_submission` | `video.TransitionState(STATE_OFFLINE)` |
| `batch_offline_episode` | `catalog.OfflineEpisode` |
| `batch_expire_window` | `rights.ExpireWindow` |
| `batch_reject_appeal` | `moderation-orchestrator.ProcessAppeal(VERDICT_REJECT)` |

### 7. 审计索引

| 方法 | 说明 |
|---|---|
| `ListAuditIndex` | 查询管理操作审计索引（正文证据在下游服务/`audit`） |

## 数据模型与迁移

| 表 | 唯一约束 | 用途 |
|---|---|---|
| `op_admin_user` | `uniq_username` | 管理员主表、口令散列、登录护栏计数 |
| `op_role` | `uniq_name` | 角色 |
| `op_permission` | `uniq_resource_action(resource, action)` | 权限点 |
| `op_role_permission` / `op_admin_role` | 关系表 | RBAC 授权 |
| `op_menu` | 主键 `menu_id` + `KEY idx_parent_sort`（**无唯一键**：允许同名节点） | 菜单树，`required_permission` 形如 `resource#action` |
| `op_config` | `uniq_cfg_key_scope(cfg_key, scope)` | 后台一键值对 + 乐观锁版本 |
| `op_admin_task` | `uniq_request_id` | 批量任务（幂等键） |
| `op_admin_task_step` | `uniq_task_step(task_id, step_no)` | 步骤明细与逐行结果 |
| `op_audit_index` | — | 「谁在何时对哪个聚合做了什么」+ trace/request 关联 |
| `op_admin_session` | — | 后台会话（token 摘要、过期、吊销状态） |

迁移位于 `deploy/migrations/operation/`：`000001`~`000003` 建表、
`000004`~`000006` 权限与角色种子（种子↔`gateway/admin` 权限点有漂移门禁）。
迁移只在隔离实例（`127.0.0.1:3399`，数据目录 `.gotmp/mysql-data`）应用与对账；
用户本机 3306 实例从未被本项目迁移写入。

## 登录与会话链路

- 口令：`pbkdf2_sha256$<迭代数>$<盐 hex>$<派生密钥 hex>`（`internal/repository/password.go:12`），
  强度下限 `passwordStrengthOK`（`:46`），不采用参考仓库的 MD5+盐。
- 防爆破：连续失败达 `Login.MaxFail`（默认 5）进入锁定态 `Login.LockMinutes`（默认 15 分钟），
  锁定期满可重试，成功清零（`internal/repository/admin_user.go:26-29`、`:146-171`、`:284-285`）；
  **二次校验码错也计入同一计数**，防止短信码在线爆破（`:226-234`）。
- 不存在账号与口令错同一响应（`:202-204`），不透露账号存在性。
- token：`adm_<issuer>_<random>_<hmac-sha256>`，`hmac.Equal` 定长比较（`internal/repository/session.go:106-134`）；
  签名密钥只从 `AdminSession.TokenSecretRef` 指向的环境变量读取，
  缺失时 `AdminLogin` 直接失败而**不签发无摘要段弱 token**（`internal/svc/servicecontext.go:78-93`）。
- 会话缓存 miss 才回源；吊销走 `RevokeSession`/`RevokeAllSessions` 并删缓存（`internal/repository/session.go:159-236`）。

## 审计口径

`op_audit_index` 只存索引：动作、对象类型与 ID、结果、trace_id/request_id、
IP 的 sha256 短哈希（加固定前缀盐，`internal/repository/audit.go:28-30`）、
剥掉凭证片段后截断的 UA。口令、token、明文 IP 一律不落（AGENTS.md §7、docs/data-design.md §6）。

## 已知缺口

按影响分组编号，每条给出口位置；改动前先复核行号。

### A. 任务执行没有调度方（发布阻塞）

1. **`RunAdminTask` 只能人工点**。全仓没有任何调度方 import `services/operation/rpc`
   （`cron` 服务也不引用；探针：`grep -rln "services/operation/rpc"` 结果只有
   `gateway/admin` 与本服务），`deploy/migrations/cron/000001`~`000002` 没有为
   `operation.RunAdminTask` 登记任何 `cron_task_definition` 种子。
   于是 `SubmitAdminTask` 建出来的 `pending` 任务**永远不会自己推进**，
   批量下架/版权失效/申诉驳回全部依赖有人点 `POST /admin/operation/task/run`
   （`gateway/admin/api/admin.api:2336`、`docs/api/http/admin/13-admin-operation.md:117`）。契约注释也承认这一点
   （`rpc/operation.proto:507`：「由 cron 或人工触发，本服务不内置 worker」）。

### B. 管理员二次校验不可用（发布阻塞）

2. **启用了 2FA 的管理员登不进来，而且会被锁死**。`checkSecondFactor` 在
   `two_factor_target` 非空时强制要求带码，并向 `account.CheckCapture` 校验
   （`internal/repository/admin_user.go:267-277`、调用点 `:226-234`，
   `internal/repository/downstream.go:151-168`），
   但**本仓库没有任何发送通道**：`grep -rn "SendCapture" services/operation gateway/admin`
   在非测试代码里零命中，`gateway/admin/api/admin.api` 也没有「给管理员发二次校验码」的路由。
   Redis 里因此永远不会出现 `cap_code_1_<target>`（account 的验证码键派生见
   `services/account/internal/repository/login.go:81-82`，biz=1 即登录），带码必错；
   而错码计入防爆破（`admin_user.go:226-234`），5 次后账号进入锁定态。
   即「配置了 2FA = 该管理员被永久拒之门外」，属于配置项与链路不对称的真实缺陷。

### C. 审计面割裂与陈旧说明

3. **两个审计面互不关联**：本服务只写自有 `op_audit_index`，从不调用 `audit.AppendAudit`
   （`grep -rn "AppendAudit|services/audit/rpc" services/operation` 非测试代码零命中）。
   对比 `ops-config`（10 个写 logic 全部转发 `audit`，并把 `entry_id` 记进版本行），
   管理员动作在 append-only 哈希链存证里**没有条目**，`audit.VerifyAuditChain` 也覆盖不到后台写操作。
4. **`internal/repository/audit.go:5` 的边界说明已过期**：它写着
   「与 `services/audit` 的边界（该服务本期未实现，见 README）」。
   `services/audit` 早已实现（14 个 RPC 方法，`gateway/admin` 已接入 12 个 logic 文件），
   这句话现在会把读者引向「不必接线」的错误结论。修缺口 3 时一并订正。

### D. 配置所有权仍未裁决

5. `op_config`（本服务，一键值对 + 乐观锁）与 `ops_config_item`（`ops-config`，
   发布式版本 + 灰度 + 缓存失效）**并存且没有搬迁计划**，
   裁决口径见 `services/ops-config/README.md`「所有权结论与待评审」与
   `services/ops-config/rpc/opsconfig.proto:24-30`。
   新增配置该写哪一侧，目前只靠注释约定，没有服务端拦截。

### E. 契约口径不一致

6. **`task_type` 的契约名与实现不符**：`rpc/operation.proto:330` 与由其生成的
   `docs/api/rpc/operation.md:455`、以及 `gateway/admin/api/admin.api:2066`
   都写 `batch_process_appeal`，但 `model.ValidTaskType` 只接受
   `batch_reject_appeal`（`model/errors.go:135`、`:139-147`），
   实现也只发 `VERDICT_REJECT`（`internal/repository/downstream.go:127-135`）。
   按文档提交的操作者会立刻拿到 `ErrTaskTypeUnknown`。
   正确修法是把两处源注释改成 `batch_reject_appeal` 再重新生成 pb / 接口文档，
   而不是放宽校验去接受一个没有实现的动作名。

### F. 覆盖边界

7. `internal/logic` 的用例走进程内替身（`fakes_test.go`），
   下游 RPC 的失败注入覆盖「不可用→步骤 failed」，
   但真库联跑与 gRPC 端到端联调没有做过；SQL 文本只由静态对账保证。
8. `op_audit_index` 的哈希/脱敏规则有直接断言，
   跨服务 trace_id 关联（本服务索引 ↔ audit 条目）没有用例，因为链路本身未接（缺口 3）。

## 运行

```powershell
# 从仓库根目录生成（框架代码由 goctl 产出；logic/repository/model/迁移是源文件）
./scripts/gen.ps1 -Service operation

# 运行（纯 RPC）
go run ./services/operation -f services/operation/etc/operation.v1.yaml

# 健康检查：gRPC health 探针（grpc_health_probe -addr=127.0.0.1:8109）
```

后台 token 签名密钥必须在环境里注入 `OPERATION_ADMIN_TOKEN_SECRET`
（`AdminSession.TokenSecretRef` 只是环境变量名，真实密钥进 Secret/Vault）。

## 关键约束

- 只暴露 gRPC；HTTP 入口、鉴权中间件与限流在 `gateway/admin`。
- 不写其它领域库表；业务写一律走下游公开 RPC，未配置下游时步骤 `failed`
  并写明「downstream not configured」，绝不静默成功（`downstream.go:8-9`、`:176-207`）。
- 缓存（权限快照/配置/菜单/会话）不可用即退化直连 MySQL，判定结果不变；
  缓存只是加速，事实源恒在库里。
- 不提供订单、支付、投币、广告投放与广告位配置能力（AGENTS.md §1）。

## 测试覆盖

离线单测（纯 Go，不连 MySQL/Redis/gRPC，AGENTS.md §9）。数字来自导出明细，格式 `顶层/子用例`
（`top` 已排除 `TestMain`）。规模合计 **161 个顶层用例 + 34 个子用例**，0 条 skip。

### 1. `internal/logic`（4 个用例文件 + `fakes_test.go`）— `81/26`

| 文件 | 顶层/子 | 钉住了什么 |
|---|---|---|
| `admintask_logic_test.go` | 24/15 | `SubmitAdminTask` 落库并留痕、守卫拒绝后**零依赖调用**、步骤数超上限整批拒、同目标去重且步号连续、同 `request_id` 重放返回既有任务且不重复建步骤、幂等键大小写不敏感命中、步骤写入失败任务留 `pending` 且无步骤、回读失败原样外传、审计写失败时任务与步骤已成立；`GetAdminTask` 按 `task_id`/`request_id` 定位与二者同给时以 `task_id` 为准；`ListAdminTasks` 分页钳制、未知筛选值在查询前拒、六个合法状态都可筛、列表不返回步骤明细 |
| `menu_logic_test.go` | 22/3 | `SaveMenu` 入参校验全部先于任何 SQL 与缓存写、新建回传真实主键与时间戳、孤儿节点直接入库但让后续合法更新被父链上溯拒、父级指向自身/父节点不存在/父链成环拒、更新的 `ctime`/`mtime` 恒 0、`required_permission` **不**校验权限点是否存在、审计写失败时节点与缓存失效已生效；`GetMenu` 按角色并集过滤并缓存全量树、第二次读命中两级缓存不再打库、`ttl` 取自配置而非实现兜底、不同管理员共用同一份树缓存但可见集合不同、写后立即读到新树且旧槽不再被引用、允许代查他人菜单且读接口不写审计、账号非正常态拒答且不碰菜单表、空树也回填缓存 |
| `permission_logic_test.go` | 18/3 | `CreatePermission` 应答与库里那一行一致、显式 `domain` 只小写去空白、资源为通配符时 `domain` 退化为通配符、描述 255 字通过而 256 字被拒、`domain` 无长度校验超长原样入库、重复权限点返回哨兵且零副作用、并发唯一冲突**原文外传**而非可识别哨兵、写入失败不碰缓存与审计、审计写失败时权限点与缓存失效已生效；`ListPermissions` 域筛选真的进了 SQL、域参数只去空白不转小写、空域不过滤并整表排序、分页钳制与翻页切片、读接口不打缓存、投影不回传敏感列 |
| `adminlogin_logic_test.go` | 17/5 | 成功签发会话并留痕、账号名归一后查库、**口令错与账号不存在同类错误**（不透露存在性）、连续失败到阈值锁定、锁定期满且口令对则清零、禁用账号拒登、二次校验**缺码不计**爆破计数而**错码计入**、二次校验通道不可用则不签发、摘要密钥缺失先于任何库写拒绝、入参守卫零依赖调用、纯空白口令按口令错处理、散列损坏不降级为口令错、拒绝时审计仍归因到账号、审计写失败不回滚业务结果、计数写失败不留痕、凭证与二次校验目标不外泄 |
| `fakes_test.go` | 0/0 | 替身层与注入缝（见 §4），只含 `TestMain` 注入，不贡献用例数 |

### 2. 其他层

| 层 | 文件 | 顶层/子 | 内容 |
|---|---|---|---|
| `internal/repository` | 7 + `fakes_test.go` | 63/6 | `password_test.go`(13/3) PBKDF2 固定向量、散列格式与校验、损坏散列拒、强度与迭代数下限、账号名检查、二次校验目标归一、防爆破计数固定向量、锁定判定、选项默认值、分页对、操作者必填；`task_test.go`(10/2) 参数归一、提交校验、步骤去重重编号、`request_id` 幂等、取消非法态拒、**并发推进者输**、终态幂等、步骤标记守状态机；`admin_user_test.go`(9/0) 签发/失败计数/阈值锁定/禁用/未知账号同错误/必须配置签名密钥/二次校验 fail-closed/空口令/弱口令先于任何写入拒；`rbac_test.go`(9/0) token 与权限点匹配、角色并集、畸形授权忽略、跨角色去重、非正常账号拒、入参守卫；`ops_config_test.go`(9/0) `expect_version=0` 新建、已存在键需版本、版本冲突、**读写之间版本丢失**、成功 bump、缺行与操作者守卫、值校验、scope/类型归一、未找到与缓存守卫；`session_test.go`(8/0) 无密钥拒签、签发解析往返、token 唯一、issuer 归一、TTL 回落、未知与已吊销拒、缓存会话不泄漏凭证；`audit_test.go`(5/1) 主体脱敏、IP 哈希确定但非明文、截断 rune 安全、UA 脱敏、动作命名约定 |
| `model` | 3 | 16/1 | `role_seed_test.go`(6/0) 种子覆盖每个权限域、派生绑定迁移不得落后权限点种子、结构抗漂移、`readonly` 非空壳；`migration_sync_test.go`(5/0) 迁移恰好覆盖 model 的表、逐列 ↔ struct tag、表自描述、幂等键存在、文件带 owner 与回滚；`task_test.go`(5/1) 任务与步骤状态机矩阵（手写规格对照实现）、终态无出边、终态判定、`ValidTaskType` |
| `internal/config` | 1 | 1/1 | `etc` 示例配置可被 go-zero 加载（子用例逐个文件），守住 `CacheRedis` 不与 `zrpc` 内嵌 `Redis` 同名 |
| `internal/svc` | 0 | — | **无离线单测**：`ServiceContext` 装配（密钥缺失时降级、客户端未配置）只被 logic 用例间接经过 |
| `internal/server` | 0 | — | **无离线单测**（goctl 生成壳，见 §5） |
| `internal/consumer` / `internal/policy` | — | — | 本服务没有这两层：任务推进不在本服务内置（见「已知缺口」A），策略判定内联在 `repository` 与 `model` |

### 3. 构造器级覆盖：`22/22`

探针取 `internal/logic` 全部 `NewXxxLogic` 构造器（22 个，与本服务对外的 22 个 RPC 方法一一对应），
逐个在 `*_test.go` 里查引用，`gaps:` 为空——每个方法都有构造器级用例，不依赖间接断言。

### 4. 替身层与断言口径

- `internal/logic/fakes_test.go` 的做法是 `repository.NewWithDeps(真实 Cache + 内存 Store, 内存 conn,
  内存 model, 下游)`：装的是**真实 Repository + 真实 Cache**，只把最外层依赖换成替身，
  于是「口令校验、防爆破锁定、会话签发与缓存回填、权限快照与版本失效、审计脱敏、角色并集判定」
  整条判定链都在被测路径上，而不是把 Repository 一起 mock 掉。
- 五条替身纪律：① 每次读返回值拷贝（共享指针会掩盖「有没有真的落库」）；② 写入按真实 SQL 口径
  复刻主键、列集与唯一约束（`op_admin_user` 用 `ON DUPLICATE KEY UPDATE mtime = mtime` +
  `RowsAffected==0` 判重名 → `model.ErrAdminExists`；`op_role` 撞 `uniq_name` 才是 1062；
  `op_role_permission` 不去重 → 重复权限点撞主键整体回滚）；③ 副作用按**顺序**记录（`callLog`），
  断完整序列而不是只数次数；④ 布数据走静默写入路径，不算被测调用；
  ⑤ `fakeConn` 内嵌 nil `sqlx.SqlConn`，任何绕过 model 直接发 SQL 的写法立刻 panic。
- `internal/repository/fakes_test.go`：内存 model + `conn` 为 nil，触达数据库即 panic；
  未实现的方法内嵌接口，被调用即 panic，用来暴露意外的依赖泄漏。
- 列表类替身把**拼出来的 WHERE 文本与绑定实参**记进轨迹，所以「筛选条件到底有没有进 SQL、
  按什么顺序进」是用例断言的一部分，而不只是「返回了几行」。
- 断言纪律：判别性前置 + 边界对照，不改生产代码做变异；禁止永真断言与吞错误；
  拒绝类用例断「零次 recorded op」，而不是「返回了错误」。
- 成本口径：PBKDF2 用生产默认迭代数 210000（非导出函数，测试不降强度），
  本包因此真实付出散列成本——「登录必须走真散列比对」本身就是被测结论。

### 5. 覆盖边界（不可省略）

- 用例不连接 MySQL、Redis、etcd，也不起 gRPC server/client；下游四个客户端
  （`video`/`catalog`/`rights`/`moderation-orchestrator`）是替身，「不可用→步骤 failed」
  是注入出来的结论（见「已知缺口」F7）。
- **迁移与真实库的对账只到静态一致 + 隔离实例**：`model/migration_sync_test.go` 逐列比对
  `model` struct tag 与 DDL，但 SQL 文本、列名与索引命中只能由静态对账保证；
  迁移按本 README「数据模型与迁移」节的声明，只在隔离实例（`127.0.0.1:3399`，
  数据目录 `.gotmp/mysql-data`）应用与对账，**未在目标/共享实例复验**，
  真库联跑与 gRPC 端到端联调没有做过。
- 乐观并发的 CAS 用「下一步会输」的静默开关复刻，验证的是实现拿到 `false` 之后的处置，
  **不等价于**真实 MySQL 的行锁与隔离级别行为；驱动返回的 matched vs changed rows 不在断言口径内。
- 缓存只复刻 Store 层字符串读写：用例只断下发给 `Setex` 的 ttl 数值，不模拟时间流逝后的自动失效。
- **数字与「跑绿」是两件事**（2026-10-03 的实测教训）：本节 161/34 在整树 `go test -p 1 -count=1 ./...`
  之前只被计数与编译检查过；整树第一次真跑本包时 `menu_logic_test.go` 有六处断言是红的，全部是
  **断言侧口径写错**，不是生产代码变了：
  ① 五处 RBAC 快照 key 写成斜杠形态 `op:rbac:0/1`，而生产是冒号分段
  `op:rbac:%d:%d`（`internal/repository/cache.go:22`，`rbac.go:126` 组装），已改为调用测试侧的
  `rbacSnapshotKey(version, adminID)`（`internal/logic/fakes_test.go`，刻意重写常量而不是引用生产，
  命名空间漂移必须让用例变红）；
  ② `SaveMenu` 孤儿节点用例多写了 `cache.Incr:op:menu:ver/2592000` 与一次重复的 `Get`——
  `menuVersion` 只在**首次分配版本**时 Incr（`internal/repository/menu.go:73-81`），而该用例前面那次
  `SaveMenu` 已经把 `op:menu:ver` 建到 1，所以这里不该再出现 Incr。
  两处修完都**没有放宽断言**：key 形态仍然逐字节钉住命名空间，序列仍然精确到每一次缓存调用。
  结论同 cron §9.6：**「用例数」不等于「用例跑过」**，每轮收口以整树 `go test -p 1` 为准。
- `internal/server`（goctl 生成的 gRPC server 壳）、`*.pb.go`、`internal/svc` 装配不在纯单测可达范围，
  本包只测到 logic 出入口。
- 跨服务 trace_id 关联（本服务索引 ↔ `audit` 条目）没有用例，因为链路本身未接（「已知缺口」C3）。

### 6. 验证命令

```powershell
go test -p 1 -count=1 ./services/operation/...   # -p 1 必须带：Windows 页面文件限制，并发跑多个测试包会 OOM(errno=1455)
gofmt -l services/operation                      # 必须为空
go vet ./services/operation/...                  # 应无输出
```

生成一致性另见 `./scripts/gen.ps1 -Service operation`（`.proto` 变更后必须重新生成，禁止手改
`internal/server`）。本节只声明覆盖范围与口径，不含任何一次运行的结论。
