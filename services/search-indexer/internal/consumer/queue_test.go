// queue_test.go 用假工厂与假 Store 驱动 Kafka 接入抽象，不连 broker、不连 MySQL
// （AGENTS.md §9：替身必须按用例返回真实错误，不允许「永不失败」）。
//
// 注意这批用例钉的是**接口契约**：回调映射、位点是否提交（返回值）、启停与回滚顺序、
// 配置校验的点名能力。它不能证明任何真实 Kafka 投递语义，见 README「已知缺口」1。
package consumer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/search-indexer/internal/config"
)

// validKafkaConf 返回一份完整的消费配置，用例按需改坏单个字段。
func validKafkaConf() config.KafkaConf {
	return config.KafkaConf{
		Enabled:            true,
		Brokers:            []string{"127.0.0.1:9092"},
		Group:              "search-indexer.v1",
		Topics:             []string{"content.published.v1", "engagement.action.v1"},
		Offset:             "last",
		Conns:              1,
		Consumers:          2,
		Processors:         4,
		MaxRetries:         5,
		InProcessAttempts:  2,
		RetryBackoffSec:    5,
		MaxRetryBackoffSec: 1800,
	}
}

// validConfig 返回可消费的完整 Config（Name/Mode 走内嵌 ServiceConf）。
func validConfig() config.Config {
	c := config.Config{Kafka: validKafkaConf()}
	c.Name = "search-indexer.v1"
	c.Mode = "file"
	c.Log = logx.LogConf{ServiceName: "search-indexer.v1", Mode: "file"}
	return c
}

// fakeQueue 记录启停次数与顺序。
type fakeQueue struct {
	topic   string
	events  *[]string
	started int
	stopped int
}

func (f *fakeQueue) Start() {
	f.started++
	*f.events = append(*f.events, "start:"+f.topic)
}

func (f *fakeQueue) Stop() {
	f.stopped++
	*f.events = append(*f.events, "stop:"+f.topic)
}

// fakeFactory 按 topic 返回替身消费者，并可对单个 topic 注入创建错误。
type fakeFactory struct {
	events   []string
	created  []*fakeQueue
	errFor   map[string]error
	lastSett Settings
	nilQueue bool
}

func (f *fakeFactory) New(s Settings, topic string, h *Handler) (MessageQueue, error) {
	f.lastSett = s
	if err := f.errFor[topic]; err != nil {
		return nil, err
	}
	if f.nilQueue {
		return nil, nil
	}
	q := &fakeQueue{topic: topic, events: &f.events}
	if h == nil {
		return nil, errors.New("fake factory requires a handler")
	}
	f.created = append(f.created, q)
	return q, nil
}

var _ QueueFactory = (*fakeFactory)(nil)

