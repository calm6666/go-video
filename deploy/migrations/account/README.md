# account 服务数据库迁移说明

本目录是 `services/account` 的 MySQL 迁移文件。所有表、字段、索引均带详细中文注释，
迁移文件为 forward-only（只进不退），回滚方式在各文件头部注明。

## 1. 表清单与数据所有者

| 迁移 | 表 | 用途 | 所有者 |
|---|---|---|---|
| 000001 | `account` | 账号主表：mid、状态、是否游客、注册时间、注册 IP | account（AGENTS.md §5） |
| 000002 | `account_credential` | 登录标识：用户名/手机/邮箱，按 `credential_type` 区分 | account（AGENTS.md §5） |
| 000011 | `account_secret` | 登录密码哈希与历史密码（MD5+salt，改密置历史） | account（AGENTS.md §5） |
| 000012 | `account_session` | 登录会话：token/refresh/cookie（30/90 天过期，吊销状态） | account（AGENTS.md §5） |
| 000013 | `account_login_log` | 登录/注册日志（成功失败、IP、设备） | account（AGENTS.md §5） |
| 000014 | `account_capture_log` | 验证码发送审计（登录/注册/找回） | account（AGENTS.md §5） |

其他服务禁止直连这些表，只能通过 account 的 gRPC（etcd 注册 key
`account.v1.rpc`）访问；对外 HTTP 由 gateway/app、gateway/admin 聚合。

## 2. 参考仓库字段映射

参考仓库 `openbilibili-go-common/app/service/main/account` 自身不持有 MySQL 表，
它通过 HTTP/RPC 聚合 passport、member、vip、usersuit、relation、coin、ES 的数据。
移植到本项目后按 AGENTS.md §5 重新划分所有权：

| 参考数据源 | 参考字段 | 本项目归属 | 说明 |
|---|---|---|---|
| passport `/intranet/acc/detail` | `join_time` | `account.created_at` | Profile.join_time / reg_ts / regtime |
| passport `/intranet/acc/detail` | `is_tourist` | `account.is_tourist` | Profile.is_tourist |
| passport `/intranet/acc/detail` | `spacesta` | `account.status` | status=1 时 Profile.silence=1、v1/v2 spacesta=-2 |
| passport `/intranet/acc/queryByMid` | `join_ip` | `account.reg_ip` | /privacy 的 reg_ip |
| passport `/intranet/acc/detail` | `email` | `account_credential`(type=3) | Profile.email_status |
| passport `/intranet/acc/detail` | `phone` | `account_credential`(type=2) | Profile.tel_status、/privacy 的 tel |
| ES `user_base`/`kwname` | 昵称搜索 | `account_credential`(type=1) | /info/by/name 的 name→mid 查询 |
| member（昵称/头像/签名/等级/经验/道德/实名） | — | `user-profile` 服务（未实现） | account 通过 RPC 聚合，不建表 |
| relation（关注/黑名单/统计） | — | `social-graph` 服务 | account 通过 RPC 聚合，不建表 |
| vip（会员） | — | 不实现（AGENTS.md §1 商业化范围外） | Vip3/Vips3 降级返回零值 |
| coin（硬币） | — | 不实现（AGENTS.md §1 商业化范围外） | ProfileWithStat3.coins 固定 0 |
| usersuit（挂件/勋章） | — | 暂无所有者服务 | Card/Profile 相关字段降级为零值 |
| realname（实名认证） | — | `user-profile` 服务 | /privacy 实名字段经 user-profile RealnameDetail 聚合 |
| passport 密码存储 + `/history/pwd/check` | pwd/salt/历史密码 | `account_secret` | MD5(pwd+">>BiLiSaLt<<"+salt)，改密置历史 |
| passport-auth token/cookie/refresh（按月分表+HBase） | token/cookie/refresh | `account_session` | 单表 + Redis 缓存（前缀 ak_/ck_/rk_），30/90 天过期 |
| passport hbase_login_log + RPC.LoginLogs | 登录日志 | `account_login_log` | 本地 MySQL 替代 HBase |
| sms 验证码发送（登录/注册/找回） | 验证码 | Redis（cap_code_*）+ `account_capture_log` | 短信下发待 notification，当前降级日志模式 |
| identify GetCookieInfo/GetTokenInfo | 鉴权 | account `TokenInfo`/`CookieInfo` RPC | 网关统一鉴权（AGENTS.md §6） |
| secure 改密 / account-recovery 找回 | 密码操作 | account `SetPassword`/`ResetPassword` RPC | 改密/重置后吊销全部会话 |

## 3. 执行与回滚

```powershell
# 执行（按 deploy/docker-compose 提供的本地 MySQL）
./scripts/migrate.ps1 -Action up

# 回滚（无专用回滚脚本时，按文件头部注释手动执行 DROP TABLE）
# DROP TABLE IF EXISTS account_credential;
# DROP TABLE IF EXISTS account;
```

生产环境禁止直接执行未评审 SQL；每次迁移必须记录 forward、回滚补偿和锁风险
（本目录两个文件均为建表，空库无锁风险）。

## 4. 新增迁移规范

- 文件名按 `NNNNNN_动作_表名.sql` 递增编号，一次迁移只做一件事。
- 每个字段、每张表、每个索引必须有中文注释；头部写明用途、数据所有者、
  参考映射、回滚方式和锁风险。
- 禁止修改已应用的迁移文件；变更通过新增迁移完成。
