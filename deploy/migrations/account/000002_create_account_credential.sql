-- =====================================================================
-- account 服务 - 凭证表迁移
-- =====================================================================
-- 用途：建立 account_credential 表，记录用户登录标识（用户名/手机/邮箱）。
-- 数据所有者：account 服务（AGENTS.md §5）。其他服务禁止直接读写本表。
-- 一个 mid 可以有多条凭证记录（用户名、手机、邮箱各一条）。
-- 参考映射：本表承接参考仓库 passport detail 接口中的登录标识字段：
--     email  → credential_type=3 的记录
--     phone  → credential_type=2 的记录
--   昵称（uname）不在 account 域，由 user-profile 服务持有，/info/by/name
--   通过 credential_type=1（用户名）查询 mid 后回源聚合。
-- 消费方：
--   /privacy 接口的 Tel 字段（credential_type=2 且 status=0）；
--   RawProfile 的 tel_status/email_status（credential_type=2/3 且 status=0）；
--   /info/by/name 的 name→mid 查询（credential_type=1，LOWER 比较）。
-- 回滚：DROP TABLE IF EXISTS account_credential;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表；uk_type_identifier 唯一索引在大量存量数据导入时可能产生
--   建索引锁，空库迁移无风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `account_credential`
(
    -- id 自增主键，仅作为行标识，无业务语义
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（无业务语义）',

    -- mid 用户 ID，外键逻辑关联 account.mid（不强约束外键，避免跨表写入阻塞）
    -- 同一 mid 可以有多条凭证记录（credential_type 区分）
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（关联 account.mid，不强制外键）',

    -- credential_type 凭证类型：1 用户名、2 手机号、3 邮箱
    -- 与 model.CredentialTypeUsername/Phone/Email 常量对应
    -- Profile.tel_status/email_status 通过查询 credential_type=2/3 且 status=0 的记录得出
    `credential_type` TINYINT UNSIGNED NOT NULL COMMENT '凭证类型：1 用户名、2 手机号、3 邮箱',

    -- identifier 凭证值：用户名/手机号/邮箱
    -- 查询时按 LOWER(identifier) 比较，避免大小写歧义
    -- VARCHAR(255) 覆盖邮箱和用户名最大长度
    `identifier` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '凭证值：用户名/手机号/邮箱（查询时按 LOWER 比较）',

    -- status 凭证状态：0 正常、1 已解绑
    -- 解绑后保留行用于风控回溯，查询时需过滤 status=0
    `status` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '凭证状态：0 正常、1 已解绑（查询时过滤 status=0）',

    -- created_at 凭证创建时间，Unix 秒
    `created_at` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '凭证创建时间（Unix 秒）',

    -- updated_at 凭证最近变更时间，Unix 秒
    `updated_at` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '凭证最近变更时间（Unix 秒）',

    PRIMARY KEY (`id`),
    -- 唯一约束：同一类型下凭证值唯一（含 status=1 的解绑记录，避免重新绑定冲突）
    -- 业务侧在解绑后保留行，重新绑定时需要先更新原记录 status=1 再插入新记录
    UNIQUE KEY `uk_type_identifier` (`credential_type`, `identifier`),
    -- 二级索引：按 mid 反查凭证列表，/privacy 和 RawProfile 都按 mid 查
    KEY `idx_mid` (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='account 服务凭证表：用户名、手机号、邮箱等登录标识。一个 mid 可有多条凭证，按类型区分。';
