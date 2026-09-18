-- =====================================================================
-- user-profile 服务 - 用户变更日志表迁移
-- =====================================================================
-- 用途：记录经验值（log_type=11）与节操值（log_type=12）变更日志，
--   支撑 ExpLog/MoralLog 查询与 UndoMoral 撤销。参考仓库把日志写入
--   报表搜索服务（HBase/ES），本项目落地为本地 MySQL 表，查询语义对齐
--   （最近 7 天、按时间倒序、最多 1000 条）。
-- 数据所有者：user-profile 服务。
-- 回滚：DROP TABLE IF EXISTS member_log;
-- 锁风险：仅建表；uk_log_type_log_id/idx_type_mid_ts 空库建索引无锁风险。
-- 清理策略：日志默认保留 7 天以上（查询只取 7 天），建议 cron 定期
--   归档/清理超过 30 天的历史行（services/cron）。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `member_log`
(
    -- id 自增主键
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',

    -- log_type 日志类型：11 经验变更、12 节操变更（对应参考仓库业务号）
    `log_type` TINYINT UNSIGNED NOT NULL COMMENT '日志类型：11 经验变更、12 节操变更',

    -- mid 用户 ID
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（关联 user_base.mid）',

    -- log_id 日志唯一 ID（UUID v4）
    `log_id` CHAR(36) NOT NULL DEFAULT '' COMMENT '日志唯一 ID（UUID v4）',

    -- ts 操作时间（Unix 秒）
    `ts` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '操作时间（Unix 秒）',

    -- ip 操作来源 IP（VARCHAR(45) 兼容 IPv6）
    `ip` VARCHAR(45) NOT NULL DEFAULT '' COMMENT '操作来源 IP（兼容 IPv6）',

    -- content 日志内容 JSON（map[string]string：from_/to_、origin、status、remark、operater、reason 等）
    `content` TEXT NOT NULL COMMENT '日志内容 JSON（map[string]string：变更前后值、来源、状态、备注、操作人、原因等）',

    -- status 日志状态：0 有效、1 已撤销（UndoMoral 置位后不再参与查询）
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '日志状态：0 有效、1 已撤销（撤销后不再参与查询）',

    -- ctime 写入时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '写入时间（Unix 秒）',

    PRIMARY KEY (`id`),
    -- UndoMoral 按 log_id 查单条日志
    UNIQUE KEY `uk_log_type_log_id` (`log_type`, `log_id`),
    -- 按用户 + 时间倒序查询（最近 7 天日志）
    KEY `idx_type_mid_ts` (`log_type`, `mid`, `ts`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务用户变更日志表：经验/节操变更日志，支撑日志查询与节操撤销。';
