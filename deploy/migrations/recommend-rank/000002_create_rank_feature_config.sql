-- =====================================================================
-- recommend-rank 服务 - 排序特征配置版本表
-- =====================================================================
-- 数据库：go_video_recommend_rank（取自 services/recommend-rank/etc/recommendrank.v1.yaml 的
--   DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
-- 数据所有者：recommend-rank 服务（AGENTS.md §5）。其他服务只能通过 rpc.Rank 读写，
--   禁止直连本库；特征的实际存储与取数属 feature-store（本期尚未接入，
--   见 internal/repository/downstream.go 的 FeatureSource stub 与「契约缺口」）。
--
-- 表与代码对应关系（列名严格取自 services/recommend-rank/model/featureconfig.go 的 db tag 与 SQL 字符串）：
--   * rank_feature_config  model.RankFeatureConfig —— 一行 = 一个不可变的特征清单版本，
--     rank_model_version.feature_config_version 引用它。
--
-- 为什么单独建表而不是把特征清单塞进模型版本行：
--   同一份特征配置会被多个模型版本复用；如果每个模型版本各存一份清单，
--   改特征时可以只改其中一行，「模型版本可审计」就会被偷偷变化的清单破坏。
--   拆表后清单只有一个 owner（config_version），模型版本只能引用已登记的版本。
--
-- 唯一键与幂等设计：
--   1. uniq_config_version (config_version)：UpsertFeatureConfig 的幂等锚点。
--      版本号一旦登记即不可变（model 接口里没有 UpdateFeatureKeys，只有 UpdateState）；
--      model.Insert 捕获 1062 后转 model.ErrFeatureConfigExists，
--      调用方按「同 key 重放」处理而不是报 500。
--   2. keys_digest = sha256(feature_keys) 是清单指纹：同一份清单换版本号重登记时
--      指纹相同，巡检据此发现「复制粘贴式假版本」；它不是唯一键，
--      因为「同样的清单在不同时期确实是不同语义」是合法用法。
--
-- 索引取自真实查询路径（见 model/featureconfig.go 的 SQL）：
--   * uniq_config_version：FindOne(config_version)、UpdateState（WHERE config_version = ? AND state = ?）。
--   * idx_state (state)：List（可选 state 过滤 + ORDER BY id DESC LIMIT/OFFSET）；
--     InnoDB 二级索引隐含主键尾列，故 state 等值 + id 倒序即索引序，不产生 filesort。
--     不给 feature_keys 建索引：TEXT 列只能前缀索引，而没有任何查询按清单内容检索。
--
-- 约束说明（本仓库迁移不使用 ENUM/CHECK，取值域由 model 层把守，此处仅记录口径）：
--   * missing_policy 只允许 model.MissingPolicy* 三个受控 key：default / drop_source / reject，
--     由 ValidMissingPolicy 在 Insert 前拒绝其他值。
--   * state 只有两态（0 停用、1 生效），语义版本不可改；停用前必须先由
--     model.RankModelVersionModel.CountByFeatureConfig 确认没有 ACTIVE/READY 模型引用它
--     （repository.DisableFeatureConfig 负责这段跨表检查与 ErrFeatureConfigInUse）。
--   * feature_count 是冗余巡检列：Insert 时按 feature_keys 重新计算，
--     与清单不一致即为代码缺陷，靠「人工排障只读它、决策不读它」限制影响面。
--   * feature_store_scene 只是将来接 feature-store 的读取场景 key（跨服务只传主键/稳定 key，
--     不传对方内部结构，AGENTS.md §5）。
--
-- 回滚（本期只建表，DROP 即可完全回滚；表内是配置登记信息，DROP 前必须先备份）：
--   DROP TABLE IF EXISTS `rank_feature_config`;
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不触碰既有表，不锁其他表；
--   本表写入极低频（发版/配置变更），无热点行；UpdateState 按 config_version 等值命中唯一索引，
--   只锁单行。后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `rank_feature_config` (
  `id`                  BIGINT        NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `config_version`      VARCHAR(64)   NOT NULL COMMENT '特征配置版本（唯一，登记后不可变）；改清单必须发新版本号',
  `feature_keys`        TEXT          NOT NULL COMMENT '特征 key 清单（model.JoinFeatureKeys 生成：升序、去重、逗号分隔；实际取数由 feature-store 提供）',
  `feature_count`       INT           NOT NULL DEFAULT 0 COMMENT '特征条数（冗余巡检列，Insert 时按 feature_keys 重算；上限 config.Rank.MaxFeatureKeys）',
  `missing_policy`      VARCHAR(16)   NOT NULL DEFAULT 'default' COMMENT '缺失策略受控 key：default 补默认值、drop_source 整路候选不参排、reject 视为不可排序并降级（model.MissingPolicy*）',
  `feature_store_scene` VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '将来接 feature-store 的读取场景 key（稳定 key，不含对方内部结构）',
  `keys_digest`         CHAR(64)      NOT NULL DEFAULT '' COMMENT 'sha256(feature_keys) 十六进制，配置指纹；用于识别重复登记的等价清单',
  `state`               TINYINT       NOT NULL DEFAULT 1 COMMENT '状态：0 停用、1 生效（model.FeatureState*，与 rpc 语义一致）；被 ACTIVE/READY 模型引用时禁止停用',
  `revision`            INT           NOT NULL DEFAULT 1 COMMENT '元数据修订号（启停等非语义变更 +1），config_version 不变',
  `operator`            VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '最后操作者（审计必填）',
  `note`                VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '变更原因（rpc UpsertFeatureConfigReq.reason / 停用理由落库列）',
  `ctime`               BIGINT        NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`               BIGINT        NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_config_version` (`config_version`),
  KEY `idx_state` (`state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='排序特征配置版本表（清单版本不可变，指纹留档，被引用的版本禁止停用）';
