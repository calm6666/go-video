-- =====================================================================
-- creator 服务 - UP 主身份属性表迁移
-- =====================================================================
-- 用途：记录 UP 主在不同来源下的身份判定（是否有创作者身份）。
-- 数据所有者：creator 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   只能通过 gRPC UpAttr 接口访问。
-- 参考映射：移植自参考仓库 up 服务的 up_base 表（仅保留身份子集）。
-- 回滚：DROP TABLE IF EXISTS up_attr;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `up_attr`
(
    -- mid 用户 ID
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID',

    -- from 来源：0 稿件作者、1 移动投稿作者、2 直播 UP、3 直播白名单
    `from` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '来源：0 稿件作者、1 移动投稿作者、2 直播 UP、3 直播白名单',

    -- is_author 是否有身份：0 否、1 是
    `is_author` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '是否有身份：0 否、1 是',

    PRIMARY KEY (`mid`, `from`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='creator 服务 UP 主身份属性表：按来源区分的创作者身份判定。creator 持有，禁止跨服务直接读写。';
