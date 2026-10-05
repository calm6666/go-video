-- =====================================================================
-- recommend-recall 服务 - 在线召回请求审计日志表（事实）
-- =====================================================================
-- 用途：建立 recall_request_log，为每次 RecallCandidates 落一行"当时读了哪些版本、
--   每路给了多少条、有没有降级、降级原因是什么"。
--   这是回答"这个用户当时为什么看到这批候选"的唯一依据，也是排序服务回放请求的锚点。
-- 数据所有者：recommend-recall 服务（AGENTS.md §5）。
--
-- 数据库：go_video_recommend_recall。
--
-- 事实定位：事实（审计证据），不是投影。
--   一次在线召回读到的是哪些版本，随时间不可再生（池版本随时被 cron 重算并切换），
--   因此本表在保留期内只允许 INSERT 与按 ctime 的 DELETE（DeleteBefore），
--   不提供任何 UPDATE 路径；model/requestlog.go 里也没有 Update 方法。
--
-- 唯一键与幂等设计：
--   1. uniq_request_id：request_id 是一次召回请求的幂等/审计键，
--      同一 request_id 重试只会留下首条记录（Insert 捕获重复键返回 ErrRequestLogExists，
--      调用方改为回放旧结果，而不是落第二条互相矛盾的统计）。
--   2. uniq_snapshot_id：snapshot_id 是本次候选快照 ID，排序服务在写回排序结果时携带它，
--      两列都有唯一索引，因此 GetRecallRequestLog 可以按任一 ID 直接命中（LIMIT 1 只是防御性下界）。
--
-- 索引取自真实查询路径（对应 model/requestlog.go）：
--   · find(request_id|snapshot_id)：-> 上面两个唯一键；
--   · List / Count：条件由 requestLogWhere 统一生成（List 与 Count 共用，保证分页口径一致），
--     仅 mid / scene / ctime 区间三种组合，排序固定 ORDER BY ctime DESC, id DESC：
--     - idx_mid_ctime (mid, ctime)：InnoDB 二级索引隐含末尾补主键 id，
--       实际顺序是 (mid, ctime, id)，与 "WHERE mid=? ORDER BY ctime DESC, id DESC" 完全反向一致，
--       因此按用户查时间线是索引反向扫描 + 提前终止，不产生 filesort；
--     - idx_scene_ctime (scene, ctime)：按场景（home.feed / play.related ...）聚合排障，同理；
--     - idx_ctime (ctime)：只给时间窗口的全局扫描与保留期清理（DeleteBefore WHERE ctime < ? LIMIT ?）。
--   深翻页由代码层硬拦（MaxRequestLogOffset=10000 之外返回 ErrPageTooDeep），
--   因此不为"任意深 OFFSET"优化，超深需求请改用时间窗口收窄条件。
--
-- 敏感信息约束（AGENTS.md §7）：
--   本表不存 IP、明文设备号、手机号、UA 原文；只存 mid、platform、app_version、region、
--   scene 与 per_source（每路统计 JSON，内容是召回路与条数）。
--   rpc.RequestContext.device_id_hash 不落本表（当前没有任何按设备聚合的查询路径），
--   一旦将来要存，必须是 sha256 摘要列（model.ErrRawDeviceID 负责拒绝明文）。
--   另：本表不得出现广告位/投放/分成等商业化字段（AGENTS.md §7 禁区）。
--
-- 容量：与 RecallCandidates QPS 同量级，是全库增长最快的表。
--   config.Recall.RequestLogRetentionSeconds（默认 604800 秒 = 7 天）给出保留期，
--   清理由 services/cron 调 RequestLog.DeleteBefore 分批执行（maxRows 上限保护主从延迟）。
--
-- 回滚：
--   DROP TABLE IF EXISTS `recall_request_log`;
--   警告：本表是事实，删除后历史召回不可回放（不影响在线出数，但会失去排障能力），
--   回滚前必须确认审计保留期要求已解除。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行；
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `recall_request_log` (
  `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `request_id`        VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '召回请求 ID（唯一，幂等与审计键；空由服务端生成 ULID 并回显）',
  `snapshot_id`       VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '召回快照 ID（唯一，排序服务回填以便完整回放本次候选集）',
  `mid`               BIGINT          NOT NULL DEFAULT 0 COMMENT '用户 ID，0 表示游客/未登录（游客只走冷启动与热门路）',
  `scene`             VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '场景稳定 key（如 home.feed、play.related）；只作日志与配额维度，不驱动任何 UI 行为',
  `platform`          SMALLINT        NOT NULL DEFAULT 0 COMMENT '客户端平台，取值同 rpc.Platform：1 Android、2 iOS、3 HarmonyOS、4 桌面端（不支持小程序）',
  `app_version`       VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '客户端版本号（回放时区分"当时那版客户端"）',
  `region`            VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '地区代码（分区偏好与版权可见性维度）',
  `requested_sources` VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '请求的召回路，csv 升序去重（model.SourceListString 保证同集合恒定同串，可直接分组统计）',
  `per_source`        TEXT            NOT NULL COMMENT '逐路统计 JSON 数组：每路计划条数/实际返回/读到的版本与批次/是否降级/错误 key（排障展示用）',
  `candidate_count`   INT             NOT NULL DEFAULT 0 COMMENT '去重合并后的候选条数',
  `returned_count`    INT             NOT NULL DEFAULT 0 COMMENT '实际下发条数（裁到 limit 之后）',
  `degraded`          TINYINT         NOT NULL DEFAULT 0 COMMENT '0 正常出数、1 发生降级（降级必须可统计，不允许静默降级）',
  `degrade_reason`    VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '主降级原因的稳定 key（pool_not_ready/feature_unavailable/downstream_timeout/store_unavailable/budget_exhausted/cold_start/all_sources_empty），受 model.ValidDegradeReason 白名单约束',
  `dropped_sources`   VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '被丢弃/未出数的召回路 csv（降级细节，配合 degrade_reason 定位是哪一路掉的）',
  `versions_digest`   CHAR(64)        NOT NULL DEFAULT '' COMMENT 'sha256(读取到的 (source,version) 序列)：回放时先比摘要即可判断"是不是同一批池快照"',
  `cost_ms`           INT             NOT NULL DEFAULT 0 COMMENT '本次召回耗时（毫秒），预算裁剪（BudgetMillis）的排障依据',
  `trace_id`          VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '调用方透传的链路追踪 ID',
  `ctime`             BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）；保留期与时间窗口过滤按此列',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  UNIQUE KEY `uniq_snapshot_id` (`snapshot_id`),
  KEY `idx_mid_ctime` (`mid`, `ctime`),
  KEY `idx_scene_ctime` (`scene`, `ctime`),
  KEY `idx_ctime` (`ctime`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='在线召回请求审计日志表（一次召回读了哪些版本的事实记录，保留期内只插不改，按 ctime 分批清理）';
