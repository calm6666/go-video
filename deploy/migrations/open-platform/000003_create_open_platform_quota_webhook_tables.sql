-- =====================================================================
-- open-platform 服务 - 配额规则/用量投影、调用流水与 Webhook 回调
-- =====================================================================
-- owner：open-platform 服务（数据所有者，AGENTS.md §5）。库：go_video_open_platform。
-- 影响：新建 5 张表（配额规则、配额用量投影、调用流水、回调端点、投递任务）
--       并 seed 全局默认配额；不改动任何已有表；不建跨服务外键。
-- 配额口径：op_api_call_log 是事实源（append-only），op_quota_usage 只是「按窗口计数」的
--       投影：网关鉴权不扫流水，只读写投影；投影丢失只会短时放宽/收紧限额，
--       不破坏业务数据，随时可由 RecomputeQuota 从流水重算（因此本表允许按保留期清理）。
-- 幂等口径：request_id（网关请求）与 event_id（领域事件）两个唯一键分别是
--       AuthorizeRequest 与 EnqueueWebhookEvent 的幂等锚点，重试不重复扣配额、不重复投递。
-- 回滚：DROP TABLE IF EXISTS
--         `op_webhook_delivery`,`op_webhook_endpoint`,`op_api_call_log`,`op_quota_usage`,`op_quota_policy`;
--       回滚会丢失调用审计与待投递事件，必须先停用网关开放入口并导出死信；
--       op_quota_usage 可直接重建，无需备份。
-- 锁风险：仅建表 + 幂等 seed（唯一键 upsert，不覆盖运营改过的 enabled），无 ALTER，无锁风险。
-- =====================================================================

