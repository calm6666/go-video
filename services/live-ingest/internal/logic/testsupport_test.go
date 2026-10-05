// 本文件只放用例之间共用的脚手架：环境装配、断言助手、副作用探针与种子数据。
// 内存替身本体在 fakes_test.go。
//
// 装配口径：走真 repository.New + 逐个替换 model 接口字段，而不是手搓一个
// 「测试专用 Repository」——前者能在编译期跟着 Repository 的字段变更一起红，
// 后者会在实现加了新表之后静默漏测。

package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go-video/services/live-ingest/internal/config"
	"go-video/services/live-ingest/internal/repository"
	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

func bg() context.Context { return context.Background() }

// testEnv 是一份隔离的「被测服务」：内存库 + 真 Repository 装配 + 可换网关。
type testEnv struct {
	svc  *svc.ServiceContext
	repo *repository.Repository
	db   *fakeDB
	gw   *fakeGateway
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	db := newFakeDB()
	cfg := config.Config{
		LiveIngest: config.LiveIngestConf{
			IssueTtlSeconds:            604800,
			MaxIssueTtlSeconds:         2592000,
			KeyRandomBytes:             18,
			RotateGraceSeconds:         300,
			MaxStreamsPerKey:           1,
			InterruptGraceSeconds:      60,
			HeartbeatTimeoutSeconds:    30,
			HealthSampleWindowSeconds:  10,
			HealthNoDataSeconds:        30,
			CriticalMinVideoBitrateBps: 200000,
			DegradedMinVideoBitrateBps: 800000,
			CriticalMaxPacketLossPpm:   50000,
			CriticalMinFpsX100:         500,
			MaxListPageSize:            50,
			MaxEventPageSize:           200,
			MaxEventRetryBatch:         200,
			MaxSamplePoints:            120,
			CountHardLimit:             10000,
			CallbackSkewSeconds:        300,
			CallbackNonceTTLSec:        600,
		},
		Cdn: config.CdnConf{
			PublishDomains: []string{"push.example.com"},
		},
	}
	env := &testEnv{db: db, gw: &fakeGateway{}}
	repo := repository.New(&fakeConn{db: db}, nil, cfg)
	repo.Stream = &fakeStream{db: db}
	repo.StreamKey = &fakeStreamKey{db: db}
	repo.IngestNode = &fakeIngestNode{db: db}
	repo.NodeAssignment = &fakeNodeAssignment{db: db}
	repo.StreamEvent = &fakeStreamEvent{db: db}
	repo.Outbox = &fakeOutbox{db: db}
	repo.CdnCallback = &fakeCdnCallback{db: db}
	repo.StreamInterruption = &fakeInterruption{db: db}
	repo.StreamHealthReport = &fakeHealthReport{db: db}
	env.repo = repo
	env.svc = &svc.ServiceContext{Config: cfg, Repository: repo, Gateway: env.gw}
	return env
}

// withoutRepository 复刻「ServiceContext 没装配 Repository」的裸启动形态：
// 用来钉死「没有依赖就报错」而不是「没有依赖就返回空成功」。
func (e *testEnv) withoutRepository() *svc.ServiceContext {
	return &svc.ServiceContext{Config: e.svc.Config, Gateway: e.gw}
}

// --- 断言助手（house 口径：单返回值 + 显式标签） ---

func wantOK[T any](t *testing.T, got T, err error, label string) T {
	t.Helper()
	if err != nil {
		t.Fatalf("%s：意外失败：%v", label, err)
	}
	return got
}

// wantErr 只认 errors.Is：契约用哨兵表达，改文案不该让测试红。
func wantErr(t *testing.T, err error, target error, label string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s：期望错误 %v，实得 %v", label, target, err)
	}
}

// wantFail 额外要求 err 非 nil：nil 错误 + 零值响应就是「伪造成功」，
// 是这套测试最需要挡住的一类通过。
func wantFail(t *testing.T, err error, target error, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s：期望失败 %v，实得成功（伪造成功是最坏的一类通过）", label, target)
	}
	wantErr(t, err, target, label)
}

