-- =====================================================================
-- feature-store 服务 - 特征定义与版本表（元数据，唯一事实源）
-- =====================================================================
-- 用途：建立 feature_definition，一行 = 一个特征的一个版本。
--   存放需要持久化与审计的特征元数据：值类型、主体维度、计算来源、时间窗口、
--   TTL、缺失降级用的默认值、列表/向量维度、状态、口径说明与两份摘要。
--   「谁能读」（privacy_level）与「算什么」（source/window_seconds）都在这张表定口径，
--   在线特征值本身不在这里（见 000003 与本文件末尾的「事实定位」）。
-- 数据所有者：feature-store 服务（AGENTS.md §5）。
--   其他服务（spm / recommend-recall / recommend-rank / risk-control）只能通过
--   FeatureStore RPC 读写，禁止直连本库任何表。
--
-- 数据库：go_video_feature_store。
--
-- 幂等与唯一性：
--   · PRIMARY KEY (feature_key, version) 就是契约声明的 uniq_key_version：
--     RegisterFeature 用 INSERT ... ON DUPLICATE KEY UPDATE mtime = mtime 探测，
--     RowsAffected=0 即「该版本已登记」，logic 再比对 definition_digest 决定
--     reused=true（完全一致）还是返回 ErrFeatureMetadataImmutable/
--     ErrFeatureDefinitionImmutable（不可变字段或版本元数据不同）。
--     已有版本永不原地改写：口径变化只能注册新版本并切 ACTIVE，
--     这样历史 feature_value 永远能被产生它时的那份定义解释。
--   · 无 AUTO_INCREMENT 主键是刻意的（与 recall_pool_current 同一取舍）：
--     model.FeatureDefinition 的列清单包含 feature_key 与 version，全部读写
--     （FindOne / ListByKey / ListByKeys / UpdateState / UpdatePrivacy）
--     都按 (feature_key, version) 定位或按其前缀扫描，聚簇主键即唯一访问路径。
--
-- 隐私边界（AGENTS.md §7、docs/data-design.md §6）：
--   · privacy_level NOT NULL 且无默认值可用：注册强制显式定级（0 一律被
--     model.ValidPrivacyLevel 拒掉），本表不提供「未声明」的落库形态；
--   · source 的取值域结构上不存在广告/支付/会员/分成来源（AGENTS.md §1 范围外），
--     CHECK 之外的第二道闸是 model.ForbiddenFeatureKey 的整段命名黑名单；
--   · 本表不含任何主体标识列，因此不存在明文 PII；值侧的脱敏见 000003。
--
-- 事实定位：本表是元数据的唯一事实源，不是特征值事实源。
--   在线特征值的主读存储是 Redis（CacheRedis，键见 services/feature-store/README.md
--   「缓存」一节），feature_value 只是回源兜底与隐私删除的持久层，可从上游重算。
--
-- 索引：
--   · idx_state_privacy (state, privacy_level, feature_key, version)
--     ListFeatureDefinitions 的常驻谓词是「按授权收敛可见集合」：
--     state = ? AND privacy_level <= ?，排序固定 (feature_key, version) 升序，
--     与本索引尾列同序，因此该过滤路径不产生 filesort；
--   · idx_scope_source (entity_scope, source, feature_key, version)
--     同上，服务「按主体维度 + 来源」的定义盘点视图；
--   · 不单独为 feature_key 前缀 LIKE 建索引：前缀匹配已能用 uniq 主键的最左列，
--     再建一份 (feature_key) 二级索引只会让每次注册多维护一棵树。
--
-- 回滚：
--   DROP TABLE IF EXISTS `feature_definition`;
--   本表是元数据唯一事实源，不可由特征值重算：误删只能从代码评审记录与
--   feature_version_switch 的 from_digest/to_digest 反查（摘要不可逆，只能定位到
--   「改过什么」，恢复不了口径说明文本）。执行前必须先备份，见 000004 的审计链。
--   回滚窗口内在线读会退化为 SOURCE_UNAVAILABLE（没有定义就没有默认值，
--   服务不允许伪造 0 值），因此本表的 DROP 属于全链路熔断操作。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改动既有数据；
--   注册是单行 INSERT，无范围锁。后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `feature_definition` (
  `feature_key`      VARCHAR(64)   NOT NULL COMMENT '特征键，snake_case，形态与长度由 model.ValidFeatureKey 约束（2..64）；范围外语义整段命中即拒绝注册',
  `version`          INT           NOT NULL COMMENT '版本号，>= 1；同一 feature_key 可多版本共存，(feature_key, version) 唯一',
  `name`             VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '展示名（model 上限 maxNameLen=128）',
  `value_type`       SMALLINT      NOT NULL COMMENT '值类型：1 int64 2 double 3 bool 4 string 5 int64_list 6 double_list；注册后不可变（进 immutable_digest）',
  `entity_scope`     SMALLINT      NOT NULL COMMENT '主体类型：1 mid 2 aid 3 zone 4 设备哈希 5 搜索词 6 版权条目 7 IP 摘要；注册后不可变',
  `source`           SMALLINT      NOT NULL COMMENT '计算来源：1 spm 指标 2 spm 兴趣 3 spm 留存 4 离线模型 5 实时规则滑窗 6 静态配置。结构上不存在广告/支付/会员来源，注册后不可变',
  `privacy_level`    SMALLINT      NOT NULL COMMENT '隐私级别（必填，0 不允许落库）：1 公开聚合 2 内容属性 3 受控标识 4 用户画像；与 entity_scope 的自洽区间由 model.PrivacyMatchesScope 校验，变更走 UpdateFeaturePrivacy 并留审计',
  `window_seconds`   BIGINT        NOT NULL DEFAULT 0 COMMENT '统计时间窗口（秒）；0 = 无窗口，只有实时规则滑窗与静态配置允许（SourceRequiresWindow 拦住行为类来源的 0 值）',
  `ttl_seconds`      BIGINT        NOT NULL COMMENT '值存活时间（秒），注册强制 > 0：TTL 缺失等于特征值永不过期，读侧无法判断新鲜度（ErrTTLRequired）',
  `default_value`    TEXT          NOT NULL COMMENT '缺失降级用的默认值，按 value_type 序列化的字符串形态；注册时用 ValidateDefaultValue 试解析，保证降级返回的东西一定可用。用 TEXT 而不是 VARCHAR：向量类默认值最坏宽度由 dimension 上限（512 个定点 double）决定',
  `dimension`        SMALLINT      NOT NULL DEFAULT 0 COMMENT '列表/向量类特征的元素数上限（1..model.MaxDimension=512）；标量类恒为 0，与 value_type 双向自洽（ErrDimensionInvalid）',
  `state`            SMALLINT      NOT NULL DEFAULT 1 COMMENT '状态机：1 DRAFT 2 ACTIVE 3 RETIRED；新注册一律 DRAFT（不允许跳过评审直接 ACTIVE），RETIRED 不接受写入且不可复活',
  `description`      VARCHAR(1024) NOT NULL COMMENT '口径说明：为什么存在、怎么算（注册必填，上限 maxDescriptionLen=1024）。没有它一年后没人知道这个特征是什么',
  `change_note`      VARCHAR(512)  NOT NULL DEFAULT '' COMMENT '本版本的变更说明（相对上一版本改了什么，上限 maxChangeNoteLen=512）',
  `created_by`       VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '登记人：admin:<id> / system:<svc> / offline-job:<id>',
  `immutable_digest` VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '不可变五字段（value_type/entity_scope/source/window_seconds/dimension）的 sha256 十六进制；回答「这两个版本是不是同一个特征」，版本切换前置校验用',
  `definition_digest` VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '不可变字段 + ttl_seconds + default_value 的 sha256；回答「这两次注册是不是同一份定义」，注册幂等复用判定用。刻意不含 privacy_level（它独立变更，算进来会让调过级别的历史注册无法重放）',
  `digest_ver`       VARCHAR(32)   NOT NULL DEFAULT '' COMMENT '摘要算法版本（model.DigestVersion）：换算法时必须显式提升，不能让新旧摘要直接比对',
  `ctime`            BIGINT        NOT NULL DEFAULT 0 COMMENT '注册时间（Unix 秒）',
  `mtime`            BIGINT        NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）；只有 state / privacy_level 两处可原地改写',
  PRIMARY KEY (`feature_key`, `version`),
  KEY `idx_state_privacy` (`state`, `privacy_level`, `feature_key`, `version`),
  KEY `idx_scope_source` (`entity_scope`, `source`, `feature_key`, `version`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='特征定义与版本表（元数据唯一事实源，一行=一个版本，已有版本禁止原地改写）';
