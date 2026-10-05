//go:build !liveingest_kafka

// kafkaruntime_disabled_test.go 钉住默认构建的真实上限：本二进制发不出事件，
// 而且必须把这件事说清楚（错误里给可执行的下一步，启动日志给结论）。
package publisher

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/live-ingest/internal/config"
)

func TestDefaultBuildRefusesToCreateSender(t *testing.T) {
	// 参数完全合规也拿不到发送端：默认构建根本没链接 kq。
	sender, err := NewSender(SenderSettings{
		Brokers: []string{"127.0.0.1:9092"},
		Topics:  []string{RequiredTopic()},
	})
	if sender != nil {
		t.Fatalf("默认构建不应返回发送端：%+v", sender)
	}
	if !errors.Is(err, ErrKafkaRuntimeNotBuilt) {
		t.Fatalf("err=%v，期望 ErrKafkaRuntimeNotBuilt", err)
	}
	msg := err.Error()
	for _, want := range []string{"liveingest_kafka", "broker", "Kafka.Enabled"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误文本缺少 %q，运维拿不到可执行的下一步：%s", want, msg)
		}
	}
}

// TestDefaultRuntimeNotes 钉住启动日志的两条结论：未链接 + 不投递的后果。
// 第二条必须点名「事件留在哪张表」，否则运维只会看到「没有事件」而不知道去哪查。
func TestDefaultRuntimeNotes(t *testing.T) {
	notes := strings.Join(RuntimeNotes(config.KafkaConf{Enabled: false}), "\n")
	if !strings.Contains(notes, "未链接") {
		t.Errorf("日志应说明默认构建未链接发送端：%s", notes)
	}
	if !strings.Contains(notes, "live_ingest_outbox") || !strings.Contains(notes, RequiredTopic()) {
		t.Errorf("Enabled=false 的说明要点名积压位置与 topic：%s", notes)
	}
	// Enabled=true 时只剩「未链接」这一条：此时 svc 会在 NewSender 处失败，
	// 不需要在这里重复解释不投递的后果。
	enabled := RuntimeNotes(config.KafkaConf{Enabled: true})
	if len(enabled) != 1 || !strings.Contains(enabled[0], "未链接") {
		t.Errorf("Enabled=true 的说明=%v，期望只有未链接一条", enabled)
	}
}
