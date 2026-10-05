// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

// Package middleware 存放 gateway/admin 的 HTTP 中间件扩展。
// 中间件实现属于业务扩展，允许手写（骨架由 goctl 生成，
// 挂载点由 internal/handler/routes.go 依据 admin.api 的 `middleware: AdminPermission` 生成）。
//
// 职责边界（AGENTS.md §3/§6）：网关只做「这条路由需要什么权限 + 你会不会」的判定入口，
// 角色、权限点、会话有效性全部由 services/operation 判定：
//   - 网关不解析 token 摘要、不查后台会话表，因此不持有 OPERATION_ADMIN_TOKEN_SECRET；
//   - 网关不缓存判定结果：operation 已按 RBAC 版本失效自己的快照，
//     网关再缓存会让「禁用账号立即失效」这条保证退化（reply.ttl 因此不下发给调用方）；
//   - 判定失败与异常一律 fail-closed：写统一响应信封并中断请求，绝不透传到 logic。
package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"go-video/common/httpresponse"
	operationrpc "go-video/services/operation/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/rest/httpx"
)

// traceIDHeader 是链路追踪 ID 的请求头约定（docs/api-and-events.md：
// trace_id 通过响应头或网关扩展字段传递）。缺失时传空串，由 operation 侧留空。
const traceIDHeader = "X-Trace-Id"

// reasonSessionInvalid 与 services/operation/internal/logic/helpers.go 的同名常量对应：
// 会话非法（token 过期/被吊销）应以 401 表达，权限不足以 403 表达。
// 这里重复字面量而不是 import 服务内部常量：跨服务只能依赖公开契约（AGENTS.md §5），
// 该字符串是 VerifyAdminPermissionReply.reason 的对外稳定值。
const reasonSessionInvalid = "admin_session_invalid"

var (
	operationCliMu sync.RWMutex
	// operationCli 是 AdminPermission 判定要调用的 operation RPC 客户端。
	// 由 internal/svc/servicecontext.go 构造时注入（与 gateway/app 的
	// AppkeyVerify 中间件用 SetPrivacyAppKeys 注入依赖的机制完全一致）；
	// nil 表示未配置 OperationRPC，此时受保护路由一律 503，不放行。
	operationCli operationrpc.OperationClient
)

// SetOperationClient 注入 operation RPC 客户端；传 nil 表示未配置，中间件拒绝一切受保护请求。
func SetOperationClient(cli operationrpc.OperationClient) {
	operationCliMu.Lock()
	defer operationCliMu.Unlock()
	operationCli = cli
}

// operationClient 读取已注入的客户端（并发安全）。
func operationClient() operationrpc.OperationClient {
	operationCliMu.RLock()
	defer operationCliMu.RUnlock()
	return operationCli
}

// adminPermission 是一条路由所需的权限点（resource + action）。
type adminPermission struct {
	Resource string
	Action   string
}

