package consumer

// queue_test.go 覆盖「配置校验 + 消费者生命周期」，全程不接触任何 broker。
//
// 三条必须能用断言证明的事：
//  1. 配置少写一个键时错误里点名了那个键。静默不消费比启动失败更难查：
//     断流事件进不来，档位就一直挂在已断的流上对外分发。
//  2. 订阅面只能有 live.state.v1。本服务那 9 个 livemedia.* topic 是**出站**的，
//     订阅它们等于把自己刚发的下线事件读回来（配合发布循环就是自激回环）。
//  3. Start 半途失败必须回滚已建好的消费者，且 started 只在全部就绪后才置位，
//     否则进程会带着「已在消费」的假象跑在没有消费者的状态上。
//
// 注意：NewSupervisor 走 ValidateKafka，真实配置最多只有一个 topic，
// 所以多 topic 的逐条回滚分支只能由本包内手工装配 Supervisor 覆盖（见下面两处注释）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/zrpc"

	"go-video/services/live-media/internal/config"
)

// validKafka 返回一份「本包认可」的完整消费配置（结构体字面量不会应用 tag 默认值，逐字段写全）。
func validKafka() config.KafkaConf {
	return config.KafkaConf{
		Enabled:         true,
		Brokers:         []string{"127.0.0.1:9092"},
		Group:           "live-media.v1",
		SubscribeTopics: []string{SupportedTopic},
		MaxRetries:      5,
		Offset:          "last",
		Conns:           1,
		Consumers:       2,
		Processors:      4,
	}
}

func validConfig() config.Config {
	return config.Config{
		RpcServerConf: zrpc.RpcServerConf{
			ListenOn: "0.0.0.0:8120",
			ServiceConf: service.ServiceConf{
				Name: "livemedia.v1.rpc",
				Log:  logx.LogConf{Mode: "console", Level: "info"},
				Mode: "dev",
			},
		},
		Kafka: validKafka(),
	}
}

// countApplicator 永远返回「下了 2 个档位」，让生命周期用例只关注队列本身。
func countApplicator() Applicator {
	return ApplicatorFunc(func(context.Context, *OfflineCommand) (*OfflineResult, error) {
		return &OfflineResult{Affected: 2, Scanned: 2}, nil
	})
}

// fakeQueue 记录 Start/Stop 次数，并把动作写进共享的 order/done 以便断言关闭与回滚顺序。
type fakeQueue struct {
	mu     sync.Mutex
	topic  string
	starts int
	stops  int
	order  *[]string
	done   *[]string
}

func (q *fakeQueue) Start() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.starts++
	if q.order != nil {
		*q.order = append(*q.order, "start:"+q.topic)
	}
}

func (q *fakeQueue) Stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stops++
	if q.order != nil {
		*q.order = append(*q.order, "stop:"+q.topic)
	}
	if q.done != nil {
		*q.done = append(*q.done, q.topic)
	}
}

func (q *fakeQueue) counts() (int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.starts, q.stops
}

type factoryCall struct {
	settings Settings
	topic    string
	handler  *Handler
}

// fakeFactory 是 QueueFactory 的替身：可按 topic 注入错误、返回 nil 队列，或记录入参。
type fakeFactory struct {
	mu     sync.Mutex
	calls  []factoryCall
	queues []*fakeQueue
	failOn map[string]error
	nilOn  map[string]bool
	order  *[]string
	done   *[]string
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{failOn: map[string]error{}, nilOn: map[string]bool{}}
}

func (f *fakeFactory) New(s Settings, topic string, h *Handler) (MessageQueue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, factoryCall{settings: s, topic: topic, handler: h})
	if err, bad := f.failOn[topic]; bad {
		return nil, err
	}
	if f.nilOn[topic] {
		return nil, nil
	}
	q := &fakeQueue{topic: topic, order: f.order, done: f.done}
	f.queues = append(f.queues, q)
	return q, nil
}

func (f *fakeFactory) snapshotCalls() []factoryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factoryCall(nil), f.calls...)
}

func (f *fakeFactory) snapshotQueues() []*fakeQueue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeQueue(nil), f.queues...)
}