func mustTrue(t *testing.T, v bool, label string) {
	t.Helper()
	if !v {
		t.Fatalf("%s：期望 true", label)
	}
}

func mustFalse(t *testing.T, v bool, label string) {
	t.Helper()
	if v {
		t.Fatalf("%s：期望 false（true 意味着多做了副作用或谎报了状态）", label)
	}
}

// --- 副作用快照 ---

type effectCounts struct {
	rows [9]int
	txs  int
}

func (e *testEnv) effects() effectCounts {
	return effectCounts{
		rows: [9]int{
			len(e.db.keys), len(e.db.streams), len(e.db.nodes), len(e.db.assigns),
			len(e.db.events), len(e.db.outbox), len(e.db.callbacks), len(e.db.interrupt),
			len(e.db.reports),
		},
		txs: e.db.txCalls,
	}
}

func (c effectCounts) String() string {
	return fmt.Sprintf("key=%d stream=%d node=%d assign=%d event=%d outbox=%d callback=%d interruption=%d health=%d tx=%d",
		c.rows[0], c.rows[1], c.rows[2], c.rows[3], c.rows[4], c.rows[5], c.rows[6], c.rows[7], c.rows[8], c.txs)
}

// requireSameEffects 断言一次请求没留下任何**数据**痕迹：入参被拒、迁移矩阵不合法、
// CAS 输了、事务回滚了，四种情形都要求「九张表的行数」与调用前完全一致。
//
// 这里刻意不比事务开合次数：实现合法的形态是「开事务 → 在事务内判定不合法 → 回滚」，
// 那正是回滚语义要测的东西，把它当副作用会逼测试放宽别的断言。
// 「连事务都不该开」（预检阶段就该返回）用 requireNoTransaction 单独钉。
func (e *testEnv) requireSameEffects(t *testing.T, before effectCounts, label string) {
	t.Helper()
	if got := e.effects(); got.rows != before.rows {
		t.Fatalf("%s：产生了副作用\n  前：%s\n  后：%s", label, before, got)
	}
}

// requireNoTransaction 钉住「这次调用根本没进事务」：幂等预检与入参校验阶段就该返回，
// 拖到事务里再失败会白占连接，MySQL 抖动时表现为整条推流链路排队。
func (e *testEnv) requireNoTransaction(t *testing.T, before effectCounts, label string) {
	t.Helper()
	if got := e.effects(); got.txs != before.txs {
		t.Fatalf("%s：不该开启事务（前 %d 后 %d）", label, before.txs, got.txs)
	}
}

// delta 只回被测调用自己那一段的行数增量（种子写入不该算到被测代码头上）。
func (e *testEnv) delta(before effectCounts) [9]int {
	got := e.effects()
	var out [9]int
	for i := range out {
		out[i] = got.rows[i] - before.rows[i]
	}
	return out
}

func (e *testEnv) requireDelta(t *testing.T, before effectCounts, want [9]int, label string) {
	t.Helper()
	if got := e.delta(before); got != want {
		t.Fatalf("%s：写入增量不符\n  got =%v\n want=%v", label, got, want)
	}
}

// --- 调用顺序留痕 ---

// markCalls 记一个日志水位，配合 callsFrom 只看「被测那一次调用」产生的序列，
// 种子写入的序列不该算到被测代码头上（与 delta 同一口径）。
func (e *testEnv) markCalls() int { return len(e.db.callLog) }

// callsFrom 回水位之后新增的 model 方法调用名（按发起顺序）。
func (e *testEnv) callsFrom(from int) []string {
	return append([]string(nil), e.db.callLog[from:]...)
}

// requireCallsFrom 逐名比对整段序列：次数、顺序、不多做一项全部一起钉住。
// 它是「写序」契约的最强形式——但代价是把读路径也钉死了，
// 所以只在「收尾三件事的先后」这类必须整段锁死的用例上用。
func (e *testEnv) requireCallsFrom(t *testing.T, from int, want []string, label string) {
	t.Helper()
	got := e.callsFrom(from)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s：调用序列不符\n  want=%v\n  got =%v", label, want, got)
	}
}

