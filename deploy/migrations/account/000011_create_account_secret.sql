-- =====================================================================
-- account 服务 - 账号密钥表迁移（登录密码与历史密码）
-- =====================================================================
-- 用途：记录账号密码的盐值哈希与历史密码，支撑密码登录、改密与历史密码
--   校验（/history/pwd/check）。哈希算法与参考仓库 passport 服务一致：
--   MD5(pwd + ">>BiLiSaLt<<" + salt)（生产建议后续平滑升级 bcrypt）。
-- 数据所有者：account 服务（AGENTS.md §5）。其他服务禁止直接读写本表。
-- 参考映射：移植自参考仓库 passport 服务的密码存储（历史密码校验
--   HistoryPwdCheck/getSaltPwd）。
-- 回滚：DROP TABLE IF EXISTS account_secret;
-- 锁风险：仅建表；uk_mid_type_status 空库建索引无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `account_secret`
(
    -- id 自增主键，无业务语义
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（无业务语义）',

    -- mid 用户 ID（关联 account.mid，不强制外键）
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（关联 account.mid，不强制外键）',

    -- secret_type 密钥类型：1 登录密码（当前仅此类型）
    `secret_type` TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '密钥类型：1 登录密码',

    -- salt 随机盐（hex 字符串，每次改密重新生成）
    `salt` CHAR(32) NOT NULL DEFAULT '' COMMENT '随机盐（hex 字符串，每次改密重新生成）',

    -- hash 哈希值：MD5(pwd + ">>BiLiSaLt<<" + salt) 的 hex
    `hash` CHAR(32) NOT NULL DEFAULT '' COMMENT '密码哈希（MD5(pwd+">>BiLiSaLt<<"+salt) 的 hex）',

    -- status 状态：0 当前生效、1 历史（改密后旧行置 1，用于历史密码校验）
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '状态：0 当前生效、1 历史（改密后旧行置 1）',

    -- ctime 创建时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    -- mtime 最近更新时间（Unix 秒）
    `mtime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',

    PRIMARY KEY (`id`),
    -- 保证同一用户同一类型最多一条当前生效密钥（status=0 唯一）
    UNIQUE KEY `uk_mid_type_status` (`mid`, `secret_type`, `status`),
    -- 历史密码校验按 mid 全量查询
    KEY `idx_mid` (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='account 服务账号密钥表：登录密码盐值哈希与历史密码。account 持有，禁止跨服务直接读写。';
