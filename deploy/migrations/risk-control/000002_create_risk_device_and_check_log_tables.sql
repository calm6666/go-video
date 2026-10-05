-- =====================================================================
-- risk-control 服务 - 设备画像 / 设备账号关联 / 裁决日志
-- =====================================================================
-- 数据库：go_video_risk_control（与 000001 同库；建库由 scripts/migrate.ps1 负责）。
-- 数据所有者：risk-control 服务（AGENTS.md §5）。
--
-- 表与代码对应关系（列名严格取自 services/risk-control/model/*.go 的 db tag 与 SQL 字符串）：
--   * risk_device_profile  model/device.go RiskDeviceProfile —— 设备画像（只存 sha256 摘要）
--   * risk_device_mid      model/device.go RiskDeviceMid     —— 设备-账号关联事实
--   * risk_check_log       model/checklog.go RiskCheckLog    —— 每次 CheckAction 的可解释裁决日志
--
-- 唯一键与幂等设计：
--   1. risk_device_profile.uniq_device_hash：Upsert 的 ON DUPLICATE KEY UPDATE 锚点，
--      first_seen 用 LEAST、last_seen 用 GREATEST 收敛，重复上报不会产生新行。
--   2. risk_device_mid.uniq_device_mid：INSERT IGNORE + 命中后仅刷新 last_seen，
--      同设备同账号只留一条事实；related_mid_count 是这张表 COUNT(*) 的投影，
--      可全量重算（UpdateRelatedCount），不作为唯一事实源。
--   3. risk_check_log.uniq_request_id：CheckAction 的 request_id 幂等键，
--      INSERT IGNORE 保证同一 request_id 的重试只在审计表留下首条裁决
--      （重复裁决由 Redis rc:ck:<request_id> 回放，不再落第二条）。
--
-- 敏感信息约束（AGENTS.md §7）：本组表只保存摘要与截断值，不保存设备号原文、
--   明文 IP、手机号；hit_rule_ids 形如 "rule_id@version,..."，最多 40 条（maxHitsInLog），
--   因此 VARCHAR(500) 足够，超长视为规则配置异常而不是放宽列宽。
--
-- 索引取自真实查询路径：
--   * risk_device_profile：FindOne / UpdateRelatedCount 均按 device_hash 命中唯一索引。
--   * risk_device_mid：AddRelation(唯一键)、CountByDevice(device_hash 前缀)。
--   * risk_check_log：写入按 request_id 去重；运营排障按 (mid, ctime) 取时间线、
--     按 ctime 做保留期归档（config.RiskControl.CheckLogRetentionDays，归档由 services/cron 执行）。
--     本服务不提供日志读接口，索引只覆盖上述两类访问，不预留分析型索引。
--
-- 容量：risk_check_log 与 CheckAction 同量级（当前是投稿/评论/弹幕/关注/登录/改名/开播
--   七个动作的检查流量），是本库增长最快的表；超过保留期先归档再 DELETE，不分库分表。
--
-- 回滚：
--   DROP TABLE IF EXISTS `risk_check_log`;
--   DROP TABLE IF EXISTS `risk_device_mid`;
--   DROP TABLE IF EXISTS `risk_device_profile`;
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `risk_device_profile` (
  `id`                BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `device_hash`       CHAR(64)     NOT NULL COMMENT '设备受控 ID：sha256(小写去空格设备号) 十六进制，不存原文',
  `labels`            VARCHAR(512) NOT NULL DEFAULT '' COMMENT '风险标签，逗号分隔、去重、字典序（model.MergeLabels 保证同集合恒定同串）',
  `first_seen`        BIGINT       NOT NULL DEFAULT 0 COMMENT '首次出现时间（Unix 秒），Upsert 取 LEAST',
  `last_seen`         BIGINT       NOT NULL DEFAULT 0 COMMENT '最近出现时间（Unix 秒），Upsert 取 GREATEST',
  `related_mid_count` BIGINT       NOT NULL DEFAULT 0 COMMENT '关联账号数（由 risk_device_mid COUNT 重算的投影）',
  `risk_score`        SMALLINT     NOT NULL DEFAULT 0 COMMENT '设备风险分 0-100，供 device_risk_score 指标读取',
  `source`            VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '最近写入来源：login/gateway/operation/system',
  `operator`          BIGINT       NOT NULL DEFAULT 0 COMMENT '最近人工写入的运营 ID（0 表示系统写入）',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_device_hash` (`device_hash`),
  KEY `idx_last_seen` (`last_seen`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='风控设备画像表（设备风险分与标签，指标 device_risk_score/device_mid_count 的事实源）';

-- 回滚：DROP TABLE IF EXISTS `risk_device_mid`;
CREATE TABLE IF NOT EXISTS `risk_device_mid` (
  `id`          BIGINT   NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `device_hash` CHAR(64) NOT NULL COMMENT '设备受控 ID（sha256 摘要）',
  `mid`         BIGINT   NOT NULL COMMENT '在该设备上出现过的账号 ID',
  `first_seen`  BIGINT   NOT NULL DEFAULT 0 COMMENT '首次关联时间（Unix 秒）',
  `last_seen`   BIGINT   NOT NULL DEFAULT 0 COMMENT '最近关联时间（Unix 秒），重复登录只刷新此列',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_device_mid` (`device_hash`, `mid`),
  KEY `idx_mid` (`mid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='风控设备-账号关联表（related_mid_count 的事实源，可重算）';

-- 回滚：DROP TABLE IF EXISTS `risk_check_log`;
CREATE TABLE IF NOT EXISTS `risk_check_log` (
  `id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `request_id`   VARCHAR(64)  NOT NULL COMMENT 'CheckAction 幂等键（唯一），重复上报保留首条裁决',
  `mid`          BIGINT       NOT NULL DEFAULT 0 COMMENT '账号 ID（未登录场景为 0）',
  `action`       TINYINT      NOT NULL DEFAULT 0 COMMENT '受保护动作：1 投稿、2 评论、3 弹幕、4 关注、5 登录、6 改名、7 直播开播',
  `decision`     TINYINT      NOT NULL DEFAULT 1 COMMENT '裁决：1 ALLOW、2 CHALLENGE、3 BLOCK、4 REVIEW',
  `score`        SMALLINT     NOT NULL DEFAULT 0 COMMENT '0-100 严重度（decisionScore + 额外命中每条 +5，可复算）',
  `hit_rule_ids` VARCHAR(500) NOT NULL DEFAULT '' COMMENT '命中规则，格式 "rule_id@version,..."，最多 40 条，顺序为 priority DESC、rule_id ASC',
  `basis`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '决策依据：blacklist/whitelist/punishment/rules/no_rule/fallback_db_unavailable/fallback_local_rate_limited',
  `platform`     VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '客户端平台（android/ios/harmony/desktop）',
  `app_version`  VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '客户端版本',
  `device_hash`  CHAR(64)     NOT NULL DEFAULT '' COMMENT '设备受控 ID（未上报为空串）',
  `ip_hash`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '调用方预哈希 IP（未提供或非法为空串）',
  `trace_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路追踪 ID',
  `degraded`     TINYINT      NOT NULL DEFAULT 0 COMMENT '0 正常裁决、1 依赖故障降级（ALLOW/BLOCK-on-error，必须可统计）',
  `ctime`        BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒），归档按此列，保留期见 CheckLogRetentionDays',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='风控裁决日志表（事后解释「当时为什么这么判」与规则回放，不存明文 IP/设备号）';
