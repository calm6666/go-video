# gateway/app HTTP 接口索引

> 由 `node scripts/gen-api-docs.mjs` 从契约真源生成，**请勿手工编辑**；改接口先改 `.api`/`.proto` 再重新生成。

**共 198 条路由，按域拆成 28 个文件。**

基址：`http://127.0.0.1:8080`（来自 `gateway/app/etc/app.yaml` 的 `Port`）。

鉴权口径：
- 免鉴权 —— 网关无中间件。终端身份按本仓约定由 `mid` 入参携带，不是可信凭据。
- `AppkeyVerify` —— 仅 `/account/privacy`，query `appkey` 白名单。

| 文件 | 前缀 | 路由数 | 鉴权 | 小节 |
|---|---|---|---|---|
| [01-api.md](./01-api.md) | `/api` | 1 | 免鉴权 | 1 |
| [02-account.md](./02-account.md) | `/account` | 10 | 免鉴权 · AppkeyVerify | 2 |
| [03-account-v1.md](./03-account-v1.md) | `/account/v1` | 4 | 免鉴权 | 1 |
| [04-account-v2.md](./04-account-v2.md) | `/account/v2` | 2 | 免鉴权 | 1 |
| [05-x-member.md](./05-x-member.md) | `/x/member` | 16 | 免鉴权 | 2 |
| [06-x-member-realname.md](./06-x-member-realname.md) | `/x/member/realname` | 8 | 免鉴权 | 1 |
| [07-x-passport-login.md](./07-x-passport-login.md) | `/x/passport-login` | 13 | 免鉴权 | 2 |
| [08-video.md](./08-video.md) | `/video` | 6 | 免鉴权 | 2 |
| [09-social.md](./09-social.md) | `/social` | 12 | 免鉴权 | 2 |
| [10-feed.md](./10-feed.md) | `/feed` | 7 | 免鉴权 | 2 |
| [11-upload.md](./11-upload.md) | `/upload` | 5 | 免鉴权 | 1 |
| [12-catalog.md](./12-catalog.md) | `/catalog` | 5 | 免鉴权 | 1 |
| [13-engagement.md](./13-engagement.md) | `/engagement` | 12 | 免鉴权 | 2 |
| [14-playback.md](./14-playback.md) | `/playback` | 3 | 免鉴权 | 1 |
| [15-danmaku.md](./15-danmaku.md) | `/danmaku` | 6 | 免鉴权 | 1 |
| [16-search.md](./16-search.md) | `/search` | 8 | 免鉴权 | 1 |
| [17-inbox.md](./17-inbox.md) | `/inbox` | 5 | 免鉴权 | 1 |
| [18-comment.md](./18-comment.md) | `/comment` | 7 | 免鉴权 | 1 |
| [19-notification.md](./19-notification.md) | `/notification` | 2 | 免鉴权 | 1 |
| [20-up.md](./20-up.md) | `/up` | 5 | 免鉴权 | 1 |
| [21-moderation.md](./21-moderation.md) | `/moderation` | 1 | 免鉴权 | 1 |
| [22-private-message.md](./22-private-message.md) | `/private-message` | 11 | 免鉴权 | 1 |
| [23-live.md](./23-live.md) | `/live` | 15 | 免鉴权 | 2 |
| [24-membership.md](./24-membership.md) | `/membership` | 6 | 免鉴权 | 1 |
| [25-coin.md](./25-coin.md) | `/coin` | 7 | 免鉴权 | 1 |
| [26-wallet.md](./26-wallet.md) | `/wallet` | 7 | 免鉴权 | 1 |
| [27-order.md](./27-order.md) | `/order` | 6 | 免鉴权 | 1 |
| [28-creator-revenue.md](./28-creator-revenue.md) | `/creator/revenue` | 8 | 免鉴权 | 1 |

小节 = 同一前缀下按鉴权/域拆出的 `@server` 块数；文件内每个小节自带独立标题、鉴权说明与类型附录。
