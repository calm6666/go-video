-- =====================================================================
-- creator 服务 - 特殊用户组表迁移
-- =====================================================================
-- 用途：定义创作者分类标签（如"高能联盟"、"知名 UP"等），前端展示为色块徽章。
-- 数据所有者：creator 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   只能通过 gRPC UpGroups 接口访问。
-- 参考映射：移植自参考仓库 up 服务的 up_group 表。
-- 回滚：DROP TABLE IF EXISTS up_group;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `up_group`
(
    -- id 分组 ID，主键
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '分组 ID',

    -- name 分组名（展示用）
    `name` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '分组名（展示用）',

    -- tag 标签名称
    `tag` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '标签名',

    -- short_tag 简称（小屏展示）
    `short_tag` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '简称',

    -- font_color 字体色（CSS 颜色值，如 #FFFFFF）
    `font_color` VARCHAR(16) NOT NULL DEFAULT '#FFFFFF' COMMENT '字体色（CSS 颜色值）',

    -- bg_color 背景色（CSS 颜色值，如 #FB7299）
    `bg_color` VARCHAR(16) NOT NULL DEFAULT '#FB7299' COMMENT '背景色（CSS 颜色值）',

    -- note 备注（运营内部使用）
    `note` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注',

    PRIMARY KEY (`id`),
    KEY `idx_name` (`name`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='creator 服务特殊用户组定义表：分类标签、徽章样式。creator 持有，禁止跨服务直接读写。';
