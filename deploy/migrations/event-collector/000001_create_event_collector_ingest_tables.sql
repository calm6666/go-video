-- =====================================================================
-- event-collector 服务 - 事件接收批次台账与逐条事件台账
-- =====================================================================
-- 用途：客户端 SDK 批量上报（CollectEvents）与服务端埋点（IngestServerEvents）的
--       接收幂等、逐条校验结论与投递状态台账。事件正文不入库（只存 sha256 摘要与
--       可选对象存储引用），正文经 common/eventenvelope 投递 MQ 由 spm 消费。
-- 数据所有者：event-collector 服务（AGENTS.md §5「用户行为分析：event-collector / spm」）。
--       其他服务只能经 gRPC EventCollector 读取，禁止直连本库（含 spm、cron、operation）。
-- 库：go_video_event_collector。
-- 隐私（AGENTS.md §7）：本文件所有列都不含明文 IP/设备号/手机号。mid 是 account 主键引用；
--       device_hash 是 HMAC-SHA256(盐, 盐版本||设备号)，ip_segment 是 /24 脱敏段，
--       keyword_digest/payload_digest 是单向摘要。盐值只存在于 Secret/环境变量，
--       库泄露时无法离线还原任何主体标识。
-- 回滚：DROP TABLE IF EXISTS `ec_event_record`, `ec_ingest_batch`;
--       本库是行为分析台账，不含业务主数据，回滚不阻塞其他服务；
--       但已接收未投递的事件会随台账一并丢失（MQ 里已投递的部分不受影响），
--       回滚前须确认上游 SDK 的重发窗口（客户端本地缓冲）仍能补齐。
-- 锁风险：仅建表，无 ALTER，无锁风险；重复执行由 IF NOT EXISTS 兜底。
--       注意 ec_event_record 是高频写入表（每事件一行），上线后加列必须走
--       online DDL（ALGORITHM=INPLACE, LOCK=NONE）并单独提变更，不得在此文件追加。
-- =====================================================================

