-- =====================================================================
-- user-profile 服务 - 节操值表迁移
-- =====================================================================
-- 用途：记录用户节操值及累计增减、恢复时间。节操值以 1/100 为单位
--   （70.00 → 7000），初始 7000、上限 10000；低于 60/30 与恢复至 60
--   触发站内通知（Outbox 事件 user.moral.notice）。
-- 数据所有者：user-profile 服务。变更必须走 AddMoral/BatchAddMoral 事务接口
--   （account 的 AddMoral3 委托本服务），禁止直连本表。
-- 参考映射：移植自参考仓库 member 服务 user_moral 表，字段与语义一致。
-- 回滚：DROP TABLE IF EXISTS user_moral;
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `user_moral`
(
    -- mid 用户 ID，主键
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，关联 user_base.mid）',

    -- moral 当前节操值（1/100 单位），初始 7000，范围 [0, 10000]
    `moral` BIGINT UNSIGNED NOT NULL DEFAULT 7000 COMMENT '当前节操值（1/100 单位；初始 7000，上限 10000，下限 0）',

    -- added 累计增加值（1/100 单位），随加分事务累加，用于统计
    `added` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '累计增加值（1/100 单位，随加分事务累加）',

    -- deducted 累计扣减值（1/100 单位），随扣分事务累加
    `deducted` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '累计扣减值（1/100 单位，随扣分事务累加）',

    -- last_recover_date 上次节操从 >=7000 跌到 <7000 的时间（Unix 秒），
    -- 用于节操恢复类业务判断；初始 -28800 表示从未跌破
    `last_recover_date` BIGINT NOT NULL DEFAULT -28800 COMMENT '上次节操跌破基准值(7000)的时间（Unix 秒，-28800 表示从未跌破）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务节操值表：当前值、累计增减与恢复时间。变更走事务接口，禁止跨服务直接读写。';
