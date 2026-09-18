-- =====================================================================
-- user-profile 服务 - 用户经验值表迁移
-- =====================================================================
-- 用途：记录用户经验值，等级（0-6）由经验值实时推导（model.BuildLevel），
--   不落库。经验值对外单位为“分”，入库值 = 分 × 100（ExpMulti）。
-- 数据所有者：user-profile 服务。其他服务变更经验值必须调用 UpdateExp RPC
--   （account 的 AddExp3 委托本服务），禁止直连本表。
-- 参考映射：移植自参考仓库 member 服务 user_exp_%02d（mid%100 分表），
--   本项目采用单表（理由同 user_base）。
-- 回滚：DROP TABLE IF EXISTS user_exp;
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `user_exp`
(
    -- mid 用户 ID，主键
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，关联 user_base.mid）',

    -- exp 经验值（入库值 = 分 × 100；等级阈值 level1..level6 也按分计算后换算）
    `exp` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '经验值（入库值 = 分 × 100，BuildLevel 换算等级 0-6）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务用户经验值表：等级由经验值实时推导不落库。user-profile 持有，禁止跨服务直接读写。';
