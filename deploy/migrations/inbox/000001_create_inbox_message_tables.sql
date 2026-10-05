-- =====================================================================
-- inbox 服务 - 站内信消息与收件状态
-- =====================================================================
-- owner：inbox 服务（数据所有者，AGENTS.md §5）。库：go_video_inbox。
-- 影响：新建 3 张表（消息主体、收件明细、未读快照），不改动任何已有表；
--       不写跨服务外键，biz_id 只作为字符串业务主键保存。
-- 回滚：DROP TABLE IF EXISTS `inbox_unread_stat`,`inbox_user_message`,`inbox_message`;
--       站内信是终端展示数据，回滚前需导出未读快照，重放窗口由
--       inbox_consumer_offset（见 000002）与 idempotency_key 保证。
-- 锁风险：仅建表，无 ALTER，无锁风险；重复执行由 IF NOT EXISTS 兜底。
-- =====================================================================

-- 消息主体表：一条消息一行，收件人关系见 inbox_user_message。
CREATE TABLE IF NOT EXISTS `inbox_message` (
  `msg_id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '消息 ID（主键）',
  `category`        TINYINT      NOT NULL DEFAULT 1 COMMENT '分类：1 系统、2 互动、3 内容、4 直播',
  `msg_type`        TINYINT      NOT NULL DEFAULT 1 COMMENT '载体类型：1 纯文本、2 文本+跳转、3 结构化富文本',
  `title`           VARCHAR(128) NOT NULL DEFAULT '' COMMENT '标题',
  `content`         TEXT         NOT NULL COMMENT '正文（纯文本，禁止写入手机号/Token 等敏感数据）',
  `sender_mid`      BIGINT       NOT NULL DEFAULT 0 COMMENT '发送方 mid，0 表示系统账号',
  `biz_type`        VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '业务类型：submission/comment/danmaku/live_room/moderation/event',
  `biz_id`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '业务主键（字符串，不建跨服务外键）',
  `extra`           TEXT         NOT NULL COMMENT '扩展信息 JSON 文本（客户端渲染所需，缺省为空串）',
  `state`           TINYINT      NOT NULL DEFAULT 0 COMMENT '消息主体状态：0 正常、1 已撤回（对所有收件人不再展示）',
  `idempotency_key` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '幂等键：调用方传入或事件派生（evt:<event_id>）',
  `operator`        BIGINT       NOT NULL DEFAULT 0 COMMENT '运营管理员 mid（审计用，0 表示非人工触发）',
  `ctime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`msg_id`),
  UNIQUE KEY `uniq_idempotency_key` (`idempotency_key`),
  KEY `idx_biz` (`biz_type`, `biz_id`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='站内信消息主体表：同一消息可投递给多个收件人';

-- 收件明细表：每个收件人一行，已读/删除都是用户侧状态。
-- category 是从消息主体冗余过来的不可变快照，用于按分类计数避免 JOIN。
CREATE TABLE IF NOT EXISTS `inbox_user_message` (
  `id`         BIGINT  NOT NULL AUTO_INCREMENT COMMENT '主键 ID',
  `mid`        BIGINT  NOT NULL DEFAULT 0 COMMENT '收件人 mid',
  `msg_id`     BIGINT  NOT NULL DEFAULT 0 COMMENT '消息 ID（inbox_message.msg_id）',
  `category`   TINYINT NOT NULL DEFAULT 1 COMMENT '分类快照：1 系统、2 互动、3 内容、4 直播',
  `read_state` TINYINT NOT NULL DEFAULT 1 COMMENT '读取状态：1 未读、2 已读（与 rpc.ReadState 对齐）',
  `del_state`  TINYINT NOT NULL DEFAULT 0 COMMENT '用户侧删除：0 正常、1 已删除（不影响其它收件人）',
  `ctime`      BIGINT  NOT NULL DEFAULT 0 COMMENT '投递时间（Unix 秒，列表游标）',
  `mtime`      BIGINT  NOT NULL DEFAULT 0 COMMENT '状态变更时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_mid_msg` (`mid`, `msg_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_mid_read_ctime` (`mid`, `read_state`, `ctime`),
  KEY `idx_mid_category_read` (`mid`, `category`, `read_state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='站内信收件明细表：唯一键 (mid,msg_id) 保证重复投递不产生第二行';

-- 未读数快照表：Redis 计数缺失时的回落层，真值始终可由 inbox_user_message 重算。
CREATE TABLE IF NOT EXISTS `inbox_unread_stat` (
  `mid`      BIGINT  NOT NULL COMMENT '用户 mid（联合主键第 1 列）',
  `category` TINYINT NOT NULL COMMENT '分类：1 系统、2 互动、3 内容、4 直播',
  `unread`   BIGINT  NOT NULL DEFAULT 0 COMMENT '未读数快照（由明细表重算写入，允许为 0）',
  `mtime`    BIGINT  NOT NULL DEFAULT 0 COMMENT '快照更新时间（Unix 秒）',
  PRIMARY KEY (`mid`, `category`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='站内信未读数快照表：RecomputeUnread 可从明细表完全重建';
