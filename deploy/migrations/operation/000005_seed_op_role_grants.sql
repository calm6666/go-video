-- 000005_seed_op_role_grants.sql
-- 目的：为后台 RBAC 建一组**可解释的默认角色**，并把 000004 登记的权限点按域挂到角色上。
-- 计数口径：本文件不写死清单（角色与绑定全部由 INSERT...SELECT 现算），所以数字只是写作时的快照：
--   写作时 123 个权限点，2026-09-22 实测 162 个 / 29 个域，重放本迁移即自动覆盖新增点。
--
-- 为什么需要这条迁移：000004 只登记权限点，明写「不写角色绑定，谁拿哪个权限点属运营决策」。
-- 那一条判断本身没错，但它留下一个可证明的死锁：`AdminPermission` 中间件按「角色 → 权限点」判定，
-- 判定不中一律 403，而 `GrantPermissions` 只能挂**已存在**的权限点给**已存在**的角色。
-- 新库跑完 000001~000004 之后库里 0 个角色，于是任何入口都无从授权——
-- 后台能登录、能读，写入口对所有人 403，且只能靠人手写 123 条绑定 SQL 才能解锁。
-- 本迁移把「建角色 + 挂权限点」这段纯机械的工作做完，把真正的组织决策留在唯一一步：
-- **不把任何角色绑给任何账号**（本文件不写 `op_admin_role`，也不写 `op_admin_user`）。
-- 谁该拿到哪个角色仍由运营决定，只是从「手写 123 条授权 SQL」变成「挑一个已有角色挂上去」。
--
-- 角色集合的口径（全部是派生量，不是手工清单）：
--   1. `domain_<domain>`：每个 `op_permission.domain` 一个角色，取该域全部权限点。
--      域名与成员都由 `SELECT DISTINCT domain FROM op_permission` 现算，
--      所以加新域时本文件重放即自动多出一个角色，无需在此维护对照表。
--   2. `readonly`：全部 `action = 'read'` 的权限点（写作时 14 个，2026-09-22 实测 16 个），给排障/客服的最小安全面。
--   3. `super_admin`：全部权限点，仅作 break-glass；本文件不把它绑给任何账号。
--
-- 权限风险（必须读）：`domain_operation` 域含 `operation:role#create/delete/assign`、
--   `operation:admin_user#create/update/disable`、`operation:permission#create`，
--   即**拿到该角色就能给自己加权限**。它是「管理员管理」这一职能的完整权限，
--   不得作为日常运营岗的默认角色；日常岗用 `readonly` 或对应业务域角色。
--   `super_admin` 同理只用于一次性初始化与紧急恢复。
--
-- owner：运营平台（operation 服务，op_* 前缀，deploy/migrations/operation）；
--   影响范围：只 INSERT `op_role` 与 `op_role_permission`，不建表、不改表结构、不动账号与菜单。
--
-- 幂等口径：`op_role` 靠 uniq_name (name) 去重，`op_role_permission` 靠主键 (role_id, permission_id) 去重，
--   全部 INSERT IGNORE，可重复执行；已存在的角色不覆盖 title（运营可自行改名，重跑不会打回）。
--   `ctime/mtime` 固定 0、`operator` 固定 0：表示「由迁移建立，不是人工 CreateRole 建的」。
--   注意：**重放本文件才会并入新权限点**。`scripts/migrate.ps1` 跳过已记录版本，
--   因此以后新增权限点（0000NN）时，若希望它同时并入既有域角色，
--   要在新迁移里重复本文件末尾那三段派生 SELECT，而不是在本文件追加硬编码 id。
--
-- 缓存收敛：本服务用 `op:rbac:ver` 版本号失效「管理员 → 权限点」快照（internal/repository/rbac.go 的
--   invalidateRBAC），而 SQL 迁移不触发它。新库无缓存，不受影响；在已有实例上重放本迁移时，
--   既有管理员的授权快照要等 TTL 过期或由其下一次 `AssignRoles` 才收敛，因此上线顺序应为
--   「先跑迁移，再挂账号」。
--
-- 锁风险：只 INSERT 新行，不改表结构、不 UPDATE 既有行，无在线锁风险。
-- 回滚（先确认没有账号通过 op_admin_role 挂在这些角色上，否则收权会让在岗管理员立刻 403）：
--   DELETE FROM `op_role_permission` WHERE `role_id` IN
--     (SELECT `role_id` FROM `op_role` WHERE `name` = 'super_admin' OR `name` = 'readonly' OR `name` LIKE 'domain\_%');
--   DELETE FROM `op_role` WHERE `name` = 'super_admin' OR `name` = 'readonly' OR `name` LIKE 'domain\_%';
--   注：只删角色与绑定，不删权限点（000004 的 `op_permission` 行保留，后台入口仍可由手工角色重新授权）。

-- ==== 种子数据 ====
-- 域角色：名字与成员都从 op_permission.domain 现算，本文件不维护域名清单。
INSERT IGNORE INTO `op_role` (`name`, `title`, `state`, `operator`, `ctime`, `mtime`)
SELECT CONCAT('domain_', d.`domain`), CONCAT('域角色：', d.`domain`), 1, 0, 0, 0
FROM (SELECT DISTINCT `domain` FROM `op_permission` WHERE `domain` <> '') AS d;

-- 两个职能角色：跨域、无域名可派生，因此显式命名。
INSERT IGNORE INTO `op_role` (`name`, `title`, `state`, `operator`, `ctime`, `mtime`) VALUES
('readonly', '只读（全域 read 权限点）', 1, 0, 0, 0),
('super_admin', '超级管理员（break-glass，不绑账号）', 1, 0, 0, 0);

-- 绑定 1：域角色 ← 该域全部权限点。
INSERT IGNORE INTO `op_role_permission` (`role_id`, `permission_id`, `ctime`)
SELECT r.`role_id`, p.`permission_id`, 0
FROM `op_role` r
JOIN `op_permission` p ON p.`domain` = SUBSTRING(r.`name`, LENGTH('domain_') + 1)
WHERE r.`name` LIKE 'domain\_%' AND p.`domain` <> '';

-- 绑定 2：readonly ← 全部 action = 'read' 的权限点。
INSERT IGNORE INTO `op_role_permission` (`role_id`, `permission_id`, `ctime`)
SELECT r.`role_id`, p.`permission_id`, 0
FROM `op_role` r
JOIN `op_permission` p ON p.`action` = 'read'
WHERE r.`name` = 'readonly';

-- 绑定 3：super_admin ← 全部权限点。
INSERT IGNORE INTO `op_role_permission` (`role_id`, `permission_id`, `ctime`)
SELECT r.`role_id`, p.`permission_id`, 0
FROM `op_role` r
JOIN `op_permission` p
WHERE r.`name` = 'super_admin';
