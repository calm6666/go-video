# 接口文档（HTTP / RPC / Postman / 冒烟脚本）

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

## 目录结构

```text
docs/api/
├── http/           对外 HTTP 面，按域分文件
│   ├── app/        gateway/app —— 198 条路由 / 28 个分组文件
│   └── admin/      gateway/admin —— 313 条路由 / 32 个分组文件
└── rpc/            内部 RPC 面，一服务一文件 —— 43 服务 / 589 方法
```

配套产物：
- Postman：[`postman/`](../../postman/README.md)（两个 collection，folder 与本文档分组一一对应）
- RPC 冒烟：[`scripts/rpc/`](../../scripts/rpc/README.md)（**未执行过**，需要真实进程）

## 重新生成

```powershell
node scripts/gen-api-docs.mjs          # 生成并写盘
node scripts/gen-api-docs.mjs --check   # 只校验：漂移门禁 + 产物是否最新
```

## 阅读前要知道的四件事

1. **HTTP 响应统一信封**：所有 `/api`、`/admin`、`/x` 面的成功与业务错误都返回 `code`/`message`/`data`/`ttl`；
   文档里的响应类型表自带这四个字段，`data` 内层结构在各文件的「类型附录」中展开（AGENTS.md §6）。
2. **鉴权不是全局的**：`gateway/app` 没有 JWT 中间件，`mid` 只是入参约定；`gateway/admin` 只有挂了
   `AdminPermission` 的分组才校验 Bearer 会话与权限点，其余分组免鉴权（刻意如此，理由写在分组文件里）。
3. **权限点覆盖情况**（本轮实测）：admin 受保护路由 170 条已在
   `routePermissions` 登记；权限表内路径全部能在 .api 中找到对应路由。
   角色↔权限点绑定不在迁移里，新库仍需运营手工授权。
4. **入参位置决定编码**：`path`→路径段，`form`→URL 查询串（POST 也一样），`json`→JSON 请求体。
   依据是 go-zero `httpx.Parse` 会同时跑 `ParseForm` 与 `ParseJsonBody`；结构体数组不能挂在 `form` 上，
   细节和实测结论见 [postman/README.md](../../postman/README.md) 的「入参编码」一节。

## 快速跳转

### 终端面（198 条）

| 分组 | 前缀 | 路由 | 鉴权 |
|---|---|---|---|
| [01-api](http/app/01-api.md) | `/api` | 1 | 免鉴权 |
| [02-account](http/app/02-account.md) | `/account` | 10 | 免鉴权 · AppkeyVerify |
| [03-account-v1](http/app/03-account-v1.md) | `/account/v1` | 4 | 免鉴权 |
| [04-account-v2](http/app/04-account-v2.md) | `/account/v2` | 2 | 免鉴权 |
| [05-x-member](http/app/05-x-member.md) | `/x/member` | 16 | 免鉴权 |
| [06-x-member-realname](http/app/06-x-member-realname.md) | `/x/member/realname` | 8 | 免鉴权 |
| [07-x-passport-login](http/app/07-x-passport-login.md) | `/x/passport-login` | 13 | 免鉴权 |
| [08-video](http/app/08-video.md) | `/video` | 6 | 免鉴权 |
| [09-social](http/app/09-social.md) | `/social` | 12 | 免鉴权 |
| [10-feed](http/app/10-feed.md) | `/feed` | 7 | 免鉴权 |
| [11-upload](http/app/11-upload.md) | `/upload` | 5 | 免鉴权 |
| [12-catalog](http/app/12-catalog.md) | `/catalog` | 5 | 免鉴权 |
| [13-engagement](http/app/13-engagement.md) | `/engagement` | 12 | 免鉴权 |
| [14-playback](http/app/14-playback.md) | `/playback` | 3 | 免鉴权 |
| [15-danmaku](http/app/15-danmaku.md) | `/danmaku` | 6 | 免鉴权 |
| [16-search](http/app/16-search.md) | `/search` | 8 | 免鉴权 |
| [17-inbox](http/app/17-inbox.md) | `/inbox` | 5 | 免鉴权 |
| [18-comment](http/app/18-comment.md) | `/comment` | 7 | 免鉴权 |
| [19-notification](http/app/19-notification.md) | `/notification` | 2 | 免鉴权 |
| [20-up](http/app/20-up.md) | `/up` | 5 | 免鉴权 |
| [21-moderation](http/app/21-moderation.md) | `/moderation` | 1 | 免鉴权 |
| [22-private-message](http/app/22-private-message.md) | `/private-message` | 11 | 免鉴权 |
| [23-live](http/app/23-live.md) | `/live` | 15 | 免鉴权 |
| [24-membership](http/app/24-membership.md) | `/membership` | 6 | 免鉴权 |
| [25-coin](http/app/25-coin.md) | `/coin` | 7 | 免鉴权 |
| [26-wallet](http/app/26-wallet.md) | `/wallet` | 7 | 免鉴权 |
| [27-order](http/app/27-order.md) | `/order` | 6 | 免鉴权 |
| [28-creator-revenue](http/app/28-creator-revenue.md) | `/creator/revenue` | 8 | 免鉴权 |

