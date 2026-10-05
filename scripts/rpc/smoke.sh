#!/usr/bin/env bash
# 契约真源：services/*/rpc/*.proto（43 份）。
# 覆盖 43 个服务、589 个 RPC 方法；先按消费面分大节，节内再按服务分节。
# 
# 状态：本脚本**从未在本仓执行过**——这里没有运行中的服务进程、没有 etcd，
# 维护者机器上的 MySQL 也不允许被自动化触碰。它只是把「每个方法怎么调」固化成
# 可复用入口；请求体是 proto 字段骨架，**不含业务前置数据**，绝大多数调用会因
# 记录不存在/参数校验失败而返回错误，这属于预期。不要把它当成通过/失败断言。
# 
# 前置：`grpcurl` 在 PATH（或 GRPCURL 环境变量指到可执行文件），目标服务已启动，
# 并且用 -d 里的占位值换成真实存在的主键才能观察到成功分支。
# 脚本靠 reflection 解析方法名：服务的 services/*/*.v1.go 只在 Mode 为 dev/test 时
# reflection.Register（示例 etc/*.yaml 都是 Mode: dev，本地默认可用）。
# 若目标进程是 Mode: pro，反射不会注册，需要改成
# grpcurl -import-path services/<svc>/rpc -proto <svc>.proto 的显式模式。
set -uo pipefail
GRPCURL="${GRPCURL:-grpcurl}"
HOST="${RPC_HOST:-127.0.0.1}"
PASS=0; FAIL=0
rpc() { # rpc <port> <package.Service/Method> <json>
  printf '==> %s :%s\n' "$2" "$1"
  if "$GRPCURL" -plaintext -d "$3" "$HOST:$1" "$2" 2>&1; then PASS=$((PASS+1)); else FAIL=$((FAIL+1)); fi
}

# @@ 消费面：终端面与运营面都消费 —— 18 个服务 / 245 个方法 @@

# ---------- account (端口 8083) ----------
# 查询单个用户基础信息
rpc 8083 'account.v1.Account/Info3' '{"mid":0,"real_ip":""}'
# 批量查询用户基础信息
rpc 8083 'account.v1.Account/Infos3' '{"mids":[0],"real_ip":""}'
# 按用户名批量查询
rpc 8083 'account.v1.Account/InfosByName3' '{"names":[""],"real_ip":""}'
# 查询单个用户名片
rpc 8083 'account.v1.Account/Card3' '{"mid":0,"real_ip":""}'
# 批量查询用户名片
rpc 8083 'account.v1.Account/Cards3' '{"mids":[0],"real_ip":""}'
# 查询用户完整资料
rpc 8083 'account.v1.Account/Profile3' '{"mid":0,"real_ip":""}'
# 查询带统计的资料
rpc 8083 'account.v1.Account/ProfileWithStat3' '{"mid":0,"real_ip":""}'
# 增加经验值
rpc 8083 'account.v1.Account/AddExp3' '{"mid":0,"exp":0,"operater":"","operate":"","reason":"","real_ip":""}'
# 增加道德值
rpc 8083 'account.v1.Account/AddMoral3' '{"mid":0,"moral":0,"oper":"","reason":"","remark":"","real_ip":""}'
# 查询关注关系
rpc 8083 'account.v1.Account/Relation3' '{"mid":0,"owner":0,"real_ip":""}'
# 查询关注列表
rpc 8083 'account.v1.Account/Attentions3' '{"mid":0,"real_ip":""}'
# 查询黑名单
rpc 8083 'account.v1.Account/Blacks3' '{"mid":0,"real_ip":""}'
# 批量查询关系
rpc 8083 'account.v1.Account/Relations3' '{"mid":0,"owners":[0],"real_ip":""}'
# 查询富关系
rpc 8083 'account.v1.Account/RichRelations3' '{"owner":0,"mids":[0],"real_ip":""}'
# 查询会员信息
rpc 8083 'account.v1.Account/Vip3' '{"mid":0,"real_ip":""}'
# 批量查询会员信息
rpc 8083 'account.v1.Account/Vips3' '{"mids":[0],"real_ip":""}'
# 失效指定用户的缓存（资料变更方调用：user-profile 等下游服务通过本方法 / 通知 account 失效 Info/Card/Profile/Vip 缓存，action=updateVip 时 / 额外触发 5 秒延迟二次失效）
rpc 8083 'account.v1.Account/DelCache' '{"mid":0,"action":""}'
# ==================== 登录与会话 ==================== / 密码登录（登录标识：用户名/手机/邮箱 + 密码）
rpc 8083 'account.v1.Account/PasswordLogin' '{"account":"","password":"","login_type":0,"capture_code":"","ip":"","device":"","buvid":""}'
# 验证码登录（手机 + 验证码）
rpc 8083 'account.v1.Account/CaptureLogin' '{"account":"","password":"","login_type":0,"capture_code":"","ip":"","device":"","buvid":""}'
# 注册（用户名+密码 或 手机+验证码+密码）
rpc 8083 'account.v1.Account/Register' '{"account":"","password":"","capture_code":"","ip":""}'
# 登出（吊销 token）
rpc 8083 'account.v1.Account/Logout' '{"token":""}'
# token 校验（参考 identify.GetTokenInfo，供网关统一鉴权）
rpc 8083 'account.v1.Account/TokenInfo' '{"token":"","buvid":""}'
# cookie 会话校验（参考 identify.GetCookieInfo）
rpc 8083 'account.v1.Account/CookieInfo' '{"cookie":""}'
# 刷新 token（参考 passport-login /token/renew）
rpc 8083 'account.v1.Account/RenewToken' '{"refresh_token":"","ip":""}'
# 发送登录/注册/找回验证码（参考 sms 服务的账号侧验证码能力）
rpc 8083 'account.v1.Account/SendCapture' '{"biz":0,"target":"","ip":""}'
# 校验验证码（参考 passport-login /captcha/check）
rpc 8083 'account.v1.Account/CheckCapture' '{"biz":0,"target":"","capture_code":""}'
# 设置/修改密码（参考 secure 服务；改密需旧密码）
rpc 8083 'account.v1.Account/SetPassword' '{"mid":0,"old_password":"","new_password":"","ip":""}'
# 重置密码（账号找回，参考 account-recovery；验证码校验后重置）
rpc 8083 'account.v1.Account/ResetPassword' '{"account":"","capture_code":"","new_password":"","ip":""}'
# 历史密码校验（参考 passport /history/pwd/check）
rpc 8083 'account.v1.Account/CheckHistoryPassword' '{"mid":0,"password":""}'
# 登录日志查询（参考 passport RPC.LoginLogs 与 /x/internal/passport/records/loginlog）
rpc 8083 'account.v1.Account/LoginLogs' '{"mid":0,"limit":0}'

# ---------- catalog (端口 8096) ----------
# 运营创建作品
rpc 8096 'catalog.v1.Catalog/CreateWork' '{"title":"","cover":"","typeid":0,"intro":"","operator":""}'
# 查询作品
rpc 8096 'catalog.v1.Catalog/GetWork' '{"season_id":0}'
# 分页查询作品
rpc 8096 'catalog.v1.Catalog/ListWorks' '{"typeid":0,"state":0,"pn":0,"ps":0}'
# 运营创建季
rpc 8096 'catalog.v1.Catalog/CreateSeason' '{"season_id":0,"season_no":0,"title":"","cover":"","operator":""}'
# 查询某作品的季列表
rpc 8096 'catalog.v1.Catalog/ListSeasons' '{"season_id":0}'
# 运营创建集（关联 asset_id；建集前经 asset RPC 校验媒资存在且已完成扫描探测）
rpc 8096 'catalog.v1.Catalog/CreateEpisode' '{"season_id":0,"ep_no":0,"title":"","asset_id":0,"duration":0,"operator":""}'
# 查询某季的集列表
rpc 8096 'catalog.v1.Catalog/ListEpisodes' '{"season_id":0}'
# 查询集详情
rpc 8096 'catalog.v1.Catalog/GetEpisode' '{"epid":0,"operator_mid":0,"region":""}'
# 上架集（状态流转到 PUBLISHED；先经 rights 版权窗口与 asset 媒资就绪校验）
rpc 8096 'catalog.v1.Catalog/PublishEpisode' '{"epid":0,"operator_mid":0,"region":""}'
# 下架集
rpc 8096 'catalog.v1.Catalog/OfflineEpisode' '{"epid":0,"operator_mid":0,"region":""}'
# 分区树（扁平列表）
rpc 8096 'catalog.v1.Catalog/ListZones' '{}'
# 标签查询（按名字模糊或 ID 列表）
rpc 8096 'catalog.v1.Catalog/ListTags' '{"name":"","tagids":[0],"pn":0,"ps":0}'

# ---------- coin (端口 8163) ----------
# 我的硬币账户（含今日额度）
rpc 8163 'coin.v1.Coin/GetCoinAccount' '{"mid":0}'
# 投币（扣币 + 记录 + 限额判定，幂等）
rpc 8163 'coin.v1.Coin/TossCoin' '{"mid":0,"target_aid":0,"count":0,"request_id":"","platform":{},"client_trace_id":""}'
# 取消投币（窗口内全额退回）
rpc 8163 'coin.v1.Coin/CancelToss' '{"mid":0,"target_aid":0,"request_id":"","operator":"","reason":""}'
# 我的投币记录
rpc 8163 'coin.v1.Coin/ListMyTosses' '{"mid":0,"state":{},"page":0,"size":0}'
# 单内容投币汇总
rpc 8163 'coin.v1.Coin/GetTargetSummary' '{"aid":0}'
# 列表页批量汇总
rpc 8163 'coin.v1.Coin/BatchGetTargetSummary' '{"aids":[0]}'
# 谁投了这条内容（运营/排障）
rpc 8163 'coin.v1.Coin/ListTargetTossers' '{"target_aid":0,"page":0,"size":0}'
# 发放/扣回硬币（运营授权或订单履约）
rpc 8163 'coin.v1.Coin/GrantCoin' '{"mid":0,"delta":0,"flow_type":{},"biz_no":"","operator":"","request_id":"","reason":""}'
# 硬币流水台账分页
rpc 8163 'coin.v1.Coin/ListCoinFlows' '{"mid":0,"flow_type":{},"biz_no":"","from_ts":0,"to_ts":0,"page":0,"size":0}'
# 生效参数读取
rpc 8163 'coin.v1.Coin/GetTossConfig' '{}'

# ---------- comment (端口 8082) ----------
# 发布评论或回复（state 通常为待审核）
rpc 8082 'comment.v1.Comment/PostComment' '{"oid":0,"tp":0,"root":0,"parent":0,"mid":0,"content":"","state":0,"trace_id":""}'
# 删除评论（本人或管理员）
rpc 8082 'comment.v1.Comment/DeleteComment' '{"rpid":0,"mid":0,"admin":false}'
# 分页查询目标下的根评论
rpc 8082 'comment.v1.Comment/ListComments' '{"oid":0,"tp":0,"viewer_mid":0,"sort":{},"pn":0,"ps":0}'
# 分页查询某根评论下的楼中楼回复
rpc 8082 'comment.v1.Comment/ListReplies' '{"root":0,"viewer_mid":0,"pn":0,"ps":0}'
# 置顶/取消置顶评论
rpc 8082 'comment.v1.Comment/PinComment' '{"rpid":0,"oid":0,"pin":false,"admin_mid":0}'
# 举报评论（写入 moderation-orchestrator 待审队列）
rpc 8082 'comment.v1.Comment/ReportComment' '{"rpid":0,"reporter_mid":0,"reason":0,"content":"","trace_id":""}'
# 查询目标下的评论计数快照
rpc 8082 'comment.v1.Comment/CommentStats' '{"oid":0,"tp":0}'

# ---------- creator (端口 8086) ----------
# 查询单个 UP 主特殊属性
rpc 8086 'creator.v1.Creator/UpSpecial' '{"mid":0}'
# 批量查询 UP 主特殊属性
rpc 8086 'creator.v1.Creator/UpsSpecial' '{"mids":[0]}'
# 查询所有特殊用户组
rpc 8086 'creator.v1.Creator/UpGroups' '{}'
# 查询某个分组下的所有用户
rpc 8086 'creator.v1.Creator/UpGroupMids' '{"group_id":0,"pn":0,"ps":0}'
# 查询 UP 主身份属性
rpc 8086 'creator.v1.Creator/UpAttr' '{"mid":0,"from":0}'
# 设置 UP 主关注弹窗开关
rpc 8086 'creator.v1.Creator/SetUpSwitch' '{"mid":0,"from":0,"state":0}'
# 查询 UP 主关注弹窗开关
rpc 8086 'creator.v1.Creator/UpSwitch' '{"mid":0,"from":0,"state":0}'
# 查询高能联盟 UP 主签约信息
rpc 8086 'creator.v1.Creator/GetHighAllyUps' '{"mids":[0]}'

# ---------- creator-revenue (端口 8164) ----------
# 运营面：新增/修改规则草稿
rpc 8164 'creatorrevenue.v1.CreatorRevenue/UpsertRevenueRule' '{"rule_id":0,"rule_code":"","source_type":{},"name":"","description":"","unit_price_per_1000_minor":0,"currency":"","unit":"","min_quantity":0,"monthly_cap_minor":0,"effective_from":0,"expected_version":0,"operator":"","request_id":"","reason":""}'
# 运营面：规则状态切换（DRAFT→ACTIVE→ARCHIVED）
rpc 8164 'creatorrevenue.v1.CreatorRevenue/SetRevenueRuleState' '{"rule_id":0,"target_state":{},"expected_version":0,"operator":"","request_id":"","reason":""}'
# 规则列表（运营面可见全部，创作者端只查 ACTIVE）
rpc 8164 'creatorrevenue.v1.CreatorRevenue/ListRevenueRules' '{"state":{},"source_type":{},"page":0,"size":0}'
# 规则读取（支持按历史版本复核）
rpc 8164 'creatorrevenue.v1.CreatorRevenue/GetRevenueRule' '{"rule_id":0,"rule_code":"","version":0}'
# 参加计划（必须带已确认的规则版本）
rpc 8164 'creatorrevenue.v1.CreatorRevenue/EnrollCreator' '{"mid":0,"agreed_rule_version":0,"operator":"","request_id":""}'
# 退出计划
rpc 8164 'creatorrevenue.v1.CreatorRevenue/LeavePlan' '{"mid":0,"operator":"","request_id":"","reason":""}'
# 运营面：暂停/恢复参与
rpc 8164 'creatorrevenue.v1.CreatorRevenue/SetEnrollmentState' '{"mid":0,"target_state":{},"operator":"","request_id":"","reason":""}'
# 查询参与状态
rpc 8164 'creatorrevenue.v1.CreatorRevenue/GetEnrollment' '{"mid":0}'
# 运营面：参与名单分页
rpc 8164 'creatorrevenue.v1.CreatorRevenue/ListEnrollments' '{"state":{},"page":0,"size":0}'
# 写入/更正计量台账（cron、spm 回填或运营手工激励）
rpc 8164 'creatorrevenue.v1.CreatorRevenue/RecordRevenueMetric' '{"period":"","mid":0,"aid":0,"source_type":{},"rule_code":"","quantity":0,"operator":"","request_id":"","source_detail":"","reason":""}'
# 计量台账分页
rpc 8164 'creatorrevenue.v1.CreatorRevenue/ListRevenueMetrics' '{"period":"","mid":0,"aid":0,"source_type":{},"page":0,"size":0}'
# 生成/重算周期结算单
rpc 8164 'creatorrevenue.v1.CreatorRevenue/GenerateSettlement' '{"period":"","mid":0,"force_void_confirmed":false,"operator":"","request_id":"","reason":""}'
# 运营确认结算单（金额冻结）
rpc 8164 'creatorrevenue.v1.CreatorRevenue/ConfirmSettlement' '{"settlement_nos":[""],"operator":"","request_id":"","reason":""}'
# 结算单分页
rpc 8164 'creatorrevenue.v1.CreatorRevenue/ListSettlements' '{"period":"","mid":0,"state":{},"page":0,"size":0}'
# 结算单详情（含分项）
rpc 8164 'creatorrevenue.v1.CreatorRevenue/GetSettlement' '{"settlement_no":"","mid":0}'
# 创作者端收益概览
rpc 8164 'creatorrevenue.v1.CreatorRevenue/GetRevenueSummary' '{"mid":0}'

# ---------- danmaku (端口 8103) ----------
# 发送弹幕：校验 → 限流 → 屏蔽词过滤 → 落待审状态 → 提交机审 → 计数
rpc 8103 'danmaku.v1.Danmaku/PostDanmaku' '{"oid":0,"aid":0,"mid":0,"progress_ms":0,"mode":{},"fontsize":0,"color":0,"content":"","idempotency_key":"","client_msg_id":"","trace_id":""}'
# 按 oid + 时间分段批量拉取弹幕（段缓存命中优先，miss 回源 MySQL 并回填）
rpc 8103 'danmaku.v1.Danmaku/ListDanmaku' '{"oid":0,"viewer_mid":0,"start_seg":0,"end_seg":0,"start_progress_ms":0,"end_progress_ms":0,"limit":0,"with_self_pending":false}'
# 删除弹幕（本人或管理员，软删除并保留 op_log 审计）
rpc 8103 'danmaku.v1.Danmaku/DeleteDanmaku' '{"dmid":0,"mid":0,"admin":false,"reason":"","trace_id":""}'
# 举报弹幕（写本地举报表，供 moderation 拉取，不直连其库）
rpc 8103 'danmaku.v1.Danmaku/ReportDanmaku' '{"dmid":0,"reporter_mid":0,"reason":0,"content":"","trace_id":""}'
# 运营侧屏蔽词增删改
rpc 8103 'danmaku.v1.Danmaku/BlockWord' '{"action":{},"word":"","scope":{},"oid":0,"operator_mid":0,"trace_id":""}'
# 运营侧屏蔽词分页查询
rpc 8103 'danmaku.v1.Danmaku/ListBlockWords' '{"scope":{},"oid":0,"only_enabled":false,"pn":0,"ps":0,"operator_mid":0}'
# 用户级屏蔽（屏蔽某用户或某关键词的弹幕）
rpc 8103 'danmaku.v1.Danmaku/UserBlock' '{"mid":0,"type":{},"blocked_mid":0,"keyword":"","unblock":false,"trace_id":""}'
# 用户级屏蔽列表
rpc 8103 'danmaku.v1.Danmaku/ListUserBlocks' '{"mid":0,"type":{},"pn":0,"ps":0}'
# 回写审核结论，按合法状态机推进（moderation.result.v1 消费者入口）
rpc 8103 'danmaku.v1.Danmaku/ApplyModerationResult' '{"dmid":0,"task_id":0,"verdict":{},"reason":"","operator":0,"event_id":"","trace_id":""}'

