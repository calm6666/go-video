-- =====================================================================
-- operation 服务 - 管理后台 RBAC：账号 / 角色 / 权限点 / 绑定 / 菜单
-- =====================================================================
-- 数据库：go_video_operation（取自 services/operation/etc/operation.v1.yaml 的
--   DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
-- 数据所有者：operation 服务（AGENTS.md §5「账号/会话」以外的后台管理员域，
--   与终端用户账号体系完全隔离——account 管 C 端账号，本库只存后台管理员）。
--   其他服务不得直连本库；gateway/admin 只通过 operation.v1.rpc 读写（AGENTS.md §3）。
--
-- 表与代码对应关系（列名严格取自 services/operation/model/*.go 的 db tag 与 SQL 字符串）：
--   * op_admin_user       model/admin_user.go AdminUser      —— 管理员账号（口令只存散列）
--   * op_role             model/role.go Role                 —— 角色
--   * op_permission       model/permission.go Permission     —— 权限点 (resource, action)
--   * op_role_permission  model/role.go GrantPermissions     —— 角色↔权限点
--   * op_admin_role       model/role.go AssignRoles          —— 管理员↔角色
--   * op_menu             model/menu.go Menu                 —— 后台菜单树（仅 Web 后台，项目不支持小程序）
--
-- 唯一键与幂等设计（都是代码实际依赖，不是装饰）：
--   1. op_admin_user.uniq_username：AdminUser.Insert 用
--      `INSERT ... ON DUPLICATE KEY UPDATE mtime = mtime` + RowsAffected==0 判定重名
--      （model.ErrAdminExists），没有该唯一索引就无法做到「不依赖 driver 错误码」的并发安全新建。
--   2. op_role.uniq_name：CreateRole 的 FindByName 预检之外的兜底，并发建同名角色时只有一次成功。
--   3. op_permission.uniq_resource_action：CreatePermission 的 (resource, action) 唯一性依据
--      （model.ErrPermissionExists）；权限判定按「资源模式 + 动作模式」匹配，不允许同对重复登记。
--   4. op_role_permission 主键 (role_id, permission_id)：GrantPermissions 在事务内
--      先 DELETE 再逐条 INSERT，主键即并发下的去重保证。
--   5. op_admin_role 主键 (admin_id, role_id)：AssignRoles 同上（先删后插），
--      并让「同一管理员同一角色」不可能出现重复行，MemberCount 统计才不会虚高。
--   菜单不做唯一键：name 允许重复（层级由 parent_id + sort 表达），本期无按 name 查询路径。
--
-- 敏感信息约束（AGENTS.md §7、docs/data-design.md §6）：
--   * password_hash 只存 `pbkdf2_sha256$迭代数$盐hex$派生密钥hex`（internal/repository/password.go），
--     任何 RPC 出参都不返回该列；pwd_algo 保留算法标识，便于后续平滑升级散列。
--   * two_factor_target 是二次校验投递目标（手机号等），只用于服务端发送校验码，
--     出参仅返回「是否启用」布尔值（AdminUserView.SecondFactorEnabled）。
--
-- 索引取自真实查询路径：
--   * op_admin_user：FindByUsername(username)、FindMany(admin_id IN)、
--     List(state + username LIKE，ORDER BY admin_id DESC 分页) → idx_state_admin_id。
--   * op_role：FindByName、List(state + name/title LIKE，ORDER BY role_id ASC) → idx_state_role_id。
--   * op_permission：FindByResourceAction、FindMany(permission_id IN)、
--     List(domain 过滤，ORDER BY domain,resource,action) → uniq_resource_action + idx_domain。
--   * op_admin_role：ListRoleBindings(admin_id IN，ORDER BY admin_id,role_id) 走主键；
--     CountMembers/MemberAdminIDs(role_id) 走 idx_role_id。
--   * op_menu：ListAll(ORDER BY parent_id,sort,menu_id) 与 CountChildren(parent_id) 走 idx_parent_sort。
--
-- owner：运营平台（operation 服务）；影响范围：仅新增 6 张 RBAC 表，不改动任何既有表。
-- 回滚（表内是权限拓扑与账号凭证，DROP 前必须先备份，否则后台无人可登录）：
--   DROP TABLE IF EXISTS `op_menu`;
--   DROP TABLE IF EXISTS `op_admin_role`;
--   DROP TABLE IF EXISTS `op_role_permission`;
--   DROP TABLE IF EXISTS `op_permission`;
--   DROP TABLE IF EXISTS `op_role`;
--   DROP TABLE IF EXISTS `op_admin_user`;
-- 锁风险：全部为新建空表，索引在空表上建立，无在线锁风险；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- 不使用跨服务外键（AGENTS.md §5）：operator/admin_id/role_id 只作逻辑引用，
--   服务内一致性由 internal/repository 的事务与 model.Err* 保证。
-- =====================================================================

-- 管理员账号表。state：1 正常、2 禁用、3 锁定（locked_until 到期后可重试）。
-- fail_count/locked_until 是 AdminLogin 的防爆破字段：口令或二次校验码错一次即 +1，
-- 达到 Login.MaxFail 时置 state=3 并写 locked_until，登录成功时清零（model.UpdateLoginGuard/TouchLogin）。
CREATE TABLE IF NOT EXISTS `op_admin_user` (
  `admin_id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '管理员 ID（主键，即 OpContext.operator_id）',
  `username`          VARCHAR(32)  NOT NULL COMMENT '登录账号名（3-32 位小写字母/数字/下划线/点，全局唯一）',
  `password_hash`     VARCHAR(255) NOT NULL COMMENT '口令散列 pbkdf2_sha256$迭代数$盐hex$派生密钥hex（禁止任何接口返回该列）',
  `pwd_algo`          VARCHAR(32)  NOT NULL DEFAULT 'pbkdf2_sha256' COMMENT '口令算法标识，便于后续升级散列时按行平滑迁移',
  `state`             TINYINT      NOT NULL DEFAULT 1 COMMENT '账号状态：1 正常、2 禁用、3 锁定（防爆破）',
  `fail_count`        INT          NOT NULL DEFAULT 0 COMMENT '连续登录失败次数，登录成功清零',
  `locked_until`      BIGINT       NOT NULL DEFAULT 0 COMMENT '锁定截止时间（Unix 秒），0 表示未锁定；到期后允许重试',
  `last_login_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次成功登录时间（Unix 秒），0 表示从未登录',
  `remark`            VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注（岗位、责任范围），最长 255 字符',
  `two_factor_target` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '二次校验投递目标（空表示该账号未启用二次校验；不外发原文）',
  `operator`          BIGINT       NOT NULL DEFAULT 0 COMMENT '最后修改该账号的管理员 ID，0 表示系统初始化',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`admin_id`),
  UNIQUE KEY `uniq_username` (`username`),
  -- ListAdminUsers：state 过滤 + admin_id DESC 分页
  KEY `idx_state_admin_id` (`state`, `admin_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='后台管理员账号表：只存口令散列与防爆破计数，与 C 端 account 库无任何共享';

-- 角色表。state：1 启用、2 停用；停用的角色在 LoadAdminGrants 的 JOIN 条件里
-- 直接被排除（r.state = 1），因此不需要删除绑定即可整体收权。
CREATE TABLE IF NOT EXISTS `op_role` (
  `role_id`  BIGINT      NOT NULL AUTO_INCREMENT COMMENT '角色 ID（主键）',
  `name`     VARCHAR(64) NOT NULL COMMENT '角色标识（英文，全局唯一，授权快照与审计按此展示）',
  `title`    VARCHAR(64) NOT NULL DEFAULT '' COMMENT '角色显示名（最长 64 字符）',
  `state`    TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 启用、2 停用（停用即整体收权）',
  `operator` BIGINT      NOT NULL DEFAULT 0 COMMENT '最后修改人 admin_id',
  `ctime`    BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`    BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`role_id`),
  UNIQUE KEY `uniq_name` (`name`),
  KEY `idx_state_role_id` (`state`, `role_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='后台角色表：权限判定取「管理员所挂角色」的并集';

-- 权限点表。resource/action 支持通配（repository/rbac.go）：
--   "*" 全匹配、"video:*" 域内通配、"*:read" 跨域只读；判定为「存在一条覆盖即放行」。
-- domain 仅用于列表分组（video/catalog/rights/moderation/operation/system），未传时取 resource 前缀。
CREATE TABLE IF NOT EXISTS `op_permission` (
  `permission_id` BIGINT       NOT NULL AUTO_INCREMENT COMMENT '权限点 ID（主键）',
  `resource`      VARCHAR(64)  NOT NULL COMMENT '资源标识或模式（如 video:submission、admin_user、video:*），最长 64',
  `action`        VARCHAR(64)  NOT NULL COMMENT '动作标识或模式（如 offline、read、*:read），最长 64',
  `domain`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '所属域（列表分组用，小写；未指定时取 resource 冒号前缀）',
  `description`   VARCHAR(255) NOT NULL DEFAULT '' COMMENT '权限点说明（最长 255 字符）',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`permission_id`),
  UNIQUE KEY `uniq_resource_action` (`resource`, `action`),
  -- ListPermissions：domain 过滤后按 domain,resource,action 排序
  KEY `idx_domain_resource_action` (`domain`, `resource`, `action`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='权限点表（resource+action，支持通配模式），是 RBAC 的最小授权单位';

-- 角色↔权限点绑定。只追加/整体重建（GrantPermissions 先 DELETE 再 INSERT），
-- 因此无 mtime 列；权限变更的收敛靠 op:rbac:ver 版本号失效，见 repository/rbac.go。
CREATE TABLE IF NOT EXISTS `op_role_permission` (
  `role_id`       BIGINT NOT NULL COMMENT '角色 ID',
  `permission_id` BIGINT NOT NULL COMMENT '权限点 ID',
  `ctime`         BIGINT NOT NULL DEFAULT 0 COMMENT '绑定时间（Unix 秒）',
  PRIMARY KEY (`role_id`, `permission_id`),
  -- 反向查询与「权限点被哪些角色引用」的排障路径
  KEY `idx_permission_id` (`permission_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='角色-权限点绑定表（主键即去重键，一个角色不会重复挂同一权限点）';

-- 管理员↔角色绑定。DeleteRole 与 AssignRoles 都会按 role_id/admin_id 整体删除重建。
CREATE TABLE IF NOT EXISTS `op_admin_role` (
  `admin_id` BIGINT NOT NULL COMMENT '管理员 ID',
  `role_id`  BIGINT NOT NULL COMMENT '角色 ID',
  `ctime`    BIGINT NOT NULL DEFAULT 0 COMMENT '绑定时间（Unix 秒）',
  PRIMARY KEY (`admin_id`, `role_id`),
  -- CountMembers（删除角色前的「仍有成员」校验）、MemberAdminIDs、ListRoleBindings JOIN
  KEY `idx_role_id_admin_id` (`role_id`, `admin_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='管理员-角色绑定表（主键即去重键，同一管理员不会重复挂同一角色）';

-- 后台菜单表（树形用 parent_id 表达，0 表示根节点）。
-- 可见性依据「角色权限并集 + required_permission」由服务端计算（GetMenu），前端不硬编码；
-- SaveMenu 全量替换后自增 op:menu:ver 版本号整体失效缓存。
CREATE TABLE IF NOT EXISTS `op_menu` (
  `menu_id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '菜单节点 ID（主键）',
  `parent_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT '父节点 ID，0 表示根节点（禁止自引用，见 ErrMenuParentSelf）',
  `name`                VARCHAR(64)  NOT NULL COMMENT '节点标题（最长 64 字符，允许同名）',
  `path`                VARCHAR(255) NOT NULL DEFAULT '' COMMENT '后台路由路径（最长 255 字符）',
  `icon`                VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '图标标识（最长 64 字符）',
  `sort`                INT          NOT NULL DEFAULT 0 COMMENT '同级排序，越小越前',
  `required_permission` VARCHAR(160) NOT NULL DEFAULT '' COMMENT '可见性所需权限点，格式 resource#action（空表示登录后即可见）',
  `state`               TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 显示、2 隐藏',
  `ctime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`               BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`menu_id`),
  -- ListAll 的 ORDER BY parent_id, sort, menu_id 与 CountChildren(parent_id)
  KEY `idx_parent_sort` (`parent_id`, `sort`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='后台菜单树（仅 Web 管理后台使用；项目不支持小程序，无端专属菜单表）';
