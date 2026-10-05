# RPC · `user-profile`

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

| 项 | 值 |
|---|---|
| 契约文件 | `services/user-profile/rpc/userprofile.proto` |
| protobuf 包 | `userprofile.v1` |
| go_package | `go-video/services/user-profile/rpc` |
| 发现用的 etcd key | `user-profile.v1.rpc`（`services/user-profile/etc/userprofile.v1.yaml` 顶层 `Etcd.Key`，网关要命中这个值） |
| 配置里的 `Name` | `userprofile.v1.rpc`——**与注册 key 不同**，按 `Name` 连不上本服务 |
| 监听 | `8085`（`services/user-profile/etc/userprofile.v1.yaml` 的 `ListenOn`） |
| 数据库 | `userprofile` |
| 方法数 | 35（service `UserProfile`） |
| 网关消费方 | `app:UserProfileRPC`、`admin:UserProfileRPC` |

## 契约说明

> 说明：本契约移植自参考仓库 openbilibili-go-common/app/service/main/member/api/api.proto。
> 依据 AGENTS.md §5，block（封禁）子域属于 risk-control 服务，BlockInfo/BlockBatchInfo/
> BlockBatchDetail 三个方法不移植到 user-profile（详见服务 README）。

## service `UserProfile`

> UserProfile 用户资料服务（移植自参考仓库 member 服务，封禁子域除外）

gRPC 方法前缀：`userprofile.v1.UserProfile/`

