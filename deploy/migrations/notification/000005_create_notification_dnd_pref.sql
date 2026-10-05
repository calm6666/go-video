-- =====================================================================
-- notification 服务 - 用户通道偏好与免打扰表
-- =====================================================================
-- 数据所有者：notification 服务（AGENTS.md §5）。用户主资料归 user-profile，
-- 本表只保存通知通道偏好这类 notification 自有的投递控制数据。
-- 语义：muted_channels 为通道位掩码（1 Push、2 短信、4 邮件）；
--       quiet_start/quiet_end 为 HH:MM 本地时段，支持跨天（如 22:00-08:00），
--       判定使用 timezone 列的 IANA 时区，空串表示不设时段。
-- 约束：本表只服务“减少打扰”，不提供任何营销/广告推送开关（AGENTS.md §1）。
-- 影响：新建表；mid 主键 + Upsert 保证一个用户一行。
-- 锁风险：CREATE TABLE IF NOT EXISTS，仅元数据锁，可在线执行。
-- 回滚：DROP TABLE IF EXISTS `notification_dnd_pref`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `notification_dnd_pref` (
  `mid`            BIGINT      NOT NULL COMMENT '用户 ID（主键）',
  `muted_channels` INT         NOT NULL DEFAULT 0 COMMENT '已关闭通道位掩码：1 Push、2 短信、4 邮件',
  `quiet_start`    CHAR(5)     NOT NULL DEFAULT '' COMMENT '免打扰开始时间 HH:MM，空串表示不设时段',
  `quiet_end`      CHAR(5)     NOT NULL DEFAULT '' COMMENT '免打扰结束时间 HH:MM，支持跨天',
  `timezone`       VARCHAR(64) NOT NULL DEFAULT 'Asia/Shanghai' COMMENT 'IANA 时区名，用于跨天与夏令时边界判定',
  `state`          TINYINT     NOT NULL DEFAULT 0 COMMENT '免打扰总开关：0 关闭、1 开启',
  `ctime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`mid`),
  KEY `idx_mtime` (`mtime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户通知通道偏好与免打扰设置表';
