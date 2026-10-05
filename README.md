# Go-Video：面向视频社区的 go-zero 微服务项目

> 当前仓库先建立可开发的单仓骨架和领域契约，业务实现按路线图逐步补齐。支持 Android、iOS、HarmonyOS、电脑客户端和管理后台 Web；不支持小程序。会员、订单、支付/充值、投币和创作者分成已纳入本期范围（只走沙箱台账，不接真实支付渠道），广告投放、广告位分析和广告推荐仍在范围外——口径与理由见 [AGENTS.md](AGENTS.md) §1。

## 项目入口

- [AGENTS.md](AGENTS.md)：开发者和智能代理必须遵守的工程规则。
- [docs/development.md](docs/development.md)：本地环境、go-zero 代码生成、测试和提交流程。
- [docs/service-catalog.md](docs/service-catalog.md)：服务目录、数据所有权、依赖和拆分阶段。
- [docs/api-and-events.md](docs/api-and-events.md)：HTTP、RPC、事件和兼容性规范。
- [docs/data-design.md](docs/data-design.md)：核心对象、状态机、数据库和缓存原则。
- [docs/operations.md](docs/operations.md)：配置、部署、监控、备份和故障处理。
- [docs/roadmap.md](docs/roadmap.md)：按迭代阶段落地功能，避免一次性启动全部服务。
- [docs/commands.md](docs/commands.md)：所有安装、goctl 生成、运行、测试、迁移、部署和排障命令；也是命令的唯一维护位置。
- [docs/api/](docs/api/README.md)：逐接口清单，按域分组（终端面/运营面 HTTP 各一组文件 + 每个 RPC 服务一份契约文档）。
- [postman/](postman/README.md)：与上面同一份契约派生的 Postman 集合（folder 按域分组）和本地环境变量。

HTTP 响应统一规范见 [docs/api-and-events.md](docs/api-and-events.md)：所有接口返回 `code`、`message`、`data`、`ttl` 四字段。

## 当前仓库状态

当前仓库提供 `gateway/`（拆分为 `gateway/app` 和 `gateway/admin`）、`services/`、`common/`、`api/`、`deploy/`、`scripts/`、`docs/` 的可开发骨架，以及各领域服务说明文件。`gateway/app`、`gateway/admin` 已有 goctl 生成的 HTTP 框架代码（分别为 198 和 313 条路由，按 `internal/handler/routes.go` 的
`Method:` 条目数；admin 侧 313 = `AdminPermission` 保护 125 条 + 免鉴权读写 188 条），43 个领域服务都已落地 `rpc/*.proto` 契约、goctl RPC 生成代码、`model`、`etc/*.yaml`（含 `Validate()` 与配置加载用例）、可运行入口和 `deploy/migrations/` 迁移脚本（迁移在隔离实例 `127.0.0.1:3399` 逐个 `up` + `status` 复验为 `applied`；唯一例外是 2026-10-05 为事件链路新增的 `upload/000003` 与 `video/000004` 两个文件，仍是 `pending`，逐目录状态以 [deploy/migrations/README.md](deploy/migrations/README.md) 为权威）。
**完成度并不齐平**：43 个服务目录的 `internal/logic` 已全部落地，不再有返回 `model.ErrNotImplemented` 的空桩，也不返回伪造成功的空 Reply（仍会返回该哨兵的只剩三处**显式降级**而非空桩：`recommend-rank` 的未接线特征源替身、`account` 在 user-profile client 未注入时的委托分支、`cron` registry 对空结果的防线）；测试覆盖的强度按服务差异很大，逐包的用例数与「哪些层没有单测」以各服务 README 的「测试覆盖」一节为准（2026-10-05 实测样例：`live-media/internal/logic` 12 个测试文件、`251/252` 顶层/子用例，`open-platform/internal/logic` 10 个文件、`147/338`）；全仓也**没有做过端到端联调**，MQ 侧只有「可编译 / 可 `go vet` / 该包单测通过」这一级证据，不能按「已在生产提供的服务」对待。逐服务进度、刻意不通过 HTTP 暴露的内部方法、以及跨服务契约缺口见 [docs/roadmap.md](docs/roadmap.md)。

除业务逻辑、仓储实现、消费者、领域策略、迁移、事件 schema 和测试外，**所有服务目录中的 go-zero 框架代码必须由 goctl/protoc 生成或更新，禁止手写** handler、路由、types、ServiceContext、RPC client/server、`.pb.go` 和入口模板。修改 `.api`/`.proto` 后必须执行 [docs/commands.md](docs/commands.md) 中的增量更新命令。详见 [AGENTS.md](AGENTS.md)。