# ---------- inbox (端口 8104) ----------
# 系统/运营向单个或多个用户投递站内信（同事务写主体与收件行）。
rpc 8104 'inbox.v1.Inbox/SendSystemMessage' '{"mids":[0],"title":"","content":"","category":{},"msg_type":{},"sender_mid":0,"biz_type":"","biz_id":"","extra":"","idempotency_key":"","operator":0,"trace_id":""}'
# 按分类分页拉取收件箱（cursor 优先）。
rpc 8104 'inbox.v1.Inbox/ListMessages' '{"mid":0,"category":{},"cursor":"","ps":0,"unread_only":false}'
# 幂等标记已读。
rpc 8104 'inbox.v1.Inbox/MarkRead' '{"mid":0,"msg_ids":[0]}'
# 幂等把某分类（或全部）标记已读。
rpc 8104 'inbox.v1.Inbox/MarkAllRead' '{"mid":0,"category":{}}'
# 分类未读数（Redis 加速，缺失回落 DB 快照）。
rpc 8104 'inbox.v1.Inbox/GetUnreadCount' '{"mid":0,"force_recompute":false}'
# 从明细表重算未读并修复快照与 Redis。
rpc 8104 'inbox.v1.Inbox/RecomputeUnread' '{"mid":0}'
# 用户侧软删除。
rpc 8104 'inbox.v1.Inbox/DeleteMessage' '{"mid":0,"msg_ids":[0]}'

# ---------- live-room (端口 8119) ----------
# 创建直播间：校验分区有效与房间数上限 → 落 PENDING → 建绑定与配置 → 送资料审核
rpc 8119 'liveroom.v1.LiveRoom/CreateRoom' '{"mid":0,"title":"","cover":"","area_id":0,"platform":{},"app_version":"","setting":{"room_id":0,"danmaku_enabled":false,"reply_enabled":false,"record_enabled":false,"linkmic_enabled":false,"live_type":0,"min_client_version_code":0,"mtime":0},"request_id":"","trace_id":""}'
# 修改标题/封面/分区：终态房间不可改；改动后重新送审（PENDING/READY 才允许）
rpc 8119 'liveroom.v1.LiveRoom/UpdateRoomInfo' '{"room_id":0,"operator_mid":0,"title":"","cover":"","area_id":0,"request_id":"","trace_id":""}'
# 读房间（可按 room_id 或房主 mid），可附带配置与进行中场次
rpc 8119 'liveroom.v1.LiveRoom/GetRoom' '{"room_id":0,"owner_mid":0,"with_setting":false,"with_active_session":false}'
# 分页浏览房间（发现页/主播主页/运营列表）
rpc 8119 'liveroom.v1.LiveRoom/ListRooms' '{"owner_mid":0,"area_id":0,"state":{},"order":{},"page":0,"page_size":0}'
# 开播前置检查：主播资格(creator) + 风控(risk-control) + 资料审核 + 未禁播，全通过才 PENDING→READY
rpc 8119 'liveroom.v1.LiveRoom/PrepareLive' '{"room_id":0,"mid":0,"platform":{},"device_hash":"","ip_hash":"","request_id":"","trace_id":""}'
# 开播：READY→LIVING 并新建场次（不接收推流密钥，只登记 stream_id 引用）
rpc 8119 'liveroom.v1.LiveRoom/StartLive' '{"room_id":0,"mid":0,"stream_id":"","request_id":"","trace_id":""}'
# 下播：LIVING→READY 并把场次置为 ENDED（时长簿记）
rpc 8119 'liveroom.v1.LiveRoom/EndLive' '{"room_id":0,"session_id":0,"mid":0,"end_reason":{},"request_id":"","trace_id":""}'
# 关闭房间：任意非终态 → FINISHED，强制终止进行中场次并保留审计
rpc 8119 'liveroom.v1.LiveRoom/CloseRoom' '{"room_id":0,"operator_mid":0,"admin":false,"reason":"","request_id":"","trace_id":""}'
# 推流状态事件入口（live.state.v1 消费者或 live-ingest 直调）：event_id 去重 + seq 乱序守卫
rpc 8119 'liveroom.v1.LiveRoom/ReportStreamState' '{"event_id":"","room_id":0,"session_id":0,"stream_id":"","stream_state":0,"stream_seq":0,"occurred_at":0,"interrupted_seconds":0,"reason":"","trace_id":""}'
# 资料审核结论回写（moderation.result.v1 消费者入口），按合法状态机推进
rpc 8119 'liveroom.v1.LiveRoom/ApplyRoomModerationResult' '{"room_id":0,"task_id":0,"verdict":{},"reason":"","operator":0,"event_id":"","trace_id":""}'
# 禁播：进入 BANNED 并终止进行中场次（运营/系统，需 operator_mid）
rpc 8119 'liveroom.v1.LiveRoom/BanRoom' '{"room_id":0,"ban_type":{},"duration_seconds":0,"reason":"","operator_mid":0,"request_id":"","trace_id":""}'
# 解除禁播：BANNED→READY
rpc 8119 'liveroom.v1.LiveRoom/LiftBan' '{"room_id":0,"ban_id":0,"operator_mid":0,"reason":"","request_id":"","trace_id":""}'
# 分页查询禁播记录（运营侧审计）
rpc 8119 'liveroom.v1.LiveRoom/ListRoomBans' '{"room_id":0,"mid":0,"state":0,"page":0,"page_size":0,"operator_mid":0}'
# 读场次（按 session_id，或按 room_id 取最近 N 场之一）
rpc 8119 'liveroom.v1.LiveRoom/GetSession' '{"session_id":0,"room_id":0,"offset":0}'
# cursor 分页拉取历史场次
rpc 8119 'liveroom.v1.LiveRoom/ListSessions' '{"room_id":0,"mid":0,"state":{},"cursor":"","page_size":0}'
# 关联回放：只写 record/asset/aid 引用与回放状态，不落媒资数据
rpc 8119 'liveroom.v1.LiveRoom/AttachReplay' '{"session_id":0,"room_id":0,"record_id":0,"record_asset_id":0,"record_aid":0,"replay_state":{},"request_id":"","trace_id":""}'
# 更新直播配置（弹幕/回复/录制/连麦/直播类型）
rpc 8119 'liveroom.v1.LiveRoom/UpdateRoomSetting' '{"room_id":0,"operator_mid":0,"setting":{"room_id":0,"danmaku_enabled":false,"reply_enabled":false,"record_enabled":false,"linkmic_enabled":false,"live_type":0,"min_client_version_code":0,"mtime":0},"request_id":"","trace_id":""}'
# 绑定或解绑主播（房主/联合主播/房管），含单主播房间数上限校验
rpc 8119 'liveroom.v1.LiveRoom/MutateAnchor' '{"room_id":0,"operator_mid":0,"target_mid":0,"action":{},"role":{},"request_id":"","trace_id":""}'
# 分页查询房间主播绑定
rpc 8119 'liveroom.v1.LiveRoom/ListAnchors' '{"room_id":0,"role":{},"only_enabled":false,"page":0,"page_size":0}'
# 运营侧新建/修改直播分区
rpc 8119 'liveroom.v1.LiveRoom/UpsertArea' '{"area_id":0,"area_name":"","parent_area_id":0,"sort":0,"state":0,"operator_mid":0,"request_id":""}'
# 分区列表（客户端与运营共用）
rpc 8119 'liveroom.v1.LiveRoom/ListAreas' '{"parent_area_id":0,"state":0,"page":0,"page_size":0}'

# ---------- membership (端口 8160) ----------
# 终端套餐列表（只含在售且平台可见）
rpc 8160 'membership.v1.Membership/ListPlans' '{"platform":{},"vip_type":{},"on_sale_only":false}'
# 单个套餐读取（下单前置校验用）
rpc 8160 'membership.v1.Membership/GetPlan' '{"plan_id":0,"plan_code":""}'
# 运营面：新建或修改套餐草稿
rpc 8160 'membership.v1.Membership/UpsertPlan' '{"plan_id":0,"plan_code":"","name":"","description":"","vip_type":{},"duration_days":0,"unit_count":0,"price_minor":0,"prom_price_minor":0,"currency":"","platforms":[{}],"auto_renew_supported":false,"expected_version":0,"operator":"","request_id":"","reason":""}'
# 运营面：上下架（带理由，写变更台账）
rpc 8160 'membership.v1.Membership/SetPlanState' '{"plan_id":0,"target_state":{},"expected_version":0,"operator":"","request_id":"","reason":""}'
# 运营面：分页查询全部套餐（含草稿与已下架）
rpc 8160 'membership.v1.Membership/ListPlansAdmin' '{"page":0,"size":0,"state":{},"vip_type":{},"keyword":""}'
# 我的会员状态
rpc 8160 'membership.v1.Membership/GetMembership' '{"mid":0,"vip_type":{}}'
# 单项权益判定——全站唯一的会员权益口径出口
rpc 8160 'membership.v1.Membership/CheckEntitlement' '{"mid":0,"code":""}'
# 多项权益判定（播放详情页等一次问多项）
rpc 8160 'membership.v1.Membership/CheckEntitlements' '{"mid":0,"codes":[""]}'
# 开通/续期（只由订单履约或运营授权调用）
rpc 8160 'membership.v1.Membership/GrantMembership' '{"mid":0,"vip_type":{},"plan_id":0,"delta_days":0,"source":{},"biz_order_no":"","payment_no":"","operator":"","request_id":"","reason":""}'
# 收回（退款回收/运营纠错）
rpc 8160 'membership.v1.Membership/RevokeMembership' '{"mid":0,"vip_type":{},"clear_remaining":false,"delta_days":0,"operator":"","request_id":"","reason":"","plan_id":0,"biz_order_no":"","payment_no":""}'
# 自动续费签约位翻转（沙箱，不建真实代扣协议）
rpc 8160 'membership.v1.Membership/SetAutoRenew' '{"mid":0,"vip_type":{},"on":false,"channel":"","operator":"","request_id":"","reason":""}'
# 授予台账分页（运营面与用户面共用，mid=0 才有跨用户语义）
rpc 8160 'membership.v1.Membership/ListGrants' '{"mid":0,"vip_type":{},"source":{},"biz_order_no":"","from_ts":0,"to_ts":0,"page":0,"size":0}'
# cron：扫描到期区间
rpc 8160 'membership.v1.Membership/ListExpiringMemberships' '{"from_expire_at":0,"to_expire_at":0,"auto_renew_only":false,"limit":0}'
# cron：幂等置过期
rpc 8160 'membership.v1.Membership/ExpireMembership' '{"mid":0,"vip_type":{},"operator":"","request_id":"","reason":""}'
# 权益码目录读取
rpc 8160 'membership.v1.Membership/ListEntitlements' '{"enabled_only":false}'
# 运营面：权益码新增/开关
rpc 8160 'membership.v1.Membership/UpsertEntitlement' '{"code":"","name":"","description":"","min_vip_type":{},"enabled":false,"expected_version":0,"operator":"","request_id":""}'

# ---------- moderation-orchestrator (端口 8093) ----------
# 领域服务提交审核任务（创建 task 并入 MQ 待处理）
rpc 8093 'moderation.v1.ModerationOrchestrator/SubmitForReview' '{"submission_id":0,"content_type":{},"mid":0,"up_mid":0,"business":"","reason":"","ip":""}'
# 查询任务详情
rpc 8093 'moderation.v1.ModerationOrchestrator/GetTask' '{"task_id":0,"ip":""}'
# 查询审核结论
rpc 8093 'moderation.v1.ModerationOrchestrator/GetResult' '{"task_id":0,"ip":""}'
# 分页查询任务列表（运营后台用）
rpc 8093 'moderation.v1.ModerationOrchestrator/ListTasks' '{"mid":0,"content_type":{},"state":{},"pn":0,"ps":0,"ip":""}'
# 提交申诉
rpc 8093 'moderation.v1.ModerationOrchestrator/SubmitAppeal' '{"task_id":0,"mid":0,"content":"","ip":""}'
# 处理申诉（运营）
rpc 8093 'moderation.v1.ModerationOrchestrator/ProcessAppeal' '{"appeal_id":0,"handler":0,"final_verdict":{},"final_reason":"","ip":""}'
# worker 调用，回写识别结果（由 moderation-worker 调用）
rpc 8093 'moderation.v1.ModerationOrchestrator/SubmitWorkerResult' '{"task_id":0,"worker_id":0,"verdict":{},"reason":"","ip":""}'

# ---------- notification (端口 8108) ----------
# 投递通知：模板渲染 -> 频次/免打扰校验 -> 落投递任务（biz_key 幂等）-> 按配置同步或异步投递
rpc 8108 'notification.v1.Notification/SendNotification' '{"recipients":[{"mid":0,"target_ref":"","language":{},"device_id":""}],"channel":{},"template_code":"","template_params":{"k":""},"idempotency_key":"","biz_key":"","priority":{},"expire_at":0,"trace_id":"","default_language":{}}'
# 仅渲染模板供上游预览，不落库
rpc 8108 'notification.v1.Notification/RenderTemplate' '{"channel":{},"template_code":"","version":0,"language":{},"template_params":{"k":""},"operator":""}'
# 新增或更新模板（草稿或直接发布新版本）
rpc 8108 'notification.v1.Notification/UpsertTemplate' '{"template_code":"","channel":{},"language":{},"title_tpl":"","body_tpl":"","operator":"","publish":false}'
# 分页查询模板
rpc 8108 'notification.v1.Notification/ListTemplates' '{"template_code":"","channel":{},"language":{},"state":{},"pn":0,"ps":0}'
# 发布指定草稿版本
rpc 8108 'notification.v1.Notification/PublishTemplate' '{"template_code":"","channel":{},"language":{},"version":0,"operator":""}'
# 查询单条投递记录（含供应商回执）
rpc 8108 'notification.v1.Notification/GetDeliveryStatus' '{"delivery_id":""}'
# 分页查询投递记录
rpc 8108 'notification.v1.Notification/ListDeliveries' '{"mid":0,"channel":{},"state":{},"biz_key":"","start_ctime":0,"end_ctime":0,"pn":0,"ps":0}'
# 分页查询死信
rpc 8108 'notification.v1.Notification/ListDeadLetters' '{"event_id":"","state":{},"topic":"","pn":0,"ps":0}'
# 运营侧重投死信（按 operator 记审计，幂等）
rpc 8108 'notification.v1.Notification/RetryDeadLetter' '{"id":0,"operator":"","reason":""}'
# 更新用户通道偏好与免打扰设置
rpc 8108 'notification.v1.Notification/UpdateDndPreference' '{"mid":0,"muted_channels":[{}],"quiet_start":"","quiet_end":"","timezone":"","enabled":false}'
# 查询用户通道偏好与免打扰设置
rpc 8108 'notification.v1.Notification/GetDndPreference' '{"mid":0}'

# ---------- payment (端口 8161) ----------
# 查询余额
rpc 8161 'payment.v1.Payment/GetWallet' '{"mid":0,"currency":""}'
# 运营调整余额（有台账、有理由）
rpc 8161 'payment.v1.Payment/AdjustBalance' '{"mid":0,"delta_minor":0,"currency":"","operator":"","request_id":"","reason":""}'
# 开充值单（仅 SANDBOX）
rpc 8161 'payment.v1.Payment/OpenRecharge' '{"mid":0,"amount_minor":0,"currency":"","channel":{},"request_id":"","client_trace_id":""}'
# 沙箱结算充值单并入账
rpc 8161 'payment.v1.Payment/SettleSandboxRecharge' '{"recharge_no":"","operator":"","request_id":"","reason":""}'
# 取消未结算充值单
rpc 8161 'payment.v1.Payment/CancelRecharge' '{"recharge_no":"","operator":"","request_id":"","reason":""}'
# 充值台账分页
rpc 8161 'payment.v1.Payment/ListRecharges' '{"mid":0,"state":{},"from_ts":0,"to_ts":0,"page":0,"size":0}'
# 受理支付（余额扣减或沙箱渠道立即成功）
rpc 8161 'payment.v1.Payment/CreatePayment' '{"biz_order_no":"","mid":0,"amount_minor":0,"currency":"","method":{},"subject":"","expire_at":0,"request_id":"","operator":""}'
# 支付单读取（按 payment_no 或订单号）
rpc 8161 'payment.v1.Payment/GetPayment' '{"payment_no":"","biz_order_no":""}'
# 关闭未支付/未履约支付单
rpc 8161 'payment.v1.Payment/ClosePayment' '{"payment_no":"","operator":"","request_id":"","reason":""}'
# 支付台账分页
rpc 8161 'payment.v1.Payment/ListPayments' '{"mid":0,"state":{},"method":{},"from_ts":0,"to_ts":0,"page":0,"size":0}'
# 退款（只退回余额；原路退回渠道返回 not-configured）
rpc 8161 'payment.v1.Payment/RefundPayment' '{"payment_no":"","amount_minor":0,"to_balance":false,"operator":"","request_id":"","reason":""}'
# 退款台账分页
rpc 8161 'payment.v1.Payment/ListRefunds' '{"mid":0,"payment_no":"","page":0,"size":0,"from_ts":0,"to_ts":0}'
# 资金流水分页
rpc 8161 'payment.v1.Payment/ListFlows' '{"mid":0,"biz_type":{},"biz_no":"","from_ts":0,"to_ts":0,"page":0,"size":0}'
# 渠道能力自述（沙箱模式与真实渠道是否配置）
rpc 8161 'payment.v1.Payment/DescribeChannels' '{}'

