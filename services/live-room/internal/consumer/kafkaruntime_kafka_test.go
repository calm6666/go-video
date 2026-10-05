//go:build liveroom_kafka

// kafkaruntime_kafka_test.go 只验证工厂的入参守卫与运行时声明，不连接任何 broker。
//
// kq.NewQueue 在 Start 之前不会拨号，但那是 go-queue 的实现细节，单测不依赖它：
// 这里刻意不调用 factory.New 建真实队列。「链接了 kq」永远不等于
// 「消费语义已在真实集群上可用」（见 README「已知缺口」）。
package consumer

import (
	"strings"
	"testing"

	"github.com/zeromicro/go-queue/kq"
)

// 编译期钉子：*Handler 必须直接满足 kq.ConsumeHandler（Consume(ctx, key, value) error）。
// 这条兼容是整座桥只用一个文件接 Kafka 的前提；go-queue 改签名时这里先编译失败。
var _ kq.ConsumeHandler = (*Handler)(nil)

func TestLinkedBuildProvidesFactory(t *testing.T) {
	factory, err := NewKqFactory()
	if err != nil {
		t.Fatalf("带标签构建必须给出工厂: %v", err)
	}
	if factory == nil {
		t.Fatal("工厂不得为 nil")
	}
	// 工厂给出后，配置校验仍然必须拦住没有映射的 topic：
	// 少这一条，Supervisor 会订阅一个每封都只能丢掉的 topic。
	c := validConfig()
	c.Kafka.SubscribeTopics = []string{"moderation.result.v1"}
	if _, err := NewSupervisor(c, countApplicator(), factory); err == nil ||
		!strings.Contains(err.Error(), "moderation.result.v1") {
		t.Fatalf("链接了运行时也要拒绝没有映射的 topic: %v", err)
	}
}

func TestLinkedFactoryRejectsBadArgumentsBeforeTouchingBroker(t *testing.T) {
	factory, err := NewKqFactory()
	if err != nil {
		t.Fatal(err)
	}
	s := SettingsFrom(validConfig())
	h := NewHandler(countApplicator(), OptionsFrom(validKafka()))

	if _, err := factory.New(s, "", h); err == nil || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("空 topic 必须点名 topic 并拒绝: %v", err)
	}
	if _, err := factory.New(s, SupportedTopic, nil); err == nil ||
		!strings.Contains(err.Error(), "handler") {
		t.Fatalf("nil handler 必须拒绝，否则消息会被静默吞掉: %v", err)
	}
}

func TestLinkedRuntimeNotesSayWhatIsStillMissing(t *testing.T) {
	k := validKafka()
	notes := strings.Join(RuntimeNotes(k), "\n")
	if !strings.Contains(notes, "已链接") || !strings.Contains(notes, "liveroom_kafka") {
		t.Fatalf("必须说明运行时由哪个标签链接: %s", notes)
	}
	if strings.Contains(notes, "已在消费") {
		t.Fatalf("不得声称消费语义可用: %s", notes)
	}
	// Enabled=true 时必须写清「没在 broker 上联调过」，否则日志会被读成链路已验证。
	if !strings.Contains(notes, "broker") || !strings.Contains(notes, "从未") {
		t.Fatalf("已启用分支必须保留未联调声明: %s", notes)
	}

	k.Enabled = false
	if notes = strings.Join(RuntimeNotes(k), "\n"); !strings.Contains(notes, "Kafka.Enabled=false") ||
		!strings.Contains(notes, "live.state.v1") {
		t.Fatalf("链接了运行时但没打开开关时也要写清楚: %s", notes)
	}
	if strings.Contains(notes, "从未") {
		t.Fatalf("未启用的分支不必重复联调声明: %s", notes)
	}
}
