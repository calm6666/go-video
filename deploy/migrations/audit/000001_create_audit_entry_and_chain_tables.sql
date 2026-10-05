-- =====================================================================
-- audit 服务 - 审计存证主表与哈希链链头表
-- =====================================================================
-- 数据库：go_video_audit（见 services/audit/etc/audit.v1.yaml 的 DataSource）。
-- 数据所有者：audit 服务。本服务是「不可抵赖存证」的唯一事实源。
--
-- 与其他服务的边界（结论写在 services/audit/README.md，冲突已按证据裁决）：
--   * operation.op_audit_index 保留为「后台控制台的操作性索引」（谁在哪个页面点了什么），
--     不迁移、不删除：它没有哈希链、没有前后摘要、也没有合规保留期语义，
--     替代它需要迁移历史数据，超出本期边界。
--     真正需要长期不可抵赖的存证由本表承接，两边可通过
--     op_audit_index.request_id ↔ audit_entry.request_id 对齐。
--   * moderation-worker / risk-control / catalog 等领域服务继续持有自己域的业务数据
--     （审核工单、风控判定、稿件状态）。本表只记录「谁对什么做了什么、结果如何」，
--     不复制任何业务正文，因此不存在双写主数据。
--   * 跨服务只存主键引用：target_type 是类型标识（如 ops_config:release），
--     target_id 是对方主键的字符串形式，本服务永远不 JOIN 其它库的表。
--
-- 表与代码对应关系（列名严格取自 services/audit/model/*.go 的 db tag 与 SQL 字符串）：
--   * audit_entry       model/auditentry.go AuditEntry    —— 追加式：接口无 UPDATE/DELETE，
--                              唯一例外 MarkArchived 只写不参与哈希的 archived_at 列。
--   * audit_chain_head  model/auditchainhead.go ChainHead —— 每条链的尾部状态，
--                              追加时在事务内 SELECT ... FOR UPDATE 锁定，Advance 带
--                              WHERE seq = ? 乐观锁，防止并发分叉。
--
-- 唯一键与幂等设计：
--   1. uniq_event_id(event_id)：写接口的幂等键。gRPC 重试、消息重投、调用方自己重发
--      都只会命中同一行，repository 先 FindByEventID 回查、命中即返回原条目（不新增）。
--      这是「至少一次」投递下审计不重复的唯一手段，因此必须是唯一键而不是普通索引。
--   2. uniq_chain_seq(chain_key, seq)：链内序号唯一。并发追加若算出相同 seq，
--      第二条会撞唯一键 → repository 转成 ErrChainConflict 重试。
--      没有它，链会静默分叉，整表自证能力作废，所以这是本表最关键的一条约束。
--   3. audit_chain_head 以 chain_key 为主键，Ensure 用 ON DUPLICATE KEY UPDATE 做幂等建链。
--
-- 索引全部取自真实查询路径（model/auditentry.go 的 build / ListByChainRange / MaxSeq）：
--   * idx_occurred(occurred_at, entry_id)：ListAuditEntries 强制时间范围 + 固定
--     ORDER BY occurred_at DESC, entry_id DESC。排序键与索引键一致才不会 filesort。
--   * idx_actor(actor_type, actor_id, occurred_at)：「某管理员/某用户做过什么」。
--   * idx_action(action, occurred_at)：按动作标识排查（如 admin.login 失败集中爆发）。
--   * idx_domain_time(action_domain, occurred_at)：按域巡检与归档作业定位待归档区间。
--   * idx_target(target_type, target_id, occurred_at)：「某个对象被谁改过」——
--     合规调查最常问的反向视角，也是 ops-config 回查发布存证的入口。
--   * idx_trace(trace_id)：一次请求跨服务串起所有条目。
--   * idx_archived(archived_at, occurred_at)：只扫热表（archived_at = 0）的归档/清理作业，
--     避免反复全表判断。
--
-- 为什么不做 MySQL RANGE 分区（本期决定，理由要留档）：
--   分区表要求分区键进入每一个唯一键，本表有两个唯一键（event_id 与 (chain_key, seq)），
--   把 occurred_at 塞进它们都会破坏 event_id 的全局幂等语义。
--   因此容量治理走「归档批次 + 索引」路线（见 000003 与本服务 README），
--   等单表确实顶不住时再按 entry_id 做水平分表，而不是现在牺牲正确性。
--
-- 敏感信息约束（AGENTS.md §7）：
--   * 不存在任何明文来源列：只有 ip_hash / device_hash 两个 CHAR(32) 加盐短哈希列，
--     写入路径不接受原文（model.ShortHash）。
--   * before_digest / after_digest 走格式白名单（model.ValidDigest）：只允许空串、
--     64 位十六进制、或 `字段名=16 位十六进制` 的分号列表。
--     手机号/邮箱/证件号在格式上无法落入白名单，因此明文 PII 进不了这两列。
--   * reason / actor_name 是自由文本，写入前经 model.LooksLikePII 兜底判定，命中即整条拒写。
--
-- owner：平台治理（audit 服务）；影响范围：新增 2 张表，不改任何既有表。
-- 回滚：
--   DROP TABLE IF EXISTS `audit_chain_head`;
--   DROP TABLE IF EXISTS `audit_entry`;
--   警告：audit_entry 是追加式存证，DROP 等于销毁证据链，只有在全新环境验证时才这么做；
--   已入库环境必须先完成 000003 的归档批次（state=purged）再考虑清理。
-- 锁风险：全部为新建空表，不锁既有表。运行期锁集中在 audit_chain_head 的单行
--   （按 chain_key 分散，粒度是「动作域 × UTC 日」），事务只覆盖「锁链头 → 插条目 → 推链头」
--   三条语句，持锁时间是毫秒级；不要在本事务里做任何外部 RPC 调用。
-- =====================================================================

-- 审计存证主表：追加式，任何修正都靠再写一条补偿条目（action 形如 <对象>.revoke）。
CREATE TABLE IF NOT EXISTS `audit_entry` (
  `entry_id`       BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键；由 (chain_key, seq) 唯一决定，因此不参与哈希',
  `event_id`       VARCHAR(128) NOT NULL COMMENT '全局幂等键：<服务>:<request_id>:<动作>，重复提交返回原条目不新增',
  `schema_version` INT          NOT NULL DEFAULT 1 COMMENT '条目契约版本，参与哈希第 1 位；当前恒为 1',
  `chain_key`      VARCHAR(64)  NOT NULL COMMENT '所属哈希链："<action_domain>/<UTC 自然日>"，由 model.ChainKey 生成',
  `seq`            BIGINT       NOT NULL COMMENT '链内序号，从 1 连续递增；出现空洞即视为链被截断',
  `actor_type`     TINYINT      NOT NULL COMMENT '发起者类型：1 管理员、2 用户、3 系统、4 未知（0 禁止入库）',
  `actor_id`       BIGINT       NOT NULL DEFAULT 0 COMMENT '发起者 ID（admin_id / mid）；类型未知时为 0',
  `actor_name`     VARCHAR(128) NOT NULL DEFAULT '' COMMENT '发起者展示名快照（改名不追改，故不参与哈希）',
  `action`         VARCHAR(64)  NOT NULL COMMENT '动作标识：<对象>.<动作>，如 ops_config.publish',
  `action_domain`  VARCHAR(32)  NOT NULL COMMENT '动作域：决定所属链与保留策略（audit_retention_policy.action_domain）',
  `target_type`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '目标类型标识（跨服务只存类型与主键，不存正文）',
  `target_id`      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '目标主键（字符串，兼容不同服务的主键形态）',
  `result`         TINYINT      NOT NULL DEFAULT 1 COMMENT '结果：1 ok、2 denied、3 error（0 禁止入库；拒绝路径必须留痕）',
  `before_digest`  VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更前摘要，白名单格式（空串/64hex/字段=16hex 分号列表）；新增类动作留空',
  `after_digest`   VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更后摘要，白名单格式；删除类动作留空',
  `reason`         VARCHAR(512) NOT NULL DEFAULT '' COMMENT '业务原因（自由文本，入参按 Write.MaxReasonLen 限长并经 LooksLikePII 过滤）',
  `source_app`     TINYINT      NOT NULL DEFAULT 0 COMMENT '来源端：1 android、2 ios、3 harmony、4 desktop、5 后台 Web、6 内部 RPC、7 cron；0 禁止入库',
  `ip_hash`        CHAR(32)     NOT NULL DEFAULT '' COMMENT '来源 IP 的加盐短哈希（sha256hex 前 32 位）；本表不存在明文 IP 列',
  `device_hash`    CHAR(32)     NOT NULL DEFAULT '' COMMENT '设备标识的加盐短哈希；未知来源留空串，不用假哈希占位',
  `trace_id`       VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路 ID，用于跨服务串起一次请求',
  `request_id`     VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '调用方幂等键（一次请求可产生多条条目，故不建唯一键）',
  `caller_service` VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '写入方服务名：回答「谁声称做了这件事」，与 actor_id 互补',
  `occurred_at`    BIGINT       NOT NULL COMMENT '业务发生时间（Unix 秒）；决定所属链与查询排序',
  `prev_hash`      CHAR(64)     NOT NULL COMMENT '同链上一条 entry_hash；链头为 sha256hex("go-video/audit/v1/genesis/" + chain_key)',
  `entry_hash`     CHAR(64)     NOT NULL COMMENT '本条目摘要 sha256hex(19 个字段以 0x1F 连接)，算法见 model/hashchain.go',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '入库时间（Unix 秒）；不参与哈希，避免重放时无法复算',
  `archived_at`    BIGINT       NOT NULL DEFAULT 0 COMMENT '归档完成时间，0 表示仍在热表；不参与哈希（其证据由 audit_archive_batch.manifest_hash 覆盖）',
  PRIMARY KEY (`entry_id`),
  UNIQUE KEY `uniq_event_id` (`event_id`),
  UNIQUE KEY `uniq_chain_seq` (`chain_key`, `seq`),
  KEY `idx_occurred` (`occurred_at`, `entry_id`),
  KEY `idx_actor` (`actor_type`, `actor_id`, `occurred_at`),
  KEY `idx_action` (`action`, `occurred_at`),
  KEY `idx_domain_time` (`action_domain`, `occurred_at`),
  KEY `idx_target` (`target_type`, `target_id`, `occurred_at`),
  KEY `idx_trace` (`trace_id`),
  KEY `idx_archived` (`archived_at`, `occurred_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='审计存证主表（append-only + 哈希链自证；无明文 PII，只存加盐短哈希与白名单摘要）';

-- 哈希链链头表：一条链一行。追加必须在事务内先锁本行，否则并发会算出相同 prev_hash/seq。
CREATE TABLE IF NOT EXISTS `audit_chain_head` (
  `chain_key`     VARCHAR(64) NOT NULL COMMENT '链标识："<action_domain>/<UTC 日>"，主键（锁粒度即此列）',
  `last_entry_id` BIGINT      NOT NULL DEFAULT 0 COMMENT '链尾条目的 entry_id，0 表示空链',
  `last_hash`     CHAR(64)    NOT NULL DEFAULT '' COMMENT '链尾条目的 entry_hash，即新条目的 prev_hash',
  `seq`           BIGINT      NOT NULL DEFAULT 0 COMMENT '已分配的链内序号；Advance 以 WHERE seq = ? 做乐观锁',
  `entry_count`   BIGINT      NOT NULL DEFAULT 0 COMMENT '链上条目数；与 seq 不等即说明发生过截断或人为改写',
  `first_at`      BIGINT      NOT NULL DEFAULT 0 COMMENT '链建立时间（Unix 秒）',
  `mtime`         BIGINT      NOT NULL DEFAULT 0 COMMENT '最近推进时间（Unix 秒），用于「链是否还在长」的巡检',
  PRIMARY KEY (`chain_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='审计哈希链链头表（并发追加的串行化点；按动作域 + UTC 日分链以分散行锁）';
