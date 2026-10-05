-- =====================================================================
-- recommend-rank 服务 - 排序模型版本登记表
-- =====================================================================
-- 数据库：go_video_recommend_rank（取自 services/recommend-rank/etc/recommendrank.v1.yaml 的
--   DataSource；建库与 schema_migrations 由 scripts/migrate.ps1 负责，本文件不写 CREATE DATABASE）。
-- 数据所有者：recommend-rank 服务（AGENTS.md §5）。其他服务只能通过 rpc.Rank 读写，
--   禁止直连本库；稿件/用户/媒资主数据分属 video、user-profile、asset，本库只存 aid 之类的主键引用。
--
-- 表与代码对应关系（列名严格取自 services/recommend-rank/model/modelversion.go 的 db tag 与 SQL 字符串）：
--   * rank_model_version  model.RankModelVersion  —— 一个 (model_key, version) 一行，
--     登记它绑定的特征配置版本、多目标权重、离线指标与模型工件引用。
--
-- 唯一键与幂等设计：
--   1. uniq_model_version (model_key, version)：UpsertModelVersion 的幂等锚点。
--      版本号一旦登记即不可变（改语义必须发新版本），model.Insert 捕获 1062 后转
--      model.ErrModelVersionExists，调用方按「同 key 重放」处理而不是报 500。
--   2. uniq_active (active_model_key)：用 STORED 生成列把「每个 model_key 至多一行 ACTIVE」
--      变成数据库级约束 —— MySQL 没有部分唯一索引，生成列是唯一能真正表达该不变量的方式：
--        active_model_key = IF(state = 3, model_key, NULL)
--      NULL 不参与唯一性判定，所以 DRAFT/READY/RETIRED 任意多行可共存；
--      一旦有两行同时 ACTIVE，第二条 INSERT/UPDATE 直接报 1062，
--      而不是等到线上出现「两个实例读到不同模型版本」才发现。
--      注意：本列由数据库计算，任何 INSERT/UPDATE 语句都不得写入它
--      （model 包的全部语句均按显式列名书写，未包含 active_model_key）。
--
-- 语义字段与审计字段：
--   * objective_weights / feature_config_version 是语义字段：model.UpdateWeights 的 SQL
--     带 `state IN (1,2)` 条件，ACTIVE/RETIRED 之后改不动（要改就登记新版本）。
--   * artifact_ref 只存对象存储 key（模型本体与任何密钥都不入库，AGENTS.md §4/§5）。
--   * offline_metrics 是离线指标 JSON，仅登记供人工核对，不参与在线决策；
--     其中不得包含广告收入、付费转化等商业化指标（AGENTS.md §7）。
--   * previous_active 记录激活时的上一个 ACTIVE 版本，回滚路径因此可查；
--     operator/note 对应 rpc 的 operator/reason，激活与回滚必须写明理由（审计要求）。
--
-- 索引取自真实查询路径（见 model/*.go 的 SQL）：
--   * uniq_model_version：FindOne(model_key, version)。
--   * idx_model_state (model_key, state, activated_at)：FindActive
--     （model_key AND state=3 ORDER BY activated_at DESC, id DESC LIMIT 1）、
--     Deactivate（model_key AND state=3 AND id<>?）、
--     List(ModelVersionQuery 按 model_key/state 过滤 + id DESC 分页)。
--   * idx_feature_config (feature_config_version, state)：CountByFeatureConfig
--     （停用特征配置前确认没有 ACTIVE/READY 模型在引用它）。
--
-- 回滚（本期只建表，DROP 即可完全回滚；表内是模型登记与审计信息，DROP 前必须先备份）：
--   DROP TABLE IF EXISTS `rank_model_version`;
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不触碰既有表，不锁其他表；
--   生成列 + 唯一索引会让「同时激活两个版本」的写入失败（这是预期行为，不是锁风险）。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `rank_model_version` (
  `id`                     BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `model_key`              VARCHAR(64)  NOT NULL COMMENT '逻辑模型名（如 home_feed_multi_gate），一个场景一个 key',
  `version`                VARCHAR(64)  NOT NULL COMMENT '版本号，登记后不可变；改语义必须发新版本',
  `feature_config_version` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '绑定的特征配置版本（rank_feature_config.config_version），必须是已登记且启用的版本',
  `objective_weights`      VARCHAR(1024) NOT NULL DEFAULT '' COMMENT '多目标权重 JSON，key 只能是 model.SupportedObjectives() 登记的受控目标（禁止广告/付费/会员目标）',
  `artifact_ref`           VARCHAR(512) NOT NULL DEFAULT '' COMMENT '模型工件引用（对象存储 key，非密钥；模型本体不入库）',
  `offline_metrics`        VARCHAR(2048) NOT NULL DEFAULT '' COMMENT '离线指标 JSON（AUC/校准等），仅审计展示，不参与在线决策',
  `state`                  TINYINT      NOT NULL DEFAULT 1 COMMENT '状态：1 DRAFT、2 READY、3 ACTIVE、4 RETIRED（model.ModelState*，与 rpc ModelVersionState 同值）',
  `revision`               INT          NOT NULL DEFAULT 1 COMMENT '元数据修订号（非语义字段变更 +1），版本号不变',
  `activated_at`           BIGINT       NOT NULL DEFAULT 0 COMMENT '激活时间（Unix 秒），0 表示从未激活',
  `previous_active`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '激活时上一个 ACTIVE 版本号（回滚线索），空表示首次激活',
  `operator`               VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '最后操作者（审计必填，model.ErrOperatorRequired）',
  `note`                   VARCHAR(255) NOT NULL DEFAULT '' COMMENT '变更原因（rpc reason 落库列，激活/回滚必填）',
  `ctime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                  BIGINT       NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）',
  `active_model_key`       VARCHAR(64) GENERATED ALWAYS AS (IF(`state` = 3, `model_key`, NULL)) STORED COMMENT '仅当 state=ACTIVE 时等于 model_key，否则 NULL；用于「每个 model_key 至多一个 ACTIVE 版本」的唯一约束',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uniq_model_version` (`model_key`, `version`),
  UNIQUE KEY `uniq_active` (`active_model_key`),
  KEY `idx_model_state` (`model_key`, `state`, `activated_at`),
  KEY `idx_feature_config` (`feature_config_version`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci COMMENT='排序模型版本登记表（版本号不可变，ACTIVE 唯一性由生成列约束，激活/回滚可审计）';