# ---------- private-message (端口 8150) ----------
# 定位或创建单聊会话（pair_key 唯一索引保证幂等）。
rpc 8150 'privatemessage.v1.PrivateMessage/GetOrCreateConversation' '{"mid":0,"peer_mid":0,"trace_id":""}'
# 发送私信（client_msg_id 幂等 + 门禁顺序见 SendMessageReq 注释）。
rpc 8150 'privatemessage.v1.PrivateMessage/SendMessage' '{"mid":0,"conversation_id":0,"peer_mid":0,"msg_type":{},"content":"","media_ref":"","client_msg_id":"","trace_id":""}'
# 会话列表（cursor 分页，黑名单/风控/隐藏会话在查询层过滤）。
rpc 8150 'privatemessage.v1.PrivateMessage/ListConversations' '{"mid":0,"cursor":"","ps":0,"only_unread":false,"include_hidden":false,"trace_id":""}'
# 会话内消息分页（conversation_id + seq 游标，禁止 offset 全表扫）。
rpc 8150 'privatemessage.v1.PrivateMessage/ListMessages' '{"conversation_id":0,"mid":0,"cursor_seq":0,"ps":0,"trace_id":""}'
# 前移已读游标（幂等，只前进不回退）。
rpc 8150 'privatemessage.v1.PrivateMessage/MarkRead' '{"conversation_id":0,"mid":0,"read_seq":0,"trace_id":""}'
# 未读汇总（投影，可重算）。
rpc 8150 'privatemessage.v1.PrivateMessage/GetUnreadSummary' '{"mid":0,"force":false,"trace_id":""}'
# 撤回消息（只改可见性标记 + 写审计）。
rpc 8150 'privatemessage.v1.PrivateMessage/WithdrawMessage' '{"msg_id":0,"operator_mid":0,"source":{},"reason":"","audit_task_id":0,"trace_id":""}'
# 本方隐藏/恢复会话。
rpc 8150 'privatemessage.v1.PrivateMessage/HideConversation' '{"mid":0,"conversation_id":0,"hide":false,"trace_id":""}'
# 更新反骚扰偏好。
rpc 8150 'privatemessage.v1.PrivateMessage/UpdateUserSetting' '{"mid":0,"allow_from":{},"reject_stranger":false,"keyword_filter":false,"mute_conversation":false,"trace_id":""}'
# 查询反骚扰偏好。
rpc 8150 'privatemessage.v1.PrivateMessage/GetUserSetting' '{"mid":0,"trace_id":""}'
# 举报私信（写本域举报事实并向 moderation 送审）。
rpc 8150 'privatemessage.v1.PrivateMessage/ReportMessage' '{"msg_id":0,"reporter_mid":0,"reason":0,"description":"","trace_id":""}'
# 运营侧举报分页。
rpc 8150 'privatemessage.v1.PrivateMessage/ListReports' '{"state":{},"target_mid":0,"cursor":"","ps":0,"operator_mid":0,"trace_id":""}'
# 运营侧举报处置（幂等键防重复处置）。
rpc 8150 'privatemessage.v1.PrivateMessage/HandleReport' '{"report_id":0,"action":{},"handler":0,"note":"","withdraw_message":false,"idempotency_key":"","trace_id":""}'
# 审核结论回写（唯一写结论入口，按 event_id 去重）。
rpc 8150 'privatemessage.v1.PrivateMessage/ApplyModerationVerdict' '{"msg_id":0,"task_id":0,"verdict":{},"reason":"","operator":0,"event_id":"","trace_id":""}'
# 留存到期清理（正文物理删除，审计保留）。
rpc 8150 'privatemessage.v1.PrivateMessage/PurgeExpiredMessages' '{"before_time":0,"batch_limit":0,"dry_run":false,"operator":0,"trace_id":""}'

# ---------- trade-order (端口 8162) ----------
# 下单（服务端重算金额，沙箱下内联受理）
rpc 8162 'tradeorder.v1.TradeOrder/CreateOrder' '{"mid":0,"biz_type":{},"plan_id":0,"plan_code":"","quantity":0,"pay_method":{},"amount_minor":0,"request_id":"","platform":{},"client_trace_id":"","title":""}'
# 订单详情（带归属校验）
rpc 8162 'tradeorder.v1.TradeOrder/GetOrder' '{"order_no":"","mid":0}'
# 我的订单
rpc 8162 'tradeorder.v1.TradeOrder/ListMyOrders' '{"mid":0,"state":{},"biz_type":{},"page":0,"size":0}'
# 运营面订单查询（有界窗口）
rpc 8162 'tradeorder.v1.TradeOrder/ListOrders' '{"mid":0,"state":{},"biz_type":{},"pay_method":{},"order_no":"","payment_no":"","from_ts":0,"to_ts":0,"page":0,"size":0,"max_window_seconds":0}'
# 订单状态流转台账
rpc 8162 'tradeorder.v1.TradeOrder/ListOrderEvents' '{"order_no":"","page":0,"size":0}'
# 取消未支付订单
rpc 8162 'tradeorder.v1.TradeOrder/CancelOrder' '{"order_no":"","mid":0,"operator":"","request_id":"","reason":""}'
# 绑定支付结论（补偿推进）
rpc 8162 'tradeorder.v1.TradeOrder/BindPayment' '{"order_no":"","payment_no":"","amount_minor":0,"operator":"","request_id":""}'
# 执行/重试履约
rpc 8162 'tradeorder.v1.TradeOrder/FulfillOrder' '{"order_no":"","operator":"","request_id":""}'
# cron：卡单扫描
rpc 8162 'tradeorder.v1.TradeOrder/ListStuckOrders' '{"older_than_seconds":0,"states":[{}],"limit":0}'
# 申请退款
rpc 8162 'tradeorder.v1.TradeOrder/RequestRefund' '{"order_no":"","mid":0,"operator":"","amount_minor":0,"reason":"","request_id":""}'
# 审批通过并回收权益
rpc 8162 'tradeorder.v1.TradeOrder/ApproveRefund' '{"order_no":"","operator":"","request_id":"","reason":"","expected_version":0}'
# 驳回退款
rpc 8162 'tradeorder.v1.TradeOrder/RejectRefund' '{"order_no":"","operator":"","request_id":"","reason":""}'

# ---------- transcode (端口 8100) ----------
# 创建转码任务（PENDING 状态，本期占位不调用 FFmpeg）
rpc 8100 'transcode.v1.Transcode/SubmitTask' '{"asset_id":0,"template_id":0,"input_bucket":"","input_key":"","output_bucket":"","output_key":""}'
# 查询任务详情
rpc 8100 'transcode.v1.Transcode/GetTask' '{"task_id":0}'
# 分页查询任务（按 asset_id 或 state 过滤）
rpc 8100 'transcode.v1.Transcode/ListTasks' '{"asset_id":0,"state":{},"pn":0,"ps":0}'
# Worker 上报进度（校验状态机：PENDING→PROCESSING→SUCCEEDED/FAILED）
rpc 8100 'transcode.v1.Transcode/UpdateProgress' '{"task_id":0,"progress":0,"state":{},"errno":0,"err_msg":""}'
# 分页查询模板列表
rpc 8100 'transcode.v1.Transcode/ListTemplates' '{"pn":0,"ps":0}'
# 查询模板详情
rpc 8100 'transcode.v1.Transcode/GetTemplate' '{"template_id":0}'
# 运营创建模板
rpc 8100 'transcode.v1.Transcode/CreateTemplate' '{"name":"","codec":"","width":0,"height":0,"bitrate":0,"fps":0,"segment_seconds":0}'

# ---------- user-profile (端口 8085) ----------
# 查询单个用户基础资料
rpc 8085 'userprofile.v1.UserProfile/Base' '{"mid":0,"remote_ip":""}'
# 批量查询用户基础资料
rpc 8085 'userprofile.v1.UserProfile/Bases' '{"mids":[0],"remote_ip":""}'
# 查询单个用户全量信息（基础 + 等级 + 官方认证）
rpc 8085 'userprofile.v1.UserProfile/Member' '{"mid":0,"remote_ip":""}'
# 批量查询用户全量信息
rpc 8085 'userprofile.v1.UserProfile/Members' '{"mids":[0],"remote_ip":""}'
# 查询用户是否修改过昵称
rpc 8085 'userprofile.v1.UserProfile/NickUpdated' '{"mid":0,"remote_ip":""}'
# 标记用户已修改过昵称
rpc 8085 'userprofile.v1.UserProfile/SetNickUpdated' '{"mid":0,"remote_ip":""}'
# 提交官方认证文档
rpc 8085 'userprofile.v1.UserProfile/SetOfficialDoc' '{"mid":0,"name":"","state":0,"role":0,"title":"","desc":"","reject_reason":"","realname":0,"operator":"","telephone":"","email":"","address":"","company":"","credit_code":"","organization":"","organization_type":"","business_license":"","business_scale":"","business_level":"","business_auth":"","supplement":"","professional":"","identification":"","submit_source":""}'
# 设置性别
rpc 8085 'userprofile.v1.UserProfile/SetSex' '{"mid":0,"sex":0,"remote_ip":""}'
# 设置昵称
rpc 8085 'userprofile.v1.UserProfile/SetName' '{"mid":0,"name":"","remote_ip":""}'
# 设置头像
rpc 8085 'userprofile.v1.UserProfile/SetFace' '{"mid":0,"face":"","remote_ip":""}'
# 设置排名
rpc 8085 'userprofile.v1.UserProfile/SetRank' '{"mid":0,"rank":0,"remote_ip":""}'
# 设置生日
rpc 8085 'userprofile.v1.UserProfile/SetBirthday' '{"mid":0,"birthday":0,"remote_ip":""}'
# 设置签名
rpc 8085 'userprofile.v1.UserProfile/SetSign' '{"mid":0,"sign":"","remote_ip":""}'
# 查询官方认证文档
rpc 8085 'userprofile.v1.UserProfile/OfficialDoc' '{"mid":0,"real_ip":""}'
# 查询节操值
rpc 8085 'userprofile.v1.UserProfile/Moral' '{"mid":0,"remote_ip":""}'
# 查询节操值变更日志
rpc 8085 'userprofile.v1.UserProfile/MoralLog' '{"mid":0,"remote_ip":""}'
# 变更节操值
rpc 8085 'userprofile.v1.UserProfile/AddMoral' '{"mid":0,"delta":0,"origin":0,"reason":"","reason_type":0,"operator":"","remark":"","status":0,"is_notify":false,"ip":""}'
# 批量变更节操值
rpc 8085 'userprofile.v1.UserProfile/BatchAddMoral' '{"mids":[0],"delta":0,"origin":0,"reason":"","reason_type":0,"operator":"","remark":"","status":0,"is_notify":false,"ip":""}'
# 撤销节操值变更（参考 member 服务 /moral/undo 的 RPC 化）
rpc 8085 'userprofile.v1.UserProfile/UndoMoral' '{"log_id":"","remark":"","operator":""}'
# 查询经验等级信息（含当前经验）
rpc 8085 'userprofile.v1.UserProfile/Exp' '{"mid":0,"real_ip":""}'
# 查询等级信息（不含当前经验）
rpc 8085 'userprofile.v1.UserProfile/Level' '{"mid":0,"real_ip":""}'
# 更新经验值
rpc 8085 'userprofile.v1.UserProfile/UpdateExp' '{"mid":0,"count":0,"reason":"","operate":"","ip":""}'
# 直接设置经验值（仅运营，参考 member 服务 /exp/set 的 RPC 化）
rpc 8085 'userprofile.v1.UserProfile/SetExp' '{"mid":0,"count":0,"reason":"","operate":"","ip":""}'
# 查询经验变更日志
rpc 8085 'userprofile.v1.UserProfile/ExpLog' '{"mid":0,"real_ip":""}'
# 查询当日经验奖励统计
rpc 8085 'userprofile.v1.UserProfile/ExpStat' '{"mid":0,"real_ip":""}'
# 查询实名认证状态
rpc 8085 'userprofile.v1.UserProfile/RealnameStatus' '{"mid":0,"remote_ip":""}'
# 查询实名申请流程状态
rpc 8085 'userprofile.v1.UserProfile/RealnameApplyStatus' '{"mid":0,"remote_ip":""}'
# 发送实名手机验证码
rpc 8085 'userprofile.v1.UserProfile/RealnameTelCapture' '{"mid":0,"remote_ip":""}'
# 提交实名认证申请
rpc 8085 'userprofile.v1.UserProfile/RealnameApply' '{"mid":0,"capture_code":0,"realname":"","card_type":0,"card_code":"","country":0,"hand_img_token":"","front_img_token":"","back_img_token":""}'
# 查询实名详情（含性别与手持照）
rpc 8085 'userprofile.v1.UserProfile/RealnameDetail' '{"mid":0,"remote_ip":""}'
# 查询脱敏实名信息
rpc 8085 'userprofile.v1.UserProfile/RealnameStrippedInfo' '{"mid":0,"remote_ip":""}'
# 按证件号批量查询 mid
rpc 8085 'userprofile.v1.UserProfile/MidByRealnameCard' '{"card_code":[""],"country":0,"card_type":0}'
# 添加用户到监控名单
rpc 8085 'userprofile.v1.UserProfile/AddUserMonitor' '{"mid":0,"operator":"","remark":""}'
# 查询用户是否在监控名单
rpc 8085 'userprofile.v1.UserProfile/IsInMonitor' '{"mid":0,"real_ip":""}'
# 添加用户属性变更审核（参考 member 服务 /property/review/add 的 RPC 化）
rpc 8085 'userprofile.v1.UserProfile/AddPropertyReview' '{"mid":0,"new":"","state":0,"property":0,"extra":""}'

# ---------- video (端口 8095) ----------
# 创建稿件（DRAFT 状态），返回新稿件
rpc 8095 'video.v1.Video/CreateSubmission' '{"mid":0,"title":"","desc":"","cover":"","typeid":0,"tag":"","ip":""}'
# 查询稿件详情
rpc 8095 'video.v1.Video/GetSubmission' '{"aid":0,"mid":0,"ip":""}'
# 分页查询稿件（按 mid 或 typeid 过滤）
rpc 8095 'video.v1.Video/ListSubmissions' '{"mid":0,"typeid":0,"pn":0,"ps":0,"ip":""}'
# 更新稿件元信息（仅 DRAFT 可改）
rpc 8095 'video.v1.Video/UpdateSubmission' '{"aid":0,"mid":0,"title":"","desc":"","cover":"","typeid":0,"tag":"","ip":""}'
# 删除稿件（状态流转到 DELETED，保留审计）
rpc 8095 'video.v1.Video/DeleteSubmission' '{"aid":0,"mid":0,"ip":""}'
# 推进稿件状态机（校验合法转换，非法转换返回 ErrInvalidStateTransition）
rpc 8095 'video.v1.Video/TransitionState' '{"aid":0,"target":{},"operator":"","reason":"","ip":""}'
# 按状态查询稿件（运营/系统用）
rpc 8095 'video.v1.Video/ListByState' '{"state":{},"pn":0,"ps":0,"ip":""}'
# 查询稿件当前可播放版次（aid → asset_id，供网关解析播放来源）
rpc 8095 'video.v1.Video/GetPlayableSource' '{"aid":0,"mid":0,"ip":""}'

# @@ 消费面：只有终端面消费 —— 6 个服务 / 55 个方法 @@