| # | 方法 | 请求 | 响应 | 说明 |
|---|---|---|---|---|
| 1 | `Base` | [`MemberMidReq`](#message-membermidreq) | [`BaseInfoReply`](#message-baseinforeply) | 查询单个用户基础资料 |
| 2 | `Bases` | [`MemberMidsReq`](#message-membermidsreq) | [`BaseInfosReply`](#message-baseinfosreply) | 批量查询用户基础资料 |
| 3 | `Member` | [`MemberMidReq`](#message-membermidreq) | [`MemberInfoReply`](#message-memberinforeply) | 查询单个用户全量信息（基础 + 等级 + 官方认证） |
| 4 | `Members` | [`MemberMidsReq`](#message-membermidsreq) | [`MemberInfosReply`](#message-memberinfosreply) | 批量查询用户全量信息 |
| 5 | `NickUpdated` | [`MemberMidReq`](#message-membermidreq) | [`NickUpdatedReply`](#message-nickupdatedreply) | 查询用户是否修改过昵称 |
| 6 | `SetNickUpdated` | [`MemberMidReq`](#message-membermidreq) | [`EmptyReply`](#message-emptyreply) | 标记用户已修改过昵称 |
| 7 | `SetOfficialDoc` | [`OfficialDocReq`](#message-officialdocreq) | [`EmptyReply`](#message-emptyreply) | 提交官方认证文档 |
| 8 | `SetSex` | [`UpdateSexReq`](#message-updatesexreq) | [`EmptyReply`](#message-emptyreply) | 设置性别 |
| 9 | `SetName` | [`UpdateUnameReq`](#message-updateunamereq) | [`EmptyReply`](#message-emptyreply) | 设置昵称 |
| 10 | `SetFace` | [`UpdateFaceReq`](#message-updatefacereq) | [`EmptyReply`](#message-emptyreply) | 设置头像 |
| 11 | `SetRank` | [`UpdateRankReq`](#message-updaterankreq) | [`EmptyReply`](#message-emptyreply) | 设置排名 |
| 12 | `SetBirthday` | [`UpdateBirthdayReq`](#message-updatebirthdayreq) | [`EmptyReply`](#message-emptyreply) | 设置生日 |
| 13 | `SetSign` | [`UpdateSignReq`](#message-updatesignreq) | [`EmptyReply`](#message-emptyreply) | 设置签名 |
| 14 | `OfficialDoc` | [`MidReq`](#message-midreq) | [`OfficialDocInfoReply`](#message-officialdocinforeply) | 查询官方认证文档 |
| 15 | `Moral` | [`MemberMidReq`](#message-membermidreq) | [`MoralReply`](#message-moralreply) | 查询节操值 |
| 16 | `MoralLog` | [`MemberMidReq`](#message-membermidreq) | [`UserLogsReply`](#message-userlogsreply) | 查询节操值变更日志 |
| 17 | `AddMoral` | [`UpdateMoralReq`](#message-updatemoralreq) | [`EmptyReply`](#message-emptyreply) | 变更节操值 |
| 18 | `BatchAddMoral` | [`UpdateMoralsReq`](#message-updatemoralsreq) | [`UpdateMoralsReply`](#message-updatemoralsreply) | 批量变更节操值 |
| 19 | `UndoMoral` | [`UndoMoralReq`](#message-undomoralreq) | [`EmptyReply`](#message-emptyreply) | 撤销节操值变更（参考 member 服务 /moral/undo 的 RPC 化） |
| 20 | `Exp` | [`MidReq`](#message-midreq) | [`LevelInfoReply`](#message-levelinforeply) | 查询经验等级信息（含当前经验） |
| 21 | `Level` | [`MidReq`](#message-midreq) | [`LevelInfoReply`](#message-levelinforeply) | 查询等级信息（不含当前经验） |
| 22 | `UpdateExp` | [`AddExpReq`](#message-addexpreq) | [`EmptyReply`](#message-emptyreply) | 更新经验值 |
| 23 | `SetExp` | [`AddExpReq`](#message-addexpreq) | [`EmptyReply`](#message-emptyreply) | 直接设置经验值（仅运营，参考 member 服务 /exp/set 的 RPC 化） |
| 24 | `ExpLog` | [`MidReq`](#message-midreq) | [`UserLogsReply`](#message-userlogsreply) | 查询经验变更日志 |
| 25 | `ExpStat` | [`MidReq`](#message-midreq) | [`ExpStatReply`](#message-expstatreply) | 查询当日经验奖励统计 |
| 26 | `RealnameStatus` | [`MemberMidReq`](#message-membermidreq) | [`RealnameStatusReply`](#message-realnamestatusreply) | 查询实名认证状态 |
| 27 | `RealnameApplyStatus` | [`MemberMidReq`](#message-membermidreq) | [`RealnameApplyInfoReply`](#message-realnameapplyinforeply) | 查询实名申请流程状态 |
| 28 | `RealnameTelCapture` | [`MemberMidReq`](#message-membermidreq) | [`EmptyReply`](#message-emptyreply) | 发送实名手机验证码 |
| 29 | `RealnameApply` | [`RealnameApplyReq`](#message-realnameapplyreq) | [`EmptyReply`](#message-emptyreply) | 提交实名认证申请 |
| 30 | `RealnameDetail` | [`MemberMidReq`](#message-membermidreq) | [`RealnameDetailReply`](#message-realnamedetailreply) | 查询实名详情（含性别与手持照） |
| 31 | `RealnameStrippedInfo` | [`MemberMidReq`](#message-membermidreq) | [`RealnameStrippedInfoReply`](#message-realnamestrippedinforeply) | 查询脱敏实名信息 |
| 32 | `MidByRealnameCard` | [`MidByRealnameCardsReq`](#message-midbyrealnamecardsreq) | [`MidByRealnameCardReply`](#message-midbyrealnamecardreply) | 按证件号批量查询 mid |
| 33 | `AddUserMonitor` | [`AddUserMonitorReq`](#message-addusermonitorreq) | [`EmptyReply`](#message-emptyreply) | 添加用户到监控名单 |
| 34 | `IsInMonitor` | [`MidReq`](#message-midreq) | [`IsInMonitorReply`](#message-isinmonitorreply) | 查询用户是否在监控名单 |
| 35 | `AddPropertyReview` | [`AddPropertyReviewReq`](#message-addpropertyreviewreq) | [`EmptyReply`](#message-emptyreply) | 添加用户属性变更审核（参考 member 服务 /property/review/add 的 RPC 化） |

## 消息与枚举

### message `MidReq`

> 单个用户请求（带真实 IP）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `real_ip` | `string` | 2 | — | 请求来源真实 IP |

### message `MemberMidReq`

> 单个用户请求（带远端 IP）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `remote_ip` | `string` | 2 | — | 调用方远端 IP |

### message `MemberMidsReq`

> 批量用户请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 用户 ID 列表（最多 100 个） |
| `remote_ip` | `string` | 2 | — | 调用方远端 IP |

### message `MidByRealnameCardsReq`

> 按身份证号批量查询请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `card_code` | `string` | 1 | repeated | 证件号列表 |
| `country` | `int32` | 2 | — | 国家：0 中国 |
| `card_type` | `int32` | 3 | — | 证件类型：0 身份证 |

### message `EmptyReply`

> 空响应（对应参考仓库 EmptyStruct）

（空消息）

### message `LevelInfoReply`

> 等级信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `cur` | `int32` | 1 | — | 当前等级 |
| `min` | `int32` | 2 | — | 当前等级经验阈值 |
| `now_exp` | `int32` | 3 | — | 当前经验值（按 ExpMulti 折算后的整数） |
| `next_exp` | `int32` | 4 | — | 下一等级经验阈值（满级为 -1） |

### message `UserLogReply`

> 用户变更日志

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `ip` | `string` | 2 | — | 操作来源 IP |
| `ts` | `int64` | 3 | — | 操作时间（Unix 秒） |
| `log_id` | `string` | 4 | — | 日志唯一 ID（UUID） |
| `content` | `map<string, string>` | 5 | — | 日志内容明细 |

### message `UserLogsReply`

> 用户变更日志集合

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `user_logs` | [`UserLogReply`](#message-userlogreply) | 1 | repeated | 日志列表 |

### message `AddExpReq`

> 经验值变更请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `count` | `double` | 2 | — | 变更量（单位：分） |
| `reason` | `string` | 3 | — | 变更原因 |
| `operate` | `string` | 4 | — | 操作类型 |
| `ip` | `string` | 5 | — | 操作来源 IP |

### message `ExpStatReply`

> 经验统计（当日登录/观看/投币/分享奖励标记）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `login` | `bool` | 1 | — | 当日是否领取登录奖励 |
| `watch` | `bool` | 2 | — | 当日是否领取观看奖励 |
| `coin` | `int64` | 3 | — | 当日投币奖励次数 |
| `share` | `bool` | 4 | — | 当日是否领取分享奖励 |

### message `BaseInfoReply`

> 用户基础资料

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `name` | `string` | 2 | — | 昵称 |
| `sex` | `int64` | 3 | — | 性别：0 保密、1 男、2 女 |
| `face` | `string` | 4 | — | 头像 URL |
| `sign` | `string` | 5 | — | 个人签名 |
| `rank` | `int64` | 6 | — | 排名 |
| `birthday` | `int64` | 7 | — | 生日（Unix 秒，默认 -28800 表示未设置） |

### message `OfficialInfoReply`

> 官方认证信息（生效状态）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `role` | `int32` | 1 | — | 认证角色：0 未认证、1 UP 主、2 身份、3 企业、4 政府、5 媒体、6 其他 |
| `title` | `string` | 2 | — | 认证称号 |
| `desc` | `string` | 3 | — | 认证描述 |

### message `BaseInfosReply`

> 批量基础资料

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `base_infos` | [`map<int64, BaseInfoReply>`](#message-baseinforeply) | 1 | — | mid → 基础资料 |

### message `MemberInfoReply`

> 用户全量信息（基础 + 等级 + 官方认证）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `base_info` | [`BaseInfoReply`](#message-baseinforeply) | 1 | — | 基础资料 |
| `level_info` | [`LevelInfoReply`](#message-levelinforeply) | 2 | — | 等级信息 |
| `official_info` | [`OfficialInfoReply`](#message-officialinforeply) | 3 | — | 官方认证信息 |

### message `MemberInfosReply`

> 批量用户全量信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `member_infos` | [`map<int64, MemberInfoReply>`](#message-memberinforeply) | 1 | — | mid → 全量信息 |

### message `NickUpdatedReply`

> 昵称是否修改过

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `nick_updated` | `bool` | 1 | — | 是否已首次修改昵称 |

### message `OfficialDocReq`

> 官方认证文档提交请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `name` | `string` | 2 | — | 认证主体名称 |
| `state` | `int32` | 3 | — | 审核状态（服务端强制置为待审核） |
| `role` | `int32` | 4 | — | 认证角色 |
| `title` | `string` | 5 | — | 认证称号 |
| `desc` | `string` | 6 | — | 认证描述 |
| `reject_reason` | `string` | 7 | — | 拒绝原因（审核回填） |
| `realname` | `int32` | 8 | — | 是否实名：0 否、1 是 |
| `operator` | `string` | 9 | — | 经营人/联系人 |
| `telephone` | `string` | 10 | — | 联系电话 |
| `email` | `string` | 11 | — | 联系邮箱 |
| `address` | `string` | 12 | — | 联系地址 |
| `company` | `string` | 13 | — | 公司名称 |
| `credit_code` | `string` | 14 | — | 统一社会信用代码 |
| `organization` | `string` | 15 | — | 政府或组织机构名称 |
| `organization_type` | `string` | 16 | — | 组织机构类型 |
| `business_license` | `string` | 17 | — | 营业执照 |
| `business_scale` | `string` | 18 | — | 企业规模 |
| `business_level` | `string` | 19 | — | 企业等级 |
| `business_auth` | `string` | 20 | — | 企业授权函 |
| `supplement` | `string` | 21 | — | 其他补充材料 |
| `professional` | `string` | 22 | — | 专业资质 |
| `identification` | `string` | 23 | — | 身份证明 |
| `submit_source` | `string` | 24 | — | 提交来源 |

### message `UpdateSexReq`

> 资料字段更新请求（性别/昵称/头像/排名/生日/签名）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `sex` | `int64` | 2 | — | 性别：0 保密、1 男、2 女 |
| `remote_ip` | `string` | 3 | — | 调用方远端 IP |

### message `UpdateUnameReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `name` | `string` | 2 | — | 新昵称 |
| `remote_ip` | `string` | 3 | — | 调用方远端 IP |

### message `UpdateFaceReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `face` | `string` | 2 | — | 新头像 URL |
| `remote_ip` | `string` | 3 | — | 调用方远端 IP |

### message `UpdateRankReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `rank` | `int64` | 2 | — | 新排名 |
| `remote_ip` | `string` | 3 | — | 调用方远端 IP |

### message `UpdateBirthdayReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `birthday` | `int64` | 2 | — | 新生日（Unix 秒） |
| `remote_ip` | `string` | 3 | — | 调用方远端 IP |

### message `UpdateSignReq`

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `sign` | `string` | 2 | — | 新签名 |
| `remote_ip` | `string` | 3 | — | 调用方远端 IP |

### message `OfficialDocInfoReply`

> 官方认证文档详情

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `name` | `string` | 2 | — | 认证主体名称 |
| `state` | `int32` | 3 | — | 审核状态：0 待审核、1 通过、2 不通过、3 重新提交 |
| `role` | `int32` | 4 | — | 认证角色 |
| `title` | `string` | 5 | — | 认证称号 |
| `desc` | `string` | 6 | — | 认证描述 |
| `reject_reason` | `string` | 7 | — | 拒绝原因 |
| `realname` | `int32` | 8 | — | 是否实名 |
| `operator` | `string` | 9 | — | 经营人/联系人 |
| `telephone` | `string` | 10 | — | 联系电话 |
| `email` | `string` | 11 | — | 联系邮箱 |
| `address` | `string` | 12 | — | 联系地址 |
| `company` | `string` | 13 | — | 公司名称 |
| `credit_code` | `string` | 14 | — | 统一社会信用代码 |
| `organization` | `string` | 15 | — | 政府或组织机构名称 |
| `organization_type` | `string` | 16 | — | 组织机构类型 |
| `business_license` | `string` | 17 | — | 营业执照 |
| `business_scale` | `string` | 18 | — | 企业规模 |
| `business_level` | `string` | 19 | — | 企业等级 |
| `business_auth` | `string` | 20 | — | 企业授权函 |
| `supplement` | `string` | 21 | — | 其他补充材料 |
| `professional` | `string` | 22 | — | 专业资质 |
| `identification` | `string` | 23 | — | 身份证明 |

### message `MoralReply`

> 节操值信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `moral` | `int64` | 2 | — | 当前节操值（基准 7000，最大 10000） |
| `added` | `int64` | 3 | — | 累计增加值 |
| `deducted` | `int64` | 4 | — | 累计扣减值 |
| `last_recover_date` | `int64` | 5 | — | 上次低于 70 时的恢复时间（Unix 秒） |

### message `UpdateMoralReq`

> 单个节操值变更请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `delta` | `int64` | 2 | — | 变更量（可为负） |
| `origin` | `int64` | 3 | — | 来源类型：1 举报奖励、2 违规惩罚、3 撤销奖励、4 撤销惩罚、5 自动恢复、6 手动修改 |
| `reason` | `string` | 4 | — | 变更原因 |
| `reason_type` | `int64` | 5 | — | 原因类型：1 弹幕、2 评论、3 TAG、4 电波、5 账号、6 管理系统 |
| `operator` | `string` | 6 | — | 操作人 |
| `remark` | `string` | 7 | — | 备注 |
| `status` | `int64` | 8 | — | 日志状态：0 可撤销、1 已撤销、2 不可撤销 |
| `is_notify` | `bool` | 9 | — | 是否通知用户 |
| `ip` | `string` | 10 | — | 操作来源 IP |

### message `UpdateMoralsReq`

> 批量节操值变更请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mids` | `int64` | 1 | repeated | 用户 ID 列表 |
| `delta` | `int64` | 2 | — | 变更量（可为负） |
| `origin` | `int64` | 3 | — | 来源类型（同 UpdateMoralReq） |
| `reason` | `string` | 4 | — | 变更原因 |
| `reason_type` | `int64` | 5 | — | 原因类型（同 UpdateMoralReq） |
| `operator` | `string` | 6 | — | 操作人 |
| `remark` | `string` | 7 | — | 备注 |
| `status` | `int64` | 8 | — | 日志状态 |
| `is_notify` | `bool` | 9 | — | 是否通知用户 |
| `ip` | `string` | 10 | — | 操作来源 IP |

### message `UndoMoralReq`

> 撤销节操值变更请求（参考 member 服务 /moral/undo，RPC 化供网关运营路由聚合）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `log_id` | `string` | 1 | — | 原变更日志 ID |
| `remark` | `string` | 2 | — | 撤销备注 |
| `operator` | `string` | 3 | — | 撤销操作人 |

### message `AddPropertyReviewReq`

> 属性变更审核提交请求（参考 member 服务 /property/review/add，RPC 化）

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `new` | `string` | 2 | — | 变更后的值 |
| `state` | `int32` | 3 | — | 审核状态：0 待审核、1 通过、2 驳回、10 自动审核中 |
| `property` | `int32` | 4 | — | 审核属性：1 头像、2 签名、3 昵称 |
| `extra` | `string` | 5 | — | 审核扩展信息 JSON（空为 {}） |

### message `UpdateMoralsReply`

> 批量节操值变更结果

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `after_morals` | `map<int64, int64>` | 1 | — | mid → 变更后的节操值 |

### message `AddUserMonitorReq`

> 添加用户监控请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `operator` | `string` | 2 | — | 操作人 |
| `remark` | `string` | 3 | — | 备注 |

### message `IsInMonitorReply`

> 是否处于监控中

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `is_in_monitor` | `bool` | 1 | — | 是否在监控名单 |

### message `RealnameStatusReply`

> 实名认证状态

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `realname_status` | `int32` | 1 | — | 实名状态：0 未通过、1 已通过 |

### message `RealnameApplyInfoReply`

> 实名申请流程状态

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `status` | `int32` | 1 | — | 流程状态：0 审核中、1 通过、2 驳回、3 未申请 |
| `remark` | `string` | 2 | — | 驳回原因/备注 |

### message `RealnameApplyReq`

> 实名认证申请请求

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `capture_code` | `int64` | 2 | — | 手机验证码 |
| `realname` | `string` | 3 | — | 真实姓名 |
| `card_type` | `int32` | 4 | — | 证件类型：0 身份证 |
| `card_code` | `string` | 5 | — | 证件号码（身份证须为 15/18 位） |
| `country` | `int32` | 6 | — | 国家：0 中国 |
| `hand_img_token` | `string` | 7 | — | 手持证件照 token |
| `front_img_token` | `string` | 8 | — | 证件正面照 token |
| `back_img_token` | `string` | 9 | — | 证件背面照 token |

### message `RealnameDetailReply`

> 实名详情

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `realname` | `string` | 1 | — | 真实姓名 |
| `card` | `string` | 2 | — | 证件号 |
| `card_type` | `int32` | 3 | — | 证件类型 |
| `status` | `int32` | 4 | — | 实名状态：0 未通过、1 已通过 |
| `gender` | `string` | 5 | — | 性别：male/female/unknown（由证件号解析） |
| `hand_img` | `string` | 6 | — | 手持证件照 URL |

### message `RealnameStrippedInfoReply`

> 脱敏实名信息

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `mid` | `int64` | 1 | — | 用户 ID |
| `status` | `int32` | 2 | — | 流程状态 |
| `channel` | `int32` | 3 | — | 渠道：0 主站、1 支付宝 |
| `country` | `int32` | 4 | — | 国家 |
| `card_type` | `int32` | 5 | — | 证件类型 |
| `adult_type` | `int32` | 6 | — | 成年状态：0 未成年、1 已成年、2 未知 |

### message `MidByRealnameCardReply`

> 证件号 → mid 映射

| 字段 | 类型 | 编号 | 修饰 | 说明 |
|---|---|---|---|---|
| `code_to_mid` | `map<string, int64>` | 5 | — | 证件号 → 用户 ID |
