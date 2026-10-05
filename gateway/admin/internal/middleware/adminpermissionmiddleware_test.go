package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-video/common/httpresponse"
	operationrpc "go-video/services/operation/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧的鉴权口径：token 怎么取、路由需要什么权限、判定结果如何落到
// HTTP 状态码与统一信封。打桩方式为内嵌生成的 client 接口 + 覆盖所需方法，
// 不建 gRPC 连接、不碰数据库（AGENTS.md §9）。

// fakeOperation 只实现 VerifyAdminPermission，其余方法继承接口（未调用即 panic，
// 一旦有用例越界会立刻暴露）。
type fakeOperation struct {
	operationrpc.OperationClient
	reply *operationrpc.VerifyAdminPermissionReply
	err   error
	reqs  []*operationrpc.VerifyAdminPermissionReq
}

func (f *fakeOperation) VerifyAdminPermission(_ context.Context, in *operationrpc.VerifyAdminPermissionReq,
	_ ...grpc.CallOption) (*operationrpc.VerifyAdminPermissionReply, error) {
	f.reqs = append(f.reqs, in)
	return f.reply, f.err
}

// allowedNext 是被中间件放行时执行的处理函数，顺带断言 context 里的身份。
func allowedCheck(t *testing.T, wantAdmin int64) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := AdminFromContext(r.Context())
		if !ok {
			t.Fatalf("中间件放行但 context 里没有管理员身份")
		}
		if id.AdminID != wantAdmin {
			t.Fatalf("context admin_id = %d, want %d", id.AdminID, wantAdmin)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func doHandle(t *testing.T, mw *AdminPermissionMiddleware, path, authorization, traceID string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	if traceID != "" {
		r.Header.Set(traceIDHeader, traceID)
	}
	w := httptest.NewRecorder()
	mw.Handle(allowedCheck(t, 77))(w, r)
	return w
}

func decodeEnvelope(t *testing.T, w *httptest.ResponseRecorder) httpresponse.Envelope {
	t.Helper()
	var env httpresponse.Envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是统一信封 JSON: %v body=%s", err, w.Body.String())
	}
	if env.Data == nil {
		t.Fatalf("拒绝响应也必须带 data 字段（四字段信封），body=%s", w.Body.String())
	}
	return env
}

func TestExtractAdminToken(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"缺失请求头", "", ""},
		{"Bearer 前缀", "Bearer adm_op_1a2b_3c4d", "adm_op_1a2b_3c4d"},
		{"scheme 小写", "bearer adm_op_1a2b_3c4d", "adm_op_1a2b_3c4d"},
		{"scheme 大写", "BEARER adm_op_1a2b_3c4d", "adm_op_1a2b_3c4d"},
		{"多个空格", "Bearer    adm_op_1a2b", "adm_op_1a2b"},
		{"裸 token", "adm_op_1a2b", "adm_op_1a2b"},
		{"首尾空白", "  adm_op_1a2b  ", "adm_op_1a2b"},
		{"scheme 与凭证都有空白", "  Bearer   adm_op_1a2b  ", "adm_op_1a2b"},
		{"Bearer 后为空", "Bearer ", ""},
		{"只有 scheme", "Bearer", ""},
		{"Basic 认证不接受", "Basic YWJjOmRlZg==", ""},
		{"其它 scheme 不接受", "Digest adm_op_1a2b", ""},
		{"只有空白", "   ", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/admin/operation/user/list", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			if got := ExtractAdminToken(r); got != c.want {
				t.Fatalf("ExtractAdminToken() = %q, want %q", got, c.want)
			}
		})
	}
}