-- 批次接收台账：一次批量上报一行。
-- 幂等：uniq_batch_id(batch_id) 是采集幂等的唯一落点，重复上报不新增行，
--       logic 回读首行的计数与结论直接回放（INSERT IGNORE + 回查，不靠「先查后插」避免竞态）。
-- 计数列（total/accepted/duplicated/rejected/sampled_out/dispatched/dead）都是投影：
--       与 ec_event_record 同事务累加，并可用 model.RecountFromRecords 从事实行全量重算，
--       不作为唯一事实源（对账任务据此修漂移）。
-- 策略归因：policy_version + salt_version + sanitize_version 记录「这批用哪版采样/脱敏」，
--       事后重放与合规审计都以此为准，策略版本切换不追改历史行。
CREATE TABLE IF NOT EXISTS `ec_ingest_batch` (
  `id`               BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `batch_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '批次幂等键（客户端/调用方生成，<=64）',
  `source`           TINYINT      NOT NULL DEFAULT 1 COMMENT '来源：1 客户端 SDK、2 服务端内部埋点',
  `caller_service`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '服务端来源的服务名（信封 producer），客户端来源为空',
  `idempotency_key`  VARCHAR(128) NOT NULL DEFAULT '' COMMENT '服务端来源的动作幂等键（跨进程重试识别），客户端来源为空',
  `platform`         TINYINT      NOT NULL DEFAULT 0 COMMENT '平台：1 Android、2 iOS、3 HarmonyOS、4 桌面、5 服务端（0 未知）',
  `app_id`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '应用标识（区分多产品）',
  `app_version`      VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '客户端版本',
  `sdk_version`      VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '埋点 SDK 版本',
  `mid`              BIGINT       NOT NULL DEFAULT 0 COMMENT '登录用户 mid（account 主键引用，不建外键；0 未登录）',
  `device_hash`      VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '加盐设备哈希 h1:<hex>，非明文设备号',
  `ip_segment`       VARCHAR(43)  NOT NULL DEFAULT '' COMMENT '脱敏 IP 段，如 203.0.113.0/24，非明文出口 IP',
  `salt_version`     INT          NOT NULL DEFAULT 0 COMMENT '本次哈希使用的盐版本（轮换后旧数据不可逆推）',
  `policy_version`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '本批采用的采样/脱敏策略版本（ec_dispatch_policy.version）',
  `total`            INT          NOT NULL DEFAULT 0 COMMENT '批次内事件条数（投影，可重算）',
  `accepted`         INT          NOT NULL DEFAULT 0 COMMENT '通过校验并落库条数（投影，可重算）',
  `duplicated`       INT          NOT NULL DEFAULT 0 COMMENT 'event_id 已存在条数（投影，可重算）',
  `rejected`         INT          NOT NULL DEFAULT 0 COMMENT '校验被拒条数（投影，可重算）',
  `sampled_out`      INT          NOT NULL DEFAULT 0 COMMENT '命中采样丢弃条数（投影，可重算）',
  `dispatched`       INT          NOT NULL DEFAULT 0 COMMENT '已投递到 MQ 条数（投影，可重算）',
  `dead`             INT          NOT NULL DEFAULT 0 COMMENT '转死信条数（投影，可重算）',
  `request_bytes`    BIGINT       NOT NULL DEFAULT 0 COMMENT '请求体字节数（容量与攻击面观测）',
  `state`            TINYINT      NOT NULL DEFAULT 1 COMMENT '批次状态：1 已接收、2 已校验、3 投递中、4 已投递、5 部分失败、6 整批被拒',
  `top_reason`       TINYINT      NOT NULL DEFAULT 1 COMMENT '整批最主要拒绝原因（rpc.RejectReason 取值，1 无问题）',
  `client_seq`       INT          NOT NULL DEFAULT 0 COMMENT '客户端自增序号（识别乱序/重发批次）',
  `request_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '传输层请求 ID（排障）',
  `last_error`       VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次失败的脱敏摘要（禁止写 payload 原文）',
  `trace_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '批次级链路 ID',
  `received_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '接收时刻（Unix 秒）',
  `finished_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '投递收敛时刻（Unix 秒，0 未收敛；只写一次不改写）',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，翻页游标第 1 列）',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_batch_id` (`batch_id`),
  KEY `idx_ctime_id` (`ctime`, `id`),
  KEY `idx_state_ctime` (`state`, `ctime`),
  KEY `idx_source_ctime` (`source`, `ctime`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_device_ctime` (`device_hash`, `ctime`),
  KEY `idx_ipseg_ctime` (`ip_segment`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='事件接收批次台账：uniq_batch_id 使重复上报天然幂等；计数列均为可重算投影';

-- 逐条事件台账：一个 event_id 一行。
-- 定位：这是「采集与投递台账」，不是行为事实表；行为分析必须消费 MQ topic（AGENTS.md §7）。
-- 幂等：uniq_event_id(event_id) 是逐条去重落点；同一 event_id 第二次上报只累加批次
--       duplicated 计数并回 DUPLICATED 结论，不新增行、不重复投递（至少一次 + 下游按 ID 去重）。
-- 隐私：payload 只存 payload_digest + payload_bytes；搜索词只存 keyword_digest + 长度；
--       超大正文归档对象存储时也只存 bucket/object_key 引用（密钥进 Secret，不存 URL 明文）。
-- 留存：retention 超期由 services/cron 按 (ctime) 分批 DELETE（model.DeleteBefore），
--       台账可清，但已投递到 MQ 的行为数据不受影响。
CREATE TABLE IF NOT EXISTS `ec_event_record` (
  `id`                 BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `event_id`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '事件全局唯一 ID（客户端生成，服务端按其去重）',
  `batch_id`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '所属批次（ec_ingest_batch.batch_id）',
  `event_type`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '归一化后的事件类型，如 behavior.play',
  `category`           SMALLINT     NOT NULL DEFAULT 0 COMMENT '行为类别（rpc.BehaviorCategory 取值）',
  `schema_version`     INT          NOT NULL DEFAULT 0 COMMENT '服务端实际采用的事件结构版本（>=1）',
  `occurred_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '客户端上报的事件发生时间（Unix 秒）',
  `received_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '服务端接收时间（Unix 秒）',
  `clock_skew_seconds` BIGINT       NOT NULL DEFAULT 0 COMMENT 'occurred_at 与服务器时间偏差（有符号），识别时钟漂移',
  `decision`           TINYINT      NOT NULL DEFAULT 0 COMMENT '结论：1 通过、2 重复、3 拒绝、4 采样丢弃、5 批次降级',
  `reason`             TINYINT      NOT NULL DEFAULT 1 COMMENT '拒绝/丢弃原因码（rpc.RejectReason 取值，1 无问题）',
  `reason_detail`      VARCHAR(512) NOT NULL DEFAULT '' COMMENT '已脱敏补充说明（禁止写 payload 原文与明文标识）',
  `delivery_state`     TINYINT      NOT NULL DEFAULT 1 COMMENT '投递状态：1 不投递、2 待投递、3 已发送、4 退避中、5 死信（真值在 ec_pending_delivery）',
  `topic`              VARCHAR(128) NOT NULL DEFAULT '' COMMENT '投递目标 topic，如 behavior.play.v1（未投递为空）',
  `envelope_event_id`  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '投递信封的 event_id（与入参 event_id 分开记账）',
  `delivery_attempts`  INT          NOT NULL DEFAULT 0 COMMENT '已尝试投递次数',
  `next_retry_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '下次重试时间（Unix 秒，0 不需要）',
  `last_error`         VARCHAR(512) NOT NULL DEFAULT '' COMMENT '最近一次投递失败的脱敏摘要',
  `mid`                BIGINT       NOT NULL DEFAULT 0 COMMENT '归属用户 mid（0 未登录）',
  `device_hash`        VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '加盐设备哈希 h1:<hex>，非明文',
  `ip_segment`         VARCHAR(43)  NOT NULL DEFAULT '' COMMENT '脱敏 IP 段（/24），非明文',
  `salt_version`       INT          NOT NULL DEFAULT 0 COMMENT '哈希盐版本',
  `content_type`       VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '内容主类型：ugc/pgc/live/keyword 等',
  `content_id`         BIGINT       NOT NULL DEFAULT 0 COMMENT '内容 ID（catalog 作品/集主键引用，不建外键）',
  `aid`                BIGINT       NOT NULL DEFAULT 0 COMMENT '稿件 aid（UGC 场景，video 主键引用）',
  `vid`                VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '稿件 vid（UGC 场景）',
  `session_id`         VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '播放/会话主键（quality/play 场景串联）',
  `target_mid`         BIGINT       NOT NULL DEFAULT 0 COMMENT '行为对象用户（关注/分享），0 无',
  `keyword_digest`     VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '搜索词摘要 sha256:<hex>，原文不入库',
  `keyword_runes`      INT          NOT NULL DEFAULT 0 COMMENT '截断后搜索词长度（rune 数）',
  `payload_digest`     VARCHAR(80)  NOT NULL DEFAULT '' COMMENT '事件正文摘要 sha256:<hex>，原文不入库',
  `payload_bytes`      INT          NOT NULL DEFAULT 0 COMMENT '事件正文字节数',
  `blob_bucket`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '超大正文归档桶（仅引用，空表示未归档；密钥不入本库）',
  `blob_object_key`    VARCHAR(255) NOT NULL DEFAULT '' COMMENT '归档 object key（不含凭据、不是可公开访问 URL）',
  `sanitize_version`   VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '实际生效的脱敏规则版本',
  `policy_version`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '实际生效的采样/脱敏策略版本（归因依据）',
  `trace_id`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路 ID',
  `ctime`              BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，翻页游标第 1 列）',
  `mtime`              BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  KEY `idx_batch_ctime` (`batch_id`, `ctime`),
  KEY `idx_ctime_id` (`ctime`, `id`),
  KEY `idx_type_ctime` (`event_type`, `ctime`),
  KEY `idx_decision_ctime` (`decision`, `ctime`),
  KEY `idx_reason_ctime` (`reason`, `ctime`),
  KEY `idx_delivery_ctime` (`delivery_state`, `ctime`),
  KEY `idx_topic_state` (`topic`, `delivery_state`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_device_ctime` (`device_hash`, `ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='逐条事件接收与投递台账：uniq_event_id 去重；正文与搜索词只存摘要（AGENTS.md §7）';
