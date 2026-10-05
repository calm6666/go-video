//go:build !upload_kafka

// kafkaruntime_disabled_test.go 钉住默认构建的真实上限：本二进制发不出事件，
// 而且必须把这件事说清楚（错误里给可执行的下一步，启动日志给结论）。
package publisher

import (
	"errors"
	"strings"
	"testing"

	"go-video/services/upload/internal/config"
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
	for _, want := range []string{"upload_kafka", "broker", "Kafka.Enabled"} {
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
	if !strings.Contains(notes, "upload_outbox") || !strings.Contains(notes, RequiredTopic()) {
		t.Errorf("Enabled=false 的说明要点名积压位置与 topic：%s", notes)
	}
	if !strings.Contains(notes, "还没有消费方") {
		t.Errorf("Enabled=false 要点名下游还没有消费方，否则运维会以为打开开关就有转码：%s", notes)
	}
	// Enabled=true 时只剩「未链接」这一条：此时 svc 会在 NewSender 处失败，
	// 不需要在这里重复解释不投递的后果。
	enabled := RuntimeNotes(config.KafkaConf{Enabled: true})
	if len(enabled) != 1 || !strings.Contains(enabled[0], "未链接") {
		t.Errorf("Enabled=true 的说明=%v，期望只有未链接一条", enabled)
	}
}

// TestDefaultBuildStartPublisherFails 用 svc 的入口形状确认「打开开关就炸」：
// Enabled=true 时 NewSender 必然失败，配置再合规也建不出发送端。
// 这条是运维真正会撞上的路径，不能只靠读注释确认。
func TestDefaultBuildStartPublisherFails(t *testing.T) {
	c := loadExampleConfig(t)
	c.Kafka.Enabled = true
	if err := ValidatePublishKafka(c.Kafka); err != nil {
		t.Fatalf("示例配置翻转 Enabled 后参数应仍合格：%v", err)
	}
	sender, err := NewSender(SenderSettingsFrom(c.Kafka))
	if !errors.Is(err, ErrKafkaRuntimeNotBuilt) {
		t.Fatalf("svc 在 Enabled=true 的默认构建上应拿不到发送端：%v", err)
	}
	if sender != nil {
		t.Fatal("错误里同时返回了非 nil 发送端：svc 会带着一个发不出东西的对象继续启动")
	}
}
