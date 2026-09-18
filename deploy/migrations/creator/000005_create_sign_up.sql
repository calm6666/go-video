-- =====================================================================
-- creator 服务 - 高能联盟签约信息表迁移
-- =====================================================================
-- 用途：记录高能联盟 UP 主的签约状态与时间窗口。
-- 数据所有者：creator 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   只能通过 gRPC GetHighAllyUps 接口访问。
-- 参考映射：移植自参考仓库 up 服务的 sign_up 表。
-- 回滚：DROP TABLE IF EXISTS sign_up;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `sign_up`
(
    -- mid 签约 UP 主 ID，主键
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '签约 UP 主 ID',

    -- state 签约状态（业务定义，如 1 生效、2 到期、3 终止）
    `state` TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '签约状态',

    -- begin_date 签约开始时间（Unix 秒）
    `begin_date` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '签约开始时间（Unix 秒）',

    -- end_date 签约结束时间（Unix 秒）
    `end_date` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '签约结束时间（Unix 秒）',

    PRIMARY KEY (`mid`),
    KEY `idx_state_end` (`state`, `end_date`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='creator 服务高能联盟签约表：UP 主签约状态与时间窗口。creator 持有，禁止跨服务直接读写。';
