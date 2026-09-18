# account/internal/repository

account 服务的数据访问层，承担三类职责：

1. **本地数据持久化**：通过 `services/account/model` 操作 account 服务自有的 `account`、`account_credential` 表。
2. **缓存层**：使用 Redis 缓存 `Info`/`Card`/`Profile`/`Vip`，命中则直接返回；缓存 miss 时回源并异步回填。
3. **下游服务聚合**：调用 `user-profile`、`social-graph` 等领域服务的 RPC，聚合得到 `Info`/`Card`/`Profile` 等组合数据。

## 数据流

```
logic → repository ──> Redis 缓存（命中即返回）
                  └─> account model（本地 MySQL，仅 account 主表和凭证表）
                  └─> user-profile RPC（昵称、头像、签名、性别、等级、生日、道德值、实名状态、邮箱/手机绑定状态）
                  └─> social-graph RPC（关注、粉丝、关系、黑名单、关注列表、富关系）
                  └─> vip HTTP（会员信息：商业化范围外，降级返回默认值）
                  └─> passport HTTP（注册时间、注册 IP、手机号：仅 privacy 接口使用）
```

## 范围豁免

依据 [AGENTS.md §1](../../../../AGENTS.md) 商业化范围外约束，下列字段在 RPC 契约中保留以维持兼容性，但实现降级返回默认值：

- `Vip3`/`Vips3`：返回零值 `VipInfo`（type=0、status=0、due_date=0、vip_pay_type=0）
- `ProfileWithStat3.coins`：返回 0
- `Profile.identification`（实名认证）：返回 0
- `Card.pendant`、`Card.nameplate`、`Card.official`：返回零值
- `Profile.pendant`、`Profile.nameplate`、`Profile.official`：返回零值

`AddExp3`/`AddMoral3` 不在本期落地，对应 `user-profile` 的 exp/moral 写接口暂未实现，调用时返回 `ecode.ErrNotImplemented`。后续如需落地，需单独评审并更新本说明。

## 文件

| 文件 | 内容 |
|---|---|
| `repository.go` | `Repository` 结构、`New` 构造函数、下游 RPC client 接口定义 |
| `cache.go` | Redis 缓存读写：Info/Card/Profile/Vip 的单查、批量查、删除 |
| `raw.go` | 原始数据获取：从下游 RPC + 本地 model 聚合 Info/Card/Profile/Vip |
| `passport.go` | Passport HTTP 集成：注册信息查询，仅供 `/privacy` 接口使用 |

## 约束

- 缓存 miss 时回源，回源成功后异步回填缓存（`common/fanout`）。
- 批量查询按 50 一组分批回源，并发使用 `common/errgroup`，单个失败降级跳过。
- 跨服务聚合时不阻塞主请求：下游失败时记日志并降级返回零值字段，不让单个下游故障导致整体失败。
- 缓存 key 前缀：`i3_`/`c3_`/`p3_`/`v3_` + mid，沿用参考仓库约定，便于跨语言联调。
- 不在本层做业务规则（如权限校验、状态机推进），业务规则放在 `internal/logic`。