> 本文是产品与技术架构基线，不是把所有模块一次性拆成微服务的施工清单。设计参考哔哩哔哩公开可见的产品形态：视频投稿、创作中心、分区与标签、评论/弹幕、动态、直播、番剧/电影、搜索、推荐和开放平台。会员、订单、支付/充值（只走沙箱台账）、投币与创作者分成已在范围内（2026-09-22 修订，见 [AGENTS.md](AGENTS.md) §1）；广告投放、广告位分析与广告推荐仍不实现；电影、电视剧只是内容目录中的一种，不应被建模成“普通用户上传后即可播放”。

## 1. 结论与设计原则

### 1.0 HTTP API 统一响应

所有客户端 HTTP 接口统一返回以下响应信封，具体 `data` 类型由各服务 `.api` 契约定义并由 goctl 生成：

```json
{
  "code": 0,
  "message": "ok",
  "data": {},
  "ttl": 0
}
```

`code=0` 表示成功；`message` 成功固定为 `ok`，错误返回可读且不泄漏内部细节的消息；`data` 无内容时为 `{}`；`ttl` 是客户端缓存秒数，默认 `0`。HTTP 状态码仍必须正确表达 4xx/5xx。gRPC/RPC 保持 protobuf 原生响应和状态，不套 HTTP 信封。

### 1.1 现有方案需要修正的地方

1. **领域混杂**：把视频投稿、版权长视频和直播流放进同一条上传链路，导致权限、审核和版权规则无法落地。
2. **拆分层级不够清晰**：原方案按功能罗列了 18 个服务，但没有区分“目标领域边界”和“当前部署单元”。应保留更细的领域服务地图，同时按流量、团队和数据隔离需求分阶段独立部署，避免一开始创建大量空壳服务。
3. **SPM 职责需要明确**：本项目的 SPM 专指用户行为分析服务，负责采集播放、点击、搜索、跳过、点赞、收藏、关注等行为，生成视频推荐所需的统计指标和特征；不承担广告位追踪、广告投放或商业化分析。
4. **上传模型不完整**：缺少分片/断点续传、预签名上传、校验和、病毒扫描、内容安全、转码版本、媒资与播放资产的关系。
5. **一致性与安全边界不清**：播放量、点赞、收藏等高频行为不能依赖同步 MySQL 自增；网关 JWT 也不能直接当作所有内部服务的信任根。
6. **数据库容量被预先假定**：直接规定 8/16 库没有依据。应先单库设计、监控容量和热点，再按访问模式选择分片键和迁移方案。

### 1.2 目标

- 支持 Android、iOS、HarmonyOS 和电脑客户端（桌面端）；管理后台可使用 Web，统一提供 HTTPS API，必要时提供 WebSocket/SSE。
- 覆盖 UGC/PUGC 视频、直播、版权内容目录、创作者工具、社区互动、搜索推荐和运营管理。
- 重要操作可审计、可重试、幂等；最终一致的统计和搜索不阻塞主交易。
- 支持本地开发、Docker Compose 和 Kubernetes，但生产依赖优先使用托管 MySQL、Redis、对象存储、MQ、ES/CDN。
- 满足中国大陆场景下的内容审核、版权、隐私、未成年人保护和数据留存要求（具体合规事项由法务确认）。

### 1.3 演进原则

```text
模块化单体/少量服务
        │ 以事件和稳定 API 定义边界
        ▼
独立扩展热点链路（媒资、播放、互动、搜索、推荐、直播）
        │ 出现明确团队/容量/可靠性需求
        ▼
按领域拆分可独立发布的微服务
```

服务边界按“数据所有权”划分：一个领域只有一个写入者，跨域通过 API 或事件访问，禁止直接读写别的服务数据库。

### 1.4 参考 `openbilibili-go-common` 后的架构取舍

本地参考仓库只用于分析 B 站的领域拆分和设计理念，不作为本项目的目录模板。它体现出的 `videoup/archive/up/relation/dynamic/reply/dm/search` 等边界，说明投稿、稿件、创作者、关系链、动态、评论、弹幕、搜索不应全部塞进一个 `video` 服务；`interface`、`admin`、`job` 则说明外部接口、后台和异步任务应有清晰职责。

本项目仍严格采用 go-zero 的单仓多服务规范：顶层保留 `gateway/`（拆分为 `gateway/app` 终端客户端 BFF 和 `gateway/admin` 管理后台入口聚合两个独立子服务），业务服务统一位于 `services/`；每个服务按 goctl 生成的 API/RPC 结构组织。参考仓库中的 Kratos、Blademaster、Warden、旧版配置中心和 `cmd/conf/dao` 命名不直接照搬，只借鉴领域边界、服务独立发布和任务与在线请求隔离的思想。

