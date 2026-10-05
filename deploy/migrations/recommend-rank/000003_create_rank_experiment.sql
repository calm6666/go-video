-- =====================================================================
-- recommend-rank 服务 - A/B 实验变体与分桶配置表
-- =====================================================================
-- 数据库：go_video_recommend_rank（取自 services/recommend-rank/etc/recommendrank.v1.yaml 的
--   DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
-- 数据所有者：recommend-rank 服务（AGENTS.md §5）。其他服务只能通过 rpc.Rank 读写，
--   禁止直连本库；实验读数（曝光/完播/留存）属 spm，本库只登记「谁在哪个桶」的配置事实。
--
-- 表与代码对应关系（列名严格取自 services/recommend-rank/model/experiment.go 的 db tag 与 SQL 字符串）：
--   * rank_experiment  model.RankExperiment —— 一行 = 一个 (exp_key, variant_key) 变体，
--     不是一个实验：同实验的每个变体各自持有互不重叠的左闭右开桶区间 [bucket_start, bucket_end)。
--
-- 可审计语义（为什么分桶必须登记成表，而不是代码里 if 一下）：
--   1. 桶区间是登记出来的事实：UpsertExperiment 前必须先跑 ListOverlappingBuckets
--      （同 layer_key、RUNNING/PAUSED、[start,end) 相交）证明同层互斥，
--      而不是运行时靠约定；DRAFT 尚未分流故不参与判定，STOPPED 已不占桶位同理。
--   2. state 只经 UpdateState 的条件 UPDATE 推进（DRAFT→RUNNING⇄PAUSED→STOPPED，
--      model.CanTransitionExpState 把守），RowsAffected=0 表示并发下状态已被别人改掉，
--      调用方必须重读而不是当成功。
--   3. RUNNING 期间 hash_seed / bucket_count / 桶区间 / 层 / 模型与特征绑定不可热改
--      （UpdateBucketConfig 的 WHERE 自带 `state IN (DRAFT, PAUSED) AND revision = ?`）：
--      中途换盐或挪区间会让同一主体跳组，实验结论直接失效；要改必须先 PAUSED。
--      历史 rank_decision_log 仍指向旧 revision，因此「当时跑的是哪个版本的变体」可回查。
--   4. 每次语义变更 revision + 1，并留下 operator/note（rpc reason 落在这两列）。
--   5. 本表不提供任何「把某个 aid 推进/置顶/屏蔽」的列；overrides 的顶层 key 由
--      model.SupportedOverrides() 白名单把守（model.ValidateOverrideKeys 在 Insert/UpdateBucketConfig
--      写库前拒绝未登记 key，含 boost_aids、pin_aid、ad_slot 这类夹带），
--      因此「借实验手工修改推荐结果」在模型层就走不通（AGENTS.md §7）；
--      运营干预只能通过 ops-config 配置位表达，且必须落到本表的 operator/note 可审计。
--
-- 唯一键与幂等设计：
--   * uniq_variant (exp_key, variant_key)：UpsertExperiment 的幂等锚点，
--     同时天然表达「变体 key 在实验内唯一」；model.Insert 捕获 1062 转 ErrExperimentExists。
--     bucket_count 不在唯一键内：分桶空间大小由 config.Rank.BucketCount 固定（默认 1000），
--     把它放进业务主键会让调用方传的 bucket_count 造出重复行。
--   * 已知缺口：「同层桶区间不重叠」只有应用层前置校验（先查后写），
--     两个并发请求登记相交区间时数据库不会拒绝 —— MySQL 无法为区间相交建唯一约束。
--     要彻底封死需按 layer_key 串行化（GET_LOCK 或层锁行），本期未做，已在 README 记录。
--
-- 索引取自真实查询路径（见 model/experiment.go 的 SQL）：
--   * uniq_variant：FindOne(exp_key, variant_key)、UpdateState（WHERE exp_key=? AND variant_key=? AND state=?）。
--   * idx_layer_state (layer_key, state, bucket_start, bucket_end)：ListOverlappingBuckets
--     （layer_key 等值 + state IN (2,3) + 左闭右开相交判定 bucket_start < ? AND ? < bucket_end），
--     把区间包含四列都放进索引，使相交判定在索引内完成而不回表判宽。
--   * idx_running (state, exp_key, bucket_start)：ListRunning
--     （state=2 等值 + ORDER BY exp_key ASC, bucket_start ASC LIMIT ?，索引序即结果序；
--     start_at/end_at 作为残余过滤 —— RUNNING 行数与变体数同量级，不是大集合）。
--   * idx_state_end (state, end_at)：StopExpired（state IN (2,3) AND end_at>0 AND end_at<=? LIMIT ?，
--     运维巡检按索引定位过期变体并分批推进，避免一次锁住大区间）。
--   * idx_exp_key (exp_key, bucket_start)：ListByExpKey（exp_key 等值 + 按桶起点升序看分流全貌）。
--   * 不给 model_key / variant_key 单列建索引：本表规模是「实验变体数」量级（百级），
--     ModelVersionQuery 式的分页由 LIMIT 兜住；多建索引只增加变体登记时的写放大。
--
-- 回滚（本期只建表，DROP 即可完全回滚；表内是实验配置与审计信息，DROP 前必须先备份）：
--   DROP TABLE IF EXISTS `rank_experiment`;
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不触碰既有表，不锁其他表；
--   UpdateState / UpdateBucketConfig 均以唯一索引或主键等值命中，只锁单行；
--   StopExpired 使用 UPDATE ... LIMIT，要求 binlog_format=ROW
--   （statement 格式下 UPDATE...LIMIT 主从可能不一致，属已知不安全写法，故仅供巡检低频调用）。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `rank_experiment` (
  `id`                     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `exp_key`                VARCHAR(64)  NOT NULL COMMENT '实验稳定 key（如 home_feed_gate_v2）',
  `variant_key`            VARCHAR(64)  NOT NULL COMMENT '变体 key，实验内唯一；对照组固定为 model.ControlVariant = control',
  `layer_key`              VARCHAR(64)  NOT NULL DEFAULT 'default' COMMENT '互斥层：同层变体互斥分流、异层正交（空层名由 model 归一为 default，保证校验始终有作用域）',
  `hash_seed`              VARCHAR(64)  NOT NULL COMMENT '分桶哈希盐（必填，model.ErrHashSeedRequired）；RUNNING 期间不可改，换盐即整体重分桶',
  `bucket_count`           INT          NOT NULL DEFAULT 1000 COMMENT '分桶空间大小（登记后不可变，默认 config.Rank.BucketCount=1000）',
  `bucket_start`           INT          NOT NULL DEFAULT 0 COMMENT '桶区间左闭端，[0, bucket_count)',
  `bucket_end`             INT          NOT NULL COMMENT '桶区间右开端，必须 > bucket_start（model.ValidBucketRange 把守）',
  `model_key`              VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '本变体绑定的逻辑模型名（rank_model_version.model_key）',
  `model_version`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '指定模型版本；空表示沿用该 model_key 当时的 ACTIVE 版本（沿用即"不锁定"，需在 decision_log 里看到实际生效版本）',
  `feature_config_version` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '指定特征配置版本；空表示沿用模型登记的版本',
  `overrides`              VARCHAR(4096) NOT NULL DEFAULT '' COMMENT '参数覆盖 JSON：顶层 key 必须是 model.SupportedOverrides() 登记的打分/打散/频控参数（字节上限 model.MaxOverridesBytes=4096），禁止 aid 列表、置顶位与任何商业化字段（AGENTS.md §7）',
  `state`                  TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 DRAFT、2 RUNNING、3 PAUSED、4 STOPPED（model.ExpState*，与 rpc ExperimentState 同值；4 为终态）',
  `revision`               INT          NOT NULL DEFAULT 1 COMMENT '语义修订号（配置变更 +1），rank_decision_log.exp_revision 回指此值',
  `start_at`               BIGINT       NOT NULL DEFAULT 0 COMMENT '生效时间（Unix 秒）',
  `end_at`                 BIGINT       NOT NULL DEFAULT 0 COMMENT '结束时间（Unix 秒），0 表示长期实验未设定（永不被动停止）',
  `operator`               VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后操作者（审计必填；StopExpired 写 system）',
  `note`                   VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更原因（rpc reason 落库列，启停必填 model.ErrReasonRequired）',
  `ctime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_variant` (`exp_key`, `variant_key`),
  KEY `idx_layer_state` (`layer_key`, `state`, `bucket_start`, `bucket_end`),
  KEY `idx_running` (`state`, `exp_key`, `bucket_start`),
  KEY `idx_state_end` (`state`, `end_at`),
  KEY `idx_exp_key` (`exp_key`, `bucket_start`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='A/B 实验变体与分桶配置表（一行一个变体，桶区间左闭右开且同层互斥，RUNNING 语义不可热改，revision+operator 留审计线索）';
