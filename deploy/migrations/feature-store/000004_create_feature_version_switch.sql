-- =====================================================================
-- feature-store 服务 - 特征变更审计表（只追加，不改写不删除）
-- =====================================================================
-- 用途：建立 feature_version_switch，一行 = 一次「改变了对外可读口径」的动作。
--   六类变更共用这一张表：activate（首次上线，from_version=0）、version_switch（人工切版本）、
--   rollback（回滚，必须引用被回滚的 switch_id）、state_change（定义状态变更）、
--   privacy_change（隐私级别调整）、backfill_auto_switch（回填成功后自动切换）。
--   不拆成六张表是因为运维排查的问句只有一个：「这个 key 现在为什么是这个行为」——
--   按 feature_key 一次倒序扫描就能看到全部原因，拆表等于把这个问句变成六次查询。
-- 数据所有者：feature-store 服务（AGENTS.md §5）。
--
-- 数据库：go_video_feature_store。
--
-- 列分工（同一张表承载两类审计，靠 switch_type 区分语义）：
--   · 版本类（activate/version_switch/rollback/backfill_auto_switch）：
--     from_version -> to_version 是生效指针的移动，from_value/to_value 留空；
--   · 定义类（state_change/privacy_change）：
--     from_version = to_version = 被改的版本，from_value/to_value 记改前改后的枚举值。
--   这样一张表既能回答「现在哪个版本生效、是怎么来的」，也能回答
--   「这个特征的隐私级别什么时候被人动过」。契约的 ListVersionSwitches 只对外暴露
--   版本相关列（rpc SwitchRecord），元数据列留在库内做内部审计。
--
-- 幂等与并发：
--   · UNIQUE KEY uniq_request_switch (request_id, switch_type)：同一 request_id 重放同一种
--     变更时 Append 命中 1062 → model 转 ErrRequestIdReused，logic 必须回放首次结果，
--     而不是把审计写第二遍（AGENTS.md §5 幂等要求）。
--     键里带 switch_type 而不是只用 request_id：一次请求在语义上可能同时留下
--     「切换 + 状态变更」两条不同性质的审计，只按 request_id 唯一会把第二条判成冲突；
--     而「同一请求重放同一类变更」才是真正要拦的事故。
--   · request_id 列宽与 feature_write_receipt 同口径（64），两者对齐才可能互相核对。
--
-- 只追加的纪律：
--   本表没有 mtime 列，也不提供任何 UPDATE/DELETE 方法（VersionSwitchModel 只有
--   Append/FindOne/List/LatestByKey）。回滚的表示方式是「再写一条 rollback 审计 +
--   把指针切回去」，绝不是删掉那条切换记录 —— 删审计等于抹掉「曾经切错过」的事实，
--   下一次评审就会重复同一个错误（AGENTS.md §8 要求下架/撤回保留审计证据）。
--   privacy_level 调整必须单独留痕（契约把 UpdateFeaturePrivacy 做成独立入口的原因：
--   它不改值语义，只改谁能读）。
--
-- 隐私与脱敏：
--   · reason 只允许写变更依据（离线评估结论、回滚单号、隐私工单号），
--     禁止粘贴特征值原文或主体标识明文；
--   · trace_id 不含任何主体标识；operator 是身份串（admin:<id> / system:<svc> /
--     offline-job:<id>），不是邮箱、手机号；
--   · from_digest / to_digest 存的是 feature_definition.definition_digest（sha256），
--     不可逆，只用于事后复算口径是否一致。
--
-- 事实定位：不可重算的事实源（与 feature_value 相反）。
--   本表是「为什么变成现在这样」的唯一证据链，任何重算流程都不覆盖它。
--
-- 索引：
--   · idx_key_switch (feature_key, switch_id)：ListVersionSwitches 的
--     WHERE feature_key = ? ORDER BY switch_id DESC，以及 LatestByKey。
--     自增主键即时间序，因此倒序分页天然有序、不 filesort；
--   · idx_ctime (ctime, switch_id)：只带 since/until 的全量审计翻页
--     （不带 feature_key 时无法用 idx_key_switch 的最左列）；
--   · 不为 switch_type 单建索引：它是低基数列（6 个值），单独命中它会扫全表；
--     与 feature_key 或 ctime 组合时上面两条索引已能收敛，
--     再加一条只会让这条只追加的高写入路径多维护一棵树。
--
-- 回滚：
--   DROP TABLE IF EXISTS `feature_version_switch`;
--   本表**不可重算**：删掉就永久失去「谁在什么时候为什么切了版本 / 调了隐私级别」的证据，
--   feature_active_version.last_switch_id 也会同时悬空（当前指针无从解释）。
--   因此本文件的回滚语句在生产上应视为禁止操作：执行前必须离线备份并走变更评审。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改动既有数据。
--   纯 INSERT 路径，无 UPDATE/DELETE，因此本表自身不产生长事务；
--   但它与 feature_active_version 的更新在同一事务内提交，
--   持锁时长由切换事务决定 —— 审计行必须放在事务的最后写，
--   避免「审计已落、指针校验还要扫值」把行锁窗口拉长。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `feature_version_switch` (
  `switch_id`          BIGINT       NOT NULL AUTO_INCREMENT COMMENT '自增主键，同时是 feature_active_version.last_switch_id 的指向目标；自增即时间序，倒序分页不 filesort',
  `feature_key`        VARCHAR(64)  NOT NULL COMMENT '被变更的特征键',
  `switch_type`        VARCHAR(24)  NOT NULL COMMENT '审计类型：activate / version_switch / rollback / state_change / privacy_change / backfill_auto_switch（model.ValidSwitchType 白名单）',
  `from_version`       INT          NOT NULL DEFAULT 0 COMMENT '变更前生效版本（0 = 之前没有生效版本）；定义类审计时 = 被改的版本',
  `to_version`         INT          NOT NULL DEFAULT 0 COMMENT '变更后生效版本；定义类审计时等于 from_version',
  `from_value`         VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '变更前枚举值（状态/隐私类审计用，记 FeatureState / PrivacyLevel 数值），空串 = 不适用',
  `to_value`           VARCHAR(32)  NOT NULL DEFAULT '' COMMENT '变更后枚举值',
  `from_digest`        VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '变更前 feature_definition.definition_digest（sha256，不可逆）：事后复算口径是否一致',
  `to_digest`          VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '变更后定义摘要',
  `operator`           VARCHAR(64)  NOT NULL COMMENT '操作人：admin:<id> / system:<svc> / offline-job:<id>（backfill_auto_switch 必须是 offline-job:*）。必填，无主审计不成立',
  `reason`             VARCHAR(512) NOT NULL COMMENT '变更理由（离线评估结论、回滚单号、隐私工单号）：必填。这一列的存在意义就是解释「为什么变」，理由缺失的审计行等于没留痕。禁止粘贴特征值原文',
  `request_id`         VARCHAR(64)  NOT NULL COMMENT '触发本次变更的幂等键（与 feature_write_receipt 同口径，两者可互相核对）',
  `trace_id`           VARCHAR(64)  NOT NULL DEFAULT '' COMMENT '链路 ID（不含任何主体标识明文）',
  `rollback_switch_id` BIGINT       NOT NULL DEFAULT 0 COMMENT '仅 switch_type=rollback 使用：被回滚掉的那条 switch_id。让「切错了又切回去」在审计里成对出现，而不是伪装成一次普通切换；引用不存在的 id 会被 ErrSwitchNotFound 拒绝，避免审计链断裂',
  `ctime`              BIGINT       NOT NULL DEFAULT 0 COMMENT '变更时间（Unix 秒）。本表无 mtime：只追加，不改写',
  PRIMARY KEY (`switch_id`),
  UNIQUE KEY `uniq_request_switch` (`request_id`, `switch_type`),
  KEY `idx_key_switch` (`feature_key`, `switch_id`),
  KEY `idx_ctime` (`ctime`, `switch_id`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='特征口径变更审计表（只追加不改写；版本切换/回滚/状态/隐私留痕的唯一证据链，不可重算）';
