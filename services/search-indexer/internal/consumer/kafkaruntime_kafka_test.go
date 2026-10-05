//go:build searchindexer_kafka

// kafkaruntime_kafka_test.go 只验证工厂的入参守卫与运行时声明，不连接任何 broker：
// kq.NewQueue 在 Start 之前不会拨号，但也不要在单测里依赖这个实现细节。
// 「链接了 kq」永远不等于「消费语义已在真实集群上可用」（README「已知缺口」1）。
package consumer

import (
	"strings"
	"testing"
)

func TestLinkedBuildProvidesFactory(t *testing.T) {
	factory, err := NewKqFactory()
	if err != nil {
		t.Fatalf("带标签构建必须给出工厂: %v", err)
	}
	if factory == nil {
		t.Fatal("工厂不得为 nil")
	}
}

func TestLinkedFactoryRejectsBadArgumentsBeforeTouchingBroker(t *testing.T) {
	factory, err := NewKqFactory()
	if err != nil {
		t.Fatal(err)
	}
	s := SettingsFrom(validConfig())
	h := NewHandler("content.published.v1", New(&fakeStore{}, fastOptions()))

	if _, err := factory.New(s, "", h); err == nil {
		t.Fatal("空 topic 必须拒绝")
	} else if !strings.Contains(err.Error(), "topic") {
		t.Fatalf("错误应点名 topic: %v", err)
	}
	if _, err := factory.New(s, "content.published.v1", nil); err == nil {
		t.Fatal("nil handler 必须拒绝，否则消息会被吞掉")
	} else if !strings.Contains(err.Error(), "handler") {
		t.Fatalf("错误应点名 handler: %v", err)
	}
}

func TestLinkedRuntimeNotesSayWhatIsStillMissing(t *testing.T) {
	k := validKafkaConf()
	notes := strings.Join(RuntimeNotes(k), "\n")
	if !strings.Contains(notes, "已链接") || !strings.Contains(notes, "searchindexer_kafka") {
		t.Fatalf("必须说明运行时由哪个标签链接: %s", notes)
	}
	if strings.Contains(notes, "已在消费") {
		t.Fatalf("不得声称消费语义可用: %s", notes)
	}

	k.Enabled = false
	if notes = strings.Join(RuntimeNotes(k), "\n"); !strings.Contains(notes, "Kafka.Enabled=false") {
		t.Fatalf("链接了运行时但没打开开关时也要写清楚: %s", notes)
	}
}
