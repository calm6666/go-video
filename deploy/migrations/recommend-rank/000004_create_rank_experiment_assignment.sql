-- =====================================================================
-- recommend-rank 服务 - 实验分桶归属事实表
-- =====================================================================
-- 数据库：go_video_recommend_rank（取自 services/recommend-rank/etc/recommendrank.v1.yaml 的
--   DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
-- 数据所有者：recommend-rank 服务（AGENTS.md §5）。其他服务只能通过 rpc.Rank 读写，禁止直连本库。
--
-- 表与代码对应关系（列名严格取自 services/recommend-rank/model/experimentassignment.go 的 db tag 与 SQL 字符串）：
--   * rank_experiment_assignment  model.RankExperimentAssignment
--     —— 一行 = 一个主体在一个实验（某盐）下的首次分桶事实。
--
-- 为什么必须落库，而不是每次用 BucketOf 现算：
--   1. rpc GetExperimentAssignmentReply.newly_assigned 要区分「首次分桶」与「sticky 复用」，
--      纯函数取模虽然稳定，但答不出「这个用户是什么时候进组的」；
--   2. 实验读数需要按组去重人数，且要能证明同层多变体下桶号同源，
--      这要求有一份「主体 → 桶号 → 变体」的事实表，而不是每次重算的推导结果；
--   3. variant_key/revision 落库即冻结：即使运营随后改了变体的桶区间，
--      历史归属仍可回查（revision 回指 rank_experiment.revision）。
--
-- 幂等与 sticky 语义：
--   * uniq_subject (exp_key, subject_type, subject_id, hash_seed) 是唯一的幂等锚点。
--     model.Insert 用 INSERT IGNORE + RowsAffected==0 表达「已存在」，
--     既没有先查后插的竞态窗口，也不会覆盖任何既有字段（分桶事实不可改写，故不用
--     ON DUPLICATE KEY UPDATE —— 那等于允许后到的请求改前一条的桶号）。
--   * hash_seed 进入唯一键：换盐后重分桶会写入新行，旧行保留，
--     「换盐前后各组人数变化」因此可查、可审计。
--   * bucket_count 不在唯一键内：落库桶号永远归一到 config.Rank.BucketCount（默认 1000）空间，
--     调用方另传的 bucket_count 只用于响应回显（model.ScaleBucket 线性换算），
--     否则同一主体会因客户端传参不同产生多行，sticky 语义当场失效。
--
-- 隐私约束（AGENTS.md §7）：subject_id 只存 mid 十进制串或设备 sha256 摘要（64 hex），
--   明文设备号由 model.ValidateSubjectID 在写入前拒绝（ErrRawDeviceID）；
--   本表因此不构成可反查的设备档案。
--
-- 索引取自真实查询路径（见 model/experimentassignment.go 的 SQL）：
--   * uniq_subject：FindOne(exp_key, subject_type, subject_id, hash_seed) 四列等值命中；
--     INSERT IGNORE 的冲突判定同样依赖它。
--   * idx_variant (exp_key, variant_key)：ListByVariant
--     （exp_key [+ variant_key] 等值 + ORDER BY id ASC LIMIT/OFFSET，
--     InnoDB 二级索引隐含主键尾列，故 id 升序即索引序，不产生 filesort）。
--   * idx_seed_variant (exp_key, hash_seed, variant_key)：CountByVariant 带 hash_seed 时
--     （换盐前后分组人数对比）；不带 hash_seed 时走 idx_variant 做分组，
--     两个索引都是覆盖式读，不必回表判宽。
--   * idx_assigned_at (assigned_at)：DeleteOlderThan 按保留期定位，避免全表扫描。
--
-- 保留期与容量：
--   * 本表规模 = 参与实验的主体数 × 实验数，是本库第二大的表（仅次于 rank_decision_log）；
--   * 保留期取 config.Rank.AssignmentRetentionDays（示例 180 天），必须大于最长实验周期；
--   * 清理只按 assigned_at 分批（DELETE ... LIMIT），调用方必须循环到返回 0 而不放大 limit；
--   * 删行不会改变分组：桶号由 BucketOf(主体, exp_key, hash_seed) 纯函数决定，
--     被清理的主体下次请求会重新写入同一桶号，丢失的只是「首次分桶时间」这一线索，
--     这是可接受的取舍（明细归属可从 rank_decision_log 的 bucket_no 复核）。
--
-- 回滚（本期只建表，DROP 即可完全回滚；DROP 会丢失全部 sticky 归属，必须先备份）：
--   DROP TABLE IF EXISTS `rank_experiment_assignment`;
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不触碰既有表，不锁其他表；
--   写入是 INSERT IGNORE 单行，冲突即返回 0 行，不长持锁；
--   DeleteOlderThan 使用 DELETE ... LIMIT，要求 binlog_format=ROW
--   （statement 格式下 DELETE...LIMIT 主从可能不一致）；清理必须放在低峰期分批执行。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `rank_experiment_assignment` (
  `id`           BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键（时间序，供分批清理游标）',
  `exp_key`      VARCHAR(64) NOT NULL COMMENT '实验 key（rank_experiment.exp_key）',
  `layer_key`    VARCHAR(64) NOT NULL DEFAULT '' COMMENT '落库时的互斥层（冗余列，便于按层核对分流均匀度）',
  `subject_type` TINYINT     NOT NULL DEFAULT 1 COMMENT '分桶主体类型：1 mid、2 设备摘要（model.Subject*，与 rpc SubjectType 同值）',
  `subject_id`   VARCHAR(64) NOT NULL COMMENT '主体标识：mid 十进制串或设备 sha256 摘要（明文设备号被 ValidateSubjectID 拒绝）',
  `hash_seed`    VARCHAR(64) NOT NULL COMMENT '分桶哈希盐（唯一键成员）：换盐即重分桶并产生新行，旧行保留以便对比',
  `bucket_count` INT         NOT NULL DEFAULT 1000 COMMENT '落库口径的分桶空间大小（固定为 config.Rank.BucketCount，不随调用方入参变化）',
  `bucket_no`    INT         NOT NULL COMMENT '命中的桶号，[0, bucket_count)；回显其他空间时由 model.ScaleBucket 换算，不落库',
  `variant_key`  VARCHAR(64) NOT NULL DEFAULT 'control' COMMENT '由桶号解析出的变体（未落入任何变体区间时为 control）',
  `revision`     INT         NOT NULL DEFAULT 0 COMMENT '命中变体当时的 revision（回指 rank_experiment.revision，审计用）',
  `assigned_at`  BIGINT      NOT NULL DEFAULT 0 COMMENT '首次分桶时间（Unix 秒），sticky 语义的时间锚点，保留期按此列计算',
  `ctime`        BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`        BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒；本表事实不可改写，正常恒等于 ctime）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_subject` (`exp_key`, `subject_type`, `subject_id`, `hash_seed`),
  KEY `idx_variant` (`exp_key`, `variant_key`),
  KEY `idx_seed_variant` (`exp_key`, `hash_seed`, `variant_key`),
  KEY `idx_assigned_at` (`assigned_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='实验分桶归属事实表（INSERT IGNORE 实现 sticky 首次分桶，唯一键含 hash_seed 使换盐可审计，按 assigned_at 分批清理）';
