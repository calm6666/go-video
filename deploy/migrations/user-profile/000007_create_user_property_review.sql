-- =====================================================================
-- user-profile 服务 - 用户属性变更审核表迁移
-- =====================================================================
-- 用途：记录用户资料属性（头像/签名/昵称）变更审核记录，供审核流程
--   回溯；新增审核时同属性待审核记录自动归档（state=3）。
-- 数据所有者：user-profile 服务（审核结论更新由审核方经 RPC 完成，
--   审核流程编排属于 moderation-orchestrator）。
-- 参考映射：移植自参考仓库 member 服务 user_property_review 表。
-- 回滚：DROP TABLE IF EXISTS user_property_review;
-- 锁风险：仅建表；idx_mid_property 为空库建索引无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `user_property_review`
(
    -- id 自增主键，无业务语义
    `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键（无业务语义）',

    -- mid 用户 ID
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID（关联 user_base.mid）',

    -- old 变更前的值（头像取 URL 路径）
    `old` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更前的值（头像取 URL 路径）',

    -- new 变更后的值
    `new` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更后的值',

    -- state 审核状态：0 待审核、1 通过、2 驳回、3 已归档、10 自动审核中
    `state` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '审核状态：0 待审核、1 通过、2 驳回、3 已归档、10 自动审核中',

    -- property 审核属性：0 无意义、1 头像、2 签名、3 昵称
    `property` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '审核属性：0 无意义、1 头像、2 签名、3 昵称',

    -- is_monitor 提交时用户是否在监控名单
    `is_monitor` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '提交时用户是否在监控名单：0 否、1 是',

    -- extra 审核扩展信息 JSON
    `extra` TEXT NOT NULL COMMENT '审核扩展信息 JSON',

    -- operator 归档操作人（审核通过/驳回时回填）
    `operator` VARCHAR(64) NOT NULL DEFAULT '' COMMENT '归档操作人（审核通过/驳回时回填）',

    -- remark 归档备注（审核通过/驳回时回填）
    `remark` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '归档备注（审核通过/驳回时回填）',

    -- ctime 创建时间（Unix 秒）
    `ctime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',

    -- mtime 最近更新时间（Unix 秒）
    `mtime` BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',

    PRIMARY KEY (`id`),
    -- 查询/归档索引：按 mid+property 定位待审核记录
    KEY `idx_mid_property` (`mid`, `property`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='user-profile 服务用户属性变更审核表：头像/签名/昵称变更审核记录与归档。';