// routePermissions 是 /admin/operation 受保护路由的静态权限表。
//
// 键是注册后的完整路径（rest.WithPrefix 后 r.URL.Path 就是这个值），
// 值遵循 operation.proto 对 resource 的命名口径 `<域>:<实体>` + 动作：
// 账号/角色/权限/菜单/任务/审计属 operation 域，运营配置沿用 proto 注释里的 ops:config。
//
// 表外路由一律拒绝（fail-closed），所以新增路由必须同时在这里登记，
// 避免出现「有路由无权限定义 = 人人可访问」的空洞。
//
// 契约缺口：operation.proto 没有提供权限点目录或 ListAllResources 之类的契约，
// 可判定的 resource 取值完全取决于运营通过 CreatePermission 落库的 op_permission 行，
// 本表与库内权限点的一致性只能靠上线前的授权数据核对（见 gateway/admin/README.md）。
var routePermissions = map[string]adminPermission{
	"/admin/operation/user/create":       {Resource: "operation:admin_user", Action: "create"},
	"/admin/operation/user/update":       {Resource: "operation:admin_user", Action: "update"},
	"/admin/operation/user/disable":      {Resource: "operation:admin_user", Action: "disable"},
	"/admin/operation/user/list":         {Resource: "operation:admin_user", Action: "read"},
	"/admin/operation/role/assign":       {Resource: "operation:role", Action: "assign"},
	"/admin/operation/role/create":       {Resource: "operation:role", Action: "create"},
	"/admin/operation/role/list":         {Resource: "operation:role", Action: "read"},
	"/admin/operation/role/delete":       {Resource: "operation:role", Action: "delete"},
	"/admin/operation/permission/list":   {Resource: "operation:permission", Action: "read"},
	"/admin/operation/permission/create": {Resource: "operation:permission", Action: "create"},
	"/admin/operation/menu/get":          {Resource: "operation:menu", Action: "read"},
	"/admin/operation/menu/save":         {Resource: "operation:menu", Action: "update"},
	"/admin/operation/config/get":        {Resource: "ops:config", Action: "read"},
	"/admin/operation/config/save":       {Resource: "ops:config", Action: "update"},
	"/admin/operation/task/submit":       {Resource: "operation:task", Action: "create"},
	"/admin/operation/task/get":          {Resource: "operation:task", Action: "read"},
	"/admin/operation/task/list":         {Resource: "operation:task", Action: "read"},
	"/admin/operation/task/cancel":       {Resource: "operation:task", Action: "cancel"},
	"/admin/operation/task/run":          {Resource: "operation:task", Action: "run"},
	"/admin/operation/audit/list":        {Resource: "operation:audit", Action: "read"},
	// audit 域的写入口（导出申请/推进、保留策略、归档触发）。
	// 只读的检索与链校验不在本表内：它们走免中间件路由组，由 audit 侧的
	// action_domain=data_access 自审计留痕（见 services/audit/rpc/audit.proto）。
	// 权限码沿用 proto 的 `<域>:<实体>` 口径，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/audit/export/create":  {Resource: "audit:export", Action: "create"},
	"/admin/audit/export/run":     {Resource: "audit:export", Action: "run"},
	"/admin/audit/retention/save": {Resource: "audit:retention", Action: "update"},
	"/admin/audit/archive/run":    {Resource: "audit:archive", Action: "create"},
	// ops-config 域的写入口（发布式配置、灰度、专题/坑位条目、开关、缓存失效）。
	// 只读的 config/rollout/topic/slot/switch 列表与 topic/get 走免中间件路由组，与 audit 读面同一口径：
	// 后台列表页每次刷新都会打一条 RPC，若全部经 VerifyAdminPermission 会把 operation
	// 变成读放大瓶颈，而 ops-config 侧的 audit_entry_id 已经留下了「谁改了」的证据。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/ops/config/publish":   {Resource: "ops:config", Action: "publish"},
	"/admin/ops/config/rollback":  {Resource: "ops:config", Action: "rollback"},
	"/admin/ops/rollout/save":     {Resource: "ops:rollout", Action: "update"},
	"/admin/ops/rollout/state":    {Resource: "ops:rollout", Action: "enable"},
	"/admin/ops/topic/save":       {Resource: "ops:topic", Action: "update"},
	"/admin/ops/topic/items/save": {Resource: "ops:topic_item", Action: "update"},
	"/admin/ops/slot/save":        {Resource: "ops:slot", Action: "update"},
	"/admin/ops/slot/items/save":  {Resource: "ops:slot_item", Action: "update"},
	"/admin/ops/switch/save":      {Resource: "ops:switch", Action: "update"},
	"/admin/ops/cache/refresh":    {Resource: "ops:cache", Action: "refresh"},
	// cron 域的写入口（任务定义注册/修改/启停、手动触发、人工重试、游标推进）。
	// 只读的 task/run/checkpoint/lease/audit 列表与 scheduler/health 走免中间件路由组，
	// 与 audit、ops-config 读面同一口径（见上面的注释与 gateway/admin/README.md）。
	// 暂停与停用分开授权：pause 可恢复且过期点按 MisfirePolicy 补偿，disable 是终态只能重新注册；
	// 人工触发与人工重试也分开，后者是在已终结的执行上追加 attempt。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/cron/task/register":   {Resource: "cron:task", Action: "create"},
	"/admin/cron/task/update":     {Resource: "cron:task", Action: "update"},
	"/admin/cron/task/pause":      {Resource: "cron:task", Action: "pause"},
	"/admin/cron/task/resume":     {Resource: "cron:task", Action: "resume"},
	"/admin/cron/task/disable":    {Resource: "cron:task", Action: "disable"},
	"/admin/cron/task/trigger":    {Resource: "cron:task", Action: "trigger"},
	"/admin/cron/run/retry":       {Resource: "cron:run", Action: "retry"},
	"/admin/cron/checkpoint/save": {Resource: "cron:checkpoint", Action: "update"},
	// live-room 域的写入口（运营下架房间、禁播下发与解除、直播配置、分区字典维护）。
	// 只读的房间/场次/分区/主播绑定/禁播台账走免中间件路由组，与 audit、ops-config、cron
	// 读面同一口径（后台列表页每次刷新都会打一次 RPC，全量挂判定会把 operation 变成读放大瓶颈）。
	// 关闭与禁播分开授权：close 是终态下架（FINISHED，房间不复用），ban 可被 LiftBan 解除回 READY；
	// 禁播下发与解除也分开，避免「能解封」的角色顺带获得「能封人」的能力；
	// 分区（字典定义）与房间配置（单个房间行为）分属不同角色，也不共用一个 live:room 权限点。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/live/room/close":     {Resource: "live:room", Action: "close"},
	"/admin/live/room/ban":       {Resource: "live:ban", Action: "create"},
	"/admin/live/room/ban/lift":  {Resource: "live:ban", Action: "lift"},
	"/admin/live/setting/update": {Resource: "live:setting", Action: "update"},
	"/admin/live/area/upsert":    {Resource: "live:area", Action: "update"},
	// live-ingest 域的写入口（强制断流、吊销推流密钥、接入节点登记、失败事件重试）。
	// 只读的流状态/密钥台账/健康/节点/分配/断流/事件走免中间件路由组，与 live-room 读面同一口径。
	// 断流与吊销分开授权：吊销是不可逆终态且能级联停流，比一次性的断流更重；
	// 节点登记（改容量、摘流）影响所有后续分配，事件重试只动 outbox 的发布状态，各自独立；
	// 事件重试单列是因为它能批量把失败事件重新推给所有消费方，不该混在 live:stream 里。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/live/stream/close":      {Resource: "live:stream", Action: "close"},
	"/admin/live/stream/key/revoke": {Resource: "live:key", Action: "revoke"},
	"/admin/live/node/upsert":       {Resource: "live:node", Action: "update"},
	"/admin/live/event/retry":       {Resource: "live:event", Action: "retry"},
	// live-gateway 域的写入口（强制下线、排空房间路由、房间内下发、接入配额）。
	// 只读的连接列表/路由/广播审计/配额读取走免中间件路由组，同上口径。
	// 强制下线与排空路由分开授权：kick 针对单个用户/连接（含可写的禁止重连窗口），
	// drain 针对整房间的下发拓扑，影响面差一个量级；
	// 房间内下发（BroadcastToRoom）单列——它是唯一能主动把内容推到在线连接的能力，
	// 不与 live:connection 的处置权混用；配额是全局继承链的配置，改一层会影响所有下级作用域。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/live/connection/kick": {Resource: "live:connection", Action: "kick"},
	"/admin/live/route/drain":     {Resource: "live:route", Action: "drain"},
	"/admin/live/broadcast/send":  {Resource: "live:broadcast", Action: "send"},
	"/admin/live/quota/upsert":    {Resource: "live:quota", Action: "update"},
	// recommend 域的写入口（召回池数据与版本指针、模型/特征/实验的登记与状态迁移）。
	// 只读的池快照、版本台账、召回日志、决策摘要与运行时配置走免中间件路由组，
	// 与 audit、ops-config、cron、live 读面同一口径（后台列表页每次刷新都会打一次 RPC）。
	// 池的三条指针动作分开授权：publish 是「按流程上线一个 READY 版本」，
	// rollback 是「把刚上线的版本撤下去」（应急开关，能不发布就能回滚），
	// prune 会真删历史版本行（删了就没了回放证据），因此三者各占一个 action，
	// 不让「能上线池版本」顺带等于「能销毁审计数据」。
	// 模型与实验的「登记」与「状态迁移」也分开：能登记一个还没上线的模型版本/实验变体，
	// 不等于能把它切成线上生效——后者才是真正影响所有终端用户推荐结果的动作。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/recommend/pool/item/upsert":           {Resource: "recommend:pool", Action: "update"},
	"/admin/recommend/pool/version/publish":       {Resource: "recommend:pool", Action: "publish"},
	"/admin/recommend/pool/version/rollback":      {Resource: "recommend:pool", Action: "rollback"},
	"/admin/recommend/pool/version/prune":         {Resource: "recommend:pool", Action: "prune"},
	"/admin/recommend/rank/model/upsert":          {Resource: "recommend:model", Action: "create"},
	"/admin/recommend/rank/model/state":           {Resource: "recommend:model", Action: "state"},
	"/admin/recommend/rank/feature-config/upsert": {Resource: "recommend:feature", Action: "create"},
	"/admin/recommend/rank/experiment/upsert":     {Resource: "recommend:experiment", Action: "create"},
	"/admin/recommend/rank/experiment/state":      {Resource: "recommend:experiment", Action: "state"},
	// live-media 域的写入口（直播转码任务的登记/停止/重试/取消、分发档位的上下线、录制任务的
	// 起止、回放拼接与引用回填与投影刷新、回收任务提交）。
	// 只读的六张台账（转码/档位/录制/切片/回放/引用/回收）走免中间件路由组，与 live-room、
	// live-ingest、live-gateway、audit、ops-config、cron、recommend 读面同一口径。
	// 转码的四个动作分开授权，因为它们后果不同且不可互相替代：start 新增一路转码（会产生成本），
	// stop 让运行中的任务收尾（Worker 仍是执行体，可停在 STOPPED），retry 是把 FAILED 的任务重新
	// 放回队列（再次拉起、attempt+1，等于再次产生成本），cancel 是放弃任务且**不可复活**（终态）。
	// 合成一个「操作转码」点会让「能取消」顺带等于「能重开」。
	// 档位的 upsert 与 offline 也分开：offline 直接影响观众侧可用性（该档位立刻从分发列表消失），
	// 与登记/刷新一个档位是相反方向的动作。
	// 录制的 start 与 stop 对应两条独立状态机里的两端（录制必须能断点续录，stop 之后才能拼回放）。
	// 回放三条按后果分列：submit 只登记拼接意图、bind 只写 asset/稿件引用行、
	// state 只刷新 video 侧的只读投影 —— 三条**都不得**推进稿件状态（AGENTS.md §5/§8），
	// 但把它们并成一点会让「点一次刷新投影」顺带获得「提交一次拼接」的能力。
	// 回收只有一条 submit：purge=true 会真删对象存储引用，但契约里它是 submit 的布尔参数而非
	// 独立方法，无法拆成两点授权（补偿口径是 reason 必填 + operator 由会话渲染，见 README 缺口）。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/live/transcode/start":      {Resource: "live:transcode", Action: "start"},
	"/admin/live/transcode/stop":       {Resource: "live:transcode", Action: "stop"},
	"/admin/live/transcode/retry":      {Resource: "live:transcode", Action: "retry"},
	"/admin/live/transcode/cancel":     {Resource: "live:transcode", Action: "cancel"},
	"/admin/live/output/upsert":        {Resource: "live:output", Action: "update"},
	"/admin/live/output/offline":       {Resource: "live:output", Action: "offline"},
	"/admin/live/record/start":         {Resource: "live:record", Action: "start"},
	"/admin/live/record/stop":          {Resource: "live:record", Action: "stop"},
	"/admin/live/replay/submit":        {Resource: "live:replay", Action: "submit"},
	"/admin/live/replay/asset/bind":    {Resource: "live:replay", Action: "bind"},
	"/admin/live/replay/content/state": {Resource: "live:replay", Action: "state"},
	"/admin/live/retention/submit":     {Resource: "live:retention", Action: "submit"},
	// event-collector 域的写入口（投递推进、死信重放、策略草稿与生效切换）。
	// 只读的批次/事件台账、死信列表、策略读取与采集健康度走免中间件路由组，
	// 与 audit、ops-config、cron、live、recommend 读面同一口径（排障页每次刷新都会打一次 RPC）。
	// 四条动作的影响面互不相同，因此各占一个权限点：
	//   - delivery/retry 只把「已到期的在途事件」再推一轮，是有 cron 兜底任务在做的常规动作；
	//   - deadletter/replay 把已被判定放弃的事件送回队列，会改变下游已经收敛过的结论，
	//     且它是唯一能让「已死」事件重新出现在 spm 输入里的入口，不能跟着「推进一轮」一起送来；
	//   - policy/update 只写 DRAFT（不生效，对未来没有任何影响）；
	//   - policy/enable 切换 ACTIVE，立刻改变所有终端上报的采样与脱敏口径，
	//     进而决定下游特征完整性——本域最重的一步，必须能单独授予与单独收回。
	// 网关侧还有一道配套约束：/policy/upsert 的表单没有 state 位，
	// 想「改完就生效」必须再走一次 /policy/activate，权限点无法被顺带绕过。
	// 权限码同样是 `<域>:<实体>` + 动作，需要 op_permission seed 迁移补齐（见 README）。
	"/admin/collector/delivery/retry":     {Resource: "collector:delivery", Action: "retry"},
	"/admin/collector/dead-letter/replay": {Resource: "collector:deadletter", Action: "replay"},
	"/admin/collector/policy/upsert":      {Resource: "collector:policy", Action: "update"},
	"/admin/collector/policy/activate":    {Resource: "collector:policy", Action: "enable"},

	// 私信运营面两条写入口各占一个权限点，拆开的理由是「能不能销毁证据」：
	//   - pm:report:handle 把一条举报推到终态（DISMISS 会让真实举报静默消失，
	//     PUNISH 还会把当事人交给 risk-control）；
	//   - pm:retention:purge 是**物理删除私信正文**，不可逆，且影响面是一批消息而不是一条，
	//     因此不与处置共用权限点：能处置举报的人不应默认能删正文。
	// 举报台账读取（/report/list）不在表里，与 audit/ops-config/cron/live/collector 的只读面同口径。
	// 权限码需要 op_permission seed 迁移补齐（见 README），未 seed 时两个写入口一律 403。
	"/admin/private-message/report/handle":   {Resource: "pm:report", Action: "handle"},
	"/admin/private-message/retention/purge": {Resource: "pm:retention", Action: "purge"},

	// 商业化五域的写入口（会员/支付/交易订单/硬币/创作者分成）。这五组是全仓唯一
	// 会改动「权益台账」与「资金台账」的后台面，因此分组比前面任何域都更细。
	//
	// 共同口径：五域的只读面（/plan/list /member/get /grant/list /expiring/list
	// /entitlement/list、/wallet/get /recharge/list /payment/list /refund/list /flow/list
	// /channel/describe、/list /get /event/list /stuck/list、/account/get /flow/list
	// /toss/config、/rule/* /enrollment/list /metric/list /settlement/*）都不在本表里，
	// 与 audit/ops-config/cron/live/recommend/collector/pm 的读面同一口径 —— 后台列表页
	// 每次刷新都会打一次 RPC，全量挂判定会把 operation 变成读放大瓶颈，
	// 而「谁查了什么」由服务侧 audit_entry 留痕，不靠网关逐条拦。
	//
	// 五域一律 fail-closed：表内没有任何「未登记的写路径」，goctl 新写的路由若忘了在这里
	// 登记，中间件会直接 403 而不是放行（见 route_permission_drift_test.go 的反向断言）。
	//
	// membership 域按「改目录 / 给人权益」两个方向分三点：
	//   - membership:plan 的 update 只写 DRAFT 草稿（无 state 位，改完不会自动生效），
	//     publish 才把套餐推进到在售状态 —— 能改文案价格的人不该默认能把套餐上架；
	//     改价必须走新草稿正是为此（ACTIVE 行不可原地改价，见 admin.api 与 membership.proto）；
	//   - membership:grant 的 create 与 revoke 分开：发放是「白送一段会员」（可被收回纠正），
	//     收回是把用户已经在用的权益立刻拔掉（可立即失效，也可按天扣回），
	//     后果方向相反，且 revoke 是客服投诉里最容易误用的动作，必须能单独收回授权；
	//   - membership:entitlement 单列：权益码开关是全局能力位，关掉一个码会让全站该能力判否
	//     （影响所有会员，不是一个用户），比改一个套餐的目录影响面大一个量级。
	// ExpireMembership 与 SetAutoRenew 没有后台路由：前者是 cron/服务侧到期动作，
	// 后台点它等于手工改状态；后者是用户本人的订阅意愿，属 gateway/app 面（AGENTS.md §1）。
	// 权限码需要 op_permission seed 迁移补齐（见 README），未 seed 时写入口一律 403。
	"/admin/membership/plan/upsert":        {Resource: "membership:plan", Action: "update"},
	"/admin/membership/plan/state":         {Resource: "membership:plan", Action: "publish"},
	"/admin/membership/grant":              {Resource: "membership:grant", Action: "create"},
	"/admin/membership/grant/revoke":       {Resource: "membership:grant", Action: "revoke"},
	"/admin/membership/entitlement/upsert": {Resource: "membership:entitlement", Action: "update"},

	// payment 域只有两个写入口，且分属两种完全不同性质的「动钱」：
	//   - payment:recharge 的 settle 只把一张沙箱充值单推到 SUCCESS 并入账，
	//     驱动方是本服务状态机，钱不来自任何真实渠道（channel 恒 SANDBOX）；
	//   - payment:balance 的 adjust 是**全后台唯一直接改写资金台账的入口**（增减余额、
	//     只用于差错更正），它不经过任何订单、不产生充值单，写错没有任何上游能纠正，
	//     因此绝不允许跟「结算充值单」共用一个权限点：能补账的人不该默认能把任意一张
	//     充值单判成已到账。配套约束在契约里：idempotency_key + operator + reason 三者必填，
	//     服务侧另记一笔审计（见 admin.api 的 /balance/adjust 注释）。
	// CreatePayment/ClosePayment/RefundPayment 没有后台路由：订单支付与退款的唯一驱动方是
	// trade-order 的订单状态机，后台另开一个口就会出现第二个写主，两处并发时钱会记重。
	// OpenRecharge/CancelRecharge 是用户自己发起的充值意向，属 gateway/app 面。
	// 权限码同样需要 op_permission seed 迁移补齐（见 README）。
	"/admin/payment/recharge/settle": {Resource: "payment:recharge", Action: "settle"},
	"/admin/payment/balance/adjust":  {Resource: "payment:balance", Action: "adjust"},

	// order 域只开放退款的「审批」两端，approve 与 reject 分两点，理由是失败代价不对称：
	//   - order:refund 的 approve 会真的把余额退回、把权益回收（部分成功原样投影在
	//     revoke_detail，不回滚），一旦执行就没有后台撤销口，只能再走一次 balance/adjust；
	//   - order:refund 的 reject 不动钱不动权益，只把订单推回原状态并留一行理由，
	//     是日常客服动作。两者共用一个点会让「能驳回」顺带等于「能退款」。
	// RequestRefund 与 CreateOrder/CancelOrder/ListMyOrders 都归买方（gateway/app）：
	// 后台代客下单/代客申请退款等于伪造用户意图，退款必须来自用户的一次申请单。
	// BindPayment 属支付回调链、FulfillOrder 由服务/cron 驱动，都不开后台口。
	// 权限码同样需要 op_permission seed 迁移补齐（见 README）。
	"/admin/order/refund/approve": {Resource: "order:refund", Action: "approve"},
	"/admin/order/refund/reject":  {Resource: "order:refund", Action: "reject"},

	// coin 域只有一条写入口，且刻意不与任何资金入口同构：
	//   - coin:grant 的 create 发放/扣回硬币（amount 正负皆可），只接受 ORDER_PACK 与
	//     ADMIN_GRANT 两种来源，**不产生资金流水**（硬币是站内积分，不是钱，AGENTS.md §1）。
	// 之所以只有一档：扣回是发放的负数形态、走同一方法同一状态机，拆两点没有可执行边界；
	// 真正需要区分的是「有没有动钱」，那已经由 payment:balance 承担。
	// TossCoin/CancelToss/ListMyTosses 是终端用户动作；GetTargetSummary 等三个聚合面
	// 属内容侧读面（gateway/app + 审核），都不在后台开放。
	// 权限码同样需要 op_permission seed 迁移补齐（见 README）。
	"/admin/coin/grant": {Resource: "coin:grant", Action: "create"},

	// creator-revenue 域是「算钱给谁」这一侧，四类动作按「是否产生应付金额」分点：
	//   - revenue:rule 的 update 只写 DRAFT 草稿（无 state 位），publish 才把规则推进 ACTIVE；
	//     ACTIVE 规则不可原地改价，改价只能新草稿 + 再 publish，两点分离正是为了让
	//     「拟稿的人」和「让新价格对所有创作生效的人」可以不是同一个人；
	//   - revenue:enrollment 的 update 只暂停/恢复某个创作者的参与（暂停期间不结算），
	//     改的是一个人的计入口径，与改全站规则差一个量级，不并入 rule；
	//     EnrollCreator/LeavePlan 归创作者本人（gateway/app），后台不代签参与意愿；
	//   - revenue:settlement 的 create 与 confirm 分开：generate 按周期把计量折算成结算单
	//     （可重算，force_void_confirmed 位会把旧单作废，故必须带 reason），
	//     confirm 是金额冻结后的「认账」——两者都**不是钱已付出**（payout_state 恒
	//     NOT_PAYABLE，本期无出金通道，打款/提现/退款到卡一律不在范围内），
	//     但拆开后「能重算台账」不等于「能盖章认账」，认账才是给作者的承诺。
	// RecordRevenueMetric 没有后台路由：计量只能由 spm/coin 写入，后台写一次就污染了计量的来源。
	// 权限码同样需要 op_permission seed 迁移补齐（见 README）。
	"/admin/creator-revenue/rule/upsert":         {Resource: "revenue:rule", Action: "update"},
	"/admin/creator-revenue/rule/state":          {Resource: "revenue:rule", Action: "publish"},
	"/admin/creator-revenue/enrollment/state":    {Resource: "revenue:enrollment", Action: "update"},
	"/admin/creator-revenue/settlement/generate": {Resource: "revenue:settlement", Action: "create"},
	"/admin/creator-revenue/settlement/confirm":  {Resource: "revenue:settlement", Action: "confirm"},

	// spm 域：口径注册表与聚合作业的写入口，外加一条定向的个人画像读。
	//
	// 十处只读（/metric/get /metric/batch-get /hot-subject/list /retention/get
	// /definition/get /definition/list /job/get /job/list /consumer-state/list
	// /dead-letter/list）不在本表里，与 audit/ops-config/cron/live/recommend/collector/pm
	// 及商业化五域的读面同一口径：排障页每次刷新都会打一次 RPC，全量挂判定会把 operation
	// 变成读放大瓶颈。
	//
	// 唯一的例外是 spm:interest 的 read —— 它不是「看个榜」，是**定向读某一个 mid 的兴趣画像**，
	// 有明确的被读主体，属个人数据访问：授权要能单独收回（数据分析岗能看聚合热度，
	// 不必然是能逐个翻看用户画像的人），访问要进判定链路留痕。AGENTS.md §7 要求行为数据
	// 脱敏，画像内容本身已只回受控词表，但「谁读了谁的画像」这条事实不能没有门禁。
	//
	// 四个写入口按后果分离：
	//   - spm:definition 的 create 只新增一个 DRAFT/待审版本，永远改不动已登记版本
	//     （命中同 (metric_key, metric_version) 且规格不同回 ErrMetricVersionImmutable），
	//     所以它能与 state 拆得开：拟口径的人不等于让口径生效的人；
	//   - spm:definition 的 state 才是上下架（DRAFT→ACTIVE→RETIRED）——激活一个版本会顶掉
	//     另一个 ACTIVE，直接改变后续所有窗口的解释口径，影响面是全站新算的数据；
	//   - spm:job 的 create 排队一个聚合作业（消耗算力、覆盖窗口值，但不改口径定义），
	//     spm:metric 的 recompute 是「从事实表按指定版本重算历史窗口」——这是唯一正当的
	//     「让已发布的数变掉」路径，它改写的是历史窗口的结论，与向前生效的上下架后果相反，
	//     因此绝不与 job 共用：能补跑一段作业的人不该默认能把历史数抹平重算。
	//     契约里它要求显式 metric_version（0 不行），网关也照此挡正数。
	// WriteMetricWindow 没有后台路由：它是计算链路的专属写回通道（proto 头注释即「只接受
	// 计算链路来源」），后台开一个口等于允许手工改指标，违反 AGENTS.md §7 第 3 条。
	// 权限码需要 op_permission seed 迁移补齐（见 README），未 seed 时五个入口一律 403。
	"/admin/spm/interest/get":      {Resource: "spm:interest", Action: "read"},
	"/admin/spm/definition/upsert": {Resource: "spm:definition", Action: "create"},
	"/admin/spm/definition/state":  {Resource: "spm:definition", Action: "state"},
	"/admin/spm/job/submit":        {Resource: "spm:job", Action: "create"},
	"/admin/spm/metric/recompute":  {Resource: "spm:metric", Action: "recompute"},

	// feature-store 域：特征定义/版本/回填的写入口，外加一条定向的个人特征导出读。
	//
	// 五处只读（/definition/get /definition/list /version-switch/list /backfill/get
	// /backfill/list）不在本表里，与 spm 及前面各域的读面同一口径：排障页
	// 每次刷新都会打一次 RPC，全量挂判定会把 operation 变成读放大瓶颈。
	//
	// 唯一例外是 feature:entity-value 的 read —— 它不是「看个目录」，是**定向读某一个主体的
	// 全部特征值**（含 mid 维度的个体画像与设备哈希维度），有明确的被读主体：授权要能单独
	// 收回（能看特征目录的人不必然是能逐个翻看个人值的人），访问要进判定链路留痕。
	// 服务侧还有第二道闸（Privacy.ExportMaxPrivacyLevel 收敛可见级别），但那只管「能给多少」，
	// 不管「谁可以来问」。
	//
	// 七个写入口按后果分离：
	//   - feature:definition 拆成 create/state/privacy 三点，因为三者后果方向不同：
	//     create 只新增一个 DRAFT 版本（服务强制 DRAFT，注册即生效等于绕过评审），
	//     state 推进 DRAFT→ACTIVE→RETIRED（RETIRED 让在线读侧立刻改走默认值），
	//     privacy 只改「谁能读」不改值语义——所以它单独一点，允许被授予「可调隐私级别」
	//     而不可上下架的隐私岗，也避免一次操作顺手把两件事都办了；
	//   - feature:active-version 的 switch 移动对外生效的版本指针，切换后所有在线读换一版，
	//     影响面与单版本的状态变更不同，绝不与 state 共用；
	//   - feature:backfill-job 的 create 排队一次历史值补写（覆盖既有值，可 auto_switch），
	//     属于「写数据」而不是「改定义」；
	//   - feature:value 的 purge 与 feature:entity-value 的 erase 都是删除且不可逆：
	//     purge 按 TTL 批量清（常态是 cron 调，本路由只用于补跑），erase 按主体定向抹，
	//     后者另受服务侧 Privacy.OperatorPrefixes 白名单约束（空白名单 = 谁都拒），
	//     网关不代替它放行。两点分开是因为「清过期」与「响应某人的删除工单」不是同一职责。
	// WriteFeatures/GetFeature/BatchGetFeatures 没有后台路由（前者是计算链路专属写回通道，
	// 后两者是在线热路径），理由见 admin.api 的 feature-store 段头注释。
	// 权限码需要 op_permission seed 迁移补齐（见 README），未 seed 时八个入口一律 403。
	"/admin/feature-store/entity-feature/list":  {Resource: "feature:entity-value", Action: "read"},
	"/admin/feature-store/definition/register":  {Resource: "feature:definition", Action: "create"},
	"/admin/feature-store/definition/state":     {Resource: "feature:definition", Action: "state"},
	"/admin/feature-store/definition/privacy":   {Resource: "feature:definition", Action: "privacy"},
	"/admin/feature-store/version/switch":       {Resource: "feature:active-version", Action: "switch"},
	"/admin/feature-store/backfill/submit":      {Resource: "feature:backfill-job", Action: "create"},
	"/admin/feature-store/entity-feature/erase": {Resource: "feature:entity-value", Action: "erase"},
	"/admin/feature-store/retention/purge":      {Resource: "feature:value", Action: "purge"},

	// open-platform 域：第三方应用、scope 审批、授权撤销、配额与回调投递。
	//
	// 本域的划线与 spm/feature-store 不同：**六条读里五条挂判定，只有 /scope/list 免**。
	// 理由不是重要性而是形状——除 ListScopesReq 之外，本域每个读请求都自带运营主体位
	// （operator_mid / caller_mid），服务用「mid 是否为 0」区分 owner 自查与运营读
	// （listwebhookslogic / listquotausagelogic 的身份口径注释：mid==0 时归属由网关校验）。
	// 后台既不是 owner 也没做过那道归属校验，只能填 mid>0；而 mid 是表单自报的，
	// 网关必须先用会话确认「这确实是后台在调」，否则那个 mid 就是请求体自封的身份。
	// 第二条独立理由：应用配置（redirect_uri 白名单、已批 scope、密钥状态）与回调地址
	// 本身属**外部主体的凭证面**，白名单可用于构造授权钓鱼。
	// /scope/list 两条都不沾：契约里没有任何操作者位，目录本身是授权页的对外公示文案。
	//
	// 九条写按后果分离，一条一点：
	//   - openplatform:application 的 read 与 state 分开——state 决定「这个第三方能不能调用平台」，
	//     读详情不改变任何外部行为；资料改动（含回调白名单）在本域刻意没有后台入口；
	//   - openplatform:secret 的 rotate/revoke 是两个方向：轮换是例行换钥（旧钥有宽限期），
	//     吊销是泄露应急（立即失效）。合并成一个点就没法授予「只做应急下线」的岗位；
	//   - openplatform:scope 的 grant 决定应用能读到什么，回收立即生效并连带影响 token 校验；
	//   - openplatform:authorization 的 revoke 打死用户凭证，USER_ALL 一次覆盖该用户的全部第三方
	//     授权，blast radius 比应用级处置更大，单独一点；
	//   - openplatform:quota-policy 的 read/upsert 与 openplatform:quota 的 read/recompute 分开：
	//     规则决定限流上限（判定链上游），用量与投影只是读与收敛，四者互不隐含；
	//   - openplatform:webhook 的 read/delete：端点地址决定事件数据去向，删除同时抑制未投递任务；
	//     注册端点没有后台路由（段头口径 1），所以这里也没有「改地址」的口子；
	//   - openplatform:delivery 的 read/retry：重放只重置既有记录的 attempt，
	//     与刻意不开服的 EnqueueWebhookEvent（注入新事实）分得开。
	// RegisterApplication/RegisterWebhook/EnqueueWebhookEvent 与 OAuth 五法
	// （IssueAuthorizationCode/ExchangeAuthorizationCode/RefreshAccessToken/IntrospectToken/
	// AuthorizeRequest）在本网关没有任何调用点，因此也不会出现在表里（口径见 admin.api 文末说明）。
	// 权限码需要 op_permission seed 迁移补齐（见 README），未 seed 时十四个入口一律 403。
	"/admin/open-platform/application/get":        {Resource: "openplatform:application", Action: "read"},
	"/admin/open-platform/application/list":       {Resource: "openplatform:application", Action: "read"},
	"/admin/open-platform/application/state":      {Resource: "openplatform:application", Action: "state"},
	"/admin/open-platform/secret/rotate":          {Resource: "openplatform:secret", Action: "rotate"},
	"/admin/open-platform/secret/revoke":          {Resource: "openplatform:secret", Action: "revoke"},
	"/admin/open-platform/scope/grant":            {Resource: "openplatform:scope", Action: "grant"},
	"/admin/open-platform/authorization/revoke":   {Resource: "openplatform:authorization", Action: "revoke"},
	"/admin/open-platform/quota/policy/list":      {Resource: "openplatform:quota-policy", Action: "read"},
	"/admin/open-platform/quota/policy/upsert":    {Resource: "openplatform:quota-policy", Action: "upsert"},
	"/admin/open-platform/quota/usage/list":       {Resource: "openplatform:quota", Action: "read"},
	"/admin/open-platform/quota/recompute":        {Resource: "openplatform:quota", Action: "recompute"},
	"/admin/open-platform/webhook/list":           {Resource: "openplatform:webhook", Action: "read"},
	"/admin/open-platform/webhook/delete":         {Resource: "openplatform:webhook", Action: "delete"},
	"/admin/open-platform/webhook/delivery/list":  {Resource: "openplatform:delivery", Action: "read"},
	"/admin/open-platform/webhook/delivery/retry": {Resource: "openplatform:delivery", Action: "retry"},

	// ==== 阶段 1-2 十三个域的写入口 ====
	// 这十三组是本仓最早落地的运营路由，当时网关还没有全局鉴权（gateway/admin 不挂 JWT，
	// AdminPermission 是唯一认证+授权点），整组都处于免鉴权状态：稿件状态机推进、集上架、
	// 版权窗口过期、处罚下发、全站站内信群发、索引别名切换、节操值/经验值变更都能匿名调用。
	// 本轮按既有口径补齐：**写面全部挂判定，读面默认免鉴权**（后台列表页每次刷新都会打一次
	// RPC，全量挂判定会把 operation 变成读放大瓶颈），例外只允许「有被读主体的定向个人数据读」，
	// 与 spm:interest / feature:entity-value 同一划线，见 route_permission_drift_test.go 的白名单。
	//
	// account：cache/del 与 cache/clear 是同一个能力的两个入参形状（后者收 JSON 消息），
	// 因此共用一个权限点；两者都会让指定用户的会话/资料缓存失效，不属于读面。
	// 权限码需要 op_permission seed 迁移补齐（见 README），未 seed 时入口一律 403。
	"/admin/account/cache/del":   {Resource: "account:cache", Action: "invalidate"},
	"/admin/account/cache/clear": {Resource: "account:cache", Action: "invalidate"},

	// member（user-profile，前缀沿用历史的 /x/member）：
	//   - 节操值的 update 与 undo 分开——撤销一次变更是独立的纠偏动作，不隐含「能再改一次」；
	//   - 经验值的 set 与 update 分开：set 直接覆盖绝对值（能清零），update 只增量；
	//   - 属性审核只是提交一条待审记录，生效由审核侧决定，因此不与 moral/exp 混用；
	//   - realname 两条按能力方向分列：read 是「已知 mid 看脱敏结果」，reverse-lookup 是
	//     「已知证件号反查 mid」，后者可被用来把人对着证件号清单批量配对，属另一种泄露面；
	//   - login-log 是某个用户的登录行为明细，与上面两条同属被读主体明确的个人数据读。
	"/x/member/morals/update":          {Resource: "member:moral", Action: "update"},
	"/x/member/moral/update":           {Resource: "member:moral", Action: "update"},
	"/x/member/moral/undo":             {Resource: "member:moral", Action: "undo"},
	"/x/member/exp/set":                {Resource: "member:exp", Action: "set"},
	"/x/member/exp/update":             {Resource: "member:exp", Action: "update"},
	"/x/member/property/review/add":    {Resource: "member:property-review", Action: "create"},
	"/x/member/realname/stripped/info": {Resource: "member:realname", Action: "read"},
	"/x/member/realname/mid/by/card":   {Resource: "member:realname", Action: "reverse-lookup"},
	"/x/member/web/login/log":          {Resource: "member:login-log", Action: "read"},

	// video：只有状态机推进这一条写入口，且服务侧已禁止直接置为 PUBLISHED（AGENTS.md §8）。
	// 稿件列表与详情属内容运营常规读面，不进本表。
	"/admin/video/submissions/:aid/transition": {Resource: "video:submission", Action: "transition"},

	// catalog：作品/季/集的创建都是「往版权目录里加一行」，各自独立点；
	// 集的 publish 与 offline 与创建分开——上架要让 rights 校验版权窗口并通过审核，
	// 是内容对公众可见性的开关，能建目录条目不等于能上下架。
	"/admin/catalog/works":                  {Resource: "catalog:work", Action: "create"},
	"/admin/catalog/seasons":                {Resource: "catalog:season", Action: "create"},
	"/admin/catalog/episodes":               {Resource: "catalog:episode", Action: "create"},
	"/admin/catalog/episodes/:epid/publish": {Resource: "catalog:episode", Action: "publish"},
	"/admin/catalog/episodes/:epid/offline": {Resource: "catalog:episode", Action: "offline"},

	// rights：合同与窗口都是「给版权内容授予可播范围」的写入口；
	// 手动过期窗口是提前撤回公众可见性（应急下线），与新建一条窗口的方向相反，单列一点。
	"/admin/rights/contracts":                 {Resource: "rights:contract", Action: "create"},
	"/admin/rights/windows":                   {Resource: "rights:window", Action: "create"},
	"/admin/rights/windows/:window_id/expire": {Resource: "rights:window", Action: "expire"},

	// moderation：申诉处置直接改写一条审核结论的终态，比查任务/结论重，故只登记这一条。
	"/admin/moderation/appeals": {Resource: "moderation:appeal", Action: "handle"},

	// transcode：模板是所有后续转码任务的参数来源，改一行会影响之后每次转码，
	// 而任务与模板的读取都是排障读面。
	"/admin/transcode/templates": {Resource: "transcode:template", Action: "create"},

	// danmaku：屏蔽词是「事前拦所有内容」的规则写入口，删除单条弹幕是「事后处置一条内容」，
	// 两者作用面不同，不复用同一个点。
	"/admin/danmaku/block_word": {Resource: "danmaku:block-word", Action: "update"},
	"/admin/danmaku/delete":     {Resource: "danmaku:item", Action: "delete"},

	// search：重建任务会重灌整份投影，别名切换是零停机重建的生效开关（切错即全量查询换索引），
	// 两者后果不同且都可独立发生，各占一点；任务进度、列表与健康度是读面。
	"/admin/search/rebuild":      {Resource: "search:index", Action: "rebuild"},
	"/admin/search/alias/switch": {Resource: "search:alias", Action: "switch"},

	// risk：
	//   - punishment 的 create 与 lift 分开，与 live:ban 同一理由：能解封的人不该默认能封人；
	//   - rule 与 list-entry 是判定链的上游（改一条影响所有后续裁决），设备画像是单个主体的数据；
	//   - check 虽然不落业务数据，但它按任意主体/动作跑一次真实裁决，是规则的在线探测口；
	//   - report 会写滑窗计数，伪造上报能直接把正常用户喂进阈值处罚，故与 check 同挂判定。
	"/admin/risk/check":           {Resource: "risk:check", Action: "run"},
	"/admin/risk/report":          {Resource: "risk:report", Action: "create"},
	"/admin/risk/device":          {Resource: "risk:device", Action: "update"},
	"/admin/risk/punishment":      {Resource: "risk:punishment", Action: "create"},
	"/admin/risk/punishment/lift": {Resource: "risk:punishment", Action: "lift"},
	"/admin/risk/rule":            {Resource: "risk:rule", Action: "update"},
	"/admin/risk/list_entry":      {Resource: "risk:list-entry", Action: "update"},

	// comment：删除是让内容对所有读者消失并写审计，置顶改变的是评论区的呈现顺序，方向不同。
	"/admin/comment/delete": {Resource: "comment:item", Action: "delete"},
	"/admin/comment/pin":    {Resource: "comment:item", Action: "pin"},

	// notification：
	//   - upsert 只存草稿（publish=false 时对投递无影响），publish 才让一个版本对外生效，
	//     两点分开后「能改文案」不等于「能发给用户」；
	//   - render 不落库，但能渲染未发布草稿并看到变量填充结果，属模板内容泄露面，单列一点；
	//   - deadletter/retry 会把已判定失败的消息重新推给供应商通道（真实触达用户），独立点。
	"/admin/notification/template/upsert":  {Resource: "notify:template", Action: "update"},
	"/admin/notification/template/publish": {Resource: "notify:template", Action: "publish"},
	"/admin/notification/template/render":  {Resource: "notify:template", Action: "render"},
	"/admin/notification/deadletter/retry": {Resource: "notify:deadletter", Action: "retry"},

	// inbox：
	//   - message/send 是「向一批用户投递站内信」的唯一后台入口（MaxRecipients 上限由服务侧判定），
	//     全仓最重的用户触达能力，必须能单独授予与单独收回；
	//   - unread/recompute 用 GET 形状做写操作（重算快照并回填缓存），它是计数纠偏工具，
	//     与投递消息不是一回事，因此不共用 inbox:message；GET 挂判定是有意的——
	//     留在免鉴权组里就等于「任何人都能触发批量重算」，读写形状不该决定鉴权形状。
	"/admin/inbox/message/send":     {Resource: "inbox:message", Action: "send"},
	"/admin/inbox/unread/recompute": {Resource: "inbox:unread", Action: "recompute"},
}