# ---------- engagement (端口 8084) ----------
# 点赞/取消点赞/点踩（幂等：重复请求不重复增加计数）
rpc 8084 'engagement.v1.Engagement/Like' '{"business":"","mid":0,"up_mid":0,"origin_id":0,"message_id":0,"action":{},"ip":""}'
# 批量查询对象计数与当前用户状态
rpc 8084 'engagement.v1.Engagement/Stats' '{"business":"","origin_id":0,"message_ids":[0],"mid":0,"ip":""}'
# 跨业务批量查询计数
rpc 8084 'engagement.v1.Engagement/MultiStats' '{"mid":0,"business":{"k":null},"ip":""}'
# 批量查询用户是否点赞
rpc 8084 'engagement.v1.Engagement/HasLike' '{"business":"","message_ids":[0],"mid":0,"ip":""}'
# 用户的点赞列表（分页）
rpc 8084 'engagement.v1.Engagement/UserLikes' '{"business":"","mid":0,"pn":0,"ps":0,"ip":""}'
# 对象的点赞人列表（分页）
rpc 8084 'engagement.v1.Engagement/ItemLikes' '{"business":"","origin_id":0,"message_id":0,"last_mid":0,"pn":0,"ps":0,"ip":""}'
# 运营修改计数（增量）
rpc 8084 'engagement.v1.Engagement/UpdateCount' '{"business":"","origin_id":0,"message_id":0,"like_change":0,"dislike_change":0,"operator":"","ip":""}'
# 查询原始计数（未修正值）
rpc 8084 'engagement.v1.Engagement/RawStat' '{"business":"","origin_id":0,"message_id":0,"ip":""}'
# 添加收藏
rpc 8084 'engagement.v1.Engagement/AddFav' '{"tp":0,"mid":0,"fid":0,"oid":0,"otype":0}'
# 删除收藏
rpc 8084 'engagement.v1.Engagement/DelFav' '{"tp":0,"mid":0,"fid":0,"oid":0,"otype":0}'
# 查询是否已收藏
rpc 8084 'engagement.v1.Engagement/IsFavored' '{"tp":0,"mid":0,"oid":0}'
# 批量查询是否已收藏
rpc 8084 'engagement.v1.Engagement/IsFavoreds' '{"tp":0,"mid":0,"oids":[0]}'
# 用户收藏夹列表
rpc 8084 'engagement.v1.Engagement/UserFolders' '{"tp":0,"mid":0,"vmid":0,"oid":0,"all_count":false,"otype":0}'
# 创建收藏夹
rpc 8084 'engagement.v1.Engagement/AddFolder' '{"tp":0,"mid":0,"name":"","description":"","cover":"","public":0}'
# 删除收藏夹
rpc 8084 'engagement.v1.Engagement/DelFolder' '{"tp":0,"mid":0,"fid":0}'
# 记录分享并返回分享数
rpc 8084 'engagement.v1.Engagement/AddShare' '{"oid":0,"mid":0,"type":0,"ip":""}'

# ---------- feed (端口 8092) ----------
# 领域服务推送新动态（写扩散：fan-out 到所有粉丝收件箱）
rpc 8092 'feed.v1.Feed/PushFeed' '{"mid":0,"oid":0,"otype":{},"action":{},"ctime":0,"title":"","cover":"","uri":"","forward_id":0,"source":"","operator":"","real_ip":""}'
# 拉取关注流（cursor 翻页）
rpc 8092 'feed.v1.Feed/PullFeed' '{"mid":0,"cursor":0,"ps":0,"real_ip":""}'
# 查询某用户主页动态
rpc 8092 'feed.v1.Feed/ListUserFeed' '{"vmid":0,"mid":0,"cursor":0,"ps":0,"real_ip":""}'
# 置顶动态
rpc 8092 'feed.v1.Feed/PinFeed' '{"mid":0,"feed_id":0,"operator":"","real_ip":""}'
# 取消置顶
rpc 8092 'feed.v1.Feed/UnpinFeed' '{"mid":0,"feed_id":0,"operator":"","real_ip":""}'
# 查询用户未读动态数
rpc 8092 'feed.v1.Feed/GetUnreadCount' '{"mid":0,"real_ip":""}'
# 清零未读计数
rpc 8092 'feed.v1.Feed/ClearUnread' '{"mid":0,"real_ip":""}'
# 删除自己的动态
rpc 8092 'feed.v1.Feed/DeleteFeed' '{"mid":0,"feed_id":0,"operator":"","real_ip":""}'

# ---------- playback (端口 8102) ----------
# 签发短期防盗链播放地址并创建播放会话（request_id 幂等）
rpc 8102 'playback.v1.Playback/GetPlaybackToken' '{"content_type":{},"content_id":0,"vid":"","object_key":"","mid":0,"platform":{},"app_version":"","region":"","request_id":"","trace_id":""}'
# 边缘/网关回源校验 auth_key 与会话有效期，并按会话首次放行累加播放计数
rpc 8102 'playback.v1.Playback/VerifyPlaybackToken' '{"session_id":"","uri":"","auth_key":"","client_ip":""}'
# 上报播放心跳：幂等更新进度，并在同一事务写 playback.heartbeat 事件（Outbox）
rpc 8102 'playback.v1.Playback/ReportHeartbeat' '{"session_id":"","position_ms":0,"duration_ms":0,"buffer_count":0,"avg_bitrate":0,"last_error":0,"trace_id":""}'
# 查询播放会话与进度详情（管理后台/排障）
rpc 8102 'playback.v1.Playback/GetSession' '{"session_id":""}'

# ---------- search-query (端口 8107) ----------
# Search 关键词搜索（cursor 优先分页；引擎不可用时返回明确错误码）。
rpc 8107 'searchquery.v1.SearchQuery/Search' '{"keyword":"","search_type":{},"zone_id":0,"duration":{},"sort":{},"pn":0,"ps":0,"cursor":"","viewer_mid":0,"platform":"","app_version":"","request_id":"","trace_id":"","published_after":0,"published_before":0}'
# Suggest 输入前缀联想（Redis ZSET 词典 + 冷启动回源索引前缀查询）。
rpc 8107 'searchquery.v1.SearchQuery/Suggest' '{"keyword":"","limit":0,"viewer_mid":0,"platform":"","app_version":"","search_type":{},"trace_id":""}'
# HotKeywords 全站/分区热词（读 DB 快照表 + Redis 缓存）。
rpc 8107 'searchquery.v1.SearchQuery/HotKeywords' '{"scope":"","limit":0,"viewer_mid":0,"platform":"","trace_id":""}'
# GetSearchConfig 该端/分区的排序与分页能力配置（无 UI 硬编码）。
rpc 8107 'searchquery.v1.SearchQuery/GetSearchConfig' '{"platform":"","app_version":"","search_type":{},"zone_id":0,"trace_id":""}'
# ListSearchHistory 用户搜索历史（cursor 倒序分页）。
rpc 8107 'searchquery.v1.SearchQuery/ListSearchHistory' '{"mid":0,"cursor":"","limit":0,"platform":"","trace_id":""}'
# DeleteSearchHistory 删除单个历史词（需 confirm=true，物理删除）。
rpc 8107 'searchquery.v1.SearchQuery/DeleteSearchHistory' '{"mid":0,"keyword":"","confirm":false,"request_id":"","trace_id":""}'
# ClearSearchHistory 清空用户全部历史（需 confirm=true，物理删除）。
rpc 8107 'searchquery.v1.SearchQuery/ClearSearchHistory' '{"mid":0,"confirm":false,"request_id":"","trace_id":""}'
# ReportQuery 上报查询行为（写 query_log 与 search_outbox，同事务）。
rpc 8107 'searchquery.v1.SearchQuery/ReportQuery' '{"query_id":"","keyword":"","search_type":{},"mid":0,"hit_count":0,"result_state":{},"latency_ms":0,"platform":"","app_version":"","ip_hash":"","trace_id":"","device_id_hash":""}'

# ---------- social-graph (端口 8091) ----------
# 关注（幂等：重复不重复计数）
rpc 8091 'socialgraph.v1.SocialGraph/Follow' '{"mid":0,"follower_mid":0,"real_ip":""}'
# 取关（幂等）
rpc 8091 'socialgraph.v1.SocialGraph/Unfollow' '{"mid":0,"follower_mid":0,"real_ip":""}'
# 查询 mid 是否关注 owner
rpc 8091 'socialgraph.v1.SocialGraph/IsFollowing' '{"mid":0,"owner":0,"real_ip":""}'
# 批量查询 mid 是否关注 owners
rpc 8091 'socialgraph.v1.SocialGraph/IsFollowedBatch' '{"mid":0,"owners":[0],"real_ip":""}'
# 批量查询 owner 与 mids 的全部关系位（双向关注 + 拉黑 + 特别关注）
rpc 8091 'socialgraph.v1.SocialGraph/RichRelations' '{"owner":0,"mids":[0],"real_ip":""}'
# mid 的关注列表（分页）
rpc 8091 'socialgraph.v1.SocialGraph/ListFollowing' '{"mid":0,"pn":0,"ps":0,"real_ip":""}'
# mid 的粉丝列表（分页）
rpc 8091 'socialgraph.v1.SocialGraph/ListFollower' '{"mid":0,"pn":0,"ps":0,"real_ip":""}'
# 查询关注数与粉丝数
rpc 8091 'socialgraph.v1.SocialGraph/Stat' '{"mid":0,"real_ip":""}'
# 拉黑（自动取关）
rpc 8091 'socialgraph.v1.SocialGraph/AddBlack' '{"mid":0,"black_mid":0,"real_ip":""}'
# 取消拉黑
rpc 8091 'socialgraph.v1.SocialGraph/DelBlack' '{"mid":0,"black_mid":0,"real_ip":""}'
# 查询 mid 是否拉黑 owner
rpc 8091 'socialgraph.v1.SocialGraph/IsBlacked' '{"mid":0,"owner":0,"real_ip":""}'
# mid 的黑名单列表（分页）
rpc 8091 'socialgraph.v1.SocialGraph/ListBlacks' '{"mid":0,"pn":0,"ps":0,"real_ip":""}'
# 特别关注（必先关注）
rpc 8091 'socialgraph.v1.SocialGraph/AddSpecial' '{"mid":0,"special_mid":0,"real_ip":""}'
# 取消特别关注
rpc 8091 'socialgraph.v1.SocialGraph/DelSpecial' '{"mid":0,"special_mid":0,"real_ip":""}'

# ---------- upload (端口 8098) ----------
# 初始化上传会话，返回 upload_id 和 OSS bucket/object_key 占位
rpc 8098 'upload.v1.Upload/InitUpload' '{"mid":0,"filename":"","size":0,"typeid":0,"md5":"","chunk_size":0,"total_chunks":0,"ip":""}'
# 为某分片获取 OSS 预签名 PUT URL（短期，默认 15 分钟有效）
rpc 8098 'upload.v1.Upload/GetUploadUrl' '{"upload_id":"","chunk_no":0,"chunk_size":0,"ip":""}'
# 客户端上传完全部分片后调用，服务端校验分片清单并触发 OSS 完成分片上传； / 返回 asset_id 占位（后续 asset 服务接管），并发布 media.task.v1 事件
rpc 8098 'upload.v1.Upload/CompleteUpload' '{"upload_id":"","parts":[{"chunk_no":0,"etag":""}],"md5":"","ip":""}'
# 取消上传（删除 OSS 分片）
rpc 8098 'upload.v1.Upload/AbortUpload' '{"upload_id":"","ip":""}'
# 查询上传状态和已完成分片列表
rpc 8098 'upload.v1.Upload/GetUploadStatus' '{"upload_id":"","ip":""}'

# @@ 消费面：只有运营面消费 —— 17 个服务 / 278 个方法 @@

# ---------- asset (端口 8099) ----------
# 上传完成后登记媒资；返回 asset_id
rpc 8099 'asset.v1.Asset/RegisterAsset' '{"upload_id":0,"mid":0,"bucket":"","object_key":"","size":0,"md5":"","ip":""}'
# 查询单个媒资元数据
rpc 8099 'asset.v1.Asset/GetAsset' '{"asset_id":0,"ip":""}'
# 分页查询媒资列表（可按 mid 或 state 过滤）
rpc 8099 'asset.v1.Asset/ListAssets' '{"mid":0,"state":{},"pn":0,"ps":0,"ip":""}'
# 更新媒资元数据（transcode 完成回调写入 duration/width/height/codec）
rpc 8099 'asset.v1.Asset/UpdateAssetMeta' '{"asset_id":0,"duration":0,"width":0,"height":0,"codec":"","ip":""}'
# 推进媒资状态机（UPLOADED→SCANNED→TRANSCODED）
rpc 8099 'asset.v1.Asset/TransitionState' '{"asset_id":0,"to_state":{},"ip":""}'
# 添加封面
rpc 8099 'asset.v1.Asset/AddCover' '{"asset_id":0,"bucket":"","object_key":"","width":0,"height":0,"ip":""}'
# 查询某媒资的封面列表
rpc 8099 'asset.v1.Asset/ListCovers' '{"asset_id":0,"ip":""}'
# 添加字幕
rpc 8099 'asset.v1.Asset/AddSubtitle' '{"asset_id":0,"lang":"","bucket":"","object_key":"","ip":""}'
# 查询某媒资的字幕列表
rpc 8099 'asset.v1.Asset/ListSubtitles' '{"asset_id":0,"ip":""}'
# 添加截图
rpc 8099 'asset.v1.Asset/AddScreenshot' '{"asset_id":0,"bucket":"","object_key":"","timestamp":0,"ip":""}'

# ---------- audit (端口 8110) ----------
# 追加一条审计（event_id 幂等）
rpc 8110 'audit.v1.Audit/AppendAudit' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"entry":{"event_id":"","schema_version":0,"actor_type":{},"actor_id":0,"actor_name":"","action":"","action_domain":"","target_type":"","target_id":"","result":{},"before_digest":"","after_digest":"","reason":"","source_app":{},"ip":"","device_id":"","occurred_at":0}}'
# 批量追加（Outbox 重放/领域服务批量留痕；任一条非法整批拒绝）
rpc 8110 'audit.v1.Audit/BatchAppendAudit' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"entries":[{"event_id":"","schema_version":0,"actor_type":{},"actor_id":0,"actor_name":"","action":"","action_domain":"","target_type":"","target_id":"","result":{},"before_digest":"","after_digest":"","reason":"","source_app":{},"ip":"","device_id":"","occurred_at":0}]}'
# 按 entry_id / event_id 取单条
rpc 8110 'audit.v1.Audit/GetAuditEntry' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"entry_id":0,"event_id":""}'
# 强约束分页查询（时间范围 + 收窄维度必填）
rpc 8110 'audit.v1.Audit/ListAuditEntries' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"start_at":0,"end_at":0,"actor_type":{},"actor_id":0,"action":"","action_domain":"","target_type":"","target_id":"","trace_id":"","result":{},"source_app":{},"pn":0,"ps":0}'
# 哈希链完整性自证（按链区间重放校验）
rpc 8110 'audit.v1.Audit/VerifyAuditChain' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"chain_key":"","from_seq":0,"to_seq":0,"max_entries":0}'
# 提交导出任务（request_id 幂等；导出不走同步大查询）
rpc 8110 'audit.v1.Audit/CreateAuditExport' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"start_at":0,"end_at":0,"actor_type":{},"actor_id":0,"action":"","action_domain":"","target_type":"","target_id":"","format":"","reason":""}'
# 查询导出任务，必要时签发短期下载地址
rpc 8110 'audit.v1.Audit/GetAuditExport' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"task_id":0,"request_id":""}'
# 分页列出导出任务
rpc 8110 'audit.v1.Audit/ListAuditExports' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"operator_id":0,"state":"","start_at":0,"end_at":0,"pn":0,"ps":0}'
# 推进一个导出任务（由 services/cron 或人工触发，本服务不内置 worker）
rpc 8110 'audit.v1.Audit/RunAuditExportTask' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"task_id":0,"batch_rows":0}'
# 分页列出保留期策略
rpc 8110 'audit.v1.Audit/ListRetentionPolicies' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"state":0,"pn":0,"ps":0}'
# 新建/更新保留期策略（expect_version 乐观锁）
rpc 8110 'audit.v1.Audit/SaveRetentionPolicy' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"action_domain":"","hot_days":0,"archive_after_days":0,"delete_after_days":0,"state":0,"expect_version":0,"remark":""}'
# 归档一个链区间（request_id 幂等，先落清单再标记账号）
rpc 8110 'audit.v1.Audit/ArchiveAuditEntries' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"chain_key":"","from_seq":0,"to_seq":0,"purge_hot":false}'
# 分页列出归档批次
rpc 8110 'audit.v1.Audit/ListArchiveBatches' '{"ctx":{"caller_service":"","operator_id":0,"trace_id":"","request_id":"","ip":"","user_agent":""},"chain_key":"","state":"","start_at":0,"end_at":0,"pn":0,"ps":0}'