// requireCallOrder 只校验 want 里这些调用的相对顺序与出现次数（允许中间夹别的调用）：
// 用于「关心写序、不锁定读次数」的用例，放宽的只有读侧，写侧仍逐项钉死。
func (e *testEnv) requireCallOrder(t *testing.T, from int, want []string, label string) {
	t.Helper()
	got := e.callsFrom(from)
	// 出现次数一致：少一次是漏做，多一次是重复做（例如两次退配额）。
	count := map[string]int{}
	for _, name := range got {
		count[name]++
	}
	wantCount := map[string]int{}
	for _, name := range want {
		wantCount[name]++
	}
	for name, n := range wantCount {
		if count[name] != n {
			t.Fatalf("%s：%s 调用次数 %d，期望 %d\n  全序列=%v", label, name, count[name], n, got)
		}
	}
	// 相对顺序：按 want 顺序在 got 里做单调游标，任何倒序都会停下。
	cursor := 0
	for _, name := range want {
		found := -1
		for i := cursor; i < len(got); i++ {
			if got[i] == name {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatalf("%s：%s 未按期望顺序出现（游标停在 %d）\n  want=%v\n  got =%v", label, name, cursor, want, got)
		}
		cursor = found + 1
	}
}

// requireNoCallsAfter 钉住「这次调用连一次 SQL 都没发出去」：
// 入参守卫与预检失败必须拦在读库之前，否则会白占连接并放大故障面。
func (e *testEnv) requireNoCallsAfter(t *testing.T, from int, label string) {
	t.Helper()
	if got := e.callsFrom(from); len(got) != 0 {
		t.Fatalf("%s：不该触达任何 model 方法，实得 %v", label, got)
	}
}

// --- 日志捕获（隐私断言用） ---

// captureLogs 挂一份内存日志接收器。用 AddWriter 而不是 SetWriter：
// 后者会摘掉默认输出，断言写错时连日志都看不见了。
func captureLogs(t *testing.T) *fakeLogWriter {
	t.Helper()
	w := &fakeLogWriter{}
	logx.AddWriter(w)
	return w
}

// fakeLogWriter 把 logx 的输出接一份到内存。
//
// 它存在的理由：本服务有一类契约**只能从日志证明**——「明文密钥/厂商签名/来源 IP
// 一次都不进日志」（README「不保存长期明文推流密钥」+ helpers.go 文件头的隐私纪律）。
// 响应里没有 key_hash 只说明「没回显」，说明不了「日志里也找不到」。
// 用 AddWriter 挂进链而不顶掉默认输出：断言失败时人还能在 go test 输出里看到现场。
type fakeLogWriter struct {
	mu   sync.Mutex
	text []string
}

func (w *fakeLogWriter) add(kind string, v any, fields ...logx.LogField) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.text = append(w.text, kind+" "+fmt.Sprint(v)+fieldsText(fields))
}

// fieldsText 把结构化字段并成 `key=value` 一起留痕。
// logx 把 Infow 的字段作为可变参数传给每个 writer，忽略它们就等于：凡是按
// `reason=...` 这类字段值做的断言全部失效——正向断言永假、隐私反向断言永真。
func fieldsText(fields []logx.LogField) string {
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, " %s=%v", f.Key, f.Value)
	}
	return b.String()
}

func (w *fakeLogWriter) Alert(v any) { w.add("severe", v) }

func (w *fakeLogWriter) Close() error { return nil }

func (w *fakeLogWriter) Debug(v any, fields ...logx.LogField) { w.add("debug", v, fields...) }
func (w *fakeLogWriter) Error(v any, fields ...logx.LogField) { w.add("error", v, fields...) }
func (w *fakeLogWriter) Info(v any, fields ...logx.LogField)  { w.add("info", v, fields...) }
func (w *fakeLogWriter) Severe(v any)                         { w.add("severe", v) }
func (w *fakeLogWriter) Slow(v any, fields ...logx.LogField)  { w.add("slow", v, fields...) }
func (w *fakeLogWriter) Stack(v any)                          { w.add("error", v) }
func (w *fakeLogWriter) Stat(v any, fields ...logx.LogField)  { w.add("stat", v, fields...) }