// paramPrefix 是 go-zero 路由里路径参数的书写形状（/submissions/:aid/transition）。
const paramPrefix = ":"

// routePattern 是一条含路径参数的权限表项，按段匹配真实请求路径。
type routePattern struct {
	segs []string
	perm adminPermission
}

var (
	routePatternsOnce sync.Once
	routePatterns     []routePattern
)

// buildRoutePatterns 从权限表里挑出含 :param 的键并预切段。表是包级只读的，
// 因此用 sync.Once 建一次；建不出任何模板也不报错（当前可能确实没有参数化路由）。
func buildRoutePatterns() {
	pats := make([]routePattern, 0, 4)
	for path, perm := range routePermissions {
		if !strings.Contains(path, paramPrefix) {
			continue
		}
		pats = append(pats, routePattern{segs: strings.Split(strings.Trim(path, "/"), "/"), perm: perm})
	}
	routePatterns = pats
}

// matchRoutePattern 按段匹配路径模板：段数必须相等，非 :param 段必须逐字相等。
// 不做前缀匹配，所以 /admin/catalog/episodes/1/2/publish 不会命中 /episodes/:epid/publish。
func matchRoutePattern(path string) (adminPermission, bool) {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	for _, p := range routePatterns {
		if len(p.segs) != len(segs) {
			continue
		}
		match := true
		for i, want := range p.segs {
			if strings.HasPrefix(want, paramPrefix) {
				if segs[i] == "" {
					match = false
					break
				}
				continue
			}
			if want != segs[i] {
				match = false
				break
			}
		}
		if match {
			return p.perm, true
		}
	}
	return adminPermission{}, false
}

