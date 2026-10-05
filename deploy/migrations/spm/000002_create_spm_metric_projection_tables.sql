-- =====================================================================
-- spm 服务 - 指标口径注册表、窗口指标投影、窗口水位与内容只读投影
-- =====================================================================
-- 库名：go_video_spm（见 services/spm/etc/spm.v1.yaml 的 DataSource）。
-- 数据所有者：spm 服务（AGENTS.md §7）。对应 model 文件：
--   spm_metric_definition    -> model/metric_definition.go   口径目录（唯一事实源，只能登记）
--   spm_metric_window        -> model/metric_window.go       窗口指标投影（可重算，非事实源）
--   spm_window_watermark     -> model/window_watermark.go    窗口闭合水位投影（可重算，非事实源）
--   spm_content_projection   -> model/content_projection.go  content.published.v1 本地只读投影（可重放重建）
-- 影响范围：仅新增四张表，不改动任何既有表，可独立应用。
-- 投影属性（AGENTS.md §5、docs/data-design.md §5）：
--   spm_metric_window / spm_window_watermark / spm_content_projection 都是**投影**，
--   可由 spm_behavior_event 事实与 content.published.v1 事件全量重算，不作为任何服务的
--   唯一事实源；分区归属、上下架状态的真值在 video/catalog/rights，本库只投影过滤所需的
--   最小列，且不建跨库外键。跨库一致性靠事件重放与 RecomputeMetrics 修复链路。
-- 幂等键（写入侧唯一约束，缺一条就会把重放变成累加）：
--   uniq_metric_version (metric_key, metric_version)          口径版本不可原地改写
--   uniq_request_id     (request_id)                          登记请求重放识别
--   uniq_metric (subject_type, subject_id, metric_key, metric_version, window_type, window_start)
--                                                             同窗口重放整行覆盖，绝不累加
--   uniq_watermark (subject_type, metric_key, metric_version, window_type)
--   uniq_subject  (subject_type, subject_id)                  投影只保留最新状态
-- 回滚：
--   DROP TABLE IF EXISTS `spm_content_projection`;
--   DROP TABLE IF EXISTS `spm_window_watermark`;
--   DROP TABLE IF EXISTS `spm_metric_window`;
--   DROP TABLE IF EXISTS `spm_metric_definition`;
-- 锁风险：
--   新建空表本身无锁风险。真正的风险在写入形态：
--   1) spm_metric_window 的 UpsertBatch 是多值 INSERT ... ON DUPLICATE KEY UPDATE，
--      单批上限 500 行（model.maxMetricPointsPerBatch，与契约 WriteMetricWindowReq.points
--      同源），按 uniq_metric 顺序写入以避免交叉锁；重算链路的先清后写必须带 LIMIT 分批。
--   2) 本表行数 = 主体数 × 口径数 × 窗口数，是库里增长最快的表；按 window_start 归档
--      或分区由运维轮处理，本期只保留 DeleteByWindow 的分批删除。
--   3) ListHot/CountHot 对 spm_content_projection 做 LEFT JOIN，两侧都按 (subject_type,
--      subject_id) 主键唯一索引命中，是索引内连接；榜查询必须带 window_start 等值条件，
--      否则退化为全表扫描（model.buildHotQuery 已在缺参数时直接拒绝）。
-- =====================================================================

