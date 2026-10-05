package publisher

// 本文件钉住「配置 → 发布参数」这一段（params.go）。它决定的是运维能改的东西：
// 九个旋钮各自来自哪个 yaml 键、配置漂移要点名到哪个键、以及 svc 入口 NewPublisher
// 在什么组合下必须失败。
//
// 与 playback 的同名文件相比，本服务多了一类判据：PublishTopics 有 9 个值，
// 所以「集合相等」比「有没有那一个」更常被抓到，两侧漂移的后果也不同方向
//（少一个 → 该类事件每行退避到判死；多一个 → 白占通道并误导排障），
// 用例因此四个方向都覆盖：缺项、多项、重复项、空项。

import (
	"strings"
	"testing"
	"time"

	"go-video/common/outbox"
	"go-video/services/live-media/internal/config"
)

// validKafka 是一份全部合规的配置（Enabled 不参与参数校验，它只决定 svc 开不开循环）。
func validKafka() config.KafkaConf {
	return config.KafkaConf{
		Enabled:            true,
		Brokers:            []string{"127.0.0.1:9092"},
		PublishTopics:      RequiredTopics(),
		MaxRetries:         5,
		RetryBackoffSec:    5,
		RetryMaxBackoffSec: 1800,
		PollIntervalSec:    2,
		BatchLimit:         100,
		SendTimeoutSec:     5,
	}
}

// TestSenderSettingsFromDedupesAndTrims 钉住建立发送通道的参数推导：
// 空白 topic 丢弃、重复 topic 只留一条、顺序保持声明顺序，
// 且 Brokers 必须是复制而不是配置的底层切片。
func TestSenderSettingsFromDedupesAndTrims(t *testing.T) {
	required := RequiredTopics()
	k := config.KafkaConf{
		Brokers: []string{"127.0.0.1:9092", " "},
		PublishTopics: append(append([]string{}, required...),
			required[0], "  "+required[1]+" ", "", required[2]),
	}
	got := SenderSettingsFrom(k)
	// 去重后必须正好回到 9 个，且第一个位置仍是 required[0]（顺序按声明顺序，不排序）。
	if strings.Join(got.Topics, ",") != strings.Join(required, ",") {
		t.Errorf("topics=%v，期望去空白去重后正好是声明的 %d 个", got.Topics, len(required))
	}
	if strings.Join(got.Brokers, ",") != "127.0.0.1:9092, " {
		t.Errorf("brokers=%q，期望原样复制（空串由校验点名，不在这里悄悄删）", got.Brokers)
	}
	got.Brokers[0] = "mutated"
	if k.Brokers[0] != "127.0.0.1:9092" {
		t.Error("SenderSettingsFrom 返回了配置的底层切片，改它会改配置")
	}
}

