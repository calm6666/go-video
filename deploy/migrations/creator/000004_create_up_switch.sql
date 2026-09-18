-- =====================================================================
-- creator 服务 - UP 主关注弹窗开关表迁移
-- =====================================================================
-- 用途：记录 UP 主对不同业务场景下关注弹窗的开关偏好。
-- 数据所有者：creator 服务（AGENTS.md §5）。其他服务禁止直接读写本表，
--   只能通过 gRPC SetUpSwitch/UpSwitch 接口访问。
-- 参考映射：移植自参考仓库 up 服务的 up_switch 表。
-- 回滚：DROP TABLE IF EXISTS up_switch;
-- 字符集：utf8mb4，引擎：InnoDB
-- 锁风险：仅建表，无锁风险。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `up_switch`
(
    -- mid 用户 ID
    `mid` BIGINT UNSIGNED NOT NULL COMMENT '用户 ID',

    -- from 业务来源：0 播放器关注开关、1 UP 主荣誉周报退订
    `from` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '业务来源：0 播放器关注开关、1 UP 主荣誉周报退订',

    -- state 开关状态：0 关闭、1 打开
    `state` TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '开关状态：0 关闭、1 打开',

    PRIMARY KEY (`mid`, `from`)
) ENGINE = InnoDB
  DEFAULT CHARSET = utf8mb4
  DEFAULT COLLATE = utf8mb4_unicode_ci
  COMMENT ='creator 服务 UP 主关注弹窗开关表：mid+from 维度的偏好状态。creator 持有，禁止跨服务直接读写。';