## 2. 业务范围与概念模型

### 2.1 内容类型

| 类型 | 典型生产者 | 发布前提 | 播放资产 | 关键规则 |
|---|---|---|---|---|
| UGC/PUGC 视频 | 普通用户、UP 主、机构 | 投稿人授权、机器+人工审核 | 原始文件转码后的多码率资产 | 分区、标签、字幕、封面、可见范围、稿件版本 |
| 版权 PGC | 平台运营、版权方 | 合同、地域/时间/媒介权利校验 | 正片、预告、片头片尾、字幕 | 版权窗口、排期、下架时间 |
| 直播 | 主播/机构 | 主播资质、直播审核 | 实时流、录制回放 | 开播状态、禁播、延迟、CDN、弹幕 |
| 动态/专栏/课程（可选） | 用户、运营、机构 | 文本/图片/附件审核 | 富文本或附件 | 与视频、用户关系和消息联动 |

“电影/电视剧上传”应理解为版权方或运营人员对**内容目录和版权合同**进行媒资入库；文件上传只是媒资接入的一步，不能因此绕过审核、授权和发布策略。

### 2.2 核心对象关系

```text
作品 Work（电影/剧/番剧/纪录片）
 ├─季 Season ─集 Episode ─稿件 Submission ─媒资 Asset ─播放版本/清晰度 Variant
 └─版权合同 Rights（地域、时间、平台和下线策略）

用户 User ─创作者身份 Creator ─投稿 Submission
      └─关系/互动 Action（赞、收藏、分享、关注、不感兴趣）
视频/直播 ─评论 Comment ─回复 Reply
          └─弹幕 Danmaku（时间轴消息，单独的反垃圾和审核规则）
```

作品、稿件、媒资、播放版本和版权是不同对象；不要用一张 `video` 表承载全部状态。

## 3. 总体架构

```text
Android / iOS / HarmonyOS / 电脑客户端 / 管理后台 Web / 开放平台
                 │ HTTPS / WebSocket
        gateway/app (终端 BFF) + gateway/admin (管理后台入口)
  (鉴权、限流、风控、路由、灰度、审计、协议适配)
                 │ HTTP/gRPC
 ┌───────────────┼────────────────────────┐
 │核心交易域     │高吞吐与异步域             │数据与基础设施
 │身份/用户      │上传与媒资、转码、播放    │MySQL、Redis、对象存储/CDN
 │内容/稿件      │互动、弹幕、直播           │MQ、搜索、数仓、监控
 │版权/审核      │通知、推荐                 │
 └───────────────┴────────────────────────┘
                 │ Outbox/Event Bus
          搜索索引、推荐特征、统计报表
```

网关不拥有业务数据；BFF 只做面向端的聚合和裁剪。内部服务通过 mTLS/服务身份、短期 service token 或网格策略认证，不能因为请求来自网关就完全信任。

## 4. 服务边界（目标服务地图与分阶段部署）

可以继续拆分，建议将下表作为最终目标服务地图，再按阶段合并部署。拆分依据不是“每张表一个服务”，而是数据所有权、独立扩缩容、故障隔离、发布节奏和团队边界。

### 4.1 接入与身份域

| 领域/服务 | 数据所有权 | 主要能力 | 与 B 站形态的对应 |
|---|---|---|---|
| `gateway/app` | 无 | 路由、鉴权、限流、风控、聚合、灰度、审计 | Android/iOS/HarmonyOS/桌面端统一入口 |
| `gateway/admin` | 无 | 管理后台入口聚合、管理员鉴权、RBAC、限流、审计 | 管理后台 Web 统一入口 |
| `account` | 账号、设备、会话、登录凭证 | 注册登录、绑定、OAuth/OIDC、设备管理、会话撤销 | 账号中心 |
| `user-profile` | 用户资料、头像、隐私设置 | 个人空间、资料编辑、隐私和可见性 | 用户主页 |
| `creator` | 创作者身份、认证、投稿权限、创作数据摘要 | UP 主/机构认证、创作等级、权限和配额 | 创作中心身份 |
| `risk-control` | 风控规则、设备画像、处罚状态 | 登录/投稿/互动反作弊、限流、黑名单和封禁 | 安全中心 |

### 4.2 内容、版权与媒资域

