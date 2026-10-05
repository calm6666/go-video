-- =====================================================================
-- notification 服务 - 通知模板表
-- =====================================================================
-- 数据所有者：notification 服务（AGENTS.md §5）。
-- 影响：新建表，无数据迁移；模板按 (template_code, channel, lang, version) 唯一，
--       同一键最多一个已发布版本（由服务在单事务内保证）。
-- 锁风险：CREATE TABLE IF NOT EXISTS，仅元数据锁，可在线执行。
-- 回滚：DROP TABLE IF EXISTS `notification_template`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `notification_template` (
  `id`             BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `template_code`  VARCHAR(64)  NOT NULL COMMENT '模板业务码，例如 video_audit_approved',
  `channel`        TINYINT      NOT NULL COMMENT '通道：1 Push、2 短信、3 邮件（无小程序通道）',
  `lang`           VARCHAR(16)  NOT NULL COMMENT '语言：zh-CN/zh-TW/en',
  `title_tpl`      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '标题模板，占位符 {{var}}',
  `body_tpl`       TEXT         NOT NULL COMMENT '正文模板，占位符 {{var}}',
  `version`        INT          NOT NULL DEFAULT 1 COMMENT '版本号，同键递增',
  `state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 草稿、2 已发布、3 已下线',
  `operator`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后操作人（operation 域管理员账号）',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_code_channel_lang_version` (`template_code`, `channel`, `lang`, `version`),
  KEY `idx_code_channel_lang_state` (`template_code`, `channel`, `lang`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='通知模板表（按通道与语言维度版本化）';