# ---------- cron (端口 8112) ----------
# 注册任务定义（task_key 唯一，重复注册幂等返回）。
rpc 8112 'cron.v1.Cron/RegisterTask' '{"definition":{"task_id":0,"task_key":"","name":"","handler":"","task_group":"","schedule_type":{},"cron_expr":"","interval_seconds":0,"timezone":"","timeout_seconds":0,"max_attempts":0,"retry_base_seconds":0,"retry_max_seconds":0,"concurrency_limit":0,"lease_ttl_seconds":0,"misfire_policy":{},"misfire_backfill_limit":0,"params":"","secret_refs":"","state":{},"next_fire_at":0,"last_fire_at":0,"last_success_at":0,"last_error":"","version":0,"owner":"","operator":"","ctime":0,"mtime":0},"idempotency_key":"","operator":"","trace_id":""}'
# 修改任务定义（乐观锁）。
rpc 8112 'cron.v1.Cron/UpdateTask' '{"task_key":"","definition":{"task_id":0,"task_key":"","name":"","handler":"","task_group":"","schedule_type":{},"cron_expr":"","interval_seconds":0,"timezone":"","timeout_seconds":0,"max_attempts":0,"retry_base_seconds":0,"retry_max_seconds":0,"concurrency_limit":0,"lease_ttl_seconds":0,"misfire_policy":{},"misfire_backfill_limit":0,"params":"","secret_refs":"","state":{},"next_fire_at":0,"last_fire_at":0,"last_success_at":0,"last_error":"","version":0,"owner":"","operator":"","ctime":0,"mtime":0},"expected_version":0,"operator":"","trace_id":""}'
# 查询单个任务定义。
rpc 8112 'cron.v1.Cron/GetTask' '{"task_key":""}'
# 分页列出任务定义。
rpc 8112 'cron.v1.Cron/ListTasks' '{"state":{},"task_group":"","handler":"","cursor":"","page_size":0}'
# 暂停任务（可恢复）。
rpc 8112 'cron.v1.Cron/PauseTask' '{"task_key":"","reason":"","expected_version":0,"idempotency_key":"","operator":"","trace_id":""}'
# 恢复任务，按 MisfirePolicy 处理暂停期间的过期计划点。
rpc 8112 'cron.v1.Cron/ResumeTask' '{"task_key":"","expected_version":0,"idempotency_key":"","operator":"","trace_id":""}'
# 停用任务（终态，保留历史）。
rpc 8112 'cron.v1.Cron/DisableTask' '{"task_key":"","reason":"","expected_version":0,"idempotency_key":"","operator":"","trace_id":""}'
# 立即触发一次执行，或补跑指定计划时刻。
rpc 8112 'cron.v1.Cron/TriggerTask' '{"task_key":"","params":"","planned_at":0,"idempotency_key":"","operator":"","trace_id":""}'
# 拉取到期任务清单（只读，不占租约）。
rpc 8112 'cron.v1.Cron/ListDueTasks' '{"now":0,"limit":0,"task_group":"","lookahead_seconds":0}'
# 抢占某个计划时刻（同事务写租约与执行记录）。
rpc 8112 'cron.v1.Cron/AcquireLease' '{"task_key":"","planned_at":0,"attempt":0,"scope":"","owner":"","ttl_seconds":0,"trigger_type":{},"trace_id":""}'
# 心跳续租，栅栏令牌不一致时返回租约已失效。
rpc 8112 'cron.v1.Cron/RenewLease' '{"run_id":0,"owner":"","fence_token":0,"ttl_seconds":0}'
# 释放租约并以 SKIPPED/CANCELED 终结执行记录。
rpc 8112 'cron.v1.Cron/ReleaseLease' '{"run_id":0,"owner":"","fence_token":0,"final_state":{},"reason":""}'
# 上报执行结果（幂等，可带游标 CAS）。
rpc 8112 'cron.v1.Cron/ReportTaskResult' '{"run_id":0,"owner":"","fence_token":0,"state":{},"result_summary":"","error_message":"","checkpoint":{"task_key":"","scope_key":"","value":0,"value_str":"","version":0,"operator":"","ctime":0,"mtime":0},"trace_id":""}'
# 查询单条执行记录。
rpc 8112 'cron.v1.Cron/GetTaskRun' '{"run_id":0}'
# 分页查询执行记录。
rpc 8112 'cron.v1.Cron/ListTaskRuns' '{"task_key":"","state":{},"planned_from":0,"planned_to":0,"cursor":"","page_size":0}'
# 人工重试已终结的执行（同计划时刻追加 attempt）。
rpc 8112 'cron.v1.Cron/RetryRun' '{"run_id":0,"idempotency_key":"","operator":"","reason":"","trace_id":""}'
# 查询单个游标。
rpc 8112 'cron.v1.Cron/GetCheckpoint' '{"task_key":"","scope_key":""}'
# 分页查询游标。
rpc 8112 'cron.v1.Cron/ListCheckpoints' '{"task_key":"","cursor":"","page_size":0}'
# 独立推进游标（CAS）。
rpc 8112 'cron.v1.Cron/SaveCheckpoint' '{"checkpoint":{"task_key":"","scope_key":"","value":0,"value_str":"","version":0,"operator":"","ctime":0,"mtime":0},"expected_version":0,"idempotency_key":"","operator":"","trace_id":""}'
# 查询单个任务级租约。
rpc 8112 'cron.v1.Cron/GetLease' '{"task_key":"","scope":""}'
# 分页查询租约（含已过期可抢占项）。
rpc 8112 'cron.v1.Cron/ListLeases' '{"task_key":"","only_expired":false,"now":0,"cursor":"","page_size":0}'
# 分页查询任务变更审计。
rpc 8112 'cron.v1.Cron/ListTaskAudits' '{"task_key":"","action":"","ctime_from":0,"ctime_to":0,"cursor":"","page_size":0}'
# 调度健康度（积压、运行中、退避、近一小时失败、过期租约）。
rpc 8112 'cron.v1.Cron/GetSchedulerHealth' '{"now":0,"task_group":""}'

# ---------- event-collector (端口 8152) ----------
# 客户端 SDK 批量上报（batch_id 幂等，受条数/字节/限流约束）。
rpc 8152 'eventcollector.v1.EventCollector/CollectEvents' '{"batch_id":"","source":{},"context":{"mid":0,"device_id":"","device_type":"","device_hash":"","ip":"","ip_segment":"","platform":{},"app_id":"","app_version":"","sdk_version":"","os_version":"","network_type":"","model":"","region":"","session_id":"","page":"","spm":""},"events":[{"event_id":"","event_type":"","schema_version":0,"occurred_at":0,"reported_at":0,"category":{},"trace_id":"","content_type":"","content_id":0,"aid":0,"vid":"","target_mid":0,"session_id":"","position_ms":0,"duration_ms":0,"buffer_count":0,"first_frame_ms":0,"avg_bitrate":0,"error_code":"","keyword":"","result_index":0,"target_url":"","payload":""}],"policy_version":"","client_seq":0,"request_id":""}'
# 服务端内部埋点上报（不采样、trace_id 必填、要求服务身份）。
rpc 8152 'eventcollector.v1.EventCollector/IngestServerEvents' '{"batch_id":"","caller_service":"","idempotency_key":"","context":{"mid":0,"device_id":"","device_type":"","device_hash":"","ip":"","ip_segment":"","platform":{},"app_id":"","app_version":"","sdk_version":"","os_version":"","network_type":"","model":"","region":"","session_id":"","page":"","spm":""},"events":[{"event_id":"","event_type":"","schema_version":0,"occurred_at":0,"reported_at":0,"category":{},"trace_id":"","content_type":"","content_id":0,"aid":0,"vid":"","target_mid":0,"session_id":"","position_ms":0,"duration_ms":0,"buffer_count":0,"first_frame_ms":0,"avg_bitrate":0,"error_code":"","keyword":"","result_index":0,"target_url":"","payload":""}],"trace_id":""}'
# 单事件干跑校验：不落库不投递，返回缺失字段与归一化结果。
rpc 8152 'eventcollector.v1.EventCollector/ValidateEventSchema' '{"source":{},"event":{"event_id":"","event_type":"","schema_version":0,"occurred_at":0,"reported_at":0,"category":{},"trace_id":"","content_type":"","content_id":0,"aid":0,"vid":"","target_mid":0,"session_id":"","position_ms":0,"duration_ms":0,"buffer_count":0,"first_frame_ms":0,"avg_bitrate":0,"error_code":"","keyword":"","result_index":0,"target_url":"","payload":""},"context":{"mid":0,"device_id":"","device_type":"","device_hash":"","ip":"","ip_segment":"","platform":{},"app_id":"","app_version":"","sdk_version":"","os_version":"","network_type":"","model":"","region":"","session_id":"","page":"","spm":""}}'
# 查询批次接收台账。
rpc 8152 'eventcollector.v1.EventCollector/GetIngestBatch' '{"batch_id":""}'
# 分页查询批次台账。
rpc 8152 'eventcollector.v1.EventCollector/ListIngestBatches' '{"source":{},"state":{},"mid":0,"device_hash":"","ip_segment":"","ctime_from":0,"ctime_to":0,"cursor":"","page_size":0}'
# 查询单条事件的校验结论与投递状态。
rpc 8152 'eventcollector.v1.EventCollector/GetEventRecord' '{"event_id":""}'
# 分页查询事件台账。
rpc 8152 'eventcollector.v1.EventCollector/ListEventRecords' '{"batch_id":"","event_type":"","category":{},"decision":{},"reason":{},"delivery_state":{},"topic":"","mid":0,"device_hash":"","ctime_from":0,"ctime_to":0,"cursor":"","page_size":0}'
# 推进到期未发送事件（cron 兜底任务与运维入口）。
rpc 8152 'eventcollector.v1.EventCollector/RetryPendingDelivery' '{"topic":"","now":0,"limit":0,"idempotency_key":"","operator":""}'
# 分页查询投递死信。
rpc 8152 'eventcollector.v1.EventCollector/ListDeadLetters' '{"topic":"","state":"","ctime_from":0,"ctime_to":0,"cursor":"","page_size":0}'
# 重放投递死信。
rpc 8152 'eventcollector.v1.EventCollector/ReplayDeadLetter' '{"dead_letter_ids":[0],"idempotency_key":"","operator":"","reason":""}'
# 新建/修改采样与脱敏策略草稿。
rpc 8152 'eventcollector.v1.EventCollector/UpsertDispatchPolicy' '{"policy":{"version":"","state":{},"sample_rules":[{}],"salt_version":0,"salt_ref":"","field_whitelist":[""],"drop_fields":[""],"max_events_per_batch":0,"max_request_bytes":0,"max_event_payload_bytes":0,"max_clock_skew_seconds":0,"max_backfill_seconds":0,"keyword_max_runes":0,"retention_days":0,"deliver_max_attempts":0,"retry_base_seconds":0,"retry_max_seconds":0,"note":"","operator":"","ctime":0,"mtime":0},"idempotency_key":"","operator":""}'
# 切换生效策略版本（旧版本转 ARCHIVED，保留归因）。
rpc 8152 'eventcollector.v1.EventCollector/ActivateDispatchPolicy' '{"version":"","expected_current_version":"","idempotency_key":"","operator":"","reason":""}'
# 查询当前生效策略。
rpc 8152 'eventcollector.v1.EventCollector/GetActiveDispatchPolicy' '{}'
# 分页查询策略版本。
rpc 8152 'eventcollector.v1.EventCollector/ListDispatchPolicies' '{"state":{},"cursor":"","page_size":0}'
# 采集与投递健康度。
rpc 8152 'eventcollector.v1.EventCollector/GetCollectorHealth' '{"now":0}'

# ---------- feature-store (端口 8130) ----------
# 注册特征版本（privacy_level 必填，不可变字段命中冲突时拒绝）
rpc 8130 'featurestore.v1.FeatureStore/RegisterFeature' '{"definition":{"feature_key":"","version":0,"name":"","value_type":{},"entity_scope":{},"source":{},"privacy_level":{},"window_seconds":0,"ttl_seconds":0,"default_value":"","dimension":0,"state":{},"description":"","change_note":"","created_by":"","ctime":0,"mtime":0},"operator":"","request_id":""}'
# 变更特征状态（DRAFT/ACTIVE/RETIRED）
rpc 8130 'featurestore.v1.FeatureStore/UpdateFeatureState' '{"feature_key":"","version":0,"state":{},"operator":"","reason":"","request_id":""}'
# 调整隐私级别（独立入口，单独留痕）
rpc 8130 'featurestore.v1.FeatureStore/UpdateFeaturePrivacy' '{"feature_key":"","version":0,"privacy_level":{},"operator":"","reason":"","request_id":""}'
# 查询单个特征定义
rpc 8130 'featurestore.v1.FeatureStore/GetFeatureDefinition' '{"feature_key":"","version":0}'
# 特征定义列表（分页、按 scope/source/state/隐私级别过滤）
rpc 8130 'featurestore.v1.FeatureStore/ListFeatureDefinitions' '{"feature_key_prefix":"","entity_scope":{},"source":{},"state":{},"max_privacy_level":{},"pn":0,"ps":0}'
# 批量写入特征值（request_id 整批幂等，逐行返回结果）
rpc 8130 'featurestore.v1.FeatureStore/WriteFeatures' '{"writes":[{"feature":{},"entity":{},"value":{},"event_time":0,"source_metric_key":""}],"writer":{},"request_id":"","operator":""}'
# 读取单个特征值（缺失必降级，降级必须显式表达）
rpc 8130 'featurestore.v1.FeatureStore/GetFeature' '{"feature":{"feature_key":"","version":0},"entity":{"entity_scope":{},"entity_id":""},"allow_stale":false}'
# 批量读取（feature × entity 笛卡尔积，有硬上限）
rpc 8130 'featurestore.v1.FeatureStore/BatchGetFeatures' '{"features":[{"feature_key":"","version":0}],"entities":[{"entity_scope":{},"entity_id":""}],"allow_stale":false}'
# 切换对外生效的版本（乐观校验 + 审计留痕）
rpc 8130 'featurestore.v1.FeatureStore/SwitchFeatureVersion' '{"feature_key":"","from_version":0,"to_version":0,"expected_from_version":0,"operator":"","reason":"","request_id":""}'
# 版本切换审计列表
rpc 8130 'featurestore.v1.FeatureStore/ListVersionSwitches' '{"feature_key":"","since":0,"pn":0,"ps":0}'
# 提交回填任务（request_id 幂等）
rpc 8130 'featurestore.v1.FeatureStore/SubmitBackfillJob' '{"feature_key":"","version":0,"entity_scope":{},"entity_ids":[""],"window_from":0,"window_to":0,"source":{},"auto_switch":false,"from_version":0,"request_id":"","operator":"","reason":""}'
# 查询回填任务（按 job_id 或 request_id）
rpc 8130 'featurestore.v1.FeatureStore/GetBackfillJob' '{"job_id":0,"request_id":""}'
# 回填任务列表（分页）
rpc 8130 'featurestore.v1.FeatureStore/ListBackfillJobs' '{"feature_key":"","state":{},"since":0,"pn":0,"ps":0}'
# 清理 TTL 过期值（cron 调用）
rpc 8130 'featurestore.v1.FeatureStore/PurgeExpired' '{"limit":0,"before":0,"request_id":"","operator":""}'
# 按主体删除个体特征（隐私工单执行）
rpc 8130 'featurestore.v1.FeatureStore/EraseEntityFeatures' '{"entity":{"entity_scope":{},"entity_id":""},"min_privacy_level":{},"operator":"","reason":"","request_id":""}'
# 按主体导出特征（隐私核对）
rpc 8130 'featurestore.v1.FeatureStore/ListEntityFeatures' '{"entity":{"entity_scope":{},"entity_id":""},"min_privacy_level":{},"pn":0,"ps":0}'

# ---------- live-gateway (端口 8121) ----------
# --- 连接租约与心跳（主存储 Redis） --- / 申请连接租约：校验 (mid, room_id, role) 与配额，签发 lease_id + 重连票据
rpc 8121 'livegateway.v1.LiveGateway/AcquireConnectionLease' '{"room_id":0,"mid":0,"claimed_role":{},"conn_id":"","node_id":"","client":{"platform":{},"app_version":"","device_id_hash":"","network_type":""},"ttl_seconds":0,"request_id":"","trace_id":""}'
# 续租（TTL 刷新）：三元组不匹配一律拒绝，不静默改绑
rpc 8121 'livegateway.v1.LiveGateway/RenewConnectionLease' '{"lease_id":"","conn_id":"","room_id":0,"mid":0,"ttl_seconds":0,"trace_id":""}'
# 释放租约（正常断开/切房）
rpc 8121 'livegateway.v1.LiveGateway/ReleaseConnectionLease' '{"lease_id":"","conn_id":"","room_id":0,"mid":0,"reason":"","trace_id":""}'
# 查询租约（下发前的权限校验入口之一）
rpc 8121 'livegateway.v1.LiveGateway/GetConnectionLease' '{"lease_id":"","conn_id":"","room_id":0,"mid":0}'
# 客户端心跳簿记：推进 last_heartbeat/max_seq，抽样产出 QoE 事件
rpc 8121 'livegateway.v1.LiveGateway/ReportClientHeartbeat' '{"lease_id":"","conn_id":"","room_id":0,"mid":0,"client_time":0,"seq":0,"rtt_ms":0,"received_lag_ms":0,"trace_id":""}'
# 房间在线连接列表（Redis 视图，运营排障与主播工具）
rpc 8121 'livegateway.v1.LiveGateway/ListRoomConnections' '{"room_id":0,"mid":0,"role":{},"page":{"pn":0,"ps":0}}'
# --- 断线重连票据 --- / 以现有租约为凭据签发一次性重连票据
rpc 8121 'livegateway.v1.LiveGateway/IssueReconnectTicket' '{"lease_id":"","room_id":0,"mid":0,"ttl_seconds":0,"request_id":"","trace_id":""}'
# 用票据换取新租约（校验 mid/room/有效期，一次性消费）
rpc 8121 'livegateway.v1.LiveGateway/RedeemReconnectTicket' '{"ticket":"","room_id":0,"mid":0,"conn_id":"","node_id":"","ttl_seconds":0,"request_id":"","trace_id":""}'
# 撤销票据（封禁、踢人、房间关闭）
rpc 8121 'livegateway.v1.LiveGateway/RevokeReconnectTicket' '{"ticket_id":"","room_id":0,"mid":0,"reason":"","operator":"","request_id":"","trace_id":""}'
# --- 房间路由与订阅 --- / 加入房间（登记订阅关系 + 复用/创建房间路由）
rpc 8121 'livegateway.v1.LiveGateway/JoinRoom' '{"lease_id":"","conn_id":"","room_id":0,"mid":0,"node_id":"","topics":[""],"request_id":"","trace_id":""}'
# 退出房间
rpc 8121 'livegateway.v1.LiveGateway/LeaveRoom' '{"lease_id":"","conn_id":"","room_id":0,"mid":0,"topics":[""],"reason":"","trace_id":""}'
# 查询房间路由（广播第一跳与副本节点）
rpc 8121 'livegateway.v1.LiveGateway/GetRoomRoute' '{"room_id":0}'
# 分页查询房间路由（运营/发布排障）
rpc 8121 'livegateway.v1.LiveGateway/ListRoomRoutes' '{"node_id":"","state":{},"page":{"pn":0,"ps":0}}'
# 排空某节点上的房间路由（优雅下线，版本号乐观校验）
rpc 8121 'livegateway.v1.LiveGateway/DrainRoomRoute' '{"room_id":0,"node_id":"","expected_version":0,"target_node_id":"","reason":"","request_id":"","operator":"","trace_id":""}'
# --- 广播、单播与事件转发 --- / 房间广播：权限矩阵 + 配额限流 + message_id 去重，允许丢弃但原因必须可解释
rpc 8121 'livegateway.v1.LiveGateway/BroadcastToRoom' '{"room_id":0,"kind":{},"message_id":"","sender_mid":0,"sender_role":{},"sender_lease_id":"","sender_ticket":"","payload":"","target_roles":[""],"target_topics":[""],"expire_at":0,"priority":0,"require_reliable":false,"trace_id":""}'
# 房间内单播（审核处置、私信提示等）
rpc 8121 'livegateway.v1.LiveGateway/SendToUser' '{"room_id":0,"target_mid":0,"kind":{},"message_id":"","sender_mid":0,"sender_role":{},"sender_lease_id":"","sender_ticket":"","payload":"","require_reliable":false,"trace_id":""}'
# 弹幕转发（弹幕事实仍归 danmaku，本服务只扇出）
rpc 8121 'livegateway.v1.LiveGateway/ForwardDanmaku' '{"room_id":0,"danmaku_id":0,"sender_mid":0,"sender_lease_id":"","sender_ticket":"","content_digest":"","payload":"","sent_at":0,"message_id":"","trace_id":""}'
# 系统事件转发（开播/断流/下播/审核处置）
rpc 8121 'livegateway.v1.LiveGateway/ForwardSystemEvent' '{"room_id":0,"event_id":"","kind":{},"event_type":"","anchor_mid":0,"payload":"","require_reliable":false,"source_service":"","trace_id":""}'
# 强制下线（风控/审核/主播踢人），可同时撤销票据与写禁止重连窗口
rpc 8121 'livegateway.v1.LiveGateway/KickConnection' '{"room_id":0,"mid":0,"lease_id":"","conn_id":"","reason":"","ban_seconds":0,"revoke_tickets":false,"operator":"","request_id":"","trace_id":""}'
# --- 审计与配额配置 --- / 分页查询广播审计流水（按房间）
rpc 8121 'livegateway.v1.LiveGateway/ListBroadcastLogs' '{"room_id":0,"kind":{},"sender_mid":0,"only_dropped":false,"page":{"pn":0,"ps":0}}'
# 读取某作用域生效的配额（含继承链解析结果）
rpc 8121 'livegateway.v1.LiveGateway/GetAccessQuota' '{"scope":{},"scope_id":0}'
# 新建或更新配额配置（运营面，带版本与操作者审计）
rpc 8121 'livegateway.v1.LiveGateway/UpsertAccessQuota' '{"quota":{"scope":{},"scope_id":0,"scope_key":"","max_connections":0,"broadcast_qps":0,"danmaku_qps":0,"lease_ttl_seconds":0,"ticket_ttl_seconds":0,"max_payload_bytes":0,"allow_guest":false,"version":0,"updated_by":"","ctime":0,"mtime":0},"expected_version":0,"operator":"","request_id":"","trace_id":""}'

