-- =====================================================================
-- operation 服务 - 管理操作审计索引与后台会话
-- =====================================================================
-- 数据库：go_video_operation（见 services/operation/etc/operation.v1.yaml 的 DataSource）。
-- 数据所有者：operation 服务。
--   * op_audit_index 是「审计索引」，只回答“谁在何时对哪个聚合做了什么、结果如何”，
--     并带 trace_id/request_id 便于回溯；请求正文、前后快照与审核证据仍由被操作的
--     领域服务保留，需要长期不可抵赖存证时由尚未实现的 services/audit 以 append-only 承接
--     （边界见 services/operation/README.md「边界与后续工作」）。
--   * op_admin_session 是后台管理会话（token）的事实源，与 account 的 C 端会话表
--     完全独立：token 前缀 adm_、缓存 key op:sess:*，两端 token 互不通用。
--
-- 表与代码对应关系（列名严格取自 services/operation/model/*.go 的 db tag 与 SQL 字符串）：
--   * op_audit_index   model/audit.go AuditIndex     —— 只追加，不更新：model 层没有 UPDATE/DELETE 方法
--   * op_admin_session model/admin_session.go AdminSession —— token 即主键（model.FindByToken）
--
-- 唯一键与幂等设计：
--   1. op_admin_session 以 token 为主键：签发即 INSERT（无 upsert 语义），
--      Revoke/RevokeAllByAdmin 按 token / (admin_id, state) 更新，PurgeExpired 按 expires 分批 DELETE。
--   2. op_audit_index 不建 request_id 唯一键：一次请求可能落多条索引
--      （如 SubmitAdminTask 与后续 RunAdminTask 会复用同一 request_id），
--      且读接口不写审计时 request_id 可为空串，唯一索引会直接冲突。
--      该表是 append-only 事实流水，重复写入由调用方「一次业务动作一次 writeAudit」保证，
--      需要跨服务去重的存证由 services/audit 以 event_id 唯一键承接（本期未实现）。
--
-- 敏感信息约束（AGENTS.md §7、docs/data-design.md §6）：
--   * ip_hash：SHA-256(ipHashSalt + IP) 前 32 个 hex 字符，不可逆、不落明文 IP；
--     同值可聚合（同一来源的登录异常可查），但无法反推原始 IP。
--   * user_agent 按 255 个字符截断（repository.truncate 按 rune 切，不产生半个字）。
--   * 两表都不写口令、口令散列、token 原文之外的凭证材料；action/resource_type/resource_id
--     只标「对哪个聚合做了什么」，不复制请求正文。
--   * op_admin_session.token 本身即能力凭证（等同口令），只存本表与 Redis 缓存，
--     严禁出现在日志与任何 RPC 出参（logic 层日志只打 operator/admin_id/错误）。
--
-- 索引取自真实查询路径：
--   * op_audit_index：List(admin_id / action / (resource_type, resource_id) /
--     ctime 区间过滤，LEFT JOIN op_admin_user 取用户名，ORDER BY id DESC 分页)。
--   * op_admin_session：FindByToken(主键)、RevokeAllByAdmin(admin_id + state)、
--     PurgeExpired(expires < ? LIMIT 1000)。
--
-- owner：运营平台（operation 服务）；影响范围：仅新增 2 张表。
-- 回滚：
--   DROP TABLE IF EXISTS `op_admin_session`;
--   DROP TABLE IF EXISTS `op_audit_index`;
--   注意：删除 op_admin_session 会让所有后台登录态立即失效（需重新登录）；
--   op_audit_index 是唯一的管理操作留痕，DROP 前必须导出，否则审计链断裂。
-- 锁风险：全部为新建空表。两表都是只增/定期清理型，容量治理按 ctime/expires 归档，
--   本期仅提供 PurgeExpired（会话），审计索引的保留期策略留给 services/audit（见 README 缺口）。
-- =====================================================================

-- 管理操作审计索引：result 取值 ok/denied/error（model.AuditResult*）。
-- 拒绝路径同样落一行（例如口令错、二次校验码错、权限不足），便于排查越权尝试。
CREATE TABLE IF NOT EXISTS `op_audit_index` (
  `id`            BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（列表按此倒序，等价于时间倒序）',
  `admin_id`      BIGINT       NOT NULL DEFAULT 0 COMMENT '操作管理员 ID（op_admin_user.admin_id；登录失败时可能为 0）',
  `action`        VARCHAR(64)  NOT NULL COMMENT '动作标识，命名 <对象>.<动作>：admin.login、admin_user.create、ops_config.save、admin_task.run 等',
  `resource_type` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '目标聚合类型：admin_user、admin_role、ops_config、admin_task、video:submission 等（跨服务只存类型与 ID 引用）',
  `resource_id`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '目标聚合 ID（字符串，兼容不同下游主键形态；配置类为 cfg_key/scope?v=n）',
  `result`        VARCHAR(16)  NOT NULL DEFAULT 'ok' COMMENT '结果：ok 成功、denied 被拒（权限/口令/状态）、error 出错',
  `ip_hash`       CHAR(32)     NOT NULL DEFAULT '' COMMENT '来源 IP 的不可逆短哈希（不落明文 IP，空 IP 存空串）',
  `user_agent`    VARCHAR(255) NOT NULL DEFAULT '' COMMENT '客户端 UA（按 255 字符截断）',
  `trace_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路 ID（与下游服务日志对齐，最长 64）',
  `request_id`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '请求/幂等 ID（同一请求可能多条索引，故不建唯一键）',
  `ctime`         BIGINT       NOT NULL DEFAULT 0 COMMENT '发生时间（Unix 秒）',
  PRIMARY KEY (`id`),
  KEY `idx_admin_ctime` (`admin_id`, `ctime`),
  KEY `idx_action_ctime` (`action`, `ctime`),
  KEY `idx_resource` (`resource_type`, `resource_id`),
  -- 按 ctime 区间扫描与归档任务；request_id 用于「一次请求做了什么」的回溯
  KEY `idx_ctime` (`ctime`),
  KEY `idx_request_id` (`request_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='管理操作审计索引表（append-only，不含口令/token/明文 IP；长期存证由 services/audit 承接）';

-- 后台会话表：token 为业务主键（adm_<issuer>_<random hex>_<hmac 摘要 hex>）。
-- state：1 有效、2 已吊销。禁用账号或重置口令时 RevokeAllByAdmin 会一次性吊销，
-- 同时作废权限快照（op:rbac:ver），实现「改权限即断旧登录态」的收敛语义。
CREATE TABLE IF NOT EXISTS `op_admin_session` (
  `token`      VARCHAR(128) NOT NULL COMMENT '后台会话 token（主键；能力凭证，等同口令，禁止写日志与出参）',
  `admin_id`   BIGINT       NOT NULL COMMENT '会话所属管理员 ID（op_admin_user.admin_id）',
  `expires`    BIGINT       NOT NULL DEFAULT 0 COMMENT '过期时间（Unix 秒，签发时间 + AdminSession.TokenTTL）',
  `state`      TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 有效、2 已吊销（吊销后缓存同步删除）',
  `ip_hash`    CHAR(32)     NOT NULL DEFAULT '' COMMENT '登录来源 IP 的不可逆短哈希（隐私最小化，不落明文）',
  `user_agent` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '登录 UA（按 255 字符截断）',
  `ctime`      BIGINT       NOT NULL DEFAULT 0 COMMENT '签发时间（Unix 秒）',
  PRIMARY KEY (`token`),
  -- RevokeAllByAdmin(admin_id + state=1) 与「某管理员当前有几个活动会话」的排障查询
  KEY `idx_admin_state` (`admin_id`, `state`),
  -- PurgeExpired(expires < ?) 分批清理
  KEY `idx_expires` (`expires`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='后台管理会话表（token 事实源；与 account 的用户会话表、缓存命名空间完全隔离）';