// TestValidateKafkaRejectsEveryIncompleteKey 逐字段破坏配置，断言错误里点名那一个键。
// 「错误只有 Kafka 配置不完整」这种笼统文案不算通过：值班照文案改不了现场。
func TestValidateKafkaRejectsEveryIncompleteKey(t *testing.T) {
	if err := ValidateKafka(validKafka()); err != nil {
		t.Fatalf("完整配置不应报错: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(k *config.KafkaConf)
		want   string
	}{
		{"缺 brokers", func(k *config.KafkaConf) { k.Brokers = nil }, "Kafka.Brokers"},
		{"brokers 全是空白串也算没配", func(k *config.KafkaConf) { k.Brokers = []string{"  "} }, "Kafka.Brokers"},
		{"缺 group", func(k *config.KafkaConf) { k.Group = "  " }, "Kafka.Group"},
		{"没订阅 topic", func(k *config.KafkaConf) { k.SubscribeTopics = nil }, "Kafka.SubscribeTopics"},
		{"订阅自己出的 topic 会组成回环", func(k *config.KafkaConf) {
			k.SubscribeTopics = []string{"livemedia.stream.output.offline.v1"}
		}, "livemedia.stream.output.offline.v1"},
		{"订阅别人还没生产者的 topic", func(k *config.KafkaConf) {
			k.SubscribeTopics = []string{"content.published.v1"}
		}, "content.published.v1"},
		{"订阅串带空白仍要认出来", func(k *config.KafkaConf) {
			k.SubscribeTopics = []string{" live.state.v1 ", "livemedia.stream.output.online.v1"}
		}, "livemedia.stream.output.online.v1"},
		{"订阅串大小写不对", func(k *config.KafkaConf) {
			k.SubscribeTopics = []string{"Live.State.v1"}
		}, "Live.State.v1"},
		{"offset 非法", func(k *config.KafkaConf) { k.Offset = "earliest" }, "Kafka.Offset"},
		{"conns 为 0", func(k *config.KafkaConf) { k.Conns = 0 }, "Kafka.Conns"},
		{"consumers 为负", func(k *config.KafkaConf) { k.Consumers = -1 }, "Kafka.Consumers"},
		{"processors 为 0", func(k *config.KafkaConf) { k.Processors = 0 }, "Kafka.Processors"},
		{"没有尝试上限", func(k *config.KafkaConf) { k.MaxRetries = 0 }, "Kafka.MaxRetries"},
		{"只给 username", func(k *config.KafkaConf) { k.Username = "app" }, "Username"},
		{"只给 password", func(k *config.KafkaConf) { k.Password = "secret" }, "Password"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := validKafka()
			tc.mutate(&k)
			err := ValidateKafka(k)
			if err == nil {
				t.Fatalf("非法配置必须拒绝（否则进程会静默不消费）: %+v", k)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应点名 %q，实得: %v", tc.want, err)
			}
			if !strings.HasPrefix(err.Error(), "live-media: Kafka 消费配置不完整: ") {
				t.Fatalf("错误必须带统一前缀，实得: %v", err)
			}
		})
	}
}