// 路由表口径：受保护路由必须全部登记，公开路由（登录、权限判定）必须不在表内。
func TestRoutePermissionTable(t *testing.T) {
	// operation 22 个方法减去 login/verify 两个公开入口 = 20，再加 audit 的 4 个写入口、
	// ops-config 的 10 个写入口、cron 的 8 个写入口、live-room 的 5 个写入口、
	// live-ingest 的 4 个写入口、live-gateway 的 4 个写入口、recommend 的 9 个写入口、
	// live-media 的 12 个写入口、event-collector 的 4 个写入口、私信运营面的 2 个写入口，
	// 商业化五域的 15 个写入口（membership 5 + payment 2 + order 2 + coin 1 + revenue 5），
	// spm 的 4 个写入口 + 1 个定向个人画像读（interest/get），
	// 以及 feature-store 的 7 个写入口 + 1 个定向个人特征导出读（entity-feature/list），
	// 以及 open-platform 的 9 个写入口 + 6 条台账读（应用详情与检索、配额规则、配额用量、
	// 回调端点、投递台账）。
	// 五域的 24 条只读、spm 的 10 条只读、feature-store 的 5 条只读与 open-platform 的
	// 1 条 scope 目录读一律不登记，与其它域的读面同口径（见权限表上方的分组注释）；
	// 除 operation 域早期已登记的读取入口外，只有按主体的定向个人读（interest/get、
	// entity-feature/list、member 的实名正查/证件号反查/登录日志）与 open-platform 的台账读挂判定，
	// 理由写在表内各段注释里；受保护 GET 的完整合法集合由
	// route_permission_drift_test.go 的 protectedGetAllowlist 逐条点名。
	// 阶段 1-2 十三个域补齐后：account 1 点/2 条、member 8 点/9 条、video 1、catalog 5、
	// rights 3、moderation 1、transcode 1、danmaku 2、search 2、risk 7、comment 2、
	// notify 4、inbox 2 条（共 41 条路由项 / 39 个权限点）。
	// 条目数 166 比权限点数 162 多 4：operation 的 task read 覆盖 get 与 list、
	// open-platform 的 application read 覆盖 get 与 list、account 的 cache invalidate 覆盖
	// del 与 clear、member 的 moral update 覆盖 morals/update 与 moral/update
	//（seed 迁移按点登记，不按路由登记，见 seed_permission_test.go 的双向相等断言）。
	// 这张表的完整性由 TestEveryProtectedRouteIsRegistered 对照 routes.go 兜底，
	// 这里的条目数只是防止「改了表忘了改期望值」的第二道闸。
	if len(routePermissions) != 166 {
		t.Fatalf("权限表条目 = %d, want 166", len(routePermissions))
	}
	publicPrefixes := []string{
		"/admin/operation/", "/admin/audit/", "/admin/ops/", "/admin/cron/", "/admin/live/",
		"/admin/recommend/", "/admin/collector/", "/admin/private-message/", "/admin/spm/",
		"/admin/feature-store/", "/admin/open-platform/", "/admin/membership/", "/admin/payment/",
		"/admin/order/", "/admin/coin/", "/admin/creator-revenue/",
		// 阶段 1-2 补齐的十三个域；/x/member 是 user-profile 沿用的历史前缀。
		"/admin/account/", "/x/member/", "/admin/video/", "/admin/catalog/", "/admin/rights/",
		"/admin/moderation/", "/admin/transcode/", "/admin/danmaku/", "/admin/search/", "/admin/risk/",
		"/admin/comment/", "/admin/notification/", "/admin/inbox/",
	}
	for path, p := range routePermissions {
		full := false
		for _, prefix := range publicPrefixes {
			if strings.HasPrefix(path, prefix) {
				full = true
				break
			}
		}
		if !full {
			t.Fatalf("权限表键必须是完整路由: %s", path)
		}
		if p.Resource == "" || p.Action == "" {
			t.Fatalf("%s 缺少 resource/action，会被中间件永久拒绝", path)
		}
		if !strings.Contains(p.Resource, ":") {
			t.Fatalf("%s 的 resource=%q 不符合 operation.proto 的 <域>:<实体> 口径", path, p.Resource)
		}
	}
	for _, public := range []string{"/admin/operation/login", "/admin/operation/permission/verify"} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("公开路由 %s 不应出现在权限表里", public)
		}
	}
	// 表外路径必须查不到，否则就是「新路由默认免鉴权」。
	if _, ok := permissionFor("/admin/operation/user/role/delete"); ok {
		t.Fatalf("未登记的路径不应命中权限表")
	}
	if _, ok := permissionFor("/admin/audit/entry/list"); ok {
		t.Fatalf("audit 只读检索走免中间件路由组，不应出现在权限表里")
	}
	if _, ok := permissionFor("/admin/ops/config/list"); ok {
		t.Fatalf("ops-config 只读列表走免中间件路由组，不应出现在权限表里")
	}
	want, ok := permissionFor("/admin/operation/task/run")
	if !ok || want.Resource != "operation:task" || want.Action != "run" {
		t.Fatalf("task/run 权限 = %+v, want operation:task/run", want)
	}
	// audit 写入口的权限点必须与 op_permission seed 迁移里登记的码一致（见 gateway/admin/README.md）。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/audit/export/create":  {"audit:export", "create"},
		"/admin/audit/export/run":     {"audit:export", "run"},
		"/admin/audit/retention/save": {"audit:retention", "update"},
		"/admin/audit/archive/run":    {"audit:archive", "create"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// ops-config 写入口：发布/回滚必须分开授权（回滚是把旧值再发布一次的高危动作），
	// 条目全量覆盖（topic_item/slot_item）与定义保存（topic/slot）也分开，
	// 这样「能排专题内容」的角色不等于「能改配置值」。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/ops/config/publish":   {"ops:config", "publish"},
		"/admin/ops/config/rollback":  {"ops:config", "rollback"},
		"/admin/ops/rollout/save":     {"ops:rollout", "update"},
		"/admin/ops/rollout/state":    {"ops:rollout", "enable"},
		"/admin/ops/topic/save":       {"ops:topic", "update"},
		"/admin/ops/topic/items/save": {"ops:topic_item", "update"},
		"/admin/ops/slot/save":        {"ops:slot", "update"},
		"/admin/ops/slot/items/save":  {"ops:slot_item", "update"},
		"/admin/ops/switch/save":      {"ops:switch", "update"},
		"/admin/ops/cache/refresh":    {"ops:cache", "refresh"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// live-room 写入口：下架（终态）与禁播（可解除）分开，禁播下发与解除也分开，
	// 这样「能解封违规房间」的角色不会顺带拿到「封人」的能力，
	// 分区字典维护与单个房间配置也分属不同角色。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/live/room/close":     {"live:room", "close"},
		"/admin/live/room/ban":       {"live:ban", "create"},
		"/admin/live/room/ban/lift":  {"live:ban", "lift"},
		"/admin/live/setting/update": {"live:setting", "update"},
		"/admin/live/area/upsert":    {"live:area", "update"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// live-ingest 写入口：强制断流与吊销密钥分开——吊销是不可逆终态且能级联停流，
	// 比一次性的断流更重；节点登记改变所有后续分配的依据，事件重试会批量重推给全部消费方，
	// 四者各占一个权限点，不让「能处置一条流」顺带等于「能改基础设施」。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/live/stream/close":      {"live:stream", "close"},
		"/admin/live/stream/key/revoke": {"live:key", "revoke"},
		"/admin/live/node/upsert":       {"live:node", "update"},
		"/admin/live/event/retry":       {"live:event", "retry"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// live-gateway 写入口：kick 针对单连接（含可写的禁止重连窗口），drain 针对整房间的下发拓扑，
	// broadcast/send 是唯一能主动推内容到在线连接的能力，quota/update 改一层影响全部下级作用域。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/live/connection/kick": {"live:connection", "kick"},
		"/admin/live/route/drain":     {"live:route", "drain"},
		"/admin/live/broadcast/send":  {"live:broadcast", "send"},
		"/admin/live/quota/upsert":    {"live:quota", "update"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// event-collector 写入口四条各占一个权限点：推进在途投递是常规运维动作（有 cron 兜底），
	// 死信重放会让下游已收敛的结论重新出现，策略 upsert 只写 DRAFT（不生效），
	// 切换 ACTIVE 立刻改变所有终端上报的采样与脱敏口径——本域最重的一步必须能单独收回。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/collector/delivery/retry":     {"collector:delivery", "retry"},
		"/admin/collector/dead-letter/replay": {"collector:deadletter", "replay"},
		"/admin/collector/policy/upsert":      {"collector:policy", "update"},
		"/admin/collector/policy/activate":    {"collector:policy", "enable"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// collector 只读面（批次/事件台账、死信列表、策略读取、健康度、干跑校验）必须在表外：
	// 排障页每次刷新都会打一次 RPC，全量挂判定会把 operation 变成读放大瓶颈；
	// /schema/validate 虽走 POST，但它是干跑（不落库、不投递、不写台账），与读取同组。
	for _, public := range []string{
		"/admin/collector/schema/validate", "/admin/collector/batch", "/admin/collector/batch/list",
		"/admin/collector/event", "/admin/collector/event/list",
		"/admin/collector/dead-letter/list", "/admin/collector/policy/active",
		"/admin/collector/policy/list", "/admin/collector/health",
	} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("collector 只读路由 %s 不应出现在权限表里", public)
		}
	}
	// 私信运营面的两个写入口：处置举报与「物理删除到期正文」影响面完全不同
	// （前者改一条举报的终态，后者不可逆且一次一批），必须能单独授予与单独收回。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/private-message/report/handle":   {"pm:report", "handle"},
		"/admin/private-message/retention/purge": {"pm:retention", "purge"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// 举报台账读取在表外：与其它只读面同口径，读取主体由 operator_mid 承载
	// （private-message 拒无主体读取）；ApplyModerationVerdict 根本没有 HTTP 入口
	// （审核结论只能由审核侧以服务身份写入），因此这里也不为它留任何路径。
	for _, public := range []string{"/admin/private-message/report/list"} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("私信只读路由 %s 不应出现在权限表里", public)
		}
	}
	if _, ok := permissionFor("/admin/private-message/verdict"); ok {
		t.Fatalf("审核结论回写不得成为后台路由（会多出一条不过审核就能改结论的入口）")
	}
	// live 只读面必须在表外：房间/场次/分区/主播/禁播台账走免中间件路由组，
	// 台账的读取主体由 operator_mid 承载（live-room 拒无主体读取），不占用权限点。
	for _, public := range []string{
		"/admin/live/room", "/admin/live/room/list", "/admin/live/room/bans",
		"/admin/live/session", "/admin/live/session/list",
		"/admin/live/area/list", "/admin/live/anchor/list",
	} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("live 只读路由 %s 不应出现在权限表里", public)
		}
	}
	// live-ingest / live-gateway 的只读面同上：流状态、密钥台账、健康、节点、分配、断流、事件、
	// 连接列表、路由、广播审计、配额读取全部走免中间件路由组。
	// 注意 /stream 与 /stream/list 这类前缀互为包含关系，必须整串精确比对（本断言用的是 map 键）。
	for _, public := range []string{
		"/admin/live/stream", "/admin/live/stream/list", "/admin/live/stream/key",
		"/admin/live/stream/key/list", "/admin/live/stream/health",
		"/admin/live/node/list", "/admin/live/assignment/list",
		"/admin/live/interruption/list", "/admin/live/event/list",
		"/admin/live/connection/list", "/admin/live/route/list",
		"/admin/live/broadcast/log", "/admin/live/quota",
	} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("live 只读路由 %s 不应出现在权限表里", public)
		}
	}
	// recommend 写入口：池的上线/回滚/清理各自独立——prune 会真删历史版本行（删了就没回放证据），
	// 不该跟着「能上线一个 READY 版本」一起送来；rollback 是应急开关，能回滚不等于能发布。
	// 模型与实验的「登记」与「状态迁移」分开：能登记一个未上线的模型版本或实验变体，
	// 不等于能把它切成线上生效（后者直接影响所有终端用户的推荐结果）。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/recommend/pool/item/upsert":           {"recommend:pool", "update"},
		"/admin/recommend/pool/version/publish":       {"recommend:pool", "publish"},
		"/admin/recommend/pool/version/rollback":      {"recommend:pool", "rollback"},
		"/admin/recommend/pool/version/prune":         {"recommend:pool", "prune"},
		"/admin/recommend/rank/model/upsert":          {"recommend:model", "create"},
		"/admin/recommend/rank/model/state":           {"recommend:model", "state"},
		"/admin/recommend/rank/feature-config/upsert": {"recommend:feature", "create"},
		"/admin/recommend/rank/experiment/upsert":     {"recommend:experiment", "create"},
		"/admin/recommend/rank/experiment/state":      {"recommend:experiment", "state"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// recommend 只读面必须在表外：池快照/版本台账/召回日志/决策摘要/运行时配置走免中间件路由组，
	// 与 audit、ops-config、cron、live 读面同一口径（读的是排障数据，不产生任何写入）。
	for _, public := range []string{
		"/admin/recommend/pool/snapshot", "/admin/recommend/pool/version/list",
		"/admin/recommend/pool/config", "/admin/recommend/recall/log",
		"/admin/recommend/recall/log/list", "/admin/recommend/rank/decision",
		"/admin/recommend/rank/decision/list", "/admin/recommend/rank/runtime-config",
	} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("recommend 只读路由 %s 不应出现在权限表里", public)
		}
	}
	// 刻意不接的下游方法不得因为「顺手」而出现路由：热路径与按主体的分桶查询都不属 admin 面。
	for _, absent := range []string{
		"/admin/recommend/candidates", "/admin/recommend/recall/candidates",
		"/admin/recommend/rank/candidates", "/admin/recommend/rank/experiment/assignment",
	} {
		if _, ok := routePermissions[absent]; ok {
			t.Fatalf("%s 不该出现在权限表里（在线热路径/按主体分桶不属 admin 面）", absent)
		}
	}
	// live-media 写入口：转码的四个动作后果互不可替代，必须分列——
	// start 新增一路转码（产生成本）、stop 让运行中的任务收尾、retry 重开 FAILED 任务（attempt+1，
	// 再次产生成本）、cancel 是不可复活的终态；并成一个「操作转码」点会让「能取消」顺带等于「能重开」。
	// 档位方向相反的两条（upsert 让档位存在 / offline 让观众侧立刻取不到）分列；
	// 录制的 start 与 stop 同理；回放的 submit（登记拼接）/ bind（写引用）/ state（刷新 video 投影）
	// 三条按后果分列，三条都不得推进稿件状态；回收只有 submit（purge 是它的参数而非独立方法）。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/live/transcode/start":      {"live:transcode", "start"},
		"/admin/live/transcode/stop":       {"live:transcode", "stop"},
		"/admin/live/transcode/retry":      {"live:transcode", "retry"},
		"/admin/live/transcode/cancel":     {"live:transcode", "cancel"},
		"/admin/live/output/upsert":        {"live:output", "update"},
		"/admin/live/output/offline":       {"live:output", "offline"},
		"/admin/live/record/start":         {"live:record", "start"},
		"/admin/live/record/stop":          {"live:record", "stop"},
		"/admin/live/replay/submit":        {"live:replay", "submit"},
		"/admin/live/replay/asset/bind":    {"live:replay", "bind"},
		"/admin/live/replay/content/state": {"live:replay", "state"},
		"/admin/live/retention/submit":     {"live:retention", "submit"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// live-media 只读面必须在表外：转码/档位/录制/切片/回放/引用/回收七类台账的读取
	// 走免中间件路由组，与 audit、ops-config、cron、live 其余三服务、recommend 读面同一口径
	// （读的是排障数据，不产生任何写入，也没有需要留痕的处置动作）。
	for _, public := range []string{
		"/admin/live/transcode", "/admin/live/transcode/list",
		"/admin/live/output/list",
		"/admin/live/record", "/admin/live/record/list", "/admin/live/record/segment/list",
		"/admin/live/replay", "/admin/live/replay/list", "/admin/live/replay/asset/list",
		"/admin/live/retention", "/admin/live/retention/list",
	} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("live-media 只读路由 %s 不应出现在权限表里", public)
		}
	}
	// 刻意不接的 live-media 下游方法不得出现路由：五个 Report* 是 Worker→服务的回报口，
	// 入参带 worker_id 与 expected_version，后台开面等于把状态机推进权交给控制台（AGENTS.md §8）。
	for _, absent := range []string{
		"/admin/live/transcode/report", "/admin/live/record/report",
		"/admin/live/record/segment/report", "/admin/live/replay/report",
		"/admin/live/retention/report",
	} {
		if _, ok := routePermissions[absent]; ok {
			t.Fatalf("%s 不该出现在权限表里（Worker 回报口不属 admin 面）", absent)
		}
	}
	// spm 域：四个写入口 + 一个定向个人画像读。登记与上下架分开（能拟一个新口径版本不等于
	// 能让它生效，生效会改变全站后续窗口的解释），job 与 recompute 也分开
	// （补跑一段作业 ≠ 把历史窗口的数抹平重算）。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/spm/interest/get":      {"spm:interest", "read"},
		"/admin/spm/definition/upsert": {"spm:definition", "create"},
		"/admin/spm/definition/state":  {"spm:definition", "state"},
		"/admin/spm/job/submit":        {"spm:job", "create"},
		"/admin/spm/metric/recompute":  {"spm:metric", "recompute"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// spm 十处只读必须在表外：指标读、榜单、留存、口径读、作业读、消费状态与死信台账
	// 都是排障/看数入口，每次刷新都会打一次 RPC（与 audit/cron/live/recommend/collector 读面同口径）。
	// /interest/get 是唯一例外——它有明确的被读主体（某个 mid），属个人数据访问。
	for _, public := range []string{
		"/admin/spm/metric/get", "/admin/spm/metric/batch-get", "/admin/spm/hot-subject/list",
		"/admin/spm/retention/get", "/admin/spm/definition/get", "/admin/spm/definition/list",
		"/admin/spm/job/get", "/admin/spm/job/list",
		"/admin/spm/consumer-state/list", "/admin/spm/dead-letter/list",
	} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("spm 只读路由 %s 不应出现在权限表里", public)
		}
	}
	// WriteMetricWindow 是计算链路的窗口写回通道，不得成为后台路由：
	// 开一个口等于允许手工改指标，违反 AGENTS.md §7 第 3 条（SPM 不提供推荐结果的写面）。
	if _, ok := permissionFor("/admin/spm/metric/write"); ok {
		t.Fatalf("窗口指标写回不得成为后台路由（手工改指标没有证据价值）")
	}
	// feature-store 域：七个写入口 + 一个定向个人特征导出读。
	// 定义的登记/状态/隐私三点分开（新增一个 DRAFT 版本 ≠ 让它生效 ≠ 改它能被谁读），
	// 版本指针切换单列（切一次全站在线读换一版），回填作业是写数据，
	// 两条删除路径（按 TTL 批量清 / 按主体擦）各自独立且都不可逆。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/feature-store/entity-feature/list":  {"feature:entity-value", "read"},
		"/admin/feature-store/definition/register":  {"feature:definition", "create"},
		"/admin/feature-store/definition/state":     {"feature:definition", "state"},
		"/admin/feature-store/definition/privacy":   {"feature:definition", "privacy"},
		"/admin/feature-store/version/switch":       {"feature:active-version", "switch"},
		"/admin/feature-store/backfill/submit":      {"feature:backfill-job", "create"},
		"/admin/feature-store/entity-feature/erase": {"feature:entity-value", "erase"},
		"/admin/feature-store/retention/purge":      {"feature:value", "purge"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// feature-store 五处只读必须在表外：特征目录/单定义读/版本切换流水/作业台账都是排障入口。
	// /entity-feature/list 是唯一例外——它有明确的被读主体，读的是那个人的全部特征值。
	for _, public := range []string{
		"/admin/feature-store/definition/get", "/admin/feature-store/definition/list",
		"/admin/feature-store/version-switch/list", "/admin/feature-store/backfill/get",
		"/admin/feature-store/backfill/list",
	} {
		if _, ok := routePermissions[public]; ok {
			t.Fatalf("feature-store 只读路由 %s 不应出现在权限表里", public)
		}
	}
	// WriteFeatures 是计算链路专属写回通道（后台代写等于让运营手捏特征值喂推荐），
	// GetFeature/BatchGetFeatures 是在线热路径（配额与延迟预算属 recommend-*）：
	// 三者都不得因为「顺手加个排障入口」而出现路由。
	for _, absent := range []string{
		"/admin/feature-store/feature/write", "/admin/feature-store/feature/get",
		"/admin/feature-store/feature/batch-get",
	} {
		if _, ok := permissionFor(absent); ok {
			t.Fatalf("%s 不该出现在权限表里（特征值写回与在线热路径读不属 admin 面）", absent)
		}
	}
	// open-platform 域：9 个写入口 + 6 条台账读。
	// 三条硬口径决定哪些点必须分开：状态推进决定第三方整体能不能调用，密钥的轮换与吊销
	// 是不可逆程度不同的两件事（轮换有宽限期、吊销是紧急止血且能一次清空），
	// scope 授予决定「能读到哪些用户数据范围」而授权撤销决定「某个用户的授权还在不在」，
	// 配额规则改的是限流阈值、配额重算改的是投影数据，回调端点删除会顺带抑制未投递任务，
	// 死信重放是明知上游未修好也要再打一次。九点任何两点合并都会让一个角色顺带拿到别的后果。
	for path, wantPerm := range map[string]adminPermission{
		"/admin/open-platform/application/get":        {"openplatform:application", "read"},
		"/admin/open-platform/application/list":       {"openplatform:application", "read"},
		"/admin/open-platform/application/state":      {"openplatform:application", "state"},
		"/admin/open-platform/secret/rotate":          {"openplatform:secret", "rotate"},
		"/admin/open-platform/secret/revoke":          {"openplatform:secret", "revoke"},
		"/admin/open-platform/scope/grant":            {"openplatform:scope", "grant"},
		"/admin/open-platform/authorization/revoke":   {"openplatform:authorization", "revoke"},
		"/admin/open-platform/quota/policy/list":      {"openplatform:quota-policy", "read"},
		"/admin/open-platform/quota/policy/upsert":    {"openplatform:quota-policy", "upsert"},
		"/admin/open-platform/quota/usage/list":       {"openplatform:quota", "read"},
		"/admin/open-platform/quota/recompute":        {"openplatform:quota", "recompute"},
		"/admin/open-platform/webhook/list":           {"openplatform:webhook", "read"},
		"/admin/open-platform/webhook/delete":         {"openplatform:webhook", "delete"},
		"/admin/open-platform/webhook/delivery/list":  {"openplatform:delivery", "read"},
		"/admin/open-platform/webhook/delivery/retry": {"openplatform:delivery", "retry"},
	} {
		if got, ok := permissionFor(path); !ok || got != wantPerm {
			t.Fatalf("%s 权限 = %+v, want %+v", path, got, wantPerm)
		}
	}
	// /scope/list 是本域唯一免判定的读：scope 目录是纯字典（可授予哪些能力），
	// 且 ListScopesReq 里没有操作者位——挂判定只会把一个「谁在查目录」的假主体塞进去。
	if _, ok := permissionFor("/admin/open-platform/scope/list"); ok {
		t.Fatalf("open-platform scope 目录读不应出现在权限表里")
	}
	// 刻意不接的八个下游方法不得出现路由：三个「代替归属者表达意愿」的注册/入队口
	// （RegisterApplication/RegisterWebhook/EnqueueWebhookEvent）与凭证链路的五法
	// （IssueAuthorizationCode/ExchangeAuthorizationCode/RefreshAccessToken/IntrospectToken/
	// AuthorizeRequest）——后台开面等于允许运营以用户身份取 token 或凭空造一次投递。
	for _, absent := range []string{
		"/admin/open-platform/application/register", "/admin/open-platform/webhook/register",
		"/admin/open-platform/webhook/event/enqueue",
		"/admin/open-platform/oauth/code/issue", "/admin/open-platform/oauth/code/exchange",
		"/admin/open-platform/oauth/token/refresh", "/admin/open-platform/oauth/token/introspect",
		"/admin/open-platform/oauth/authorize-request",
	} {
		if _, ok := permissionFor(absent); ok {
			t.Fatalf("%s 不该出现在权限表里（归属者注册与凭证签发/校验不属 admin 面）", absent)
		}
	}
}