| 服务 | 数据所有权 | 主要能力 |
|---|---|---|
| `video` | 视频稿件、稿件版本、可见范围、发布状态 | UGC/PUGC 投稿、编辑、定时发布、上下架 |
| `catalog` | 作品、季、集、分区、标签、系列关系 | 电影/电视剧/番剧/纪录片目录和剧集结构 |
| `rights` | 版权合同、地域和时间窗口 | PGC 授权校验、排期、撤权和到期事件 |
| `upload` | 上传会话、分片、校验和、配额 | 预签名 URL、断点续传、秒传、失败清理 |
| `asset` | 原文件、封面、字幕、音轨、截图等媒资元数据 | 媒资归档、版本、关联关系和生命周期 |
| `transcode` | 转码任务、Worker、播放版本 | FFmpeg 转码、截图、封面、HLS/DASH、任务重试 |
| `content-fingerprint` | 音视频指纹和匹配结果 | 重复投稿、版权比对、侵权线索 |
| `playback` | 播放会话、签名地址、清晰度策略 | 播放鉴权、防盗链、CDN 回源、播放心跳入口 |

### 4.3 直播与实时互动域

| 服务 | 数据所有权 | 主要能力 |
|---|---|---|
| `live-room` | 直播间、主播状态、直播配置 | 创建/关闭直播间、开播状态和房间信息 |
| `live-ingest` | 推流密钥、接入节点、流状态 | RTMP/SRT/WebRTC 接入、推流鉴权和健康检查 |
| `live-media` | 直播转码、录制、回放任务 | 多码率分发、录制切片、回放回收至 asset/video |
| `live-gateway` | 长连接会话和实时房间路由 | WebSocket、心跳、房间广播和连接治理 |
| `danmaku` | 时间轴弹幕、屏蔽词、用户屏蔽 | 弹幕发送/拉取、反刷、审核和分片存储 |
| `comment` | 评论、回复、举报、折叠状态 | 楼中楼、置顶、排序、审核联动 |
| `social-graph` | 关注、粉丝、黑名单、特别关注 | 用户关系写入、关系查询和增量事件 |
| `feed` | 动态、关注流、转发和可见性投影 | 动态发布、关注流 fan-out、屏蔽过滤 |
| `engagement` | 点赞、收藏、分享、不感兴趣、计数 | 高频互动幂等写入、异步计数和收藏夹 |

### 4.4 审核、搜索、推荐与数据域

| 服务 | 数据所有权 | 主要能力 |
|---|---|---|
| `moderation-orchestrator` | 审核任务、规则版本、审核结论、申诉 | 机审/人审编排、证据、复审和处罚联动 |
| `moderation-worker` | OCR/ASR/图像/音频检测结果 | 调用第三方或自建算法，回传可重试结果 |
| `search-indexer` | 搜索索引、联想词、索引版本 | 消费内容事件，构建视频/用户/直播/作品索引 |
| `search-query` | 查询日志、热词和排序配置 | 搜索、纠错、联想、过滤和分页 |
| `recommend-recall` | 召回池、候选集、特征快照 | 热门、关注、协同、标签和向量召回 |
| `recommend-rank` | 排序模型、实验配置、推荐结果 | 多目标排序、A/B 实验和降级策略 |
| `event-collector` | 客户端/服务端原始事件 | 埋点校验、采样、脱敏和可靠投递 |
| `spm` | 用户行为事件、视频指标和推荐特征 | 播放/点击/搜索/跳过/点赞/收藏/关注分析，完播率、留存和推荐特征计算 |
| `feature-store`（后续） | 推荐特征快照和用户画像 | 为推荐召回/排序提供在线特征读取和离线回填；不承担广告分析 |

### 4.5 消息、运营与开放平台域

| 服务 | 数据所有权 | 主要能力 |
|---|---|---|
| `inbox` | 站内信、系统消息、未读状态 | @提醒、审核结果、关注和动态通知 |
| `private-message` | 私信会话、消息、黑名单过滤 | 单聊、会话列表、撤回、已读和反骚扰 |
| `notification` | 投递记录、模板、供应商结果 | App Push、短信、邮件、重试和死信 |
| `operation` | 运营配置、RBAC、审计日志 | 管理后台 BFF、审核工作台、分区标签、专题配置、数据看板 |
| `ops-config` | 分区、标签、专题和推荐位 | 内容运营配置和灰度发布 |
| `audit` | 管理员操作、登录、审核审计 | 不可抵赖日志、查询和导出 |
| `open-platform` | 应用、密钥、授权、配额、Webhook | OAuth 授权、签名、限额、版本和撤销 |
| `cron` | 任务定义、执行记录、锁和租约 | 排期发布、版权到期、重试、报表和清理 |

