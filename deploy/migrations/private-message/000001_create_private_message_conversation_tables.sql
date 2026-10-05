-- =====================================================================
-- private-message 服务 - 单聊会话、会话成员与私信消息
-- =====================================================================
-- owner：private-message 服务（数据所有者，AGENTS.md §5）。库：go_video_private_message。
-- 影响：新建 3 张表（会话主体、会话成员投影、消息密文），不改动任何已有表；
--       不建跨服务外键，user_a/user_b/mid/sender_mid/media_ref 只保存对方主键。
-- 边界：与 inbox（站内信/系统消息）分库分表，绝不共表——私信是用户之间的通信，
--       系统消息是平台对用户的投递，两者的留存、加密与审计口径不同
--       （services/private-message/README.md 约束「不与系统消息共表」）。
-- 回滚：DROP TABLE IF EXISTS `pm_message`,`pm_conversation_member`,`pm_conversation`;
--       私信属 P4 私密通信数据，回滚前必须走数据导出审批（法务/安全），
--       禁止把导出文件放进对象存储公共读桶；回滚会丢失未读游标，
--       但 unread_count 与列表投影可由 pm_message 全量重算（RebuildProjection）。
-- 锁风险：仅建表，无 ALTER，无锁风险；重复执行由 IF NOT EXISTS 兜底。
-- =====================================================================

