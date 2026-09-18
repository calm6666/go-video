-- =====================================================================
-- account 服务 - 验证码发送记录表迁移
-- =====================================================================
-- 用途：记录登录/注册/账号找回验证码的发送行为，供审计与风控回溯。
--   验证码本身存 Redis（cap_code_<biz>_<target> 前缀，10 分钟过期），
--   本表仅保留发送记录（不含验证码明文）。
-- 数据所有者：account 服务（AGENTS.md §5）。
-- 参考映射：移植自参考仓库 sms 服务的账号侧验证码能力（短信下发由
--   notification 服务承接，待接入；当前验证码发送为开发降级日志模式）。
-- 回滚：DROP TABLE IF EXISTS account_capture_log;
-- 锁风险：仅建表；idx_target_ctime 空库建索引无锁风险。
-- 清理策略：建议 cron 定期清理超过 30 天的历史行（services/cron）。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `account_capture_log`
(
    -- id 自增主键，无业务语义
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（无业务语义）',

    -- biz 业务类型：1 登录、2 注册、3 账号找回
    `biz` TINYINT UNSIGNED NOT NULL COMMENT '业务类型：1 登录、2 注册、3 账号找回',

    -- target 接收方（手机号）
    `target` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '接收方（手机号）',

    -- ip 发送请求来源 IP（VARCHAR(45) 兼容 IPv6）
    `ip` VARCHAR(45) NOT NULL DEFAULT '' COMMENT '发送请求来源 IP（兼容 IPv6）',

    -- status 发送结果：0 成功（已入 Redis）、1 失败（超频/服务不可用）
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '发送结果：0 成功（已入 Redis）、1 失败（超频/服务不可用）',

    -- reason 失败原因（成功为空）
    `reason` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '失败原因（成功为空）',

    -- ctime 创建时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    PRIMARY KEY (`id`),
    -- 风控回溯：按接收方 + 时间倒序
    KEY `idx_target_ctime` (`target`, `ctime`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='account 服务验证码发送记录表：登录/注册/找回验证码的发送审计（不含验证码明文）。';
