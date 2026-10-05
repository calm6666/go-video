-- =====================================================================
-- feature-store 服务 - 写请求执行权与幂等回执表
-- =====================================================================
-- 用途：建立 feature_write_receipt，一行 = 一个 (request_id, op_type) 写请求的
--   执行权、最小可回放响应快照与收尾结果。契约里 8 个写方法共用这一张表：
--   register / state_change / privacy_change / write / switch / backfill / purge / erase
--   （model.ReceiptOp* 常量，ValidReceiptOp 白名单）。
-- 数据所有者：feature-store 服务（AGENTS.md §5）。
--
-- 数据库：go_video_feature_store。
--
-- 为什么需要这张表，而不是「靠各业务表的唯一键天然幂等」（AGENTS.md §5 要求
-- 所有写接口有幂等键、状态版本或唯一约束）：
--   · SwitchFeatureVersion 的副作用是「一行指针更新 + 一条审计」，两者都没有能承接
--     「这个 request_id」的列，重放时无法判断该不该再切一次；
--   · WriteFeatures 是整批语义，逐行唯一键只能防单行重复，防不了「整批重放一次」
--     造成的 written 计数翻倍；
--   · 更关键的是并发：两条同 request_id 的请求同时通过业务表检查，会各自留下一条
--     互相矛盾的审计。执行权必须先落到一行上，再干活。
--   使用协议固定三步，缺一步就是错的：Begin → 执行操作 → MarkDone / MarkFailed。
--
-- 幂等与并发：
--   · UNIQUE KEY uniq_request_op (request_id, op_type)：op_type 参与唯一键而不是只进日志，
--     因为「同一 request_id 在不同接口间串味」是最难查的一类幂等事故
--     （注册请求的回放被当成切换请求的回放）。
--   · Begin 用 INSERT ... ON DUPLICATE KEY UPDATE mtime = mtime 探测（RowsAffected=0 即已存在），
--     再按三种既有状态分别处置：done → 回放（Execute=false）；failed → 重置 in_progress 接手
--     （失败未产生副作用，同 request_id 重试本就是幂等语义的一部分）；
--     in_progress 且租约未过期 → ErrReceiptInProgress（必须报错而不是「顺手再执行一遍」，
--     并发改两次执行会写双份行、留两条矛盾审计）；in_progress 且租约已过期 → 接管。
--     接管用条件更新（WHERE state=? [AND lease_expire_at<=?]）关掉「读-改-写」窗口，
--     否则两个接管者会同时拿到执行权。
--   · request_digest 让「同一 request_id 换了内容」直接判 ErrRequestIdReused：
--     幂等回放会给出错误结果，比报错危险。摘要只覆盖决定副作用的字段，
--     trace_id 这类链路元数据不进摘要（否则同一次请求换个 trace_id 重放就变成「不同请求」）。
--
-- 保留期与回收：
--   本表随写请求量线性增长，是唯一带「保留期清理」路径的元数据表：
--   SelectAgedIDs（终态 + mtime < before，走 idx_state_mtime）先选主键，
--   DeleteByIDs 再按主键批删 —— 不做范围 DELETE，
--   范围锁会堵住同 request_id 前缀上的在线写。
--   幂等窗口（配置 ReceiptRetentionSeconds）必须显著大于上游的最大重试跨度，
--   删早了等于作废幂等键：上游重试会拿到「新执行一遍」的结果。
--
-- 隐私与脱敏：
--   · result_json 只存「重放这次请求所需的最小事实」（计数、ID、版本、行级短码），
--     上限 MaxReceiptResultBytes=2048；超限直接拒绝写入而不是悄悄截断。
--     它不是响应缓存，也不允许出现特征值原文；
--   · error_code 是哨兵短码，不含 SQL 与特征值原文；
--   · entity_id 只存受控形态（主键十进制串或加盐哈希摘要），明文标识禁止出现在这一列；
--   · detail_kept=0 时逐行明细不入库，但 result_digest 仍在：回放时能如实告诉调用方
--     「首次请求的结果就是这份，只是本表没存逐行明细」，而不是伪造一份明细。
--
-- 事实定位：控制位 + 幂等台账，不是业务事实。
--   可由各业务表的 request_id 列（feature_value.write_request_id、
--   feature_backfill_job.request_id、feature_version_switch.request_id）反推出「谁做过什么」，
--   但推不出首次响应的回放内容与执行权状态 —— 删表的后果是重放请求会被再执行一遍。
--
-- 索引：
--   · uniq_request_op (request_id, op_type)：Begin/Find/MarkDone/MarkFailed 的定位路径，
--     同时也是唯一约束本体（不存在与之重复的二级索引）；
--   · idx_state_mtime (state, mtime, receipt_id)：SelectAgedIDs 的
--     WHERE state IN (done,failed) AND mtime < ? ORDER BY receipt_id ASC；
--   · idx_state_lease (state, lease_expire_at)：CountStuck 统计
--     「拿了执行权却没收尾」的过期 in_progress 行数（cron 巡检报警：数量上升
--     说明有请求在收尾前崩溃）。崩溃的请求靠租约自动释放，不需要人工清行。
--
-- 回滚：
--   DROP TABLE IF EXISTS `feature_write_receipt`;
--   执行前必须确认没有 in_progress 行：删表会让所有正在执行的写请求收尾失败
--   （MarkDone 条件不命中），且此后任何重放都会被当成新请求再执行一遍 ——
--   对 WriteFeatures 是双份行，对 SwitchFeatureVersion 是双份矛盾审计。
--   正确顺序：停写 → 等租约全部过期 → 再 DROP。
--
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可重复执行，不改动既有数据。
--   本表是每个写请求的第一条语句，锁持有时间 = 整个业务操作的时长（Begin 到 MarkDone
--   不在同一事务内，靠租约兜底），因此 lease_owner 必填：没有持有者的回执
--   等于「谁都能收尾」，崩溃的请求也无法被判定为过期，request_id 会被永久占用。
--   后续变更必须新增 0000NN_*.sql，禁止修改本文件。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `feature_write_receipt` (
  `receipt_id`      BIGINT        NOT NULL AUTO_INCREMENT COMMENT '自增主键；保留期清理按它批删',
  `request_id`      VARCHAR(64)   NOT NULL COMMENT '调用方给的幂等键（上限 maxReceiptRequestIDLen=64，与 feature_version_switch.request_id 同口径）',
  `op_type`         VARCHAR(24)   NOT NULL COMMENT '操作类型：register/state_change/privacy_change/write/switch/backfill/purge/erase。参与唯一键，防止同一 request_id 在不同接口间串味',
  `feature_key`     VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '目标特征键（purge/erase 这类无 key 的操作留空串）',
  `version`         INT           NOT NULL DEFAULT 0 COMMENT '目标版本，0 = 不针对具体版本',
  `entity_scope`    SMALLINT      NOT NULL DEFAULT 0 COMMENT '主体类型，0 = 不针对具体主体',
  `entity_id`       VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '主体标识（受控形态：十进制主键或加盐哈希摘要），明文标识不允许出现在这一列',
  `row_count`       INT           NOT NULL DEFAULT 0 COMMENT '请求体量（WriteFeatures 的行数、Erase 的目标行数）：事后解释「这个 request_id 当初有多大一只鸟」',
  `request_digest`  VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '请求内容摘要（sha256，只覆盖决定副作用的字段）：同一 request_id 换了内容即判 ErrRequestIdReused',
  `digest_ver`      VARCHAR(32)   NOT NULL DEFAULT '' COMMENT '摘要算法版本（model.ReceiptDigestVersion），与特征定义摘要分开编号：两者序列化字段完全不同，共用版本号会在改其中一个时误伤另一个的判定',
  `state`           VARCHAR(16)   NOT NULL COMMENT '状态：in_progress / done / failed。用字符串而不是枚举数字——回执不进对外契约，运维直接读这列时 in_progress 比 2 有用',
  `result_json`     VARCHAR(2048) NOT NULL DEFAULT '' COMMENT '首次成功响应的最小快照（计数/ID/版本/行级短码），上限 MaxReceiptResultBytes=2048，超限拒绝写入而非截断。不含特征值原文',
  `result_digest`   VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '完整首次响应的 sha256：明细未入库时仍能证明「回放的是同一份结果」，而不是伪造一份明细',
  `detail_kept`     SMALLINT      NOT NULL DEFAULT 0 COMMENT '1 = result_json 足以完整回放；0 = 只存了汇总计数，回放必须明确告知明细不可重放',
  `affected_rows`   BIGINT        NOT NULL DEFAULT 0 COMMENT '本次操作影响的行数（written/purged/erased/switched 的统一起点）',
  `error_code`      VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '失败时的哨兵短码（MaxReceiptErrorCodeLen=64）：不含 SQL 与特征值原文',
  `operator`        VARCHAR(64)   NOT NULL COMMENT '调用方身份（必填）',
  `trace_id`        VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '链路 ID。刻意不进 request_digest：同一次请求换 trace_id 重放不该变成「不同请求」',
  `lease_owner`     VARCHAR(128)  NOT NULL COMMENT '执行权持有者 <pod 名>#<worker id>，Begin 强制非空：无主回执等于谁都能收尾，崩溃请求无法判过期。MarkDone/MarkFailed 只有持有者能改写',
  `lease_expire_at` BIGINT        NOT NULL DEFAULT 0 COMMENT '执行权租约到期时间（Unix 秒，上限 MaxReceiptLeaseSeconds=300）：进程崩溃后同一 request_id 可被重新取得，不会永久卡死',
  `ctime`           BIGINT        NOT NULL DEFAULT 0 COMMENT '首次受理时间（Unix 秒）',
  `mtime`           BIGINT        NOT NULL DEFAULT 0 COMMENT '最后更新时间（Unix 秒）；Begin 的 no-op 探测依赖它自等，保留期清理按它判据',
  `finished_at`     BIGINT        NOT NULL DEFAULT 0 COMMENT '进入 done/failed 的时间（Unix 秒），in_progress 为 0；接管重置回 in_progress 时清 0',
  PRIMARY KEY (`receipt_id`),
  UNIQUE KEY `uniq_request_op` (`request_id`, `op_type`),
  KEY `idx_state_mtime` (`state`, `mtime`, `receipt_id`),
  KEY `idx_state_lease` (`state`, `lease_expire_at`)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_unicode_ci
  COMMENT ='写请求执行权与幂等回执表（8 个写方法共用；控制位与幂等台账，有保留期清理，不承载业务值）';
