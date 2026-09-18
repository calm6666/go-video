-- =====================================================================
-- account 服务 - 登录会话表迁移（token/refresh/cookie 会话）
-- =====================================================================
-- 用途：记录登录会话（access token、refresh token、cookie 会话），
--   支撑 TokenInfo/CookieInfo 校验、RenewToken 刷新与 Logout 吊销。
-- 数据所有者：account 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   鉴权一律经 account TokenInfo/CookieInfo RPC（网关统一鉴权）。
-- 参考映射：移植自参考仓库 passport-auth 服务的 token/cookie/refresh 存储
--   （参考实现按月分表 + HBase，本项目简化单表 + Redis 缓存，语义对齐：
--   token 过期 30 天、refresh 过期 90 天，缓存前缀沿用 ak_/ck_/rk_）。
-- 回滚：DROP TABLE IF EXISTS account_session;
-- 锁风险：仅建表；uk_token/uk_refresh/idx_mid 空库建索引无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `account_session`
(
    -- id 自增主键，无业务语义
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（无业务语义）',

    -- token access token（32 字节随机 hex，客户端请求携带；Redis 缓存 ak_<token>）
    `token` CHAR(64) NOT NULL DEFAULT '' COMMENT 'access token（32 字节随机 hex，Redis 缓存 ak_<token>）',

    -- refresh_token 刷新令牌（32 字节随机 hex，用于换取新 token；Redis 缓存 rk_<refresh>）
    `refresh_token` CHAR(64) NOT NULL DEFAULT '' COMMENT '刷新令牌（32 字节随机 hex，用于换取新 token）',

    -- mid 用户 ID（关联 account.mid，不强制外键）
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（关联 account.mid，不强制外键）',

    -- csrf CSRF token（随机 hex，随登录态返回，写操作校验用）
    `csrf` CHAR(32) NOT NULL DEFAULT '' COMMENT 'CSRF token（随机 hex，随登录态返回）',

    -- expires token 过期时间（Unix 秒，签发时间 + 30 天）
    `expires` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'token 过期时间（Unix 秒，签发时间 + 30 天）',

    -- refresh_expires 刷新令牌过期时间（Unix 秒，签发时间 + 90 天）
    `refresh_expires` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '刷新令牌过期时间（Unix 秒，签发时间 + 90 天）',

    -- status 会话状态：0 有效、1 已吊销（登出/改密后吊销）
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '会话状态：0 有效、1 已吊销（登出/改密后吊销）',

    -- create_ip 签发来源 IP（VARCHAR(45) 兼容 IPv6）
    `create_ip` VARCHAR(45) NOT NULL DEFAULT '' COMMENT '签发来源 IP（兼容 IPv6）',

    -- device 设备标识（客户端上报）
    `device` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '设备标识（客户端上报）',

    -- buvid 设备 BUVID（客户端上报）
    `buvid` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '设备 BUVID（客户端上报）',

    -- ctime 创建时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    -- mtime 最近更新时间（Unix 秒）
    `mtime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',

    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_token` (`token`),
    UNIQUE KEY `uk_refresh` (`refresh_token`),
    -- 登出/改密时按 mid 吊销全部会话
    KEY `idx_mid` (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='account 服务登录会话表：access token、refresh token 与 cookie 会话。account 持有，鉴权经 TokenInfo/CookieInfo RPC。';