// joined 回**全部**级别的日志（含结构化字段）：隐私断言必须覆盖 info——把明文密钥拼进推流地址再
// Info 出来，恰好是最顺手的一种泄露方式，只看 error 级等于没测。
func (w *fakeLogWriter) joined() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.text, "\n")
}

// requireAbsent 断言明文材料一次都没进过日志：隐私契约只能被证伪，不能被「只测正向」糊过去。
func requireAbsent(t *testing.T, logs, forbidden, label string) {
	t.Helper()
	if forbidden == "" {
		return
	}
	if strings.Contains(logs, forbidden) {
		t.Fatalf("%s：日志里出现了敏感材料 %q\n%s", label, forbidden, logs)
	}
}

// --- 网关替身 ---

// fakeGateway 默认全部失败：外部依赖缺席时被测代码必须走降级或 fail-closed 分支，
// 而不是伪造「已探测/已踢流」。真实现（repository.stubIngestGateway）恒回
// ErrCdnNotConfigured，本替身只用来制造「配置齐了但仍调用失败」这一格。
//
// probeNodeID/probeStreamID 记下最后一次 ProbeStream 的实参：ProbeStream 的两个入参
// 都是 string，顺序写反（node/stream）在编译期与「返回值被丢弃」的调用点上都看不出来，
// 只能靠实参留痕证伪。
type fakeGateway struct {
	probe     *repository.StreamProbe
	probeErr  error
	kickErr   error
	bindErr   error
	kickCalls int
	probeCall int
	bindCall  int
	// probeNodeID/probeStreamID 只在 probeCall>0 时有意义（零值是「从未调用」）。
	probeNodeID   string
	probeStreamID string
}

func (g *fakeGateway) ProbeStream(_ context.Context, nodeID, streamID string) (*repository.StreamProbe, error) {
	g.probeCall++
	g.probeNodeID, g.probeStreamID = nodeID, streamID
	return g.probe, g.probeErr
}

func (g *fakeGateway) KickStream(context.Context, string, string, string) error {
	g.kickCalls++
	return g.kickErr
}

func (g *fakeGateway) BindCallbackDomain(context.Context, string) error {
	g.bindCall++
	return g.bindErr
}

// --- 种子数据 ---

// seedKey 造一把密钥（走 model 写路径，唯一键与默认值都会先校验种子合法性）。
func (e *testEnv) seedKey(t *testing.T, k *model.StreamKey) *model.StreamKey {
	t.Helper()
	if k.KeyHash == "" {
		k.KeyHash = sha256Hex("seed-" + k.StreamName)
	}
	if k.RequestID == "" {
		k.RequestID = "seed-req-" + k.StreamName
	}
	if k.KeyRef == "" {
		ref, err := keyRefFor(k.StreamName, k.Version)
		if err != nil {
			t.Fatalf("seed key_ref：%v", err)
		}
		k.KeyRef = ref
	}
	id, err := e.repo.StreamKey.Insert(bg(), nil, k)
	if err != nil {
		t.Fatalf("seed 密钥 %s：%v", k.StreamName, err)
	}
	row, err := e.repo.StreamKey.FindOne(bg(), id)
	if err != nil || row == nil {
		t.Fatalf("seed 密钥回读：%v", err)
	}
	return row
}

