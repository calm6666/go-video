-- =====================================================================
-- recommend-rank 服务 - 排序决策摘要表（可审计落地表）
-- =====================================================================
-- 数据库：go_video_recommend_rank（取自 services/recommend-rank/etc/recommendrank.v1.yaml 的
--   DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
-- 数据所有者：recommend-rank 服务（AGENTS.md §5）。其他服务只能通过 rpc.Rank 读写，禁止直连本库；
--   本表只存 aid/mid 等主键引用与摘要哈希，稿件标题、用户画像等正文本一律不入库（AGENTS.md §5/§7）。
--
-- 表与代码对应关系（列名严格取自 services/recommend-rank/model/decisionlog.go 的 db tag 与 SQL 字符串）：
--   * rank_decision_log  model.RankDecisionLog —— 一次排序一行，回答四个问题：
--     1) 用的哪套配置：model_key / model_version / feature_config_version /
--        exp_key / variant_key / bucket_no / exp_revision；
--     2) 候选从哪来：input_count + source_summary（"1:12,2:8" 来源分布）+
--        snapshot_id / pool_version（回指 recommend-recall 的召回快照与池版本）；
--     3) 结果是什么：returned_count、input_digest、result_digest、top_aids；
--        input_digest 与 result_digest 同时存在，才能证明「出参是入参的子集且没有编造 aid」；
--     4) 是否降级：degraded / degrade_reason / fallback_strategy / scored_count
--        + 五个过滤计数（safety_filtered、frequency_filtered、dedup_filtered、
--         diversified_moved、truncated），被丢弃的候选必须留下计数，不允许静默吞。
--
-- 唯一键与幂等设计（三个标识列各有分工，避免在 hottest 表上堆叠可互换的唯一键）：
--   * uniq_request_id (request_id)：幂等/重放的唯一锚点。rpc 契约里
--     RequestContext.request_id 是「同 key 重放返回同一 decision」的承诺方，
--     model.Insert 捕获 1062 转 ErrDecisionExists，logic 据此读旧行回放而不是二次打分。
--   * uniq_decision_id (decision_id)：对外审计 ID（model.NewDecisionID 生成的 32 位 hex，
--     随机而非自增，避免把流量规模泄露给客户端日志）。
--   * idempotency_key：调用方可选的提示位，索引但**不唯一** ——
--     一次刷新可能对应多个 request_id，唯一化会把「重试」变成写入失败；
--     人工核对时按 FindByIdempotencyKey 取最近一条（ORDER BY id DESC LIMIT 1）。
--
-- 索引取自真实查询路径（见 model/decisionlog.go 的 SQL；本表是写热表，
-- 8 个二级索引即写放大的上界，新增索引必须先说明能删掉哪个既有索引）：
--   * uniq_request_id：FindByRequestID（幂等回放，在线链路上唯一会跑的点查）。
--   * uniq_decision_id：FindByDecisionID（GetRankDecision）。
--   * idx_idempotency (idempotency_key)：FindByIdempotencyKey；InnoDB 二级索引隐含主键尾列，
--     故 WHERE idempotency_key=? ORDER BY id DESC LIMIT 1 即索引序。
--   * idx_exp_variant (exp_key, variant_key)：ListRankDecisions 按实验/变体下钻
--     （等值后再按 id 倒序分页，隐含主键尾列使 ORDER BY id DESC 免 filesort）。
--   * idx_model_version (model_key, model_version)：ListRankDecisions 按模型版本下钻
--     —— 「新版本上线后结果分布是否异常」的第一现场查询。
--   * idx_scene_ctime (scene, ctime)：ListRankDecisions 的 scene + 时间窗组合。
--   * idx_degraded_ctime (degraded, ctime)：CountDegradedSince（健康探针/告警）
--     与 ListRankDecisions 的 only_degraded + 时间窗。
--   * idx_ctime (ctime)：只带时间窗、不带其他过滤的运营查询。
--     注意：归档扫描 SelectExpiredBefore（WHERE ctime < ? ORDER BY id ASC LIMIT ?）
--     走的是主键顺序扫描而非本索引 —— 超期行永远是 id 最小的一段，主键序即可提前停止。
--   * 不给 snapshot_id / pool_version / trace_id 建索引：本期没有任何查询路径按它们检索
--     （trace_id 的定位靠日志系统），无用索引只会拖累在线写入。
--
-- 保留期与归档策略（本表增长最快，绝不允许无限增长）：
--   * 每次在线排序写一行，日增量 = 推荐请求量，量级远大于其他表；
--   * 保留期由 config.Rank.DecisionRetentionDays 决定（示例配置 14 天），
--     在线只保留「够排障」的明细；
--   * 归档两步式：先 SelectExpiredBefore(cutoff, ArchiveBatchSize) 挑出主键并导出到对象存储
--     （导出任务归 services/cron 规划），确认写成功后再由 DeleteExpiredBefore(ids) 按主键批删；
--     严禁 TRUNCATE、严禁单个大事务清空、严禁未归档先删；
--   * 需要长期留存的不是明细而是聚合结果（曝光/完播/留存等读数归 spm），
--     因此本表按「短期排障明细」定位，不作为长期分析仓库；
--   * 本期契约没有 prune RPC（见 README「已知缺口」），清理只能由运维巡检/cron 触发；
--   * 不做按 ctime 的分区表：MySQL 要求分区键进入所有唯一索引，
--     那会把 uniq_request_id 降级成「分区内唯一」，破坏幂等承诺；
--     删旧数据靠主键序分批即可，不需要分区裁剪。
--
-- 回滚（本期只建表；DROP 会一次性丢失全部排序审计证据，必须先完成归档备份）：
--   DROP TABLE IF EXISTS `rank_decision_log`;
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不触碰既有表，不锁其他表；
--   在线写入为单行 INSERT，冲突由唯一键即时返回；
--   批删按主键 IN(...) 定点删除，不产生范围锁；但 DELETE ... 与归档扫描必须低峰分批执行，
--   否则大事务会造成主从延迟与 undo 膨胀（这也是模型侧坚持 limit 参数的原因）。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `rank_decision_log` (
  `id`                     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键（时间序，供分批清理游标与 id DESC 分页）',
  `decision_id`            CHAR(64)     NOT NULL COMMENT '对外审计 ID（model.NewDecisionID：32 位 hex 随机），GetRankDecision 的查询键',
  `request_id`             VARCHAR(64)  NOT NULL COMMENT '幂等/重放锚点（唯一，同 request_id 只有一行；重复写入转 model.ErrDecisionExists）',
  `idempotency_key`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '调用方给的幂等提示（可空，索引但不唯一：一次刷新可对应多个 request_id）',
  `trace_id`               VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '调用方透传 trace_id（与日志系统对账用，不建索引）',
  `snapshot_id`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'recommend-recall 召回快照 ID（审计回指，跨服务只传主键）',
  `pool_version`           BIGINT       NOT NULL DEFAULT 0 COMMENT '召回池版本（审计回指，0 表示未透传）',
  `mid`                    BIGINT       NOT NULL DEFAULT 0 COMMENT '登录用户 ID，0 表示游客',
  `subject_type`           TINYINT      NOT NULL DEFAULT 0 COMMENT '分桶主体类型：0 未参与分桶、1 mid、2 设备摘要（model.Subject*）',
  `subject_id`             VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '主体标识：mid 十进制串或设备 sha256 摘要（明文设备号由 ValidateSubjectID 拒绝写入）',
  `scene`                  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '场景稳定 key（home.feed / play.related ...）',
  `platform`               TINYINT      NOT NULL DEFAULT 0 COMMENT '客户端平台：0 未指定、1 Android、2 iOS、3 Harmony、4 Desktop（model.Platform*，与 rpc Platform 同值）',
  `app_version`            VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '客户端版本号',
  `region`                 VARCHAR(16)  NOT NULL DEFAULT '' COMMENT '地区代码',
  `exp_key`                VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '命中的实验 key（未命中为空串）',
  `variant_key`            VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '命中的变体 key（未命中为空/控制组，与 rank_experiment.variant_key 对齐）',
  `bucket_no`              INT          NOT NULL DEFAULT 0 COMMENT '命中的桶号（rank_experiment_assignment.bucket_no 口径，固定 1000 分位空间）',
  `exp_revision`           INT          NOT NULL DEFAULT 0 COMMENT '命中变体当时的 revision（配置事后被改也能还原当时语义）',
  `model_key`              VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '本次实际使用的逻辑模型名',
  `model_version`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '本次实际生效的模型版本（实验沿用 ACTIVE 时也记实际值，不记空）',
  `feature_config_version` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '本次实际生效的特征配置版本',
  `input_count`            INT          NOT NULL DEFAULT 0 COMMENT '入参候选条数（上限 config.Rank.MaxCandidates）',
  `returned_count`         INT          NOT NULL DEFAULT 0 COMMENT '出参条数（上限 config.Rank.MaxReturn）',
  `scored_count`           INT          NOT NULL DEFAULT 0 COMMENT '真正被模型打分的条数（降级时小于出参条数）',
  `source_summary`         VARCHAR(255) NOT NULL DEFAULT '' COMMENT '候选来源分布 csv（"1:12,2:8"，来源 key 升序），证明结果由哪几路候选产生',
  `input_digest`           CHAR(64)     NOT NULL DEFAULT '' COMMENT 'sha256(入参有序 aid, 分隔符 "|")：与 result_digest 一起证明出参是入参子集且无编造',
  `result_digest`          CHAR(64)     NOT NULL DEFAULT '' COMMENT 'sha256(出参有序 aid, 分隔符 "|")，rpc RankCandidatesReply.result_digest 的落库值，回放比对用',
  `top_aids`               VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '前 N 个出参 aid 的 csv（N = config.Rank.MaxDigestAids，默认 20），人工排障直接可读',
  `degraded`               TINYINT      NOT NULL DEFAULT 0 COMMENT '0 正常、1 降级（TINYINT 而非 BOOLEAN，配合 idx_degraded_ctime 做告警统计）',
  `degrade_reason`         VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '降级原因受控 key（model.DegradeReason*，与 rpc RankDegradeReason 同名；未降级为空）',
  `fallback_strategy`      VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '兜底策略受控 key：recall_order / previous_model / safety_only（model.Fallback*）',
  `safety_filtered`        INT          NOT NULL DEFAULT 0 COMMENT '因内容安全/审核不可见被剔除的条数（丢弃必须留痕）',
  `frequency_filtered`     INT          NOT NULL DEFAULT 0 COMMENT '因频控被剔除的条数',
  `dedup_filtered`         INT          NOT NULL DEFAULT 0 COMMENT '入参重复 aid 去重条数',
  `diversified_moved`      INT          NOT NULL DEFAULT 0 COMMENT '打散导致位置移动的条数（不减少条数）',
  `truncated`              INT          NOT NULL DEFAULT 0 COMMENT '超出 limit 被截断的条数',
  `cost_ms`                INT          NOT NULL DEFAULT 0 COMMENT '本次排序耗时（毫秒，用于对照 config.Rank.ScoreBudgetMs）',
  `degrade_detail`         VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '排障文本（禁止包含用户敏感信息、密钥或下游响应原文）',
  `ctime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒），保留期与归档按此列计算',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_request_id` (`request_id`),
  UNIQUE KEY `uniq_decision_id` (`decision_id`),
  KEY `idx_idempotency` (`idempotency_key`),
  KEY `idx_exp_variant` (`exp_key`, `variant_key`),
  KEY `idx_model_version` (`model_key`, `model_version`),
  KEY `idx_scene_ctime` (`scene`, `ctime`),
  KEY `idx_degraded_ctime` (`degraded`, `ctime`),
  KEY `idx_ctime` (`ctime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='排序决策摘要表（配置/来源/结果摘要/降级四类证据齐备，按 DecisionRetentionDays 归档后分批删除，不做长期分析仓库）';
