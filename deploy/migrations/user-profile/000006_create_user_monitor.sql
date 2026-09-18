-- =====================================================================
-- user-profile 服务 - 用户监控名单表迁移
-- =====================================================================
-- 用途：记录受监控用户名单；受监控用户的头像/签名/昵称变更自动进入
--   属性审核流程（AddPropertyReview 读取 is_deleted=0 的记录）。
-- 数据所有者：user-profile 服务。
-- 参考映射：移植自参考仓库 member 服务 user_monitor 表，字段与语义一致。
-- 回滚：DROP TABLE IF EXISTS user_monitor;
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `user_monitor`
(
    -- mid 用户 ID，主键（同一用户一条监控记录）
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，同一用户一条监控记录）',

    -- operator 添加监控的操作人
    `operator` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '添加监控的操作人',

    -- remark 备注（监控原因等）
    `remark` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注（监控原因等）',

    -- is_deleted 软删除：0 在监控中、1 已移出；重新添加时置回 0 并更新操作人与备注
    `is_deleted` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '软删除：0 在监控中、1 已移出（重新添加置回 0）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务用户监控名单表：受监控用户的资料变更自动进入属性审核流程。';
