package publisher

import (
	"strings"
	"testing"
	"time"

	"go-video/common/outbox"
	"go-video/services/playback/internal/config"
)

// validKafka 是一份除 Enabled 外全部合规的配置（Enabled 不参与参数校验）。
func validKafka() config.KafkaConf {
	return config.KafkaConf{
		Enabled:            true,
		Brokers:            []string{"127.0.0.1:9092"},
		PublishTopics:      []string{RequiredTopic()},
		MaxRetries:         5,
		RetryBackoffSec:    2,
		RetryMaxBackoffSec: 1800,
		PollIntervalSec:    2,
		BatchLimit:         100,
		SendTimeoutSec:     5,
	}
}

// TestSenderSettingsFromDedupesAndTrims 钉住建立发送通道的参数推导：
// 空白 topic 丢弃、重复 topic 只留一条、顺序保持声明顺序，
// 且不得改写原配置切片（Brokers 用 append(nil, ...) 复制，测试据此验证）。
func TestSenderSettingsFromDedupesAndTrims(t *testing.T) {
	k := config.KafkaConf{
		Brokers:       []string{"127.0.0.1:9092", " "},
		PublishTopics: []string{"  " + RequiredTopic() + " ", RequiredTopic(), "", "content.published.v1"},
	}
	got := SenderSettingsFrom(k)
	if strings.Join(got.Topics, ",") != RequiredTopic()+",content.published.v1" {
		t.Errorf("topics=%v，期望去空白去重后保留两条", got.Topics)
	}
	if strings.Join(got.Brokers, ",") != "127.0.0.1:9092, " {
		t.Errorf("brokers=%q，期望原样复制（空串由校验点名，不在这里悄悄删）", got.Brokers)
	}
	// 复制必须是新切片：改派生结果不能污染配置本身。
	got.Brokers[0] = "mutated"
	if k.Brokers[0] != "127.0.0.1:9092" {
		t.Error("SenderSettingsFrom 返回了配置的底层切片，改它会改配置")
	}
}

// TestOptionsFromMapsEveryKey 逐个键确认映射没有被张冠李戴：
// 六个旋钮分别来自六个配置键，配错任意一个都只该红这一项。
func TestOptionsFromMapsEveryKey(t *testing.T) {
	got := OptionsFrom(validKafka())
	want := outbox.Options{
		Name:        label,
		Interval:    2 * time.Second,
		Batch:       100,
		MaxAttempts: 5,
		BaseBackoff: 2 * time.Second,
		MaxBackoff:  1800 * time.Second,
		SendTimeout: 5 * time.Second,
	}
	if got != want {
		t.Errorf("OptionsFrom=%+v，期望 %+v", got, want)
	}
	if got.Name != label {
		t.Errorf("Name=%q，期望 %q：日志必须能分辨是哪个服务的循环", got.Name, label)
	}
}

// TestValidatePublishKafkaRejectsIncompleteConfig 逐键点名：
// 每个不合规配置都要在错误里出现自己的键名，否则运维只能逐个试。
func TestValidatePublishKafkaRejectsIncompleteConfig(t *testing.T) {
	if err := ValidatePublishKafka(validKafka()); err != nil {
		t.Fatalf("合规配置被拒：%v", err)
	}
	if err := ValidatePublishKafka(config.KafkaConf{}); err == nil {
		t.Fatal("全零配置必须被拒（默认构建下 Enabled=false 时 svc 不校验，但打开开关前必须能查出来）")
	}

	cases := []struct {
		name   string
		mutate func(*config.KafkaConf)
		want   []string
	}{
		{"没有 broker", func(k *config.KafkaConf) { k.Brokers = nil }, []string{"Kafka.Brokers"}},
		{"broker 含空串", func(k *config.KafkaConf) { k.Brokers = []string{""} }, []string{"Brokers[0]"}},
		{"没有 topic", func(k *config.KafkaConf) { k.PublishTopics = nil }, []string{"Kafka.PublishTopics 为空", RequiredTopic()}},
		{"topic 含空串", func(k *config.KafkaConf) { k.PublishTopics = []string{" "} }, []string{"PublishTopics 含空串"}},
		{"多写一个本服务不产出的 topic", func(k *config.KafkaConf) {
			k.PublishTopics = []string{RequiredTopic(), "content.published.v1"}
		}, []string{"content.published.v1", "不产出"}},
		{"尝试上限为 0", func(k *config.KafkaConf) { k.MaxRetries = 0 }, []string{"Kafka.MaxRetries"}},
		{"退避基数为 0", func(k *config.KafkaConf) { k.RetryBackoffSec = 0 }, []string{"Kafka.RetryBackoffSec"}},
		{"退避上限为 0", func(k *config.KafkaConf) { k.RetryMaxBackoffSec = 0 }, []string{"Kafka.RetryMaxBackoffSec"}},
		{"退避上限小于基数", func(k *config.KafkaConf) {
			k.RetryBackoffSec, k.RetryMaxBackoffSec = 100, 10
		}, []string{"RetryMaxBackoffSec 不得小于"}},
		{"轮询间隔为 0", func(k *config.KafkaConf) { k.PollIntervalSec = 0 }, []string{"Kafka.PollIntervalSec"}},
		{"批次为 0", func(k *config.KafkaConf) { k.BatchLimit = 0 }, []string{"Kafka.BatchLimit"}},
		{"投递超时为 0", func(k *config.KafkaConf) { k.SendTimeoutSec = 0 }, []string{"Kafka.SendTimeoutSec"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := validKafka()
			tc.mutate(&k)
			err := ValidatePublishKafka(k)
			if err == nil {
				t.Fatalf("应拒绝：%+v", k)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("错误=%v，期望点名 %q", err, want)
				}
			}
			if !strings.Contains(err.Error(), "playback") {
				t.Errorf("错误=%v，应带 playback 前缀让运维分辨服务", err)
			}
		})
	}
}