# ---------- live-ingest (端口 8118) ----------
# 签发推流密钥：入库只有哈希与 Secret/Vault 引用，明文只在本响应出现一次
rpc 8118 'liveingest.v1.LiveIngest/IssueStreamKey' '{"room_id":0,"anchor_mid":0,"session_id":0,"protocols":[{}],"ttl_seconds":0,"max_streams":0,"request_id":"","trace_id":""}'
# 接入鉴权（RTMP/SRT/WebRTC 入口在建连时调用）：比对哈希、协议、有效期、配额，可选建档 IDLE 流
rpc 8118 'liveingest.v1.LiveIngest/VerifyPublishAuth' '{"stream_name":"","plaintext_key":"","protocol":{},"room_id":0,"client_ip":"","client_version":"","create_stream":false,"request_id":"","trace_id":""}'
# 轮转密钥：新密钥生效、旧密钥进入宽限期，重连不中断
rpc 8118 'liveingest.v1.LiveIngest/RotateStreamKey' '{"key_id":0,"grace_seconds":0,"ttl_seconds":0,"request_id":"","operator_mid":0,"force":false,"trace_id":""}'
# 吊销密钥：立即失效，可按需级联停止进行中的流
rpc 8118 'liveingest.v1.LiveIngest/RevokeStreamKey' '{"key_id":0,"operator_mid":0,"admin":false,"stop_stream":false,"reason":"","request_id":"","trace_id":""}'
# 查询密钥元数据（永不回显明文或哈希）
rpc 8118 'liveingest.v1.LiveIngest/GetStreamKey' '{"key_id":0,"stream_name":""}'
# 分页查询密钥列表
rpc 8118 'liveingest.v1.LiveIngest/ListStreamKeys' '{"room_id":0,"anchor_mid":0,"state":{},"pn":0,"ps":0,"operator_mid":0,"admin":false}'
# 节点注册/心跳上报（幂等 upsert，运维面）
rpc 8118 'liveingest.v1.LiveIngest/UpsertIngestNode' '{"node":{"node_id":"","name":"","region":"","protocols":[{}],"endpoint_rtmp":"","endpoint_srt":"","endpoint_webrtc":"","state":{},"capacity_streams":0,"active_streams":0,"health_score":0,"last_heartbeat_at":0,"labels":"","ctime":0,"mtime":0},"create_if_absent":false,"heartbeat_only":false,"operator_mid":0,"request_id":"","trace_id":""}'
# 分页查询接入节点
rpc 8118 'liveingest.v1.LiveIngest/ListIngestNodes' '{"region":"","protocol":{},"state":{},"pn":0,"ps":0,"operator_mid":0}'
# 为流分配接入节点（就近 + 配额 + 健康分打分；支持迁移）
rpc 8118 'liveingest.v1.LiveIngest/AssignIngestNode' '{"stream_id":"","protocol":{},"prefer_region":"","prefer_node_id":"","force_reassign":false,"request_id":"","reason":"","trace_id":""}'
# 释放流的节点占用（停流或运维摘流）
rpc 8118 'liveingest.v1.LiveIngest/ReleaseIngestNode' '{"stream_id":"","node_id":"","reason":"","request_id":"","trace_id":""}'
# 分页查询节点分配记录（容量对账与排障）
rpc 8118 'liveingest.v1.LiveIngest/ListNodeAssignments' '{"stream_id":"","node_id":"","state":{},"pn":0,"ps":0,"operator_mid":0}'
# 上报流状态：按状态机 CAS 推进 + 分配 seq + 同事务写事件与 outbox（report_id 幂等）
rpc 8118 'liveingest.v1.LiveIngest/ReportStreamState' '{"stream_id":"","state":{},"node_id":"","reason":"","occurred_at":0,"report_id":"","expect_seq":0,"trace_id":""}'
# 查询单流当前状态（按 stream_id 或房间的活跃流）
rpc 8118 'liveingest.v1.LiveIngest/GetStreamState' '{"stream_id":"","room_id":0}'
# 分页查询流列表（开播巡检、断流扫描）
rpc 8118 'liveingest.v1.LiveIngest/ListStreams' '{"room_ids":[0],"node_id":"","state":{},"protocol":{},"heartbeat_before":0,"pn":0,"ps":0,"operator_mid":0,"admin":false}'
# 强制停流（主播下播、运营/风控切断；IDLE/PUBLISHING/INTERRUPTED → STOPPED）
rpc 8118 'liveingest.v1.LiveIngest/CloseStream' '{"stream_id":"","stop_reason":{},"reason":"","request_id":"","operator_mid":0,"admin":false,"trace_id":""}'
# 健康采样上报：更新最新健康字段并留采样点，越过危险阈值时触发 INTERRUPTED 迁移
rpc 8118 'liveingest.v1.LiveIngest/ReportStreamHealth' '{"stream_id":"","node_id":"","video_bitrate_bps":0,"audio_bitrate_bps":0,"fps_x100":0,"packet_loss_ppm":0,"rtt_ms":0,"sample_window_seconds":0,"occurred_at":0,"report_id":"","trace_id":""}'
# 流健康检查：当前判定 + 窗口聚合 + 最近采样点
rpc 8118 'liveingest.v1.LiveIngest/GetStreamHealth' '{"stream_id":"","window_seconds":0,"sample_limit":0}'
# 断流与重连记录查询
rpc 8118 'liveingest.v1.LiveIngest/ListStreamInterruptions' '{"stream_id":"","room_id":0,"only_open":false,"start_time":0,"end_time":0,"limit":0,"trace_id":""}'
# 事件位点：按 seq 游标拉取状态事件（live-room/live-media 补偿与对账）
rpc 8118 'liveingest.v1.LiveIngest/ListStreamEvents' '{"stream_id":"","after_seq":0,"limit":0,"desc":false}'
# Outbox 发布位点与滞后度（观测「事件必须可追踪」）
rpc 8118 'liveingest.v1.LiveIngest/GetEventPublishCheckpoint' '{"pending_limit":0,"include_failed":false}'
# 把超过重试上限的失败事件重置为待发布（运营补偿，request_id 幂等）
rpc 8118 'liveingest.v1.LiveIngest/RetryFailedEvents' '{"event_ids":[""],"limit":0,"request_id":"","operator_mid":0,"reason":"","trace_id":""}'
# CDN/入口回调鉴权：签名 + 时间窗 + nonce 防重放，只建议状态、由调用方走 ReportStreamState
rpc 8118 'liveingest.v1.LiveIngest/VerifyCdnCallback' '{"domain":"","stream_name":"","event_type":"","timestamp":0,"nonce":"","signature":"","client_ip":"","raw_params_digest":"","trace_id":""}'

# ---------- live-media (端口 8120) ----------
# --- 直播转码任务：启停、重试、超时、取消、进度上报 --- / 登记直播转码任务（PENDING），request_id 幂等；不在此调用 FFmpeg
rpc 8120 'livemedia.v1.LiveMedia/StartLiveTranscode' '{"room_id":0,"live_session_id":0,"template_id":0,"bitrate_level":{},"protocol":{},"source_ref":"","anchor_mid":0,"max_attempts":0,"timeout_seconds":0,"request_id":"","trace_id":""}'
# 请求停止（RUNNING→STOPPING，Worker 收尾后 STOPPED）
rpc 8120 'livemedia.v1.LiveMedia/StopLiveTranscode' '{"task_id":0,"expected_version":0,"reason":{},"request_id":"","operator":"","trace_id":""}'
# 重试失败任务（FAILED→PENDING，attempt+1，受 max_attempts 限制）
rpc 8120 'livemedia.v1.LiveMedia/RetryLiveTranscode' '{"task_id":0,"expected_version":0,"reason":"","request_id":"","operator":"","trace_id":""}'
# 取消未运行/停止中的任务（PENDING|STOPPING→CANCELLED 终态）
rpc 8120 'livemedia.v1.LiveMedia/CancelLiveTranscode' '{"task_id":0,"expected_version":0,"reason":{},"request_id":"","operator":"","trace_id":""}'
# Worker 上报心跳/进度/终态（含超时判定），条件 UPDATE + 版本校验
rpc 8120 'livemedia.v1.LiveMedia/ReportLiveTranscodeProgress' '{"task_id":0,"expected_version":0,"state":{},"progress":0,"reason":{},"errno":0,"err_msg":"","worker_id":"","trace_id":""}'
# 查询单个转码任务
rpc 8120 'livemedia.v1.LiveMedia/GetLiveTranscodeTask' '{"task_id":0}'
# 分页查询转码任务（房间/场次/状态/模板）
rpc 8120 'livemedia.v1.LiveMedia/ListLiveTranscodeTasks' '{"room_id":0,"live_session_id":0,"state":{},"template_id":0,"page":{"pn":0,"ps":0}}'
# --- 分发输出（直播实时链路） --- / 登记或刷新一个码率档位的分发输出（(room,session,level,protocol) 唯一）
rpc 8120 'livemedia.v1.LiveMedia/UpsertStreamOutput' '{"room_id":0,"live_session_id":0,"task_id":0,"bitrate_level":{},"protocol":{},"bucket":"","object_key":"","cdn_domain":"","width":0,"height":0,"bitrate_kbps":0,"fps":0,"online_expire_at":0,"request_id":"","trace_id":""}'
# 下线一个档位（断流/到期/人工），与回放发布状态无关
rpc 8120 'livemedia.v1.LiveMedia/OfflineStreamOutput' '{"output_id":0,"room_id":0,"bitrate_level":{},"protocol":{},"reason":{},"request_id":"","trace_id":""}'
# 查询房间当前可分发档位（live-gateway / live-room 只读投影）
rpc 8120 'livemedia.v1.LiveMedia/ListStreamOutputs' '{"room_id":0,"live_session_id":0,"include_offline":false,"page":{"pn":0,"ps":0}}'
# --- 录制任务与切片 --- / 登记录制任务（PENDING），request_id 幂等
rpc 8120 'livemedia.v1.LiveMedia/StartLiveRecord' '{"room_id":0,"live_session_id":0,"source_task_id":0,"start_at":0,"end_at":0,"segment_seconds":0,"timeout_seconds":0,"output_bucket":"","output_prefix":"","request_id":"","trace_id":""}'
# 停止录制（RECORDING→STOPPING，最后一片落库后 STOPPED）
rpc 8120 'livemedia.v1.LiveMedia/StopLiveRecord' '{"record_id":0,"expected_version":0,"end_at":0,"reason":{},"request_id":"","operator":"","trace_id":""}'
# Worker 上报录制心跳与状态（含超时/断点续录）
rpc 8120 'livemedia.v1.LiveMedia/ReportLiveRecordProgress' '{"record_id":0,"expected_version":0,"state":{},"last_seq":0,"heartbeat_at":0,"reason":{},"errno":0,"err_msg":"","worker_id":"","trace_id":""}'
# 查询单个录制任务（含 last_seq，供断点续录）
rpc 8120 'livemedia.v1.LiveMedia/GetLiveRecordTask' '{"record_id":0}'
# 分页查询录制任务
rpc 8120 'livemedia.v1.LiveMedia/ListLiveRecordTasks' '{"room_id":0,"live_session_id":0,"state":{},"page":{"pn":0,"ps":0}}'
# 逐片登记切片（(record_id,seq) 幂等，缺口必须显式登记 MISSING）
rpc 8120 'livemedia.v1.LiveMedia/ReportRecordSegment' '{"record_id":0,"seq":0,"start_at":0,"end_at":0,"duration_ms":0,"state":{},"bucket":"","object_key":"","size_bytes":0,"checksum":"","worker_id":"","trace_id":""}'
# keyset 分页拉取切片（回放拼接与排障）
rpc 8120 'livemedia.v1.LiveMedia/ListRecordSegments' '{"record_id":0,"state":{},"after_seq":0,"limit":0}'
# --- 回放拼接与资产引用 --- / 提交回放拼接任务（只登记与校验切片区间，不拼接、不发布）
rpc 8120 'livemedia.v1.LiveMedia/SubmitReplayTask' '{"room_id":0,"live_session_id":0,"record_id":0,"from_seq":0,"to_seq":0,"start_at":0,"end_at":0,"allow_gaps":false,"anchor_mid":0,"title":"","description":"","request_id":"","trace_id":""}'
# Worker 上报回放进度（拼接/上传/登记/送审）
rpc 8120 'livemedia.v1.LiveMedia/ReportReplayProgress' '{"replay_id":0,"expected_version":0,"state":{},"segment_count":0,"gap_count":0,"duration_ms":0,"output_bucket":"","output_key":"","reason":{},"errno":0,"err_msg":"","worker_id":"","trace_id":""}'
# 查询单个回放任务
rpc 8120 'livemedia.v1.LiveMedia/GetReplayTask' '{"replay_id":0}'
# 分页查询回放任务
rpc 8120 'livemedia.v1.LiveMedia/ListReplayTasks' '{"room_id":0,"live_session_id":0,"state":{},"page":{"pn":0,"ps":0}}'
# 回填回放产物与 asset/稿件的引用关系（只存引用，不推进稿件状态）
rpc 8120 'livemedia.v1.LiveMedia/BindReplayAsset' '{"replay_id":0,"asset_id":0,"aid":0,"bvid":"","bucket":"","object_key":"","duration_ms":0,"request_id":"","trace_id":""}'
# 同步 video 侧审核/发布投影（单向：video → live-media）
rpc 8120 'livemedia.v1.LiveMedia/ApplyReplayContentState' '{"replay_id":0,"asset_id":0,"review_state":{},"published_at":0,"event_id":"","source":"","trace_id":""}'
# 分页查询回放资产引用（房间/场次/主播/投影状态）
rpc 8120 'livemedia.v1.LiveMedia/ListReplayAssetRefs' '{"room_id":0,"live_session_id":0,"review_state":{},"anchor_mid":0,"page":{"pn":0,"ps":0}}'
# --- 回收任务 --- / 提交回收任务（超期切片/回放产物/残留档位），先登记后执行，保留审计证据
rpc 8120 'livemedia.v1.LiveMedia/SubmitRetentionTask' '{"target_kind":{},"room_id":0,"target_id":0,"expire_before":0,"purge":false,"batch_limit":0,"reason":"","request_id":"","operator":"","trace_id":""}'
# Worker 上报回收结果（扫描/删除/跳过计数）
rpc 8120 'livemedia.v1.LiveMedia/ReportRetentionResult' '{"retention_id":0,"expected_version":0,"state":{},"scanned":0,"deleted":0,"skipped":0,"fail_reason":{},"errno":0,"err_msg":"","worker_id":"","trace_id":""}'
# 查询单个回收任务
rpc 8120 'livemedia.v1.LiveMedia/GetRetentionTask' '{"retention_id":0}'
# 分页查询回收任务
rpc 8120 'livemedia.v1.LiveMedia/ListRetentionTasks' '{"target_kind":{},"state":{},"room_id":0,"page":{"pn":0,"ps":0}}'

