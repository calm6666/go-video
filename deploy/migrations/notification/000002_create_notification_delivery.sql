-- =====================================================================
-- notification 服务 - 外部通道投递任务与回执表
-- =====================================================================
-- 数据所有者：notification 服务（AGENTS.md §5）。站内信正文归 inbox，本表只记外部通道投递。
-- 影响：新建表；biz_key 唯一索引是幂等写入依赖（AGENTS.md §5 要求所有写接口有唯一约束）。
--       隐私：不落明文手机号/邮箱与渲染后正文，只落受控标识 target_ref、渲染变量快照
--       params_json 与渲染结果摘要 payload_digest；异步投递时按锁定的模板版本重渲染。
-- 锁风险：CREATE TABLE IF NOT EXISTS，仅元数据锁，可在线执行。
-- 回滚：DROP TABLE IF EXISTS `notification_delivery`;
-- =====================================================================
CREATE TABLE IF NOT EXISTS `notification_delivery` (
  `delivery_id`      VARCHAR(64)  NOT NULL COMMENT '投递任务 ID（ULID）',
  `biz_key`          VARCHAR(64)  NOT NULL COMMENT '行级幂等键：sha256(请求级 biz_key:channel:收件人)',
  `biz_group_key`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '调用方请求级业务键，用于整批查询',
  `mid`              BIGINT       NOT NULL DEFAULT 0 COMMENT '接收人用户 ID，0 表示只有 target_ref',
  `channel`          TINYINT      NOT NULL COMMENT '通道：1 Push、2 短信、3 邮件',
  `template_code`    VARCHAR(64)  NOT NULL COMMENT '模板码',
  `template_version` INT          NOT NULL DEFAULT 0 COMMENT '锁定的模板版本',
  `lang`             VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '语言',
  `target_ref`       VARCHAR(128) NOT NULL DEFAULT '' COMMENT '受控投递标识（设备 token 引用/供应商收件人引用），禁止明文号码',
  `params_json`      TEXT         NOT NULL COMMENT '渲染变量快照（JSON，调用方需脱敏）',
  `payload_digest`   CHAR(64)     NOT NULL DEFAULT '' COMMENT '渲染结果 sha256 hex',
  `state`            TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 pending、2 sent、3 failed、4 retry、5 dead_letter、6 suppressed',
  `provider`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '实际使用的通道适配器名',
  `provider_msg_id`  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '供应商回执消息 ID',
  `priority`         TINYINT      NOT NULL DEFAULT 2 COMMENT '优先级：1 低、2 普通、3 高',
  `retry_count`      INT          NOT NULL DEFAULT 0 COMMENT '已重试次数',
  `next_retry_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒），0 表示不再重试',
  `last_error`       VARCHAR(500) NOT NULL DEFAULT '' COMMENT '最近一次错误（脱敏，不含密钥）',
  `sent_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '投递成功时间（Unix 秒）',
  `expire_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '过期时间（Unix 秒），0 表示不过期',
  `source_event_id`  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '来源事件 ID（RPC 直投时为空）',
  `trace_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路 ID',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`delivery_id`),
  UNIQUE KEY `uk_biz_key` (`biz_key`),
  KEY `idx_state_retry` (`state`, `next_retry_at`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_group_ctime` (`biz_group_key`, `ctime`),
  KEY `idx_source_event` (`source_event_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='外部通道投递任务与供应商回执表';
