# gateway/admin HTTP 接口索引

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

**共 313 条路由，按域拆成 32 个文件。**

基址：`http://127.0.0.1:8081`（来自 `gateway/admin/etc/admin.yaml` 的 `Port`）。

鉴权口径：
- 免鉴权 —— 网关无中间件。这类只读运营面刻意不进 `routePermissions`，取舍记录在各服务 README。
- `AdminPermission` —— 需 `Authorization: Bearer <admin_token>`，再按路由权限点判定；**表外路径 fail-closed 直接拒绝**。

| 文件 | 前缀 | 路由数 | 鉴权 | 小节 |
|---|---|---|---|---|
| [01-admin.md](./01-admin.md) | `/admin` | 1 | 免鉴权 | 1 |
| [02-admin-account.md](./02-admin-account.md) | `/admin/account` | 2 | AdminPermission | 1 |
| [03-x-member.md](./03-x-member.md) | `/x/member` | 9 | AdminPermission | 1 |
| [04-admin-video.md](./04-admin-video.md) | `/admin/video` | 3 | 免鉴权 · AdminPermission | 2 |
| [05-admin-catalog.md](./05-admin-catalog.md) | `/admin/catalog` | 6 | 免鉴权 · AdminPermission | 2 |
| [06-admin-rights.md](./06-admin-rights.md) | `/admin/rights` | 5 | 免鉴权 · AdminPermission | 2 |
| [07-admin-moderation.md](./07-admin-moderation.md) | `/admin/moderation` | 4 | 免鉴权 · AdminPermission | 2 |
| [08-admin-transcode.md](./08-admin-transcode.md) | `/admin/transcode` | 4 | 免鉴权 · AdminPermission | 2 |
| [09-admin-asset.md](./09-admin-asset.md) | `/admin/asset` | 2 | 免鉴权 | 1 |
| [10-admin-danmaku.md](./10-admin-danmaku.md) | `/admin/danmaku` | 3 | 免鉴权 · AdminPermission | 3 |
| [11-admin-search.md](./11-admin-search.md) | `/admin/search` | 5 | 免鉴权 · AdminPermission | 2 |
| [12-admin-risk.md](./12-admin-risk.md) | `/admin/risk` | 11 | 免鉴权 · AdminPermission | 2 |
| [13-admin-operation.md](./13-admin-operation.md) | `/admin/operation` | 22 | 免鉴权 · AdminPermission | 2 |
| [14-admin-comment.md](./14-admin-comment.md) | `/admin/comment` | 5 | 免鉴权 · AdminPermission | 2 |
| [15-admin-notification.md](./15-admin-notification.md) | `/admin/notification` | 8 | 免鉴权 · AdminPermission | 2 |
| [16-admin-creator.md](./16-admin-creator.md) | `/admin/creator` | 3 | 免鉴权 | 1 |
| [17-admin-inbox.md](./17-admin-inbox.md) | `/admin/inbox` | 2 | AdminPermission | 1 |
| [18-admin-audit.md](./18-admin-audit.md) | `/admin/audit` | 11 | 免鉴权 · AdminPermission | 2 |
| [19-admin-ops.md](./19-admin-ops.md) | `/admin/ops` | 17 | 免鉴权 · AdminPermission | 2 |
| [20-admin-cron.md](./20-admin-cron.md) | `/admin/cron` | 18 | 免鉴权 · AdminPermission | 2 |
| [21-admin-live.md](./21-admin-live.md) | `/admin/live` | 56 | 免鉴权 · AdminPermission | 8 |
| [22-admin-recommend.md](./22-admin-recommend.md) | `/admin/recommend` | 17 | 免鉴权 · AdminPermission | 2 |
| [23-admin-collector.md](./23-admin-collector.md) | `/admin/collector` | 13 | 免鉴权 · AdminPermission | 2 |
| [24-admin-private-message.md](./24-admin-private-message.md) | `/admin/private-message` | 3 | 免鉴权 · AdminPermission | 2 |
| [25-admin-membership.md](./25-admin-membership.md) | `/admin/membership` | 10 | 免鉴权 · AdminPermission | 2 |
| [26-admin-payment.md](./26-admin-payment.md) | `/admin/payment` | 8 | 免鉴权 · AdminPermission | 2 |
| [27-admin-order.md](./27-admin-order.md) | `/admin/order` | 6 | 免鉴权 · AdminPermission | 2 |
| [28-admin-coin.md](./28-admin-coin.md) | `/admin/coin` | 4 | 免鉴权 · AdminPermission | 2 |
| [29-admin-creator-revenue.md](./29-admin-creator-revenue.md) | `/admin/creator-revenue` | 11 | 免鉴权 · AdminPermission | 2 |
| [30-admin-spm.md](./30-admin-spm.md) | `/admin/spm` | 15 | 免鉴权 · AdminPermission | 2 |
| [31-admin-feature-store.md](./31-admin-feature-store.md) | `/admin/feature-store` | 13 | 免鉴权 · AdminPermission | 2 |
| [32-admin-open-platform.md](./32-admin-open-platform.md) | `/admin/open-platform` | 16 | 免鉴权 · AdminPermission | 2 |

小节 = 同一前缀下按鉴权/域拆出的 `@server` 块数；文件内每个小节自带独立标题、鉴权说明与类型附录。
