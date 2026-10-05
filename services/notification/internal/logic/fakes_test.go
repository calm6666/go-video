package logic

// fakes_test.go 是 notification logic 层 11 个 RPC 方法单测的装配 helper。
//
// 注入缝口径（全部复用既有缝，本文件不新增任何生产注入点）：
//   - repository.NewWithModels(quota, tmpl, deliv, offset, dead, dnd)：把 5 张表的 model
//     换成 internal/testx 的内存假件，因此不需要真实 MySQL；
//   - send.New(repo, Options)：Enqueuer 是具体类型且只吃 *Repository，
//     所以 SendNotification 走的是**真实**入队用例（校验/模板解析/配额/幂等），不是替身；
//   - consumer.NewDispatcher / NewEventHandler 同样用假件 repo 装配，
//     SyncSend 分支因此可断言「落库后真的外发了一次」；
//   - ServiceContext 用结构体字面量装配：生产 NewServiceContext 会连
//     MySQL/Redis/etcd 并起后台协程，单测禁止调用，本文件也不改它的行为。
//
// 四条假件纪律（改本文件时保持）：
//  ① 读取返回值拷贝：testx 假件的 FindOne/Find/List 全部返回克隆行（对应「每次查询返回新对象」），
//     logic 里对返回结构的收尾赋值不会反向污染「库存行」，所以「没写库却改了状态」这类缺陷
//     能被人手读回 Rows() 的断言抓到。
//  ② Insert 像真实 SQL 一样分配自增主键：notification_template.id 与
//     notification_dead_letter.id 由假件next 计数器分配并回写（对应 LastInsertId）；
//     delivery_id 由生产代码 idgen.MustULID() 生成，假件不代填。
//     生产 SQL 从不写的字段，假件也一律不写，否则 id=0 冲突这类缺陷会被掩盖。
//  ③ 有序 callLog：testx.Trace 记录 "<假件>.<方法>:<定位键>"，既断言次数也断言先后顺序
//     （RetryDeadLetter 的「先复位投递任务、再把死信标记为已重投」只有顺序能证明）。
//     定位键只用确定性字段；ULID 主键与 sha256 行键不进轨迹，非确定性值改用
//     「调用次数 + 读回值」断言，不断言字面量。
//  ④ 预热必须静默：Seed/Rows 直接打在具体假件上、不经过 Spy 代理，因此不写 callLog，
//     wantOps 的期望序列里不会出现 setup 噪声。
//
// 覆盖边界（如实声明）：假件只证明 logic 的判定链与投影口径，**不证明 SQL**。
// logic 的 11 个方法全部经 model 接口读写，不触达 conn；但 repository.Ping 与
// model 层的裸 SQL/事务（PublishDraft 的 TransactCtx）在本层无法离线覆盖，见 README「已知缺口」。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go-video/services/notification/internal/config"
	"go-video/services/notification/internal/consumer"
	"go-video/services/notification/internal/provider"
	"go-video/services/notification/internal/repository"
	"go-video/services/notification/internal/send"
	"go-video/services/notification/internal/svc"
	"go-video/services/notification/internal/testx"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// 用例共用的固定名字，避免每条用例各写一套导致断言口径漂移。
const (
	codeShip   = "order_shipped"       // 已发布模板码
	codeVerify = "login_code"          // 验证码类模板码
	tzShanghai = "Asia/Shanghai"       // 默认时区
	opAdmin    = "op-admin-1"          // 运营操作人
	midAlice   = int64(1001)           // 常规接收人
	midBob     = int64(1002)           // 第二个接收人
	tplTitle   = "发货通知"                // 无变量标题
	tplBody    = "订单 {{order_no}} 已发货" // 带一个变量的正文
)

// --- 装配 ---

type env struct {
	tr     *testx.Trace
	tmpl   *testx.TemplateModel
	deliv  *testx.DeliveryModel
	offset *testx.OffsetModel
	dead   *testx.DeadLetterModel
	dnd    *testx.DndPrefModel
	quota  *testx.QuotaCounter

	// 各域的 Spy，用例按方法名注入存储错误。
	tmplSpy   *testx.TemplateSpy
	delivSpy  *testx.DeliverySpy
	offsetSpy *testx.OffsetSpy
	deadSpy   *testx.DeadLetterSpy
	dndSpy    *testx.DndSpy

	conf    config.NotificationConf
	provs   []provider.Provider
	svcCtx  *svc.ServiceContext
	enqueue *send.Enqueuer
	repo    *repository.Repository
}

// option 在装配 ServiceContext 之前调整环境。
type option func(t *testing.T, e *env)