func TestAdminPermissionDeniesWithoutClient(t *testing.T) {
	SetOperationClient(nil)
	defer SetOperationClient(nil)

	w := doHandle(t, NewAdminPermissionMiddleware(), "/admin/operation/user/list", "Bearer adm_op_1", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配置 operation 时应 503, got %d body=%s", w.Code, w.Body.String())
	}
	if env := decodeEnvelope(t, w); env.Code != httpresponse.CodeInternalError {
		t.Fatalf("业务码 = %d, want %d", env.Code, httpresponse.CodeInternalError)
	}
}

func TestAdminPermissionHandle(t *testing.T) {
	const path = "/admin/operation/user/list"

	cases := []struct {
		name        string
		reply       *operationrpc.VerifyAdminPermissionReply
		rpcErr      error
		authorizing string
		wantToken   string
		wantStatus  int
		wantCode    int
	}{
		{
			name:        "无 token",
			authorizing: "",
			wantStatus:  http.StatusUnauthorized,
			wantCode:    httpresponse.CodeUnauthorized,
		},
		{
			name:        "会话非法 401",
			authorizing: "Bearer adm_op_expired",
			wantToken:   "adm_op_expired",
			reply:       &operationrpc.VerifyAdminPermissionReply{Allowed: false, Reason: reasonSessionInvalid},
			wantStatus:  http.StatusUnauthorized,
			wantCode:    httpresponse.CodeUnauthorized,
		},
		{
			name:        "权限不足 403",
			authorizing: "Bearer adm_op_ok",
			wantToken:   "adm_op_ok",
			reply:       &operationrpc.VerifyAdminPermissionReply{Allowed: false, AdminId: 77, Reason: "permission_denied"},
			wantStatus:  http.StatusForbidden,
			wantCode:    httpresponse.CodeForbidden,
		},
		{
			name:        "下游未给 reason 也按拒绝",
			authorizing: "Bearer adm_op_ok",
			wantToken:   "adm_op_ok",
			reply:       &operationrpc.VerifyAdminPermissionReply{Allowed: false, AdminId: 77},
			wantStatus:  http.StatusForbidden,
			wantCode:    httpresponse.CodeForbidden,
		},
		{
			name:        "判定异常 fail-closed",
			authorizing: "Bearer adm_op_ok",
			wantToken:   "adm_op_ok",
			rpcErr:      errors.New("operation: rpc unavailable"),
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    httpresponse.CodeInternalError,
		},
		{
			name:        "放行但给不出主体",
			authorizing: "Bearer adm_op_ok",
			wantToken:   "adm_op_ok",
			reply:       &operationrpc.VerifyAdminPermissionReply{Allowed: true, AdminId: 0},
			wantStatus:  http.StatusServiceUnavailable,
			wantCode:    httpresponse.CodeInternalError,
		},
		{
			name:        "放行",
			authorizing: "Bearer adm_op_ok",
			wantToken:   "adm_op_ok",
			reply:       &operationrpc.VerifyAdminPermissionReply{Allowed: true, AdminId: 77, MatchedRoles: []string{"content_ops"}},
			wantStatus:  http.StatusNoContent,
		},
		{
			name:        "裸 token 放行",
			authorizing: "adm_op_ok",
			wantToken:   "adm_op_ok",
			reply:       &operationrpc.VerifyAdminPermissionReply{Allowed: true, AdminId: 77},
			wantStatus:  http.StatusNoContent,
		},
		{
			name:        "非 bearer scheme 视为无凭证",
			authorizing: "Basic YWJjOmRlZg==",
			wantStatus:  http.StatusUnauthorized,
			wantCode:    httpresponse.CodeUnauthorized,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := &fakeOperation{reply: c.reply, err: c.rpcErr}
			SetOperationClient(cli)
			defer SetOperationClient(nil)

			w := doHandle(t, NewAdminPermissionMiddleware(), path, c.authorizing, "trace-abc")
			if w.Code != c.wantStatus {
				t.Fatalf("HTTP 状态 = %d, want %d body=%s", w.Code, c.wantStatus, w.Body.String())
			}
			if c.wantStatus == http.StatusNoContent {
				if env := w.Body.String(); env != "" {
					t.Fatalf("放行时不应写响应体: %q", env)
				}
			} else if env := decodeEnvelope(t, w); env.Code != c.wantCode {
				t.Fatalf("业务码 = %d, want %d", env.Code, c.wantCode)
			}

			calls := len(cli.reqs)
			if c.wantToken == "" {
				if calls != 0 {
					t.Fatalf("没有有效凭证时不应调用判定 RPC, calls=%d", calls)
				}
				return
			}
			if calls != 1 {
				t.Fatalf("应调用判定 RPC 一次, calls=%d", calls)
			}
			in := cli.reqs[0]
			if in.GetResource() != "operation:admin_user" || in.GetAction() != "read" {
				t.Fatalf("判定入参 = %s/%s, want operation:admin_user/read", in.GetResource(), in.GetAction())
			}
			if in.GetToken() != c.wantToken {
				t.Fatalf("token 透传 = %q, want %q", in.GetToken(), c.wantToken)
			}
			if in.GetAdminId() != 0 {
				t.Fatalf("中间件只带 token 判定，admin_id 应由 operation 解析, got %d", in.GetAdminId())
			}
			if in.GetTraceId() != "trace-abc" {
				t.Fatalf("trace_id = %q, want trace-abc", in.GetTraceId())
			}
		})
	}
}