### 4.6 与参考仓库领域的映射

下表用于避免命名漂移。左侧是本项目的领域名，右侧是参考仓库中能看到的相近职责；它们不是代码级依赖关系，也不意味着照搬旧接口。

| 本项目 | 参考职责 | 说明 |
|---|---|---|
| `video` + `upload` | `videoup` | 用户投稿入口、上传会话和稿件提交；上传与稿件状态仍然分开 |
| `video` | `archive` | 内部稿件/视频主数据、发布和状态查询 |
| `creator` | `up` | UP 主身份、认证、创作者资料和投稿权限 |
| `catalog` | `tv`、`pgc-season` 类职责 | 作品、季、集、番剧/电影目录；不与 UGC 稿件混表 |
| `social-graph` | `relation` | 关注、粉丝、黑名单等关系链 |
| `feed` | `dynamic`、`feed`、`reply-feed` | 动态发布和关注流投影，采用事件驱动 fan-out |
| `comment` | `reply`、`comment` | 评论、回复、举报和审核状态 |
| `danmaku` | `dm`、`dm2` | 时间轴弹幕，独立于普通评论的存储和实时通道 |
| `engagement` | `thumbup`、`favorite`、`share`、`history`、`coin` | 本项目只实现点赞、收藏、分享、历史等非商业互动；投币/支付类能力不实现 |
| `tag`（可并入 `catalog`） | `tag` | 分区、标签、频道和内容分类 |
| `playback` | `player`、`app-player` | 播放鉴权、清晰度、CDN 和播放质量数据 |
| `search-query` + `search-indexer` | `search`、`riot-search` | 查询面与索引面分离，避免索引重建影响线上查询 |
| `moderation-orchestrator` | `workflow`、`filter`、`antispam` | 机审、人审、申诉、反垃圾和规则编排 |
| `inbox` + `notification` | `sys-msg`、`notice-service`、`push`、`sms` | 站内消息、系统通知和外部投递拆开，统一事件驱动 |
| `gateway/app` + `gateway/admin` | `interface/main/web`、`app-view`、`app-feed` | 对外接口层只聚合领域服务，不拥有领域数据；终端与管理后台入口拆开部署 |
| `operation` + `ops-config` + `audit` | `admin/main/*` | 管理后台、运营配置和审计日志独立于用户端接口 |
| `cron` 及各服务 `internal/consumer` | `job/main/*`、`job/live/*` | 定时任务和 MQ 消费者独立运行，但业务写入仍由领域服务负责 |

#### 不直接照搬的部分

- 参考仓库中存在许多本项目范围之外的业务，本项目不实现商业化，因此不创建相关服务。
- 参考仓库按历史组织形成了许多端接口和内部服务；本项目由 `gateway/app` 承载 Android、iOS、HarmonyOS、桌面端的端适配与 BFF 聚合，由 `gateway/admin` 承载管理后台 Web 的入口聚合，两者独立部署；只有端侧流量或发布节奏明显不同再拆出独立 go-zero API 服务。
- `common` 不复制参考仓库的业务 model。跨服务共享的是 protobuf、事件 schema、错误码和基础客户端，业务实体通过 API 返回 DTO 或只读投影。

商业化服务本期不实现，也不纳入当前目录；如未来需要，只能在明确产品范围和合规要求后单独设计。

上表约 34 个逻辑服务，但不代表需要 34 个进程。推荐的部署合并关系如下：`account + user-profile + creator` 可先合并为 `identity`；`video + catalog + rights` 可先合并为 `content`；`upload + asset + transcode + content-fingerprint` 可先合并为 `media`；`live-room + live-ingest + live-media` 可先合并为 `live`；`social-graph + feed + engagement` 可先合并为 `community`；`moderation-orchestrator + moderation-worker` 可先合并为 `moderation`；`search-indexer + search-query`、`recommend-recall + recommend-rank`、`event-collector + spm` 也可分别合并。`operation + ops-config + audit` 可先合并为 `operation`，`cron` 独立作为任务进程。

建议独立部署的优先级：`upload/transcode`、`playback`、`live-gateway`、`danmaku`、`engagement`、`search-query` 和 `event-collector`。这些链路的流量特征、资源消耗或故障影响与普通 CRUD 服务明显不同；`spm` 可先作为 `event-collector` 的分析模块，达到数据量和计算隔离需求后再独立部署。

## 5. 关键业务流程

### 5.1 UGC/PUGC 投稿

