# user-profile 服务数据库迁移说明

本目录是 `services/user-profile` 的 MySQL 迁移文件。所有表、字段、索引均带详细
中文注释，迁移文件为 forward-only，回滚方式在各文件头部注明。

## 1. 表清单与数据所有者

| 迁移 | 表 | 用途 |
|---|---|---|
| 000001 | `user_base` | 用户可变展示资料：昵称/性别/头像/签名/排名/生日 |
| 000002 | `user_exp` | 经验值（等级由经验实时推导，不落库） |
| 000003 | `user_flag` | 用户标志位（首次改昵称等） |
| 000004 | `user_moral` | 节操值：当前值、累计增减、恢复时间 |
| 000005 | `user_official` / `user_official_doc` / `user_official_doc_addit` | 官方认证：生效信息 / 申请文档 / 附加键值 |
| 000006 | `user_monitor` | 受监控用户名单（资料变更自动进审核） |
| 000007 | `user_property_review` | 属性（头像/签名/昵称）变更审核记录 |
| 000008 | `realname_info` / `realname_apply` / `realname_apply_img` | 实名认证：信息（证件密文）/申请单/证件照 |
| 000009 | `member_log` | 经验/节操变更日志（替代参考仓库 HBase/ES 报表存储） |
| 000010 | `member_outbox` | 领域事件 Outbox（AGENTS.md §5） |

以上表全部属于 user-profile 服务（AGENTS.md §5），其他服务禁止直连，
只能通过 gRPC（etcd 注册 key `user-profile.v1.rpc`）访问。

## 2. 参考仓库字段映射与移植差异

参考仓库 `member` 服务使用以下存储，本项目按 go-zero 技术栈与项目规范调整：

| 参考仓库 | 本项目 | 差异说明 |
|---|---|---|
| `user_base_%02d`（mid%100 分 100 张表） | `user_base` 单表 | 起步阶段单表；分表时仅切换表名，字段/索引不变 |
| `user_exp_%02d`（分表） | `user_exp` 单表 | 同上 |
| `user_flag` / `user_moral` | 同名同结构 | 一致 |
| `user_official` / `user_official_doc` / `user_official_doc_addit` | 同名同结构 | 一致（`description` 列对应代码字段 Desc） |
| `user_monitor` / `user_property_review` | 同名，补充 ctime/mtime | 参考 `user_property_review` 无时间列，本项目补充便于归档审计 |
| `realname_info` / `realname_apply` / `realname_apply_img` | 同名同结构 | 一致；`dede_identification_card_apply*` 旧表不迁移 |
| `realname_alipay_apply`（芝麻渠道） | 不迁移 | 支付宝渠道仅参考仓库 gorpc 暴露，本项目暂不启用（见服务 README） |
| HBase `ugc:MoralLog` + 报表搜索服务 | `member_log` | 日志落地本地 MySQL，查询语义对齐（7 天/1000 条） |
| databus 两个主题（日志上报、AccountNotify） | `member_outbox` + account `DelCache` RPC | 资料更新事件经 Outbox 发布器调用 account 的 DelCache RPC 失效缓存 |
| block 子模块四表（block_user 等） | 不迁移 | 封禁/处罚属于 risk-control 域（AGENTS.md §5） |

## 3. 执行与回滚

```powershell
# 执行（按 deploy/docker-compose 提供的本地 MySQL）
./scripts/migrate.ps1 -Action up

# 回滚（无专用回滚脚本时，按文件头部注释逆序手动执行 DROP TABLE）
```

生产环境禁止直接执行未评审 SQL；每次迁移必须记录 forward、回滚补偿和锁风险
（本目录均为建表，空库无锁风险）。

## 4. 隐私与安全约束

- `realname_info.card` / `realname_apply.card_num` 仅存 RSA 公钥加密密文，
  明文只在缓存中短暂存在；私钥经 Secret/Vault 注入配置。
- `card_md5` 为带盐哈希，用于查重与反查，不可逆。
- 日志与 Outbox 中的 `ip` 字段为操作来源 IP，仅用于审计，需遵守隐私策略。
