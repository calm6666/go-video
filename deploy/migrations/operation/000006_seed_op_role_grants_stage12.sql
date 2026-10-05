-- 000006_seed_op_role_grants_stage12.sql
-- 目的：把阶段 1-2 十三个域新增的 39 个权限点并入 000005 建立的默认角色体系。
--
-- 为什么需要再派生一轮：000005 的角色与绑定全是 `INSERT ... SELECT`，跑一次就冻结在那一刻的
-- `op_permission` 快照上。本轮给 gateway/admin 的阶段 1-2 运营面补挂 `AdminPermission`
-- （见 gateway/admin/README.md「写入口鉴权」一节），000004 因此从 123 个权限点增到 162 个，
-- 新增的 13 个域（account/member/video/catalog/rights/moderation/transcode/danmaku/search/
-- risk/comment/notify/inbox）在库里既没有对应 `domain_*` 角色，也没有任何绑定，
-- 结果就是这些刚补上鉴权的入口**对所有角色 403**——补鉴权等于把后台写入口锁死。
-- scripts/migrate.ps1 按版本号记录并跳过已执行迁移，不会回头重放 000005，
-- 所以并入动作必须由一条新迁移承担。
--
-- 本文件与 000005 的关系：**逐字重复 000005 末尾的三段派生 SELECT**（外加域角色建名），
-- 不新增任何硬编码 id、不新增角色种类、不写 `op_admin_role`/`op_admin_user`。
-- 因此它天然幂等（`op_role` 靠 uniq_name、`op_role_permission` 靠主键去重），
-- 且对未来再加的权限点同样成立——照抄本文件即可。
-- `services/operation/model/role_seed_test.go` 双向断言：000004 的每个 domain 都必须被
-- 000005/000006 里的一段派生 SELECT 覆盖，且每个权限点都有角色持有。
--
-- 权限风险（与 000005 同源，仍需读）：`domain_operation` 含 `operation:role#create/delete/assign`
--   与 `operation:admin_user#create/update/disable`、`operation:permission#create`，
--   拿到即能给自己加权限；`super_admin` 是 break-glass。两者都不得当日常岗用。
--   本轮新域里最需要单独看管的是 `domain_inbox`（`inbox:message#send` = 向一批用户群发站内信）
--   与 `domain_risk`（`risk:punishment#create` = 处置真人），它们各自成域，
--   不会再有「因为和只读台账同域所以顺带拿到」的情况。
--   个人数据读面（`member:realname#read`、`member:realname#reverse-lookup`、
--   `member:login-log#read`）刻意不并入 `readonly`：其中两条 action 不是 `read`，
--   而 `readonly` 的定义就是「全域 action=read 的最小安全面」，不因新域扩大而变。
--
-- owner：运营平台（operation 服务，op_* 前缀，deploy/migrations/operation）；
--   影响范围：只 INSERT `op_role` 与 `op_role_permission`，不建表、不改表结构、不动账号与菜单。
--
-- 幂等口径：全部 INSERT IGNORE；已存在的角色不覆盖 title（运营改过名不会被重跑打回）。
--   `ctime/mtime/operator` 固定 0：表示「由迁移建立」。
--
-- 缓存收敛：同 000005——SQL 迁移不触发 `op:rbac:ver`（internal/repository/rbac.go 的
--   invalidateRBAC 只由 RPC 写路径调用）。新库无缓存不受影响；在已跑过 000005 的实例上执行本迁移后，
--   在岗管理员的授权快照要等 TTL 过期或下一次 `AssignRoles` 才收敛，
--   上线顺序应为「先跑本迁移，再把新域角色挂给账号」。
--
-- 锁风险：只 INSERT 新行，不改表结构、不 UPDATE 既有行，无在线锁风险。
-- 回滚（只回滚本轮并入的部分，保留 000005 建立的 16 个域角色与其绑定）：
--   DELETE FROM `op_role_permission` WHERE `permission_id` IN
--     (SELECT `permission_id` FROM `op_permission` WHERE `domain` IN
--       ('account','member','video','catalog','rights','moderation','transcode','danmaku',
--        'search','risk','comment','notify','inbox'));
--   DELETE FROM `op_role` WHERE `name` IN
--     ('domain_account','domain_member','domain_video','domain_catalog','domain_rights',
--      'domain_moderation','domain_transcode','domain_danmaku','domain_search','domain_risk',
--      'domain_comment','domain_notify','domain_inbox');
--   注：删完这些角色绑定后，对应后台入口回到「对所有角色 403」，重跑本文件即可恢复；
--   若同时想撤掉入口本身，应先在 gateway/admin 摘路由再删 000004 的权限点行。

-- ==== 种子数据 ====
-- 域角色：名字从 op_permission.domain 现算（与 000005 同一条语句），本轮因此自动多出 13 个角色。
INSERT IGNORE INTO `op_role` (`name`, `title`, `state`, `operator`, `ctime`, `mtime`)
SELECT CONCAT('domain_', d.`domain`), CONCAT('域角色：', d.`domain`), 1, 0, 0, 0
FROM (SELECT DISTINCT `domain` FROM `op_permission` WHERE `domain` <> '') AS d;

-- 绑定 1：域角色 ← 该域全部权限点（含本轮新域；既有域因 INSERT IGNORE 不受影响）。
INSERT IGNORE INTO `op_role_permission` (`role_id`, `permission_id`, `ctime`)
SELECT r.`role_id`, p.`permission_id`, 0
FROM `op_role` r
JOIN `op_permission` p ON p.`domain` = SUBSTRING(r.`name`, LENGTH('domain_') + 1)
WHERE r.`name` LIKE 'domain\_%' AND p.`domain` <> '';

-- 绑定 2：readonly ← 全部 action = 'read' 的权限点（本轮并入 member 的两条个人数据定向读）。
INSERT IGNORE INTO `op_role_permission` (`role_id`, `permission_id`, `ctime`)
SELECT r.`role_id`, p.`permission_id`, 0
FROM `op_role` r
JOIN `op_permission` p ON p.`action` = 'read'
WHERE r.`name` = 'readonly';

-- 绑定 3：super_admin ← 全部权限点（break-glass，本文件不把它绑给任何账号）。
INSERT IGNORE INTO `op_role_permission` (`role_id`, `permission_id`, `ctime`)
SELECT r.`role_id`, p.`permission_id`, 0
FROM `op_role` r
JOIN `op_permission` p
WHERE r.`name` = 'super_admin';