// withNotify 调整投递策略配置（配额、免打扰开关、同步发送等）。
func withNotify(mut func(*config.NotificationConf)) option {
	return func(_ *testing.T, e *env) { mut(&e.conf) }
}

// withProviders 只注册通道适配器，不打开 SyncSend/Dispatcher：
// 用于「配置组合不完整」（SyncSend=true 而 DispatcherEnabled=false）这类边界用例。
func withProviders(provs ...*testx.Provider) option {
	return func(_ *testing.T, e *env) {
		for _, p := range provs {
			e.provs = append(e.provs, p)
		}
	}
}

// withSyncSend 打开 SyncSend 并装配投递调度器：
// 用例因此能断言「落库后立即投一次」这条只在 logic 里存在的接线，
// 以及「幂等回放/被拦截任务绝不外发」。
func withSyncSend(provs ...*testx.Provider) option {
	return func(_ *testing.T, e *env) {
		e.conf.SyncSend = true
		e.conf.DispatcherEnabled = true
		for _, p := range provs {
			e.provs = append(e.provs, p)
		}
	}
}

// defaultConf 取 etc 示例里的等价默认值（配额先设成不限制，
// 让不关心频控的用例不必额外构造 Redis 计数）。
func defaultConf() config.NotificationConf {
	return config.NotificationConf{
		DailyQuotaPerMid:   0,
		DndEnabled:         true,
		DefaultTimezone:    tzShanghai,
		DefaultLanguage:    model.LangZhCN,
		BackoffSeconds:     []int64{1, 2, 3},
		MaxDeliveryRetries: 3,
		SyncSend:           false,
		DispatcherEnabled:  false,
		DispatchIntervalMs: 1000,
		DispatchBatch:      64,
		MaxRecipients:      200,
	}
}

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	e := &env{
		tr:     testx.NewTrace(),
		tmpl:   testx.NewTemplateModel(),
		deliv:  testx.NewDeliveryModel(),
		offset: testx.NewOffsetModel(),
		dead:   testx.NewDeadLetterModel(),
		dnd:    testx.NewDndPrefModel(),
		quota:  testx.NewQuotaCounter(),
		conf:   defaultConf(),
	}
	e.tmplSpy = testx.SpyTemplate(e.tmpl, e.tr)
	e.delivSpy = testx.SpyDelivery(e.deliv, e.tr)
	e.offsetSpy = testx.SpyOffset(e.offset, e.tr)
	e.deadSpy = testx.SpyDeadLetter(e.dead, e.tr)
	e.dndSpy = testx.SpyDnd(e.dnd, e.tr)

	for _, o := range opts {
		o(t, e)
	}

	var cfg config.Config
	cfg.Notification = e.conf

	e.repo = repository.NewWithModels(
		testx.SpyQuota(e.quota, e.tr), e.tmplSpy, e.delivSpy, e.offsetSpy, e.deadSpy, e.dndSpy)

	enq, err := send.New(e.repo, send.OptionsFrom(cfg.Notification))
	if err != nil {
		t.Fatalf("send.New: %v", err)
	}
	e.enqueue = enq

	s := &svc.ServiceContext{Config: cfg, Repository: e.repo, Enqueuer: enq}

	// 通道适配器注册表 + 投递调度器：只在用例显式打开 DispatcherEnabled 时装配，
	// 与生产 svc.startWorkers 的分支条件保持一致。
	registry := provider.NewRegistry()
	for _, p := range e.provs {
		if err := registry.Register(p); err != nil {
			t.Fatalf("注册 fake provider: %v", err)
		}
	}
	s.Providers = registry
	if cfg.Notification.DispatcherEnabled {
		d, derr := consumer.NewDispatcher(e.repo, registry, provider.NewPassthroughResolver(),
			consumer.NewDispatchPolicy(cfg.Notification))
		if derr != nil {
			t.Fatalf("NewDispatcher: %v", derr)
		}
		s.Dispatcher = d
	}

	// 事件处理器：生产里始终存在（RetryDeadLetter 靠它重放事件），且不 Start 就不连 Kafka。
	h, herr := consumer.NewEventHandler(e.repo, enq, consumer.NewEventPolicy(cfg.Kafka, cfg.Notification))
	if herr != nil {
		t.Fatalf("NewEventHandler: %v", herr)
	}
	s.Events = h

	e.svcCtx = s
	return e
}

// newProvider 构造一个已受理的 fake 通道适配器。
func newProvider(ch string, msgID string) *testx.Provider {
	return &testx.Provider{Chan: ch, AdapterName: "fake-" + ch, MsgID: msgID, Accepted: true}
}