-- 配额规则表：应用 × 接口 × 时间窗。app_id=0 为全局默认，api_code='*' 为该应用全部接口。
-- quota_limit 列名注意：LIMIT 是 MySQL 保留字，禁止用裸 limit 建列。
-- 多窗口并存是有意设计（例：60s→100 与 86400→50000 同时生效），鉴权对生效层级内
-- 每条规则各扣一次窗口，任一超限即拒；层级选取规则见 model.NarrowPolicies。
CREATE TABLE IF NOT EXISTS `op_quota_policy` (
  `policy_id`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '规则 ID（主键）',
  `app_id`         BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID，0 表示全局默认',
  `api_code`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '接口标识（配额与 scope 的键），"*" 表示全部接口',
  `window_seconds` BIGINT       NOT NULL DEFAULT 0 COMMENT '时间窗长度（秒，必须 > 0）',
  `quota_limit`    BIGINT       NOT NULL DEFAULT 0 COMMENT '窗口内允许次数；<=0 表示禁用该接口',
  `enabled`        TINYINT      NOT NULL DEFAULT 1 COMMENT '是否生效：0 停用、1 生效',
  `operator_mid`   BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次修改的运营 mid（必填 > 0）',
  `reason`         VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更原因（审计）',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒，列表游标第 1 列）',
  PRIMARY KEY (`policy_id`),
  UNIQUE KEY `uniq_app_api_window` (`app_id`, `api_code`, `window_seconds`),
  KEY `idx_app_mtime` (`app_id`, `mtime`, `policy_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='配额规则表：同一 (应用,接口,窗口) 只有一行，upsert 即改限额';

-- 配额用量投影表：应用 × 接口 × 窗口起点 → 已用次数（真值在 op_api_call_log）。
-- window_end 冗余存列：清理任务按「窗口是否已结束」扫描，写成算术表达式会让索引失效。
-- 唯一键使「建行或累加」在一条 INSERT ... ON DUPLICATE KEY UPDATE 内原子完成，不丢更新。
CREATE TABLE IF NOT EXISTS `op_quota_usage` (
  `usage_id`        BIGINT  NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `app_id`          BIGINT  NOT NULL DEFAULT 0 COMMENT '应用 ID（全局默认规则用 0 记账，与 op_quota_policy.app_id 对齐）',
  `api_code`        VARCHAR(64) NOT NULL DEFAULT '' COMMENT '接口标识（"*" 表示合并口径）',
  `window_seconds`  BIGINT  NOT NULL DEFAULT 0 COMMENT '窗口长度（秒）',
  `window_start`    BIGINT  NOT NULL DEFAULT 0 COMMENT '窗口起点（Unix 秒，由 AlignWindow 取齐）',
  `window_end`      BIGINT  NOT NULL DEFAULT 0 COMMENT '窗口终点（Unix 秒 = start + seconds，供索引化清理）',
  `used`            BIGINT  NOT NULL DEFAULT 0 COMMENT '已用次数（投影，可由流水重算）',
  `limit_snapshot`  BIGINT  NOT NULL DEFAULT 0 COMMENT '记账时的限额快照（解释“为什么被拒”，不代表当前生效限额）',
  `updated_at`      BIGINT  NOT NULL DEFAULT 0 COMMENT '最近一次累加时间（Unix 秒）',
  `ctime`           BIGINT  NOT NULL DEFAULT 0 COMMENT '窗口首次记账时间（Unix 秒）',
  PRIMARY KEY (`usage_id`),
  UNIQUE KEY `uniq_window` (`app_id`, `api_code`, `window_seconds`, `window_start`),
  KEY `idx_window_end` (`window_end`),
  KEY `idx_app_api_start` (`app_id`, `api_code`, `window_start`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='配额用量投影表：可完全由 op_api_call_log 重建，允许按保留期清理';

-- 接口调用流水表：配额与审计的事实来源（append-only）。
-- uniq_request_id 是 AuthorizeRequest 的幂等锚点：网关重试同一 request_id 时
-- 直接回放首次判定结果，既不重复扣配额也不写第二条流水。
-- 隐私口径：只记 body_digest 与脱敏后的 client_ip_masked，不记请求体、不记 token 明文；
-- 本表内容只对运营开放，不出现在任何面向第三方应用的响应中。
CREATE TABLE IF NOT EXISTS `op_api_call_log` (
  `call_log_id`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '流水 ID（主键）',
  `request_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '网关请求幂等键（必填且唯一）',
  `app_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID',
  `api_code`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '接口标识（配额键）',
  `mid`              BIGINT       NOT NULL DEFAULT 0 COMMENT '授权用户 mid（应用级签名为 0）',
  `token_id`         BIGINT       NOT NULL DEFAULT 0 COMMENT '使用的 token 行（签名模式为 0）',
  `grant_id`         BIGINT       NOT NULL DEFAULT 0 COMMENT '授权关系（签名模式为 0）',
  `allowed`          TINYINT      NOT NULL DEFAULT 1 COMMENT '判定结果：1 放行、2 拒绝',
  `deny_reason`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '拒绝原因码（inactive/expired/revoked/app_suspended/scope_missing/quota_exceeded/signature_invalid）',
  `method`           VARCHAR(16)  NOT NULL DEFAULT '' COMMENT 'HTTP 方法',
  `path`             VARCHAR(255) NOT NULL DEFAULT '' COMMENT '请求路径（不含查询串，避免把参数写进流水）',
  `body_digest`      VARCHAR(128) NOT NULL DEFAULT '' COMMENT '请求体摘要 sha256:<hex>，不含原文',
  `client_ip_masked` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '脱敏后的来源 IP（IPv4 抹末段、IPv6 抹后半段）',
  `quota_limit`      BIGINT       NOT NULL DEFAULT 0 COMMENT '判定时生效限额',
  `quota_remaining`  BIGINT       NOT NULL DEFAULT 0 COMMENT '判定后剩余额度',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '判定时间（Unix 秒，配额重算按本列取窗）',
  PRIMARY KEY (`call_log_id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  KEY `idx_app_api_ctime` (`app_id`, `api_code`, `ctime`),
  KEY `idx_ctime` (`ctime`),
  KEY `idx_token` (`token_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='接口调用流水表：配额与审计的事实来源；按保留期清理前先归档到离线仓';

-- 回调端点表：签名密钥不入库——投递签名由服务端 master pepper 与 app_id + sign_key_version
-- 派生，表里只留版本号，因此数据库泄露既拿不到历史签名密钥、也无法伪造合法回调。
-- verified_at=0 的端点不参与投递（ListMatching 过滤），避免注册任意 URL 直接变成 SSRF 跳板；
-- URL 本身的校验（https only、禁止内网/本机地址）在 logic 层完成。
CREATE TABLE IF NOT EXISTS `op_webhook_endpoint` (
  `endpoint_id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '端点 ID（主键）',
  `app_id`               BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID',
  `event_type`           TINYINT      NOT NULL DEFAULT 1 COMMENT '订阅事件类型：1 投稿转码/审核结果、2 内容下架、3 授权被撤销、4 配额告警（无商业化事件）',
  `callback_url`         VARCHAR(512) NOT NULL DEFAULT '' COMMENT '回调地址（https only，禁止内网地址）',
  `sign_key_version`     INT          NOT NULL DEFAULT 1 COMMENT '当前签名密钥版本（不含任何密钥材料）',
  `enabled`              TINYINT      NOT NULL DEFAULT 1 COMMENT '是否启用：0 暂停投递、1 启用',
  `description`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注',
  `verified_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '验证通过时间（Unix 秒，0 表示未验证即不投递）',
  `challenge_hash`       CHAR(64)     NOT NULL DEFAULT '' COMMENT '验证挑战的哈希（明文挑战只在注册响应出现一次）',
  `challenge_expires_at` BIGINT       NOT NULL DEFAULT 0 COMMENT '挑战有效期（Unix 秒）',
  `deleted_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '删除时间（Unix 秒，0 表示未删除；软删以保留投递归属）',
  `delete_reason`        VARCHAR(255) NOT NULL DEFAULT '' COMMENT '删除原因（审计）',
  `ctime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`endpoint_id`),
  UNIQUE KEY `uniq_app_event_url` (`app_id`, `event_type`, `callback_url`),
  KEY `idx_event_deliverable` (`event_type`, `enabled`, `deleted_at`),
  KEY `idx_app_deleted` (`app_id`, `deleted_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='Webhook 回调端点表：只存签名密钥版本号，未验证的端点不投递';

-- 投递任务表：一个（事件，端点）一行，uniq_event_endpoint 保证重复入队不产生第二个任务。
-- 退避与死信：attempt 在 Claim 时自增，超过 max_attempts 置 DEAD 等待人工重放；
-- next_retry_at 在 DELIVERING 期间配合 lease_until 做租约，worker 崩溃后任务会被回收，
-- 不会永久卡在「投递中」。payload 必须存（重放不能要求上游重新生产），
-- 但对外只回显 payload_digest，且按保留期清空正文列（PurgePayloadBefore）。
CREATE TABLE IF NOT EXISTS `op_webhook_delivery` (
  `delivery_id`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '投递任务 ID（主键，参与签名串）',
  `app_id`           BIGINT       NOT NULL DEFAULT 0 COMMENT '应用 ID（冗余，便于按应用排障）',
  `endpoint_id`      BIGINT       NOT NULL DEFAULT 0 COMMENT '端点 ID（op_webhook_endpoint.endpoint_id）',
  `event_type`       TINYINT      NOT NULL DEFAULT 1 COMMENT '事件类型快照（与端点一致，枚举见 op_webhook_endpoint.event_type）',
  `event_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '领域事件幂等键（来自事件 envelope，必填）',
  `payload`          MEDIUMTEXT   NOT NULL COMMENT '投递正文 JSON（禁止含 token/secret/身份证/手机号明文；到期清理）',
  `payload_digest`   VARCHAR(128) NOT NULL DEFAULT '' COMMENT '正文摘要 sha256:<hex>，对外只回显本列',
  `state`            TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 待投递、2 投递中、3 成功、4 已排定重试、5 死信、6 人工忽略/端点删除后抑制',
  `attempt`          INT          NOT NULL DEFAULT 0 COMMENT '已尝试次数（Claim 时自增，人工重放归零）',
  `max_attempts`     INT          NOT NULL DEFAULT 0 COMMENT '最大尝试次数（入队时从配置快照，改配置不影响历史任务）',
  `next_retry_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '下次可投递时间（Unix 秒）',
  `lease_until`      BIGINT       NOT NULL DEFAULT 0 COMMENT 'worker 租约到期时间（Unix 秒，0 表示无租约）',
  `last_status_code` BIGINT       NOT NULL DEFAULT 0 COMMENT '最近一次 HTTP 状态码',
  `last_error`       VARCHAR(255) NOT NULL DEFAULT '' COMMENT '最近一次错误摘要（截断且脱敏，禁止含响应体原文）',
  `delivered_at`     BIGINT       NOT NULL DEFAULT 0 COMMENT '成功时间（Unix 秒）',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '入队时间（Unix 秒，列表游标第 1 列）',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`delivery_id`),
  UNIQUE KEY `uniq_event_endpoint` (`event_id`, `endpoint_id`),
  KEY `idx_due` (`state`, `next_retry_at`),
  KEY `idx_lease` (`state`, `lease_until`),
  KEY `idx_app_ctime` (`app_id`, `ctime`, `delivery_id`),
  KEY `idx_endpoint_state` (`endpoint_id`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='Webhook 投递任务表：退避重试 + 死信；同一 (event_id,endpoint_id) 只有一行';

-- ---------------------------------------------------------------------
-- 全局默认配额 seed：app_id=0 + api_code='*' 是最兜底层级（第 4 层），
-- 应用级与接口级规则一旦存在就会覆盖它（见 model.NarrowPolicies 的层级选取）。
-- 幂等：ON DUPLICATE KEY UPDATE 只刷新限额与窗口，不覆盖运营改过的 enabled/reason。
-- ---------------------------------------------------------------------
INSERT INTO `op_quota_policy`
  (`app_id`, `api_code`, `window_seconds`, `quota_limit`, `enabled`, `operator_mid`, `reason`, `ctime`, `mtime`)
VALUES
  (0, '*', 60,    600,   1, 0, 'seed：全局默认每分钟上限',  UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  (0, '*', 3600,  20000, 1, 0, 'seed：全局默认每小时上限',  UNIX_TIMESTAMP(), UNIX_TIMESTAMP()),
  (0, '*', 86400, 200000, 1, 0, 'seed：全局默认每日上限',   UNIX_TIMESTAMP(), UNIX_TIMESTAMP())
ON DUPLICATE KEY UPDATE
  `quota_limit` = VALUES(`quota_limit`),
  `reason` = VALUES(`reason`),
  `mtime` = VALUES(`mtime`);
