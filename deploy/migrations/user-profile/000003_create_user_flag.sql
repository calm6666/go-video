-- =====================================================================
-- user-profile 服务 - 用户标志位表迁移
-- =====================================================================
-- 用途：记录用户按位标志（当前仅 NickUpdated=1：是否已首次修改昵称）。
-- 数据所有者：user-profile 服务。
-- 参考映射：移植自参考仓库 member 服务 user_flag 表，字段与语义一致。
-- 回滚：DROP TABLE IF EXISTS user_flag;
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `user_flag`
(
    -- mid 用户 ID，主键
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，关联 user_base.mid）',

    -- flag 标志位集合，按位或：1 = 已首次修改昵称（NickUpdated）。
    -- 未来新增标志位需在 model/user_flag 常量中登记并在本注释同步枚举
    `flag` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '标志位集合（按位或）：1 = 已首次修改昵称（NickUpdated）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务用户标志位表：按位记录昵称修改等一次性状态。user-profile 持有，禁止跨服务直接读写。';
