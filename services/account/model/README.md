# account/model

account 服务的数据库模型与查询代码。本包对应 [AGENTS.md §3](../../../AGENTS.md) 中 `services/<service>/model/` 的定位，仅承载 account 服务**自有** MySQL 表的实体定义和持久化查询，不持有展示资料、关系、会员等归属其他服务的数据。

## 数据所有权

依据 [AGENTS.md §5](../../../AGENTS.md)，account 服务只持有：

| 表 | 用途 |
|---|---|
| `account` | 账号主表：mid、账号状态、注册时间、注册 IP、是否游客 |
| `account_credential` | 登录标识：mid、凭证类型（用户名/手机/邮箱）、identifier、状态 |

可变展示资料（昵称、头像、签名、性别、等级、生日、道德值、实名状态等）由 `user-profile` 服务持有，account 不复制；社交关系由 `social-graph` 服务持有；会员、硬币、挂件、勋章、官方认证属于商业化范围外，本期不实现。

## 文件

| 文件 | 内容 |
|---|---|
| `account.go` | `Account` 实体、`AccountModel` 接口、`defaultAccountModel` 实现 |
| `account_credential.go` | `AccountCredential` 实体、`AccountCredentialModel` 接口、`defaultAccountCredentialModel` 实现 |

## 使用约束

- 查询使用 `sqlx.SqlConn`，所有 SQL 参数化，禁止字符串拼接。
- 写入接口设计幂等键、状态版本或唯一约束。
- 不在本包内做缓存（缓存由 `internal/repository` 层负责）。
- 跨服务禁止直连其他服务的 MySQL 表；本包只操作 account 自有的表。