// --- 断言小工具（本包共享） ---

func wantNoErr(t *testing.T, label string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：意外错误 %v", label, err)
	}
}

// wantErrIs 断言错误链里含 want 这个哨兵（logic 用 %w 包装下游错误是允许的）。
func wantErrIs(t *testing.T, label string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want errors.Is(..., %v)", label, want)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s：错误 = %v, want errors.Is(..., %v)", label, err, want)
	}
}

// wantErrContains 断言裸错误（logic 里 errors.New 出来的参数校验分支）带指定线索，
// 避免退化成「只要报错了就行」这种永真断言。
func wantErrContains(t *testing.T, label string, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：错误 = nil, want 包含 %q", label, want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("%s：错误 = %q, want 包含 %q", label, err.Error(), want)
	}
}

func wantEQ[T comparable](t *testing.T, label, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s：%s = %#v, want %#v", label, field, got, want)
	}
}

func wantTrue(t *testing.T, label, field string, got bool) {
	t.Helper()
	if !got {
		t.Errorf("%s：%s 应为真", label, field)
	}
}

// wantContains 断言字符串字段含指定线索（配合 wantErrContains 用于状态原因/摘要断言）。
func wantContains(t *testing.T, label, field, got, want string) {
	t.Helper()
	if want == "" {
		t.Fatalf("%s：wantContains 的线索不得为空，否则是永真断言", label)
	}
	if !strings.Contains(got, want) {
		t.Errorf("%s：%s = %q, want 包含 %q", label, field, got, want)
	}
}

// contains 是表驱动用例里的纯判定版（调用方自己决定如何报错）。
func contains(got, want string) bool { return strings.Contains(got, want) }

func wantStringsEQ(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：%v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 项 = %q, want %q（完整 %v）", label, i+1, got[i], want[i], got)
		}
	}
}

// wantInt64s 断言 int64 序列逐项相等（含顺序）。
// 死信列表按 ctime DESC, id DESC 排序，主键是假件分配的自增值，
// 因此期望序列只能用 Seed 的返回值拼出来，不能手写 id=1。
func wantInt64s(t *testing.T, label string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：%v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 项 = %d, want %d（完整 %v）", label, i+1, got[i], want[i], got)
		}
	}
}

// wantChannelsEQ 断言 rpc 通道列表逐项相等（含顺序）。
// 偏好里的 muted_channels 是位掩码还原出来的列表，顺序漂了客户端会闪，
// 因此不能用「集合相等」代替「序列相等」。
func wantChannelsEQ(t *testing.T, label string, got, want []rpc.Channel) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：%v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 项 = %v, want %v（完整 %v）", label, i+1, got[i], want[i], got)
		}
	}
}

// wantOps 断言一段调用轨迹与期望完全一致（顺序 + 条数 + 内容）。
func wantOps(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：调用序列 = [%s], want [%s]", label, strings.Join(got, " → "), strings.Join(want, " → "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s：第 %d 次调用 = %s, want %s（完整序列 [%s]）",
				label, i+1, got[i], want[i], strings.Join(got, " → "))
		}
	}
}

// wantNotContains 断言字符串里不含某片段（隐私不变量：敏感值/未渲染残片不得外泄）。
func wantNotContains(t *testing.T, label, got, forbidden string) {
	t.Helper()
	if forbidden == "" {
		t.Fatalf("%s：wantNotContains 的禁用串不得为空，否则是永真断言", label)
	}
	if strings.Contains(got, forbidden) {
		t.Errorf("%s：%q 不应包含 %q", label, got, forbidden)
	}
}

// --- 读回假件状态（不经过 Spy，保持静默） ---

func (e *env) mark() int             { return e.tr.Mark() }
func (e *env) ops(from int) []string { return e.tr.OpsFrom(from) }

// opsContaining 返回 from 之后轨迹里含指定片段的调用（用于「拒绝路径不得出现某次写」的断言）。
func (e *env) opsContaining(from int, frag string) []string {
	var out []string
	for _, op := range e.ops(from) {
		if strings.Contains(op, frag) {
			out = append(out, op)
		}
	}
	return out
}

// deliveryCount 返回投递表行数。
func (e *env) deliveryCount() int { return len(e.deliv.Rows()) }

// allDeliveries 返回投递表全部行，按 delivery_id 升序（map 迭代顺序随机，
// 不排序的话「第一行」在不同机器上会漂，断言就成了看运气）。
func (e *env) allDeliveries(t *testing.T) []*model.NotificationDelivery {
	t.Helper()
	rows := e.deliv.Rows()
	out := make([]*model.NotificationDelivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeliveryId < out[j].DeliveryId })
	return out
}