# ---------- open-platform (端口 8151) ----------
# 注册应用（client_token 幂等），secret 仅此一次返回。
rpc 8151 'openplatform.v1.OpenPlatform/RegisterApplication' '{"name":"","description":"","owner_mid":0,"redirect_uris":[""],"scopes":[""],"client_token":"","trace_id":""}'
# 查询应用（不回显密钥）。
rpc 8151 'openplatform.v1.OpenPlatform/GetApplication' '{"app_id":0,"app_key":"","caller_mid":0,"operator":false,"trace_id":""}'
# 开发者/运营侧应用分页列表。
rpc 8151 'openplatform.v1.OpenPlatform/ListApplications' '{"owner_mid":0,"status":{},"cursor":"","ps":0,"operator":false,"trace_id":""}'
# 修改资料或推进状态机（乐观锁版本）。
rpc 8151 'openplatform.v1.OpenPlatform/UpdateApplication' '{"app_id":0,"name":"","description":"","redirect_uris":[""],"target_status":{},"expected_version":0,"operator_mid":0,"is_operator":false,"reason":"","trace_id":""}'
# 轮换密钥（旧密钥宽限期后可用性明确）。
rpc 8151 'openplatform.v1.OpenPlatform/RotateApplicationSecret' '{"app_id":0,"operator_mid":0,"is_operator":false,"grace_seconds":0,"reason":"","trace_id":""}'
# 吊销密钥（疑似泄露的应急处置）。
rpc 8151 'openplatform.v1.OpenPlatform/RevokeApplicationSecret' '{"app_id":0,"secret_id":0,"operator_mid":0,"is_operator":false,"reason":"","trace_id":""}'
# scope 目录（含读写与风险级别声明）。
rpc 8151 'openplatform.v1.OpenPlatform/ListScopes' '{"app_id":0,"only_enabled":false,"trace_id":""}'
# 运营审批 scope 授予/回收。
rpc 8151 'openplatform.v1.OpenPlatform/GrantApplicationScopes' '{"app_id":0,"grant":[""],"revoke":[""],"operator_mid":0,"reason":"","idempotency_key":"","trace_id":""}'
# 用户同意后签发短期授权码。
rpc 8151 'openplatform.v1.OpenPlatform/IssueAuthorizationCode' '{"app_id":0,"mid":0,"scope":[""],"redirect_uri":"","state":"","consent_given":false,"trace_id":""}'
# 授权码换 token（一次性消费 + 重放检测）。
rpc 8151 'openplatform.v1.OpenPlatform/ExchangeAuthorizationCode' '{"app_id":0,"code":"","redirect_uri":"","trace_id":""}'
# refresh token 轮换（旧值重放即撤销整条 grant）。
rpc 8151 'openplatform.v1.OpenPlatform/RefreshAccessToken' '{"app_id":0,"refresh_token":"","scope":[""],"trace_id":""}'
# 撤销授权（写撤销位点，立即生效）。
rpc 8151 'openplatform.v1.OpenPlatform/RevokeAuthorization' '{"target":{},"app_id":0,"mid":0,"token_id":0,"token_hint":"","operator_mid":0,"is_operator":false,"reason":"","trace_id":""}'
# token 校验（含 grant 撤销位点比对）。
rpc 8151 'openplatform.v1.OpenPlatform/IntrospectToken' '{"access_token":"","token_id":0,"api_code":"","required_scope":"","trace_id":""}'
# 网关前置聚合检查：凭证 + scope + 配额扣减 + 调用流水（request_id 幂等）。
rpc 8151 'openplatform.v1.OpenPlatform/AuthorizeRequest' '{"access_token":"","app_key":"","signature":"","timestamp":0,"nonce":"","method":"","path":"","body_digest":"","api_code":"","required_scope":"","request_id":"","client_ip":"","trace_id":""}'
# 新增/更新配额规则（运营）。
rpc 8151 'openplatform.v1.OpenPlatform/UpsertQuotaPolicy' '{"policy_id":0,"app_id":0,"api_code":"","window_seconds":0,"limit":0,"enabled":false,"operator_mid":0,"trace_id":""}'
# 配额规则分页。
rpc 8151 'openplatform.v1.OpenPlatform/ListQuotaPolicies' '{"app_id":0,"api_code":"","cursor":"","ps":0,"operator_mid":0,"trace_id":""}'
# 配额用量查询（投影）。
rpc 8151 'openplatform.v1.OpenPlatform/ListQuotaUsage' '{"app_id":0,"api_code":"","window_start":0,"operator_mid":0,"trace_id":""}'
# 从调用流水重算配额投影。
rpc 8151 'openplatform.v1.OpenPlatform/RecomputeQuota' '{"app_id":0,"api_code":"","window_start":0,"window_end":0,"dry_run":false,"operator_mid":0,"trace_id":""}'
# 注册回调端点（需验证后才投递）。
rpc 8151 'openplatform.v1.OpenPlatform/RegisterWebhook' '{"app_id":0,"event_type":{},"url":"","description":"","operator_mid":0,"is_operator":false,"trace_id":""}'
# 回调端点列表。
rpc 8151 'openplatform.v1.OpenPlatform/ListWebhooks' '{"app_id":0,"include_disabled":false,"operator_mid":0,"trace_id":""}'
# 删除回调端点（抑制未投递任务）。
rpc 8151 'openplatform.v1.OpenPlatform/DeleteWebhook' '{"app_id":0,"endpoint_id":0,"operator_mid":0,"is_operator":false,"reason":"","trace_id":""}'
# 领域事件入队（event_id 幂等）。
rpc 8151 'openplatform.v1.OpenPlatform/EnqueueWebhookEvent' '{"app_id":0,"event_type":{},"event_id":"","payload":"","occurred_at":0,"mid":0,"biz_type":"","biz_id":"","trace_id":""}'
# 投递记录分页（观测与排障）。
rpc 8151 'openplatform.v1.OpenPlatform/ListWebhookDeliveries' '{"app_id":0,"endpoint_id":0,"state":{},"cursor":"","ps":0,"operator_mid":0,"trace_id":""}'
# 死信重放（运营）。
rpc 8151 'openplatform.v1.OpenPlatform/RetryWebhookDelivery' '{"delivery_id":0,"operator_mid":0,"ignore_dead":false,"reason":"","trace_id":""}'

# ---------- operation (端口 8109) ----------
# 管理员登录（口令校验 + 防爆破锁定），签发后台专用 token
rpc 8109 'operation.v1.Operation/AdminLogin' '{"username":"","password":"","second_factor":"","ip":"","user_agent":"","trace_id":"","request_id":""}'
# 权限校验（gateway/admin 每个受保护路由调用；结果带短缓存）
rpc 8109 'operation.v1.Operation/VerifyAdminPermission' '{"token":"","admin_id":0,"resource":"","action":"","trace_id":""}'
# 创建管理员账号（口令只在入参出现，响应永不返回散列）
rpc 8109 'operation.v1.Operation/CreateAdminUser' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"username":"","password":"","remark":"","role_ids":[0],"second_factor_target":""}'
# 更新管理员账号（备注/状态/重置口令，重置口令会吊销会话）
rpc 8109 'operation.v1.Operation/UpdateAdminUser' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"admin_id":0,"remark":"","state":0,"new_password":"","second_factor_target":""}'
# 禁用管理员账号（同时吊销全部会话）
rpc 8109 'operation.v1.Operation/DisableAdminUser' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"admin_id":0,"reason":""}'
# 分页查询管理员账号
rpc 8109 'operation.v1.Operation/ListAdminUsers' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"state":0,"keyword":"","pn":0,"ps":0}'
# 全量覆盖管理员角色（并集生效）
rpc 8109 'operation.v1.Operation/AssignRoles' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"admin_id":0,"role_ids":[0]}'
# 创建角色并绑定权限点
rpc 8109 'operation.v1.Operation/CreateRole' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"name":"","title":"","permission_ids":[0]}'
# 分页查询角色
rpc 8109 'operation.v1.Operation/ListRoles' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"state":0,"keyword":"","pn":0,"ps":0}'
# 删除角色（仍有成员时拒绝）
rpc 8109 'operation.v1.Operation/DeleteRole' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"role_id":0}'
# 分页查询权限点
rpc 8109 'operation.v1.Operation/ListPermissions' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"domain":"","pn":0,"ps":0}'
# 创建权限点（resource + action 唯一）
rpc 8109 'operation.v1.Operation/CreatePermission' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"resource":"","action":"","domain":"","description":""}'
# 按管理员角色并集返回可见菜单（后台 Web 专用）
rpc 8109 'operation.v1.Operation/GetMenu' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"admin_id":0}'
# 新建/更新菜单节点
rpc 8109 'operation.v1.Operation/SaveMenu' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"menu_id":0,"parent_id":0,"name":"","path":"","icon":"","sort":0,"required_permission":"","state":0}'
# 读取运营配置（默认走缓存，refresh=true 强制回源）
rpc 8109 'operation.v1.Operation/GetOpsConfig' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"cfg_key":"","scope":"","refresh":false}'
# 写入运营配置（expect_version 乐观锁 + 操作者留痕）
rpc 8109 'operation.v1.Operation/SaveOpsConfig' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"cfg_key":"","cfg_value":"","value_type":"","scope":"","expect_version":0,"state":0,"remark":""}'
# 提交批量运营任务（request_id 幂等，状态 pending）
rpc 8109 'operation.v1.Operation/SubmitAdminTask' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"task_type":"","params":"","steps":[{"target_type":"","target_id":""}]}'
# 查询任务与步骤明细
rpc 8109 'operation.v1.Operation/GetAdminTask' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"task_id":0,"request_id":""}'
# 分页查询任务
rpc 8109 'operation.v1.Operation/ListAdminTasks' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"state":"","task_type":"","operator_id":0,"pn":0,"ps":0}'
# 取消任务（仅 pending/running 可取消）
rpc 8109 'operation.v1.Operation/CancelAdminTask' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"task_id":0,"reason":""}'
# 推进任务：逐步骤调用下游 RPC（由 cron 或人工触发，本服务不内置 worker）
rpc 8109 'operation.v1.Operation/RunAdminTask' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"task_id":0,"max_steps":0}'
# 查询管理操作审计索引（正文证据在下游服务/audit）
rpc 8109 'operation.v1.Operation/ListAuditIndex' '{"ctx":{"operator_id":0,"operator_name":"","ip":"","user_agent":"","trace_id":"","request_id":""},"admin_id":0,"action":"","resource_type":"","resource_id":"","start_at":0,"end_at":0,"pn":0,"ps":0}'

# ---------- ops-config (端口 8111) ----------
# 运行时解析单个配置（命中灰度规则，返回建议 TTL）
rpc 8111 'opsconfig.v1.OpsConfig/ResolveConfig' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"cfg_key":"","scope":"","target":{"platform":{},"app_version":"","mid":0,"ignore_rollout":false},"refresh":false}'
# 批量解析（网关聚合用，最多 50 键）
rpc 8111 'opsconfig.v1.OpsConfig/BatchResolveConfig' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"cfg_keys":[""],"scope":"","target":{"platform":{},"app_version":"","mid":0,"ignore_rollout":false}}'
# 后台分页列出配置项
rpc 8111 'opsconfig.v1.OpsConfig/ListConfigs' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"scope":"","keyword":"","state":0,"pn":0,"ps":0}'
# 发布新版本（乐观锁 + 幂等 + 可选一并挂灰度规则）
rpc 8111 'opsconfig.v1.OpsConfig/PublishConfig' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"cfg_key":"","scope":"","value_type":{},"value":"","expect_version":0,"reason":"","rollout":[{"name":"","mode":{},"percentage":0,"app_version_min":"","app_version_max":"","platforms":[{}],"mid_suffixes":"","whitelist_mids":[0],"priority":0,"remark":"","start_at":0,"end_at":0}]}'
# 回滚到历史版本（生成新版本，不改写历史）
rpc 8111 'opsconfig.v1.OpsConfig/RollbackConfig' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"cfg_key":"","scope":"","to_version":0,"reason":""}'
# 版本历史分页
rpc 8111 'opsconfig.v1.OpsConfig/ListConfigVersions' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"cfg_key":"","scope":"","pn":0,"ps":0}'
# 新建/更新灰度规则（按 config_id + version + name upsert）
rpc 8111 'opsconfig.v1.OpsConfig/SaveRolloutRule' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"cfg_key":"","scope":"","version":0,"rule":{"name":"","mode":{},"percentage":0,"app_version_min":"","app_version_max":"","platforms":[{}],"mid_suffixes":"","whitelist_mids":[0],"priority":0,"remark":"","start_at":0,"end_at":0}}'
# 灰度规则分页查询
rpc 8111 'opsconfig.v1.OpsConfig/ListRolloutRules' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"cfg_key":"","scope":"","version":0,"state":0,"pn":0,"ps":0}'
# 启停灰度规则（软状态切换，保留放量证据）
rpc 8111 'opsconfig.v1.OpsConfig/SetRolloutRuleState' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"rule_id":0,"state":0,"reason":""}'
# 新建/更新专题（zone_ids/tag_ids 只存引用）
rpc 8111 'opsconfig.v1.OpsConfig/SaveTopic' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"topic_id":0,"slug":"","title":"","description":"","cover":"","zone_ids":[0],"tag_ids":[0],"state":0,"sort":0,"start_at":0,"end_at":0,"expect_version":0,"reason":""}'
# 专题详情（可带条目）
rpc 8111 'opsconfig.v1.OpsConfig/GetTopic' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"topic_id":0,"slug":"","with_items":false,"item_limit":0}'
# 专题分页查询
rpc 8111 'opsconfig.v1.OpsConfig/ListTopics' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"state":0,"zone_id":0,"tag_id":0,"keyword":"","pn":0,"ps":0,"online_only":false}'
# 全量覆盖专题条目
rpc 8111 'opsconfig.v1.OpsConfig/SaveTopicItems' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"topic_id":0,"items":[{"id":0,"topic_id":0,"item_type":"","item_id":"","position":0,"state":0,"operator_id":0,"ctime":0,"mtime":0}],"reason":""}'
# 新建/更新推荐位定义
rpc 8111 'opsconfig.v1.OpsConfig/SaveSlot' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"slot_id":0,"code":"","page":"","title":"","platforms":[{}],"capacity":0,"state":0,"expect_version":0,"remark":"","reason":""}'
# 推荐位分页查询
rpc 8111 'opsconfig.v1.OpsConfig/ListSlots' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"page":"","state":0,"platform":{},"pn":0,"ps":0}'
# 全量覆盖坑位条目与排期
rpc 8111 'opsconfig.v1.OpsConfig/SaveSlotItems' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"slot_id":0,"items":[{"id":0,"slot_id":0,"position":0,"item_type":"","item_id":"","weight":0,"start_at":0,"end_at":0,"state":0,"operator_id":0,"ctime":0,"mtime":0}],"reason":""}'
# 运行时坑位视图（按端/版本/mid/时间过滤，只回引用）
rpc 8111 'opsconfig.v1.OpsConfig/ResolveSlot' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"code":"","target":{"platform":{},"app_version":"","mid":0,"ignore_rollout":false},"at":0,"limit":0}'
# 新建/更新客户端开关（按 switch_key + platform upsert）
rpc 8111 'opsconfig.v1.OpsConfig/SaveClientSwitch' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"switch_id":0,"switch_key":"","platform":{},"min_version":"","max_version":"","enabled":0,"config_id":0,"remark":"","expect_version":0,"reason":""}'
# 客户端开关分页查询
rpc 8111 'opsconfig.v1.OpsConfig/ListClientSwitches' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"platform":{},"switch_key":"","enabled":0,"pn":0,"ps":0}'
# 主动失效运行时缓存（必写审计）
rpc 8111 'opsconfig.v1.OpsConfig/RefreshCache' '{"ctx":{"operator_id":0,"operator_name":"","request_id":"","trace_id":"","caller_service":"","ip":""},"target":"","cfg_key":"","scope":"","topic_id":0,"slot_id":0,"reason":""}'

# ---------- recommend-rank (端口 8123) ----------
# 多目标排序：入参候选子集内排序，输出可审计结果摘要
rpc 8123 'recommendrank.v1.Rank/RankCandidates' '{"context":{"mid":0,"platform":{},"app_version":"","device_id_hash":"","region":"","scene":"","request_id":"","trace_id":""},"candidates":[{"aid":0,"source":{},"recall_score":0,"rank_in_source":0,"pool_version":0,"batch_id":""}],"snapshot_id":"","limit":0,"allow_degrade":false,"idempotency_key":""}'
# 按 decision_id/request_id 回放一次排序决策
rpc 8123 'recommendrank.v1.Rank/GetRankDecision' '{"decision_id":"","request_id":""}'
# 分页查排序决策摘要（审计与实验核对）
rpc 8123 'recommendrank.v1.Rank/ListRankDecisions' '{"exp_key":"","variant_key":"","model_key":"","model_version":"","scene":"","from_time":0,"to_time":0,"only_degraded":false,"pn":0,"ps":0}'
# 登记/更新模型版本元数据（版本不可变，元数据变更 revision+1）
rpc 8123 'recommendrank.v1.Rank/UpsertModelVersion' '{"model_key":"","version":"","feature_config_version":"","objective_weights":[{"objective":"","weight":0}],"artifact_ref":"","offline_metrics":"","operator":"","reason":"","idempotency_key":""}'
# 切换模型版本状态（READY/ACTIVE/RETIRED；激活是回滚开关）
rpc 8123 'recommendrank.v1.Rank/SetModelVersionState' '{"model_key":"","version":"","target_state":{},"operator":"","reason":"","idempotency_key":""}'
# 登记/更新特征配置版本
rpc 8123 'recommendrank.v1.Rank/UpsertFeatureConfig' '{"config_version":"","feature_keys":[""],"missing_policy":"","feature_store_scene":"","operator":"","reason":"","idempotency_key":""}'
# 新建/修改实验变体（同 (exp_key,variant_key) 唯一，变更 revision+1）
rpc 8123 'recommendrank.v1.Rank/UpsertExperiment' '{"exp_key":"","variant_key":"","layer_key":"","hash_seed":"","bucket_start":0,"bucket_end":0,"model_key":"","model_version":"","feature_config_version":"","overrides":"","start_at":0,"end_at":0,"operator":"","reason":"","idempotency_key":""}'
# 实验状态迁移（RUNNING/PAUSED/STOPPED）
rpc 8123 'recommendrank.v1.Rank/SetExperimentState' '{"exp_key":"","variant_key":"","target_state":{},"operator":"","reason":"","idempotency_key":""}'
# 查询/登记主体在实验中的稳定分桶
rpc 8123 'recommendrank.v1.Rank/GetExperimentAssignment' '{"exp_key":"","subject_type":{},"subject_id":"","bucket_count":0}'
# 下发在线排序参数与当前生效的模型/特征/实验状态
rpc 8123 'recommendrank.v1.Rank/GetRankRuntimeConfig' '{"scene":"","model_key":""}'

