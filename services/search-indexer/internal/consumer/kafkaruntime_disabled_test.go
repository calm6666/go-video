//go:build !searchindexer_kafka

// kafkaruntime_disabled_test.go 钉住默认构建的诚实性：没有链接 Kafka 客户端时
// 必须返回可判定的哨兵错误，日志里也不能出现「已启动消费者」的字样。
package consumer

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultBuildDoesNotLinkKafkaRuntime(t *testing.T) {
	factory, err := NewKqFactory()
	if !errors.Is(err, ErrKafkaRuntimeNotBuilt) {
		t.Fatalf("默认构建必须返回 ErrKafkaRuntimeNotBuilt，实际: %v", err)
	}
	if factory != nil {
		t.Fatal("未链接运行时时不得返回工厂，否则 svc 会以为可以起消费者")
	}
	msg := err.Error()
	for _, want := range []string{"searchindexer_kafka", "broker"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案必须给出可执行的下一步（标签名与真实原因），缺少 %q: %s", want, msg)
		}
	}
}

func TestDisabledRuntimeNotesStateTheTruth(t *testing.T) {
	k := validKafkaConf() // Enabled=true
	notes := strings.Join(RuntimeNotes(k), "\n")
	if !strings.Contains(notes, "未链接") {
		t.Fatalf("默认构建的日志不得暗示已链接: %s", notes)
	}
	if !strings.Contains(notes, "searchindexer_kafka") {
		t.Fatalf("日志必须给出启用方式: %s", notes)
	}
	if strings.Contains(notes, "已启动 topics=") {
		t.Fatalf("不能出现像「消费者已启动」的字样: %s", notes)
	}

	k.Enabled = false
	notes = strings.Join(RuntimeNotes(k), "\n")
	if !strings.Contains(notes, "Kafka.Enabled=false") {
		t.Fatalf("必须说明本进程不消费事件: %s", notes)
	}
	if !strings.Contains(notes, "UpsertContentDoc") {
		t.Fatalf("必须点出当前真实的索引写入入口: %s", notes)
	}

	k = validKafkaConf()
	k.RetrySweeperEnabled = false
	if notes = strings.Join(RuntimeNotes(k), "\n"); !strings.Contains(notes, "不会被重投") {
		t.Fatalf("关掉清扫器必须在日志里可见: %s", notes)
	}
}