// onlyDelivery 返回投递表唯一一行；行数不是 1 直接失败（避免用「第一行」掩盖重复插入）。
func (e *env) onlyDelivery(t *testing.T) *model.NotificationDelivery {
	t.Helper()
	rows := e.allDeliveries(t)
	if len(rows) != 1 {
		t.Fatalf("投递表行数 = %d, want 1", len(rows))
	}
	return rows[0]
}

// deliveriesByMid 返回某用户的全部投递行（假件返回的是快照拷贝，可安全改）。
func (e *env) deliveriesByMid(t *testing.T, mid int64) []*model.NotificationDelivery {
	t.Helper()
	var out []*model.NotificationDelivery
	for _, r := range e.allDeliveries(t) {
		if r.Mid == mid {
			out = append(out, r)
		}
	}
	return out
}

// deliveryByGroup 按请求级 biz_group_key 读回唯一一行。
// 用例里经常已经有一条同 mid 的旧任务，用「第一行」或 ULID 下标都会漂，
// 因此统一按生产代码写入的确定性字段读回（biz_group_key 是调用方给的原值）。
func (e *env) deliveryByGroup(t *testing.T, groupKey string) *model.NotificationDelivery {
	t.Helper()
	var found []*model.NotificationDelivery
	for _, r := range e.allDeliveries(t) {
		if r.BizGroupKey == groupKey {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("biz_group_key=%s 的投递行数 = %d, want 1", groupKey, len(found))
	}
	return found[0]
}

// onlyTemplate 返回模板表唯一一行。
func (e *env) onlyTemplate(t *testing.T) *model.NotificationTemplate {
	t.Helper()
	rows := e.tmpl.Rows()
	if len(rows) != 1 {
		t.Fatalf("模板表行数 = %d, want 1", len(rows))
	}
	return rows[0]
}

// templateAt 按 (code, channel, lang, version) 精确读回模板行；不存在返回 nil。
func (e *env) templateAt(t *testing.T, code string, channel int32, lang string, version int32) *model.NotificationTemplate {
	t.Helper()
	for _, r := range e.tmpl.Rows() {
		if r.TemplateCode == code && r.Channel == channel && r.Lang == lang && r.Version == version {
			return r
		}
	}
	return nil
}

// onlyDeadLetter 返回死信表唯一一行。
func (e *env) onlyDeadLetter(t *testing.T) *model.NotificationDeadLetter {
	t.Helper()
	rows := e.dead.Rows()
	if len(rows) != 1 {
		t.Fatalf("死信表行数 = %d, want 1", len(rows))
	}
	return rows[0]
}

// deadLetter 按主键静默读回死信行；不存在返回 nil。
// 假件的 Seed 会分配自增主键，用例必须拿返回值而不是手写 id。
func (e *env) deadLetter(t *testing.T, id int64) *model.NotificationDeadLetter {
	t.Helper()
	for _, r := range e.dead.Rows() {
		if r.Id == id {
			return r
		}
	}
	t.Fatalf("死信 id=%d 不存在", id)
	return nil
}

// offsetRow 按 event_id 静默读回事件消费状态行。
func (e *env) offsetRow(t *testing.T, eventID string) *model.NotificationConsumerOffset {
	t.Helper()
	row, ok := e.offset.Rows()[eventID]
	if !ok {
		t.Fatalf("事件状态行 event_id=%s 不存在", eventID)
	}
	return row
}

// pref 读回用户偏好行；不存在返回 nil。
func (e *env) pref(t *testing.T, mid int64) *model.NotificationDndPref {
	t.Helper()
	p, err := e.dnd.FindOne(context.Background(), mid)
	if err != nil {
		t.Fatalf("读回偏好失败: %v", err)
	}
	return p
}

// itoa 把假件分配/用例给定的主键拼进轨迹断言里（避免用例里散落 fmt.Sprintf）。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// findTmplOp 拼出 tmpl.Find 的轨迹键（版本参与键值，表驱动用例里避免手写字面量漂移）。
func findTmplOp(version int32) string {
	return fmt.Sprintf("tmpl.Find:%s/%d/%s/v%d", codeShip, model.ChannelPush, model.LangZhCN, version)
}

// listTmplOp 拼出 tmpl.List 的轨迹键（过滤条件 + 分页）。
func listTmplOp(code string, channel int32, lang string, state, pn, ps int32) string {
	return fmt.Sprintf("tmpl.List:code=%s/ch%d/lang=%s/st%d/pn%d/ps%d", code, channel, lang, state, pn, ps)
}

// listDelivOp 拼出 deliv.List 的轨迹键。
func listDelivOp(mid int64, channel, state, pn, ps int32) string {
	return fmt.Sprintf("deliv.List:mid%d/ch%d/st%d/pn%d/ps%d", mid, channel, state, pn, ps)
}

// listDeadOp 拼出 dead.List 的轨迹键。
func listDeadOp(eventID, topic, source string, state, pn, ps int32) string {
	return fmt.Sprintf("dead.List:evt=%s/topic=%s/src=%s/st%d/pn%d/ps%d", eventID, topic, source, state, pn, ps)
}

// --- 预热 helper（静默，不进 callLog） ---

// seedTemplate 预置一个指定状态的模板版本。
func (e *env) seedTemplate(t *testing.T, code string, channel int32, lang, title, body string,
	version, state int32) *model.NotificationTemplate {
	t.Helper()
	row := &model.NotificationTemplate{
		TemplateCode: code, Channel: channel, Lang: lang, TitleTpl: title, BodyTpl: body,
		Version: version, State: state, Operator: opAdmin, Ctime: 1000, Mtime: 1000,
	}
	e.tmpl.Seed(row)
	return row
}

// seedPublished 预置一个已发布模板。
func (e *env) seedPublished(t *testing.T, code string, channel int32, lang, title, body string) {
	t.Helper()
	e.seedTemplate(t, code, channel, lang, title, body, 1, model.TemplateStatePublished)
}

// seedMuted 预置一个「关闭了指定通道」的用户偏好。
func (e *env) seedMuted(t *testing.T, mid int64, channels ...int32) {
	t.Helper()
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: mid, MutedChannels: model.MaskOfChannels(channels), Timezone: tzShanghai,
		State: model.DndStateOn, Ctime: 1000, Mtime: 1000,
	})
}