// TestValidateKafkaRequiresReadableCaFile 是 go-queue 的实现细节防线：
// kq 读到不存在的 CaFile 会 log.Fatal 直接打死进程（没有可诊断的错误返回），
// 所以这里必须提前以普通 error 拒绝。
func TestValidateKafkaRequiresReadableCaFile(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ok, []byte("-----BEGIN CERTIFICATE-----\nplaceholder\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	k := validKafka()
	k.CaFile = ok
	if err := ValidateKafka(k); err != nil {
		t.Fatalf("CaFile 可读时应通过: %v", err)
	}

	k.CaFile = filepath.Join(dir, "missing.pem")
	err := ValidateKafka(k)
	if err == nil || !strings.Contains(err.Error(), "Kafka.CaFile=") {
		t.Fatalf("不可读的 CaFile 必须点名 Kafka.CaFile（否则 kq 会 log.Fatal）: %v", err)
	}

	// 口令与 CA 只允许由环境注入：校验函数绝不打印它们的值。
	k = validKafka()
	k.Username = "svc-live-media"
	k.Password = "p@ssword-must-not-leak"
	k.CaFile = filepath.Join(dir, "nope.pem")
	if msg := ValidateKafka(k).Error(); strings.Contains(msg, "p@ssword-must-not-leak") ||
		strings.Contains(msg, "svc-live-media") {
		t.Fatalf("校验错误里不得回显凭据: %s", msg)
	}
}

func TestEffectiveTopicsTrimsDedupesAndKeepsOrder(t *testing.T) {
	k := config.KafkaConf{SubscribeTopics: []string{" live.state.v1 ", "", "  ", "live.state.v1"}}
	if got := EffectiveTopics(k); !reflect.DeepEqual(got, []string{"live.state.v1"}) {
		t.Fatalf("订阅列表推导不符: %v", got)
	}
	if got := EffectiveTopics(config.KafkaConf{}); len(got) != 0 {
		t.Fatalf("未配置时必须是空列表而不是 [\"\"]: %v", got)
	}
}

// TestSettingsFromCarriesLoggerIdentity 钉住 Name/Log/Mode 的透传：
// kq.NewQueue 会调用 ServiceConf.SetUp，这三项留空会让它用默认值重设整个进程的全局 logger。
func TestSettingsFromCarriesLoggerIdentity(t *testing.T) {
	c := validConfig()
	c.Kafka.ForceCommit = true
	c.Kafka.Username = "svc"
	c.Kafka.Password = "from-secret"
	c.Kafka.CaFile = "ca.pem"
	c.Kafka.Brokers = []string{"127.0.0.1:19092", "127.0.0.1:29092"}

	s := SettingsFrom(c)
	if s.Name != "livemedia.v1.rpc" || s.Mode != "dev" || s.Log.Mode != "console" {
		t.Fatalf("Name/Mode/Log 未透传，kq 会重设全局 logger: %+v", s)
	}
	if !reflect.DeepEqual(s.Brokers, []string{"127.0.0.1:19092", "127.0.0.1:29092"}) {
		t.Fatalf("Brokers 不符: %v", s.Brokers)
	}
	if s.Group != "live-media.v1" || s.Offset != "last" || s.Conns != 1 || s.Consumers != 2 ||
		s.Processors != 4 || !s.ForceCommit || s.Username != "svc" || s.Password != "from-secret" ||
		s.CaFile != "ca.pem" {
		t.Fatalf("队列参数与配置不一致: %+v", s)
	}
}

func TestNewSupervisorNilGuards(t *testing.T) {
	c := validConfig()
	f := newFakeFactory()
	app := countApplicator()

	if _, err := NewSupervisor(c, nil, f); err == nil || !strings.Contains(err.Error(), "applicator is required") {
		t.Fatalf("nil applicator 必须拒绝: %v", err)
	}
	if _, err := NewSupervisor(c, app, nil); err == nil || !strings.Contains(err.Error(), "queue factory is required") {
		t.Fatalf("nil factory 必须拒绝，否则消费者永远建不出来: %v", err)
	}
	// 配置不完整同样要报错，而不是返回一个空 supervisor 让调用方以为已就绪。
	bad := validConfig()
	bad.Kafka.Brokers = nil
	if _, err := NewSupervisor(bad, app, f); err == nil || !strings.Contains(err.Error(), "Kafka.Brokers") {
		t.Fatalf("配置不完整必须拒绝: %v", err)
	}
}

func TestSupervisorStartStopLifecycle(t *testing.T) {
	var order []string
	f := newFakeFactory()
	f.order = &order
	f.done = &order
	c := validConfig()

	sup, err := NewSupervisor(c, countApplicator(), f)
	if err != nil {
		t.Fatal(err)
	}
	if sup.Started() {
		t.Fatal("构造完成不等于已在消费")
	}
	if !reflect.DeepEqual(sup.Topics(), []string{SupportedTopic}) {
		t.Fatalf("Topics 不符: %v", sup.Topics())
	}
	if got := sup.Handler().Options(); got.MaxAttempts != c.Kafka.MaxRetries {
		t.Fatalf("Handler 没有使用 Kafka.MaxRetries 作为尝试上限: %+v", got)
	}

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if !sup.Started() {
		t.Fatal("Start 成功后必须标记已启动")
	}
	calls := f.snapshotCalls()
	if len(calls) != 1 || calls[0].topic != SupportedTopic {
		t.Fatalf("应为每个 topic 建一个消费者: %+v", calls)
	}
	if !reflect.DeepEqual(calls[0].settings, SettingsFrom(c)) {
		t.Fatalf("传给工厂的 Settings 与配置推导不一致:\n got=%+v\nwant=%+v", calls[0].settings, SettingsFrom(c))
	}
	if calls[0].handler != sup.Handler() {
		t.Fatal("工厂必须拿到 supervisor 的 Handler，否则计数与重试台账会各有一份")
	}

	if err := sup.Start(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "already started") {
		t.Fatalf("重复 Start 必须拒绝（两个消费者抢同一组分区）: %v", err)
	}
	if len(f.snapshotCalls()) != 1 {
		t.Fatalf("重复 Start 不应再建消费者: %d", len(f.snapshotCalls()))
	}

	sup.Stop()
	if sup.Started() {
		t.Fatal("Stop 后不得声称已启动")
	}
	qs := f.snapshotQueues()
	if len(qs) != 1 {
		t.Fatalf("队列数不符: %d", len(qs))
	}
	if starts, stops := qs[0].counts(); starts != 1 || stops != 1 {
		t.Fatalf("Start/Stop 应各一次，实得 start=%d stop=%d", starts, stops)
	}
	sup.Stop() // 重复 Stop 不应 panic 或再关一次
	if _, stops := qs[0].counts(); stops != 1 {
		t.Fatalf("重复 Stop 不应再触发队列 Stop: %d", stops)
	}
}

// TestSupervisorRollsBackPartialStart 多 topic 的回滚分支：
// 真实配置经 ValidateKafka 只允许一个 topic，所以这里在本包内手工装配 Supervisor，
// 覆盖「第二个 topic 建立失败时已建好的第一个必须关掉」。
func TestSupervisorRollsBackPartialStart(t *testing.T) {
	var order, done []string
	f := newFakeFactory()
	f.order = &order
	f.done = &done
	f.failOn["live.state.v2"] = errors.New("broker 不支持该 topic")

	sup := &Supervisor{
		settings: SettingsFrom(validConfig()),
		topics:   []string{SupportedTopic, "live.state.v2"},
		factory:  f,
		handler:  NewHandler(countApplicator(), OptionsFrom(validKafka())),
	}
	err := sup.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "create consumer for topic live.state.v2") {
		t.Fatalf("应报出失败的 topic: %v", err)
	}
	if sup.Started() {
		t.Fatal("半途失败不得置位 started")
	}
	if !reflect.DeepEqual(done, []string{SupportedTopic}) {
		t.Fatalf("已建好的消费者必须回滚关闭，实得关闭序列 %v", done)
	}
	// 回滚后允许重新 Start：队列列表已被清空，不会重复关闭。
	f.failOn = map[string]error{}
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("修好配置后应能重新启动: %v", err)
	}
	sup.Stop()
	qs := f.snapshotQueues()
	// 第一次 Start 建成 1 个（随即回滚），第二次建成 2 个：三个队列各自只能被关闭一次，
	// 多关一次就说明回滚列表没重置。
	if len(qs) != 3 {
		t.Fatalf("应共建立 3 个队列（1 次回滚 + 2 个正常）: %d", len(qs))
	}
	for i, q := range qs {
		if _, stops := q.counts(); stops != 1 {
			t.Fatalf("队列 %d 被关闭 %d 次（回滚列表必须重置）", i, stops)
		}
	}
	// 关闭顺序必须逆序。
	if order[len(order)-1] != "stop:"+SupportedTopic {
		t.Fatalf("Stop 应逆序，最后关的是最先建的消费者，实得序列 %v", order)
	}
}

// TestSupervisorRejectsNilQueue 工厂返回 (nil, nil) 时不能当成成功：
// 那样 supervisor 会宣称已启动，却没有任何东西在消费。
func TestSupervisorRejectsNilQueue(t *testing.T) {
	var done []string
	f := newFakeFactory()
	f.done = &done
	f.nilOn[SupportedTopic] = true

	sup, err := NewSupervisor(validConfig(), countApplicator(), f)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "nil consumer for topic "+SupportedTopic) {
		t.Fatalf("nil 队列必须报错: %v", err)
	}
	if sup.Started() {
		t.Fatal("没有真实队列时不得置位 started")
	}
	sup.Stop() // 不应 panic
}
