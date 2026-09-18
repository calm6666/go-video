-- =====================================================================
-- creator 服务 - UP 主特殊属性表迁移
-- =====================================================================
-- 用途：记录 UP 主与特殊用户组的归属关系（mid → group_id 列表）。
-- 数据所有者：creator 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   只能通过 gRPC UpSpecial/UpsSpecial/UpGroupMids 接口访问。
-- 参考映射：移植自参考仓库 up 服务的 up_special 表。
-- 回滚：DROP TABLE IF EXISTS up_special;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `up_special`
(
    -- mid 用户 ID
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID',

    -- group_id 所属特殊分组 ID（外键 up_group.id，不强制外键约束以保持性能）
    `group_id` BIGINT UNSIGNED NOT NULL COMMENT '所属特殊分组 ID',

    PRIMARY KEY (`mid`, `group_id`),
    KEY `idx_group_id` (`group_id`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='creator 服务 UP 主特殊属性表：mid → group_id 归属关系。creator 持有，禁止跨服务直接读写。';
