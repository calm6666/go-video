-- =====================================================================
-- account 服务 - 账号主表迁移
-- =====================================================================
-- 用途：建立 account 服务自有的账号主表，记录账号生命周期元数据。
-- 数据所有者：account 服务（AGENTS.md §5）。其他服务禁止直接读写本表。
-- 参考映射：本表承接参考仓库 openbilibili-go-common account 服务依赖的
--   passport /intranet/acc/detail 接口中的账号域字段：
--     join_time → created_at
--     is_tourist → is_tourist
--     spacesta   → status（0 正常、1 封禁映射 Profile.silence=1）
--   参考 passport profile 中的 join_ip → reg_ip（/privacy 接口的 reg_ip）。
--   邮箱/手机等登录标识不冗余在本表，存放于 account_credential（000002）。
-- 回滚：DROP TABLE IF EXISTS account;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表，无锁风险；后续变更请使用新增迁移文件，禁止直接修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `account`
(
    -- mid 用户 ID，全局唯一，由 idgen（ULID 风格）生成；account 服务持有该主键
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，idgen 生成，全局唯一）',

    -- status 账号状态：0 正常、1 封禁、2 注销中、3 已注销
    -- 与 Profile.silence 字段联动：status=1 时 Profile.silence=1；
    -- v1/v2 接口据此把 spacesta 置为 -2（参考 V1Card/V2MyInfo.FromProfile）
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '账号状态：0 正常、1 封禁、2 注销中、3 已注销（status=1 时聚合层 Profile.silence=1、v1/v2 spacesta=-2）',

    -- is_tourist 是否游客账号：0 否、1 是
    -- 游客账号无凭证，不能登录，仅用于匿名浏览等场景；
    -- 对应参考 passport detail 的 is_tourist，聚合进 Profile.is_tourist
    `is_tourist` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '是否游客账号：0 否、1 是（对应 Profile.is_tourist）',

    -- created_at 注册时间，Unix 秒；与 Profile.join_time 一致
    `created_at` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '注册时间（Unix 秒，聚合为 Profile.join_time、/privacy 的 reg_ts、v1 的 regtime）',

    -- updated_at 账号状态最近一次变更时间，Unix 秒；UpdateStatus 时刷新
    `updated_at` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '账号状态最近变更时间（Unix 秒）',

    -- reg_ip 注册时的客户端 IP，可空；用于风控回溯和 /privacy 接口返回
    -- VARCHAR(45) 兼容 IPv6（最长 45 字符）；对应参考 passport profile 的 join_ip
    `reg_ip` VARCHAR(45) NOT NULL DEFAULT '' COMMENT '注册时的客户端 IP（兼容 IPv6，/privacy 接口返回）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='account 服务账号主表：用户 ID、账号状态、注册时间和注册 IP。account 服务持有，禁止跨服务直接读写。';
