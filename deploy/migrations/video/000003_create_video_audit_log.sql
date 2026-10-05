-- =====================================================================
-- video 服务 - 稿件状态流转审计表
-- =====================================================================
-- 与 UpdateState 在同一事务内写入（model.InsertAuditLog 接收 tx）。
-- 只追加不修改，保留审计证据（AGENTS.md §8）。
-- owner：video 服务（deploy/migrations/video，库 go_video_video）；影响范围：新增 1 张表。
-- 回滚：DROP TABLE IF EXISTS `video_audit_log`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `video_audit_log` (
  `id`         BIGINT      NOT NULL AUTO_INCREMENT COMMENT '审计记录 ID',
  `aid`        BIGINT      NOT NULL DEFAULT 0 COMMENT '稿件 ID',
  `from_state` TINYINT     NOT NULL DEFAULT 0 COMMENT '原状态',
  `to_state`   TINYINT     NOT NULL DEFAULT 0 COMMENT '新状态',
  `operator`   VARCHAR(64) NOT NULL DEFAULT '' COMMENT '操作人（运营 ID 或 system）',
  `reason`     VARCHAR(500) NOT NULL DEFAULT '' COMMENT '变更原因',
  `ctime`      BIGINT      NOT NULL DEFAULT 0 COMMENT '变更时间（Unix 秒）',
  PRIMARY KEY (`id`),
  KEY `idx_aid_id` (`aid`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='稿件状态流转审计表';