// 表外路由（例如漏登记的新路由）必须拒绝，而不是默认放行。
func TestAdminPermissionDeniesUnregisteredPath(t *testing.T) {
	cli := &fakeOperation{reply: &operationrpc.VerifyAdminPermissionReply{Allowed: true, AdminId: 77}}
	SetOperationClient(cli)
	defer SetOperationClient(nil)

	w := doHandle(t, NewAdminPermissionMiddleware(), "/admin/operation/task/approve", "Bearer adm_op_ok", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("未登记路由应 403, got %d body=%s", w.Code, w.Body.String())
	}
	if len(cli.reqs) != 0 {
		t.Fatalf("未登记路由不应调用判定 RPC")
	}
}

// 判定通过后身份与角色必须能被 logic 读到（审计主体来源）。
func TestAdminIdentityRoundTrip(t *testing.T) {
	ctx := WithAdmin(context.Background(), AdminIdentity{AdminID: 77, Roles: []string{"super_admin"}})
	id, ok := AdminFromContext(ctx)
	if !ok || id.AdminID != 77 || len(id.Roles) != 1 {
		t.Fatalf("身份回填失败: %+v ok=%v", id, ok)
	}
	if _, ok := AdminFromContext(context.Background()); ok {
		t.Fatalf("未鉴权请求不应读到身份")
	}
	// admin_id<=0 的写入视同未鉴权，避免逻辑拿到 0 主体去写审计。
	if _, ok := AdminFromContext(WithAdmin(context.Background(), AdminIdentity{AdminID: 0})); ok {
		t.Fatalf("admin_id<=0 应视为无身份")
	}
}