-- 会话主体表：一对用户一行（pair_key 规范化为「较小 mid:较大 mid」），
-- 同时是会话内 seq 的分配锚点：发送消息时在事务内对本行 SELECT ... FOR UPDATE 后自增，
-- 使消息顺序只由数据库保证，不依赖 Redis（容量与一致性取舍见 README）。
CREATE TABLE IF NOT EXISTS `pm_conversation` (
  `conversation_id` BIGINT       NOT NULL AUTO_INCREMENT COMMENT '会话 ID（跨服务只传这个主键）',
  `pair_key`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '规范化双方键：min(mid):max(mid)，(a,b) 与 (b,a) 同值',
  `user_a`          BIGINT       NOT NULL DEFAULT 0 COMMENT 'mid 较小的一方（account 主键引用，不建外键）',
  `user_b`          BIGINT       NOT NULL DEFAULT 0 COMMENT 'mid 较大的一方',
  `state`           TINYINT      NOT NULL DEFAULT 1 COMMENT '会话状态：1 正常、2 风控冻结（禁新发送，历史仍可读；真值在 risk-control）',
  `last_seq`        BIGINT       NOT NULL DEFAULT 0 COMMENT '会话内已分配的最大序列号（seq 锚点，只前进）',
  `last_msg_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '最后一条消息 ID（pm_message.msg_id）',
  `last_msg_time`   BIGINT       NOT NULL DEFAULT 0 COMMENT '最后消息时间（Unix 秒，会话排序用的粗粒度快照）',
  `created_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `updated_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`conversation_id`),
  UNIQUE KEY `uniq_pair_key` (`pair_key`),
  KEY `idx_pair_users` (`user_a`, `user_b`),
  KEY `idx_updated_at` (`updated_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='单聊会话主体表：uniq_pair_key 使“建会话”天然幂等，无需分布式锁';

-- 会话成员表：每个（会话，用户）一行，承载已读游标、本方隐藏位与列表展示投影。
-- 已读语义：read_seq 是「读到哪」的游标而非逐条已读回执表——逐条回执行数 = 成员数 × 消息数，
-- 高频用户会放大到亿级；游标方案固定 2 行/会话，未读数由 unread_count 投影列承担。
-- unread_count / last_* 都是投影：与消息写入同事务维护，可由 pm_message 完全重建，
-- 不作为唯一事实源（容量取舍与重建路径见 README）。
CREATE TABLE IF NOT EXISTS `pm_conversation_member` (
  `id`              BIGINT       NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `conversation_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '会话 ID（pm_conversation.conversation_id）',
  `mid`             BIGINT       NOT NULL DEFAULT 0 COMMENT '成员 mid',
  `peer_mid`        BIGINT       NOT NULL DEFAULT 0 COMMENT '对方 mid（列表投影直接展示，免 JOIN）',
  `read_seq`        BIGINT       NOT NULL DEFAULT 0 COMMENT '已读游标：本方看到过的最大 seq，只前进不回退',
  `unread_count`    BIGINT       NOT NULL DEFAULT 0 COMMENT '未读数投影（发送 +1、读到清零、撤回未读 -1，可重算）',
  `last_seq`        BIGINT       NOT NULL DEFAULT 0 COMMENT '最后消息 seq 快照',
  `last_msg_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '最后消息 ID 快照',
  `last_msg_type`   TINYINT      NOT NULL DEFAULT 0 COMMENT '最后消息载体类型快照：0 无、1 文本、2 图片、3 语音、4 短视频、5 分享卡片',
  `last_preview`    VARCHAR(255) NOT NULL DEFAULT '' COMMENT '最后消息脱敏摘要（如「[图片]」）；禁止写入正文原文',
  `last_msg_time`   BIGINT       NOT NULL DEFAULT 0 COMMENT '最后消息时间（Unix 秒，会话列表游标第 1 列）',
  `hide_state`      TINYINT      NOT NULL DEFAULT 0 COMMENT '本方隐藏：0 正常、1 已隐藏（不影响对方，也不删除消息）',
  `created_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `updated_at`      BIGINT       NOT NULL DEFAULT 0 COMMENT '最近更新时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_conv_mid` (`conversation_id`, `mid`),
  KEY `idx_mid_peer` (`mid`, `peer_mid`),
  KEY `idx_mid_list` (`mid`, `hide_state`, `last_msg_time`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='会话成员表：已读游标 + 未读/展示投影，会话列表只扫本表不 JOIN 消息表';

-- 私信消息表：正文一律密文存储（AES-GCM，nonce||ciphertext），明文永不入库。
-- 隐私级别 P4；主密钥与 content_hash 的 pepper 只存在于 Secret/Vault，不入库不入仓库。
-- 留存：由 private-message.MessageRetentionDays 配置控制，到期由 PurgeExpiredMessages
--       清空 content_cipher 并置 content_purged=1，保留行以维持 seq 连续与撤回/审核审计。
-- 顺序：uniq_conv_seq 既是 seq 不重复的约束，也是「按 seq 倒序翻页」的索引，
--       禁止 offset 全扫（列表路径永远是 conversation_id + seq < cursor）。
CREATE TABLE IF NOT EXISTS `pm_message` (
  `msg_id`           BIGINT       NOT NULL AUTO_INCREMENT COMMENT '消息 ID（主键）',
  `conversation_id`  BIGINT       NOT NULL DEFAULT 0 COMMENT '会话 ID',
  `seq`              BIGINT       NOT NULL DEFAULT 0 COMMENT '会话内序列号（事务内由 pm_conversation.last_seq 分配）',
  `sender_mid`       BIGINT       NOT NULL DEFAULT 0 COMMENT '发送方 mid',
  `receiver_mid`     BIGINT       NOT NULL DEFAULT 0 COMMENT '接收方 mid',
  `msg_type`         TINYINT      NOT NULL DEFAULT 1 COMMENT '载体类型：1 文本、2 图片、3 语音、4 短视频、5 分享卡片',
  `content_cipher`   BLOB         NOT NULL COMMENT '正文密文（AES-GCM：nonce||ciphertext）；留存到期置空。禁止在任何日志打印解密结果',
  `key_version`      INT          NOT NULL DEFAULT 1 COMMENT '加密密钥版本号（主密钥轮换后旧密文仍可解密）',
  `content_hash`     CHAR(64)     NOT NULL DEFAULT '' COMMENT '明文的服务端 keyed hash（HMAC-SHA256+pepper，hex）：只用于风控查重，不可反推原文',
  `media_ref`        VARCHAR(128) NOT NULL DEFAULT '' COMMENT '媒资引用（asset 主键字符串，不建跨服务外键）',
  `preview`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '脱敏摘要，事件与日志只允许携带本列',
  `state`            TINYINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 正常、2 待审核（仅发送者可见）、3 已撤回、4 审核驳回、5 处置删除',
  `audit_task_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '送审任务 ID（moderation-orchestrator 主键引用，0 表示未送审）',
  `audit_event_id`   VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '送审事件 ID（事件幂等与追溯）',
  `client_msg_id`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '客户端幂等键（必填，重试不产生第二条消息）',
  `content_purged`   TINYINT      NOT NULL DEFAULT 0 COMMENT '正文清理标记：0 留存期内、1 已按留存策略物理清除',
  `withdraw_time`    BIGINT       NOT NULL DEFAULT 0 COMMENT '撤回时间（Unix 秒，0 表示未撤回；正文行与 pm_withdraw_log 保留）',
  `ctime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒，留存期起算点）',
  `mtime`            BIGINT       NOT NULL DEFAULT 0 COMMENT '状态变更时间（Unix 秒）',
  PRIMARY KEY (`msg_id`),
  UNIQUE KEY `uniq_conv_seq` (`conversation_id`, `seq`),
  UNIQUE KEY `uniq_sender_client_msg` (`sender_mid`, `client_msg_id`),
  KEY `idx_receiver_ctime` (`receiver_mid`, `ctime`),
  KEY `idx_purge_scan` (`content_purged`, `ctime`),
  KEY `idx_audit_task` (`audit_task_id`),
  KEY `idx_content_hash` (`content_hash`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin
  COMMENT='私信消息表：密文正文 + 会话内 seq + 客户端幂等键；明文与解密结果禁止入库、入日志、入事件';