// TestValidatePublishKafkaReportsAllBadKeys 确认校验是聚合式的：
// 一次启动日志就要列全所有错键，而不是改一个报一个地来回重启。
func TestValidatePublishKafkaReportsAllBadKeys(t *testing.T) {
	k := validKafka()
	k.Brokers, k.MaxRetries, k.BatchLimit, k.SendTimeoutSec = nil, -1, 0, 0
	err := ValidatePublishKafka(k)
	if err == nil {
		t.Fatal("应拒绝")
	}
	msg := err.Error()
	for _, want := range []string{"Kafka.Brokers", "Kafka.MaxRetries", "Kafka.BatchLimit", "Kafka.SendTimeoutSec"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误里缺少 %q：%s", want, msg)
		}
	}
	if n := strings.Count(msg, "; "); n < 3 {
		t.Errorf("四条错误应各自成项（分号分隔），实际=%s", msg)
	}
}

// TestNewPublisherAssembly 钉住 svc 入口的四条出口：
// 配置不合格、model 缺失、sender 缺失都必须失败，只有三者齐备才返回可用发布器。
func TestNewPublisherAssembly(t *testing.T) {
	c := config.Config{}
	c.Kafka = validKafka()
	store := newFakeOutbox()
	sender := &fakeSender{}

	if _, err := NewPublisher(c, store, sender); err != nil {
		t.Fatalf("合规配置应能装配：%v", err)
	}

	bad := config.Config{}
	bad.Kafka = validKafka()
	bad.Kafka.PollIntervalSec = 0
	if _, err := NewPublisher(bad, store, sender); err == nil ||
		!strings.Contains(err.Error(), "Kafka.PollIntervalSec") {
		t.Errorf("配置错误必须点名配置键：%v", err)
	}

	if _, err := NewPublisher(c, nil, sender); err == nil ||
		!strings.Contains(err.Error(), "outbox model is required") {
		t.Errorf("缺 model 应失败：%v", err)
	}
	if _, err := NewPublisher(c, store, nil); err == nil ||
		!strings.Contains(err.Error(), "sender is required") {
		t.Errorf("缺 sender 应失败：%v", err)
	}
	// 返回的实例必须带上从配置推导出的参数，svc 之后的 Start 才不会再校一遍别的值。
	pub, err := NewPublisher(c, store, sender)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	if got := pub.Options(); got != OptionsFrom(c.Kafka) {
		t.Errorf("参数未透传：%+v", got)
	}
	if pub.Running() {
		t.Error("NewPublisher 不应自行启动循环：启动时机由 svc 决定，否则测试里会留下跑着的协程")
	}
}

// TestNewPublisherDelegatesEngineValidation 钉住配置层放行、引擎层拒绝的组合确实存在并被拦住：
// Kafka.MaxRetries 是 int，而引擎的 Options.MaxAttempts 是 int32（retry_count 列本身是 int32），
// 超出 int32 的值会在映射处绕成负数。配置层只判 >0 所以放行，最终必须由 outbox.New 拦住，
// 且错误要能反指回配置键，否则运维对着 yaml 看不出是哪个键。
func TestNewPublisherDelegatesEngineValidation(t *testing.T) {
	c := config.Config{}
	k := validKafka()
	k.MaxRetries = 1 << 31 // int32 上限 +1：配置层放行，映射成 int32 后绕成负数
	c.Kafka = k
	if err := ValidatePublishKafka(c.Kafka); err != nil {
		t.Fatalf("配置层只判 >0，此值应放行：%v", err)
	}
	pub, err := NewPublisher(c, newFakeOutbox(), &fakeSender{})
	if err == nil {
		t.Fatal("MaxAttempts 绕成负数后应被引擎拒绝")
	}
	if pub != nil {
		t.Errorf("被拒时不应返回实例：%+v", pub)
	}
	msg := err.Error()
	for _, want := range []string{"Options.MaxAttempts", "Kafka.MaxRetries"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误=%v，期望同时点名引擎参数与配置键 %q", msg, want)
		}
	}
}