1. 客户端向 `media` 创建上传会话，获得对象存储分片/预签名 URL；服务端校验大小、扩展名、MIME、校验和和配额。
2. 分片完成后，`media` 生成原文件 `asset_id`，执行病毒扫描、元数据探测和转码任务。
3. 用户在 `content` 创建稿件，填写标题、简介、封面、分区、标签、字幕、转载/原创声明和可见范围；稿件可保存草稿及多个版本。
4. 转码成功后进入 `moderation`：文本、封面、OCR、ASR、画面和版权指纹检测；命中规则进入人工审核或申诉队列。
5. 审核通过且投稿人有权发布，`content` 原子地发布稿件，写入 Outbox 事件；`search-indexer`、`recommend-recall`、`spm` 和 `inbox` 异步消费。
6. 播放时由 `playback` 检查登录、地区、版权窗口和风控，返回短期签名的 CDN 地址；播放心跳只进入数据链路，不在请求线程同步更新播放量。

### 5.2 版权电影/电视剧/番剧

1. 运营或版权方先创建 `Work/Season/Episode` 和 `Rights`，录入合同、授权地域、开始/结束时间、平台和下线条件。
2. 通过 `media` 导入正片、预告、花絮、封面、字幕和音轨；媒资与作品/集数关联，支持替换版本和回滚。
3. 内容安全、版权指纹、人工审核全部通过后，按排期发布；发布前由 `rights` 再次校验权利窗口。
4. 合同到期、地域变化或权利撤销时，事件驱动下架播放资产和搜索结果，并保留审计记录。

普通用户不能通过“电影上传”接口直接发布受版权保护的整片；这类内容与 UGC 共用媒资管线，但使用独立权限、审核和播放策略。

### 5.3 直播

```text
主播鉴权 → 创建直播间/推流密钥 → CDN/媒体入口接收 RTMP/SRT/WebRTC
        → 转码与分发 → 弹幕/风控
        → 下播 → 录制回放进入 media/content 审核后发布
```

直播间状态、推流状态和回放稿件分开建模；弹幕实时链路不能依赖评论表的同步事务。

### 5.4 互动、评论和通知

- 点赞、收藏、关注等写操作必须带幂等键，先写行为事实，再异步聚合计数；重复消费不能重复增加计数。
- 评论、回复、弹幕、私信分别限流和审核；被拉黑用户的可见性在查询层统一过滤。
- 互动事件进入 MQ，触发动态、@提醒、推荐特征和统计；通知投递要有模板、供应商路由、退避重试和死信队列。

## 6. 技术选型与可靠性基线

| 分类 | 推荐 | 约束 |
|---|---|---|
| API/RPC | go-zero HTTP + gRPC | protobuf/API 版本化，超时、重试、熔断和幂等明确 |
| 主库 | MySQL 8 | 先单库；按领域独立 schema，读写分离和分片以后置 |
| 缓存/计数 | Redis Cluster | 不把 Redis 当唯一事实源；计数异步落库、可重算 |
| MQ | Kafka（事件流）或 RabbitMQ（任务队列） | 同一类消息只选一种主方案，定义重试/死信/顺序键 |
| 对象存储/CDN | S3/OSS/MinIO + CDN | 私有桶、短期签名、分片上传、生命周期和跨区域策略 |
| 媒体处理 | FFmpeg + 可替换 Worker | 任务状态机、资源隔离、超时、取消、重试和幂等 |
| 搜索 | Elasticsearch/OpenSearch | 通过 Outbox/事件构建索引，版本号保证幂等 |
| 数据平台 | Kafka → Flink/ClickHouse/湖仓（按规模选） | 原始事件不可变，指标口径和隐私脱敏统一 |
| 可观测性 | OpenTelemetry + Prometheus + Loki/Tempo | trace_id、业务指标、SLO、告警和审计日志齐全 |

不要同时承诺 Kafka/RabbitMQ、Jaeger/Zipkin、ELK/Loki 等全部组合；每类基础设施选定一个默认实现，并把替换点封装在 `common` 接口中。

## 7. 数据与一致性

### 7.1 数据所有权

- 每个服务只写自己的库；跨域查询走 RPC/API 或维护只读投影。
- 事务内写业务数据和 Outbox 事件，异步发布；消费者使用 `event_id` 去重，失败进入重试和死信队列。
- 搜索、推荐、统计、通知均为最终一致；详情页的核心状态以内容服务为准。

### 7.2 分片与缓存

初期使用单库、合理索引、读副本和归档。只有在容量或热点有数据证明时才分片；分片前明确迁移、跨分片查询、唯一 ID、扩容和回滚方案。评论/弹幕/事件等按时间归档，不能无限保留在热表。

