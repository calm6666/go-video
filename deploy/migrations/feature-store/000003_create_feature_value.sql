-- =====================================================================
-- feature-store 服务 - 特征值表（回源兜底与隐私删除的持久层，可重算投影）
-- =====================================================================
-- 用途：建立 feature_value，存「一个特征版本 × 一个主体」的一个值，含 TTL 与上游口径追溯。
-- 数据所有者：feature-store 服务（AGENTS.md §5）。
--   写入方只有三条路径：WriteFeatures（上游计算链路）、SubmitBackfillJob 的 worker、
--   PurgeExpired / EraseEntityFeatures 的清理。其他服务禁止直连本表。
--
-- 数据库：go_video_feature_store。
--
-- 事实定位（本文件最重要的一条，AGENTS.md §4/§7）：
--   本表**不是**在线特征的主读存储，也不是不可重算的业务事实：
--   · 在线读的主存是 Redis（配置字段 CacheRedis），键 <prefix>:val:<key>:<ver>:<scope>:<entity>，
--     GetFeature / BatchGetFeatures 命中缓存即返回，不碰本表；
--   · 本表承接三件缓存做不到的事：(1) 缓存 miss / 过期后的回源兜底，
--     (2) 隐私擦除与主体维度导出必须扫全量（含历史版本残值，Redis 里已被 TTL 淘汰的也算），
--     (3) 回填批次改了哪些行、按 backfill_job_id 可追溯；
--   · 因此本表内容可由「上游指标重算 + 回填作业」完整重建：它是投影，不是唯一事实源。
--     真正的口径事实在 feature_definition，产出侧事实在 spm / 离线模型 / 风控滑窗。
--   · 不引入 MySQL 之外的「在线向量库」：double_list 的维度受 dimension 上限（512）约束，
--     超过就该走独立评审而不是把本列撑成 BLOB。
--
-- 幂等与并发：
--   · UNIQUE KEY uniq_feature_entity (feature_key, version, entity_scope, entity_id)
--     就是契约声明的写入幂等支点：BatchUpsert 先按行构造器 IN 批查已有行的 event_time，
--     已存在则 UPDATE ... WHERE event_time <= ?（乱序旧批次不覆盖较新值，计入 rejected，
--     只有回填路径显式 AllowStaleOverwrite 才放行）；不存在则 INSERT
--     ... ON DUPLICATE KEY UPDATE mtime = mtime，RowsAffected=0 即「被并发抢先」，
--     走同一条 event_time 守卫重判，不依赖驱动专有错误码。
--   · 整批的 request_id 幂等在 feature_write_receipt（000006），本表的 write_request_id
--     列只是「这一行最后是哪一批写进来的」排障线索，不参与幂等判定。
--   · 值永远属于具体版本：切 ACTIVE 指针不搬值，因此 PREVIOUS_VERSION 降级能读到旧版本行。
--
-- 隐私边界（AGENTS.md §7、docs/data-design.md §6）：
--   · entity_id 只允许两种形态，入库前由 model.ValidEntityID 拦：
--     - MID/AID/ZONE/CATALOG_ITEM：正十进制主键串（跨服务只传主键，不带任何属性）；
--     - DEVICE / IP_HASH：32/40/64 位小写十六进制摘要（上游用加盐 hash 产出）。
--     明文手机号、身份证、原始 IP、明文设备号一律拒绝，「看起来像哈希」的短串也拒绝；
--   · QUERY 维度只接受归一化短词（无空白、无分隔符），这是最容易夹带隐私的维度；
--   · 本表不建跨库外键：内容/用户/指标都只存主键或摘要形态的引用，
--     video / user-profile / spm 的表不复制到本库；
--   · 某行属于哪个隐私级别由 feature_definition.privacy_level 决定（不在此冗余，
--     避免调级后本表残留过期判据），因此隐私读/删一律与定义表同 SQL 取一致视图。
--
-- expire_at 与新鲜度：
--   · expire_at = event_time + 定义的 ttl_seconds（model.ExpireAt 计算，注册强制 ttl>0，
--     因此新写入恒为正）。它是读侧唯一的新鲜度判据；
--   · 0 只可能是历史脏数据，读侧一律按「已过期」处理，不允许当「永不过期」；
--   · ctime/mtime 只用于运维排障，绝不参与新鲜度判定 —— 否则一次批次重放
--     就会把历史值伪装成刚产出的值。
--
-- 索引（在线读与回填/清理是两类完全不同的访问模式，索引按两类分开）：
--   · uniq_feature_entity：GetFeature 回源、BatchGetFeatures 的行构造器 IN 批查、
--     updateRow 的等值条件更新，全部走它；
--   · idx_entity (entity_scope, entity_id, feature_key, version)：
--     ListEntityFeatures / CountByEntity / DeleteByEntity 的主体维度谓词，
--     尾两列与 ORDER BY v.feature_key, v.version 同序，隐私核对分页不产生 filesort；
--   · idx_expire (expire_at, value_id)：SelectExpiredIDs 的
--     WHERE expire_at > 0 AND expire_at < ? ORDER BY expire_at, value_id，
--     以及 CountExpired 的收敛估算。删除走「先选主键再按主键删」，
--     不做范围 DELETE（范围锁会堵住同区的在线写）；
--   · idx_feature_version_entity (feature_key, version, entity_id)：
--     ScanKeys 的 WHERE (feature_key, version) AND entity_id > ? ORDER BY entity_id
--     与 DeleteByVersion。uniq_feature_entity 的第三列是 entity_scope，
--     回填扫描不按 scope 过滤时用它接不上，因此必须单开一条（不是冗余）。
--
-- 回滚：
--   DROP TABLE IF EXISTS `feature_value`;
--   本表可重算：从 feature_definition 取口径、按 feature_backfill_job 重新提交回填即可恢复，
--   但重算需要上游（spm 指标 / 离线快照）仍在保留期内，且**降级期间在线推荐会拿到
--   DEFAULT_VALUE**。若某特征的来源已过 RETIRED 且上游已过期，那部分值不可恢复：
--   因此本表的 DROP 仍须先备份，不能凭「可重算」就直接执行。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改动既有数据。
--   本表是全库写入 QPS 最高的表，注意三条：
--   (1) 四个二级索引意味着每次 UPSERT 都要维护，回填大批量写入必须按
--       Backfill.BatchRows 分批，不要一把梭；
--   (2) 主键是 AUTO_INCREMENT，并发批量插入在高水位下会有争用，
--       这是「先选主键再按主键删」取舍的代价，换来的是清理不阻塞在线写；
--   (3) 列表值用 TEXT：单行可远超 16KB（受 MaxListValueBytes=12288 与 MaxDimension 约束），
--       批量读的响应体积上限另由 model.MaxBatchResponseBytes 卡，别指望列宽兜住。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `feature_value` (
  `value_id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键：清理与回填按它分批，避免范围删除的间隙锁；不参与业务定位',
  `feature_key`       VARCHAR(64)  NOT NULL COMMENT '特征键（与 feature_definition 同域，不建外键：本表可按版本重算，定义变更不应级联删值）',
  `version`           INT          NOT NULL COMMENT '特征版本：值永远属于具体版本，切 ACTIVE 指针不搬值',
  `entity_scope`      SMALLINT     NOT NULL COMMENT '主体类型，必须与定义的 entity_scope 一致（跨 scope 读写一律拒绝，ErrEntityScopeMismatch）',
  `entity_id`         VARCHAR(64)  NOT NULL COMMENT '主体标识：数值主键十进制串，或设备/IP 的加盐哈希摘要（32/40/64 位十六进制）。禁止明文 PII，形态由 model.ValidEntityID 校验',
  `value_type`        SMALLINT     NOT NULL COMMENT '值类型（冗余自定义，便于直读与解码自校验）：1 int64 2 double 3 bool 4 string 5 int64_list 6 double_list',
  `int64_value`       BIGINT       NOT NULL DEFAULT 0 COMMENT '标量 int64；非本类型的行留 0（一次编码只碰与 value_type 对应的那一列）',
  `double_value`      DOUBLE       NOT NULL DEFAULT 0 COMMENT '标量 double；非本类型留 0',
  `bool_value`        SMALLINT     NOT NULL DEFAULT 0 COMMENT '标量 bool（0/1）；非本类型留 0',
  `string_value`      VARCHAR(512) NOT NULL DEFAULT '' COMMENT '标量字符串，上限 model.MaxStringValueLen=512 字节：特征值不是自由文本存储',
  `list_values`       TEXT         NOT NULL COMMENT '列表/向量：逗号分隔的纯数字，无 JSON、无自由文本；元素数受定义 dimension 约束，序列化字节受 MaxListValueBytes=12288 约束（列宽用 TEXT 承载最坏向量宽度，语义上限由服务端卡）',
  `event_time`        BIGINT       NOT NULL DEFAULT 0 COMMENT '值的产出时间（Unix 秒），入参为 0 时由服务端补齐。乱序写的判据：UPDATE 带 event_time <= ? 守卫，旧批次不覆盖较新值',
  `expire_at`         BIGINT       NOT NULL DEFAULT 0 COMMENT 'TTL 到期时间 = event_time + 定义 ttl_seconds；读侧唯一新鲜度判据，0 按已过期处理',
  `source_metric_key` VARCHAR(128) NOT NULL DEFAULT '' COMMENT '上游口径追溯：来自 spm 时为 "<metric_key>@v<n>"，用于回答「这个值是哪个指标版本算出来的」',
  `write_request_id`  VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次写入该行的幂等键（排障：这一行是哪一批写进来的），不参与幂等判定',
  `written_by`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最近一次写入者身份：system:<svc> / offline-job:<id>',
  `backfill_job_id`   BIGINT       NOT NULL DEFAULT 0 COMMENT '回填作业写行时记录的 feature_backfill_job.job_id（0 = 实时链路写入）。「某个回填批次改了哪些行」由此可查、可重算，不必按时间窗猜',
  `ctime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '首次入库时间（Unix 秒），仅运维排障用，不参与新鲜度判定',
  `mtime`             BIGINT       NOT NULL DEFAULT 0 COMMENT '最后更新时间（Unix 秒），同上；ON DUPLICATE KEY 的 no-op 探测依赖它自等',
  PRIMARY KEY (`value_id`),
  UNIQUE KEY `uniq_feature_entity` (`feature_key`, `version`, `entity_scope`, `entity_id`),
  KEY `idx_entity` (`entity_scope`, `entity_id`, `feature_key`, `version`),
  KEY `idx_expire` (`expire_at`, `value_id`),
  KEY `idx_feature_version_entity` (`feature_key`, `version`, `entity_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='在线特征值表（Redis 主读的回源兜底与隐私擦除持久层；可由上游重算，不是唯一事实源）';