// permissionFor 查表返回路径所需权限点。
//
// 先精确查表（绝大多数路由是静态路径，键就是注册后的完整路径）；查不中再按 :param 模板匹配，
// 因为带路径参数的路由在 r.URL.Path 里已经是真实值（/admin/video/submissions/123/transition），
// 精确查表永远命中不了。两条路都查不中即视为表外路由，由调用方 fail-closed 拒绝。
func permissionFor(path string) (adminPermission, bool) {
	if p, ok := routePermissions[path]; ok {
		return p, true
	}
	routePatternsOnce.Do(buildRoutePatterns)
	return matchRoutePattern(path)
}

// ExtractAdminToken 从 Authorization 头取出后台 token。
// 接受 `Bearer adm_xxx`（scheme 大小写不敏感）与裸 token 两种写法：
// 后台前端与内网脚本都可能用其中一种，operation 签发的 token 自带 adm_ 前缀，
// 网关不判断前缀也不校验摘要——那是 services/operation 的职责。
// 其它 scheme（Basic/Digest…）一律视为无凭证，避免把别的认证材料喂给后台会话。
func ExtractAdminToken(r *http.Request) string {
	// 先 TrimSpace 再去切分 scheme，这样 "  Bearer   adm_x  " 与 "Bearer "（无凭证）
	// 都能落到同一套判定上：后者只剩 scheme，按无凭证处理。
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	if i := strings.IndexAny(h, " \t"); i >= 0 {
		scheme, cred := h[:i], strings.TrimSpace(h[i+1:])
		if !strings.EqualFold(scheme, "bearer") || cred == "" {
			return ""
		}
		return cred
	}
	// 只有一个词：要么是裸 token，要么只剩 scheme 没带凭证。
	if strings.EqualFold(h, "bearer") {
		return ""
	}
	return h
}

