-- =====================================================================
-- audit 服务 - 保留期策略与归档批次表（含 default 策略种子行）
-- =====================================================================
-- 数据库：go_video_audit。数据所有者：audit 服务。
--
-- 本文件回答一个问题：audit_entry 是只增不减的千万级大表，容量怎么治理，
-- 且治理过程本身怎么留下可被自证的证据。方案分两半：
--   * audit_retention_policy：按 action_domain 设定热表/归档/清除窗口（配置面，可改）。
--   * audit_archive_batch：每次归档一个批次凭证（证据面，只增）。
--
-- 为什么不用 MySQL 分区表做容量治理（本期决定，理由留档）：
--   分区键必须进入每个唯一键，而 audit_entry 有 uniq_event_id 与 uniq_chain_seq 两个唯一键，
--   把 occurred_at 塞进去会破坏 event_id 的全局幂等语义。
--   因此热表按策略「标记 archived_at + 归档对象常驻」来收敛扫描集，
--   物理删除在 delete_after_days 之后由独立的 DBA 作业执行（本期契约不提供 DELETE，
--   见 services/audit/README.md「已知缺口」）。
--
-- 表与代码对应关系（列名取自 services/audit/model/*.go 的 db tag 与 SQL 字符串）：
--   * audit_retention_policy model/auditretentionpolicy.go RetentionPolicy
--       Insert           —— 依赖 uniq_action_domain，冲突时 mtime 自等 → RowsAffected==0 → ErrPolicyExists
--       UpdateWithVersion —— WHERE action_domain=? AND version=?（乐观锁，改前必须带读到的版本）
--       FindEffective    —— 先按 action_domain 精确匹配，未命中回落 'default'
--   * audit_archive_batch    model/auditarchivebatch.go ArchiveBatch
--       FindCovering     —— WHERE chain_key=? AND from_seq<=? AND to_seq>=? AND state IN(...)
--                            校验链时判断「这段区间是否已被某个已验证批次覆盖」
--       TransitionState  —— WHERE batch_id=? AND state=?，只能沿 CanBatchTransition 走
--
-- 唯一键与幂等设计：
--   1. uniq_action_domain(action_domain)：一个域只能有一条策略。多条会让 FindEffective 不确定，
--      归档线就会漂移。
--   2. uniq_request_id(request_id)（批次表）：ArchiveAuditEntries 可被 cron 重投，
--      同一 request_id 必须回到同一批次，否则会重复扫描同一段链并生成两份清单。
--   3. 批次表不加 (chain_key, from_seq, to_seq) 唯一键：同一区间允许在失败后重试生成新批次，
--      旧批次停在 failed 作为失败证据保留——区间重叠由 FindCovering 在应用层判定。
--
-- 索引取自真实查询路径：
--   * audit_retention_policy：FindOne/FindEffective 走 uniq_action_domain；
--     List(state) + ORDER BY action_domain 由 uniq 索引前缀覆盖，不另建索引（行数 = 域数，量级为十）。
--   * audit_archive_batch：idx_chain_range(chain_key, from_seq, to_seq) 支撑 FindCovering；
--     idx_state_ctime(state, ctime) 支撑「还有多少批次卡在 writing/verified」的巡检与 List；
--     List 排序 ORDER BY batch_id DESC 走主键逆序。
--
-- 归档状态机（model.batchTransitions，DB 不做 CHECK，收敛点在服务层）：
--   pending → writing → verified → purged，任一阶段失败 → failed；purged/failed 为终态。
--   硬性约束在 repository.PurgeMark：state 必须是 verified 且 manifest_hash 非空，
--   且该判定无视任何配置开关——跳过清单校验就清热表等于销毁证据。
--
-- 敏感信息约束（AGENTS.md §7）：两表都不含 PII。manifest_hash / last_entry_hash 是
--   sha256hex 摘要；bucket / object_key 只存引用；operator 是 admin_id 引用。
--
-- owner：平台治理（audit 服务）；影响范围：新增 2 张表 + 1 行种子数据。
-- 回滚：
--   DELETE FROM `audit_retention_policy` WHERE `action_domain` = 'default';  -- 只回收本文件插入的种子行
--   DROP TABLE IF EXISTS `audit_archive_batch`;
--   DROP TABLE IF EXISTS `audit_retention_policy`;
--   注意：删除策略表不会删除数据，但会让归档作业失去归档线而停摆（安全失败）。
--   批次表是「热表行已完整搬走」的唯一凭证，DROP 前必须确认归档对象仍在对象存储里。
-- 锁风险：新建空表 + 常量级种子行，不触碰 audit_entry。种子写入用
--   ON DUPLICATE KEY UPDATE 自等，重复执行本文件不会改变已有策略（运维改过的值不被迁移覆盖）。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `audit_retention_policy` (
  `policy_id`          BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `action_domain`      VARCHAR(32) NOT NULL COMMENT '动作域（唯一键）；"default" 是未匹配域时的回退策略，必须存在',
  `hot_days`           INT         NOT NULL DEFAULT 365 COMMENT '热表保留天数：occurred_at 超过它即可被归档作业选中',
  `archive_after_days` INT         NOT NULL DEFAULT 90 COMMENT '归档触发天数：必须满足 0 < archive_after_days <= hot_days（model.CanPolicyDays）',
  `delete_after_days`  INT         NOT NULL DEFAULT 0 COMMENT '允许清理的天数：0 表示永久保留；非 0 时必须 >= archive_after_days',
  `state`              TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 启用、2 停用（停用后归档作业跳过该域）',
  `version`            BIGINT      NOT NULL DEFAULT 1 COMMENT '乐观锁版本：UpdateWithVersion 以 WHERE version = ? 推进',
  `operator`           BIGINT      NOT NULL DEFAULT 0 COMMENT '最后修改人 admin_id（引用 operation，不复制资料）',
  `remark`             VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更原因（同时由调用方写一条 audit 条目存证）',
  `ctime`              BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`              BIGINT      NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）',
  PRIMARY KEY (`policy_id`),
  UNIQUE KEY `uniq_action_domain` (`action_domain`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='审计保留期策略表（按动作域设定热表/归档/清理窗口；乐观锁更新，变更需另写审计条目）';

-- default 是唯一的强制回退策略：没有它，新出现的 action_domain 会查不到归档线而永远留在热表。
-- 数值口径：90 天起可归档、365 天为热表上限、清理永久关闭（delete_after_days = 0）。
-- 具体域（如审核洪峰域）如需更短的热表窗口，由治理方通过 SaveRetentionPolicy 增行，
-- 属数据变更而非 DDL，不再改本文件。
INSERT INTO `audit_retention_policy`
  (`action_domain`, `hot_days`, `archive_after_days`, `delete_after_days`, `state`, `version`, `operator`, `remark`, `ctime`, `mtime`)
VALUES
  ('default', 365, 90, 0, 1, 1, 0, '迁移种子：未匹配具体动作域时的回退策略', UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
ON DUPLICATE KEY UPDATE `action_domain` = `action_domain`;

CREATE TABLE IF NOT EXISTS `audit_archive_batch` (
  `batch_id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（列表按此倒序）',
  `request_id`       VARCHAR(64)  NOT NULL COMMENT '提交幂等键：cron 重投回到同一批次，避免重复扫描同一段链',
  `chain_key`        VARCHAR(64)  NOT NULL COMMENT '被归档的链："<action_domain>/<UTC 日>"；一次只归档一条链的一个区间',
  `from_seq`         BIGINT       NOT NULL DEFAULT 1 COMMENT '归档区间起始序号（链内）',
  `to_seq`           BIGINT       NOT NULL DEFAULT 0 COMMENT '归档区间结束序号；校验时据此判断区间是否封口',
  `row_count`        BIGINT       NOT NULL DEFAULT 0 COMMENT '本批次条目数：必须等于 to_seq - from_seq + 1，不等即为截断',
  `bucket`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '清单对象所在桶（只存引用，凭据在 Secret/Vault）',
  `object_key`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '归档清单对象键（每行 entry_id/seq/entry_hash）',
  `manifest_hash`    CHAR(64)     NOT NULL DEFAULT '' COMMENT '清单文件 sha256hex：PurgeMark 硬性要求非空，否则拒绝清热表',
  `last_entry_hash`  CHAR(64)     NOT NULL DEFAULT '' COMMENT '区间末条目的 entry_hash：与新链头 prev_hash 对齐，证明搬迁没有断链',
  `state`            VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT '状态：pending / writing / verified / purged / failed（合法性见 model.CanBatchTransition）',
  `operator_id`      BIGINT       NOT NULL DEFAULT 0 COMMENT '触发人 admin_id；系统自动归档时为 0 且由 caller 记系统条目',
  `trace_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路 ID',
  `err_msg`          VARCHAR(512) NOT NULL DEFAULT '' COMMENT '失败原因（脱敏文本，不含堆栈/SQL/连接串）',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '最后更新时间（Unix 秒）',
  `finished_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '进入 purged/failed 的时间',
  PRIMARY KEY (`batch_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_chain_range` (`chain_key`, `from_seq`, `to_seq`),
  KEY `idx_state_ctime` (`state`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='审计归档批次凭证表（清单哈希 + 区间尾摘要，构成热表数据离开前的可自证证据）';