# ---------- recommend-recall (端口 8116) ----------
# 多路召回主入口：按路取数 -> 过滤 -> 去重合并 -> 显式降级声明
rpc 8116 'recommendrecall.v1.Recall/RecallCandidates' '{"context":{"mid":0,"platform":{},"app_version":"","device_id_hash":"","region":"","scene":"","request_id":"","trace_id":""},"sources":[{}],"limit":0,"seed_aids":[0],"seed_tag_ids":[0],"exclude_aids":[0],"allow_degrade":false}'
# 读某个池的某个版本条目（运维/排障，分页有上限）
rpc 8116 'recommendrecall.v1.Recall/GetPoolSnapshot' '{"pool":{"source":{},"pool_key":""},"version":0,"pn":0,"ps":0}'
# 列出版本与生成批次（可追溯性）
rpc 8116 'recommendrecall.v1.Recall/ListPoolVersions' '{"pool":{"source":{},"pool_key":""},"limit":0,"include_retired":false}'
# 回放一次在线召回请求（按 request_id 或 snapshot_id）
rpc 8116 'recommendrecall.v1.Recall/GetRecallRequestLog' '{"request_id":"","snapshot_id":""}'
# 分页查召回请求日志（运营/排障）
rpc 8116 'recommendrecall.v1.Recall/ListRecallRequestLogs' '{"mid":0,"scene":"","from_time":0,"to_time":0,"pn":0,"ps":0}'
# 分批写入池条目到指定版本（idempotency_key 幂等）
rpc 8116 'recommendrecall.v1.Recall/UpsertPoolItems' '{"pool":{"source":{},"pool_key":""},"version":0,"batch_id":"","generator":"","schema_version":0,"items":[{"aid":0,"score":0}],"idempotency_key":"","is_last_batch":false}'
# 原子切换池的当前生效版本（写审计 + 发事件）
rpc 8116 'recommendrecall.v1.Recall/PublishPoolVersion' '{"pool":{"source":{},"pool_key":""},"version":0,"operator":"","reason":"","idempotency_key":""}'
# 回滚到历史版本（运营回滚开关）
rpc 8116 'recommendrecall.v1.Recall/RollbackPoolVersion' '{"pool":{"source":{},"pool_key":""},"target_version":0,"operator":"","reason":"","idempotency_key":""}'
# 分批清理过期版本（由 services/cron 调用）
rpc 8116 'recommendrecall.v1.Recall/PrunePoolVersions' '{"pool":{"source":{},"pool_key":""},"keep_versions":0,"max_rows":0,"dry_run":false,"operator":""}'
# 下发在线召回参数与池健康摘要
rpc 8116 'recommendrecall.v1.Recall/GetRecallConfig' '{"scene":"","mid":0}'

# ---------- rights (端口 8097) ----------
# 运营创建合同
rpc 8097 'rights.v1.Rights/CreateContract' '{"owner_id":0,"title":"","sign_date":0,"start_date":0,"end_date":0,"regions":[""],"operator":"","ip":""}'
# 查询单个合同
rpc 8097 'rights.v1.Rights/GetContract' '{"contract_id":0,"ip":""}'
# 分页查询合同
rpc 8097 'rights.v1.Rights/ListContracts' '{"owner_id":0,"state":{},"pn":0,"ps":0,"ip":""}'
# 为内容创建时间窗口（关联合同）
rpc 8097 'rights.v1.Rights/CreateWindow' '{"contract_id":0,"content_id":0,"content_type":{},"region":"","start_time":0,"end_time":0,"operator":"","ip":""}'
# 查询单个窗口
rpc 8097 'rights.v1.Rights/GetWindow' '{"window_id":0,"ip":""}'
# 按 content_id 或 contract_id 查询窗口
rpc 8097 'rights.v1.Rights/ListWindows' '{"content_id":0,"contract_id":0,"content_type":{},"state":{},"pn":0,"ps":0,"ip":""}'
# 校验内容在某地区是否可播放（窗口有效）
rpc 8097 'rights.v1.Rights/CheckPlayable' '{"content_id":0,"content_type":{},"region":"","ip":""}'
# 手动过期窗口（运营/cron）
rpc 8097 'rights.v1.Rights/ExpireWindow' '{"window_id":0,"ip":""}'
# 查询即将过期的窗口（cron 用）
rpc 8097 'rights.v1.Rights/ListExpiring' '{"within_seconds":0,"pn":0,"ps":0,"ip":""}'

# ---------- risk-control (端口 8105) ----------
# 同步裁决一次受保护动作（名单 → 处罚 → 规则 → 降级）。
rpc 8105 'riskcontrol.v1.RiskControl/CheckAction' '{"mid":0,"action":{},"device_id":"","ip_hash":"","platform":"","app_version":"","request_context":{"k":""},"request_id":"","trace_id":""}'
# 行为上报，写 Redis 滑窗计数供 CheckAction 评估；不做 SPM 广告分析。
rpc 8105 'riskcontrol.v1.RiskControl/ReportAction' '{"mid":0,"action":{},"device_id":"","ip_hash":"","platform":"","count":0,"occurred_at":0,"event_id":"","trace_id":""}'
# 查询设备画像。
rpc 8105 'riskcontrol.v1.RiskControl/GetDeviceProfile' '{"device_id":"","device_hash":""}'
# 写入/更新设备画像与设备-账号关联。
rpc 8105 'riskcontrol.v1.RiskControl/UpsertDeviceProfile' '{"device_id":"","device_hash":"","labels":[""],"risk_score":0,"mid":0,"source":"","operator":0,"idempotency_key":""}'
# 运营/审核下发处罚。
rpc 8105 'riskcontrol.v1.RiskControl/ApplyPunishment' '{"mid":0,"scope":{},"decision":{},"reason":"","reason_code":"","operator":0,"duration_seconds":0,"idempotency_key":"","trace_id":""}'
# 解除处罚（幂等，已终态返回当前状态）。
rpc 8105 'riskcontrol.v1.RiskControl/LiftPunishment' '{"punishment_id":0,"mid":0,"scope":{},"operator":0,"reason":"","idempotency_key":""}'
# 分页查询处罚。
rpc 8105 'riskcontrol.v1.RiskControl/ListPunishments' '{"mid":0,"scope":{},"state":{},"only_active":false,"pn":0,"ps":0}'
# 新增/更新风控规则（版本递增，operator 必填）。
rpc 8105 'riskcontrol.v1.RiskControl/UpsertRule' '{"rule_id":0,"name":"","action_type":{},"metric":{},"op":{},"threshold":0,"window_seconds":0,"decision":{},"priority":0,"state":0,"operator":0,"idempotency_key":""}'
# 分页查询风控规则。
rpc 8105 'riskcontrol.v1.RiskControl/ListRules' '{"action_type":{},"metric":{},"state":0,"pn":0,"ps":0}'
# 新增/更新名单条目。
rpc 8105 'riskcontrol.v1.RiskControl/UpsertListEntry' '{"list_type":{},"target_type":{},"target_value":"","reason":"","operator":0,"duration_seconds":0,"state":0,"idempotency_key":""}'
# 分页查询名单条目。
rpc 8105 'riskcontrol.v1.RiskControl/GetListEntries' '{"list_type":{},"target_type":{},"target_value":"","state":0,"pn":0,"ps":0}'

# ---------- search-indexer (端口 8106) ----------
# 按 content_id 写入/覆盖一条内容投影（上游显式触发或回填）。 / doc 必须携带 doc_revision；旧版本默认被拒绝，保证乱序/迟到事件不破坏终态。
rpc 8106 'searchindexer.v1.SearchIndexer/UpsertContentDoc' '{"content_id":0,"doc":{"content_id":0,"content_type":{},"title":"","description":"","cover_url":"","author_mid":0,"author_name":"","typeid":0,"type_name":"","tags":[""],"duration_sec":0,"publish_at":0,"ctime":0,"state":{},"doc_revision":0,"heat":{},"rights_expire_at":0,"language":"","subtitle_langs":[""],"sensitive":false,"schema_version":0},"request_id":"","source":"","force_overwrite":false}'
# 下架/删除时移除或降级投影，并说明 CDN/搜索投影一致性处理。
rpc 8106 'searchindexer.v1.SearchIndexer/DeleteContentDoc' '{"content_id":0,"content_type":{},"reason":"","purge":false}'
# 提交全量/分区重建任务（request_id 幂等），返回 task_id 与预分配目标索引。
rpc 8106 'searchindexer.v1.SearchIndexer/SubmitRebuildTask' '{"scope":"","scope_value":"","alias":"","request_id":"","operator":""}'
# 查询单个重建任务进度。
rpc 8106 'searchindexer.v1.SearchIndexer/GetRebuildTask' '{"task_id":""}'
# 分页查询重建任务（cursor + 状态过滤）。
rpc 8106 'searchindexer.v1.SearchIndexer/ListRebuildTasks' '{"state":"","cursor":"","limit":0}'
# 把查询别名从旧索引切到新版本索引（expected_current 乐观校验，失败返回明确错误）。
rpc 8106 'searchindexer.v1.SearchIndexer/SwitchAlias' '{"alias":"","target_index":"","expected_current":"","skip_health_check":false,"operator":""}'
# 返回各别名/索引的 doc 数与健康状态，以及重试/死信积压。
rpc 8106 'searchindexer.v1.SearchIndexer/GetIndexHealth' '{"alias":""}'

# ---------- spm (端口 8131) ----------
# 读取单主体单口径单窗口的指标值
rpc 8131 'spm.v1.Spm/GetMetric' '{"subject_type":{},"subject_id":0,"metric_key":"","metric_version":0,"window_type":{},"window_start":0}'
# 批量读取主体的一组指标/多个窗口（单次上限 50 口径 × 30 窗口）
rpc 8131 'spm.v1.Spm/BatchGetMetrics' '{"subject_type":{},"subject_id":0,"keys":[null],"window_type":{},"window_start_from":0,"window_count":0}'
# 热度榜投影（只读，按指标值倒序分页）
rpc 8131 'spm.v1.Spm/ListHotSubjects' '{"subject_type":{},"metric_key":"","metric_version":0,"window_type":{},"window_start":0,"zone_id":0,"pn":0,"ps":0}'
# 用户兴趣画像（脱敏权重，不返回行为明细）
rpc 8131 'spm.v1.Spm/GetUserInterest' '{"mid":0,"metric_version":0,"top_n":0}'
# 留存曲线
rpc 8131 'spm.v1.Spm/GetRetention' '{"cohort_type":null,"cohort_date":0,"max_day":0,"metric_version":0,"zone_id":0}'
# 聚合链路写回窗口指标（幂等覆盖，来源受 MetricSource 白名单约束）
rpc 8131 'spm.v1.Spm/WriteMetricWindow' '{"points":[{"metric_key":"","metric_version":0,"subject_type":{},"subject_id":0,"window_type":{},"window_start":0,"value":0,"numerator":0,"denominator":0,"sample_count":0,"event_time":0}],"source":{},"request_id":"","allow_late_write":false}'
# 从事实表重算指标（计数/指标漂移的修复入口，派生 JOB_TYPE_RECOMPUTE 作业）
rpc 8131 'spm.v1.Spm/RecomputeMetrics' '{"subject_type":{},"subject_id":0,"metric_key":"","metric_version":0,"window_type":{},"window_start_from":0,"window_start_to":0,"request_id":"","operator":""}'
# 登记指标口径新版本（已存在版本不可变）
rpc 8131 'spm.v1.Spm/UpsertMetricDefinition' '{"definition":{"metric_key":"","metric_version":0,"name":"","formula":"","unit":"","supported_windows":[{}],"source_event_types":"","state":{},"description":"","created_by":"","ctime":0,"mtime":0},"operator":"","request_id":""}'
# 变更口径状态（DRAFT/ACTIVE/RETIRED），不删除历史口径
rpc 8131 'spm.v1.Spm/UpdateMetricDefinitionState' '{"metric_key":"","metric_version":0,"state":{},"operator":"","reason":"","request_id":""}'
# 查询单个口径
rpc 8131 'spm.v1.Spm/GetMetricDefinition' '{"metric_key":"","metric_version":0}'
# 口径列表（分页）
rpc 8131 'spm.v1.Spm/ListMetricDefinitions' '{"metric_key":"","state":{},"pn":0,"ps":0}'
# 提交实时/离线聚合作业（request_id 幂等）
rpc 8131 'spm.v1.Spm/SubmitAggregationJob' '{"job_type":{},"subject_type":{},"subject_id":0,"metric_key":"","metric_version":0,"window_type":{},"window_start_from":0,"window_start_to":0,"request_id":"","operator":"","reason":""}'
# 查询作业（按 job_id 或 request_id）
rpc 8131 'spm.v1.Spm/GetAggregationJob' '{"job_id":0,"request_id":""}'
# 作业列表（分页）
rpc 8131 'spm.v1.Spm/ListAggregationJobs' '{"job_type":{},"state":{},"since":0,"pn":0,"ps":0}'
# 消费状态汇总（位点与堆积）
rpc 8131 'spm.v1.Spm/ListConsumerState' '{"topic":"","state":{},"pn":0,"ps":0}'
# 死信留档查询（只读；重放属于 services/cron 的待接线项）
rpc 8131 'spm.v1.Spm/ListDeadLetters' '{"topic":"","state":"","since":0,"pn":0,"ps":0}'

# @@ 消费面：两个网关都不引用（服务间 / worker / 尚未接线） —— 2 个服务 / 11 个方法 @@

# ---------- content-fingerprint (端口 8101) ----------
# 创建指纹任务（PENDING）；本期不调用真实指纹算法，只建任务。
rpc 8101 'fingerprint.v1.ContentFingerprint/SubmitTask' '{"asset_id":0,"fp_type":{},"trace_id":"","operator":""}'
# 查询单个任务详情
rpc 8101 'fingerprint.v1.ContentFingerprint/GetTask' '{"task_id":0}'
# 分页查询任务（按 asset_id 或 state 过滤）
rpc 8101 'fingerprint.v1.ContentFingerprint/ListTasks' '{"asset_id":0,"state":{},"pn":0,"ps":0}'
# Worker 回写任务结果（PENDING→SUCCEEDED/FAILED），同时写入 fingerprint_record
rpc 8101 'fingerprint.v1.ContentFingerprint/UpdateTaskResult' '{"task_id":0,"video_key":"","audio_key":"","video_hash":"","audio_hash":"","state":{},"operator":""}'
# 按指纹 key 查询匹配的 asset 列表（本期占位：返回空列表）
rpc 8101 'fingerprint.v1.ContentFingerprint/MatchByFingerprint' '{"fp_key":"","fp_type":{},"top_n":0}'
# 按 asset_id 查询其指纹的所有匹配（本期占位：返回空列表）
rpc 8101 'fingerprint.v1.ContentFingerprint/MatchByAsset' '{"asset_id":0,"fp_type":{}}'

# ---------- moderation-worker (端口 8094) ----------
# 执行 OCR 文本识别（本期占位）
rpc 8094 'moderation.worker.v1.ModerationWorker/RunOCR' '{"task_id":"","worker_task_id":"","capability":{},"media_uri":"","duration_ms":0,"params":{"k":""},"timeout_ms":0,"trace_id":""}'
# 执行 ASR 语音识别（本期占位）
rpc 8094 'moderation.worker.v1.ModerationWorker/RunASR' '{"task_id":"","worker_task_id":"","capability":{},"media_uri":"","duration_ms":0,"params":{"k":""},"timeout_ms":0,"trace_id":""}'
# 执行图像识别（本期占位）
rpc 8094 'moderation.worker.v1.ModerationWorker/RunImage' '{"task_id":"","worker_task_id":"","capability":{},"media_uri":"","duration_ms":0,"params":{"k":""},"timeout_ms":0,"trace_id":""}'
# 执行音频识别（本期占位）
rpc 8094 'moderation.worker.v1.ModerationWorker/RunAudio' '{"task_id":"","worker_task_id":"","capability":{},"media_uri":"","duration_ms":0,"params":{"k":""},"timeout_ms":0,"trace_id":""}'
# 查询任务执行结果
rpc 8094 'moderation.worker.v1.ModerationWorker/GetTaskResult' '{"worker_task_id":"","task_id":""}'

echo "调用数：成功返回 $PASS / 非零退出 $FAIL（非零退出≠缺陷，见文件头说明）"
