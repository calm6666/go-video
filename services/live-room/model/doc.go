// Package model 是 live-room 服务的数据库模型与查询代码（库 go_video_live_room）。
//
// 自有表（见 deploy/migrations/live-room，一张表一个文件）：
//   - live_room               直播间主体：状态、资料、当前场次投影、禁播到期时间
//   - live_room_setting       房间直播配置（1:1）：弹幕/回复/录制/连麦/直播类型
//   - live_room_anchor        主播绑定：房主 / 联合主播 / 房管（软解绑保留行）
//   - live_session            直播场次：状态、开播快照、流事件序号、回放引用
//   - live_room_ban           禁播记录：临时/永久，含解除留痕（审计证据不物理删除）
//   - live_area               直播分区（运营侧维护）
//   - live_room_state_log     状态流转日志：房间/资料/场次/回放四类状态迁移留痕
//   - live_room_idempotency   幂等与事件去重：request_id 与 event_id 共用一套唯一键
//
// 跨服务不得直连本库（AGENTS.md §5）：写只能经 rpc.LiveRoom 的带幂等键入口，
// 房间状态只能由本包的状态机矩阵（errors.go）校验后推进。
//
// 约定：
//   - 所有状态迁移都是「条件 UPDATE + RowsAffected 判定」，返回 (false, nil) 表示
//     并发下状态已被他人推进，调用方必须重读或按 ErrConcurrentUpdate 退出，
//     不允许「先 SELECT 再无条件 UPDATE」这种竞态写法。
//   - 所有列表查询强制带 LIMIT，且 LIMIT 由调用方夹到 config 上限。
//   - 时间列一律 BIGINT Unix 秒，取值统一走 nowUnix()（now.go 提供可注入时钟）。
package model
