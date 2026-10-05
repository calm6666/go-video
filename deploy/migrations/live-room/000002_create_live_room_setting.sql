-- =====================================================================
-- live-room 服务 - 房间直播配置表 live_room_setting
-- =====================================================================
-- 用途：单个直播间的直播能力开关与直播类型。对应 RPC UpdateRoomSetting / GetRoom(with_setting)
--       / StartLive（决定是否通知 live-media 起录制）。
-- 数据所有者：live-room 服务（AGENTS.md §5）。
--       本表只存「房间是否允许录制」这个开关；回放产物、转码任务与媒资元数据
--       归 live-media / transcode / asset，本表一个字段都不复制。
-- 与 model 的对应：列顺序与 services/live-room/model/live_room_setting.go 的
--       liveRoomSettingColumns 逐列一致。
-- 基数与幂等：与 live_room 1:1，主键即 room_id（不是自增）。
--       LiveRoomSettingModel.Upsert 用 INSERT ... ON DUPLICATE KEY UPDATE，
--       其冲突键就是 PRIMARY KEY(room_id) —— 本表必须保持 room_id 为唯一主键，
--       再加任何唯一键都会让 UPSERT 命中错行（AGENTS.md §5「所有写接口要设计幂等键」）。
-- 覆盖语义：RoomSetting 的 bool 字段「为 false 表示显式关闭」，因此 Upsert 整段覆盖全部业务列，
--       本表不提供列级局部更新；这也是每列都必须 NOT NULL 且有默认值的原因 ——
--       「未配置」这个状态由「有没有这一行」表达（RecordEnabledFor 查不到行时返回 ErrNoSettingRow），
--       而不是由列值为 NULL 表达：NULL 与 0 在 int32 读写上语义不同，留 NULL 会逼着 model 给每列判空。
-- 布尔存储：刻意用 TINYINT 而不是 MySQL BOOLEAN/TINYINT(1) 别名，
--       与 model 的 int32 + BoolToInt32/Int32ToBool 转换保持单一读写类型。
-- 锁风险：仅 CREATE TABLE IF NOT EXISTS，可在线重复执行。
--       Upsert 是单行主键写入，持行锁时间极短；CreateRoom 里它与 live_room/live_room_anchor
--       同事务提交，不引入新的死锁面（事务内表访问顺序按 000001→000002→000003→000004 固定）。
--       本表没有列表扫描型查询（只有 FindOne/RecordEnabledFor 的主键点查），无无界扫描风险。
-- 回滚：DROP TABLE IF EXISTS `live_room_setting`;
--       回滚后 GetRoom(with_setting=true) 会拿不到配置行，StartLive 的录制开关退化为
--       ErrNoSettingRow 分支（调用方按服务端默认处理），不影响房间主体。
-- =====================================================================

CREATE TABLE IF NOT EXISTS `live_room_setting` (
  `room_id`                 BIGINT  NOT NULL COMMENT '房间 ID（主键，与 live_room 1:1；UPSERT 的幂等冲突键，禁止改成自增）',
  `danmaku_enabled`         TINYINT NOT NULL DEFAULT 1 COMMENT '弹幕开关：1 开启、0 显式关闭（弹幕内容与屏蔽词归 danmaku 服务，本列只是房间侧许可）',
  `reply_enabled`           TINYINT NOT NULL DEFAULT 1 COMMENT '回复/评论开关：1 开启、0 显式关闭（评论数据归 comment 服务）',
  `record_enabled`          TINYINT NOT NULL DEFAULT 1 COMMENT '录制回放开关：1 允许录制、0 关闭；开播时据此决定是否通知 live-media 起录制',
  `linkmic_enabled`         TINYINT NOT NULL DEFAULT 0 COMMENT '连麦开关：1 允许、0 关闭（连麦嘉宾的绑定关系在 live_room_anchor，默认关闭以免新房间被自动开放连麦）',
  `live_type`               TINYINT NOT NULL DEFAULT 1 COMMENT '直播类型：1 视频、2 语音、3 屏幕分享；取值由 model.ValidLiveType 校验，越界写入直接返回 ErrSettingInvalid',
  `min_client_version_code` INT     NOT NULL DEFAULT 0 COMMENT '允许的最低客户端版本号（0 不限制）。多端各用自己的版本编码，服务端不按端写死逻辑（AGENTS.md §6）',
  `ctime`                   BIGINT  NOT NULL DEFAULT 0 COMMENT '创建时间（Unix 秒）',
  `mtime`                   BIGINT  NOT NULL DEFAULT 0 COMMENT '修改时间（Unix 秒）；Upsert 每次都刷新，是「配置何时被改」的唯一线索（本表不留历史版本）',
  PRIMARY KEY (`room_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='房间直播配置（与 live_room 1:1；整段覆盖式 UPSERT，无列级局部更新）';