// seedQuiet 预置一个「开启免打扰且整日静音」的偏好（00:00-23:59 全天窗口）。
func (e *env) seedQuiet(t *testing.T, mid int64) {
	t.Helper()
	e.dnd.Seed(&model.NotificationDndPref{
		Mid: mid, QuietStart: "00:00", QuietEnd: "23:59", Timezone: tzShanghai,
		State: model.DndStateOn, Ctime: 1000, Mtime: 1000,
	})
}

// seedDelivery 预置一条投递任务。
func (e *env) seedDelivery(t *testing.T, d *model.NotificationDelivery) {
	t.Helper()
	if d.DeliveryId == "" || d.BizKey == "" {
		t.Fatalf("seedDelivery 必须给 delivery_id 与 biz_key")
	}
	e.deliv.Seed(d)
}

// seedDeadLetter 预置一条死信并返回其主键。
func (e *env) seedDeadLetter(t *testing.T, d *model.NotificationDeadLetter) int64 {
	t.Helper()
	return e.dead.Seed(d)
}

// seedDeadLetters 批量预置死信，按预置顺序返回假件分配的自增主键。
// 用例必须用返回值引用死信，手写 id 会在假件抬高自增位后失真。
func (e *env) seedDeadLetters(t *testing.T, rows ...*model.NotificationDeadLetter) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, e.dead.Seed(r))
	}
	return ids
}

// seedOffset 预置一条事件消费状态行。
func (e *env) seedOffset(t *testing.T, o *model.NotificationConsumerOffset) {
	t.Helper()
	e.offset.Seed(o)
}

// --- 请求构造 helper ---

// pushReq 构造一条 push 通道的投递请求（模板变量单个 order_no，便于断言快照）。
func pushReq(bizKey string, recips ...*rpc.Recipient) *rpc.SendNotificationReq {
	return &rpc.SendNotificationReq{
		Recipients:     recips,
		Channel:        rpc.Channel_CHANNEL_PUSH,
		TemplateCode:   codeShip,
		TemplateParams: map[string]string{"order_no": "SO-1"},
		BizKey:         bizKey,
		TraceId:        "trace-1",
	}
}

// recip 构造一个带受控标识的接收人。
func recip(mid int64, ref string) *rpc.Recipient {
	return &rpc.Recipient{Mid: mid, TargetRef: ref}
}

// send 调用 SendNotification 并直接返回结果（错误由用例自己判定，helper 不吞）。
func (e *env) send(t *testing.T, in *rpc.SendNotificationReq) (*rpc.SendNotificationReply, error) {
	t.Helper()
	return NewSendNotificationLogic(context.Background(), e.svcCtx).SendNotification(in)
}
