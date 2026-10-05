-- =====================================================================
-- rights 服务 - 版权合同与播放窗口
-- =====================================================================
-- 数据所有者：rights 服务（AGENTS.md §5）。播放鉴权和定时任务须校验窗口，
-- 到期/撤权由 rights 发布事件驱动下架。
-- 回滚：DROP TABLE IF EXISTS `rights_contract`,`rights_window`;
-- =====================================================================

-- 版权合同表
CREATE TABLE IF NOT EXISTS `rights_contract` (
  `contract_id` BIGINT       NOT NULL AUTO_INCREMENT COMMENT '合同 ID（主键）',
  `owner_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '版权方 ID',
  `title`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '合同标题',
  `sign_date`   BIGINT       NOT NULL DEFAULT 0 COMMENT '签订日期（Unix 秒）',
  `start_date`  BIGINT       NOT NULL DEFAULT 0 COMMENT '生效日期（Unix 秒）',
  `end_date`    BIGINT       NOT NULL DEFAULT 0 COMMENT '到期日期（Unix 秒）',
  `regions`     VARCHAR(512) NOT NULL DEFAULT '' COMMENT '授权地区代码（逗号分隔）',
  `state`       TINYINT      NOT NULL DEFAULT 1 COMMENT '合同状态：1 active、2 terminated',
  `ctime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`       BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`contract_id`),
  KEY `idx_owner_ctime` (`owner_id`, `ctime`),
  KEY `idx_state_end` (`state`, `end_date`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='版权合同表';

-- 版权窗口表：一条窗口 = 合同下某内容在某地区的授权时段
-- FindActiveByContentRegion 走 (content_id, content_type, region) + state 过滤。
CREATE TABLE IF NOT EXISTS `rights_window` (
  `window_id`    BIGINT      NOT NULL AUTO_INCREMENT COMMENT '窗口 ID（主键）',
  `contract_id`  BIGINT      NOT NULL DEFAULT 0 COMMENT '关联合同 ID',
  `content_id`   BIGINT      NOT NULL DEFAULT 0 COMMENT '内容 ID（catalog 集/作品等）',
  `content_type` TINYINT     NOT NULL DEFAULT 0 COMMENT '内容类型：1 pgc、2 ugc',
  `region`       VARCHAR(16) NOT NULL DEFAULT '' COMMENT '授权地区代码',
  `start_time`   BIGINT      NOT NULL DEFAULT 0 COMMENT '窗口开始时间（Unix 秒）',
  `end_time`     BIGINT      NOT NULL DEFAULT 0 COMMENT '窗口结束时间（Unix 秒）',
  `state`        TINYINT     NOT NULL DEFAULT 1 COMMENT '窗口状态：1 active、2 expired、3 revoked',
  `ctime`        BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`window_id`),
  UNIQUE KEY `uniq_contract_content_region` (`contract_id`, `content_id`, `content_type`, `region`),
  KEY `idx_content_region_state` (`content_id`, `content_type`, `region`, `state`),
  KEY `idx_state_end` (`state`, `end_time`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='版权播放窗口表';
