-- =====================================================================
-- live-room 服务 - 直播分区表 live_area
-- =====================================================================
-- 用途：直播频道的分区树（运营维护，客户端分区页与选区用）。
--       对应 RPC UpsertArea / ListAreas，以及 CreateRoom / UpdateRoomInfo 的
--       「分区必须存在且启用」校验（LiveAreaModel.IsUsable）。
-- 数据所有者：live-room 服务（AGENTS.md §5）。本表是「直播分区」这一命名空间的唯一所有者。
-- 与 catalog / ops-config 的关系（待维护者裁决，本轮不做任何迁移或合并）：
--       catalog 库已有 catalog_zone（表注释「内容分区表」，列 zoneid/name/parent，
--       语义是版权内容的投稿分区），ops-config 有 ops_topic/ops_recommend_slot 等运营投放配置。
--       三者是同一套「分区」概念还是三个不同域，属跨服务数据所有权问题，
--       已原样登记在服务 README 与本轮交付汇报中；本文件既不引用也不复制它们的行。
-- 与 model 的对应：列顺序与 services/live-room/model/live_area.go 的
--       liveAreaColumns 逐列一致。
-- 幂等与不变量依赖的唯一键：
--       uniq_area_name (area_name)
--           UpsertArea 的「分区名全局唯一」约束：Insert 与 Update 撞重复键时由
--           model.isDuplicateErr 翻译成 ErrAreaNameConflict，FindByName 直接按它点查。
--           这是本表唯一的数据库级不变量，必须显式建出，不能靠 logic 先查后插
--           （并发下两次同名创建会都通过预检）。
--           作用域是「全局唯一」而非「同父唯一」，与 model 注释和 rpc.AreaInfo 契约一致；
--           若将来要放开成同父唯一，属契约变更，需改唯一键为 (parent_area_id, area_name)
--           并同步改 model 的错误翻译。
-- 层级与删除：两级分区（一级父 + 二级子），禁止三级嵌套，由 model.LevelOf 判定并返回
--       ErrAreaParentInvalid（父不存在或父本身还有父）。本表不提供物理删除：
--       停用是 state=0，行保留，否则历史房间与场次快照的 area_id 会变成悬空引用。
--       因此 area_id 是 AUTO_INCREMENT 且永不复用。
-- 停用前置检查：停父分区前 logic 必须查 CountChildren（本表）与 CountByArea（live_room，见 000001），
--       任一有占用即返回 ErrAreaInUse。跨表检查不建外键，靠调用顺序保证（先查子、再查房间、最后写）。
-- 时间列：ctime/mtime 为 BIGINT Unix 秒，由 model.nowUnix() 提供，不用 DB 时钟。
-- 索引依据（model 实际用到的访问路径）：
--       PRIMARY(area_id)      FindOne / Update / IsUsable / LevelOf 的行定位
--       uniq_area_name        Insert/Update 冲突键 + FindByName
--       idx_parent_sort       List 的 "WHERE parent_area_id=? ORDER BY parent_area_id,sort,area_id"
--                             与 CountChildren(parent_area_id=?)；索引尾部的 area_id 正好补全第三排序键
--       刻意不给 state 单建索引：分区是运营维护的小表（量级几十到几百行），
--       ListAreas 的 state 过滤在 idx_parent_sort 之后残余过滤即可，加索引只增加写放大。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可在线重复执行。
--       UpsertArea 是单行 INSERT/UPDATE，行锁极短；同名人并发创建由 uniq_area_name 收敛。
--       读侧（客户端分区页）命中率高，生产应在 CacheRedis 放分区树缓存
--       （config.LiveRoom.AreaListCacheTTLSeconds，默认 300 秒），
--       UpsertArea 成功后必须失效该缓存，否则新分区要等 TTL 才可选。
-- 回滚：DROP TABLE IF EXISTS `live_area`;
--       回滚后 CreateRoom/UpdateRoomInfo 的分区校验（IsUsable）失去依据，房间表 area_id
--       与场次表 area_id_snapshot 变成无解释的整数；只允许连同 000001 一起整体回退。
--       初始分区数据由运营在后台逐条 UpsertArea 建立，本迁移不预置种子行
--       —— 预置会在多环境产生不可复现的名称冲突。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_area` (
  `area_id`        BIGINT      NOT NULL AUTO_INCREMENT COMMENT '分区 ID（主键，永不复用；房间表与场次快照只存这个 ID，不冗余分区名）',
  `area_name`      VARCHAR(32) NOT NULL COMMENT '分区名（按 rune 计 1~32，与 model.AreaNameMaxRunes 和 config.LiveRoom.AreaNameMaxLength 同宽；全局唯一，由 uniq_area_name 强制）',
  `parent_area_id` BIGINT      NOT NULL DEFAULT 0 COMMENT '上级分区 ID，0 表示一级分区；层级上限两级（model.LevelOf 校验，禁止三级嵌套），不提供物理删除故父行只会停用',
  `sort`           INT         NOT NULL DEFAULT 0 COMMENT '排序权重（越小越前）；同父分区内按 (sort, area_id) 升序渲染，客户端分区页与 ListAreas 同口径',
  `state`          TINYINT     NOT NULL DEFAULT 1 COMMENT '启停位：1 启用、0 停用。停用后不出现在客户端列表且不能被新房间选用（IsUsable），但历史房间与场次快照仍保留引用',
  `operator_mid`   BIGINT      NOT NULL DEFAULT 0 COMMENT '最近一次操作的运营 ID，必须 > 0（model.ErrOperatorRequired）；本表不留变更前后镜像，审计靠 operation 域的操作日志',
  `ctime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`          BIGINT      NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）；Update 无条件刷新，即使只改 sort',
  PRIMARY KEY (`area_id`),
  UNIQUE KEY `uniq_area_name` (`area_name`),
  KEY `idx_parent_sort` (`parent_area_id`, `sort`, `area_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='直播分区（两级树、软停用、分区名全局唯一；与 catalog 的版权内容分区是两个独立命名空间）';
