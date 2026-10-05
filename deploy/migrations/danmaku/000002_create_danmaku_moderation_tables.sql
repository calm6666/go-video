-- =====================================================================
-- danmaku 服务 - 屏蔽词 / 用户屏蔽 / 举报 / 操作留痕
-- =====================================================================
-- 数据所有者：danmaku 服务（AGENTS.md §5）。
-- 本目录四张表都是弹幕域的治理辅助表，其他服务禁止直连：
--   * danmaku_blockword     发送侧词库，由运营通过 BlockWord RPC 维护；
--     与 moderation-orchestrator 的审核规则库不是一回事，后者归审核域。
--   * danmaku_user_block    用户个人屏蔽，只在读取侧生效（ListDanmaku 过滤），
--     不改主表状态，因此解除屏蔽无需回填历史。
--   * danmaku_report        只记录举报事实，审核域拉取后回写结论。
--   * danmaku_op_log        状态机迁移留痕（AGENTS.md §8 要求删除/下架可审计）。
--
-- 幂等与唯一性设计：
--   1. danmaku_blockword.uniq_word：同一词条全局唯一，运营重复 ADD 走
--      INSERT ... ON DUPLICATE KEY UPDATE（重新启用并更新 scope），不产生多行。
--   2. danmaku_user_block.uniq_mid_target：(mid, blocked_mid, keyword) 唯一，
--      屏蔽/解除均为 upsert，客户端重放无副作用。非本类型使用的列固定填
--      0 / ''，保证唯一键完整。
--   3. danmaku_report.uniq_dmid_reporter：同一人对同一条弹幕只留一条举报。
--   4. danmaku_op_log.uniq_event：event_id 可空，非事件驱动的写操作存 NULL，
--      MySQL 唯一索引允许多个 NULL，因此既能对 moderation.result.v1 消费去重，
--      又不会让普通操作互相冲突。这是 ApplyModerationResult 幂等的最终防线。
--
-- 容量：这四张表量级远小于主表（词库千级、举报万级、留痕与主表写次数同阶），
-- 不做分段；op_log 增长最快，由 services/cron 按 ctime 归档（例如 180 天前）。
--
-- 回滚：
--   DROP TABLE IF EXISTS `danmaku_op_log`;
--   DROP TABLE IF EXISTS `danmaku_report`;
--   DROP TABLE IF EXISTS `danmaku_user_block`;
--   DROP TABLE IF EXISTS `danmaku_blockword`;
-- 锁风险：仅建表（CREATE TABLE IF NOT EXISTS），可重复执行，无锁风险；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `danmaku_blockword` (
  `word_id`  BIGINT      NOT NULL AUTO_INCREMENT COMMENT '词条 ID（主键）',
  `word`     VARCHAR(64) NOT NULL COMMENT '屏蔽词原文（入库原文，匹配时两侧做归一化）',
  `scope`    TINYINT     NOT NULL DEFAULT 1 COMMENT '作用域：1 全局、2 单内容/分区',
  `oid`      BIGINT      NOT NULL DEFAULT 0 COMMENT 'scope=2 时的内容主键，scope=1 恒为 0',
  `state`    TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 生效、0 停用（保留行便于审计）',
  `operator` BIGINT      NOT NULL DEFAULT 0 COMMENT '最近一次操作的运营/管理员用户 ID',
  `ctime`    BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`    BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`word_id`),
  UNIQUE KEY `uniq_word` (`word`),
  KEY `idx_scope_state` (`scope`, `state`, `oid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='弹幕屏蔽词表（发送侧词库，加载后建首字桶做匹配）';

-- 回滚：DROP TABLE IF EXISTS `danmaku_user_block`;
CREATE TABLE IF NOT EXISTS `danmaku_user_block` (
  `id`          BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `mid`         BIGINT      NOT NULL COMMENT '屏蔽项所属用户',
  `type`        TINYINT     NOT NULL DEFAULT 1 COMMENT '类型：1 屏蔽用户、2 屏蔽关键词',
  `blocked_mid` BIGINT      NOT NULL DEFAULT 0 COMMENT '被屏蔽用户 ID（type=1 时非 0，type=2 时恒为 0）',
  `keyword`     VARCHAR(64) NOT NULL DEFAULT '' COMMENT '被屏蔽关键词（type=2 时非空，type=1 时恒为空串）',
  `state`       TINYINT     NOT NULL DEFAULT 1 COMMENT '状态：1 生效、0 已解除（保留行做幂等锚点）',
  `ctime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_target` (`mid`, `blocked_mid`, `keyword`),
  KEY `idx_mid_state` (`mid`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户弹幕屏蔽表（只在读取侧生效，不影响主表状态）';

-- 回滚：DROP TABLE IF EXISTS `danmaku_report`;
CREATE TABLE IF NOT EXISTS `danmaku_report` (
  `report_id`    BIGINT       NOT NULL AUTO_INCREMENT COMMENT '举报记录 ID（主键）',
  `dmid`         BIGINT       NOT NULL COMMENT '被举报弹幕 ID',
  `reporter_mid` BIGINT       NOT NULL COMMENT '举报者用户 ID',
  `reason`       INT          NOT NULL DEFAULT 0 COMMENT '举报原因码（由客户端与审核域约定，本服务不解释）',
  `content`      VARCHAR(500) NOT NULL DEFAULT '' COMMENT '举报补充说明',
  `state`        TINYINT      NOT NULL DEFAULT 0 COMMENT '处理状态：0 待处理、1 已处理、2 已驳回',
  `trace_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`report_id`),
  UNIQUE KEY `uniq_dmid_reporter` (`dmid`, `reporter_mid`),
  KEY `idx_state_report` (`state`, `report_id`),
  KEY `idx_reporter_ctime` (`reporter_mid`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='弹幕举报表（审核域按 state=0 拉取，本服务不直连 moderation 库）';

-- 回滚：DROP TABLE IF EXISTS `danmaku_op_log`;
CREATE TABLE IF NOT EXISTS `danmaku_op_log` (
  `log_id`        BIGINT      NOT NULL AUTO_INCREMENT COMMENT '留痕 ID（主键）',
  `dmid`          BIGINT      NOT NULL COMMENT '弹幕 ID',
  `action`        VARCHAR(32) NOT NULL COMMENT '动作：post 发送、delete 删除、moderation 审核回写、report 举报',
  `from_state`    TINYINT     NOT NULL DEFAULT 0 COMMENT '迁移前状态：0 正常、1 待审核、2 折叠、3 删除、4 驳回',
  `to_state`      TINYINT     NOT NULL DEFAULT 0 COMMENT '迁移后状态',
  `operator_mid`  BIGINT      NOT NULL DEFAULT 0 COMMENT '操作者用户 ID（系统操作为 0）',
  `operator_role` TINYINT     NOT NULL DEFAULT 3 COMMENT '操作者角色：1 本人、2 运营、3 系统',
  `reason`        VARCHAR(255) NOT NULL DEFAULT '' COMMENT '原因说明（审核理由/删除原因）',
  `event_id`      VARCHAR(64) DEFAULT NULL COMMENT '驱动本次迁移的事件 ID；非事件驱动为 NULL',
  `trace_id`      VARCHAR(64) NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `ctime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`log_id`),
  UNIQUE KEY `uniq_event` (`event_id`),
  KEY `idx_dmid_log` (`dmid`, `log_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='弹幕操作留痕表（状态机审计证据 + moderation 结果消费去重）';