### 运营面（313 条）

| 分组 | 前缀 | 路由 | 鉴权 |
|---|---|---|---|
| [01-admin](http/admin/01-admin.md) | `/admin` | 1 | 免鉴权 |
| [02-admin-account](http/admin/02-admin-account.md) | `/admin/account` | 2 | AdminPermission |
| [03-x-member](http/admin/03-x-member.md) | `/x/member` | 9 | AdminPermission |
| [04-admin-video](http/admin/04-admin-video.md) | `/admin/video` | 3 | 免鉴权 · AdminPermission |
| [05-admin-catalog](http/admin/05-admin-catalog.md) | `/admin/catalog` | 6 | 免鉴权 · AdminPermission |
| [06-admin-rights](http/admin/06-admin-rights.md) | `/admin/rights` | 5 | 免鉴权 · AdminPermission |
| [07-admin-moderation](http/admin/07-admin-moderation.md) | `/admin/moderation` | 4 | 免鉴权 · AdminPermission |
| [08-admin-transcode](http/admin/08-admin-transcode.md) | `/admin/transcode` | 4 | 免鉴权 · AdminPermission |
| [09-admin-asset](http/admin/09-admin-asset.md) | `/admin/asset` | 2 | 免鉴权 |
| [10-admin-danmaku](http/admin/10-admin-danmaku.md) | `/admin/danmaku` | 3 | 免鉴权 · AdminPermission |
| [11-admin-search](http/admin/11-admin-search.md) | `/admin/search` | 5 | 免鉴权 · AdminPermission |
| [12-admin-risk](http/admin/12-admin-risk.md) | `/admin/risk` | 11 | 免鉴权 · AdminPermission |
| [13-admin-operation](http/admin/13-admin-operation.md) | `/admin/operation` | 22 | 免鉴权 · AdminPermission |
| [14-admin-comment](http/admin/14-admin-comment.md) | `/admin/comment` | 5 | 免鉴权 · AdminPermission |
| [15-admin-notification](http/admin/15-admin-notification.md) | `/admin/notification` | 8 | 免鉴权 · AdminPermission |
| [16-admin-creator](http/admin/16-admin-creator.md) | `/admin/creator` | 3 | 免鉴权 |
| [17-admin-inbox](http/admin/17-admin-inbox.md) | `/admin/inbox` | 2 | AdminPermission |
| [18-admin-audit](http/admin/18-admin-audit.md) | `/admin/audit` | 11 | 免鉴权 · AdminPermission |
| [19-admin-ops](http/admin/19-admin-ops.md) | `/admin/ops` | 17 | 免鉴权 · AdminPermission |
| [20-admin-cron](http/admin/20-admin-cron.md) | `/admin/cron` | 18 | 免鉴权 · AdminPermission |
| [21-admin-live](http/admin/21-admin-live.md) | `/admin/live` | 56 | 免鉴权 · AdminPermission |
| [22-admin-recommend](http/admin/22-admin-recommend.md) | `/admin/recommend` | 17 | 免鉴权 · AdminPermission |
| [23-admin-collector](http/admin/23-admin-collector.md) | `/admin/collector` | 13 | 免鉴权 · AdminPermission |
| [24-admin-private-message](http/admin/24-admin-private-message.md) | `/admin/private-message` | 3 | 免鉴权 · AdminPermission |
| [25-admin-membership](http/admin/25-admin-membership.md) | `/admin/membership` | 10 | 免鉴权 · AdminPermission |
| [26-admin-payment](http/admin/26-admin-payment.md) | `/admin/payment` | 8 | 免鉴权 · AdminPermission |
| [27-admin-order](http/admin/27-admin-order.md) | `/admin/order` | 6 | 免鉴权 · AdminPermission |
| [28-admin-coin](http/admin/28-admin-coin.md) | `/admin/coin` | 4 | 免鉴权 · AdminPermission |
| [29-admin-creator-revenue](http/admin/29-admin-creator-revenue.md) | `/admin/creator-revenue` | 11 | 免鉴权 · AdminPermission |
| [30-admin-spm](http/admin/30-admin-spm.md) | `/admin/spm` | 15 | 免鉴权 · AdminPermission |
| [31-admin-feature-store](http/admin/31-admin-feature-store.md) | `/admin/feature-store` | 13 | 免鉴权 · AdminPermission |
| [32-admin-open-platform](http/admin/32-admin-open-platform.md) | `/admin/open-platform` | 16 | 免鉴权 · AdminPermission |

### RPC（43 个服务）

[按消费面分节的完整索引](./rpc/README.md)（终端面/运营面/两面/无网关消费方）
