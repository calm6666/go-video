-- =====================================================================
-- user-profile 服务 - 官方认证三表迁移（生效信息/申请文档/附加键值）
-- =====================================================================
-- 用途：
--   user_official            已生效官方认证信息（服务启动时全量载入内存快照）
--   user_official_doc        官方认证申请文档（提交/审核状态，附加资料 JSON）
--   user_official_doc_addit  认证附加键值（当前用于统一社会信用代码）
-- 数据所有者：user-profile 服务。认证审核工作流后续由 operation/creator 服务
--   通过 RPC 消费 user_official_doc；生效信息由 user-profile 维护。
-- 参考映射：移植自参考仓库 member 服务 user_official / user_official_doc /
--   user_official_doc_addit 三张表，字段与语义一致。
-- 回滚：DROP TABLE IF EXISTS user_official_doc_addit;
--        DROP TABLE IF EXISTS user_official_doc;
--        DROP TABLE IF EXISTS user_official;
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

-- 已生效官方认证信息（role>0 表示已认证）
CREATE TABLE IF NOT EXISTS `user_official`
(
    -- mid 用户 ID，主键
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，关联 user_base.mid）',

    -- role 认证角色：0 未认证、1 UP 主、2 身份、3 企业、4 政府、5 媒体、6 其他
    `role` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '认证角色：0 未认证、1 UP 主、2 身份、3 企业、4 政府、5 媒体、6 其他',

    -- title 认证称号（如"知名UP主"）
    `title` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '认证称号',

    -- description 认证描述（后缀说明）
    `description` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '认证描述（后缀说明）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务已生效官方认证信息表：role/title/description，role>0 表示已认证。';

-- 官方认证申请文档（提交即置待审核，审核结论由审核方更新）
CREATE TABLE IF NOT EXISTS `user_official_doc`
(
    -- mid 用户 ID，主键（一个用户保留最新一份申请文档）
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（主键，保留最新一份申请文档）',

    -- name 认证主体名称（必填）
    `name` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '认证主体名称（必填）',

    -- state 审核状态：0 待审核、1 通过、2 不通过、3 重新提交
    `state` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '审核状态：0 待审核、1 通过、2 不通过、3 重新提交',

    -- role 申请认证角色（枚举同 user_official.role）
    `role` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '申请认证角色：1 UP 主、2 身份、3 企业、4 政府、5 媒体、6 其他',

    -- title 认证称号
    `title` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '认证称号',

    -- description 认证描述
    `description` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '认证描述',

    -- reject_reason 拒绝原因（审核驳回时回填）
    `reject_reason` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '拒绝原因（审核驳回时回填）',

    -- extra 附加资料 JSON（OfficialExtra：实名/联系人/电话/邮箱/地址/公司/信用代码/执照等）
    `extra` TEXT NOT NULL COMMENT '附加资料 JSON（OfficialExtra：联系人、电话、邮箱、公司、信用代码、执照等）',

    -- submit_source 提交来源（如 app/web/运营后台）
    `submit_source` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '提交来源（如 app/web/运营后台）',

    -- submit_time 最后提交时间（Unix 秒）
    `submit_time` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最后提交时间（Unix 秒）',

    PRIMARY KEY (`mid`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务官方认证申请文档表：提交资料与审核状态，附加资料以 JSON 存储。';

-- 认证附加键值表（当前用于统一社会信用代码，后续可扩展其他键）
CREATE TABLE IF NOT EXISTS `user_official_doc_addit`
(
    -- mid 用户 ID
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（关联 user_official_doc.mid）',

    -- property 附加键名（当前仅 credit_code）
    `property` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '附加键名（当前仅 credit_code 统一社会信用代码）',

    -- vstring 附加键值
    `vstring` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '附加键值',

    PRIMARY KEY (`mid`, `property`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务官方认证附加键值表：mid+property 唯一。';