// seedStream 直接落一条流（按指定状态与 seq），用于测状态机而不必从签发开始铺路。
func (e *testEnv) seedStream(t *testing.T, s *model.Stream) *model.Stream {
	t.Helper()
	if s.StreamID == "" {
		s.StreamID = "STREAM" + fmt.Sprintf("%020d", len(e.db.streams)+1)
	}
	if s.PublishRequestID == "" {
		s.PublishRequestID = "seed-pub-" + s.StreamID
	}
	if s.HealthState == 0 {
		s.HealthState = model.HealthStateNoData
	}
	if s.State == 0 {
		s.State = model.StreamStateIdle
	}
	if err := e.repo.Stream.Insert(bg(), nil, s); err != nil {
		t.Fatalf("seed 流 %s：%v", s.StreamID, err)
	}
	// seq / state 是 CAS 的目标位，Insert 只落 IDLE/0，这里按用例意图对齐。
	row := e.db.streams[s.StreamID]
	row.State = s.State
	row.Seq = s.Seq
	if s.NodeID != "" {
		row.NodeID = s.NodeID
	}
	if s.KeyID > 0 {
		if k := e.db.keys[s.KeyID]; k != nil && k.CurrentStreamID == "" {
			k.CurrentStreamID = row.StreamID
		}
	}
	cp := *row
	return &cp
}

// seedNode 直接落一个接入节点（注册路径之外还需要「带占用量的节点」，故不走 Upsert）。
func (e *testEnv) seedNode(t *testing.T, n *model.IngestNode) *model.IngestNode {
	t.Helper()
	if n.NodeID == "" {
		t.Fatalf("seed 节点必须有 node_id")
	}
	if n.State == 0 {
		n.State = model.NodeStateOnline
	}
	cp := *n
	e.db.nodes[cp.NodeID] = &cp
	return cloneNode(&cp)
}

