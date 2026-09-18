-- =====================================================================
-- account 服务 - 登录日志表迁移
-- =====================================================================
-- 用途：记录登录/注册行为（成功与失败），支撑登录日志查询与风控回溯。
--   参考仓库写入 HBase（hbase_login_log），本项目落地本地 MySQL。
-- 数据所有者：account 服务（AGENTS.md §5）。
-- 参考映射：移植自参考仓库 passport 服务的登录日志能力
--   （RPC.LoginLogs 与 /x/internal/passport/records/loginlog）。
-- 回滚：DROP TABLE IF EXISTS account_login_log;
-- 锁风险：仅建表；idx_mid_ctime 空库建索引无锁风险。
-- 清理策略：建议 cron 定期归档/清理超过 180 天的历史行（services/cron）。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `account_login_log`
(
    -- id 自增主键，无业务语义
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（无业务语义）',

    -- mid 用户 ID（关联 account.mid；注册失败等场景 mid 为 0）
    `mid` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '用户 ID（关联 account.mid；注册失败等场景为 0）',

    -- login_type 登录方式：1 密码、2 验证码、3 注册
    `login_type` TINYINT UNSIGNED NOT NULL DEFAULT 1 COMMENT '登录方式：1 密码、2 验证码、3 注册',

    -- status 结果：0 成功、1 失败
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '结果：0 成功、1 失败',

    -- reason 失败原因（成功为空）
    `reason` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '失败原因（成功为空）',

    -- ip 登录来源 IP（VARCHAR(45) 兼容 IPv6）
    `ip` VARCHAR(45) NOT NULL DEFAULT '' COMMENT '登录来源 IP（兼容 IPv6）',

    -- device 设备标识（客户端上报）
    `device` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '设备标识（客户端上报）',

    -- buvid 设备 BUVID（客户端上报）
    `buvid` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '设备 BUVID（客户端上报）',

    -- ts 登录时间（Unix 秒）
    `ts` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '登录时间（Unix 秒）',

    -- ctime 写入时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '写入时间（Unix 秒）',

    PRIMARY KEY (`id`),
    -- 登录日志查询：按 mid + 时间倒序
    KEY `idx_mid_ctime` (`mid`, `ctime`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='account 服务登录日志表：登录/注册成功与失败记录。account 持有，用于日志查询与风控回溯。';