// adminKey 是写入 request context 的私有键类型，避免与其它包的键碰撞。
type adminKey struct{}

// AdminIdentity 是中间件判定通过后解析出的操作者身份，logic 用它作为审计主体。
type AdminIdentity struct {
	AdminID int64
	Roles   []string
}

// WithAdmin 把判定结果挂到请求 context 上，供 internal/logic 读取审计主体。
func WithAdmin(ctx context.Context, id AdminIdentity) context.Context {
	return context.WithValue(ctx, adminKey{}, id)
}

// AdminFromContext 读取中间件解析出的管理员身份（未鉴权路由返回 ok=false）。
func AdminFromContext(ctx context.Context) (AdminIdentity, bool) {
	id, ok := ctx.Value(adminKey{}).(AdminIdentity)
	if !ok || id.AdminID <= 0 {
		return AdminIdentity{}, false
	}
	return id, true
}

// AdminPermissionMiddleware 在每个 /admin/operation 受保护路由上调用
// operation.VerifyAdminPermission，按路由静态权限表判定 resource+action。
// 拒绝时写统一响应信封：无 token/会话非法 401、权限不足 403、
// 未配置 operation 或判定异常 503、表外路由 403。
type AdminPermissionMiddleware struct {
}

// NewAdminPermissionMiddleware 构造中间件（骨架签名由 goctl 生成，保持不变；
// operation 客户端通过 SetOperationClient 注入，与 gateway/app 的机制一致）。
func NewAdminPermissionMiddleware() *AdminPermissionMiddleware {
	return &AdminPermissionMiddleware{}
}