缓存采用 Cache Aside，并区分详情缓存、版权短缓存、幂等键、计数器、排行榜和会话。设置随机 TTL、热点保护、空值缓存和降级策略；播放量等指标必须允许从事件重算。

### 7.3 事件主题示例

| 主题 | 生产者 | 消费者 | 语义 |
|---|---|---|---|
| `media.task.v1` | upload（发布器已接线）/asset/transcode | transcode/content-fingerprint（两侧都无消费者实现） | 转码、截图、字幕、指纹 |
| `content.published.v1` | video（发布器已接线）/catalog/rights | search-indexer/inbox（消费者已接线）、recommend/spm（无消费者实现） | 稿件发布或下架 |
| `engagement.action.v1` | engagement/social-graph | event-collector/spm/recommend-recall/inbox | 赞、收藏、关注、分享 |
| `moderation.result.v1` | moderation-orchestrator | video/catalog/comment/danmaku | 审核结论 |
| `playback.heartbeat.v1` | playback | event-collector/spm | 播放进度和质量指标 |

事件必须包含 `event_id`、`event_type`、`schema_version`、`occurred_at`、`trace_id`、`producer` 和业务主键；禁止把完整个人敏感信息直接放进公共 Topic。

本表只是主题规划口径。**逐条的接线状态（哪条有生产者、哪条有消费者、挂在哪个构建标签上）以
[docs/api-and-events.md §5](docs/api-and-events.md) 为准**，两处不一致时以该节与服务 README 为权威。

## 8. 项目目录结构（go-zero 规范）

参考仓库只影响 `services/` 下的领域拆分，不改变本项目的 go-zero 单仓结构。HTTP 对外接口由 `gateway/app`（终端客户端）和 `gateway/admin`（管理后台）分别暴露；领域服务可以同时提供 go-zero API 和 RPC，异步消费者放在对应服务的 `internal/consumer` 中。

```text
go-video/
├── gateway/                         # 对外 HTTP/WebSocket 网关
│   ├── app/                         # 终端客户端 BFF（/api 前缀，公网）
│   └── admin/                       # 管理后台入口聚合（/admin 前缀，内网/VPN）
├── services/                        # 所有领域服务，统一 go-zero 结构
│   ├── account/                     # 账号、设备、会话
│   ├── user-profile/                # 用户资料和空间
│   ├── creator/                     # UP 主/创作者身份
│   ├── risk-control/                # 风控、黑名单和封禁
│   ├── video/                       # UGC/PUGC 稿件（参考 archive/videoup）
│   ├── catalog/                     # 电影/电视剧/番剧作品目录
│   ├── rights/                      # 版权窗口和发布约束
│   ├── upload/ asset/ transcode/ content-fingerprint/
│   ├── playback/                    # 播放鉴权和 CDN 地址
│   ├── live-room/ live-ingest/      # 直播间和推流接入
│   ├── live-media/ live-gateway/    # 直播媒体和实时连接
│   ├── social-graph/ feed/          # 关系链和动态流
│   ├── engagement/ comment/         # 互动和评论
│   ├── danmaku/                     # 弹幕实时链路
│   ├── moderation-orchestrator/     # 审核编排、人审和申诉
│   ├── moderation-worker/           # OCR/ASR/图像/音频检测 Worker
│   ├── search-indexer/ search-query/
│   ├── recommend-recall/ recommend-rank/
│   ├── event-collector/ spm/ feature-store/
│   ├── inbox/ private-message/ notification/
│   ├── operation/                   # 管理后台 BFF、运营配置和审计
│   ├── open-platform/               # 开放平台 API 和授权
│   └── cron/                        # 定时任务和补偿任务
├── common/                          # 稳定公共库，不放业务实体
├── api/                             # 公共 protobuf、事件 schema（可选）
├── deploy/ scripts/ docs/
├── go.mod
└── README.md
```

### 8.1 单个 go-zero 服务模板

每个 `services/*` 都可以独立构建、配置和发布；是否独立进程由部署阶段决定：

```text
services/video/
├── api/                             # go-zero HTTP API 定义（如需）
│   └── video.api
├── rpc/                             # protobuf 和 go-zero RPC 生成代码
│   └── video.proto
├── model/                           # 只访问本服务数据库的模型
├── etc/                             # 服务配置，例如 video.yaml
├── internal/
│   ├── config/                      # 配置结构
│   ├── handler/                     # HTTP handler
│   ├── logic/                       # 用例和领域编排
│   ├── server/                      # RPC server
│   ├── consumer/                    # MQ consumer/异步任务
│   ├── repository/                  # DAO/仓储实现
│   ├── svc/                         # ServiceContext 和依赖注入
│   └── types/                       # API 类型（通常由 goctl 生成）
├── Dockerfile
└── video.go                         # go-zero 入口
```