-- 指标口径注册表：(metric_key, metric_version) 唯一确定一个口径。
-- 写入方：Spm.UpsertMetricDefinition / Spm.UpdateMetricDefinitionState（只新增版本与迁移状态，
--   没有 DELETE 语义：历史窗口的解释依赖口径定义持续可查）。
-- 本表是「目录」而不是投影：不存在从事实重算的概念，只能由登记流程写入。
CREATE TABLE IF NOT EXISTS `spm_metric_definition` (
  `id`                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `metric_key`         VARCHAR(100)    NOT NULL COMMENT '指标键，如 play_finish_rate / hot_score（与 model 截断长度一致）',
  `metric_version`     INT             NOT NULL COMMENT '口径版本，>=1；改口径必须新增版本，禁止原地改写',
  `name`               VARCHAR(100)    NOT NULL DEFAULT '' COMMENT '展示名',
  `formula`            VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '口径公式说明（人读，必须写清分子/分母/去重键）',
  `unit`               VARCHAR(32)     NOT NULL DEFAULT '' COMMENT '单位：count/ratio/seconds/score',
  `supported_windows`  VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '允许的窗口粒度 CSV，如 "1,2,3"（对应 rpc.WindowType）',
  `source_event_types` VARCHAR(255)    NOT NULL DEFAULT '' COMMENT '依赖的事件类型 CSV，必须落在 model.SupportedEventType 白名单内',
  `state`              TINYINT         NOT NULL DEFAULT 1 COMMENT '状态：1 DRAFT、2 ACTIVE、3 RETIRED（新登记一律 DRAFT）',
  `description`        VARCHAR(500)    NOT NULL DEFAULT '' COMMENT '变更说明/状态迁移理由（UpdateState 会覆写为 reason）',
  `created_by`         VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '登记人',
  `request_id`         VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '登记请求幂等键（空串表示未带幂等键的历史行，因此只做普通索引）',
  `ctime`              BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`              BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_metric_version` (`metric_key`, `metric_version`),
  -- FindByRequestID：幂等回放识别（非唯一：允许未带 request_id 的多行都是空串）
  KEY `idx_request_id` (`request_id`),
  -- FindActive / CountActive：按 metric_key 找 ACTIVE 版本
  KEY `idx_key_state` (`metric_key`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='指标口径注册表（口径版本化的落点，唯一事实源；只能登记，不可重算）';

-- 窗口指标投影：一行 = 一个「主体 × 口径版本 × 窗口」的取值。
-- 这是投影，可由 spm_behavior_event + 口径重算，不作唯一事实源。
-- 写入方：Spm.WriteMetricWindow（实时聚合器 / 离线回填 / RecomputeMetrics）；
--   读取方：Spm.GetMetric / BatchGetMetrics / ListHotSubjects。
-- 比率类指标同时写 numerator/denominator，跨窗口合并才能重算而不是加权平均近似。
-- 幂等：uniq_metric 命中时整行覆盖（绝不累加），迟到保护由 allow_late_write 决定 SQL 形态。
CREATE TABLE IF NOT EXISTS `spm_metric_window` (
  `id`               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `subject_type`     TINYINT         NOT NULL DEFAULT 0 COMMENT '主体类型：1 aid、2 zone_id、3 mid、4 catalog_item（rpc.SubjectType）',
  `subject_id`       BIGINT          NOT NULL DEFAULT 0 COMMENT '主体主键（aid/zone_id/mid/catalog 条目或 episode ID），跨服务只传主键',
  `metric_key`       VARCHAR(100)    NOT NULL COMMENT '指标键',
  `metric_version`   INT             NOT NULL COMMENT '口径版本（写入时必须是具体版本，不接受 0=ACTIVE 的解析结果入库）',
  `window_type`      TINYINT         NOT NULL DEFAULT 0 COMMENT '窗口粒度：1 5min、2 hour、3 day、4 week、5 total（rpc.WindowType）',
  `window_start`     BIGINT          NOT NULL DEFAULT 0 COMMENT '窗口左边界（Unix 秒，入库前已按粒度规整；TOTAL 恒为 0）',
  `metric_value`     DOUBLE          NOT NULL DEFAULT 0 COMMENT '指标值（计数类同样落在这一列）',
  `numerator`        BIGINT          NOT NULL DEFAULT 0 COMMENT '分子（比率类必填，计数类填 0）',
  `denominator`      BIGINT          NOT NULL DEFAULT 0 COMMENT '分母（比率类必填，计数类填 0；0 表示该窗口还没有分母，读取侧不得当成 100%）',
  `sample_count`     BIGINT          NOT NULL DEFAULT 0 COMMENT '参与聚合的样本数（事件条数）',
  `source`           TINYINT         NOT NULL DEFAULT 0 COMMENT '写入来源：1 实时聚合、2 离线回填、3 重算修复（结构上不存在人工覆盖这一档）',
  `event_time`       BIGINT          NOT NULL DEFAULT 0 COMMENT '该窗口最后一次推进时间（Unix 秒，迟到判定的基准）',
  `write_request_id` VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '最近一次写入的幂等键（排查重复回填写入）',
  `ctime`            BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`            BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_metric` (`subject_type`, `subject_id`, `metric_key`, `metric_version`, `window_type`, `window_start`),
  -- 榜单查询与重算前的先清后写：按口径版本 + 窗口等值/区间过滤后按 metric_value 倒序
  KEY `idx_metric_window` (`metric_key`, `metric_version`, `window_type`, `window_start`, `subject_type`, `metric_value`),
  -- 水位兜底：某口径在某粒度下最新已写入窗口（有 spm_window_watermark 时走水位，
  -- 本索引服务口径首次上线、水位尚未建立时的 max(window_start) 回查）
  KEY `idx_metric_latest` (`metric_key`, `metric_version`, `subject_type`, `window_type`, `window_start`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='窗口指标投影（可由 spm_behavior_event 重算，不是唯一事实源）：主体 × 口径版本 × 窗口的取值';

-- 窗口闭合水位：一个「主体类型 × 口径版本 × 窗口粒度」一行，记录最近一个已闭合窗口。
-- 契约里 GetMetric / BatchGetMetrics / ListHotSubjects 的 window_start=0 都表示
-- 「最近一个已闭合窗口」。没有这张表，每次读都要对 spm_metric_window 做一次
-- max(window_start) 扫描；本表把它压成一次主键点查，同时承载 GetUserInterestReply.stale
-- 用到的「数据推进时刻」。
-- 这是投影：全量重算窗口即可重建水位，不作唯一事实源；只前进不回退（见 model.Advance 守卫）。
CREATE TABLE IF NOT EXISTS `spm_window_watermark` (
  `id`                BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `subject_type`      TINYINT         NOT NULL DEFAULT 0 COMMENT '主体类型（0 = 跨主体汇总水位，供分区榜与全站榜使用）',
  `metric_key`        VARCHAR(100)    NOT NULL COMMENT '指标键',
  `metric_version`    INT             NOT NULL COMMENT '口径版本',
  `window_type`       TINYINT         NOT NULL DEFAULT 0 COMMENT '窗口粒度（rpc.WindowType），水位不跨粒度共享',
  `last_closed_start` BIGINT          NOT NULL DEFAULT 0 COMMENT '最近一个已闭合窗口的左边界（Unix 秒，已按粒度规整；0 = 还没有闭合窗口）',
  `last_event_time`   BIGINT          NOT NULL DEFAULT 0 COMMENT '该水位对应数据的推进时刻（Unix 秒，stale 判定与堆积观测）',
  `rows_written`      BIGINT          NOT NULL DEFAULT 0 COMMENT '该闭合窗口写入的主体行数（0 或多出寻常即说明聚合器漏算）',
  `update_request_id` VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '最近一次推进水位的幂等键（排查重复推进）',
  `ctime`             BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`             BIGINT          NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_watermark` (`subject_type`, `metric_key`, `metric_version`, `window_type`),
  -- 口径退役清理：DeleteByMetricVersion 按 (metric_key, metric_version) 删除全部主体维度的水位。
  -- uniq_watermark 的前导列是 subject_type，该条件用不上它，必须单独一条索引，
  -- 否则清理会变成全表扫描 + 大范围行锁。
  KEY `idx_metric_version` (`metric_key`, `metric_version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='窗口闭合水位投影（可由 spm_metric_window 重算，不是唯一事实源）：window_start=0 的解析依据';

-- 内容本地只读投影：content.published.v1 的最小过滤列。
-- 分区归属与上下架状态的真值在 video/catalog/rights，spm 没有写权限，也不跨库 JOIN，
-- 因此把「出榜必需的列」投影到本地（AGENTS.md §5 明确允许的本地只读投影）。
-- 主体按 (subject_type, subject_id) 存而不是只存 aid：上游 payload 给的是
--   content_id + content_type（UGC 的 content_id 就是 aid，PGC 是 catalog 的集 ID），
--   分区在 video 侧叫 typeid、catalog 侧叫 zoneid；把列写成 aid 会让版权内容的分区榜
--   恒为空集（JOIN 不上），是静默错误。
-- 本表同样是投影：全量重放 content.published.v1 即可重建，不作唯一事实源。
-- 幂等：uniq_subject + Apply 的 event_time 乱序守卫（旧事件绝不覆盖新状态）。
CREATE TABLE IF NOT EXISTS `spm_content_projection` (
  `id`            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `subject_type`  TINYINT         NOT NULL DEFAULT 0 COMMENT '主体类型：1 aid（UGC 稿件）、4 catalog 条目/集（由 content_type 映射）',
  `subject_id`    BIGINT          NOT NULL DEFAULT 0 COMMENT '主体主键：UGC 为 aid，PGC 为事件里的 content_id（episode 级，见 README 契约缺口）',
  `content_type`  TINYINT         NOT NULL DEFAULT 0 COMMENT '事件原始 content_type：1 UGC、2 PGC（留作溯源，便于排查映射错序）',
  `zone_id`       BIGINT          NOT NULL DEFAULT 0 COMMENT '分区（投影自上游 typeid/zoneid；0 = 事件未给出，分区榜对该主体不可用）',
  `author_mid`    BIGINT          NOT NULL DEFAULT 0 COMMENT '作者 mid（只存主键，不存昵称等可变主资料）',
  `state`         TINYINT         NOT NULL DEFAULT 0 COMMENT '可见性：0 在架、1 下架/过期/删除（ContentStateOfAction 映射，offline/expired/delete 必须压出榜单）',
  `category`      BIGINT          NOT NULL DEFAULT 0 COMMENT '一级分类 ID（兴趣键之外的补充维度，0 = 未知）',
  `last_event_id` VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '最近一次生效的事件 ID（排查乱序覆盖，必填）',
  `event_time`    BIGINT          NOT NULL DEFAULT 0 COMMENT '最近一次生效事件的 occurred_at（Unix 秒，乱序守卫的比较列）',
  `ctime`         BIGINT          NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`         BIGINT          NOT NULL DEFAULT 0 COMMENT '本地写入时间（Unix 秒，不参与乱序比较）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_subject` (`subject_type`, `subject_id`),
  -- 分区榜与「某分区还有哪些在架内容」的重建/自检
  KEY `idx_zone_subject` (`zone_id`, `subject_type`, `state`),
  -- CountHidden / 观测下架内容是否仍带行为数据
  KEY `idx_state` (`state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='content.published.v1 的本地只读投影（只投影榜单过滤必需的列，可重放重建，不是唯一事实源）';