// Handle 是 go-zero rest.Middleware 的适配函数。
func (m *AdminPermissionMiddleware) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		logger := logx.WithContext(ctx)

		reqPerm, ok := permissionFor(r.URL.Path)
		if !ok {
			// 中间件组里出现了未登记的路由：宁可拒绝也不可放行。
			logger.Errorf("gateway/admin/AdminPermission: unregistered path=%s", r.URL.Path)
			deny(w, http.StatusForbidden, httpresponse.CodeForbidden, "permission not defined for this route")
			return
		}

		token := ExtractAdminToken(r)
		if token == "" {
			deny(w, http.StatusUnauthorized, httpresponse.CodeUnauthorized, "admin token required")
			return
		}

		cli := operationClient()
		if cli == nil {
			// 未配置 OperationRPC：受保护路由不可用，绝不退化成免鉴权。
			logger.Error("gateway/admin/AdminPermission: operation service not configured")
			deny(w, http.StatusServiceUnavailable, httpresponse.CodeInternalError, "operation service not configured")
			return
		}

		// 日志只打身份与权限点，永不打印 token（AGENTS.md §4：密钥不进日志）。
		logger.Infof("gateway/admin/AdminPermission: resource=%s action=%s", reqPerm.Resource, reqPerm.Action)
		reply, err := cli.VerifyAdminPermission(ctx, &operationrpc.VerifyAdminPermissionReq{
			Token:    token,
			Resource: reqPerm.Resource,
			Action:   reqPerm.Action,
			TraceId:  strings.TrimSpace(r.Header.Get(traceIDHeader)),
		})
		if err != nil {
			logger.Errorf("gateway/admin/AdminPermission: verify resource=%s action=%s err=%v",
				reqPerm.Resource, reqPerm.Action, err)
			deny(w, http.StatusServiceUnavailable, httpresponse.CodeInternalError, "permission check unavailable")
			return
		}
		if reply == nil || !reply.GetAllowed() {
			reason := reply.GetReason()
			if reason == "" {
				reason = "permission_denied"
			}
			status, code := http.StatusForbidden, httpresponse.CodeForbidden
			if reason == reasonSessionInvalid {
				status, code = http.StatusUnauthorized, httpresponse.CodeUnauthorized
			}
			logger.Infof("gateway/admin/AdminPermission: denied resource=%s action=%s admin=%d reason=%s",
				reqPerm.Resource, reqPerm.Action, reply.GetAdminId(), reason)
			deny(w, status, code, reason)
			return
		}
		if reply.GetAdminId() <= 0 {
			// allowed=true 却给不出主体：放行会让审计失去 owner，一律按故障拒绝。
			logger.Errorf("gateway/admin/AdminPermission: allowed without admin_id resource=%s action=%s",
				reqPerm.Resource, reqPerm.Action)
			deny(w, http.StatusServiceUnavailable, httpresponse.CodeInternalError, "permission check returned no subject")
			return
		}

		next(w, r.WithContext(WithAdmin(r.Context(), AdminIdentity{
			AdminID: reply.GetAdminId(),
			Roles:   reply.GetMatchedRoles(),
		})))
	}
}

// deny 用 common/httpresponse 的统一信封写出拒绝结果：HTTP 状态码表达鉴权语义，
// body 仍是 code/message/data/ttl 四字段（AGENTS.md §6）。
func deny(w http.ResponseWriter, status, code int, message string) {
	httpx.WriteJson(w, status, httpresponse.Envelope{
		Code:    code,
		Message: message,
		Data:    map[string]any{},
		TTL:     0,
	})
}