`gateway/app` 和 `gateway/admin` 只做路由、鉴权、限流、聚合和协议适配，不拥有领域数据；服务之间通过 RPC/API 或事件通信，禁止跨服务直接访问数据库。`common` 只放错误码、日志、追踪、Redis/MQ/OSS 客户端、幂等和事件 envelope 等稳定基础能力；API、protobuf 和事件 schema 必须版本化并做兼容性检查。

## 9. 安全、审核与合规

- 用户登录采用 OAuth 2.1/OIDC 思路：短期 access token、可轮换 refresh token、设备会话撤销；密码使用 Argon2id 或 bcrypt，验证码限频且防撞库。
- 网关做 WAF、限流、设备/账号风控；服务间使用 mTLS 或工作负载身份。日志脱敏，密钥进入 Secret/Vault，不进 Git 和镜像。
- 上传采用预签名 URL，服务端再次校验对象归属；限制大小、格式和压缩炸弹，执行病毒扫描、内容指纹和恶意文件隔离。
- 文本、图片、视频、音频、OCR、ASR 均进入机审+人审；结论、规则版本、证据、操作人和申诉记录可追溯。
- 对未成年人、隐私、数据导出/删除、版权投诉和内容留存设置明确策略；具体备案、许可和合规事项由法务确认。

## 10. 部署、观测与验收

### 10.1 部署

- 本地：只启动必要依赖和少量服务，提供种子数据与一键迁移；不要要求开发机运行完整生产集群。
- 测试：Docker Compose 或临时 Kubernetes，使用独立对象存储桶、Topic、数据库和密钥。
- 生产：无状态服务 Deployment + HPA；MySQL、Redis、MQ、ES、对象存储和 CDN 优先托管。备份、跨可用区、恢复演练和数据保留策略写入运行手册。

### 10.2 可观测性与验收指标

至少监控：上传成功率、转码排队/失败率、首帧时间、播放卡顿率、直播端到端延迟、审核积压、搜索延迟、推荐点击率、推荐召回/排序耗时、SPM 事件接收成功率、消息投递成功率和各服务 SLO。每个异步任务都要能查询状态、取消、重试和定位 `trace_id`。

### 10.3 建议迭代顺序

1. `identity(account + user-profile + creator)`、`content(video + catalog + rights)`、`media(upload + asset + transcode)`、`community(social-graph + feed + engagement)`、`operation(operation + ops-config + audit)` 五个部署单元，打通账号、UGC 投稿、媒资上传/转码、审核、播放和基础互动。
2. 独立 `playback`、`comment`、`danmaku`、`search-query`、`notification`，承接高并发读写和实时链路。
3. 拆分 `live-room/live-ingest/live-media/live-gateway`，上线直播；拆分 `catalog/rights`，上线版权内容目录。
4. 拆分 `search-indexer`、`recommend-recall/rank`、`event-collector/spm`、`open-platform`，建设搜索、推荐分析和开放 API；SPM 只服务于用户行为分析和视频推荐。

每一步都先定义 API、事件、权限、状态机、SLO 和数据留存，再决定是否拆成独立服务。

## 11. 参考状态机

### 投稿/媒资

```text
DRAFT → UPLOADING → UPLOADED → SCANNING → TRANSCODING
      → READY_FOR_REVIEW → APPROVED → SCHEDULED → PUBLISHED
                         └→ REJECTED/APPEAL
PUBLISHED → OFFLINE/EXPIRED/DELETED（保留审计和必要证据）
```

状态迁移必须由拥有者服务执行，带版本号和幂等请求；回调只允许推进合法状态，不能直接把任意稿件改成 `PUBLISHED`。

### 版权窗口

```text
待录入 → 待验权 → 生效 → 排期/上架 → 到期/撤权 → 下架
```

播放鉴权和定时任务都要检查版权窗口，避免只依赖一次性的发布任务。

## 12. 总结

这套架构可以承载类似 B 站的产品方向，但落地重点是正确的业务建模和演进顺序：UGC 投稿、直播、版权 PGC、互动社区、审核、推荐和运营各有独立规则；媒资上传、转码和播放是共享基础能力，不等于内容拥有发布权。先用少量可部署单元验证闭环，以数据证明容量和团队边界后再拆服务，配合 Outbox、幂等、可观测性和合规审计，才能在复杂度增长时保持可维护性。
