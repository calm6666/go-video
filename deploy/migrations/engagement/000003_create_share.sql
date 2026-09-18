-- 分享日志
-- 幂等：唯一索引 (oid, mid, tp, day) 保证同用户对同对象同一天只计一次。
CREATE TABLE IF NOT EXISTS `share_log` (
  `id`     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `oid`    BIGINT       NOT NULL DEFAULT 0 COMMENT '目标 ID',
  `mid`    BIGINT       NOT NULL DEFAULT 0 COMMENT '用户 ID',
  `tp`     TINYINT      NOT NULL DEFAULT 0 COMMENT '目标类型',
  `day`    INT          NOT NULL DEFAULT 0 COMMENT 'YYYYMMDD，按天幂等',
  `ctime`  BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_oid_mid_tp_day` (`oid`, `mid`, `tp`, `day`),
  KEY `idx_oid_tp` (`oid`, `tp`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='分享日志';

-- 分享计数（每对象一条）
CREATE TABLE IF NOT EXISTS `share_stat` (
  `id`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `oid`     BIGINT       NOT NULL DEFAULT 0 COMMENT '目标 ID',
  `tp`      TINYINT      NOT NULL DEFAULT 0 COMMENT '目标类型',
  `count`   BIGINT       NOT NULL DEFAULT 0 COMMENT '累计分享数',
  `ctime`   BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`   BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_oid_tp` (`oid`, `tp`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='分享计数表';