// TestOptionsFromMapsEveryKey 逐个键确认映射没有被张冠李戴：
// 六个投递旋钮分别来自六个配置键，配错任意一个都只该红这一项。
func TestOptionsFromMapsEveryKey(t *testing.T) {
	got := OptionsFrom(validKafka())
	want := outbox.Options{
		Name:        label,
		Interval:    2 * time.Second,
		Batch:       100,
		MaxAttempts: 5,
		BaseBackoff: 5 * time.Second,
		MaxBackoff:  1800 * time.Second,
		SendTimeout: 5 * time.Second,
	}
	if got != want {
		t.Errorf("OptionsFrom=%+v，期望 %+v", got, want)
	}
	if got.Name != label {
		t.Errorf("Name=%q，期望 %q：日志必须能分辨是哪个服务的循环", got.Name, label)
	}
	// 基数与上限都必须是秒而不是毫秒：配成毫秒会让退避直接跨过判死边界。
	if got.BaseBackoff != time.Duration(validKafka().RetryBackoffSec)*time.Second {
		t.Errorf("BaseBackoff=%s，期望按秒解释", got.BaseBackoff)
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
		{"broker 含空串", func(k *config.KafkaConf) { k.Brokers = []string{"127.0.0.1:9092", ""} },
			[]string{"Brokers[1]"}},
		{"topic 全空", func(k *config.KafkaConf) { k.PublishTopics = nil },
			[]string{"Kafka.PublishTopics 为空", "9 个 topic", requiredJoined()}},
		{"topic 含空串", func(k *config.KafkaConf) { k.PublishTopics = []string{" "} },
			[]string{"PublishTopics 含空串", "缺少"}},
		{"少登记一个事件类型", func(k *config.KafkaConf) {
			k.PublishTopics = RequiredTopics()[:8]
		}, []string{"缺少 \"" + RequiredTopics()[8] + "\"", "判死"}},
		{"多写一个本服务不产出的 topic", func(k *config.KafkaConf) {
			k.PublishTopics = append(RequiredTopics(), "content.published.v1")
		}, []string{"content.published.v1", "不产出"}},
		{"同一个外来 topic 写两遍只点一次名", func(k *config.KafkaConf) {
			k.PublishTopics = append(RequiredTopics(), "content.published.v1", "content.published.v1")
		}, []string{"content.published.v1"}},
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
			msg := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("错误=%s，期望点名 %q", msg, want)
				}
			}
			if !strings.Contains(msg, "live-media") {
				t.Errorf("错误=%s，应带 live-media 前缀让运维分辨服务", msg)
			}
			if strings.Count(msg, "content.published.v1") > 1 {
				t.Errorf("同一个外来 topic 被点名多次，重复项会淹没其它错键：%s", msg)
			}
		})
	}
	// 重复的自有 topic 不构成错误：SenderSettingsFrom 会去重，集合判据也不受影响。
	k := validKafka()
	k.PublishTopics = append(RequiredTopics(), RequiredTopics()[0])
	if err := ValidatePublishKafka(k); err != nil {
		t.Errorf("自有 topic 写重了不该报错：%v", err)
	}
}

// TestValidatePublishKafkaReportsAllMissingTopics 钉住「少登记」方向是一次列全 9 个：
// 只报第一个会让运维改一轮、重启一轮，而这里的每一个缺项都是「那一类事件全判死」。
func TestValidatePublishKafkaReportsAllMissingTopics(t *testing.T) {
	k := validKafka()
	k.PublishTopics = nil
	msg := ValidatePublishKafka(k).Error()
	for _, topic := range RequiredTopics() {
		if !strings.Contains(msg, topic) {
			t.Errorf("PublishTopics 为空时错误文本应枚举全部 topic，缺少 %q：%s", topic, msg)
		}
	}
	if got := strings.Index(msg, RequiredTopics()[0]); got > strings.Index(msg, RequiredTopics()[8]) {
		t.Errorf("枚举顺序应按 model 声明顺序稳定：%s", msg)
	}
}

// TestValidatePublishKafkaReportsAllBadKeys 确认校验是聚合式的：
// 一次启动日志就要列全所有错键，而不是改一个报一个地来回重启。
func TestValidatePublishKafkaReportsAllBadKeys(t *testing.T) {
	k := validKafka()
	k.Brokers, k.MaxRetries, k.BatchLimit, k.SendTimeoutSec, k.PollIntervalSec = nil, -1, 0, 0, 0
	msg := ValidatePublishKafka(k).Error()
	for _, want := range []string{"Kafka.Brokers", "Kafka.MaxRetries", "Kafka.BatchLimit",
		"Kafka.SendTimeoutSec", "Kafka.PollIntervalSec"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误里缺少 %q：%s", want, msg)
		}
	}
	if n := strings.Count(msg, "; "); n < 4 {
		t.Errorf("五条错误应各自成项（分号分隔），实际=%s", msg)
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
	// 装配期就漏掉一个 topic 也要拦住：svc 只有到这里才第一次比对两侧集合。
	partial := config.Config{}
	partial.Kafka = validKafka()
	partial.Kafka.PublishTopics = RequiredTopics()[:3]
	if _, err := NewPublisher(partial, store, sender); err == nil ||
		!strings.Contains(err.Error(), "缺少") {
		t.Errorf("PublishTopics 漂移应拦住启动：%v", err)
	}
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
	k.MaxRetries = 1 << 31
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

func requiredJoined() string {
	return strings.Join(RequiredTopics(), ", ")
}