func cloneNode(p *model.IngestNode) *model.IngestNode {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

// nodeRow 回库里当前值（副本）：断言「配额没被改动」必须读副本，
// 否则用例改指针等于改库，断言就成了自我实现的假命题。
func (e *testEnv) nodeRow(t *testing.T, nodeID string) *model.IngestNode {
	t.Helper()
	row, err := e.repo.IngestNode.FindOne(bg(), nodeID)
	if err != nil || row == nil {
		t.Fatalf("库里没有节点 %s", nodeID)
	}
	return row
}

func (e *testEnv) streamRow(t *testing.T, streamID string) *model.Stream {
	t.Helper()
	row, err := e.repo.Stream.FindOne(bg(), streamID)
	if err != nil || row == nil {
		t.Fatalf("库里没有流 %s", streamID)
	}
	return row
}

func (e *testEnv) keyRow(t *testing.T, keyID int64) *model.StreamKey {
	t.Helper()
	row, err := e.repo.StreamKey.FindOne(bg(), keyID)
	if err != nil || row == nil {
		t.Fatalf("库里没有密钥 %d", keyID)
	}
	return row
}

// seedAssignedNode 造节点 + 一条生效租约 + 配额占用，用于释放/迁移类用例。
// 配额走真 ReserveQuota 写路径而不是手工 +1：种子也要经过 CAS，
// 否则「容量满还能再占一个」这类缺陷会被种子数据自己掩盖掉。
func (e *testEnv) seedAssignedNode(t *testing.T, s *model.Stream, node *model.IngestNode, requestID string) *model.NodeAssignment {
	t.Helper()
	stream := e.seedStream(t, s)
	n := e.seedNode(t, node)
	ok, err := e.repo.IngestNode.ReserveQuota(bg(), nil, n.NodeID)
	if err != nil || !ok {
		t.Fatalf("seed 占配额 %s：ok=%v err=%v", n.NodeID, ok, err)
	}
	e.db.streams[stream.StreamID].NodeID = n.NodeID
	id, err := e.repo.NodeAssignment.Insert(bg(), nil, &model.NodeAssignment{
		RequestID: requestID, StreamID: stream.StreamID, RoomID: stream.RoomID, KeyID: stream.KeyID,
		NodeID: n.NodeID, Protocol: stream.Protocol, State: model.AssignmentStateActive, AssignedAt: 1700000100,
	})
	if err != nil {
		t.Fatalf("seed 分配：%v", err)
	}
	row, err := e.repo.NodeAssignment.FindOne(bg(), id)
	if err != nil || row == nil {
		t.Fatalf("seed 分配回读：%v", err)
	}
	return row
}

// seedOutboxRow 直接落一行 outbox（发布器状态机不在本服务实现内，只能照此造现场）。
func (e *testEnv) seedOutboxRow(t *testing.T, o *model.EventOutbox) *model.EventOutbox {
	t.Helper()
	if o.EventID == "" {
		o.EventID = "EVT" + fmt.Sprintf("%04d", len(e.db.outbox)+1)
	}
	if err := e.repo.Outbox.Insert(bg(), nil, o); err != nil {
		t.Fatalf("seed outbox %s：%v", o.EventID, err)
	}
	cp := *e.db.outbox[e.nextOutboxIDFor(o.EventID)]
	return &cp
}

func (e *testEnv) nextOutboxIDFor(eventID string) int64 {
	for id, row := range e.db.outbox {
		if row.EventID == eventID {
			return id
		}
	}
	return 0
}

// seedInterruption 直接落一条断流区间（走 model 写路径，episode_no 与主键都由写路径分配）。
//
// EpisodeNo 必须显式给：model 的 List/Count 都带恒真条件 episode_no <> 0，
// 种成 0 的行会「存在于库里但永远查不出来」，用例于是测的是替身而不是 SQL。
func (e *testEnv) seedInterruption(t *testing.T, in *model.StreamInterruption) *model.StreamInterruption {
	t.Helper()
	if in.StreamID == "" {
		t.Fatalf("seed 断流区间必须有 stream_id")
	}
	if in.EpisodeNo == 0 {
		t.Fatalf("seed 断流区间必须显式给 episode_no（恒真条件 episode_no <> 0 会挡掉 0 值行）")
	}
	if in.StartedAt == 0 {
		in.StartedAt = 1700000200
	}
	id, err := e.repo.StreamInterruption.Insert(bg(), nil, in)
	if err != nil {
		t.Fatalf("seed 断流区间 %s：%v", in.StreamID, err)
	}
	cp := *e.db.interrupt[id]
	return &cp
}

// seedFailedOutbox 造一行「发布器已判失败」的 outbox（发布器未接线，只能照此造现场）。
// 与 seedOutboxRow 分开是因为它必须带 retry_count/next_retry_at 这些退避列，
// 用同一份种子会把「重试资格只看 state」这条契约讲不清。
func (e *testEnv) seedFailedOutbox(t *testing.T, eventID string, occurredAt int64, retryCount int32, nextRetryAt int64, lastError string) *model.EventOutbox {
	t.Helper()
	return e.seedOutboxRow(t, &model.EventOutbox{
		EventID: eventID, EventType: model.EventTypeStreamState, SchemaVersion: model.SchemaVersionStreamState,
		AggregateType: model.AggregateTypeStream, AggregateID: "S-" + eventID, StreamID: "S-" + eventID,
		Seq: int64(retryCount) + 1, State: model.OutboxStateFailed, OccurredAt: occurredAt,
		RetryCount: retryCount, NextRetryAt: nextRetryAt, LastError: lastError,
		Payload: `{"event_id":"` + eventID + `"}`,
	})
}

// outboxRow 回库里当前 outbox 行（副本）：断言「重置只改 state 不改 payload」必须读副本。
func (e *testEnv) outboxRow(t *testing.T, eventID string) *model.EventOutbox {
	t.Helper()
	id := e.nextOutboxIDFor(eventID)
	row := e.db.outbox[id]
	if row == nil {
		t.Fatalf("库里没有 outbox 事件 %s", eventID)
	}
	cp := *row
	return &cp
}

// eventRow 回库里某流某 seq 的事件行（副本）。
func (e *testEnv) eventRow(t *testing.T, streamID string, seq int64) *model.StreamEvent {
	t.Helper()
	row, err := e.repo.StreamEvent.FindByStreamSeq(bg(), streamID, seq)
	if err != nil || row == nil {
		t.Fatalf("库里没有事件 %s seq=%d：%v", streamID, seq, err)
	}
	cp := *row
	return &cp
}

// assignmentRow 按 request_id 回一条分配记录（副本），用于断言释放写下的列。
func (e *testEnv) assignmentRow(t *testing.T, requestID string) *model.NodeAssignment {
	t.Helper()
	row, err := e.repo.NodeAssignment.FindByRequest(bg(), requestID)
	if err != nil || row == nil {
		t.Fatalf("库里没有分配记录 %s：%v", requestID, err)
	}
	cp := *row
	return &cp
}

// --- 快捷调用（一律走真 logic，不打桩用例） ---

func (e *testEnv) report(t *testing.T, in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error) {
	t.Helper()
	return NewReportStreamStateLogic(bg(), e.svc).ReportStreamState(in)
}

func (e *testEnv) mustReport(t *testing.T, in *rpc.ReportStreamStateReq) *rpc.ReportStreamStateReply {
	t.Helper()
	reply, err := e.report(t, in)
	if err != nil {
		t.Fatalf("ReportStreamState(%s→%v)：%v", in.StreamId, in.State, err)
	}
	return reply
}

func (e *testEnv) verifyPublish(t *testing.T, in *rpc.VerifyPublishAuthReq) (*rpc.VerifyPublishAuthReply, error) {
	t.Helper()
	return NewVerifyPublishAuthLogic(bg(), e.svc).VerifyPublishAuth(in)
}

func (e *testEnv) verifyCallback(t *testing.T, in *rpc.VerifyCdnCallbackReq) (*rpc.VerifyCdnCallbackReply, error) {
	t.Helper()
	return NewVerifyCdnCallbackLogic(bg(), e.svc).VerifyCdnCallback(in)
}

// closeStream 强制停流。默认给「运营强制关」形态（admin=true），
// 用例用 mut 改成主播自关或不带 admin 的越权形态。
func (e *testEnv) closeStream(t *testing.T, streamID, requestID string,
	mut ...func(*rpc.CloseStreamReq)) (*rpc.CloseStreamReply, error) {
	t.Helper()
	in := &rpc.CloseStreamReq{
		StreamId: streamID, RequestId: requestID, OperatorMid: 9001, Admin: true, Reason: "运营切断",
	}
	for _, m := range mut {
		m(in)
	}
	return NewCloseStreamLogic(bg(), e.svc).CloseStream(in)
}

func (e *testEnv) mustCloseStream(t *testing.T, streamID, requestID string,
	mut ...func(*rpc.CloseStreamReq)) *rpc.CloseStreamReply {
	t.Helper()
	reply, err := e.closeStream(t, streamID, requestID, mut...)
	if err != nil {
		t.Fatalf("CloseStream(%s)：%v", streamID, err)
	}
	return reply
}

// revokeKey 吊销密钥。默认给「运营吊销且不级联停流」形态。
func (e *testEnv) revokeKey(t *testing.T, keyID int64, requestID string,
	mut ...func(*rpc.RevokeStreamKeyReq)) (*rpc.RevokeStreamKeyReply, error) {
	t.Helper()
	in := &rpc.RevokeStreamKeyReq{
		KeyId: keyID, RequestId: requestID, OperatorMid: 9001, Admin: true, Reason: "密钥泄露",
	}
	for _, m := range mut {
		m(in)
	}
	return NewRevokeStreamKeyLogic(bg(), e.svc).RevokeStreamKey(in)
}

func (e *testEnv) mustRevokeKey(t *testing.T, keyID int64, requestID string,
	mut ...func(*rpc.RevokeStreamKeyReq)) *rpc.RevokeStreamKeyReply {
	t.Helper()
	reply, err := e.revokeKey(t, keyID, requestID, mut...)
	if err != nil {
		t.Fatalf("RevokeStreamKey(%d)：%v", keyID, err)
	}
	return reply
}

func (e *testEnv) listNodes(t *testing.T, mut ...func(*rpc.ListIngestNodesReq)) (*rpc.ListIngestNodesReply, error) {
	t.Helper()
	in := &rpc.ListIngestNodesReq{OperatorMid: 9001}
	for _, m := range mut {
		m(in)
	}
	return NewListIngestNodesLogic(bg(), e.svc).ListIngestNodes(in)
}

func (e *testEnv) mustListNodes(t *testing.T, mut ...func(*rpc.ListIngestNodesReq)) *rpc.ListIngestNodesReply {
	t.Helper()
	reply, err := e.listNodes(t, mut...)
	if err != nil {
		t.Fatalf("ListIngestNodes：%v", err)
	}
	return reply
}

func (e *testEnv) listAssigns(t *testing.T, mut ...func(*rpc.ListNodeAssignmentsReq)) (*rpc.ListNodeAssignmentsReply, error) {
	t.Helper()
	in := &rpc.ListNodeAssignmentsReq{OperatorMid: 9001}
	for _, m := range mut {
		m(in)
	}
	return NewListNodeAssignmentsLogic(bg(), e.svc).ListNodeAssignments(in)
}

func (e *testEnv) mustListAssigns(t *testing.T, mut ...func(*rpc.ListNodeAssignmentsReq)) *rpc.ListNodeAssignmentsReply {
	t.Helper()
	reply, err := e.listAssigns(t, mut...)
	if err != nil {
		t.Fatalf("ListNodeAssignments：%v", err)
	}
	return reply
}

func (e *testEnv) listInterrupts(t *testing.T, mut ...func(*rpc.ListStreamInterruptionsReq)) (*rpc.ListStreamInterruptionsReply, error) {
	t.Helper()
	in := &rpc.ListStreamInterruptionsReq{}
	for _, m := range mut {
		m(in)
	}
	return NewListStreamInterruptionsLogic(bg(), e.svc).ListStreamInterruptions(in)
}

func (e *testEnv) mustListInterrupts(t *testing.T, mut ...func(*rpc.ListStreamInterruptionsReq)) *rpc.ListStreamInterruptionsReply {
	t.Helper()
	reply, err := e.listInterrupts(t, mut...)
	if err != nil {
		t.Fatalf("ListStreamInterruptions：%v", err)
	}
	return reply
}

func (e *testEnv) checkpoint(t *testing.T, mut ...func(*rpc.GetEventPublishCheckpointReq)) (*rpc.GetEventPublishCheckpointReply, error) {
	t.Helper()
	in := &rpc.GetEventPublishCheckpointReq{}
	for _, m := range mut {
		m(in)
	}
	return NewGetEventPublishCheckpointLogic(bg(), e.svc).GetEventPublishCheckpoint(in)
}

func (e *testEnv) mustCheckpoint(t *testing.T, mut ...func(*rpc.GetEventPublishCheckpointReq)) *rpc.EventPublishCheckpoint {
	t.Helper()
	reply, err := e.checkpoint(t, mut...)
	if err != nil {
		t.Fatalf("GetEventPublishCheckpoint：%v", err)
	}
	if reply.GetCheckpoint() == nil {
		t.Fatalf("GetEventPublishCheckpoint 回了空位点（零值位点等于「观测不到滞后」，比报错更危险）")
	}
	return reply.GetCheckpoint()
}

func (e *testEnv) retry(t *testing.T, requestID string, mut ...func(*rpc.RetryFailedEventsReq)) (*rpc.RetryFailedEventsReply, error) {
	t.Helper()
	in := &rpc.RetryFailedEventsReq{
		RequestId: requestID, OperatorMid: 9001, Reason: "上游恢复，人工放行",
	}
	for _, m := range mut {
		m(in)
	}
	return NewRetryFailedEventsLogic(bg(), e.svc).RetryFailedEvents(in)
}

func (e *testEnv) mustRetry(t *testing.T, requestID string, mut ...func(*rpc.RetryFailedEventsReq)) *rpc.RetryFailedEventsReply {
	t.Helper()
	reply, err := e.retry(t, requestID, mut...)
	if err != nil {
		t.Fatalf("RetryFailedEvents(%s)：%v", requestID, err)
	}
	return reply
}

// --- 序列投影（断言顺序时避免把整行结构塞进 Fatalf） ---

func interruptionIDs(rows []*rpc.StreamInterruptionInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetInterruptionId())
	}
	return out
}

func assignmentIDs(rows []*rpc.NodeAssignmentInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetAssignmentId())
	}
	return out
}

func replyNodeIDs(rows []*rpc.IngestNodeInfo) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetNodeId())
	}
	return out
}
