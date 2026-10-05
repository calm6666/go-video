-- =====================================================================
-- ops-config 服务 - 配置发布三表（配置项 / 不可变版本快照 / 灰度规则）
-- =====================================================================
-- 数据库：go_video_ops_config（见 services/ops-config/etc/opsconfig.v1.yaml 的 DataSource）。
-- 数据所有者：ops-config 服务。本目录的表只有本服务读写（AGENTS.md §5）。
--
-- 本文件建 3 张表，与 model 一一对应（列名严格取自 services/ops-config/model/*.go 的
-- db tag 与 SQL 字符串，改名必须两侧同步）：
--   * ops_config_item    model/ops_config_item.go  ConfigItem    —— 配置项身份 + 当前指针。
--   * ops_config_version model/ops_config_version.go ConfigVersion —— 一次发布的不可变快照。
--   * ops_rollout_rule   model/ops_rollout_rule.go RolloutRule   —— 放量决策的证据行。
--
-- 值为什么不在 ops_config_item 上：
--   本表是**发布式**配置 —— 值只存在于 ops_config_version，item 行只承载「身份 + 当前正式版本指针」。
--   于是「改值」必然产生新行，历史永不被改写，回滚=再发布一个新版本号（change_type=rollback、
--   rollback_from 指向被回到的那个版本），版本序列单调递增，事后能回答
--   「此刻线上跑的是哪一次改动、谁改的、为什么改」。
--
-- 只读投影（本期结论，写在 services/ops-config/README.md）：
--   * ops_config_item.latest_version 与 ops_config_version 都是「指针」：
--     latest_version 的权威值可由
--     `SELECT MAX(version) FROM ops_config_version WHERE config_id = ?`（取已发布序列）重算；
--     epoch 只是缓存代次，丢失最多让调用方多回源一次，不改变任何判定结果。
--   * 因此本服务不把 Redis 里的解析结果当事实源，也不建任何计数表：
--     专题/坑位/开关的运行时视图都是可从这 8 张表整域重建的投影。
--   * 反过来说：**版本表不是投影**，它是事实源，任何清理都必须先归档（见本服务 README 缺口）。
--
-- 唯一键即幂等手段（model 侧全部用 INSERT ... ON DUPLICATE KEY UPDATE <无操作> + RowsAffected 判定，
-- 不依赖驱动专有错误码）：
--   1. uniq_key_scope(cfg_key, scope)：一个「键 + 生效范围」只能有一项。
--      PublishConfig 以 expect_version 做乐观锁，而 expect_version 比的是 latest_version 列，
--      所以这个唯一键同时也是「两个人同时新建同一个键」时唯一的仲裁点（冲突 → ErrConfigExists）。
--   2. uniq_config_version(config_id, version)：版本号在同一配置项内唯一。
--      新版本号取自 MAX(version)+1 而不是 count+1，就是为了在并发下让这个键成为仲裁点；
--      键同时服务 FindOne / FindLatest（ORDER BY version DESC）/ MaxVersion，不再另建索引。
--   3. uniq_request_id(request_id)：一次请求只产出一个版本行。
--      gRPC 重试 / 调用方重发都会命中同一行，logic 先 FindByRequestID 回查、命中即 reused=true。
--      该列 NOT NULL 且无默认值：默认空串会让第二行开始永远撞键，等于把「忘记传幂等键」
--      变成一条难以理解的生产故障。model 侧 Insert 对空 request_id 直接 ErrRequestIDRequired。
--   4. uniq_config_version_name(config_id, version, name)：SaveRolloutRule 的 upsert 本体。
--      name 是规则的幂等句柄，所以它是必填项而不是注释；没有这个键，
--      「保存规则」就会每次点一次多一行，优先级也就失去意义。
--
-- 索引全部取自真实查询路径（各 model 文件里的 SQL）：
--   * ops_config_item.idx_scope_state(scope, state, cfg_key)：ListConfigs 的
--     `WHERE scope = ? AND state = ? ... ORDER BY cfg_key ASC, scope ASC LIMIT ? OFFSET ?`
--     —— 索引列序与 WHERE/ORDER BY 一致，避免 filesort。
--     keyword 的 `(cfg_key LIKE ? OR title LIKE ?)` 不建索引：OR 跨两列时用不上任何一个，
--     且本表是千级配置项，加两个 LIKE 索引只会让每次发布多写两份维护成本。
--   * ops_config_item.idx_state(state, config_id)：BumpAllEpoch 的 `WHERE state = ?`
--     （RefreshCache target=all 只 bump 启用项，不给停用项制造缓存风暴）。
--   * ops_rollout_rule.idx_config_state(config_id, state, priority)：ListCandidates 的
--     `WHERE config_id = ? AND state = ? ORDER BY priority ASC, rule_id ASC LIMIT ?`。
--     InnoDB 二级索引自带主键后缀，所以 priority 之后自然按 rule_id 有序，排序不需 filesort；
--     时间窗（start_at=0 OR start_at<=? / end_at=0 OR end_at>?）是 OR 条件，不放进索引，
--     由 SQL 侧过滤掉过期行后再在 Go 侧用 model.EffectiveAt 复核同一语义。
--   * ops_config_version 不建 (config_id, published_at) 索引：所有历史查询都以 config_id 为锚点
--     并按 version 排序，published_at 排序在同一秒内不确定，不能作为次序依据。
--
-- 与 operation 服务的边界（**待维护者裁决**，本服务不擅自改他人文件）：
--   operation 已有 op_config(cfg_key/cfg_value/value_type/scope/version/state/operator/remark)，
--   且 gateway/admin 当前的 SaveOpsConfig/GetOpsConfig 走的是 operation。
--   本表与之的差别不是字段而是语义：op_config 是「就地改值的单行配置」，
--   本表是「每次改动留一行不可变快照 + 灰度规则 + 可回滚」。
--   详见 services/ops-config/README.md 的所有权结论（含两套并存与收敛两个方向的代价）。
--
-- 商业化范围外（AGENTS.md §1、§7）：本目录不存在广告位、投放、出价、排期购买、
-- 计费、分成、会员相关列；终端范围仅 Android/iOS/HarmonyOS/桌面端，不含小程序。
--
-- owner：平台治理（ops-config 服务）；影响范围：新增 3 张表，不改任何既有表。
-- 回滚：
--   DROP TABLE IF EXISTS `ops_rollout_rule`;
--   DROP TABLE IF EXISTS `ops_config_version`;
--   DROP TABLE IF EXISTS `ops_config_item`;
--   警告：ops_config_version 是发布与回滚的唯一历史事实源，DROP 等于销毁回滚能力，
--   只能在全新环境执行；已入库环境必须先导出全表再动手。
-- 锁风险：
--   * 全部为新建空表，不锁既有表，不加外键（跨库引用只存 ID，AGENTS.md §5）。
--   * PublishConfig 的事务只覆盖「插版本行 → 条件 UPDATE 推 item.latest_version/epoch」两条语句，
--     持锁时间是毫秒级；**严禁在该事务里做外部 RPC（含 audit）**，否则审计抖动会把发布一起拖住。
--   * BumpAllEpoch 是一次 `UPDATE ... WHERE state = ?` 的宽更新，可能锁到较多行：
--     它只由 RefreshCache(target=all) 手工触发，属运维动作而非请求路径，已在 README 标注使用约束。
-- =====================================================================

-- 配置项主表：只承载「身份 + 当前正式版本指针」，值在 ops_config_version。
CREATE TABLE IF NOT EXISTS `ops_config_item` (
  `config_id`      BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键；灰度规则与版本快照都挂在它下面，跨表引用用的就是这一列',
  `cfg_key`        VARCHAR(64)  NOT NULL COMMENT '配置键，格式 ^[a-z0-9_.]{1,64}$（model.cfgKeyRe）；会进 Redis 键与灰度分桶哈希，因此禁止大写与空格',
  `scope`          VARCHAR(16)  NOT NULL DEFAULT 'global' COMMENT '生效范围：global 或端标识 android/ios/harmony/desktop（model.ValidScope）；空串在写入前归一为 global',
  `value_type`     TINYINT      NOT NULL DEFAULT 1 COMMENT '值类型：1 string、2 int、3 bool、4 json；发布时按它校验值，类型本身不可事后偷改',
  `title`          VARCHAR(128) NOT NULL DEFAULT '' COMMENT '中文名，后台列表展示与关键字匹配用',
  `description`    VARCHAR(512) NOT NULL DEFAULT '' COMMENT '说明：这个键影响什么、改了会怎样。缺了它半年后没人敢改，这是配置表最便宜的一列',
  `state`          TINYINT      NOT NULL DEFAULT 1 COMMENT '1 启用、2 停用；停用的键在 ResolveConfig 按「未命中」处理，而不是报错',
  `latest_version` BIGINT      NOT NULL DEFAULT 0 COMMENT '当前正式版本号，0 表示从未发布。**只读投影**：权威值可由版本表 MAX(version) 重算；同时是发布乐观锁基线（expect_version 比它）',
  `epoch`          BIGINT       NOT NULL DEFAULT 0 COMMENT '缓存代次：每次发布与每次 RefreshCache +1，随响应回给调用方作为本地缓存失效依据；同为投影，丢了只多回源一次',
  `operator_id`    BIGINT       NOT NULL DEFAULT 0 COMMENT '最后操作人 admin_id（引用 operation 的管理员主键，不复制姓名与权限）',
  `ctime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT       NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）',
  PRIMARY KEY (`config_id`),
  UNIQUE KEY `uniq_key_scope` (`cfg_key`, `scope`),
  KEY `idx_scope_state` (`scope`, `state`, `cfg_key`),
  KEY `idx_state` (`state`, `config_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='运营配置项主表（发布式配置的身份与当前版本指针；值与历史在 ops_config_version）';

-- 版本快照表：追加式。唯一的 UPDATE 是 SetAuditEntry，且只补写 audit_entry_id 这一列引用。
CREATE TABLE IF NOT EXISTS `ops_config_version` (
  `version_id`     BIGINT      NOT NULL AUTO_INCREMENT COMMENT '自增主键',
  `config_id`      BIGINT      NOT NULL COMMENT '所属配置项（ops_config_item.config_id）；不建外键，靠同库同服务写入保证',
  `version`        BIGINT      NOT NULL COMMENT '版本号，同配置项内单调递增；新值取 MAX(version)+1，回滚也走这个规则（所以序列永不倒退）',
  `cfg_value`      TEXT        NOT NULL COMMENT '本次发布的值快照。列名用 cfg_value 以避开 SQL 里 VALUE/VALUES 的读写歧义。TEXT 与 model.MaxCfgValueBytes(65535) 同宽，真正的上限由代码（OpsValue.MaxBytes）守住：值会进 Redis、进网关响应、进审计摘要，超上限是拒绝而非截断',
  `value_type`     TINYINT     NOT NULL DEFAULT 1 COMMENT '值类型快照（与 config_item.value_type 同值域）。冗余一份是为了让「按 v3 解释这段值」不依赖当前行的可变状态',
  `change_type`    VARCHAR(16) NOT NULL COMMENT 'create / publish / rollback（model.ChangeType*；Insert 侧硬校验，其它值 → ErrChangeTypeInvalid）',
  `rollback_from`  BIGINT      NOT NULL DEFAULT 0 COMMENT '回滚来源版本号，0 表示非回滚。回滚不改写历史：它是「把历史值再发布一次」，因此本列是回滚可追溯的唯一线索',
  `operator_id`    BIGINT      NOT NULL DEFAULT 0 COMMENT '发布人 admin_id',
  `operator_name`  VARCHAR(64) NOT NULL DEFAULT '' COMMENT '发布人展示名快照（改名不追改，事后排障才能看清当时是谁）',
  `reason`         VARCHAR(512) NOT NULL COMMENT '变更原因，契约层必填（model.ErrReasonRequired）。超长由代码按 OpsValue.MaxReasonLen 拒绝而非截断：理由参与审计摘要，截断会让摘要与库值不一致',
  `audit_entry_id` BIGINT      NOT NULL DEFAULT 0 COMMENT 'audit.audit_entry 引用，由本服务写入后回填。0 表示审计待补偿 —— 宁可留 0 让缺口可见，也不伪造一个 ID',
  `request_id`     VARCHAR(64) NOT NULL COMMENT '幂等键 <request_id>（写入方必须给，空 → ErrRequestIDRequired）；唯一键即「一次请求只产出一个版本行」',
  `published_at`   BIGINT      NOT NULL DEFAULT 0 COMMENT '发布时间（Unix 秒），0 时由 model 填当前时间。只作展示与巡检，不作为次序依据（同一秒可能有两个版本，见 FindLatest 按 version 排序）',
  `ctime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '入库时间（Unix 秒）',
  PRIMARY KEY (`version_id`),
  UNIQUE KEY `uniq_config_version` (`config_id`, `version`),
  UNIQUE KEY `uniq_request_id` (`request_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='配置版本快照表（不可变、追加式；发布与回滚的历史事实源，回滚=再发布新版本号）';

-- 灰度规则表：放量决策的证据。收口靠 state 软切换，不靠删除行。
CREATE TABLE IF NOT EXISTS `ops_rollout_rule` (
  `rule_id`         BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键；SetRolloutRuleState 的定位键',
  `config_id`       BIGINT       NOT NULL COMMENT '挂在哪一个配置项上（ops_config_item.config_id）',
  `version`         BIGINT       NOT NULL COMMENT '规则指向的版本号：必须是本次或已发布的版本，指向不存在的版本等于把不存在的值推给线上',
  `name`            VARCHAR(64)  NOT NULL COMMENT '规则名，(config_id, version, name) 内的幂等句柄（必填 → ErrRuleNameRequired）；改名等于新建一条规则，不是重命名',
  `mode`            TINYINT      NOT NULL COMMENT '放量模式：1 FULL、2 PERCENTAGE、3 APP_VERSION、4 PLATFORM、5 MID_SUFFIX、6 WHITELIST（proto RolloutMode，model.Mode*）。mode 与实际填写的维度必须自洽（ValidateRuleShape → ErrRuleModeMismatch）',
  `percentage`      INT          NOT NULL DEFAULT 0 COMMENT '百分比放量 0..100，按 crc32(IEEE)(cfg_key + ":" + mid) % 100 分桶（model.PercentageBucket，桶数 model.BucketCount）。混入 cfg_key 是为了让不同键的同一用户不总是同时被放量；mid<=0（未登录）不参与分桶，返回 -1 即不命中',
  `app_version_min` VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '版本区间下界（闭区间），空表示不限。比较按点号逐段进行（model.CompareAppVersion），**永不做字符串序**：否则 "1.10.0" 会小于 "1.9.0"',
  `app_version_max` VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '版本区间上界（闭区间），空表示不限；与下界倒挂即拒（ErrAppVersionRangeInvalid）',
  `platforms`       VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '生效端列表，存储形态 ",1,2,"（值域来自 proto ClientPlatform：1 android/2 ios/3 harmony/4 desktop）。Go 侧一律 int32，转换点只有 model/platform.go 四处；空串表示不限端',
  `mid_suffixes`    VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'mid 尾号放量，存储形态 ",0,7,"，最多 model.MaxMidSuffixes(10) 个；尾号是分桶之外的稳定抽样，同一批人要么长期看得到要么长期看不到',
  `whitelist_mids`  TEXT         NOT NULL COMMENT '白名单 mid，存储形态 ",123,456,"，上限 model.MaxWhitelistMids(200) 且可被 Rollout.MaxWhitelistMids 收紧。白名单是排障工具不是放量手段：所以它是 TEXT 却永远不该变大',
  `priority`        INT          NOT NULL DEFAULT 100 COMMENT '命中次序，小者先（ListCandidates 按它排序）。同优先级按 rule_id 升序，保证多条规则并列时判定结果确定',
  `state`           TINYINT      NOT NULL DEFAULT 2 COMMENT '1 生效、2 停用。新建默认 2：「写规则」与「开闸」是两步，必须再显式 SetRolloutRuleState 才生效',
  `operator_id`     BIGINT       NOT NULL DEFAULT 0 COMMENT '最后操作人 admin_id',
  `remark`          VARCHAR(255) NOT NULL DEFAULT '' COMMENT '备注：为什么放这一档、预期收口时间',
  `start_at`        BIGINT       NOT NULL DEFAULT 0 COMMENT '生效窗口起（Unix 秒），0 表示不限',
  `end_at`          BIGINT       NOT NULL DEFAULT 0 COMMENT '生效窗口止（Unix 秒），0 表示不限；end_at<=start_at 且两者非 0 即拒（ErrRuleTimeRangeInvalid）—— 那是一条永远不生效的静默配置',
  `ctime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`           BIGINT       NOT NULL DEFAULT 0 COMMENT '最后修改时间（Unix 秒）',
  PRIMARY KEY (`rule_id`),
  UNIQUE KEY `uniq_config_version_name` (`config_id`, `version`, `name`),
  KEY `idx_config_state` (`config_id`, `state`, `priority`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
  COMMENT='配置灰度规则表（放量证据：mode + 版本区间 + 端 + 尾号 + 白名单；收口靠 state 软切换，保留历史）';