func TestValidateKafkaRejectsEachIncompleteField(t *testing.T) {
	if err := ValidateKafka(validKafkaConf()); err != nil {
		t.Fatalf("完整配置应通过校验: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*config.KafkaConf)
		want   string
	}{
		{"Brokers", func(k *config.KafkaConf) { k.Brokers = nil }, "Kafka.Brokers"},
		{"Group", func(k *config.KafkaConf) { k.Group = "  " }, "Kafka.Group"},
		{"Topics", func(k *config.KafkaConf) { k.Topics = nil }, "Kafka.Topics"},
		{"Offset", func(k *config.KafkaConf) { k.Offset = "earliest" }, "Kafka.Offset"},
		{"Conns", func(k *config.KafkaConf) { k.Conns = 0 }, "Kafka.Conns"},
		{"Consumers", func(k *config.KafkaConf) { k.Consumers = -1 }, "Kafka.Consumers"},
		{"Processors", func(k *config.KafkaConf) { k.Processors = 0 }, "Kafka.Processors"},
		{"MaxRetries", func(k *config.KafkaConf) { k.MaxRetries = 0 }, "Kafka.MaxRetries"},
		{"InProcessAttempts", func(k *config.KafkaConf) { k.InProcessAttempts = 0 }, "Kafka.InProcessAttempts"},
		{"RetryBackoffSec", func(k *config.KafkaConf) { k.RetryBackoffSec = 0 }, "Kafka.RetryBackoffSec"},
		{"MaxRetryBackoffSec", func(k *config.KafkaConf) { k.MaxRetryBackoffSec = 0 }, "Kafka.MaxRetryBackoffSec"},
		// 只配 Username 不配 Password（或反之）通常是漏配，必须点名而不是默默用匿名连接。
		{"SASL 半配", func(k *config.KafkaConf) { k.Username = "app" }, "Kafka.Username"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := validKafkaConf()
			tc.mutate(&k)
			err := ValidateKafka(k)
			if err == nil {
				t.Fatalf("缺 %s 时必须报错", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误必须点名配置键 %q，实际: %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "Kafka 消费配置不完整") {
				t.Fatalf("错误前缀应统一便于运维检索，实际: %v", err)
			}
		})
	}
}

func TestSettingsFromMapsEveryField(t *testing.T) {
	c := validConfig()
	c.Kafka.ForceCommit = true
	c.Kafka.Username = "svc"
	c.Kafka.Password = "from-secret"
	c.Kafka.CaFile = "/etc/tls/ca.pem"

	s := SettingsFrom(c)
	if s.Name != c.Name || s.Mode != c.Mode || s.Log.Mode != c.Log.Mode {
		t.Fatalf("日志三元组必须同源（否则 kq 会重设全进程 logger）: %+v", s)
	}
	if strings.Join(s.Brokers, ",") != strings.Join(c.Kafka.Brokers, ",") ||
		s.Group != c.Kafka.Group || s.Offset != c.Kafka.Offset ||
		s.Conns != c.Kafka.Conns || s.Consumers != c.Kafka.Consumers ||
		s.Processors != c.Kafka.Processors || s.ForceCommit != c.Kafka.ForceCommit ||
		s.Username != c.Kafka.Username || s.Password != c.Kafka.Password || s.CaFile != c.Kafka.CaFile {
		t.Fatalf("Settings 漏映射字段: %+v", s)
	}
}

// TestHandlerConsumeMapsCallbackAndDrivesStateMachine 钉住 kq 回调 → Message 的映射：
// topic 来自 Handler 绑定，key/value 原样传递，partition/offset 保持 0 而不是伪造位点。
func TestHandlerConsumeMapsCallbackAndDrivesStateMachine(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler("content.published.v1", New(store, fastOptions()))
	if h.Topic() != "content.published.v1" {
		t.Fatalf("Topic() 应返回绑定的 topic，实际 %s", h.Topic())
	}
	value := string(envelopeBytes(t, EventTypeContentPublished, map[string]interface{}{
		"action": ActionPublish, "content_id": 2002, "content_type": 1,
		"title": "回调路径", "doc_revision": 7,
	}))
	if err := h.Consume(context.Background(), "2002", value); err != nil {
		t.Fatal(err)
	}
	assertCalls(t, store, "received", "processing:evt-0001", "upsert", "succeeded:evt-0001")
	rec := store.lastReceived
	if rec == nil {
		t.Fatal("必须登记去重行")
	}
	if rec.Topic != "content.published.v1" {
		t.Fatalf("topic 应取 Handler 绑定的订阅 topic，实际 %s", rec.Topic)
	}
	if rec.PartitionNo != 0 || rec.OffsetNo != 0 {
		t.Fatalf("kq 回调拿不到位点，必须保持 0 而不是伪造: %+v", rec)
	}
	if rec.PayloadJSON != value {
		t.Fatal("原文必须完整保存以支持重启后续跑")
	}
	if len(store.upserted) != 1 || store.upserted[0].ContentID != 2002 {
		t.Fatalf("投影未写入或写错: %+v", store.upserted)
	}
}

// TestHandlerConsumeKeepsOffsetUncommittedOnTransientFailure 钉住「返回值决定位点」：
// 瞬时失败必须把错误返回给 kq（ForceCommit=false 时不提交），同时状态机已落 retry 行。
func TestHandlerConsumeKeepsOffsetUncommittedOnTransientFailure(t *testing.T) {
	store := &fakeStore{upsertErr: errors.New("opensearch unavailable")}
	h := NewHandler("content.published.v1", New(store, fastOptions()))
	err := h.Consume(context.Background(), "", string(envelopeBytes(t, EventTypeContentPublished, map[string]interface{}{
		"action": ActionPublish, "content_id": 3003, "content_type": 1, "title": "T", "doc_revision": 1,
	})))
	if err == nil {
		t.Fatal("瞬时失败必须返回错误，否则位点前移等于丢消息")
	}
	assertCalls(t, store, "received", "processing:evt-0001", "upsert", "retry")
	if len(store.upserted) != 0 {
		t.Fatal("写失败时不得记录成功投影")
	}
}

// TestHandlerConsumeRedeliveryBecomesDuplicateAndCommits 是上一条的另一半：
// broker 重投时 event_id 已登记，判定为重复并返回 nil，位点得以前进，重试由清扫器接管。
func TestHandlerConsumeRedeliveryBecomesDuplicateAndCommits(t *testing.T) {
	store := &fakeStore{receivedDuplicate: true}
	h := NewHandler("content.published.v1", New(store, fastOptions()))
	value := string(envelopeBytes(t, EventTypeContentPublished, map[string]interface{}{
		"action": ActionPublish, "content_id": 1, "content_type": 1, "title": "T", "doc_revision": 1,
	}))
	if err := h.Consume(context.Background(), "", value); err != nil {
		t.Fatalf("重复投递应返回 nil 让位点前进，实际: %v", err)
	}
	assertCalls(t, store, "received")
	if len(store.upserted) != 0 {
		t.Fatal("重投不得二次写索引")
	}
}

func TestHandlerConsumeWithoutBoundConsumerFails(t *testing.T) {
	if err := NewHandler("content.published.v1", nil).Consume(context.Background(), "k", "v"); err == nil {
		t.Fatal("未绑定状态机时必须报错，不能返回静默成功")
	}
}

func TestNewSupervisorGuards(t *testing.T) {
	c := validConfig()
	state := New(&fakeStore{}, fastOptions())

	if _, err := NewSupervisor(c, nil, &fakeFactory{}); err == nil {
		t.Fatal("nil 状态机必须拒绝")
	}
	if _, err := NewSupervisor(c, state, nil); err == nil {
		t.Fatal("nil 工厂必须拒绝（否则启动后没有任何消费者）")
	}
	broken := validConfig()
	broken.Kafka.Brokers = nil
	if _, err := NewSupervisor(broken, state, &fakeFactory{}); err == nil {
		t.Fatal("配置不完整必须拒绝，而不是起一个连不上的消费者")
	}
	emptyTopic := validConfig()
	emptyTopic.Kafka.Topics = []string{" ", "content.published.v1"}
	if _, err := NewSupervisor(emptyTopic, state, &fakeFactory{}); err == nil {
		t.Fatal("Topics 含空串必须拒绝")
	}

	// 去重 + 去空白：重复订阅会让同一条消息被处理两遍。
	dup := validConfig()
	dup.Kafka.Topics = []string{" content.published.v1 ", "content.published.v1", "engagement.action.v1"}
	sup, err := NewSupervisor(dup, state, &fakeFactory{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sup.Topics(), "|"); got != "content.published.v1|engagement.action.v1" {
		t.Fatalf("topic 应去空白并按首次出现去重，实际 %s", got)
	}
	if sup.Consumer() != state {
		t.Fatal("Supervisor 必须复用调用方传入的状态机，而不是自造一套")
	}
}

func TestSupervisorStartRollbacksHalfStartedQueues(t *testing.T) {
	c := validConfig()
	failErr := errors.New("dial refused")
	f := &fakeFactory{errFor: map[string]error{"engagement.action.v1": failErr}}
	sup, err := NewSupervisor(c, New(&fakeStore{}, fastOptions()), f)
	if err != nil {
		t.Fatal(err)
	}

	err = sup.Start()
	if err == nil {
		t.Fatal("第二个 topic 创建失败必须整体报错")
	}
	if !errors.Is(err, failErr) {
		t.Fatalf("错误必须可判定原始原因: %v", err)
	}
	if !strings.Contains(err.Error(), "engagement.action.v1") {
		t.Fatalf("错误必须点名失败的 topic: %v", err)
	}
	// 半启动回滚：第一个已建好的消费者必须关掉。
	if len(f.created) != 1 {
		t.Fatalf("只应创建过 1 个队列，实际 %d", len(f.created))
	}
	if got := strings.Join(f.events, ","); got != "stop:content.published.v1" {
		t.Fatalf("失败必须回滚已启动队列且不留下 start 悬挂: %s", got)
	}
	if sup.Topics() == nil {
		t.Fatal("Topics() 不应为 nil")
	}

	// 回滚后 started 仍为 false：修好配置可以重来，不会「半启动」状态被永久占用。
	f.errFor = nil
	if err := sup.Start(); err != nil {
		t.Fatalf("配置修复后应可重新启动: %v", err)
	}
	if len(f.created) != 3 {
		t.Fatalf("重新启动应补齐两个 topic，实际创建 %d 个", len(f.created))
	}
	if err := sup.Start(); err == nil {
		t.Fatal("重复 Start 必须拒绝，否则同一事件被双份处理")
	}
	sup.Stop()
}

func TestSupervisorStartsAllAndStopsInReverse(t *testing.T) {
	c := validConfig()
	f := &fakeFactory{}
	sup, err := NewSupervisor(c, New(&fakeStore{}, fastOptions()), f)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(); err != nil {
		t.Fatal(err)
	}
	want := "start:content.published.v1,start:engagement.action.v1"
	if got := strings.Join(f.events, ","); got != want {
		t.Fatalf("每个 topic 都要起一个消费者且顺序稳定: %s", got)
	}
	if f.lastSett.Group != c.Kafka.Group || f.lastSett.Name != c.Name {
		t.Fatalf("工厂必须收到映射后的 Settings: %+v", f.lastSett)
	}

	sup.Stop()
	want = "start:content.published.v1,start:engagement.action.v1,stop:engagement.action.v1,stop:content.published.v1"
	if got := strings.Join(f.events, ","); got != want {
		t.Fatalf("关闭必须逆序（先启动的后关）: %s", got)
	}
	// 幂等收尾：Stop 之后再 Stop 不应 panic 或重复计数。
	sup.Stop()
	sup2 := &Supervisor{}
	sup2.Stop() // 从未启动过
}

func TestSupervisorRejectsNilQueueFromFactory(t *testing.T) {
	c := validConfig()
	f := &fakeFactory{nilQueue: true}
	sup, err := NewSupervisor(c, New(&fakeStore{}, fastOptions()), f)
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(); err == nil {
		t.Fatal("工厂返回 nil 队列必须报错，否则消费者静默缺席")
	} else if !strings.Contains(err.Error(), "nil consumer") {
		t.Fatalf("错误应点名 nil 消费者: %v", err)
	}
}

// TestExampleYamlIsReadyToEnable 把「示例配置」与「消费配置校验」钉在一起：
// 提交进仓库的 etc/searchindexer.v1.yaml 必须默认不消费（Enabled=false，避免任何进程
// 一启动就去拨号），但把 Enabled 翻成 true 之后就能直接通过 ValidateKafka，
// 并且不得带任何凭据或证书内容。改坏 yaml 的键名、默认值或补上真实口令都会在这里红。
func TestExampleYamlIsReadyToEnable(t *testing.T) {
	var c config.Config
	if err := conf.Load(filepath.Join("..", "..", "etc", "searchindexer.v1.yaml"), &c); err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}
	k := c.Kafka
	if k.Enabled {
		t.Fatal("示例配置必须 Enabled=false：默认构建没链接运行时，打开就是启动即失败")
	}
	if k.RetrySweeperEnabled != true || k.RebuildRunnerEnabled != true {
		t.Fatalf("清扫器与重建执行器默认应开启（都只依赖 MySQL/OpenSearch）: %+v", k)
	}
	if k.ForceCommit {
		t.Fatal("示例配置不得 ForceCommit=true：那会让处理失败的消息被提交掉，事件直接丢")
	}
	if k.Offset != "last" {
		t.Fatalf("示例配置起点应为 last，实际 %q", k.Offset)
	}
	if k.Username != "" || k.Password != "" || k.CaFile != "" {
		t.Fatal("示例配置不得包含 SASL 凭据或证书路径内容（AGENTS.md §4：生产走 Secret/环境变量）")
	}
	if err := ValidateKafka(k); err != nil {
		t.Fatalf("示例配置翻转 Enabled 后应立即可用: %v", err)
	}

	// 只翻开关，其余原样：证明「打开消费」不需要额外补参数。
	k.Enabled = true
	sup, err := NewSupervisor(config.Config{Kafka: k}, New(&fakeStore{}, fastOptions()), &fakeFactory{})
	if err != nil {
		t.Fatalf("用示例配置构造 Supervisor 失败: %v", err)
	}
	if got := strings.Join(sup.Topics(), "|"); got != "content.published.v1|engagement.action.v1" {
		t.Fatalf("订阅 topic 与文档契约不一致: %s", got)
	}
	sup.Stop()
}
